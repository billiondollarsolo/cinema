package main

import (
	"slices"
	"testing"
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
sh: 1: sh: not found`)
	want := []string{"libnspr4.so", "zz-Xvfb", "zz-dash", "zz-kind", "zz-shellcheck", "zz-yq"}
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
