package task

import (
	"strings"
	"testing"
)

// Lab (hono, container runs): `npm install -g pnpm@X` wrote into the read-only
// runtime mount and failed. Global installs go to the container's home, found
// first on PATH, as PrivateGlobals does for host runs.
func TestSandboxGlobalInstallsOnPath(t *testing.T) {
	p := strings.Split(sandboxPath([]string{"/opt/cinema/runtimes/node/node-v24.21.0-linux-x64/bin"}), ":")
	want := []string{sandboxNpmPrefix + "/bin", sandboxPnpmHome, sandboxPnpmHome + "/bin", "/opt/cinema/runtimes/node/node-v24.21.0-linux-x64/bin"}
	if len(p) < len(want) || strings.Join(p[:len(want)], ":") != strings.Join(want, ":") {
		t.Errorf("PATH = %q, want it to start with %q", p, want)
	}
	// The pnpm store is on the cache volume, never in the checkout.
	if !strings.HasPrefix(sandboxPnpmStore, "/home/runner/.cache/") {
		t.Errorf("pnpm store %s isn't on the cache volume", sandboxPnpmStore)
	}
	for _, d := range []string{sandboxNpmPrefix, sandboxPnpmHome} {
		if strings.HasPrefix(d, runtimesInside) || !strings.HasPrefix(d, "/home/runner/") {
			t.Errorf("%s isn't in the container's own home", d)
		}
	}
}
