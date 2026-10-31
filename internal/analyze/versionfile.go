package analyze

import (
	"regexp"
	"strings"
)

var (
	// setup-node's version-file line: "nodejs 24.1.0", "node 24", or a bare version.
	toolVersionsNode = regexp.MustCompile(`(?m)^(?:node(?:js)?\s+)?v?([^\s]+)$`)
	// setup-go's: the golang line.
	toolVersionsGo = regexp.MustCompile(`(?m)^golang\s+([^\n#]+)`)
)

// toolVersionsEntry picks Go's or Node's version out of an asdf .tool-versions
// file the way setup-go and setup-node read node-version-file/go-version-file
// (lab: hono's .tool-versions lists nodejs, bun, deno and pnpm). "" if absent.
func toolVersionsEntry(content, tool string) string {
	re := toolVersionsNode
	if tool == "Go" {
		re = toolVersionsGo
	}
	if m := re.FindStringSubmatch(strings.ReplaceAll(content, "\r", "")); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}
