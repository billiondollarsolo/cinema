package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/billiondollarsolo/cinema/internal/evidence"
	"github.com/billiondollarsolo/cinema/internal/plan"
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
	defer stopProbeSandboxes()
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	remote := fs.String("remote", "origin", "git `REMOTE` to push to")
	notesOnly := fs.Bool("notes-only", false, "push evidence only, not the branch (e.g. from a VPS)")
	dryRun := fs.Bool("dry-run", false, "show what would be signed; push nothing")
	runFirst := fs.Bool("run", false, "first run, in a clean checkout of HEAD, every task that lacks clean evidence for it")
	jobs := fs.Int("j", 1, "with --run: run up to `N` tasks at once, each in its own clean checkout")
	var commits stringList
	fs.Var(&commits, "commit", "sign evidence for `REV` (repeatable; default: the commits being pushed)")
	mergeBase := fs.String("merge-base", "auto", "also test the merge of HEAD into `REF`, the tree CI builds for a PR (auto: the remote's default branch; none: don't)")
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

	head, err := gitOut(r.root, "rev-parse", "HEAD")
	if err != nil {
		return fail(exitBadArgs, "%v", err)
	}
	merge, mergeNote := mergeResult(r.root, *remote, branch, head, *mergeBase)
	if mergeNote != "" {
		fmt.Println(mergeNote)
	}
	if *runFirst && !*dryRun {
		if code := runMissingClean(r, max(*jobs, 1), head); code != 0 {
			return code
		}
		if merge != "" {
			if code := runMissingClean(r, max(*jobs, 1), merge); code != 0 {
				return code
			}
		}
	}

	os.Setenv("CINEMA_PUSHING", "1") // our own git push: the pre-push hook stays quiet
	keyID := evidence.KeyID(priv.Public().(ed25519.PublicKey))
	ref := transport.Ref(keyID)
	if !*dryRun {
		// Read from where we push (pushurl may differ from the fetch URL).
		pushURL, err := gitOut(r.root, "remote", "get-url", "--push", "--", *remote)
		if err != nil {
			return fail(exitBadArgs, "no remote %q", *remote)
		}
		if err := transport.FetchOwn(r.root, pushURL, ref); err != nil {
			// Without the published copy, pushing would replace receipts already
			// there. Stop rather than risk that.
			return fail(4, "fetching published evidence: %v", err)
		}
	}
	blobs := snapshot.NewBlobCache()
	signed := 0
	for _, rev := range commits {
		sha, err := gitOut(r.root, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
		if err != nil {
			return fail(exitBadArgs, "unknown commit %q", rev)
		}
		envs, tasks, workspaceOnly, err := receiptsFor(r, sha, repoID, priv, blobs)
		if err != nil {
			return fail(exitInternal, "%s: %v", sha[:12], err)
		}
		if len(workspaceOnly) > 0 {
			fmt.Printf("%s  working-tree runs only (CI needs a clean checkout, ADR 0002): %s\n         run: cinema push --run   (or: cinema run --commit %s %s)\n",
				sha[:12], strings.Join(workspaceOnly, ", "), sha[:12], strings.Join(workspaceOnly, " "))
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
		if sha == head && merge != "" {
			// The merge result has no commit CI knows: its receipts go on the
			// head's note, beside the head's own (plan §5.5).
			menvs, mtasks, _, err := receiptsFor(r, merge, repoID, priv, blobs)
			if err != nil {
				return fail(exitInternal, "merge result: %v", err)
			}
			if len(menvs) > 0 {
				fmt.Printf("%s  merge result: %s\n", sha[:12], strings.Join(mtasks, ", "))
				signed += len(menvs)
				envs = append(envs, menvs...)
			}
		}
		if err := transport.Write(r.root, ref, sha, mergeEnvelopes(r.root, ref, sha, envs)); err != nil {
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
	if _, err := gitOut(r.root, "rev-parse", "--verify", "--quiet", ref); err != nil {
		ref = "" // nothing ever signed on this machine: no notes ref to push
	}
	var res transport.PushResult
	switch {
	case ref == "" && branchSpec == "":
		// notes-only with nothing to send
	case ref == "":
		_, err := gitOut(r.root, "push", "--quiet", *remote, branchSpec)
		res.BranchErr = err
	default:
		res = transport.Push(r.root, *remote, branchSpec, ref)
	}
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

// receiptsFor signs one receipt per task that has a qualifying run for commit
// sha. workspaceOnly lists tasks whose only qualifying runs were in a working
// tree, which CI won't accept (ADR 0002).
func receiptsFor(r *repo, sha, repoID string, priv ed25519.PrivateKey, blobs *snapshot.BlobCache) (envs []json.RawMessage, names, workspaceOnly []string, err error) {
	for _, id := range sortedIDs(r.cfg) {
		t := r.cfg.Tasks[id]
		if !t.Reuse.Allowed {
			continue
		}
		snap, err := snapshot.TakeCommit(r.root, sha, t.ExtraInputs, t.Outputs, blobs)
		if err != nil {
			return nil, nil, nil, err
		}
		contract, err := t.ContractDigest(r.root)
		if err != nil {
			continue
		}
		runs, err := r.st.TaskRuns(r.id, t.ID, 50)
		if err != nil {
			return nil, nil, nil, err
		}
		run := qualifyingRun(t, runs, contract, snap.Digest, sha)
		result := evidence.Result{Status: "passed", ExitCode: 0, Origin: "fresh"}
		if run == nil {
			// No pass to sign: publish the failure instead, if the latest run on
			// these inputs failed. A trusted failure blocks passes from every
			// machine in CI (FAILED_ELSEWHERE), so retrying elsewhere can't hide it.
			if run = latestFailure(runs, contract, snap.Digest); run == nil {
				continue
			}
			result = evidence.Result{Status: "failed", ExitCode: run.ExitCode, Origin: "fresh"}
			if result.ExitCode == 0 {
				result.ExitCode = -1 // timed out or killed: a failure without an exit code
			}
		}
		if !t.Reuse.AcceptsCapture(run.Capture) {
			workspaceOnly = append(workspaceOnly, t.ID)
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
				SchemaVersion: 1, RunID: run.ID, RepositoryID: repoID, Platform: runPlatform(run),
				Task:      evidence.TaskRef{ID: t.ID, ContractDigest: contract},
				Source:    evidence.SourceRef{Digest: snap.Digest, ObservedCommit: sha, Capture: captureName(run.Capture)},
				EnvDigest: run.Env, VCS: vcs,
				Result:   result,
				Timing:   evidence.Timing{StartedAt: run.Started.UTC(), DurationMS: run.DurationMS, TimeAuthority: "local-clock"},
				Executor: runExecutor(run),
			},
		}
		env, err := evidence.Sign(st, priv)
		if err != nil {
			return nil, nil, nil, err
		}
		b, _ := json.Marshal(env)
		envs = append(envs, b)
		names = append(names, t.ID)
	}
	return envs, names, workspaceOnly, nil
}

// runMissingClean runs, in a clean checkout of HEAD, every reusable task this
// machine can satisfy that has no clean-checkout evidence for HEAD yet.
func runMissingClean(r *repo, jobs int, sha string) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	blobs := snapshot.NewBlobCache()
	var todo []*task.Task
	for _, id := range sortedIDs(r.cfg) {
		t := r.cfg.Tasks[id]
		if _, ok := place(t); !t.Reuse.Allowed || !ok {
			continue
		}
		snap, err := snapshot.TakeCommit(r.root, sha, t.ExtraInputs, t.Outputs, blobs)
		contract, cerr := t.ContractDigest(r.root)
		runs, rerr := r.st.TaskRuns(r.id, t.ID, 50)
		if err != nil || cerr != nil || rerr != nil {
			todo = append(todo, t)
			continue
		}
		if run := qualifyingRun(t, runs, contract, snap.Digest, sha); run == nil || run.Capture != "clean" {
			todo = append(todo, t)
		}
	}
	if len(todo) == 0 {
		return 0
	}
	var failed int
	if jobs > 1 {
		failed = runParallel(ctx, r, sha, todo, jobs)
	} else {
		sweepRunDirs()
		cr, cleanup, err := cleanCheckout(r, sha)
		defer cleanup()
		if err != nil {
			fmt.Fprintf(os.Stderr, "cinema push: %v\n", err)
			return exitInternal
		}
		failed = runTasks(ctx, cr, todo)
	}
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "cinema push: interrupted; nothing pushed")
		return exitUnsatisfied
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "cinema push: %d task(s) failed in the clean checkout; pushing anyway (CI will run them)\n", failed)
	}
	return 0
}

// qualifyingRun: newest passed, fresh run of exactly this contract on exactly
// these bytes, unmodified while it ran, and not blocked by a failure on the same
// inputs. Freshness (max_age) is the gate's call, not ours.
func qualifyingRun(t *task.Task, runs []store.TaskRun, contract, source, sha string) *store.TaskRun {
	var best *store.TaskRun
	for i := range runs {
		r := &runs[i]
		if r.State == store.StatePassed && r.Contract == contract && r.SourceBefore == source && r.SourceAfter == source &&
			r.Origin == "fresh" && r.Env != "" && (t.VCSContext != "exact" || r.VCS == sha) {
			best = r
			break
		}
	}
	if best == nil {
		return nil
	}
	// A failure of the same work (contract, source and environment) outweighs
	// a pass until the newest plan.FlakyStreak runs of it all passed: one retry
	// doesn't make a failure a flake (plan §11.2).
	streak := 0
	for i := range runs {
		r := &runs[i]
		if r.State == store.StateInterrupted || r.Contract != contract || r.SourceBefore != source || r.Env != best.Env {
			continue
		}
		if r.State != store.StatePassed {
			if streak < plan.FlakyStreak {
				return nil
			}
			break
		}
		streak++
	}
	return best
}

// latestFailure is the newest run on these inputs if it failed in a clean
// checkout: the failure push publishes (plan §11.2).
func latestFailure(runs []store.TaskRun, contract, source string) *store.TaskRun {
	for i := range runs {
		r := &runs[i]
		if r.State == store.StateInterrupted || r.Contract != contract || r.SourceBefore != source {
			continue
		}
		if r.State == store.StatePassed || r.Env == "" || r.Capture != "clean" {
			return nil
		}
		return r
	}
	return nil
}

func captureName(c string) string {
	if c == "clean" {
		return "clean-checkout"
	}
	return "workspace-observed"
}

// mergeEnvelopes keeps receipts already in the note that the fresh ones don't
// replace: one per task and tree (head and merge result are different trees).
func mergeEnvelopes(root, ref, sha string, fresh []json.RawMessage) []json.RawMessage {
	old, err := transport.Read(root, ref, sha)
	if err != nil {
		return fresh
	}
	signed := map[string]bool{}
	for _, raw := range fresh {
		signed[receiptKey(raw)] = true
	}
	out := fresh
	for _, raw := range old {
		if k := receiptKey(raw); k != "" && !signed[k] {
			out = append(out, raw)
		}
	}
	if len(out) > transport.MaxReceipts {
		out = out[:transport.MaxReceipts]
	}
	return out
}

// receiptKey is a receipt's task and source digest, "" if unreadable.
func receiptKey(raw json.RawMessage) string {
	var env evidence.Envelope
	if json.Unmarshal(raw, &env) != nil {
		return ""
	}
	var st struct {
		Predicate struct {
			Task   struct{ ID string }
			Source struct{ Digest string }
		} `json:"predicate"`
	}
	p, err := decodePayload(env)
	if err != nil || json.Unmarshal(p, &st) != nil {
		return ""
	}
	return st.Predicate.Task.ID + "\x00" + st.Predicate.Source.Digest
}

// mergeResult is a commit holding the tree GitHub builds for a PR of HEAD into
// the base: git merge-tree's result, wrapped in a commit object no ref points
// to (git's gc removes it; nothing in the repo changes). "" when there's no
// base, HEAD already contains it, or the merge conflicts (then CI runs no PR
// checks either).
func mergeResult(root, remote, branch, head, base string) (sha, note string) {
	if base == "none" {
		return "", ""
	}
	if base == "auto" {
		b, err := gitOut(root, "rev-parse", "--abbrev-ref", remote+"/HEAD")
		if err != nil {
			return "", ""
		}
		base = b
	}
	baseSHA, err := gitOut(root, "rev-parse", "--verify", "--quiet", base+"^{commit}")
	if err != nil || strings.TrimPrefix(base, remote+"/") == branch {
		return "", ""
	}
	c, err := mergeCommit(root, baseSHA, head)
	switch {
	case errors.Is(err, errConflict):
		return "", fmt.Sprintf("merge result: HEAD doesn't merge cleanly into %s; CI will run no PR checks until it does", base)
	case err != nil || c == head:
		return "", "" // up to date with the base: the merge result is HEAD's own tree
	}
	return c, fmt.Sprintf("merge result: also testing HEAD merged into %s (%s), the tree CI builds for a PR", base, short(baseSHA))
}

var errConflict = errors.New("doesn't merge cleanly")

// mergeCommit is the commit GitHub builds for a PR at head on baseSHA: head
// itself when it already contains the base, else a dangling merge commit.
func mergeCommit(root, baseSHA, head string) (string, error) {
	if _, err := gitOut(root, "merge-base", "--is-ancestor", baseSHA, head); err == nil {
		return head, nil
	}
	tree, err := gitOut(root, "merge-tree", "--write-tree", baseSHA, head)
	if err != nil {
		return "", errConflict
	}
	tree, _, _ = strings.Cut(tree, "\n")
	return gitOut(root, "-c", "user.name=cinema", "-c", "user.email=cinema@localhost", "commit-tree", tree, "-p", baseSHA, "-p", head, "-m", "cinema: merge result of "+short(head)+" into "+short(baseSHA))
}

func decodePayload(env evidence.Envelope) ([]byte, error) {
	if env.Payload == "" {
		return nil, errors.New("empty")
	}
	return base64.StdEncoding.DecodeString(env.Payload)
}

// runPlatform is the platform a run had (its environment inputs record it: a
// sandbox's, not this host's).
func runPlatform(run *store.TaskRun) string {
	for _, in := range run.EnvInputs {
		if p, ok := strings.CutPrefix(in, "platform="); ok {
			if p == task.PlatformAny {
				return task.Platform()
			}
			return p
		}
	}
	return task.Platform()
}

// runExecutor describes where a run happened, from its environment inputs.
func runExecutor(run *store.TaskRun) evidence.Executor {
	ex := evidence.Executor{Kind: "cinema-cli", Version: version, Where: "host-process"}
	for _, in := range run.EnvInputs {
		if rest, ok := strings.CutPrefix(in, "executor=container "); ok {
			ex.Where = "container"
			for _, f := range strings.Fields(rest) {
				k, v, _ := strings.Cut(f, "=")
				switch k {
				case "image":
					ex.Image = v
				case "emulated":
					ex.Emulated = v == "true"
				}
			}
		}
	}
	return ex
}
