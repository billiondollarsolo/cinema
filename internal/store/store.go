// Package store keeps run records in SQLite and output in a content-addressed
// blob directory under the per-user data dir (plan §13), and owns the
// cleanup rules for both (plan §13.1).
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"github.com/billiondollarsolo/cinema/internal/record"
)

// Run states. Only StatePassed can ever be signed (plan §7.2).
const (
	StateRecording  = "recording"  // started; not yet durable
	StatePassed     = "passed"     // exit 0, not signaled, not canceled, output stored
	StateFailed     = "failed"     // anything else that finished
	StateIncomplete = "incomplete" // recorder died mid-run (RECORDING_INCOMPLETE)
)

type Store struct {
	db     *sql.DB
	dir    string
	Policy Policy
	now    func() time.Time
}

// DataDir is $CINEMA_DATA_DIR, else the platform's per-user data dir + /cinema.
func DataDir() (string, error) {
	if d := os.Getenv("CINEMA_DATA_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "cinema"), nil
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "cinema"), nil
	}
	return filepath.Join(home, ".local", "share", "cinema"), nil
}

const schema = `
CREATE TABLE IF NOT EXISTS runs (
	id               TEXT PRIMARY KEY,
	state            TEXT NOT NULL,
	pid              INTEGER NOT NULL,  -- recorder PID, for crash detection
	argv             TEXT NOT NULL,     -- JSON array
	path             TEXT NOT NULL DEFAULT '',
	cwd              TEXT NOT NULL,
	started_at       INTEGER NOT NULL,  -- unix ms
	duration_ms      INTEGER,
	exit_code        INTEGER,
	signal           TEXT,
	canceled         INTEGER NOT NULL DEFAULT 0,
	leftovers        INTEGER NOT NULL DEFAULT 0,
	stdout_blob      TEXT,              -- sha256 hex; NULL when empty or expired
	stdout_total     INTEGER,
	stdout_truncated INTEGER,
	stderr_blob      TEXT,
	stderr_total     INTEGER,
	stderr_truncated INTEGER,
	repo_root        TEXT,              -- NULL outside a git repo
	cwd_rel          TEXT,              -- cwd relative to repo_root
	source_before    TEXT,              -- "sha256:…" whole-source fingerprint (plan §5)
	source_after     TEXT               -- differs from before → SOURCE_CHANGED_DURING_RUN
);
CREATE INDEX IF NOT EXISTS runs_started ON runs(started_at);
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);
`

// Open creates the data dir if needed, migrates, sweeps stale temp files,
// and marks runs whose recorder died as incomplete.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "tmp"), 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.Join(dir, "state.db") +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, dir: dir, Policy: DefaultPolicy, now: time.Now}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	s.sweepTmp()
	if err := s.recoverIncomplete(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// migrate brings the schema to the current version (PRAGMA user_version).
// Add a step per version; never edit a released step.
func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v < 1 {
		if _, err := db.Exec(schema + `PRAGMA user_version = 1;`); err != nil {
			return err
		}
	}
	if v < 2 { // task matching (plan §6)
		if _, err := db.Exec(`
			ALTER TABLE runs ADD COLUMN task_id TEXT;          -- exact argv+cwd match, else NULL
			ALTER TABLE runs ADD COLUMN contract_digest TEXT;
			ALTER TABLE runs ADD COLUMN env_digest TEXT;
			ALTER TABLE runs ADD COLUMN vcs TEXT;              -- commit for vcs_context: exact, "dirty", or NULL
			CREATE INDEX runs_task ON runs(repo_root, task_id, started_at);
			PRAGMA user_version = 2;`); err != nil {
			return err
		}
	}
	if v < 3 { // explain + config trust
		if _, err := db.Exec(`
			ALTER TABLE runs ADD COLUMN env_inputs TEXT;   -- JSON list of human-readable env inputs (no values)
			CREATE TABLE trusted_configs (                 -- one row per repo, replaced on re-trust
				repo_root  TEXT PRIMARY KEY,
				digest     TEXT NOT NULL,                  -- sha256 of the tasks.yaml bytes trusted
				trusted_at INTEGER NOT NULL
			);
			PRAGMA user_version = 3;`); err != nil {
			return err
		}
	}
	if v < 4 { // clean-checkout runs (plan §5.2)
		if _, err := db.Exec(`
			ALTER TABLE runs ADD COLUMN capture TEXT;  -- "workspace" | "clean"; NULL outside a repo
			PRAGMA user_version = 4;`); err != nil {
			return err
		}
	}
	if v < 5 { // tool-cache detection (plan §4.5)
		if _, err := db.Exec(`
			ALTER TABLE runs ADD COLUMN result_origin TEXT;  -- fresh | tool_cache; NULL = unknown (never eligible)
			PRAGMA user_version = 5;`); err != nil {
			return err
		}
	}
	return nil
}

// TrustConfig records that the user allows CInema to run the commands in the
// tasks.yaml with this digest for this repo (like `direnv allow`).
func (s *Store) TrustConfig(root, digest string) error {
	_, err := s.db.Exec(`INSERT INTO trusted_configs (repo_root, digest, trusted_at) VALUES (?, ?, ?)
		ON CONFLICT(repo_root) DO UPDATE SET digest=excluded.digest, trusted_at=excluded.trusted_at`,
		root, digest, s.now().UnixMilli())
	return err
}

// Trusted reports whether exactly this tasks.yaml content was trusted for root.
func (s *Store) Trusted(root, digest string) bool {
	var d string
	return s.db.QueryRow(`SELECT digest FROM trusted_configs WHERE repo_root=?`, root).Scan(&d) == nil && d == digest
}

// Source is where a run happened and the tree it saw. Zero value = outside a repo
// or fingerprinting failed: the run is kept as history and is never eligible.
type Source struct {
	RepoRoot, CwdRel string
	Before, After    string
	Capture          string // "workspace" (dev checkout) or "clean" (fresh checkout of a commit)
}

// Match is the task a run was matched to, with the digests captured when it ran.
// Zero value = no task matched: history only.
type Match struct {
	TaskID, ContractDigest, EnvDigest, VCS string
	EnvInputs                              []string // human-readable, for explain; never env values
	Origin                                 string   // fresh | tool_cache
}

// BeginRun records that a run is starting. Until FinishRun commits, the row is
// StateRecording and can never be treated as a result.
func (s *Store) BeginRun(argv []string, cwd string) (string, error) {
	id, err := newID(s.now())
	if err != nil {
		return "", err
	}
	a, _ := json.Marshal(argv)
	_, err = s.db.Exec(`INSERT INTO runs (id, state, pid, argv, cwd, started_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, StateRecording, os.Getpid(), string(a), cwd, s.now().UnixMilli())
	return id, err
}

// FinishRun makes output durable first, then flips the state in one statement,
// so a crash at any point leaves either StateRecording (→ incomplete) or a complete row.
func (s *Store) FinishRun(id string, r record.Result, src Source, m Match) (string, error) {
	outBlob, err := s.putBlob(r.Stdout.Bytes())
	if err != nil {
		return "", err
	}
	errBlob, err := s.putBlob(r.Stderr.Bytes())
	if err != nil {
		return "", err
	}
	state := StateFailed
	if r.ExitCode == 0 && r.Signal == "" && !r.Canceled {
		state = StatePassed
	}
	res, err := s.db.Exec(`UPDATE runs SET state=?, path=?, started_at=?, duration_ms=?, exit_code=?, signal=?,
		canceled=?, leftovers=?, stdout_blob=?, stdout_total=?, stdout_truncated=?,
		stderr_blob=?, stderr_total=?, stderr_truncated=?,
		repo_root=?, cwd_rel=?, source_before=?, source_after=?,
		task_id=?, contract_digest=?, env_digest=?, vcs=?, env_inputs=?, capture=?, result_origin=? WHERE id=? AND state=?`,
		state, r.Path, r.Start.UnixMilli(), r.Duration.Milliseconds(), r.ExitCode, nullStr(r.Signal),
		r.Canceled, r.Leftovers, outBlob, r.Stdout.Total, r.Stdout.Truncated,
		errBlob, r.Stderr.Total, r.Stderr.Truncated,
		nullStr(src.RepoRoot), nullStr(src.CwdRel), nullStr(src.Before), nullStr(src.After),
		nullStr(m.TaskID), nullStr(m.ContractDigest), nullStr(m.EnvDigest), nullStr(m.VCS), jsonOrNull(m.EnvInputs), nullStr(src.Capture), nullStr(m.Origin), id, StateRecording)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", fmt.Errorf("store: run %s is no longer recording", id)
	}
	return state, nil
}

// TaskRun is a recorded run of one task, newest first from TaskRuns.
type TaskRun struct {
	ID, State                 string
	Started                   time.Time
	DurationMS                int64
	ExitCode                  int
	Contract, Env, VCS        string
	SourceBefore, SourceAfter string
	EnvInputs                 []string
	Origin                    string
}

// TaskRuns returns the newest runs of taskID in repo root.
func (s *Store) TaskRuns(root, taskID string, limit int) ([]TaskRun, error) {
	rows, err := s.db.Query(`SELECT id, state, started_at, coalesce(duration_ms,0), coalesce(exit_code,-1),
		coalesce(contract_digest,''), coalesce(env_digest,''), coalesce(vcs,''),
		coalesce(source_before,''), coalesce(source_after,''), coalesce(env_inputs,''), coalesce(result_origin,'')
		FROM runs WHERE repo_root=? AND task_id=? AND state != ? ORDER BY started_at DESC, id DESC LIMIT ?`,
		root, taskID, StateRecording, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskRun
	for rows.Next() {
		var r TaskRun
		var started int64
		var inputs string
		if err := rows.Scan(&r.ID, &r.State, &started, &r.DurationMS, &r.ExitCode, &r.Contract, &r.Env,
			&r.VCS, &r.SourceBefore, &r.SourceAfter, &inputs, &r.Origin); err != nil {
			return nil, err
		}
		r.Started = time.UnixMilli(started)
		if inputs != "" {
			_ = json.Unmarshal([]byte(inputs), &r.EnvInputs)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunRow is one run as the dashboard lists it (any run in the repo, task or not).
type RunRow struct {
	ID         string    `json:"id"`
	State      string    `json:"state"`
	TaskID     string    `json:"task_id,omitempty"`
	Argv       []string  `json:"argv"`
	CwdRel     string    `json:"cwd"`
	Started    time.Time `json:"started_at"`
	DurationMS int64     `json:"duration_ms"`
	ExitCode   int       `json:"exit_code"`
	Signal     string    `json:"signal,omitempty"`
	Capture    string    `json:"capture,omitempty"`
	Mutated    bool      `json:"changed_source"` // source_before != source_after
	Source     string    `json:"source,omitempty"`

	stdoutBlob, stderrBlob string
	StdoutTotal            int64 `json:"stdout_total"`
	StderrTotal            int64 `json:"stderr_total"`
	StdoutTruncated        bool  `json:"stdout_truncated"`
	StderrTruncated        bool  `json:"stderr_truncated"`
}

const runRowCols = `id, state, coalesce(task_id,''), argv, coalesce(cwd_rel,''), started_at,
	coalesce(duration_ms,0), coalesce(exit_code,-1), coalesce(signal,''), coalesce(capture,''),
	coalesce(source_before,'') != coalesce(source_after,''), coalesce(source_before,''),
	coalesce(stdout_blob,''), coalesce(stderr_blob,''), coalesce(stdout_total,0), coalesce(stderr_total,0),
	coalesce(stdout_truncated,0), coalesce(stderr_truncated,0)`

func scanRunRow(sc interface{ Scan(...any) error }) (RunRow, error) {
	var r RunRow
	var argv string
	var started int64
	err := sc.Scan(&r.ID, &r.State, &r.TaskID, &argv, &r.CwdRel, &started, &r.DurationMS, &r.ExitCode,
		&r.Signal, &r.Capture, &r.Mutated, &r.Source, &r.stdoutBlob, &r.stderrBlob,
		&r.StdoutTotal, &r.StderrTotal, &r.StdoutTruncated, &r.StderrTruncated)
	r.Started = time.UnixMilli(started)
	_ = json.Unmarshal([]byte(argv), &r.Argv)
	return r, err
}

// RecentRuns lists the newest runs recorded in repo root.
func (s *Store) RecentRuns(root string, limit int) ([]RunRow, error) {
	rows, err := s.db.Query(`SELECT `+runRowCols+` FROM runs WHERE repo_root=?
		ORDER BY started_at DESC, id DESC LIMIT ?`, root, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunRow
	for rows.Next() {
		r, err := scanRunRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Run returns one run of repo root with its captured output ("" when expired or empty).
func (s *Store) Run(root, id string) (r RunRow, stdout, stderr []byte, err error) {
	r, err = scanRunRow(s.db.QueryRow(`SELECT `+runRowCols+` FROM runs WHERE repo_root=? AND id=?`, root, id))
	if err != nil {
		return r, nil, nil, err
	}
	if r.stdoutBlob != "" {
		stdout, _ = s.Blob(r.stdoutBlob)
	}
	if r.stderrBlob != "" {
		stderr, _ = s.Blob(r.stderrBlob)
	}
	return r, stdout, stderr, nil
}

// Signature changes whenever a run in root starts, finishes, or is cleaned up.
func (s *Store) Signature(root string) string {
	var n, recording, last int64
	_ = s.db.QueryRow(`SELECT count(*), coalesce(sum(state=?),0), coalesce(max(started_at),0) FROM runs WHERE repo_root=?`,
		StateRecording, root).Scan(&n, &recording, &last)
	return fmt.Sprintf("%d/%d/%d", n, recording, last)
}

// AbortRun deletes a run whose child never started (e.g. command not found).
func (s *Store) AbortRun(id string) error {
	_, err := s.db.Exec(`DELETE FROM runs WHERE id=? AND state=?`, id, StateRecording)
	return err
}

// recoverIncomplete marks StateRecording rows as incomplete when their recorder
// is gone, or is too old to plausibly still be the same process (PID reuse).
func (s *Store) recoverIncomplete() error {
	rows, err := s.db.Query(`SELECT id, pid, started_at FROM runs WHERE state=?`, StateRecording)
	if err != nil {
		return err
	}
	var dead []string
	cutoff := s.now().Add(-24 * time.Hour).UnixMilli()
	for rows.Next() {
		var id string
		var pid, started int64
		if err := rows.Scan(&id, &pid, &started); err != nil {
			rows.Close()
			return err
		}
		if started < cutoff || errors.Is(syscall.Kill(int(pid), 0), syscall.ESRCH) {
			dead = append(dead, id)
		}
	}
	rows.Close()
	for _, id := range dead {
		if _, err := s.db.Exec(`UPDATE runs SET state=? WHERE id=? AND state=?`, StateIncomplete, id, StateRecording); err != nil {
			return err
		}
	}
	return rows.Err()
}

// putBlob stores data under blobs/sha256/ab/<hex> via temp file + fsync + rename.
// Empty data is not stored (returns NULL).
func (s *Store) putBlob(data []byte) (any, error) {
	if len(data) == 0 {
		return nil, nil
	}
	d := digest(data)
	final := s.blobPath(d)
	if _, err := os.Stat(final); err == nil {
		now := s.now()
		_ = os.Chtimes(final, now, now) // fresh mtime keeps it safe from the orphan sweep
		return d, nil
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Join(s.dir, "blobs", "tmp"), "*.tmp")
	if err != nil {
		return nil, err
	}
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(f.Name(), final)
	}
	if werr != nil {
		os.Remove(f.Name())
		return nil, werr
	}
	return d, nil
}

// Blob returns stored output by digest.
func (s *Store) Blob(d string) ([]byte, error) { return os.ReadFile(s.blobPath(d)) }

func (s *Store) blobPath(d string) string {
	return filepath.Join(s.dir, "blobs", "sha256", d[:2], d)
}

// sweepTmp removes temp files left by a crash mid-write (plan §13.1, CLEAN-003).
func (s *Store) sweepTmp() {
	tmp := filepath.Join(s.dir, "blobs", "tmp")
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && s.now().Sub(fi.ModTime()) > time.Hour {
			os.Remove(filepath.Join(tmp, e.Name()))
		}
	}
}

func newID(t time.Time) (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("run_%d_%s", t.UnixMilli(), hex.EncodeToString(b)), nil
}

func jsonOrNull(v []string) any {
	if len(v) == 0 {
		return nil
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
