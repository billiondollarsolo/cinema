// Package record supervises one child process: exact argv, tee'd and bounded
// output, preserved exit semantics, process-group termination (plan §4.1).
package record

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// DefaultCaptureLimit is the per-stream capture bound (plan §4.1).
const DefaultCaptureLimit = 10 << 20

type Options struct {
	Argv         []string
	Dir          string    // "" = current directory
	Env          []string  // nil = inherit
	Stdin        io.Reader // nil = no stdin
	Stdout       io.Writer // terminal passthrough; nil = discard
	Stderr       io.Writer
	CaptureLimit int64         // 0 = DefaultCaptureLimit
	Grace        time.Duration // SIGTERM → SIGKILL delay, and pipe drain bound; 0 = 5s
}

type Result struct {
	Argv     []string      `json:"argv"`
	Path     string        `json:"path"` // resolved executable
	Start    time.Time     `json:"start"`
	Duration time.Duration `json:"-"` // monotonic; serialized by callers in their own unit
	ExitCode int           `json:"exit_code"`        // 128+signo when signaled (shell convention)
	Signal   string        `json:"signal,omitempty"` // set when the child died from a signal
	Canceled bool          `json:"canceled"`         // context ended before the child did
	Stdout   Capture       `json:"stdout"`
	Stderr   Capture       `json:"stderr"`
}

// Run starts o.Argv directly (no shell) and waits for it. The returned error is
// non-nil only when the child could not be started or supervised; a child that
// ran and failed is a successful Run with a non-zero ExitCode.
//
// Cancelling ctx sends SIGTERM to the child's process group, then SIGKILL after Grace.
// SIGINT/SIGTERM received by this process are forwarded to the group while it runs.
func Run(ctx context.Context, o Options) (Result, error) {
	if len(o.Argv) == 0 {
		return Result{}, errors.New("record: empty argv")
	}
	if o.CaptureLimit == 0 {
		o.CaptureLimit = DefaultCaptureLimit
	}
	if o.Grace == 0 {
		o.Grace = 5 * time.Second
	}
	res := Result{Argv: o.Argv, Stdout: Capture{Limit: o.CaptureLimit}, Stderr: Capture{Limit: o.CaptureLimit}}

	cmd := exec.CommandContext(ctx, o.Argv[0], o.Argv[1:]...)
	res.Path = cmd.Path
	cmd.Dir, cmd.Env, cmd.Stdin = o.Dir, o.Env, o.Stdin
	cmd.Stdout = io.MultiWriter(orDiscard(o.Stdout), &res.Stdout)
	cmd.Stderr = io.MultiWriter(orDiscard(o.Stderr), &res.Stderr)
	// ponytail: own process group, no tty handoff (pipe mode only, plan §4.1). A child
	// that reads the terminal would get SIGTTIN, so callers must not pass a tty as Stdin.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = o.Grace // bounds pipe draining when grandchildren keep stdout open

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)

	res.Start = time.Now()
	if err := cmd.Start(); err != nil {
		return Result{Argv: o.Argv}, err // zero Start: the child never ran
	}
	pgid := cmd.Process.Pid
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				_ = syscall.Kill(-pgid, s.(syscall.Signal))
			case <-done:
				return
			}
		}
	}()

	waitErr := cmd.Wait()
	close(done)
	res.Duration = time.Since(res.Start)
	if ctx.Err() != nil {
		res.Canceled = true
		_ = syscall.Kill(-pgid, syscall.SIGKILL) // stragglers that ignored SIGTERM (ACC-004)
	}

	ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	switch {
	case !ok:
		return res, waitErr
	case ws.Signaled():
		res.Signal = ws.Signal().String()
		res.ExitCode = 128 + int(ws.Signal())
	default:
		res.ExitCode = ws.ExitStatus()
	}
	// The child's status is authoritative. Wait errors that only report it, or a
	// pipe that WaitDelay had to close, are not supervision failures.
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) && !errors.Is(waitErr, exec.ErrWaitDelay) && !res.Canceled {
		return res, waitErr
	}
	return res, nil
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}
