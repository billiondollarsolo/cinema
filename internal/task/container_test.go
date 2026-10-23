package task

import (
	"slices"
	"strings"
	"testing"
)

func TestContainerValidation(t *testing.T) {
	for yaml, want := range map[string]string{
		"container: alpine:3.20\nruntime: {go: 1.26.10}":            "can't be combined",
		"container: alpine:3.20\ntoolchain: [{cmd: [go, version]}]": "toolchain cmd probes",
	} {
		_, err := Parse([]byte("schema_version: 1\ntasks:\n  t:\n    argv: [\"true\"]\n    platform: linux/amd64\n    " + strings.ReplaceAll(yaml, "\n", "\n    ") + "\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err %v, want %q", yaml, err, want)
		}
	}
}

func TestContainerRunArgv(t *testing.T) {
	tk := &Task{Argv: []string{"make", "test"}, Cwd: "web"}
	argv, _ := tk.ContainerRun("/w/slot", "quay.io/x/y@sha256:abc", "", []int{4, 5})
	s := strings.Join(argv, " ")
	for _, want := range []string{"docker run --rm --name cinema-", "-v /w/slot:/w/slot", "-w /w/slot/web", "--cpuset-cpus 4,5", "quay.io/x/y@sha256:abc -c", "sh make test"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %s", want, s)
		}
	}
	if !slices.Contains(argv, "--entrypoint") || !strings.Contains(s, "chown -R ") || !strings.HasSuffix(s, "make test") {
		t.Errorf("argv: %s", s)
	}
}

func TestContainerKeepsOtherContracts(t *testing.T) {
	a := &Task{ID: "t", Argv: []string{"true"}, Platform: "linux/amd64"}
	b := *a
	b.Container = "alpine:3.20"
	da, _ := a.ContractDigest(t.TempDir())
	db, _ := b.ContractDigest(t.TempDir())
	if da == db {
		t.Fatal("container: must be part of the contract")
	}
}
