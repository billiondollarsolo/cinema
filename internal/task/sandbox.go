package task

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// A Sandbox is one task's container, shaped like the GitHub runner its job
// would get (plan §9.2): a runner-like image, a non-root "runner" user with the
// host user's uid (so the bind-mounted checkout stays the user's), the runner's
// workspace path, a fresh HOME, the runner's CPU/memory/shm, pinned runtimes
// mounted read-only, and nothing else from the host. Probes and the task run
// inside it with docker exec; Stop removes it.
type Sandbox struct {
	Name     string
	Platform string // linux/amd64
	Emulated bool
	Image    string // pinned repo@sha256:…
	Workdir  string // the checkout inside: /home/runner/work/<repo>/<repo>
	env      []string
}

// SandboxSpec is what a Sandbox is started from.
type SandboxSpec struct {
	Image     string // runner-like image (resolved to its digest for Platform)
	Platform  string // linux/amd64 | linux/arm64
	Emulated  bool
	Root      string // host path of the clean checkout
	RepoName  string // last path element of the repo, for the runner's workspace path
	RepoID    string // identifies the repo, for cache volume names
	DataDir   string // cinema's data dir: runtimes come from <data>/runtimes
	Bins      []string
	Artifacts string // host dir of shared rebuilt artifacts (analyze/artifact.go), or ""
	CPUs      int    // default 4
	MemoryMB  int    // default 16384
	ShmMB     int    // default 2048
	Network   string // services network, or ""
	Env       []string
	ProbeOnly bool          // no checkout: toolchain probes are version commands (allowlisted), they don't read the source
	Lifetime  time.Duration // the container removes itself after this even if cinema dies (default 24h)
}

const runtimesInside = "/opt/cinema/runtimes"

// StartSandbox creates the container and makes the runner user the host user.
func StartSandbox(ctx context.Context, sp SandboxSpec) (*Sandbox, error) {
	arch := strings.TrimPrefix(sp.Platform, "linux/")
	if arch == sp.Platform || arch == "" {
		return nil, fmt.Errorf("sandbox platform %q: linux/<arch> only", sp.Platform)
	}
	img, err := PinnedImageFor(sp.Image, sp.Platform)
	if err != nil {
		return nil, fmt.Errorf("runner image %s: %w", sp.Image, err)
	}
	s := &Sandbox{Name: "cinema-" + randomHex(), Platform: sp.Platform, Emulated: sp.Emulated, Image: img,
		Workdir: path.Join("/home/runner/work", sp.RepoName, sp.RepoName)}
	cpus, mem, shm := cmpInt(sp.CPUs, 4), cmpInt(sp.MemoryMB, 16384), cmpInt(sp.ShmMB, 2048)
	h := sha256.Sum256([]byte(sp.RepoID))
	vol := "cinema-cache-" + hex.EncodeToString(h[:6])
	args := []string{"run", "-d", "--rm", "--name", s.Name, "--platform", sp.Platform,
		"--label", "cinema.sandbox=1", "--entrypoint", "sleep",
		"--cpus", fmt.Sprint(cpus), "--memory", fmt.Sprintf("%dm", mem), "--memory-swap", fmt.Sprintf("%dm", mem),
		"--shm-size", fmt.Sprintf("%dm", shm),
		"-v", filepath.Join(sp.DataDir, "runtimes") + ":" + runtimesInside + ":ro",
		// Download and build caches, per repo and arch: content-keyed, never results.
		"-v", vol + "-gomod:/home/runner/go/pkg/mod",
		"-v", vol + "-cache-" + arch + ":/home/runner/.cache",
		"-v", vol + "-npm:/home/runner/.npm",
	}
	if !sp.ProbeOnly {
		args = append(args, "-v", sp.Root+":"+s.Workdir)
	}
	if sp.Artifacts != "" {
		args = append(args, "-v", sp.Artifacts+":/opt/cinema/artifacts")
	}
	if sp.Network != "" {
		args = append(args, "--network", sp.Network)
	}
	life := sp.Lifetime
	if life <= 0 {
		life = 24 * time.Hour
	}
	args = append(args, img, fmt.Sprint(int(life.Seconds()))) // sleep N: a backstop, so a crash never leaves it running
	if out, err := dockerContext(ctx, args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("docker run: %v: %s", err, firstLine(string(out)))
	}
	// The engine must really see the checkout. Colima, for one, shares only the
	// home directory with its VM: any other path mounts as an empty directory,
	// and a task would run against nothing (P4).
	sentinel := path.Join(s.Workdir, ".git")
	if out, err := docker("exec", s.Name, "test", "-e", sentinel).CombinedOutput(); err != nil && !sp.ProbeOnly {
		s.Stop()
		return nil, fmt.Errorf("the engine can't see %s (the container shows an empty directory): share it with the engine's VM, e.g. Colima's mounts in ~/.colima/default/colima.yaml%s", sp.Root, strings.TrimSpace(" "+string(out)))
	}
	// The runner user takes the host user's uid/gid: files it writes into the
	// checkout belong to the user, and git sees no "dubious ownership".
	uid, gid := os.Getuid(), os.Getgid()
	setup := fmt.Sprintf(`set -e
id runner >/dev/null
[ "$(id -u runner)" = %[1]d ] || usermod -o -u %[1]d runner
[ "$(id -g runner)" = %[2]d ] || groupmod -o -g %[2]d runner
mkdir -p /home/runner/work/_temp /home/runner/go/bin %[4]s
chown %[1]d:%[2]d /home/runner /home/runner/work /home/runner/work/%[3]s /home/runner/work/_temp /home/runner/go /home/runner/go/bin /home/runner/go/pkg /home/runner/go/pkg/mod /home/runner/.cache /home/runner/.npm 2>/dev/null || true`, uid, gid, sp.RepoName, s.Workdir)
	if out, err := docker("exec", "-u", "0", s.Name, "sh", "-c", setup).CombinedOutput(); err != nil {
		s.Stop()
		return nil, fmt.Errorf("sandbox setup (does the image have a runner user?): %v: %s", err, firstLine(string(out)))
	}
	var bins []string
	for _, b := range sp.Bins {
		rel, err := filepath.Rel(filepath.Join(sp.DataDir, "runtimes"), b)
		if err != nil || strings.HasPrefix(rel, "..") {
			s.Stop()
			return nil, fmt.Errorf("runtime %s is outside the data dir", b)
		}
		bins = append(bins, path.Join(runtimesInside, filepath.ToSlash(rel)))
	}
	runnerArch := map[string]string{"amd64": "X64", "arm64": "ARM64"}[arch]
	s.env = append([]string{
		"PATH=" + strings.Join(append(bins, "/home/runner/go/bin", "/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"), ":"),
		"HOME=/home/runner", "USER=runner", "CI=true", "TZ=UTC", "LANG=C.UTF-8",
		"GITHUB_ACTIONS=true", "RUNNER_OS=Linux", "RUNNER_ARCH=" + runnerArch,
		"RUNNER_TEMP=/home/runner/work/_temp", "GITHUB_WORKSPACE=" + s.Workdir,
		"GOTOOLCHAIN=local", // setup-go sets it too: the pinned Go, never a download
	}, sp.Env...)
	if sp.Artifacts != "" {
		s.env = append(s.env, "CINEMA_ARTIFACTS=/opt/cinema/artifacts")
	}
	return s, nil
}

// Exec is the argv that runs argv inside the sandbox in cwd (repo-relative),
// for record.Run: the docker client is the child, its exit code the task's.
func (s *Sandbox) Exec(cwd string, argv []string, extraEnv []string) []string {
	return append(append(append([]string{"env"}, DockerEnv()...), "docker"), s.execArgs(cwd, argv, extraEnv)...)
}

func (s *Sandbox) execArgs(cwd string, argv []string, extraEnv []string) []string {
	out := []string{"exec", "-i", "-u", "runner", "-w", path.Join(s.Workdir, filepath.ToSlash(cwd))}
	for _, e := range append(append([]string{}, s.env...), extraEnv...) {
		out = append(out, "-e", e)
	}
	return append(append(out, s.Name), argv...)
}

// Probe runs a version command inside the sandbox (30 s cap), like toolOutput.
func (s *Sandbox) Probe(cwd string, argv []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := dockerContext(ctx, s.execArgs(cwd, argv, nil)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, firstLine(string(out)))
	}
	return string(bytes.TrimSpace(out)), nil
}

// Where is the sandbox as a fingerprint location.
func (s *Sandbox) Where() *Where {
	return &Where{Platform: s.Platform, Probe: s.Probe, Inputs: []string{
		fmt.Sprintf("executor=container image=%s emulated=%t", s.Image, s.Emulated)}}
}

// Stop removes the container (and whatever still runs in it).
func (s *Sandbox) Stop() { _ = docker("rm", "-f", s.Name).Run() }

func cmpInt(v, d int) int {
	if v > 0 {
		return v
	}
	return d
}
