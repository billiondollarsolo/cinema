package transport

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// repos: a bare remote and two clones ("laptop", "vps") with one commit.
func repos(t *testing.T) (remote, a, b, head string) {
	base := t.TempDir()
	remote = filepath.Join(base, "remote.git")
	run(t, base, "init", "-q", "--bare", "-b", "main", remote)
	a = filepath.Join(base, "a")
	run(t, base, "clone", "-q", remote, a)
	os.WriteFile(filepath.Join(a, "f"), []byte("x"), 0o644)
	run(t, a, "add", "-A")
	run(t, a, "commit", "-qm", "c")
	run(t, a, "push", "-q", "origin", "HEAD:main")
	b = filepath.Join(base, "b")
	run(t, base, "clone", "-q", remote, b)
	return remote, a, b, run(t, a, "rev-parse", "HEAD")
}

func env(s string) json.RawMessage { return json.RawMessage(`{"payloadType":"` + s + `"}`) }

func TestWriteReadSquash(t *testing.T) {
	_, a, _, head := repos(t)
	ref := Ref("aaaa")
	if got, err := Read(a, ref, head); got != nil || err != nil {
		t.Fatalf("no note yet: %v %v", got, err)
	}
	if err := Write(a, ref, head, []json.RawMessage{env("1")}); err != nil {
		t.Fatal(err)
	}
	if err := Write(a, ref, head, []json.RawMessage{env("1"), env("2")}); err != nil { // replace
		t.Fatal(err)
	}
	if err := Squash(a, ref); err != nil {
		t.Fatal(err)
	}
	if n := run(t, a, "rev-list", "--count", ref); n != "1" {
		t.Fatalf("squashed ref has %s commits", n)
	}
	if who := run(t, a, "log", "-1", "--format=%an <%ae> / %cn <%ce>", ref); who != "cinema <cinema@invalid> / cinema <cinema@invalid>" {
		t.Fatalf("notes commit identity: %s", who)
	}
	got, err := Read(a, ref, head)
	if err != nil || len(got) != 2 {
		t.Fatalf("after squash: %v %v", got, err)
	}
}

func TestOversizedNoteRejected(t *testing.T) { // ACC-027
	_, a, _, head := repos(t)
	ref := Ref("big")
	big := strings.Repeat("x", MaxNote+1)
	cmd := exec.Command("git", "-C", a, "notes", "--ref="+ref, "add", "-f", "-m", big, head)
	cmd.Env = append(os.Environ(), "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if _, err := Read(a, ref, head); err == nil {
		t.Fatal("oversized note accepted")
	}
	if err := Write(a, Ref("w"), head, make([]json.RawMessage, MaxReceipts+1)); err == nil {
		t.Fatal("wrote over MaxReceipts")
	}
}

func TestTwoMachinesSeparateRefs(t *testing.T) { // ACC-032
	remote, a, b, head := repos(t)
	Write(a, Ref("laptop"), head, []json.RawMessage{env("laptop")})
	Write(b, Ref("vps"), head, []json.RawMessage{env("vps")})
	for _, x := range []struct{ dir, id string }{{a, "laptop"}, {b, "vps"}} {
		if r := Push(x.dir, "origin", "", Ref(x.id)); r.EvidenceErr != nil {
			t.Fatal(r.EvidenceErr)
		}
	}
	if err := Fetch(a, "origin", ""); err != nil {
		t.Fatal(err)
	}
	refs, _ := Refs(a)
	if len(refs) != 2 {
		t.Fatalf("refs after fetch: %v (remote %s)", refs, remote)
	}
}

func TestAtomicFallback(t *testing.T) { // ACC-033
	remote, a, _, head := repos(t)
	run(t, remote, "config", "receive.advertiseAtomic", "false")
	os.WriteFile(filepath.Join(a, "g"), []byte("y"), 0o644)
	run(t, a, "add", "-A")
	run(t, a, "commit", "-qm", "d")
	Write(a, Ref("k"), head, []json.RawMessage{env("k")})
	r := Push(a, "origin", "HEAD:refs/heads/main", Ref("k"))
	if r.Atomic || r.BranchErr != nil || r.EvidenceErr != nil {
		t.Fatalf("fallback: %+v", r)
	}
	if got := run(t, remote, "rev-parse", "main"); got != run(t, a, "rev-parse", "HEAD") {
		t.Fatal("branch not pushed by fallback")
	}
}
