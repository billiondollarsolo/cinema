package snapshot

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// TakeTar is Take for a tree cinema can't read directly: a checkout in a Linux
// volume inside the engine's VM (task.CheckoutVolume). The volume streams a
// tar of exactly what Take would read, and the fingerprint is computed here,
// by the same code, on the bytes that ran: the paths `git ls-files` lists
// there (tracked, and untracked unignored ones unless the caller leaves them
// out as a clean run's outputs), each followed by "<path>/.git" (present
// only for an initialized submodule or nested repo), then the extra inputs
// and the task config. A listed path absent from the tar was deleted, as
// Take treats it.
//
// cfg is the SHA-256 of .cinema/tasks.yaml in the tree ("" if absent), which
// the fingerprint leaves out (ADR 0001) and the run checks separately.
//
// One difference from Take: an initialized submodule's target is
// "initialized", not its HEAD. A fresh volume never has one (Take says
// "uninitialized" there too), so a run that initializes one changes the
// fingerprint either way.
func TakeTar(r io.Reader, extra, exclude []string) (s *Snapshot, cfg string, err error) {
	type member struct {
		kind   byte
		digest [32]byte
		target string
	}
	got := map[string]member{}
	var order []string
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("snapshot: volume tar: %w", err)
		}
		p := strings.TrimSuffix(strings.TrimPrefix(h.Name, "./"), "/")
		if _, dup := got[p]; dup {
			continue // a conflicted file is listed once per stage
		}
		var m member
		switch h.Typeflag {
		case tar.TypeReg:
			m.kind = KindFile
			if h.Mode&0o111 != 0 {
				m.kind = KindExec
			}
			d := sha256.New()
			if _, err := io.Copy(d, tr); err != nil {
				return nil, "", fmt.Errorf("snapshot: volume tar: %w", err)
			}
			d.Sum(m.digest[:0])
		case tar.TypeSymlink:
			m.kind, m.target = KindSymlink, h.Linkname
		case tar.TypeDir:
			m.kind, m.target = KindSubmodule, "uninitialized"
		case tar.TypeLink:
			return nil, "", fmt.Errorf("snapshot: volume tar: unexpected hard link %q", p)
		default:
			continue // sockets, fifos
		}
		got[p] = m
		order = append(order, p)
	}
	extraSet := map[string]bool{}
	for _, p := range extra {
		c, err := CleanRel(p)
		if err != nil {
			return nil, "", err
		}
		extraSet[c] = true
	}
	ex := make([]string, len(exclude))
	for i, p := range exclude {
		if ex[i], err = CleanRel(p); err != nil {
			return nil, "", err
		}
	}
	if m, ok := got[excluded]; ok && m.kind == KindFile {
		cfg = hex.EncodeToString(m.digest[:])
	}
	var entries []Entry
	for _, p := range order {
		m := got[p]
		if base, ok := strings.CutSuffix(p, "/.git"); ok {
			if sub, isDir := got[base]; isDir && sub.kind == KindSubmodule {
				continue // the sentinel, not an entry
			}
		}
		if (p == excluded && !extraSet[p]) || (IsUnder(p, ex) && !extraSet[p]) {
			continue
		}
		if m.kind == KindSubmodule {
			if _, ok := got[p+"/.git"]; ok {
				m.target = "initialized"
			}
		}
		entries = append(entries, Entry{Path: p, Kind: m.kind, Digest: m.digest, Target: m.target})
	}
	for p := range extraSet {
		if _, ok := got[p]; !ok {
			entries = append(entries, Entry{Path: p, Kind: KindMissing})
		}
	}
	sortEntries(entries)
	return &Snapshot{Digest: "sha256:" + manifestDigest(entries), Entries: entries}, cfg, nil
}

// Changed lists the paths whose entry differs between two snapshots of one
// tree (added, removed or modified), sorted.
func Changed(a, b *Snapshot) []string {
	am := map[string]Entry{}
	for _, e := range a.Entries {
		am[e.Path] = e
	}
	var out []string
	for _, e := range b.Entries {
		if o, ok := am[e.Path]; !ok || o != e {
			out = append(out, e.Path)
		}
		delete(am, e.Path)
	}
	for p := range am {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}
