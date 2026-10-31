package task

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	Name          string
	Platform      string // linux/amd64
	Emulated      bool
	Image         string // pinned repo@sha256:…
	Workdir       string // the checkout inside: /home/runner/work/<repo>/<repo>
	CPUs          int
	MemGB         int
	OS            string // the image's os-release, "ubuntu-24.04"
	Docker        string // docker: host: the platform of the engine the task uses, "linux/arm64"
	env           []string
	inVolume      bool
	dockerVersion string
	dockerGateway string
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
	Vars      []string          // the runtimes' variables (RuntimeSandbox): host paths under <data>/runtimes, mapped inside
	Caches    map[string]string // the runtimes' download caches: volume suffix → path under /home/runner
	Artifacts string            // host dir of shared rebuilt artifacts (analyze/artifact.go), or ""
	CPUs      int               // default 4
	MemoryMB  int               // default 16384
	ShmMB     int               // default 2048
	Network   string            // services network, or ""
	Objects   string            // the real repo's .git/objects, which the clean checkout borrows (alternates)
	Env       []string
	ProbeOnly bool          // no checkout: toolchain probes are version commands (allowlisted), they don't read the source
	Lifetime  time.Duration // the container removes itself after this even if cinema dies (default 24h)
	// Volume: the checkout is this volume (CheckoutVolume), not a bind mount of
	// Root; Objects is then mounted as the clone's alternates without reading
	// Root's copy. Root stays the host slot (it names the runtime caches).
	Volume string
	// DockerHost: the task may use the engine (docker: host): its socket at
	// /var/run/docker.sock, usable by the runner user.
	DockerHost bool
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
	s.CPUs, s.MemGB = cpus, mem/1024
	var alternates []string
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
	var cacheDirs []string
	if !sp.ProbeOnly { // probes are version commands: no downloads
		mounts, dirs, err := runtimeCacheMounts(vol, arch, sp.Root, sp.Caches)
		if err != nil {
			return nil, err
		}
		args, cacheDirs = append(args, mounts...), dirs
		if sp.Volume != "" {
			// Populated by CheckoutVolume.Populate, as a shared clone of exactly
			// sp.Objects' repo: mount those objects, never what the tree names.
			if !filepath.IsAbs(sp.Objects) || strings.ContainsAny(sp.Objects, ":,\n") {
				return nil, fmt.Errorf("checkout volume: bad objects path %q", sp.Objects)
			}
			args = append(args, "-v", sp.Volume+":"+s.Workdir, "-v", sp.Objects+":"+sp.Objects+":ro")
			alternates = append(alternates, sp.Objects)
			s.inVolume = true
		} else {
			args = append(args, "-v", sp.Root+":"+s.Workdir)
		}
		// A clean checkout borrows the real repo's objects (git alternates, by
		// host path): mount them read-only at that path, or git inside the
		// container sees a broken repo (P4: go build's VCS stamping failed).
		// Only the expected directory, and only if the checkout still names
		// exactly it: the file is inside the tree a task can write, and a
		// rewritten one would otherwise mount any host path into the next task.
		if b, err := os.ReadFile(filepath.Join(sp.Root, ".git", "objects", "info", "alternates")); err == nil && sp.Volume == "" {
			got := strings.TrimSpace(string(b))
			if sp.Objects == "" || got != sp.Objects || strings.ContainsAny(got, ":,\n") || !filepath.IsAbs(got) {
				return nil, fmt.Errorf("the checkout's git alternates (%q) aren't the repo's objects: refusing to mount them", got)
			}
			args = append(args, "-v", got+":"+got+":ro")
			alternates = append(alternates, got)
		}
	}
	if sp.Artifacts != "" {
		args = append(args, "-v", sp.Artifacts+":/opt/cinema/artifacts")
	}
	if sp.Network != "" {
		args = append(args, "--network", sp.Network)
	}
	if sp.DockerHost {
		e := EngineServer()
		if e.Arch == "" {
			return nil, errors.New("docker: host: the engine doesn't answer (docker version failed)")
		}
		s.Docker, s.dockerVersion = e.GOOS+"/"+e.Arch, e.Version // probe sandboxes too: it's in the fingerprint
	}
	useDocker := sp.DockerHost && !sp.ProbeOnly
	if useDocker {
		args = append(args, "-v", engineSocket()+":/var/run/docker.sock")
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
	if out, err := docker("exec", s.Name, "cat", "/etc/os-release").Output(); err == nil {
		s.OS = OSRelease(string(out))
	}
	for _, l := range alternates {
		if err := docker("exec", s.Name, "test", "-d", path.Join(l, "pack")).Run(); err != nil {
			s.Stop()
			return nil, fmt.Errorf("the engine can't see the repo's git objects at %s: share that directory with the engine's VM (Colima shares only your home directory by default)", l)
		}
	}
	// The runner user takes the host user's uid/gid: files it writes into the
	// checkout belong to the user, and git sees no "dubious ownership".
	uid, gid := os.Getuid(), os.Getgid()
	setup := fmt.Sprintf(`set -e
id runner >/dev/null
[ "$(id -u runner)" = %[1]d ] || usermod -o -u %[1]d runner
[ "$(id -g runner)" = %[2]d ] || groupmod -o -g %[2]d runner
mkdir -p /home/runner/work/_temp /home/runner/go/bin %[4]s
chown %[1]d:%[2]d /home/runner /home/runner/work /home/runner/work/%[3]s /home/runner/work/_temp /home/runner/go /home/runner/go/bin /home/runner/go/pkg /home/runner/go/pkg/mod /home/runner/.cache /home/runner/.npm %[5]s 2>/dev/null || true
for d in %[5]s; do p=$d; while [ "$p" != /home/runner ]; do chown %[1]d:%[2]d "$p"; p=$(dirname "$p"); done; done
# As actions/checkout does: VM file sharing can show the checkout under another
# owner, and git then refuses it ("dubious ownership").
printf '[safe]\n\tdirectory = %[4]s\n' > /home/runner/.gitconfig && chown %[1]d:%[2]d /home/runner/.gitconfig
cat >> /etc/mavenrc <<'EOF'
%[6]s
EOF`, uid, gid, sp.RepoName, s.Workdir, strings.Join(cacheDirs, " "), mavenRC)
	if useDocker {
		// The socket belongs to root and the engine's docker group (Colima:
		// 0660, gid 991): give runner that group, by number, as GitHub's runner
		// user is in the docker group. Never chmod it: it's the engine's own.
		setup += `
g=$(stat -c %g /var/run/docker.sock)
name=$(getent group "$g" | cut -d: -f1)
[ -n "$name" ] || { name=docker-host; groupadd -o -g "$g" "$name"; }
usermod -aG "$name" runner`
	}
	if out, err := docker("exec", "-u", "0", s.Name, "sh", "-c", setup).CombinedOutput(); err != nil {
		s.Stop()
		return nil, fmt.Errorf("sandbox setup (does the image have a runner user?): %v: %s", err, firstLine(string(out)))
	}
	if useDocker {
		if out, err := docker("exec", "-u", "runner", s.Name, "curl", "-sfS", "--unix-socket", "/var/run/docker.sock", "http://docker/_ping").CombinedOutput(); err != nil {
			s.Stop()
			return nil, fmt.Errorf("docker: host: the engine's socket (%s) doesn't answer inside the sandbox: %v: %s", engineSocket(), err, firstLine(string(out)))
		}
		// Containers the task starts publish their ports on the engine's host,
		// which the sandbox reaches as its default gateway.
		out, _ := docker("exec", s.Name, "ip", "route", "show", "default").Output()
		if s.dockerGateway = defaultGateway(string(out)); s.dockerGateway == "" {
			s.Stop()
			return nil, fmt.Errorf("docker: host: no default route in the sandbox (%q)", strings.TrimSpace(string(out)))
		}
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
		"PATH=" + sandboxPath(bins),
		"NPM_CONFIG_PREFIX=" + sandboxNpmPrefix, "PNPM_HOME=" + sandboxPnpmHome,
		"pnpm_config_store_dir=" + sandboxPnpmStore, "npm_config_store_dir=" + sandboxPnpmStore, // pnpm 11+, pnpm ≤10
		"HOME=/home/runner", "USER=runner", "CI=true", "TZ=UTC", "LANG=C.UTF-8",
		"GITHUB_ACTIONS=true", "RUNNER_OS=Linux", "RUNNER_ARCH=" + runnerArch,
		"RUNNER_TEMP=/home/runner/work/_temp", "GITHUB_WORKSPACE=" + s.Workdir,
		"GOTOOLCHAIN=local", // setup-go sets it too: the pinned Go, never a download
		// The shared build cache keeps builds fast, but go test would replay
		// cached results, which are never evidence: run tests fresh (other go
		// commands ignore the flag). A job that sets GOFLAGS itself replaces it,
		// and the cache detector still catches a replay.
		"GOFLAGS=-count=1",
	}, sp.Env...)
	if sp.Artifacts != "" {
		s.env = append(s.env, "CINEMA_ARTIFACTS=/opt/cinema/artifacts")
	}
	if useDocker {
		s.env = append(s.env, dockerHostEnv(s.dockerGateway)...)
	}
	host := filepath.Join(sp.DataDir, "runtimes")
	for _, kv := range sp.Vars { // JAVA_HOME=<data>/runtimes/java/… → /opt/cinema/runtimes/java/…
		k, v, _ := strings.Cut(kv, "=")
		if rel, err := filepath.Rel(host, v); err == nil && !strings.HasPrefix(rel, "..") {
			v = path.Join(runtimesInside, filepath.ToSlash(rel))
		}
		s.env = append(s.env, k+"="+v)
	}
	return s, nil
}

// Global npm/pnpm installs (`npm i -g pnpm`, the pnpm/action-setup draft) go
// to the container's own home: the pinned runtimes are mounted read-only, and
// the container is the task's alone (lab: hono, every pnpm job failed with
// ENOENT under /opt/cinema/runtimes/node). As PrivateGlobals does on the host.
//
// pnpm's store goes to the download cache volume. pnpm puts it on the
// project's filesystem otherwise (node_modules/.pnpm-store in the bind-mounted
// checkout), and pnpm 12 failed to import from there on Colima's virtiofs
// ("failed to import … No such file or directory", lab: hono). The store is
// content-addressed and verified, a download cache like ~/.npm.
const (
	sandboxNpmPrefix = "/home/runner/.npm-global"
	sandboxPnpmHome  = "/home/runner/.local/share/pnpm"
	sandboxPnpmStore = "/home/runner/.cache/pnpm-store"
)

func sandboxPath(bins []string) string {
	dirs := append([]string{sandboxNpmPrefix + "/bin", sandboxPnpmHome, sandboxPnpmHome + "/bin"}, bins...)
	return strings.Join(append(dirs, "/home/runner/.local/bin", "/home/runner/go/bin", "/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"), ":")
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

// Shell is the argv of an interactive shell in the sandbox, as the runner user
// with the task's environment (cinema debug).
func (s *Sandbox) Shell(cwd string, tty bool) []string {
	args := s.execArgs(cwd, []string{"bash", "-l"}, nil)
	if tty {
		args = append([]string{args[0], "-t"}, args[1:]...) // exec -t -i …
	}
	return append(append(append([]string{"env"}, DockerEnv()...), "docker"), args...)
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
	in := fmt.Sprintf("executor=container image=%s emulated=%t", s.Image, s.Emulated)
	if s.inVolume {
		in += " checkout=volume"
	}
	w := &Where{Platform: s.Platform, Probe: s.Probe, CPUs: s.CPUs, MemGB: s.MemGB, OS: s.OS, Docker: s.Docker, Inputs: []string{in}}
	if s.Docker != "" {
		w.Inputs = append(w.Inputs, "docker server "+s.dockerVersion)
	}
	return w
}

// Stop removes the container (and whatever still runs in it).
func (s *Sandbox) Stop() { _ = docker("rm", "-f", s.Name).Run() }

// runtimeCacheMounts are the -v args and paths of the runtimes' download
// caches. Each is a volume per checkout slot (root is
// <data>/checkouts/<repo>/<n>, which cleanCheckout holds locked), so only one
// container at a time uses it, as a runner has its own ~/.gradle. These tools
// coordinate writers with locks another container never sees: Gradle asks
// the holder to release over UDP (a second build failed with "Timeout
// waiting to lock artifact cache"), cargo locks $CARGO_HOME/.package-cache
// and NuGet /tmp/NuGetScratch, outside the volume, and Maven's resolver
// locks within one JVM.
func runtimeCacheMounts(vol, arch, root string, caches map[string]string) (args, dirs []string, err error) {
	if len(caches) == 0 {
		return nil, nil, nil
	}
	slot := filepath.Base(root)
	if strings.Trim(slot, "0123456789") != "" {
		return nil, nil, fmt.Errorf("runtime caches need a clean checkout slot, not %s", root)
	}
	for _, k := range sortedKeys(caches) {
		p := path.Join("/home/runner", caches[k])
		args = append(args, "-v", vol+"-"+k+"-"+arch+"-"+slot+":"+p)
		dirs = append(dirs, p)
	}
	return args, dirs, nil
}

func cmpInt(v, d int) int {
	if v > 0 {
		return v
	}
	return d
}
