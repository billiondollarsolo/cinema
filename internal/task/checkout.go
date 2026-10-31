package task

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/billiondollarsolo/cinema/internal/snapshot"
	"github.com/billiondollarsolo/cinema/internal/testreport"
)

// Checkouts in a Linux volume (plan §9.2). On a Mac or Windows the engine runs
// in a VM, and a bind-mounted checkout lives on the host's filesystem behind
// the VM's file sharing: names are case-insensitive (`Aa` and `aa` are one
// file, so a wrong-case import passes there and fails on the runner's ext4)
// and must be valid UTF-8 (click's surrogate-path test failed with EPERM).
// There, each clean-checkout slot gets a volume in the VM's own Linux
// filesystem: the commit is checked out into it by git inside the VM, the task
// runs on it, and its fingerprint is computed from a tar of it, never from
// the host copy (snapshot.TakeTar).

// CheckoutInVolume reports whether clean checkouts go into a volume: the
// engine runs in a VM (any engine on macOS or Windows, Docker Desktop on
// Linux) or on another machine (a tcp:// or ssh:// DOCKER_HOST). A local Linux
// engine bind-mounts the host checkout, as before. CINEMA_CHECKOUT=volume|bind
// overrides.
func CheckoutInVolume() bool {
	switch os.Getenv("CINEMA_CHECKOUT") {
	case "volume":
		return true
	case "bind":
		return false
	}
	if runtime.GOOS != "linux" {
		return true
	}
	for _, kv := range DockerEnv() {
		if h, ok := strings.CutPrefix(kv, "DOCKER_HOST="); ok && h != "" && !strings.HasPrefix(h, "unix://") {
			return true
		}
	}
	return strings.Contains(EngineServer().OS, "Docker Desktop")
}

// Engine is what the engine reports about itself.
type Engine struct {
	OS      string // OperatingSystem: "Docker Desktop", "Ubuntu 24.04.4 LTS" (Colima's VM), …
	Arch    string // the server's GOARCH: amd64, arm64
	GOOS    string // the server's GOOS: linux (windows for Windows containers)
	Version string // server version
}

var (
	engineOnce sync.Once
	engineVal  Engine
)

// EngineServer asks the engine once per process (zero if it doesn't answer).
func EngineServer() Engine {
	engineOnce.Do(func() {
		out, err := docker("version", "--format", "{{.Server.Os}}|{{.Server.Arch}}|{{.Server.Version}}").Output()
		if err != nil {
			return
		}
		if f := strings.Split(strings.TrimSpace(string(out)), "|"); len(f) == 3 {
			engineVal.GOOS, engineVal.Arch, engineVal.Version = f[0], f[1], f[2]
		}
		if out, err := docker("info", "--format", "{{.OperatingSystem}}").Output(); err == nil {
			engineVal.OS = strings.TrimSpace(string(out))
		}
	})
	return engineVal
}

// A CheckoutVolume is one clean-checkout slot's tree inside the engine.
type CheckoutVolume struct {
	Name   string // the docker volume
	Slot   string // the host slot it belongs to (<data>/checkouts/<repo>/<n>): its label, for sweeps
	GitDir string // the real repo's git dir, mounted read-only at its host path: the source of the clone and its objects (alternates)
	image  string // helper image, pinned for the engine's own arch (no emulation for git and tar)
	user   string // uid:gid the tree belongs to, the sandbox's runner
}

// CheckoutVolumeName is the volume for a slot: one per slot directory, so
// two data dirs (or users) never share one.
func CheckoutVolumeName(slot string) string {
	h := sha256.Sum256([]byte(slot))
	return "cinema-checkout-" + hex.EncodeToString(h[:8])
}

// NewCheckoutVolume names slot's volume and pins the helper image.
func NewCheckoutVolume(slot, gitDir, image string) (*CheckoutVolume, error) {
	arch := EngineServer().Arch
	if arch == "" {
		return nil, errors.New("the engine doesn't answer (docker version failed)")
	}
	img, err := PinnedImageFor(image, "linux/"+arch)
	if err != nil {
		return nil, fmt.Errorf("checkout helper image %s: %w", image, err)
	}
	return &CheckoutVolume{Name: CheckoutVolumeName(slot), Slot: slot, GitDir: gitDir, image: img,
		user: fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())}, nil
}

// git with nothing from any config but the repo's own, no hooks, no
// fsmonitor: the volume was writable by the last task, and its .git with it.
const helperGit = `g() { env -i HOME=/tmp PATH=/usr/bin:/bin GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null git -c safe.directory='*' -c core.hooksPath=/dev/null -c core.fsmonitor=false "$@"; }
`

// Populate makes the volume exactly a fresh checkout of sha: emptied
// (nothing from the last run survives, .git included), a shared clone of the
// real repo (objects borrowed, as the host slot does), checked out by the
// pinned image's git as the runner's uid, then checked: HEAD is sha and
// nothing is untracked or ignored. Not "unmodified": a repo whose committed
// line endings disagree with its .gitattributes shows modified right after
// any checkout, CI's too, and those are the bytes CI tests. Like `git
// checkout` in CI, and unlike `git archive` (export-ignore, export-subst).
func (v *CheckoutVolume) Populate(ctx context.Context, sha string) error {
	if err := v.ensure(ctx); err != nil {
		return err
	}
	uid, gid, _ := strings.Cut(v.user, ":")
	root := `set -euo pipefail
shopt -s dotglob nullglob
rm -rf /w/*
chown "$U:$G" /w
exec setpriv --reuid="$U" --regid="$G" --clear-groups -- bash -c "$1"
`
	user := `set -euo pipefail
` + helperGit + `# What clone --shared makes, without its upload-pack (which refuses a repo the
# VM's file sharing shows as root's, whatever safe.directory says on git 2.43):
# the repo's objects as alternates, its branches as origin's, its tags.
g init -q /w
echo "$GITDIR/objects" > /w/.git/objects/info/alternates
g -C /w remote add origin "$GITDIR"
g --git-dir="$GITDIR" for-each-ref --format='%(objectname) %(refname)' refs/heads refs/tags |
  sed -e 's|^\([0-9a-f]*\) refs/heads/\(.*\)$|create refs/remotes/origin/\2 \1|' -e 's|^\([0-9a-f]*\) \(refs/tags/.*\)$|create \2 \1|' |
  g -C /w update-ref --stdin
g -C /w checkout -q -f --detach "$SHA"
test "$(g -C /w rev-parse HEAD)" = "$SHA" || { echo "HEAD is not $SHA"; exit 1; }
s=$(g -C /w ls-files --others --directory)
test -z "$s" || { echo "files the commit doesn't have:"; echo "$s" | head -5; exit 1; }
`
	out, err := v.helper(ctx, false, []string{"U=" + uid, "G=" + gid, "GITDIR=" + v.GitDir, "SHA=" + sha}, root, user).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "does not exist") || strings.Contains(msg, "not a git repository") {
			msg += fmt.Sprintf(" (can the engine see %s? share it with the engine's VM: Colima shares only your home directory by default)", v.GitDir)
		}
		return fmt.Errorf("checkout in volume %s: %v: %s", v.Name, err, msg)
	}
	return nil
}

// Snapshot fingerprints the tree in the volume (snapshot.TakeTar). untracked:
// count untracked unignored files too (before a run; after one, files the run
// created in a clean checkout are its outputs). cfg is tasks.yaml's hash.
func (v *CheckoutVolume) Snapshot(ctx context.Context, extra, exclude []string, untracked bool) (s *snapshot.Snapshot, cfg string, err error) {
	others := ""
	if untracked {
		others = "--others --exclude-standard"
	}
	// Each listed path, then "<path>/.git" (initialized submodules), then the
	// extra inputs and tasks.yaml; missing ones are skipped (deleted).
	script := `set -euo pipefail
` + helperGit + `cd /w
g ls-files -z --cached ` + others + ` > /tmp/l
sed -z 's|/*$|/.git|' /tmp/l > /tmp/s
{ cat /tmp/l /tmp/s; printf '%s\0' "$@" .cinema/tasks.yaml; } | tar --format=gnu --null --verbatim-files-from --no-recursion --ignore-failed-read -T - -cf - 2>/dev/null
`
	err = v.stream(ctx, script, extra, func(r io.Reader) error {
		s, cfg, err = snapshot.TakeTar(r, extra, exclude)
		return err
	})
	return s, cfg, err
}

// JUnit reads the JUnit reports the run wrote in the volume, by the rules of
// testreport.JUnitFiles.
func (v *CheckoutVolume) JUnit(ctx context.Context, since time.Time) [][]byte {
	script := `set -eo pipefail
cd /w
find . \( -name .git -o -name node_modules -o -name .venv -o -name __pycache__ \) -prune -o -type f -name '*.xml' -size -32769k -newermt "@$1" -print0 |
  head -z -n 5000 | tar --format=gnu --null --verbatim-files-from --no-recursion -T - -cf - 2>/dev/null
`
	var out [][]byte
	_ = v.stream(ctx, script, []string{strconv.FormatInt(since.Add(-2*time.Second).Unix(), 10)}, func(r io.Reader) error {
		out = testreport.JUnitTar(r, since)
		return nil
	})
	return out
}

// Remove deletes the volume (sweeps).
func (v *CheckoutVolume) Remove() error { return removeVolume(v.Name) }

func removeVolume(name string) error {
	if out, err := docker("volume", "rm", "-f", name).CombinedOutput(); err != nil {
		return fmt.Errorf("docker volume rm %s: %v: %s", name, err, firstLine(string(out)))
	}
	return nil
}

// helper is a throwaway container on the volume: the engine's native arch, no
// network, nothing else mounted but the repo's git dir (read-only). Never the
// task's own container, which the task could have changed (its git, its tar).
func (v *CheckoutVolume) helper(ctx context.Context, readOnly bool, env []string, script string, args ...string) *exec.Cmd {
	mount := v.Name + ":/w"
	if readOnly {
		mount += ":ro"
	}
	a := []string{"run", "--rm", "-i", "--name", "cinema-helper-" + randomHex(), "--network", "none", "--label", "cinema.helper=1",
		"--label", "cinema.checkout=" + v.Slot, "-u", "0", "--entrypoint", "bash", "-v", mount}
	if !readOnly {
		a = append(a, "-v", v.GitDir+":"+v.GitDir+":ro")
	}
	for _, e := range env {
		a = append(a, "-e", e)
	}
	return dockerContext(ctx, append(append(a, v.image, "-c", script, "-"), args...)...)
}

// stream runs a read-only helper and hands its stdout to read.
func (v *CheckoutVolume) stream(ctx context.Context, script string, args []string, read func(io.Reader) error) error {
	cmd := v.helper(ctx, true, nil, script, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	rerr := read(out)
	_, _ = io.Copy(io.Discard, out)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("volume %s: %v: %s", v.Name, err, firstLine(stderr.String()))
	}
	return rerr
}

// SweepCheckoutVolumes removes checkout volumes whose slot is gone (swept,
// or a data dir that no longer exists) or matches drop. A volume in use (a
// running task) is left alone: docker refuses to remove it.
func SweepCheckoutVolumes(drop func(slot string) bool) {
	out, err := docker("volume", "ls", "-q", "--filter", "label=cinema.checkout").Output()
	if err != nil {
		return
	}
	for _, name := range strings.Fields(string(out)) {
		slot, err := docker("volume", "inspect", "--format", `{{index .Labels "cinema.checkout"}}`, name).Output()
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(slot))
		if _, err := os.Stat(s); errors.Is(err, os.ErrNotExist) || (drop != nil && drop(s)) {
			_ = docker("volume", "rm", name).Run()
		}
	}
}

// ensureVolume creates the volume, labeled with its slot, if it doesn't exist.
func (v *CheckoutVolume) ensure(ctx context.Context) error {
	if docker("volume", "inspect", v.Name).Run() == nil {
		return nil
	}
	if out, err := dockerContext(ctx, "volume", "create", "--label", "cinema.checkout="+v.Slot, v.Name).CombinedOutput(); err != nil {
		return fmt.Errorf("docker volume create: %v: %s", err, firstLine(string(out)))
	}
	return nil
}
