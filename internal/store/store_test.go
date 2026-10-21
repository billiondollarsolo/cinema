package store

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/billiondollarsolo/cinema/internal/record"
)

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func result(code int, out string) record.Result {
	r := record.Result{Start: time.Now(), ExitCode: code,
		Stdout: record.Capture{Limit: 1 << 20}, Stderr: record.Capture{Limit: 1 << 20}}
	r.Stdout.Write([]byte(out))
	return r
}

func addRun(t *testing.T, s *Store, code int, out string) string {
	t.Helper()
	id, err := s.BeginRun([]string{"go", "test"}, "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishRun(id, result(code, out)); err != nil {
		t.Fatal(err)
	}
	return id
}

func (s *Store) col(t *testing.T, id, col string) any {
	t.Helper()
	var v any
	if err := s.db.QueryRow(`SELECT `+col+` FROM runs WHERE id=?`, id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestLifecycle(t *testing.T) {
	s := open(t, t.TempDir())
	pass := addRun(t, s, 0, "ok\n")
	fail := addRun(t, s, 1, "boom\n")
	if s.col(t, pass, "state") != StatePassed || s.col(t, fail, "state") != StateFailed {
		t.Fatalf("states %v %v", s.col(t, pass, "state"), s.col(t, fail, "state"))
	}
	b, err := s.Blob(s.col(t, fail, "stdout_blob").(string))
	if err != nil || string(b) != "boom\n" {
		t.Fatalf("blob %q %v", b, err)
	}
	if s.col(t, pass, "stderr_blob") != nil {
		t.Fatal("empty stream stored a blob")
	}
	if _, err := s.FinishRun(pass, result(0, "")); err == nil {
		t.Fatal("finished a run twice")
	}
}

func TestCrashLeavesIncomplete(t *testing.T) { // ACC-066
	dir := t.TempDir()
	s := open(t, dir)
	id, _ := s.BeginRun([]string{"go", "test"}, "/repo")
	alive, _ := s.BeginRun([]string{"go", "test"}, "/repo") // our own PID: still running
	// Simulate a recorder that died: a PID that can't exist.
	if _, err := s.db.Exec(`UPDATE runs SET pid=999999999 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = open(t, dir)
	if got := s.col(t, id, "state"); got != StateIncomplete {
		t.Fatalf("dead recorder: %v", got)
	}
	if got := s.col(t, alive, "state"); got != StateRecording {
		t.Fatalf("live recorder: %v", got)
	}
	if _, err := s.FinishRun(id, result(0, "")); err == nil {
		t.Fatal("incomplete run became finishable")
	}
}

func TestConcurrentWriters(t *testing.T) { // ACC-051
	dir := t.TempDir()
	open(t, dir) // create schema once
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			s, err := Open(dir) // separate connection pool, like separate processes
			if err != nil {
				t.Error(err)
				return
			}
			defer s.Close()
			for range 15 {
				id, err := s.BeginRun([]string{"x"}, "/r")
				if err == nil {
					_, err = s.FinishRun(id, result(0, "same output"))
				}
				if err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	var n int
	open(t, dir).db.QueryRow(`SELECT count(*) FROM runs WHERE state=?`, StatePassed).Scan(&n)
	if n != 120 {
		t.Fatalf("passed runs = %d, want 120", n)
	}
}

func TestSweepTmp(t *testing.T) { // CLEAN-003
	dir := t.TempDir()
	open(t, dir).Close()
	stale := filepath.Join(dir, "blobs", "tmp", "x.tmp")
	fresh := filepath.Join(dir, "blobs", "tmp", "y.tmp")
	os.WriteFile(stale, []byte("partial"), 0o600)
	os.WriteFile(fresh, []byte("in flight"), 0o600)
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(stale, old, old)
	open(t, dir)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale tmp survived")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh tmp (possibly in flight) was removed")
	}
}

func TestRetention(t *testing.T) { // CLEAN-004
	s := open(t, t.TempDir())
	pass := addRun(t, s, 0, "pass log")
	fail := addRun(t, s, 1, "fail log")
	shared := addRun(t, s, 0, "shared")
	passBlob := s.col(t, pass, "stdout_blob").(string)

	s.now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	// keep "shared" fresh: a newer run references the same blob
	if _, err := s.db.Exec(`UPDATE runs SET started_at=? WHERE id=?`, s.now().UnixMilli(), shared); err != nil {
		t.Fatal(err)
	}
	st, err := s.Maintain(true)
	if err != nil {
		t.Fatal(err)
	}
	if s.col(t, pass, "stdout_blob") != nil {
		t.Error("passing log older than 7d kept")
	}
	if s.col(t, fail, "stdout_blob") == nil {
		t.Error("failing log younger than 30d dropped")
	}
	if s.col(t, shared, "stdout_blob") == nil {
		t.Error("fresh run lost its log")
	}
	if _, err := s.Blob(passBlob); !os.IsNotExist(err) || st.BlobsDeleted != 1 {
		t.Errorf("orphan blob: err=%v deleted=%d", err, st.BlobsDeleted)
	}

	s.now = func() time.Time { return time.Now().Add(91 * 24 * time.Hour) }
	if st, _ := s.Maintain(true); st.RunsDeleted != 2 { // "shared" is only 83 days old
		t.Errorf("runs older than 90d: deleted %d, want 2", st.RunsDeleted)
	}
}

func TestSizeCapEvictsPassingFirst(t *testing.T) { // CLEAN-005
	s := open(t, t.TempDir())
	s.Policy.MaxBlobBytes = 25
	oldPass := addRun(t, s, 0, "0123456789") // 10 bytes each
	fail := addRun(t, s, 1, "abcdefghij")
	newPass := addRun(t, s, 0, "ABCDEFGHIJ")
	_ = addRun(t, s, 1, "klmnopqrst")
	st, err := s.Maintain(true)
	if err != nil {
		t.Fatal(err)
	}
	if st.LogsEvicted != 2 || s.col(t, oldPass, "stdout_blob") != nil || s.col(t, newPass, "stdout_blob") != nil {
		t.Fatalf("evicted %d; old pass %v new pass %v", st.LogsEvicted,
			s.col(t, oldPass, "stdout_blob"), s.col(t, newPass, "stdout_blob"))
	}
	if s.col(t, fail, "stdout_blob") == nil {
		t.Fatal("failing log evicted before passing logs")
	}
	if s.col(t, oldPass, "state") != StatePassed {
		t.Fatal("run metadata dropped by the size cap")
	}
}

func TestMaintainOncePerDay(t *testing.T) {
	s := open(t, t.TempDir())
	if st, _ := s.Maintain(false); st.Skipped {
		t.Fatal("first run skipped")
	}
	if st, _ := s.Maintain(false); !st.Skipped {
		t.Fatal("second run within 24h not skipped")
	}
	s.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	if st, _ := s.Maintain(false); st.Skipped {
		t.Fatal("run after 24h skipped")
	}
}

func TestUnwritableDir(t *testing.T) { // ACC-006 (store side; CLI keeps the child's exit code)
	dir := t.TempDir()
	os.Chmod(dir, 0o500)
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if _, err := Open(filepath.Join(dir, "sub")); err == nil {
		t.Fatal("opened a store in an unwritable dir")
	}
}
