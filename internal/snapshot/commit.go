package snapshot

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// TakeCommit fingerprints a commit straight from git objects, without a
// checkout (plan §5.3). For a clean checkout of the same commit it equals Take:
// same entries, same encoding. Where they can differ, they differ toward "no
// reuse": LFS/smudge filters (the blob is a pointer), submodules (the gitlink
// commit here vs. "uninitialized" in a checkout without submodules), and extra
// inputs, which a commit can't contain and so count as missing.
func TakeCommit(root, rev string, extra, exclude []string, blobs *BlobCache) (*Snapshot, error) {
	out, err := git(root, "ls-tree", "-r", "-z", "--full-tree", rev)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	var need []int // entries whose blob digest isn't cached
	oids := map[int]string{}
	seen := map[string]bool{}
	for rec := range bytes.SplitSeq(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		// "<mode> <type> <oid>\t<path>"
		meta, path, ok := bytes.Cut(rec, []byte("\t"))
		f := strings.Fields(string(meta))
		if !ok || len(f) != 3 {
			return nil, fmt.Errorf("snapshot: unexpected ls-tree record %q", rec)
		}
		p := string(path)
		if p == excluded || IsUnder(p, exclude) {
			continue
		}
		seen[p] = true
		e := Entry{Path: p}
		switch f[0] {
		case "100644":
			e.Kind = KindFile
		case "100755":
			e.Kind = KindExec
		case "120000":
			e.Kind = KindSymlink
		case "160000":
			e.Kind, e.Target = KindSubmodule, f[2]
			entries = append(entries, e)
			continue
		default:
			return nil, fmt.Errorf("snapshot: unsupported mode %s for %s", f[0], p)
		}
		oids[len(entries)] = f[2]
		if e.Kind != KindSymlink {
			if d, ok := blobs.get(f[2]); ok {
				e.Digest = d
			} else {
				need = append(need, len(entries))
			}
		} else {
			need = append(need, len(entries)) // symlink target = blob content
		}
		entries = append(entries, e)
	}
	for _, p := range extra {
		c, err := CleanRel(p)
		if err != nil {
			return nil, err
		}
		if !seen[c] {
			entries = append(entries, Entry{Path: c, Kind: KindMissing})
		}
	}
	if err := readBlobs(root, entries, need, oids, blobs); err != nil {
		return nil, err
	}
	sortEntries(entries)
	return &Snapshot{Root: root, Digest: "sha256:" + manifestDigest(entries), Entries: entries, Hashed: len(need)}, nil
}

// readBlobs streams the needed blobs through one `git cat-file --batch`.
func readBlobs(root string, entries []Entry, need []int, oids map[int]string, blobs *BlobCache) error {
	if len(need) == 0 {
		return nil
	}
	cmd := exec.Command("git", "-C", root, "cat-file", "--batch")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		w := bufio.NewWriter(stdin)
		for _, i := range need {
			fmt.Fprintln(w, oids[i])
		}
		w.Flush()
		stdin.Close()
	}()
	r := bufio.NewReaderSize(stdout, 1<<16)
	var readErr error
	for _, i := range need {
		header, err := r.ReadString('\n')
		if err != nil {
			readErr = err
			break
		}
		// "<oid> blob <size>"
		f := strings.Fields(header)
		if len(f) != 3 || f[1] != "blob" {
			readErr = fmt.Errorf("snapshot: cat-file: %q", strings.TrimSpace(header))
			break
		}
		size, _ := strconv.ParseInt(f[2], 10, 64)
		e := &entries[i]
		if e.Kind == KindSymlink {
			b := make([]byte, size)
			if _, err := io.ReadFull(r, b); err != nil {
				readErr = err
				break
			}
			e.Target = string(b)
		} else {
			h := sha256.New()
			if _, err := io.CopyN(h, r, size); err != nil {
				readErr = err
				break
			}
			h.Sum(e.Digest[:0])
			blobs.put(oids[i], e.Digest)
		}
		if _, err := r.ReadByte(); err != nil { // trailing LF
			readErr = err
			break
		}
	}
	if err := cmd.Wait(); readErr == nil {
		readErr = err
	}
	return readErr
}

// BlobCache maps git blob IDs to SHA-256 content digests. Blob IDs name
// immutable content, so entries never go stale. Safe for concurrent use; nil ok.
type BlobCache struct {
	mu sync.Mutex
	m  map[string][32]byte
}

func NewBlobCache() *BlobCache { return &BlobCache{m: map[string][32]byte{}} }

func (c *BlobCache) get(oid string) ([32]byte, bool) {
	if c == nil {
		return [32]byte{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.m[oid]
	return d, ok
}

func (c *BlobCache) put(oid string, d [32]byte) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.m[oid] = d
	c.mu.Unlock()
}
