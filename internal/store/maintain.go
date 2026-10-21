package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Policy is retention (plan §13) plus the data-dir size cap (plan §13.1).
type Policy struct {
	RunMaxAge     time.Duration // run metadata
	PassLogMaxAge time.Duration // output of passed runs
	FailLogMaxAge time.Duration // output of failed / incomplete runs
	MaxBlobBytes  int64         // past this, evict oldest passing logs, then failing
}

var DefaultPolicy = Policy{
	RunMaxAge:     90 * 24 * time.Hour,
	PassLogMaxAge: 7 * 24 * time.Hour,
	FailLogMaxAge: 30 * 24 * time.Hour,
	MaxBlobBytes:  2 << 30,
}

type MaintainStats struct {
	Skipped      bool  `json:"skipped"` // ran within the last 24 h
	RunsDeleted  int64 `json:"runs_deleted"`
	LogsExpired  int64 `json:"logs_expired"`
	LogsEvicted  int64 `json:"logs_evicted"` // dropped by the size cap
	BlobsDeleted int   `json:"blobs_deleted"`
	BytesFreed   int64 `json:"bytes_freed"`
}

const maintainEvery = 24 * time.Hour

// Maintain applies retention and the size cap, then deletes blobs nothing
// references. Without force it runs at most once per 24 h, so every command can
// call it cheaply (plan §13.1).
func (s *Store) Maintain(force bool) (MaintainStats, error) {
	var st MaintainStats
	now := s.now()
	if !force {
		var v string
		err := s.db.QueryRow(`SELECT v FROM meta WHERE k='last_maintain'`).Scan(&v)
		if last, _ := strconv.ParseInt(v, 10, 64); err == nil && now.Sub(time.UnixMilli(last)) < maintainEvery {
			st.Skipped = true
			return st, nil
		}
	}
	ago := func(d time.Duration) int64 { return now.Add(-d).UnixMilli() }
	p := s.Policy

	err := s.tx(func(tx *sql.Tx) error {
		r, err := tx.Exec(`DELETE FROM runs WHERE state != ? AND started_at < ?`, StateRecording, ago(p.RunMaxAge))
		if err != nil {
			return err
		}
		st.RunsDeleted, _ = r.RowsAffected()
		r, err = tx.Exec(`UPDATE runs SET stdout_blob=NULL, stderr_blob=NULL
			WHERE (stdout_blob IS NOT NULL OR stderr_blob IS NOT NULL) AND (
				(state = ? AND started_at < ?) OR (state IN (?, ?) AND started_at < ?))`,
			StatePassed, ago(p.PassLogMaxAge), StateFailed, StateIncomplete, ago(p.FailLogMaxAge))
		if err != nil {
			return err
		}
		st.LogsExpired, _ = r.RowsAffected()
		return nil
	})
	if err != nil {
		return st, err
	}

	files := s.blobFiles()
	if st.LogsEvicted, err = s.enforceCap(files); err != nil {
		return st, err
	}
	refs, err := s.referenced()
	if err != nil {
		return st, err
	}
	for d, f := range files {
		// The age check protects blobs written by a FinishRun that hasn't committed yet.
		if !refs[d] && now.Sub(f.mtime) > time.Hour {
			if os.Remove(f.path) == nil {
				st.BlobsDeleted++
				st.BytesFreed += f.size
			}
		}
	}
	_, err = s.db.Exec(`INSERT INTO meta (k, v) VALUES ('last_maintain', ?)
		ON CONFLICT(k) DO UPDATE SET v=excluded.v`, strconv.FormatInt(now.UnixMilli(), 10))
	return st, err
}

// enforceCap drops logs, oldest passing runs first, then failing, until the
// referenced blobs fit in MaxBlobBytes. Run metadata is kept.
func (s *Store) enforceCap(files map[string]blobFile) (int64, error) {
	rows, err := s.db.Query(`SELECT id, coalesce(stdout_blob,''), coalesce(stderr_blob,'') FROM runs
		WHERE stdout_blob IS NOT NULL OR stderr_blob IS NOT NULL
		ORDER BY CASE state WHEN ? THEN 0 ELSE 1 END, started_at`, StatePassed)
	if err != nil {
		return 0, err
	}
	type run struct{ id, out, err string }
	var runs []run
	refcount := map[string]int{}
	var total int64
	for rows.Next() {
		var r run
		if err := rows.Scan(&r.id, &r.out, &r.err); err != nil {
			rows.Close()
			return 0, err
		}
		runs = append(runs, r)
		for _, d := range []string{r.out, r.err} {
			if d != "" {
				if refcount[d] == 0 {
					total += files[d].size
				}
				refcount[d]++
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || total <= s.Policy.MaxBlobBytes {
		return 0, err
	}
	var evicted int64
	err = s.tx(func(tx *sql.Tx) error {
		for _, r := range runs {
			if total <= s.Policy.MaxBlobBytes {
				return nil
			}
			if _, err := tx.Exec(`UPDATE runs SET stdout_blob=NULL, stderr_blob=NULL WHERE id=?`, r.id); err != nil {
				return err
			}
			evicted++
			for _, d := range []string{r.out, r.err} {
				if d != "" {
					if refcount[d]--; refcount[d] == 0 {
						total -= files[d].size
					}
				}
			}
		}
		return nil
	})
	return evicted, err
}

func (s *Store) referenced() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT stdout_blob FROM runs WHERE stdout_blob IS NOT NULL
		UNION SELECT stderr_blob FROM runs WHERE stderr_blob IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	refs := map[string]bool{}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		refs[d] = true
	}
	return refs, rows.Err()
}

type blobFile struct {
	path  string
	size  int64
	mtime time.Time
}

func (s *Store) blobFiles() map[string]blobFile {
	files := map[string]blobFile{}
	root := filepath.Join(s.dir, "blobs", "sha256")
	_ = filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return nil
		}
		if fi, err := e.Info(); err == nil {
			files[e.Name()] = blobFile{path, fi.Size(), fi.ModTime()}
		}
		return nil
	})
	return files
}

func (s *Store) tx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
