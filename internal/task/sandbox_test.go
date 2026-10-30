package task

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A task can rewrite its checkout's git alternates; the next sandbox must not
// mount whatever host path it names (security review).
func TestSandboxRefusesForeignAlternates(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "objects", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, ".git", "objects", "info", "alternates"), []byte("/home/me/.ssh\n"), 0o644)
	_, err := StartSandbox(context.Background(), SandboxSpec{Image: "x@sha256:" + strings.Repeat("0", 64), Platform: "linux/amd64",
		Root: root, Objects: "/real/repo/.git/objects", DataDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "refusing to mount") {
		t.Fatalf("got %v", err)
	}
}
