package record

import (
	"context"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func run(t *testing.T, ctx context.Context, o Options) Result {
	t.Helper()
	if o.Grace == 0 {
		o.Grace = 500 * time.Millisecond
	}
	r, err := Run(ctx, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return r
}

func sh(script string) []string { return []string{"/bin/sh", "-c", script} }

func TestExitCodes(t *testing.T) { // ACC-001, ACC-002, ACC-008
	for _, tc := range []struct {
		script string
		code   int
	}{
		{"exit 0", 0},
		{"exit 3", 3},
		{"false | true", 0}, // only the explicit shell's outcome is recorded
	} {
		if r := run(t, t.Context(), Options{Argv: sh(tc.script)}); r.ExitCode != tc.code || r.Signal != "" {
			t.Errorf("%q: exit %d signal %q, want %d", tc.script, r.ExitCode, r.Signal, tc.code)
		}
	}
}

func TestSignal(t *testing.T) { // ACC-003
	r := run(t, t.Context(), Options{Argv: sh("kill -TERM $$")})
	if r.Signal != "terminated" || r.ExitCode != 128+15 {
		t.Fatalf("got signal %q exit %d", r.Signal, r.ExitCode)
	}
}

func TestCancelKillsGrandchildren(t *testing.T) { // ACC-004
	pidfile := t.TempDir() + "/pid"
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	// The grandchild ignores SIGTERM, so only the group SIGKILL can stop it.
	r := run(t, ctx, Options{Argv: sh(`sh -c 'trap "" TERM; sleep 60' & echo $! > ` + pidfile + `; wait`)})
	if !r.Canceled || r.ExitCode == 0 {
		t.Fatalf("canceled=%v exit=%d", r.Canceled, r.ExitCode)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	time.Sleep(100 * time.Millisecond)
	if syscall.Kill(pid, 0) == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("grandchild %d survived", pid)
	}
}

func TestBackgroundedGrandchildDoesNotHang(t *testing.T) {
	start := time.Now()
	r := run(t, t.Context(), Options{Argv: sh("sleep 30 & echo started")})
	if r.ExitCode != 0 || time.Since(start) > 3*time.Second {
		t.Fatalf("exit %d after %v", r.ExitCode, time.Since(start))
	}
}

func TestBoundedCapture(t *testing.T) { // ACC-007
	var term strings.Builder
	r := run(t, t.Context(), Options{
		Argv:         sh("head -c 100000 /dev/zero | tr '\\0' x"),
		Stdout:       &term,
		CaptureLimit: 1000,
	})
	if term.Len() != 100000 {
		t.Errorf("terminal got %d bytes, want all 100000", term.Len())
	}
	if r.Stdout.Total != 100000 || !r.Stdout.Truncated {
		t.Errorf("capture total=%d truncated=%v", r.Stdout.Total, r.Stdout.Truncated)
	}
}

func TestCaptureKeepsHeadAndTail(t *testing.T) {
	for _, chunk := range []int{1, 7, 64, 5000} { // write sizes smaller and larger than the ring
		c := Capture{Limit: 100}
		var all []byte
		for i := 0; len(all) < 1000; i++ {
			b := make([]byte, chunk)
			for j := range b {
				b[j] = byte('a' + (len(all)+j)%26)
			}
			all = append(all, b...)
			c.Write(b)
		}
		got := string(c.Bytes())
		head, tail := string(all[:20]), string(all[len(all)-80:])
		marker := "\n… cinema: " + strconv.Itoa(len(all)-100) + " bytes omitted …\n"
		if got != head+marker+tail {
			t.Errorf("chunk %d: got %q", chunk, got)
		}
	}
	c := Capture{Limit: 100}
	c.Write([]byte("small"))
	if string(c.Bytes()) != "small" || c.Truncated {
		t.Errorf("under limit: %q truncated=%v", c.Bytes(), c.Truncated)
	}
	c = Capture{Limit: 100}
	c.Write(make([]byte, 100)) // exactly at the limit: complete, not truncated
	if len(c.Bytes()) != 100 || c.Truncated {
		t.Errorf("at limit: len %d truncated=%v", len(c.Bytes()), c.Truncated)
	}
}

func TestNotFound(t *testing.T) {
	if _, err := Run(t.Context(), Options{Argv: []string{"definitely-not-a-command-xyz"}}); err == nil {
		t.Fatal("want start error")
	}
}
