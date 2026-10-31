package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/billiondollarsolo/cinema/internal/analyze"
	"github.com/billiondollarsolo/cinema/internal/snapshot"
	"github.com/billiondollarsolo/cinema/internal/task"
)

// volTree is a clean checkout that ran in a volume inside the engine's VM
// (task.CheckoutVolume): what the run's fingerprints, mutation check and test
// reports read instead of the host slot, which the task never saw. Shared by
// the copies of the repo a run makes (at, runOne), so runOne sees what
// execRecorded measured.
type volTree struct {
	v              *task.CheckoutVolume
	extra, exclude []string
	before, after  *snapshot.Snapshot
	cfgBefore      string // tasks.yaml's hash in the volume
	cfgAfter       string
}

// startTaskSandbox starts t's runner-like container on the clean checkout w:
// on a VM engine the commit is first checked out into the slot's volume, and
// the returned volTree reads it; on a local Linux engine the host slot is
// bind-mounted (vt nil).
func startTaskSandbox(ctx context.Context, w *repo, t *task.Task, pl placement, network string) (*task.Sandbox, *volTree, error) {
	bins, vars, caches, err := t.RuntimeSandbox(w.dataDir, "linux", strings.TrimPrefix(pl.platform, "linux/"))
	if err != nil {
		return nil, nil, fmt.Errorf("runtime: %v", err)
	}
	art, _ := artifactsRoot()
	cpus, mem := t.RunnerSize()
	img := runnerImage(t.RunsOn)
	sp := task.SandboxSpec{Image: img, Platform: pl.platform, Emulated: pl.emulated,
		Root: w.root, RepoName: filepath.Base(w.id), RepoID: w.id, DataDir: w.dataDir, Bins: bins, Vars: vars, Caches: caches, Artifacts: art,
		Objects: filepath.Join(w.id, ".git", "objects"),
		CPUs:    cpus, MemoryMB: mem * 1024, Network: network, DockerHost: t.Docker == task.DockerHost}
	var vt *volTree
	if task.CheckoutInVolume() {
		sha, err := gitOut(w.root, "rev-parse", "HEAD")
		if err != nil {
			return nil, nil, err
		}
		// The real repo's git dir (its common dir for a worktree): the source
		// of the clone and the objects it borrows.
		gitDir, err := gitOut(w.id, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			return nil, nil, err
		}
		v, err := task.NewCheckoutVolume(w.root, gitDir, img)
		if err != nil {
			return nil, nil, err
		}
		if err := v.Populate(ctx, sha); err != nil {
			return nil, nil, err
		}
		vt = &volTree{v: v, extra: t.ExtraInputs, exclude: t.Outputs}
		if vt.before, vt.cfgBefore, err = v.Snapshot(ctx, t.ExtraInputs, t.Outputs, true); err != nil {
			return nil, nil, fmt.Errorf("fingerprinting the checkout volume: %v", err)
		}
		sp.Volume, sp.Objects = v.Name, filepath.Join(gitDir, "objects")
	}
	sb, err := task.StartSandbox(ctx, sp)
	return sb, vt, err
}

// source is the volume's fingerprint: the fresh checkout's until the run
// is over (takeAfter), then what the run left.
func (vt *volTree) source(extra, exclude []string) (string, error) {
	if strings.Join(extra, "\x00") != strings.Join(vt.extra, "\x00") || strings.Join(exclude, "\x00") != strings.Join(vt.exclude, "\x00") {
		return "", fmt.Errorf("checkout volume: fingerprint asked for other inputs than the task's")
	}
	if vt.after != nil {
		return vt.after.Digest, nil
	}
	return vt.before.Digest, nil
}

// takeAfter fingerprints the tree as the run left it. Tracked files only (and
// extra inputs): in a clean checkout, files the run created are its outputs.
func (vt *volTree) takeAfter() (string, error) {
	s, cfg, err := vt.v.Snapshot(context.Background(), vt.extra, vt.exclude, false)
	if err != nil {
		return "", err
	}
	vt.after, vt.cfgAfter = s, cfg
	return s.Digest, nil
}

// changed lists what the run changed, at most 10, as changedPaths does.
func (vt *volTree) changed() []string {
	if vt.before == nil || vt.after == nil {
		return nil
	}
	var paths []string
	for _, p := range snapshot.Changed(vt.before, vt.after) {
		paths = append(paths, analyze.Printable(p))
	}
	if len(paths) > 10 {
		paths = append(paths[:10], fmt.Sprintf("… %d more", len(paths)-10))
	}
	return paths
}

// lockDockerHost holds the engine for a docker: host task across cinema
// processes: what appears on the engine while it runs is the task's, and
// removed after it (task.EngineObjects.RemoveNew).
func lockDockerHost(dataDir string) (unlock func(), err error) {
	if dataDir == "" {
		return nil, errors.New("no data dir for the engine lock")
	}
	f, err := os.OpenFile(filepath.Join(dataDir, "docker-host.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// sweepCheckoutVolumes removes the volumes of slots that are gone, the rule
// of plan §13.1: a slot's volume lives as long as its slot.
func sweepCheckoutVolumes() {
	if loadUserConfig().Engine != "docker" {
		return
	}
	task.SweepCheckoutVolumes(nil)
}
