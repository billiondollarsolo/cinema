package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/billiondollarsolo/cinema/internal/evidence"
	"github.com/billiondollarsolo/cinema/internal/snapshot"
	"github.com/billiondollarsolo/cinema/internal/store"
	"github.com/billiondollarsolo/cinema/internal/task"
	"github.com/billiondollarsolo/cinema/internal/transport"
)

const version = "0.1.0-dev"

func cmdKeys(args []string) int {
	if len(args) != 1 || (args[0] != "init" && args[0] != "export") {
		fmt.Fprintln(os.Stderr, "usage: cinema keys init | export")
		return exitBadArgs
	}
	dir, err := store.DataDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cinema keys: %v\n", err)
		return exitInternal
	}
	var pub ed25519.PublicKey
	if args[0] == "init" {
		if pub, err = evidence.InitKey(dir); err != nil {
			fmt.Fprintf(os.Stderr, "cinema keys: %v\n", err)
			return exitBadArgs
		}
		fmt.Fprintln(os.Stderr, "created this machine's signing key; add the line below to the repo variable CINEMA_TRUSTED_KEYS:")
	} else {
		priv, err := evidence.LoadKey(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cinema keys: %v\n", err)
			return exitBadArgs
		}
		pub = priv.Public().(ed25519.PublicKey)
	}
	host, _ := os.Hostname()
	fmt.Printf("%s %s\n", evidence.FormatPublicKey(pub), host)
	return 0
}

// repositoryID normalizes a remote URL to "host/owner/repo", lowercased, so the
// gate (which derives it from GITHUB_SERVER_URL + GITHUB_REPOSITORY) can reject
// receipts made for another repository.
func repositoryID(remoteURL string) (string, error) {
	u := strings.TrimSpace(remoteURL)
	if i := strings.Index(u, "@"); i >= 0 && !strings.Contains(u, "://") { // git@host:owner/repo
		host, path, ok := strings.Cut(u[i+1:], ":")
		if !ok {
			return "", fmt.Errorf("unrecognized remote %q", remoteURL)
		}
		u = "ssh://" + host + "/" + path
	}
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return "", fmt.Errorf("unrecognized remote %q", remoteURL)
	}
	path := strings.Trim(strings.TrimSuffix(p.Path, ".git"), "/")
	if strings.Count(path, "/") < 1 {
		return "", fmt.Errorf("unrecognized remote %q", remoteURL)
	}
	return strings.ToLower(p.Hostname() + "/" + path), nil
}

func cmdPush(args []string) int {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	remote := fs.String("remote", "origin", "git `REMOTE` to push to")
	notesOnly := fs.Bool("notes-only", false, "push evidence only, not the branch (e.g. from a VPS)")
	dryRun := fs.Bool("dry-run", false, "show what would be signed; push nothing")
	var commits stringList
	fs.Var(&commits, "commit", "sign evidence for `REV` (repeatable; default: the commits being pushed)")
	if err := fs.Parse(args); err != nil {
		return exitBadArgs
	}
	r, code := loadForReport("push", false)
	defer r.close()
	if code != 0 {
		return code
	}
	fail := func(code int, f string, a ...any) int {
		fmt.Fprintf(os.Stderr, "cinema push: "+f+"\n", a...)
		return code
	}
	dataDir, _ := store.DataDir()
	priv, err := evidence.LoadKey(dataDir)
	if err != nil {
		return fail(exitBadArgs, "%v", err)
	}
	remoteURL, err := gitOut(r.root, "remote", "get-url", "--", *remote)
	if err != nil {
		return fail(exitBadArgs, "no remote %q", *remote)
	}
	repoID, err := repositoryID(remoteURL)
	if err != nil {
		return fail(exitBadArgs, "%v", err)
	}

	branch, _ := gitOut(r.root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if len(commits) == 0 {
		if commits, err = commitsToPush(r.root, *remote, branch); err != nil {
			return fail(exitBadArgs, "%v", err)
		}
	}

	keyID := evidence.KeyID(priv.Public().(ed25519.PublicKey))
	ref := transport.Ref(keyID)
	blobs := snapshot.NewBlobCache()
	signed := 0
	for _, rev := range commits {
		sha, err := gitOut(r.root, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
		if err != nil {
			return fail(exitBadArgs, "unknown commit %q", rev)
		}
		envs, tasks, err := receiptsFor(r, sha, repoID, priv, blobs)
		if err != nil {
			return fail(exitInternal, "%s: %v", sha[:12], err)
		}
		if len(envs) == 0 {
			fmt.Printf("%s  no qualifying runs\n", sha[:12])
			continue
		}
		fmt.Printf("%s  %s\n", sha[:12], strings.Join(tasks, ", "))
		signed += len(envs)
		if *dryRun {
			continue
		}
		if err := transport.Write(r.root, ref, sha, mergeEnvelopes(r.root, ref, sha, envs, tasks)); err != nil {
			return fail(exitInternal, "%v", err)
		}
	}
	if *dryRun {
		fmt.Printf("dry run: would sign %d receipt(s)\n", signed)
		return 0
	}
	if err := transport.Squash(r.root, ref); err != nil {
		return fail(exitInternal, "%v", err)
	}

	branchSpec := ""
	if !*notesOnly {
		if branch == "" {
			return fail(exitBadArgs, "detached HEAD: use --notes-only, or check out a branch")
		}
		branchSpec = "HEAD:refs/heads/" + branch
	}
	res := transport.Push(r.root, *remote, branchSpec, ref)
	if branchSpec != "" {
		if res.BranchErr != nil {
			fmt.Printf("branch:   FAILED %v\n", res.BranchErr)
		} else {
			fmt.Printf("branch:   pushed %s to %s\n", branch, *remote)
		}
	}
	switch {
	case res.EvidenceErr != nil:
		fmt.Printf("evidence: FAILED %v (CI will run every task)\n", res.EvidenceErr)
	case signed == 0:
		fmt.Println("evidence: nothing new to sign")
	default:
		fmt.Printf("evidence: %d receipt(s) on %s\n", signed, ref)
	}
	if res.BranchErr != nil {
		return 4
	}
	if res.EvidenceErr != nil {
		return 4
	}
	return 0
}

// commitsToPush: commits on HEAD not yet on the remote branch (bounded), always including HEAD.
func commitsToPush(root, remote, branch string) ([]string, error) {
	rng := "HEAD"
	if branch != "" {
		if _, err := gitOut(root, "rev-parse", "--verify", "--quiet", "refs/remotes/"+remote+"/"+branch); err == nil {
			rng = "refs/remotes/" + remote + "/" + branch + "..HEAD"
		}
	}
	args := []string{"rev-list", "--max-count=50", rng}
	if rng == "HEAD" {
		args = append(args, "--not", "--remotes="+remote)
	}
	out, err := gitOut(root, args...)
	if err != nil {
		return nil, err
	}
	list := strings.Fields(out)
	if len(list) == 0 {
		list = []string{"HEAD"}
	}
	return list, nil
}

// receiptsFor signs one receipt per task that has a qualifying run for commit sha.
func receiptsFor(r *repo, sha, repoID string, priv ed25519.PrivateKey, blobs *snapshot.BlobCache) ([]json.RawMessage, []string, error) {
	var envs []json.RawMessage
	var names []string
	for _, id := range sortedIDs(r.cfg) {
		t := r.cfg.Tasks[id]
		if !t.Reuse.Allowed {
			continue
		}
		snap, err := snapshot.TakeCommit(r.root, sha, t.ExtraInputs, t.Outputs, blobs)
		if err != nil {
			return nil, nil, err
		}
		contract, err := t.ContractDigest(r.root)
		if err != nil {
			continue
		}
		runs, err := r.st.TaskRuns(r.id, t.ID, 50)
		if err != nil {
			return nil, nil, err
		}
		run := qualifyingRun(t, runs, contract, snap.Digest, sha)
		if run == nil {
			continue
		}
		vcs := ""
		if t.VCSContext == "exact" {
			vcs = sha
		}
		st := evidence.Statement{
			Type: evidence.StatementType, PredicateType: evidence.PredicateType,
			Subject: []evidence.Subject{{Name: "cinema-action:" + t.ID,
				Digest: map[string]string{"sha256": strings.TrimPrefix(task.ActionKey(contract, snap.Digest, run.Env, vcs), "sha256:")}}},
			Predicate: evidence.Predicate{
				SchemaVersion: 1, RunID: run.ID, RepositoryID: repoID, Platform: task.Platform(),
				Task:      evidence.TaskRef{ID: t.ID, ContractDigest: contract},
				Source:    evidence.SourceRef{Digest: snap.Digest, ObservedCommit: sha, Capture: captureName(run.Capture)},
				EnvDigest: run.Env, VCS: vcs,
				Result:   evidence.Result{Status: "passed", ExitCode: 0, Origin: "fresh"},
				Timing:   evidence.Timing{StartedAt: run.Started.UTC(), DurationMS: run.DurationMS, TimeAuthority: "local-clock"},
				Executor: evidence.Executor{Kind: "cinema-cli", Version: version},
			},
		}
		env, err := evidence.Sign(st, priv)
		if err != nil {
			return nil, nil, err
		}
		b, _ := json.Marshal(env)
		envs = append(envs, b)
		names = append(names, t.ID)
	}
	return envs, names, nil
}

// qualifyingRun: newest passed, fresh run of exactly this contract on exactly
// these bytes, unmodified while it ran, and not blocked by a failure on the same
// inputs. Freshness (max_age) is the gate's call, not ours.
func qualifyingRun(t *task.Task, runs []store.TaskRun, contract, source, sha string) *store.TaskRun {
	for i := range runs {
		r := &runs[i]
		sameInputs := r.Contract == contract && r.SourceBefore == source
		if r.State != store.StatePassed {
			if sameInputs {
				return nil // a failure on these inputs outweighs older passes
			}
			continue
		}
		if sameInputs && r.SourceAfter == source && r.Origin == "fresh" && r.Env != "" &&
			(t.VCSContext != "exact" || r.VCS == sha) {
			return r
		}
	}
	return nil
}

func captureName(c string) string {
	if c == "clean" {
		return "clean-checkout"
	}
	return "workspace-observed"
}

// mergeEnvelopes keeps receipts already in the note for tasks we didn't re-sign.
func mergeEnvelopes(root, ref, sha string, fresh []json.RawMessage, tasks []string) []json.RawMessage {
	old, err := transport.Read(root, ref, sha)
	if err != nil {
		return fresh
	}
	signed := map[string]bool{}
	for _, t := range tasks {
		signed[t] = true
	}
	out := fresh
	for _, raw := range old {
		var env evidence.Envelope
		if json.Unmarshal(raw, &env) != nil {
			continue
		}
		var st struct {
			Predicate struct{ Task struct{ ID string } } `json:"predicate"`
		}
		if p, err := decodePayload(env); err == nil && json.Unmarshal(p, &st) == nil && !signed[st.Predicate.Task.ID] {
			out = append(out, raw)
		}
	}
	if len(out) > transport.MaxReceipts {
		out = out[:transport.MaxReceipts]
	}
	return out
}

func decodePayload(env evidence.Envelope) ([]byte, error) {
	if env.Payload == "" {
		return nil, errors.New("empty")
	}
	return base64.StdEncoding.DecodeString(env.Payload)
}
