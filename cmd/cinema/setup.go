package main

import (
	"bufio"
	_ "embed"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/billiondollarsolo/cinema/internal/analyze"
	"github.com/billiondollarsolo/cinema/internal/snapshot"
	"github.com/billiondollarsolo/cinema/internal/task"
)

//go:embed gate.yml
var gateTemplate string

const gatePath = ".github/workflows/cinema.yml"

// cmdSetup is the guided path from existing CI to a working cinema setup: what
// this machine can take over, the files that does it, formatted the repo's way,
// written only after a yes. Nothing else changes (no keys, no pushes).
func cmdSetup(args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	withReview := fs.Bool("include-review", false, "also draft jobs with open issues (marked REVIEW): check each before relying on it")
	yes := fs.Bool("yes", false, "write without asking")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: cinema setup [--include-review] [--yes]")
		return exitBadArgs
	}
	cwd, _ := os.Getwd()
	root, err := snapshot.RepoRoot(cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cinema setup: %v\n", err)
		return exitBadArgs
	}
	jobs, err := analyze.Repo(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cinema setup: %v\n", err)
		return exitBadArgs
	}
	m := probeMachine()
	var picked []analyze.Job
	cm := make([]catMin, len(jobs))
	for i, j := range jobs {
		cat, _ := readiness(j, m)
		cm[i] = catMin{cat: cat}
		if j.Proposal != nil && (cat == "ready" || (*withReview && cat == "review")) {
			picked = append(picked, j)
		}
	}
	printCapability(m, cm, false)
	if len(picked) == 0 {
		fmt.Println("\nNothing for this machine to take over yet: see what each job needs above (`cinema analyze` has the details).")
		return exitUnsatisfied
	}
	tasks, err := draft(picked)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cinema setup: %v\n", err)
		return exitInternal
	}
	version := selfVersion()
	files := []struct{ path, body, what string }{
		{task.ConfigPath, string(tasks), fmt.Sprintf("%d tasks: %s", len(picked), taskIDs(picked))},
		{gatePath, strings.ReplaceAll(gateTemplate, "CINEMA_VERSION", version), "the CI gate: reuses verified receipts, runs the rest (one job per task)"},
	}
	fmt.Println("\ncinema setup would write:")
	for _, f := range files {
		state := "new"
		if _, err := os.Stat(filepath.Join(root, f.path)); err == nil {
			state = "REPLACES the existing file"
		}
		fmt.Printf("  %-32s %s (%s)\n", f.path, f.what, state)
	}
	if strings.Contains(version, "latest") {
		fmt.Println("\n  note: this cinema isn't a tagged build, so the gate installs @latest; pin a version before relying on it")
	}
	if !*yes && !confirm("\nWrite these files? [y/N] ") {
		fmt.Println("Nothing written.")
		return 0
	}
	for _, f := range files {
		p := filepath.Join(root, f.path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "cinema setup: %v\n", err)
			return exitInternal
		}
		if err := os.WriteFile(p, []byte(f.body), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "cinema setup: %v\n", err)
			return exitInternal
		}
	}
	formatLikeRepo(root, task.ConfigPath, gatePath)
	fmt.Print(`
Written. Next:
  1. read .cinema/tasks.yaml, then:   cinema trust
  2. make evidence:                   cinema run --commit HEAD --all -j 4
  3. once per machine:                cinema keys init && cinema keys export
     and add that line to the repo variable CINEMA_TRUSTED_KEYS (Settings → Variables)
  4. commit both files and push with: cinema push
`)
	return 0
}

// formatLikeRepo runs the repo's own prettier on files cinema wrote, if the repo
// has it installed: a repo's format check otherwise fails on them (lab: eslint
// formats YAML with 4-space indents). Never downloads a formatter.
func formatLikeRepo(root string, files ...string) {
	bin := filepath.Join(root, "node_modules", ".bin", "prettier")
	if _, err := os.Stat(bin); err != nil {
		if b, err := os.ReadFile(filepath.Join(root, "package.json")); err == nil && strings.Contains(string(b), `"prettier"`) {
			fmt.Printf("\n  this repo formats with prettier, which isn't installed here: run `npx prettier --write %s`\n", strings.Join(files, " "))
		}
		return
	}
	cmd := exec.Command(bin, append([]string{"--write"}, files...)...)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Printf("\n  prettier failed on cinema's files (%v): %s\n", err, firstLineOf(out))
		return
	}
	fmt.Println("\n  formatted with the repo's prettier")
}

func firstLineOf(b []byte) string {
	s, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return s
}

func confirm(prompt string) bool {
	if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		fmt.Println("\nNot a terminal: rerun with --yes to write.")
		return false
	}
	fmt.Print(prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(line), "y")
}

func taskIDs(jobs []analyze.Job) string {
	ids := make([]string, 0, len(jobs))
	for _, j := range jobs {
		ids = append(ids, j.Proposal.ID)
	}
	if len(ids) > 6 {
		return strings.Join(ids[:6], ", ") + fmt.Sprintf(", … %d more", len(ids)-6)
	}
	return strings.Join(ids, ", ")
}

// selfVersion is what the gate installs: this binary's module version, or its
// commit for an untagged build, so the gate runs the cinema that wrote it.
func selfVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "latest"
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && len(s.Value) == 40 {
			return s.Value
		}
	}
	return "latest"
}
