package analyze

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The drafted body builds the artifact in its own copy of the tree, as the
// producing job's runner would, and the consumer sees only the artifact: not
// the producer's other files, not its env.
func TestArtifactRebuilt(t *testing.T) {
	src := `on: pull_request
env: {SHARED: wf}
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: mkdir -p dist && echo "built:$SHARED:${ONLY_TEST:-unset}" > dist/out.txt && touch scratch.txt
      - uses: actions/upload-artifact@v4
        with: {name: dist, path: dist}
  test:
    needs: [build]
    runs-on: ubuntu-latest
    env: {ONLY_TEST: "yes"}
    steps:
      - uses: actions/checkout@v4
      - run: echo consumer-was-here > mine.txt
      - uses: actions/download-artifact@v4
        with: {name: dist, path: dist}
      - run: cat dist/out.txt && test ! -e scratch.txt && test ! -e dist/mine.txt
  file:
    needs: build
    runs-on: ubuntu-latest
    steps:
      - uses: actions/download-artifact@v4
        with: {name: dist}
  elsewhere:
    needs: build
    runs-on: ubuntu-latest
    steps:
      - uses: actions/download-artifact@v4
        with: {name: dist, path: out}
`
	jobs, err := Workflow("ci.yml", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Job{}
	for _, j := range jobs {
		by[j.ID] = j
	}
	tj := by["test"]
	if tj.Verdict != Task || tj.Proposal == nil {
		t.Fatalf("test: %v %v", tj.Verdict, tj.Reasons)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-ec", "export SHARED=wf ONLY_TEST=yes\n"+tj.Proposal.Body)
	cmd.Dir, cmd.Env = dir, append(os.Environ(), "TMPDIR="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "built:wf:unset") {
		t.Fatalf("%v\n%s\n--- body:\n%s", err, out, tj.Proposal.Body)
	}
	// dist is uploaded as a directory, so downloading it to "." (the file job)
	// would spread its contents at the root, not recreate dist/: refused at run
	// time, since only the run knows dist is a directory.
	if fj := by["file"]; fj.Verdict != Task || !strings.Contains(fj.Proposal.Body, "test -f") {
		t.Fatalf("file: %v %v", fj.Verdict, fj.Reasons)
	} else {
		cmd := exec.Command("bash", "-ec", fj.Proposal.Body)
		cmd.Dir, cmd.Env = dir, append(os.Environ(), "TMPDIR="+t.TempDir())
		if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "is not a file") {
			t.Fatalf("file job must fail closed: %v\n%s", err, out)
		}
	}
	if ej := by["elsewhere"]; ej.Verdict != Skip || !strings.Contains(ej.Reasons[0], "can't place it") {
		t.Fatalf("elsewhere: %v %v", ej.Verdict, ej.Reasons)
	}
}
