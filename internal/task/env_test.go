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

// A Linux task's environment includes the runner's shape: a 16-core machine
// (or a 2-CPU private runner) doesn't match a 4-CPU ubuntu-latest receipt, nor
// does a new OS release when ubuntu-latest moves.
func TestRunnerShapeInEnv(t *testing.T) {
	if got := OSRelease("NAME=\"Ubuntu\"\nID=ubuntu\nVERSION_ID=\"24.04\"\n"); got != "ubuntu-24.04" {
		t.Fatalf("OSRelease = %q", got)
	}
	tk := &Task{Argv: []string{"true"}, Platform: "linux/amd64"}
	fp := func(w *Where) string {
		env, err := tk.EnvFingerprintIn(t.TempDir(), nil, w)
		if err != nil {
			t.Fatal(err)
		}
		return env.Digest
	}
	base := &Where{Platform: "linux/amd64", CPUs: 4, MemGB: 16, OS: "ubuntu-24.04"}
	a := fp(base)
	for name, w := range map[string]*Where{
		"cpus": {Platform: "linux/amd64", CPUs: 2, MemGB: 16, OS: "ubuntu-24.04"},
		"mem":  {Platform: "linux/amd64", CPUs: 4, MemGB: 8, OS: "ubuntu-24.04"},
		"os":   {Platform: "linux/amd64", CPUs: 4, MemGB: 16, OS: "ubuntu-26.04"},
	} {
		if fp(w) == a {
			t.Errorf("%s change must change the environment", name)
		}
	}
	if fp(&Where{Platform: "linux/amd64", CPUs: 4, MemGB: 16, OS: "ubuntu-24.04"}) != a {
		t.Fatal("same shape, same environment")
	}
	// A runner VM's memory varies a little; CPUs never round.
	for _, c := range []struct {
		measured, declared int
		near               bool
	}{{15, 16, true}, {16, 16, true}, {17, 16, true}, {14, 16, false}, {8, 16, false}, {7, 8, true}, {9, 8, true}, {6, 8, false}} {
		if NearMemory(c.measured, c.declared) != c.near {
			t.Errorf("NearMemory(%d, %d) != %v", c.measured, c.declared, c.near)
		}
	}
	if (&Task{Memory: "16gb"}).MemoryGB() != 0 || (&Task{Memory: "8g"}).MemoryGB() != 8 {
		t.Fatal("memory parsing")
	}
}
