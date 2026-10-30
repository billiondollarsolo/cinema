package task

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A probe finds its tool on the task's PATH even when cinema's own PATH has
// none (P4-1: a clean container, Node only from runtime:).
func TestProbeUsesTaskPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "zz-tool"), []byte("#!/bin/sh\necho zz 1.2.3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := toolOutput(t.TempDir(), []string{"zz-tool", "--version"}, []string{"PATH=" + bin})
	if err != nil || out != "zz 1.2.3" {
		t.Fatalf("got %q, %v", out, err)
	}
}
