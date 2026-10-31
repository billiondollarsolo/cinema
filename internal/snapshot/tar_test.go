package snapshot

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// volumeTar builds the stream task.CheckoutVolume.Snapshot's helper writes:
// git ls-files' paths, each then "<path>/.git", then extras and tasks.yaml,
// with whatever doesn't exist skipped (tar --ignore-failed-read).
func volumeTar(t *testing.T, dir string, untracked bool, extra ...string) io.Reader {
	t.Helper()
	args := []string{"-C", dir, "ls-files", "-z", "--cached"}
	if untracked {
		args = append(args, "--others", "--exclude-standard")
	}
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		t.Fatal(err)
	}
	listed := strings.FieldsFunc(string(out), func(r rune) bool { return r == 0 })
	var names []string
	names = append(names, listed...)
	for _, p := range listed {
		names = append(names, strings.TrimRight(p, "/")+"/.git")
	}
	names = append(append(names, extra...), excluded)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, n := range names {
		full := filepath.Join(dir, n)
		fi, err := os.Lstat(full)
		if err != nil {
			continue
		}
		link := ""
		if fi.Mode()&os.ModeSymlink != 0 {
			link, _ = os.Readlink(full)
		}
		h, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			t.Fatal(err)
		}
		h.Name = n
		tw.WriteHeader(h)
		if fi.Mode().IsRegular() {
			b, _ := os.ReadFile(full)
			tw.Write(b)
		}
	}
	tw.Close()
	return &buf
}

// The volume's fingerprint is Take's, computed on a tar of the same files:
// a run on a checkout in a volume and CI's checkout of the same commit must
// match (or no receipt ever would), and a change must still show.
func TestTakeTarMatchesTake(t *testing.T) {
	dir := gitRepo(t)
	os.Symlink("main.go", filepath.Join(dir, "link"))
	os.Chmod(filepath.Join(dir, "lib/util.go"), 0o755)
	write(t, dir, ".cinema/tasks.yaml", "schema_version: 1\n")
	write(t, dir, "sub/.keep", "")
	mustGit(t, dir, "add", "-A")
	commit(t, dir)
	write(t, dir, "untracked.txt", "u")   // counts before a run
	write(t, dir, "debug.log", "ignored") // never
	write(t, dir, ".env.test", "SECRET=1")
	for _, tc := range []struct {
		extra, exclude []string
	}{{nil, nil}, {[]string{".env.test", "missing.cfg"}, nil}, {nil, []string{"lib"}}} {
		want, err := Take(dir, tc.extra, tc.exclude, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, cfg, err := TakeTar(volumeTar(t, dir, true, tc.extra...), tc.extra, tc.exclude)
		if err != nil {
			t.Fatal(err)
		}
		if got.Digest != want.Digest {
			t.Fatalf("extra=%v exclude=%v: volume %s != tree %s\n%v\n%v", tc.extra, tc.exclude, got.Digest, want.Digest, got.Entries, want.Entries)
		}
		if cfg == "" {
			t.Fatal("tasks.yaml's hash missing: a run rewriting it would go unnoticed")
		}
	}
	// After a run: tracked files only. A modified one shows, a new one doesn't.
	before, _, _ := TakeTar(volumeTar(t, dir, false), nil, nil)
	write(t, dir, "made-by-run.txt", "x")
	if s, _, _ := TakeTar(volumeTar(t, dir, false), nil, nil); s.Digest != before.Digest {
		t.Fatal("a file the run created counted as a source change")
	}
	write(t, dir, "main.go", "package main // changed\n")
	os.Remove(filepath.Join(dir, "link"))
	after, _, _ := TakeTar(volumeTar(t, dir, false), nil, nil)
	if after.Digest == before.Digest {
		t.Fatal("a modified tracked file didn't change the volume's fingerprint")
	}
	if got := strings.Join(Changed(before, after), ","); got != "link,main.go" {
		t.Fatalf("changed: %s", got)
	}
}

// A submodule directory: "uninitialized" like Take when empty; initialized
// (its .git present) it differs, as it does for Take.
func TestTakeTarSubmoduleSentinel(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "vendor/x", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.Close()
	empty, _, err := TakeTar(bytes.NewReader(buf.Bytes()), nil, nil)
	if err != nil || len(empty.Entries) != 1 || empty.Entries[0].Target != "uninitialized" {
		t.Fatalf("%v %+v", err, empty)
	}
	buf.Reset()
	tw = tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "vendor/x", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "vendor/x/.git", Typeflag: tar.TypeReg, Mode: 0o644})
	tw.Close()
	inited, _, _ := TakeTar(bytes.NewReader(buf.Bytes()), nil, nil)
	if len(inited.Entries) != 1 || inited.Digest == empty.Digest {
		t.Fatalf("initialized submodule: %+v", inited.Entries)
	}
}
