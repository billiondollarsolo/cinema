// Package transport stores receipts as git notes, one ref per signing key
// (plan §8): refs/notes/cinema/v1/<key-id>. Each note is a JSON array of DSSE
// envelopes for the commit it's attached to.
package transport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const (
	RefPrefix   = "refs/notes/cinema/v1/"
	MaxNote     = 256 << 10 // per commit (plan §8)
	MaxReceipts = 100       // per commit
)

// Ref is the notes ref for a key ID.
func Ref(keyID string) string { return RefPrefix + keyID }

// Read returns the envelopes in ref's note on commit (nil if there is none).
// Oversized or malformed notes are an error: the caller treats them as no evidence.
func Read(root, ref, commit string) ([]json.RawMessage, error) {
	blob, err := git(root, nil, "notes", "--ref="+ref, "list", commit)
	if err != nil {
		return nil, nil // no note
	}
	oid := strings.TrimSpace(string(blob))
	// Size before content: an oversized note must not be read into memory (ACC-027).
	size, err := git(root, nil, "cat-file", "-s", oid)
	if err != nil {
		return nil, err
	}
	if n, _ := strconv.Atoi(strings.TrimSpace(string(size))); n > MaxNote {
		return nil, fmt.Errorf("transport: note on %s over %d bytes", short(commit), MaxNote)
	}
	out, err := git(root, nil, "cat-file", "blob", oid)
	if err != nil {
		return nil, err
	}
	var envs []json.RawMessage
	if err := json.Unmarshal(out, &envs); err != nil {
		return nil, fmt.Errorf("transport: note on %s: %w", short(commit), err)
	}
	if len(envs) > MaxReceipts {
		return nil, fmt.Errorf("transport: note on %s has over %d receipts", short(commit), MaxReceipts)
	}
	return envs, nil
}

// Write replaces ref's note on commit.
func Write(root, ref, commit string, envs []json.RawMessage) error {
	b, err := json.Marshal(envs)
	if err != nil {
		return err
	}
	if len(b) > MaxNote || len(envs) > MaxReceipts {
		return fmt.Errorf("transport: note for %s too large (%d receipts, %d bytes)", short(commit), len(envs), len(b))
	}
	_, err = git(root, b, "notes", "--ref="+ref, "add", "-f", "-F", "-", commit)
	return err
}

// Squash rewrites ref as a single parentless commit holding the same notes, so
// a per-key ref never accumulates history (plan §13.1). Only this key writes
// this ref, so the force-push that follows can't lose anyone else's work.
func Squash(root, ref string) error {
	tree, err := git(root, nil, "rev-parse", "--verify", "--quiet", ref+"^{tree}")
	if err != nil {
		return nil // nothing to squash
	}
	c, err := git(root, nil, "commit-tree", strings.TrimSpace(string(tree)), "-m", "cinema receipts")
	if err != nil {
		return err
	}
	_, err = git(root, nil, "update-ref", ref, strings.TrimSpace(string(c)))
	return err
}

// Fetch pulls every cinema notes ref from remote, replacing local copies
// (remote is authoritative for others' keys; our own ref is rewritten on push).
func Fetch(root, remote string, extraArgs ...string) error {
	args := append([]string{"fetch", "--quiet", "--no-tags"}, extraArgs...)
	args = append(args, remote, "+"+RefPrefix+"*:"+RefPrefix+"*")
	_, err := git(root, nil, args...)
	return err
}

// Refs lists local cinema notes refs.
func Refs(root string) ([]string, error) {
	out, err := git(root, nil, "for-each-ref", "--format=%(refname)", RefPrefix)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// PushResult reports branch and evidence outcomes separately (plan §8.1).
type PushResult struct {
	Atomic      bool
	BranchErr   error
	EvidenceErr error
}

// Push sends branch (may be empty for notes-only) and the notes ref. It tries
// one atomic push, then falls back to notes first, then branch.
func Push(root, remote, branchRefspec, notesRef string) PushResult {
	notesSpec := "+" + notesRef + ":" + notesRef
	if branchRefspec == "" {
		_, err := git(root, nil, "push", "--quiet", remote, notesSpec)
		return PushResult{EvidenceErr: err}
	}
	if _, err := git(root, nil, "push", "--quiet", "--atomic", remote, branchRefspec, notesSpec); err == nil {
		return PushResult{Atomic: true}
	}
	_, nerr := git(root, nil, "push", "--quiet", remote, notesSpec)
	_, berr := git(root, nil, "push", "--quiet", remote, branchRefspec)
	return PushResult{BranchErr: berr, EvidenceErr: nerr}
}

// ErrGit wraps a failed git invocation.
var ErrGit = errors.New("git")

func git(root string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// Notes commits are machine bookkeeping in public refs: never the user's
	// identity (no email leak), and works where none is configured (CI).
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=cinema", "GIT_AUTHOR_EMAIL=cinema@invalid",
		"GIT_COMMITTER_NAME=cinema", "GIT_COMMITTER_EMAIL=cinema@invalid")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w %s: %v: %s", ErrGit, args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
