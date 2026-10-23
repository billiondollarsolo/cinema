package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// Lines from the lab's failed drafts (traefik, vite, eslint).
func TestMissingTools(t *testing.T) {
	out := []byte(`Makefile:148: *** "No zz-shellcheck in PATH".  Stop.
./ci.sh: line 3: zz-yq: command not found
sh: 1: zz-dash: not found
[FAILED] Error: spawn zz-Xvfb ENOENT
exec: "zz-kind": executable file not found in $PATH
chrome: error while loading shared libraries: libnspr4.so: cannot open shared object file
sh: command not found
package.json: not found
sh: 1: sh: not found
make: zz-unzip: No such file or directory`)
	want := []string{"libnspr4.so", "zz-Xvfb", "zz-dash", "zz-kind", "zz-shellcheck", "zz-unzip", "zz-yq"}
	if got := missingTools(out); !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestCPUSet(t *testing.T) {
	for _, c := range []struct {
		n, slot, total int
		want           []int
	}{
		{0, 0, 16, nil}, {16, 0, 16, nil}, {32, 1, 16, nil},
		{4, 0, 16, []int{0, 1, 2, 3}}, {4, 1, 16, []int{4, 5, 6, 7}},
		{4, 3, 10, []int{2, 3, 4, 5}}, // wraps
	} {
		if got := cpuSet(c.n, c.slot, c.total); !slices.Equal(got, c.want) {
			t.Errorf("cpuSet(%d,%d,%d) = %v, want %v", c.n, c.slot, c.total, got, c.want)
		}
	}
}

// CLEAN-008: shared artifacts unused for a day are swept, and partial writes
// or stale locks after an hour; fresh entries stay.
func TestSweepArtifacts(t *testing.T) {
	t.Setenv("CINEMA_DATA_DIR", t.TempDir())
	root, err := artifactsRoot()
	if err != nil {
		t.Fatal(err)
	}
	old, hour := time.Now().Add(-25*time.Hour), time.Now().Add(-2*time.Hour)
	for name, at := range map[string]time.Time{"old": old, "fresh": time.Now(), "fresh.lock": time.Now(), "k.lock": hour, "k.tmp": hour, "recent": hour} {
		p := filepath.Join(root, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	sweepRunDirs()
	entries, _ := os.ReadDir(root)
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if want := []string{"fresh", "fresh.lock", "recent"}; !slices.Equal(left, want) {
		t.Fatalf("left %v, want %v", left, want)
	}
}
