package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/billiondollarsolo/cinema/internal/plan"
	"github.com/billiondollarsolo/cinema/internal/snapshot"
	"github.com/billiondollarsolo/cinema/internal/store"
	"github.com/billiondollarsolo/cinema/internal/task"
)

// errUntrusted: the task's environment needs commands from a tasks.yaml the
// user hasn't trusted (plan §6, `cinema trust`).
// configOverride, set by --config, replaces .cinema/tasks.yaml for this
// process (e.g. an analyzer draft), in the working tree and clean checkouts.
// Trust is still per file digest.
var configOverride string

// configFlag registers --config on fs; call useConfig after fs.Parse.
func configFlag(fs *flag.FlagSet) *string {
	return fs.String("config", "", "use tasks from `FILE` instead of .cinema/tasks.yaml (e.g. a `cinema analyze --yaml` draft)")
}

func useConfig(path string) {
	if path != "" {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
	}
	configOverride = path
}

var errUntrusted = errors.New("tasks.yaml is not trusted: review it, then run `cinema trust`")

// repo is the enclosing git repo, its config, and the local store.
// Fields are zero when unavailable; callers decide whether that's fatal.
type repo struct {
	root, cwdRel string
	id           string // repo identity for records and trust: root, or the real repo for a clean checkout
	capture      string // "workspace" or "clean" (plan §5.2)
	cfg          *task.Config
	cfgErr       error // task.ErrNoConfig, a parse error, or nil
	st           *store.Store
	stErr        error
	cache        *snapshot.Cache
	cachePath    string
	snaps        map[string]string // memoized source digests by extra-inputs key
	dataDir      string            // for provisioned runtimes
	where        *task.Where       // the task runs in a sandbox (place): its platform, user and probes
}

func openRepo() (*repo, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return &repo{snaps: map[string]string{}}, err
	}
	return openRepoAt(cwd)
}

// openRepoAt opens the repo containing dir, with cwdRel relative to its root.
func openRepoAt(cwd string) (*repo, error) {
	r := &repo{snaps: map[string]string{}, capture: "workspace"}
	dataDir, err := store.DataDir()
	if err != nil {
		r.stErr = err
	} else {
		r.st, r.stErr = store.Open(dataDir)
	}
	root, err := snapshot.RepoRoot(cwd)
	if err != nil {
		return r, err // snapshot.ErrNotRepo outside a repo
	}
	// RepoRoot resolves symlinks (macOS /tmp → /private/tmp); match it.
	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = real
	}
	rel, err := filepath.Rel(root, cwd)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return r, fmt.Errorf("can't place %s inside repo %s", cwd, root)
	}
	r.root, r.cwdRel, r.id = root, filepath.ToSlash(rel), root
	r.cfg, r.cfgErr = task.Load(root)
	if configOverride != "" {
		r.cfg, r.cfgErr = task.LoadFile(configOverride)
	}
	if r.st != nil {
		r.cachePath = snapshot.CachePath(dataDir, root)
		r.dataDir = dataDir
		r.cache = snapshot.LoadCache(r.cachePath)
	}
	return r, nil
}

func (r *repo) close() {
	if r.cache != nil {
		_ = r.cache.Save(r.cachePath) // a lost cache only costs re-hashing
	}
	if r.st != nil {
		r.st.Close()
	}
}

// source is the whole-source fingerprint including extra inputs. Memoized:
// call fresh() to drop memos after something may have changed the tree.
func (r *repo) source(extra, exclude []string) (string, error) {
	key := strings.Join(extra, "\x00") + "\x01" + strings.Join(exclude, "\x00")
	if d, ok := r.snaps[key]; ok {
		return d, nil
	}
	s, err := snapshot.Take(r.root, extra, exclude, r.cache)
	if err != nil {
		return "", err
	}
	r.snaps[key] = s.Digest
	return s.Digest, nil
}

func (r *repo) fresh() { r.snaps = map[string]string{} }

func (r *repo) trusted() bool {
	return r.cfg != nil && r.st != nil && r.st.Trusted(r.id, r.cfg.Digest)
}

// current computes everything a decision for t is made against.
func (r *repo) current(t *task.Task) (plan.Current, task.Env, error) {
	cur := plan.Current{Now: time.Now()}
	var err error
	if cur.Contract, err = t.ContractDigest(r.root); err != nil {
		return cur, task.Env{}, err
	}
	if t.RunsCommands() && !r.trusted() {
		return cur, task.Env{}, errUntrusted
	}
	where := r.where
	if where == nil {
		if pl, ok := place(t); ok && pl.sandbox {
			if where, err = probeWhere(r, t, pl); err != nil {
				return cur, task.Env{}, err
			}
		} else if ok && t.Container != "" {
			where = containerWhere(t, pl)
		}
	}
	var renv []string
	if where == nil { // in a sandbox, runtimes are the container's (RuntimeBins)
		if renv, err = t.RuntimeEnv(r.dataDir); err != nil {
			return cur, task.Env{}, err
		}
	}
	env, err := t.EnvFingerprintIn(r.root, renv, where)
	if err != nil {
		return cur, env, err
	}
	cur.Env = env.Digest
	if cur.Source, err = r.source(t.ExtraInputs, t.Outputs); err != nil {
		return cur, env, err
	}
	if t.VCSContext == "exact" {
		cur.VCS = headCommit(r.root)
	}
	return cur, env, nil
}

// headCommit is HEAD's SHA for a clean tree, or "dirty": a dirty tree isn't any
// commit, so vcs_context: exact tasks can't reuse it (plan §5.3).
func headCommit(root string) string {
	head, dirty := headState(root)
	if dirty || head == "" {
		return "dirty"
	}
	return head
}

func headState(root string) (head string, dirty bool) {
	status, err := exec.Command("git", "-C", root, "status", "--porcelain", "--untracked-files=normal").Output()
	dirty = err != nil || len(status) > 0
	out, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", dirty // no commits yet
	}
	return strings.TrimSpace(string(out)), dirty
}

func toPlanRuns(rs []store.TaskRun) []plan.Run {
	out := make([]plan.Run, len(rs))
	for i, r := range rs {
		out[i] = plan.Run{ID: r.ID, State: r.State, Started: r.Started, Contract: r.Contract,
			Env: r.Env, VCS: r.VCS, SourceBefore: r.SourceBefore, SourceAfter: r.SourceAfter, Origin: r.Origin}
	}
	return out
}
