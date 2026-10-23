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
