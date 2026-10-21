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
)

// Platform is this machine as a task platform string.
func Platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

// Env is a task's environment fingerprint plus the human-readable inputs behind
// it, so `explain` can show what differed.
type Env struct {
	Digest string
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
func (t *Task) EnvFingerprint(root string) (Env, error) {
	var env Env
	e := newEnc("cinema.env.v1")
	platform := PlatformAny
	if t.Platform != PlatformAny {
		platform = Platform()
	}
	e.str(platform)
	env.Inputs = append(env.Inputs, "platform="+platform)
	// Root changes results: permission tests fail (or skip, i.e. pass) as root.
	// GitHub's runners are non-root, container jobs often root; they must match.
	user := "user=nonroot"
	if os.Geteuid() == 0 {
		user = "user=root"
	}
	e.str(user)
	env.Inputs = append(env.Inputs, user)

	dir := filepath.Join(root, filepath.FromSlash(t.Cwd))
	e.u64(uint64(len(t.Toolchain)))
	for _, tc := range t.Toolchain {
		if len(tc.Cmd) > 0 {
			out, err := toolOutput(dir, tc.Cmd)
			if err != nil {
				return Env{}, fmt.Errorf("toolchain %q: %w", strings.Join(tc.Cmd, " "), err)
			}
			e.str("cmd")
			e.list(tc.Cmd)
			e.str(out)
			env.Inputs = append(env.Inputs, strings.Join(tc.Cmd, " ")+" → "+firstLine(out))
			continue
		}
		d, err := hashRegularFile(filepath.Join(root, filepath.FromSlash(tc.File)))
		if err != nil {
			return Env{}, fmt.Errorf("toolchain file %s: %w", tc.File, err)
		}
		e.str("file")
		e.str(tc.File)
		e.str(d)
		env.Inputs = append(env.Inputs, tc.File+" → sha256:"+d[:12])
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
		env.Inputs = append(env.Inputs, "env "+n+" "+state) // never the value
	}
	env.Digest = e.sum()
	return env, nil
}

// toolOutput runs a version command: no shell, 30 s cap, combined output, trimmed.
func toolOutput(dir string, argv []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
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
