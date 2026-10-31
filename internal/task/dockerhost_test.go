package task

import (
	"strings"
	"testing"
)

// docker: host is strict, part of the contract (the same argv tests more
// with it), and its engine's platform is part of the environment.
func TestDockerHostField(t *testing.T) {
	base := "schema_version: 1\ntasks:\n  t:\n    argv: [./mvnw, verify]\n    platform: linux/amd64\n"
	for name, body := range map[string]string{
		"other value":    "    docker: dind\n",
		"bool":           "    docker: true\n",
		"container task": "    docker: host\n    container: maven:3\n",
	} {
		if _, err := Parse([]byte(base + body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Parse([]byte("schema_version: 1\ntasks:\n  t: {argv: [x], platform: any, docker: host}\n")); err == nil {
		t.Error("platform: any with docker: host accepted (the engine runs Linux containers)")
	}
	plain := mustParse(t, base).Tasks["t"]
	dock := mustParse(t, base+"    docker: host\n").Tasks["t"]
	root := t.TempDir()
	a, _ := plain.ContractDigest(root)
	b, err := dock.ContractDigest(root)
	if err != nil || a == b {
		t.Fatalf("docker: host must change the contract (%v)", err)
	}

	w := func(docker string) *Where {
		return &Where{Platform: "linux/amd64", CPUs: 4, MemGB: 16, OS: "ubuntu-26.04", Docker: docker}
	}
	amd, err := dock.EnvFingerprintIn(root, nil, w("linux/amd64"))
	if err != nil {
		t.Fatal(err)
	}
	arm, _ := dock.EnvFingerprintIn(root, nil, w("linux/arm64"))
	if amd.Digest == arm.Digest {
		t.Fatal("the engine's platform must be in the environment: its containers run as its arch")
	}
	if !strings.Contains(strings.Join(amd.Inputs, "\n"), "docker engine linux/amd64") {
		t.Fatalf("inputs: %v", amd.Inputs)
	}
	if _, err := dock.EnvFingerprintIn(root, nil, w("")); err == nil {
		t.Fatal("no engine: the environment is unknown, never a guess")
	}
	if p, _ := plain.EnvFingerprintIn(root, nil, w("")); p.Digest == amd.Digest {
		t.Fatal("same environment with and without docker")
	}
}

func TestDefaultGateway(t *testing.T) {
	for in, want := range map[string]string{
		"default via 172.17.0.1 dev eth0 \n":  "172.17.0.1",
		"default via fd00::1 dev eth0":        "fd00::1",
		"172.17.0.0/16 dev eth0 proto kernel": "",
		"default via ; rm -rf / dev eth0":     "",
	} {
		if got := defaultGateway(in); got != want {
			t.Errorf("%q: got %q", in, got)
		}
	}
	env := strings.Join(dockerHostEnv("172.17.0.1"), " ")
	for _, want := range []string{"DOCKER_HOST=unix:///var/run/docker.sock", "SERVICES_HOST=172.17.0.1", "TESTCONTAINERS_HOST_OVERRIDE=172.17.0.1"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s: %s", want, env)
		}
	}
}
