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
	stderr_truncated INTEGER
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
	if _, err := db.Exec(schema); err != nil {
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
func (s *Store) FinishRun(id string, r record.Result) (string, error) {
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
		stderr_blob=?, stderr_total=?, stderr_truncated=? WHERE id=? AND state=?`,
		state, r.Path, r.Start.UnixMilli(), r.Duration.Milliseconds(), r.ExitCode, nullStr(r.Signal),
		r.Canceled, r.Leftovers, outBlob, r.Stdout.Total, r.Stdout.Truncated,
		errBlob, r.Stderr.Total, r.Stderr.Truncated, id, StateRecording)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", fmt.Errorf("store: run %s is no longer recording", id)
	}
	return state, nil
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

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
