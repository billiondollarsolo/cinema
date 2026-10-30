package task

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/billiondollarsolo/cinema/internal/record"
)

// Platform is this machine as a task platform string.
func Platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

// Env is a task's environment fingerprint plus the human-readable inputs behind
// it, so `explain` can show what differed.
type Env struct {
	Digest string
	Image  string   // container tasks: the image pinned by digest (repo@sha256:...)
	Inputs []string // e.g. "platform=darwin/arm64", "go version → go version go1.27.1 darwin/arm64"
}

// EnvFingerprint computes the environment digest for t on this machine
// (plan §6.2): platform, whether it runs as root, toolchain outputs and
// allowlisted env vars. For
// platform "any" the platform is the literal "any", so results from any OS
// match; otherwise it's the real GOOS/GOARCH, so a macOS run never matches a
// Linux task.
//
// Env var values feed the digest only and are never shown: don't allowlist secrets.
//
// env is the environment the task runs with (RuntimeEnv; nil = inherited):
// probes run in it, so they report the runtime the task will actually use.
func (t *Task) EnvFingerprint(root string, env []string) (Env, error) {
	var fp Env
	e := newEnc("cinema.env.v1")
	platform := PlatformAny
	if t.Platform != PlatformAny {
		platform = Platform()
	}
	e.str(platform)
	fp.Inputs = append(fp.Inputs, "platform="+platform)
	// Root changes results: permission tests fail (or skip, i.e. pass) as root.
	// GitHub's runners are non-root, container jobs often root; they must match.
	user := "user=nonroot"
	if os.Geteuid() == 0 || t.Container != "" {
		user = "user=root" // container tasks run as root inside the image, like GitHub's
	}
	e.str(user)
	fp.Inputs = append(fp.Inputs, user)

	if t.Container != "" {
		img, err := PinnedImage(t.Container)
		if err != nil {
			return Env{}, fmt.Errorf("container %s: %w", t.Container, err)
		}
		e.str("container")
		e.str(img)
		fp.Image = img
		fp.Inputs = append(fp.Inputs, "container "+img)
	}
	for _, n := range sortedServices(t.Services) {
		img, err := PinnedImage(t.Services[n].Image)
		if err != nil {
			return Env{}, fmt.Errorf("service %s: %w", n, err)
		}
		e.str("service")
		e.str(n)
		e.str(img)
		fp.Inputs = append(fp.Inputs, "service "+n+" "+img)
	}
	dir := filepath.Join(root, filepath.FromSlash(t.Cwd))
	e.u64(uint64(len(t.Toolchain)))
	for _, tc := range t.Toolchain {
		if len(tc.Cmd) > 0 {
			out, err := toolOutput(dir, tc.Cmd, env)
			if err != nil {
				return Env{}, fmt.Errorf("toolchain %q: %w", strings.Join(tc.Cmd, " "), err)
			}
			e.str("cmd")
			e.list(tc.Cmd)
			e.str(out)
			fp.Inputs = append(fp.Inputs, strings.Join(tc.Cmd, " ")+" → "+firstLine(out))
			continue
		}
		if tc.Image != "" {
			d, err := imageDigest(tc.Image)
			if err != nil {
				return Env{}, fmt.Errorf("toolchain image %s: %w", tc.Image, err)
			}
			e.str("image")
			e.str(tc.Image)
			e.str(d)
			fp.Inputs = append(fp.Inputs, tc.Image+" → "+d[:min(len(d), 19)])
			continue
		}
		d, err := hashRegularFile(filepath.Join(root, filepath.FromSlash(tc.File)))
		if err != nil {
			return Env{}, fmt.Errorf("toolchain file %s: %w", tc.File, err)
		}
		e.str("file")
		e.str(tc.File)
		e.str(d)
		fp.Inputs = append(fp.Inputs, tc.File+" → sha256:"+d[:12])
	}

	names := sorted(t.Env)
	e.u64(uint64(len(names)))
	for _, n := range names {
		v, set := os.LookupEnv(n)
		e.str(n)
		e.bool(set)
		e.str(v)
		state := "unset"
		if set {
			state = "set"
		}
		fp.Inputs = append(fp.Inputs, "env "+n+" "+state) // never the value
	}
	fp.Digest = e.sum()
	return fp, nil
}

// toolOutput runs a version command: no shell, 30 s cap, combined output, trimmed.
func toolOutput(dir string, argv []string, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
		if p, err := record.LookPath(argv[0], env); err == nil {
			// The pinned runtime, not whatever is first on our PATH. Clearing Err
			// matters: exec.Command already failed its own lookup when the tool is
			// only on the task's PATH (a clean container: nothing preinstalled).
			cmd.Path, cmd.Err = p, nil
		}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, firstLine(string(out)))
	}
	return string(bytes.TrimSpace(out)), nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

const maxToolchainFile = 64 << 20

// hashRegularFile hashes a regular, non-symlink file of bounded size: a symlink
// to /dev/zero or a file outside the repo must not hang or leak (fail closed).
func hashRegularFile(path string) (string, error) {
	// O_NOFOLLOW refuses a symlink; O_NONBLOCK keeps a FIFO from blocking the open.
	// The type check is on the opened file, so nothing can be swapped in between.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("%w (symlinks aren't followed)", err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxToolchainFile+1))
	if err != nil {
		return "", err
	}
	if n > maxToolchainFile {
		return "", fmt.Errorf("larger than %d bytes", maxToolchainFile)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// imageDigest resolves an image reference to its content digest. A reference
// pinned with @sha256 is already immutable; a tag is resolved through the local
// docker daemon, pulling it first if it isn't present.
func imageDigest(ref string) (string, error) {
	if _, d, ok := strings.Cut(ref, "@sha256:"); ok {
		return "sha256:" + d, nil
	}
	inspect := func() (string, error) {
		return toolOutput("", []string{"docker", "image", "inspect", "--format", "{{json .RepoDigests}}", ref}, append(os.Environ(), DockerEnv()...))
	}
	out, err := inspect()
	if err != nil {
		// Pulls of builder images take minutes; version probes keep their 30 s cap.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		if out, perr := dockerContext(ctx, "pull", "-q", ref).CombinedOutput(); perr != nil {
			return "", fmt.Errorf("can't resolve (docker pull: %v: %s)", perr, firstLine(string(out)))
		}
		if out, err = inspect(); err != nil {
			return "", err
		}
	}
	// RepoDigests: ["repo@sha256:..."]; the image was pulled under this ref's repo.
	repo := ref
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		repo = ref[:i]
	}
	for _, rd := range strings.Split(strings.Trim(out, "[]\""), "\",\"") {
		if r, d, ok := strings.Cut(rd, "@"); ok && (r == repo || strings.HasSuffix(r, "/"+repo)) {
			return d, nil
		}
	}
	return "", fmt.Errorf("no registry digest for %s (built locally?)", ref)
}
