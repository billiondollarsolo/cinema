package analyze

import (
	"os"
	"path/filepath"
	"testing"
)

// Lab (hono): setup-node's node-version-file: .tool-versions drafted as review
// ("Node version comes from .tool-versions") while setup-bun and setup-deno
// read the same file.
func TestToolVersionsNodeAndGo(t *testing.T) {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, ".tool-versions"), []byte("bun  1.2.19\r\nnodejs 24.21.0\r\ngolang 1.26.10 # pinned\ndeno 2.4.5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var reviews []string
	rev := func(s string) { reviews = append(reviews, s) }
	if v := exactVersion(d, "", `^[0-9]+\.[0-9]+\.[0-9]+$`, "Node", ".tool-versions", "", rev, func(string) {}); v != "24.21.0" {
		t.Errorf("Node from .tool-versions = %q, want 24.21.0 (reviews %q)", v, reviews)
	}
	if v := exactVersion(d, "", `^1\.[0-9]+\.[0-9]+$`, "Go", ".tool-versions", "", rev, func(string) {}); v != "1.26.10" {
		t.Errorf("Go from .tool-versions = %q, want 1.26.10 (reviews %q)", v, reviews)
	}
	for _, c := range []struct{ in, tool, want string }{
		{"node v22.1.0\n", "Node", "22.1.0"},
		{"22.1.0\n", "Node", "22.1.0"},
		{"bun 1.2.19\n", "Node", ""},
		{"nodejs 24.21.0\n", "Go", ""},
	} {
		if got := toolVersionsEntry(c.in, c.tool); got != c.want {
			t.Errorf("toolVersionsEntry(%q, %s) = %q, want %q", c.in, c.tool, got, c.want)
		}
	}
}
