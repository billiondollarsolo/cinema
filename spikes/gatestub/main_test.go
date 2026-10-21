package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Round trip, byte flip, and wrong key: only the untouched envelope is REUSE.
func TestGateStub(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "gatestub")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	run := func(args ...string) (string, int) {
		cmd := exec.Command(bin, args...)
		out, _ := cmd.Output()
		return strings.TrimSpace(string(out)), cmd.ProcessState.ExitCode()
	}
	key, other := filepath.Join(dir, "k"), filepath.Join(dir, "k2")
	pub, _ := run("keygen", key)
	otherPub, _ := run("keygen", other)
	payload := filepath.Join(dir, "p.json")
	os.WriteFile(payload, []byte(`{"_type":"https://in-toto.io/Statement/v1"}`), 0o644)
	envJSON, _ := run("sign", key, payload)
	env := filepath.Join(dir, "env.json")
	os.WriteFile(env, []byte(envJSON), 0o644)

	if out, code := run("verify", pub, env); code != 0 || out != "REUSE" {
		t.Fatalf("valid envelope: %q %d", out, code)
	}
	if out, code := run("verify", otherPub, env); code != 1 || out != "RUN SIGNATURE_INVALID" {
		t.Fatalf("untrusted key: %q %d", out, code)
	}
	// Flip one payload byte: base64 "eyJf" starts the payload; change a char later on.
	flipped := strings.Replace(envJSON, `"payload":"eyJf`, `"payload":"eyJg`, 1)
	os.WriteFile(env, []byte(flipped), 0o644)
	if out, code := run("verify", pub, env); code != 1 || out != "RUN SIGNATURE_INVALID" {
		t.Fatalf("flipped payload: %q %d", out, code)
	}
}
