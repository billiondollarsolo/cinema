package snapshot

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q", "-b", "main")
	write(t, dir, ".gitignore", "*.log\n.env.test\n")
	write(t, dir, "main.go", "package main\n")
	write(t, dir, "lib/util.go", "package lib\n")
	mustGit(t, dir, "add", "-A")
	commit(t, dir)
	return dir
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_ALLOW_PROTOCOL=file")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func commit(t *testing.T, dir string) { mustGit(t, dir, "commit", "-qm", "c", "--allow-empty") }

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func take(t *testing.T, dir string, extra ...string) string {
	t.Helper()
	s, err := Take(dir, extra, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s.Digest
}

// Each mutation must change the fingerprint; undoing it must restore it.
func TestSensitivity(t *testing.T) { // ACC-009, 012, 013, 015
	dir := gitRepo(t)
	base := take(t, dir)
	for _, tc := range []struct {
		name       string
		do, undo   func()
		wantChange bool
	}{
		{"edit tracked file", func() { write(t, dir, "main.go", "package main\n// x\n") },
			func() { write(t, dir, "main.go", "package main\n") }, true},
		{"untracked file", func() { write(t, dir, "new.go", "package main\n") },
			func() { os.Remove(filepath.Join(dir, "new.go")) }, true},
		{"ignored file", func() { write(t, dir, "debug.log", "noise") },
			func() { os.Remove(filepath.Join(dir, "debug.log")) }, false},
		{"exec bit", func() { os.Chmod(filepath.Join(dir, "main.go"), 0o755) },
			func() { os.Chmod(filepath.Join(dir, "main.go"), 0o644) }, true},
		{"symlink", func() { os.Symlink("main.go", filepath.Join(dir, "link")) },
			func() { os.Remove(filepath.Join(dir, "link")) }, true},
		{"delete tracked file", func() { os.Remove(filepath.Join(dir, "lib/util.go")) },
			func() { write(t, dir, "lib/util.go", "package lib\n") }, true},
		{"rename", func() { os.Rename(filepath.Join(dir, "lib/util.go"), filepath.Join(dir, "lib/util2.go")) },
			func() { os.Rename(filepath.Join(dir, "lib/util2.go"), filepath.Join(dir, "lib/util.go")) }, true},
	} {
		tc.do()
		got := take(t, dir)
		tc.undo()
		if (got != base) != tc.wantChange {
			t.Errorf("%s: changed=%v, want %v", tc.name, got != base, tc.wantChange)
		}
		if again := take(t, dir); again != base {
			t.Fatalf("%s: undo didn't restore the fingerprint", tc.name)
		}
	}
	// Symlink retarget is a change even though the link path is the same.
	os.Symlink("main.go", filepath.Join(dir, "link"))
	a := take(t, dir)
	os.Remove(filepath.Join(dir, "link"))
	os.Symlink("lib/util.go", filepath.Join(dir, "link"))
	if take(t, dir) == a {
		t.Error("symlink retarget not detected")
	}
}

func TestTaskConfigExcluded(t *testing.T) { // ADR 0001
	dir := gitRepo(t)
	base := take(t, dir)
	write(t, dir, ".cinema/tasks.yaml", "schema_version: 1\n")
	if take(t, dir) != base {
		t.Fatal(".cinema/tasks.yaml affected the fingerprint")
	}
	if take(t, dir, ".cinema/tasks.yaml") == base {
		t.Fatal("declared as an extra input, it must be fingerprinted")
	}
	write(t, dir, ".cinema/other.yaml", "x")
	if take(t, dir) == base {
		t.Fatal("only tasks.yaml is excluded, not all of .cinema/")
	}
}

func TestExtraInputs(t *testing.T) {
	dir := gitRepo(t)
	missing := take(t, dir, ".env.test")
	if missing == take(t, dir) {
		t.Error("declared-but-missing input doesn't affect the fingerprint")
	}
	write(t, dir, ".env.test", "A=1")
	one := take(t, dir, ".env.test")
	write(t, dir, ".env.test", "A=2")
	if take(t, dir, ".env.test") == one || one == missing {
		t.Error("declared ignored input not fingerprinted")
	}
	for _, bad := range []string{"../x", "/etc/passwd", "a/../../x", ".git/config", ".", ""} {
		if _, err := Take(dir, []string{bad}, nil, nil); err == nil {
			t.Errorf("extra input %q accepted", bad)
		}
	}
}

func TestDirtyTreeMatchesLaterCommit(t *testing.T) { // ACC-011 (content, not commit IDs)
	dir := gitRepo(t)
	write(t, dir, "feature.go", "package main\n")
	dirty := take(t, dir)
	mustGit(t, dir, "add", "-A")
	commit(t, dir)
	if take(t, dir) != dirty {
		t.Fatal("committing identical bytes changed the fingerprint")
	}
}

func TestSubmodule(t *testing.T) { // ACC-013
	sub := gitRepo(t)
	dir := gitRepo(t)
	mustGit(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "vendor/sub")
	commit(t, dir)
	before := take(t, dir)
	write(t, sub, "more.go", "package main\n")
	mustGit(t, sub, "add", "-A")
	commit(t, sub)
	mustGit(t, filepath.Join(dir, "vendor/sub"), "-c", "protocol.file.allow=always", "pull", "-q", "origin", "main")
	if take(t, dir) == before {
		t.Fatal("submodule checkout moved but fingerprint didn't change")
	}

	// A fresh clone has the submodule uninitialized: an empty dir. It must not
	// pick up the parent's HEAD, so a new parent commit leaves it unchanged.
	clone := t.TempDir()
	mustGit(t, clone, "clone", "-q", dir, ".")
	s1, err := Take(clone, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for _, e := range s1.Entries {
		if e.Path == "vendor/sub" {
			got = e.Target
		}
	}
	if got != "uninitialized" {
		t.Fatalf("uninitialized submodule target = %q", got)
	}
	commit(t, clone)
	if take(t, clone) != s1.Digest {
		t.Fatal("parent commit changed the fingerprint via an uninitialized submodule")
	}
}

func TestRepoRoot(t *testing.T) {
	dir := gitRepo(t)
	real, _ := filepath.EvalSymlinks(dir)
	if got, err := RepoRoot(filepath.Join(dir, "lib")); err != nil || got != real {
		t.Fatalf("RepoRoot = %q, %v; want %q", got, err, real)
	}
	if _, err := RepoRoot(t.TempDir()); err != ErrNotRepo {
		t.Fatalf("outside a repo: %v", err)
	}
}

func TestCache(t *testing.T) {
	dir := gitRepo(t)
	path := filepath.Join(t.TempDir(), "c.gob")
	// Age the files past the racy window so the cache may trust them.
	old := time.Now().Add(-time.Minute)
	for _, f := range []string{"main.go", "lib/util.go", ".gitignore"} {
		os.Chtimes(filepath.Join(dir, f), old, old)
	}
	c := LoadCache(path)
	cold, _ := Take(dir, nil, nil, c)
	c.Save(path)
	c = LoadCache(path)
	warm, _ := Take(dir, nil, nil, c)
	if warm.Digest != cold.Digest || warm.Hashed != 0 || cold.Hashed != 3 {
		t.Fatalf("cold hashed=%d warm hashed=%d equal=%v", cold.Hashed, warm.Hashed, warm.Digest == cold.Digest)
	}

	// Same size, mtime forced back: ctime still changes, so the edit is seen.
	write(t, dir, "main.go", "package mian\n")
	os.Chtimes(filepath.Join(dir, "main.go"), old, old)
	if s, _ := Take(dir, nil, nil, c); s.Digest == cold.Digest {
		t.Fatal("same-size edit with restored mtime was served from cache")
	}
}

func TestRacyEntryNotTrusted(t *testing.T) {
	dir := gitRepo(t)
	c := LoadCache(filepath.Join(t.TempDir(), "c.gob"))
	Take(dir, nil, nil, c) // files were just written: inside the racy window
	if s, _ := Take(dir, nil, nil, c); s.Hashed == 0 {
		t.Fatal("freshly modified files were trusted from cache")
	}
}

func FuzzCleanRel(f *testing.F) {
	for _, s := range []string{"a/b", "../x", "/abs", "a/../../b", ".git/x", "a//b/./c", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		c, err := CleanRel(p)
		if err != nil {
			return
		}
		if strings.HasPrefix(c, "/") || c == ".." || strings.HasPrefix(c, "../") || strings.Contains(c, "/../") ||
			c == ".git" || strings.HasPrefix(c, ".git/") {
			t.Fatalf("CleanRel(%q) = %q escapes", p, c)
		}
	})
}

// The equality the whole push → gate flow depends on: a clean working tree and
// the commit it's at have the same fingerprint.
func TestCommitMatchesCleanTree(t *testing.T) {
	dir := gitRepo(t)
	os.Symlink("main.go", filepath.Join(dir, "link"))
	os.Chmod(filepath.Join(dir, "lib/util.go"), 0o755)
	write(t, dir, ".cinema/tasks.yaml", "schema_version: 1\n") // excluded in both
	write(t, dir, "deep/a/b/c.txt", strings.Repeat("x", 100000))
	mustGit(t, dir, "add", "-A")
	commit(t, dir)
	blobs := NewBlobCache()
	for _, extra := range [][]string{nil, {".env.test"}} {
		wt, err := Take(dir, extra, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		c, err := TakeCommit(dir, "HEAD", extra, nil, blobs)
		if err != nil {
			t.Fatal(err)
		}
		if wt.Digest != c.Digest {
			t.Fatalf("extra=%v: working tree %s != commit %s", extra, wt.Digest, c.Digest)
		}
	}
	// Warm blob cache: nothing re-read except symlink targets.
	c, _ := TakeCommit(dir, "HEAD", nil, nil, blobs)
	if c.Hashed != 1 {
		t.Fatalf("warm: hashed %d, want 1 (the symlink)", c.Hashed)
	}
	// A different commit has a different fingerprint.
	write(t, dir, "main.go", "package main // v2\n")
	mustGit(t, dir, "commit", "-qam", "v2")
	c2, _ := TakeCommit(dir, "HEAD", nil, nil, blobs)
	prev, _ := TakeCommit(dir, "HEAD~1", nil, nil, blobs)
	if c2.Digest == prev.Digest {
		t.Fatal("commits with different content share a fingerprint")
	}
}

func TestExcludeOutputs(t *testing.T) {
	dir := gitRepo(t)
	base := take(t, dir)
	write(t, dir, "cover-profile.out", "mode: set\n")
	write(t, dir, "coverage/a/b.html", "x")
	got, err := Take(dir, nil, []string{"cover-profile.out", "coverage"}, nil)
	if err != nil || got.Digest != base {
		t.Fatalf("declared outputs still affect the fingerprint: %v", err)
	}
	if take(t, dir) == base {
		t.Fatal("undeclared, unignored files must still count")
	}
	if g, _ := Take(dir, nil, []string{"cover"}, nil); g.Digest == base {
		t.Fatal("exclude must match whole path components, not prefixes")
	}
	if _, err := Take(dir, nil, []string{"../x"}, nil); err == nil {
		t.Fatal("exclude escaping the root accepted")
	}
}
