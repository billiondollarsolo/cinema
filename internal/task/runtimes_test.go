package task

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A registered runtime: validated, provisioned for host runs (PATH, vars) and
// sandboxes, and in the contract only when pinned.
func TestRuntimeRegistry(t *testing.T) {
	RegisterRuntime(&RuntimeKind{Name: "fakelang", Example: "1.2.3", Version: regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`),
		Provision: func(dataDir, ver, osName, arch string) (string, error) {
			root := filepath.Join(dataDir, "runtimes", "fakelang", ver+"-"+osName+"-"+arch)
			return root, os.MkdirAll(filepath.Join(root, "bin"), 0o755)
		},
		Bins: []string{"bin"}, Vars: func(root string) []string { return []string{"FAKE_HOME=" + root} },
		Caches: map[string]string{"fake": ".fake/cache"}, Probe: []string{"fake", "--version"}})
	defer delete(runtimeKinds, "fakelang")

	parse := func(rt string) (*Task, error) {
		cfg, err := Parse([]byte("schema_version: 1\ntasks:\n  t:\n    argv: [true]\n    platform: any\n    runtime: {" + rt + "}\n"))
		if err != nil {
			return nil, err
		}
		return cfg.Tasks["t"], nil
	}
	if _, err := parse("nolang: 1.0.0"); err == nil || !strings.Contains(err.Error(), "doesn't provision nolang") {
		t.Fatalf("unknown runtime: %v", err)
	}
	if _, err := parse("fakelang: '1.2'"); err == nil || !strings.Contains(err.Error(), "exact version like 1.2.3") {
		t.Fatalf("inexact version: %v", err)
	}
	tk, err := parse("fakelang: 1.2.3")
	if err != nil || tk.Runtime.Get("fakelang") != "1.2.3" || !tk.Runtime.Set() {
		t.Fatalf("%v %+v", err, tk)
	}
	data := t.TempDir()
	env, err := tk.RuntimeEnv(data)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "PATH="+filepath.Join(data, "runtimes", "fakelang")) || !strings.Contains(joined, "FAKE_HOME="+filepath.Join(data, "runtimes", "fakelang")) {
		t.Fatalf("host env lacks the runtime:\n%s", joined)
	}
	bins, vars, caches, err := tk.RuntimeSandbox(data, "linux", "amd64")
	if err != nil || len(bins) != 1 || !strings.HasSuffix(bins[0], "1.2.3-linux-amd64/bin") || len(vars) != 1 || caches["fake"] != ".fake/cache" {
		t.Fatalf("sandbox: %v %v %v %v", err, bins, vars, caches)
	}
	plain := &Task{ID: "t", Argv: []string{"true"}, Platform: PlatformAny}
	pinned := &Task{ID: "t", Argv: []string{"true"}, Platform: PlatformAny, Runtime: Runtime{Other: map[string]string{"fakelang": "1.2.3"}}}
	a, _ := plain.ContractDigest(t.TempDir())
	b, _ := pinned.ContractDigest(t.TempDir())
	if a == b {
		t.Fatal("a pinned runtime must change the contract")
	}
}
