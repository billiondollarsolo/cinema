// Package snapshot computes the whole-source fingerprint of a git working tree
// (plan §5.1): every tracked file, every untracked non-ignored file, declared
// extra inputs, and submodule commits.
package snapshot

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Entry kinds. Part of the fingerprint encoding: never renumber.
const (
	KindFile      = 'f'
	KindExec      = 'x'
	KindSymlink   = 'l'
	KindSubmodule = 's'
	KindMissing   = 'm' // declared extra input that doesn't exist; absence is an input too
)

type Entry struct {
	Path   string   // slash-separated, relative to the repo root
	Kind   byte     //
	Digest [32]byte // SHA-256 of content (files only)
	Target string   // symlink target, or submodule commit
}

type Snapshot struct {
	Root    string
	Digest  string // "sha256:<hex>"
	Entries []Entry
	Hashed  int // files whose content was read this time (cache misses)
}

// Take fingerprints the working tree at root. extra are repo-relative paths of
// ignored files a task declares it reads. cache may be nil.
func Take(root string, extra []string, cache *Cache) (*Snapshot, error) {
	start := time.Now()
	paths, err := listFiles(root)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		seen[p] = true
	}
	extraSet := make(map[string]bool, len(extra))
	for _, p := range extra {
		clean, err := CleanRel(p)
		if err != nil {
			return nil, err
		}
		extraSet[clean] = true
		if !seen[clean] {
			seen[clean] = true
			paths = append(paths, clean)
		}
	}

	entries := make([]Entry, 0, len(paths))
	type job struct {
		i  int
		st stat
	}
	var toHash []job
	for _, p := range paths {
		e := Entry{Path: strings.TrimSuffix(p, "/")} // ls-files marks untracked nested repos "dir/"
		full := filepath.Join(root, filepath.FromSlash(e.Path))
		fi, err := os.Lstat(full)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if extraSet[p] {
				e.Kind = KindMissing
				entries = append(entries, e)
			}
			continue // tracked but deleted: absent from the manifest
		case err != nil:
			return nil, err
		case fi.Mode()&os.ModeSymlink != 0:
			e.Kind = KindSymlink
			if e.Target, err = os.Readlink(full); err != nil {
				return nil, err
			}
		case fi.Mode().IsRegular():
			e.Kind = KindFile
			if fi.Mode()&0o111 != 0 {
				e.Kind = KindExec
			}
			st := statOf(fi)
			if d, ok := cache.lookup(p, st); ok {
				e.Digest = d
			} else {
				toHash = append(toHash, job{len(entries), st})
			}
		case fi.IsDir():
			// ls-files lists a submodule (or untracked nested repo) as one path.
			e.Kind = KindSubmodule
			if e.Target, err = nestedHead(full); err != nil {
				return nil, err
			}
		default:
			continue // sockets, fifos
		}
		entries = append(entries, e)
	}

	// Hash cache misses in parallel.
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	jobs := make(chan job)
	for range runtime.NumCPU() {
		wg.Go(func() {
			for j := range jobs {
				e := &entries[j.i]
				d, err := hashFile(filepath.Join(root, filepath.FromSlash(e.Path)))
				if err != nil {
					mu.Lock()
					firstErr = cmpOr(firstErr, err)
					mu.Unlock()
					continue
				}
				e.Digest = d
				cache.store(e.Path, j.st, d, start)
			}
		})
	}
	for _, j := range toHash {
		jobs <- j
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	slices.SortFunc(entries, func(a, b Entry) int { return strings.Compare(a.Path, b.Path) })
	return &Snapshot{Root: root, Digest: "sha256:" + manifestDigest(entries), Entries: entries, Hashed: len(toHash)}, nil
}

// ErrNotRepo: dir is not inside a git working tree.
var ErrNotRepo = errors.New("snapshot: not inside a git repository")

// RepoRoot returns the top of the git working tree containing dir, with
// symlinks resolved. Walks up looking for .git (a dir, or a file for worktrees
// and submodules) instead of spawning git: this runs on every exec.
func RepoRoot(dir string) (string, error) {
	d, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", ErrNotRepo
		}
		d = parent
	}
}

// CleanRel validates a repo-relative path from task config: no absolute paths,
// no escaping the root, slash-separated.
func CleanRel(p string) (string, error) {
	if p == "" || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("snapshot: invalid path %q", p)
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("snapshot: path %q must be relative to the repo root", p)
	}
	c := filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("snapshot: path %q escapes the repo root", p)
	}
	if c == ".git" || strings.HasPrefix(c, ".git/") {
		return "", fmt.Errorf("snapshot: path %q is inside .git", p)
	}
	return c, nil
}

// excluded is the task config: what matters in it for a task is in that task's
// contract digest, so editing policy or other tasks mustn't invalidate runs.
// See docs/adr/0001. Declared extra inputs still include it if a task asks.
const excluded = ".cinema/tasks.yaml"

// listFiles returns tracked + untracked-non-ignored paths in one git call.
// Plain paths only: parsing --stage output alongside --others is ambiguous for
// adversarial file names.
func listFiles(root string) ([]string, error) {
	out, err := git(root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	var paths []string
	seen := map[string]bool{excluded: true}
	for p := range bytes.SplitSeq(out, []byte{0}) {
		if len(p) > 0 && !seen[string(p)] { // a conflicted file is listed once per stage
			seen[string(p)] = true
			paths = append(paths, string(p))
		}
	}
	return paths, nil
}

// nestedHead is the commit checked out in a submodule directory, which is what
// the build sees. A directory without its own .git is an uninitialized submodule:
// no content, and running git in it would report the PARENT repo's HEAD.
// ponytail: uncommitted changes inside a submodule are not fingerprinted;
// recurse into submodules if that ever matters.
func nestedHead(dir string) (string, error) {
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err != nil {
		return "uninitialized", nil
	}
	out, err := git(dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return string(bytes.TrimSpace(out)), nil
}

func git(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", args[0], err, bytes.TrimSpace(stderr.Bytes()))
	}
	return out, nil
}

func hashFile(path string) ([32]byte, error) {
	var d [32]byte
	f, err := os.Open(path)
	if err != nil {
		return d, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return d, err
	}
	h.Sum(d[:0])
	return d, nil
}

// manifestDigest: domain-separated, length-prefixed fields over entries sorted
// by path (plan §3.1, §5.1). Changing this encoding invalidates every receipt.
func manifestDigest(entries []Entry) string {
	h := sha256.New()
	field := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	field([]byte("cinema.source.v1"))
	for _, e := range entries {
		field([]byte(e.Path))
		field([]byte{e.Kind})
		switch e.Kind {
		case KindFile, KindExec:
			field(e.Digest[:])
		default:
			field([]byte(e.Target))
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

type stat struct {
	Size, Mtime, Ctime int64
	Ino                uint64
}

func statOf(fi os.FileInfo) stat {
	st := fi.Sys().(*syscall.Stat_t)
	m, c := statTimes(st)
	return stat{fi.Size(), m, c, uint64(st.Ino)}
}

func cmpOr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}
