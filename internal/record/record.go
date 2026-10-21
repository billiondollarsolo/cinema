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
	"sync"
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
	Argv      []string      `json:"argv"`
	Path      string        `json:"path"` // resolved executable
	Start     time.Time     `json:"start"`
	Duration  time.Duration `json:"-"`                // monotonic; serialized by callers in their own unit
	ExitCode  int           `json:"exit_code"`        // 128+signo when signaled (shell convention)
	Signal    string        `json:"signal,omitempty"` // set when the child died from a signal
	Canceled  bool          `json:"canceled"`         // context ended before the child did
	Leftovers bool          `json:"leftovers"`        // child left processes running; they were stopped
	Stdout    Capture       `json:"stdout"`
	Stderr    Capture       `json:"stderr"`
}

// Run starts o.Argv directly (no shell) and waits for it. The returned error is
// non-nil only when the child could not be started or supervised; a child that
// ran and failed is a successful Run with a non-zero ExitCode.
//
// Cancelling ctx sends SIGTERM to the child's process group, then SIGKILL after Grace.
// When the child exits, anything it left running in its group is stopped the same way.
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
	// Own the pipes (instead of letting os/exec copy) so Wait returns when the child
	// exits, not when every leftover holding stdout exits; leftovers are reaped below.
	outR, outW, err := os.Pipe()
	if err != nil {
		return Result{Argv: o.Argv}, err
	}
	defer outR.Close()
	errR, errW, err := os.Pipe()
	if err != nil {
		outW.Close()
		return Result{Argv: o.Argv}, err
	}
	defer errR.Close()
	cmd.Stdout, cmd.Stderr = outW, errW
	// ponytail: own process group, no tty handoff (pipe mode only, plan §4.1). A child
	// that reads the terminal would get SIGTTIN, so callers must not pass a tty as Stdin.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = o.Grace // after cancel: SIGKILL the child if SIGTERM didn't stop it

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)

	res.Start = time.Now()
	startErr := cmd.Start()
	outW.Close() // the child has its own copies
	errW.Close()
	if startErr != nil {
		return Result{Argv: o.Argv}, startErr // zero Start: the child never ran
	}
	var copies sync.WaitGroup
	copies.Go(func() { _, _ = io.Copy(tee{o.Stdout, &res.Stdout}, outR) })
	copies.Go(func() { _, _ = io.Copy(tee{o.Stderr, &res.Stderr}, errR) })
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
	res.Canceled = ctx.Err() != nil
	res.Leftovers = reapGroup(pgid, o.Grace)

	// Pipes reach EOF once every writer is gone. A process that escaped the group
	// (setsid) may still hold one; stop waiting for it after Grace.
	drained := make(chan struct{})
	go func() { copies.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(o.Grace):
		outR.Close()
		errR.Close()
		<-drained
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
	// The child's status is authoritative; a Wait error that only reports it is not
	// a supervision failure.
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) && !res.Canceled {
		return res, waitErr
	}
	return res, nil
}

// reapGroup stops whatever the child left running in its process group:
// SIGTERM, then SIGKILL after grace (ACC-004). Processes that called setsid
// have left the group on purpose and are not touched. Reports whether
// anything was left behind.
func reapGroup(pgid int, grace time.Duration) bool {
	alive := func() bool { return syscall.Kill(-pgid, 0) == nil }
	if !alive() {
		return false // the normal case: one syscall
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	for deadline := time.Now().Add(grace); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if !alive() {
			return true
		}
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	return true
}

// tee copies to the terminal and the capture. Terminal errors (closed pipe,
// detached tty) are ignored: the child must never block on a full pipe because
// nobody is reading.
type tee struct {
	term io.Writer
	c    *Capture
}

func (t tee) Write(p []byte) (int, error) {
	if t.term != nil {
		_, _ = t.term.Write(p)
	}
	return t.c.Write(p)
}
