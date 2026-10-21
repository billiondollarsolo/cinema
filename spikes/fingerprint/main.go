// P0-4 spike: whole-source fingerprint cost (plan §5, §16).
// Throwaway. Measures list / stat / hash phases separately, cold and warm.
//
//	go run ./spikes/fingerprint -repo PATH [-cache FILE]
//
// Run twice with the same -cache to get the warm number.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"syscall"
	"time"
)

type stamp struct {
	Size, Mtime int64
	Ino         uint64
}

type cacheEntry struct {
	Stamp  stamp
	Digest [32]byte
}

type entry struct {
	path   string
	kind   byte // 'f' file, 'x' executable, 'l' symlink, 's' submodule
	digest [32]byte
	target string
	st     stamp
}

func main() {
	repo := flag.String("repo", ".", "git repository to fingerprint")
	cachePath := flag.String("cache", "", "metadata cache file (empty = always cold)")
	flag.Parse()

	t0 := time.Now()
	paths, gitlinks := listFiles(*repo)
	tList := time.Since(t0)

	cache := map[string]cacheEntry{}
	if *cachePath != "" {
		if f, err := os.Open(*cachePath); err == nil {
			_ = gob.NewDecoder(f).Decode(&cache)
			f.Close()
		}
	}

	t1 := time.Now()
	entries := make([]*entry, 0, len(paths))
	var toHash []*entry
	for _, p := range paths {
		e := &entry{path: p}
		if sha, ok := gitlinks[p]; ok {
			e.kind, e.target = 's', sha
			entries = append(entries, e)
			continue
		}
		full := filepath.Join(*repo, p)
		fi, err := os.Lstat(full)
		if err != nil {
			continue // tracked but deleted in the worktree
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			e.kind = 'l'
			e.target, _ = os.Readlink(full)
		case fi.Mode().IsRegular():
			e.kind = 'f'
			if fi.Mode()&0o111 != 0 {
				e.kind = 'x'
			}
			sys := fi.Sys().(*syscall.Stat_t)
			e.st = stamp{fi.Size(), fi.ModTime().UnixNano(), uint64(sys.Ino)}
			if c, ok := cache[p]; ok && c.Stamp == e.st {
				e.digest = c.Digest
			} else {
				toHash = append(toHash, e)
			}
		default:
			continue
		}
		entries = append(entries, e)
	}
	tStat := time.Since(t1)

	t2 := time.Now()
	var hashed int64
	jobs := make(chan *entry)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for range runtime.NumCPU() {
		wg.Go(func() {
			for e := range jobs {
				n := hashFile(filepath.Join(*repo, e.path), &e.digest)
				mu.Lock()
				hashed += n
				mu.Unlock()
			}
		})
	}
	for _, e := range toHash {
		jobs <- e
	}
	close(jobs)
	wg.Wait()
	tHash := time.Since(t2)

	digest := manifestDigest(entries)

	if *cachePath != "" {
		next := make(map[string]cacheEntry, len(entries))
		for _, e := range entries {
			if e.kind == 'f' || e.kind == 'x' {
				next[e.path] = cacheEntry{e.st, e.digest}
			}
		}
		if f, err := os.Create(*cachePath); err == nil {
			_ = gob.NewEncoder(f).Encode(next)
			f.Close()
		}
	}

	fmt.Printf("files=%d hashed=%d (%.1f MiB) digest=sha256:%s\n",
		len(entries), len(toHash), float64(hashed)/(1<<20), hex.EncodeToString(digest[:]))
	fmt.Printf("list=%v stat=%v hash=%v total=%v\n",
		tList.Round(time.Millisecond), tStat.Round(time.Millisecond),
		tHash.Round(time.Millisecond), time.Since(t0).Round(time.Millisecond))
}

// listFiles returns tracked + untracked-non-ignored paths, and submodule gitlinks.
func listFiles(repo string) ([]string, map[string]string) {
	out := git(repo, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	seen := map[string]bool{}
	var paths []string
	for _, p := range bytes.Split(out, []byte{0}) {
		if len(p) > 0 && !seen[string(p)] {
			seen[string(p)] = true
			paths = append(paths, string(p))
		}
	}
	gitlinks := map[string]string{}
	// "160000 <sha> <stage>\t<path>" for submodules
	for _, line := range bytes.Split(git(repo, "ls-files", "-z", "-s"), []byte{0}) {
		if bytes.HasPrefix(line, []byte("160000 ")) {
			tab := bytes.IndexByte(line, '\t')
			gitlinks[string(line[tab+1:])] = string(line[7:47])
		}
	}
	return paths, gitlinks
}

func git(repo string, args ...string) []byte {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "git:", err)
		os.Exit(2)
	}
	return out
}

func hashFile(path string, dst *[32]byte) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	h := sha256.New()
	n, _ := io.Copy(h, f)
	h.Sum(dst[:0])
	return n
}

// manifestDigest: domain-separated, length-prefixed, sorted by path (plan §3.1, §5.1).
func manifestDigest(entries []*entry) [32]byte {
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	h := sha256.New()
	field := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	field([]byte("cinema.source.v1"))
	for _, e := range entries {
		field([]byte(e.path))
		field([]byte{e.kind})
		if e.kind == 'f' || e.kind == 'x' {
			field(e.digest[:])
		} else {
			field([]byte(e.target))
		}
	}
	var out [32]byte
	h.Sum(out[:0])
	return out
}
