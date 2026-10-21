package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/billiondollarsolo/cinema/internal/task"
)

// Scripts worth a task, and whether their result is usually host-independent.
// "Usually" is why init only suggests `any` in a comment (plan §6.1: never infer).
var knownScripts = map[string]bool{
	"lint": true, "typecheck": true, "type-check": true, "format:check": true, "fmt:check": true,
	"test": false, "build": false, "check": false,
}

func cmdInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return exitBadArgs
	}
	r, err := openRepo()
	defer r.close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cinema init: %v\n", err)
		return exitBadArgs
	}
	path := filepath.Join(r.root, task.ConfigPath)
	if _, err := os.Stat(path); err == nil {
		fmt.Fprintf(os.Stderr, "cinema init: %s already exists; not touching it\n", task.ConfigPath)
		return exitBadArgs
	}

	var b strings.Builder
	b.WriteString(`# CInema task contracts (plan.md §6). Each task is a check your CI runs.
# A local run counts for a task only if its argv and cwd match exactly.
#
# platform: the OS/arch whose result CI will accept. "any" is YOUR claim that
# the result doesn't depend on the host. CInema never guesses it. Lint,
# typecheck and format checks usually qualify; tests and builds usually don't.
schema_version: 1
tasks:
`)
	n := 0
	if _, err := os.Stat(filepath.Join(r.root, "go.mod")); err == nil {
		n += 2
		b.WriteString(`  go-vet:
    argv: [go, vet, ./...]
    platform: linux/amd64        # "any" if no files use OS/arch build constraints
    toolchain: [{cmd: [go, version]}]
    reuse: { allowed: true, max_age: 72h }
  go-test:
    argv: [go, test, -count=1, ./...]
    platform: linux/amd64
    toolchain: [{cmd: [go, version]}]
    env: [CGO_ENABLED]
    reuse: { allowed: true, max_age: 72h }
`)
	}
	for _, dir := range packageDirs(r.root) {
		n += writeNodeTasks(&b, r.root, dir)
	}
	if n == 0 {
		b.WriteString(`  example:
    argv: [make, test]
    platform: linux/amd64
    reuse: { allowed: true, max_age: 72h }
`)
	}
	if _, err := task.Parse([]byte(b.String())); err != nil {
		fmt.Fprintf(os.Stderr, "cinema init: generated an invalid config (bug): %v\n%s", err, b.String())
		return exitInternal
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "cinema init: %v\n", err)
		return exitInternal
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "cinema init: %v\n", err)
		return exitInternal
	}
	fmt.Printf(`wrote %s with %d task(s)

next:
  1. edit it: make each argv exactly what CI runs, set platform: any where honest
  2. cinema trust          allow its toolchain commands
  3. cinema run --all      run what isn't reusable yet
  4. cinema status         see what CI could reuse

for coding agents, add to AGENTS.md / CLAUDE.md:
  Run checks with `+"`cinema run <task>`"+` (or `+"`cinema run --all`"+`). After your last
  edit, confirm with `+"`cinema status --json`"+`: exit 0 means every task this
  machine can produce is reusable for the current tree.
`, task.ConfigPath, max(n, 1))
	return 0
}

// packageDirs: the repo root and its immediate subdirectories that have a package.json.
func packageDirs(root string) []string {
	var dirs []string
	if exists(filepath.Join(root, "package.json")) {
		dirs = append(dirs, ".")
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && e.Name() != "node_modules" &&
			exists(filepath.Join(root, e.Name(), "package.json")) {
			dirs = append(dirs, e.Name())
		}
	}
	return dirs
}

func writeNodeTasks(b *strings.Builder, root, dir string) int {
	raw, err := os.ReadFile(filepath.Join(root, dir, "package.json"))
	if err != nil {
		return 0
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(raw, &pkg) != nil {
		return 0
	}
	pm, lock := "npm", ""
	for _, c := range []struct{ pm, lock string }{{"pnpm", "pnpm-lock.yaml"}, {"yarn", "yarn.lock"}, {"npm", "package-lock.json"}} {
		if exists(filepath.Join(root, dir, c.lock)) {
			pm, lock = c.pm, c.lock
			break
		}
	}
	var names []string
	for s := range pkg.Scripts {
		if _, ok := knownScripts[s]; ok {
			names = append(names, s)
		}
	}
	slices.Sort(names)
	prefix := ""
	if dir != "." {
		prefix = strings.ReplaceAll(dir, "_", "-") + "-"
	}
	for _, s := range names {
		id := strings.ToLower(prefix + strings.NewReplacer(":", "-", "_", "-").Replace(s))
		fmt.Fprintf(b, "  %s:\n    argv: [%s, run, %q]\n", id, pm, s)
		if dir != "." {
			fmt.Fprintf(b, "    cwd: %s\n", dir)
		}
		comment := ""
		if knownScripts[s] {
			comment = `        # usually safe as "any"`
		}
		fmt.Fprintf(b, "    platform: linux/amd64%s\n    toolchain:\n      - cmd: [node, --version]\n", comment)
		if lock != "" {
			fmt.Fprintf(b, "      - file: %s\n", filepath.ToSlash(filepath.Join(dir, lock)))
		}
		b.WriteString("    reuse: { allowed: true, max_age: 72h }\n")
	}
	return len(names)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return !errors.Is(err, os.ErrNotExist)
}
