package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"sort"
	"strings"

	"github.com/billiondollarsolo/cinema/internal/analyze"
	"github.com/billiondollarsolo/cinema/internal/snapshot"
	"github.com/billiondollarsolo/cinema/internal/task"
)

// cmdSetup is the guided path from existing CI to a working cinema setup: what
// this machine can take over, the files that does it, formatted the repo's way,
// written only after a yes. Nothing else changes (no keys, no pushes).
func cmdSetup(args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	withReview := fs.Bool("include-review", false, "also draft jobs with open issues (marked REVIEW): check each before relying on it")
	yes := fs.Bool("yes", false, "write without asking")
	withMinutes := fs.Bool("minutes", false, "price the jobs that will skip themselves from the repo's recent PR runs (gh CLI)")
	undo := fs.Bool("undo", false, "remove what setup added to the workflows (tasks.yaml stays)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: cinema setup [--include-review] [--minutes] [--yes] | --undo [--yes]")
		return exitBadArgs
	}
	cwd, _ := os.Getwd()
	root, err := snapshot.RepoRoot(cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cinema setup: %v\n", err)
		return exitBadArgs
	}
	if *undo {
		return setupUndo(root, *yes)
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
	required, reqErr := requiredChecks()
	var keepRequired []string
	if reqErr == nil {
		picked = slices.DeleteFunc(picked, func(j analyze.Job) bool {
			if requiredLeg(j, required) {
				_, name, _ := strings.Cut(analyzedKey(j), "\x00")
				keepRequired = append(keepRequired, name)
				return true
			}
			return false
		})
	}
	// Skip-in-place: each workflow with ready jobs gets a cinema job, and those
	// jobs skip themselves when their tasks are reused. Only tasks whose job can
	// skip are written: evidence for a job that never skips saves nothing.
	version := selfVersion()
	byWF := map[string]bool{}
	for _, j := range picked {
		byWF[j.Workflow] = true
	}
	var mins map[string]float64
	if *withMinutes {
		if mins, err = jobMinutes(30); err != nil {
			fmt.Fprintf(os.Stderr, "cinema setup: minutes: %v\n", err)
		}
	}
	price := func(wf string, ids []string) (float64, bool) { // median billed minutes of these jobs' legs
		total, any := 0.0, false
		for _, j := range jobs {
			if j.Workflow == wf && slices.Contains(ids, j.ID) {
				if v, ok := mins[analyzedKey(j)]; ok {
					total, any = total+v, true
				}
			}
		}
		return total, any
	}
	saved := 0.0
	type file struct{ path, body string }
	var files []file
	covered := map[string]bool{} // workflow#job
	var summary []string
	for _, wf := range sortedSet(byWF) {
		src, err := os.ReadFile(filepath.Join(root, ".github", "workflows", wf))
		if err != nil {
			fmt.Fprintf(os.Stderr, "cinema setup: %v\n", err)
			return exitInternal
		}
		p, err := planSkips(wf, unsetup(src), jobs, picked, version) // a rerun starts from the original
		if err != nil {
			summary = append(summary, fmt.Sprintf("  %s: left as is (%v)", wf, err))
			continue
		}
		if len(p.covered) > 0 {
			files = append(files, file{p.file, string(p.out)})
			var parts []string
			if n := len(p.skippable); n > 0 {
				parts = append(parts, strings.Join(p.skippable, ", ")+map[bool]string{true: " skips itself", false: " skip themselves"}[n == 1])
			}
			var dropping []string
			for _, id := range p.covered {
				if !slices.Contains(p.skippable, id) {
					dropping = append(dropping, id)
				}
			}
			if len(dropping) > 0 {
				parts = append(parts, strings.Join(dropping, ", ")+" drop"+map[bool]string{true: "s its", false: " their"}[len(dropping) == 1]+" reused matrix legs")
			}
			line := fmt.Sprintf("  %s: %s when cinema reused their tasks", wf, strings.Join(parts, "; "))
			if m, ok := price(wf, p.skippable); ok {
				line += fmt.Sprintf(" (~%.0f billed min/PR for the whole jobs)", m)
				saved += m
			}
			summary = append(summary, line)
			for _, id := range p.covered {
				covered[wf+"#"+id] = true
			}
		}
		for _, l := range p.left {
			summary = append(summary, "    left as is: "+l)
		}
	}
	var kept []analyze.Job
	for _, j := range picked {
		if covered[j.Workflow+"#"+j.ID] {
			kept = append(kept, j)
		}
	}
	fmt.Println("\nWhat changes in CI:")
	for _, l := range summary {
		fmt.Println(l)
	}
	for _, n := range keepRequired {
		fmt.Printf("  %s: stays in CI, branch protection requires it by name (a skipped or excluded leg never reports)\n", n)
	}
	for _, l := range summary {
		if reqErr != nil && strings.Contains(l, "reused matrix legs") {
			fmt.Printf("  note: couldn't read branch protection (%v).\n"+
				"        An excluded matrix leg reports no check; if branch protection requires a leg by name\n"+
				"        (e.g. \"build (ubuntu-latest)\"), require the job's other checks instead\n", reqErr)
			break
		}
	}
	if saved > 0 {
		fmt.Printf("  about %.0f billed min/PR saved when everything is reused, minus ~1 min per cinema job\n", saved)
	}
	if len(kept) == 0 {
		fmt.Println("\nNo job can skip itself yet (see why above); nothing written.")
		return exitUnsatisfied
	}
	tasks, err := draft(kept)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cinema setup: %v\n", err)
		return exitInternal
	}
	files = append([]file{{task.ConfigPath, string(tasks)}}, files...)
	fmt.Printf("\ncinema setup would write %d task%s (%s) and edit:\n", len(kept), map[bool]string{true: "", false: "s"}[len(kept) == 1], taskIDs(kept))
	for _, f := range files {
		state := "new"
		if _, err := os.Stat(filepath.Join(root, f.path)); err == nil {
			state = "edited"
		}
		fmt.Printf("  %s (%s)\n", f.path, state)
	}
	if strings.Contains(version, "latest") {
		fmt.Println("\n  note: this cinema isn't a tagged build, so CI installs @latest; pin a version before relying on it")
	}
	if !*yes && !confirm("\nWrite these files? [y/N] ") {
		fmt.Println("Nothing written.")
		return 0
	}
	paths := make([]string, 0, len(files))
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
		paths = append(paths, f.path)
	}
	formatLikeRepo(root, paths...)
	fmt.Print(`
Written (see git diff). Next:
  1. read .cinema/tasks.yaml, then:   cinema trust
  2. make evidence:                   cinema run --commit HEAD --all -j 4
  3. once per machine:                cinema keys init && cinema keys export
     and add that line to the repo variable CINEMA_TRUSTED_KEYS (Settings → Variables)
  4. commit, then push with:          cinema push
The PR that adds this runs everything as before; jobs start skipping once it's merged.
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

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// setupUndo puts every workflow back the way it was before setup.
func setupUndo(root string, yes bool) int {
	files, _ := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.y*ml"))
	type change struct {
		path string
		body []byte
	}
	var changes []change
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if out := unsetup(src); string(out) != string(src) {
			changes = append(changes, change{f, out})
		}
	}
	if len(changes) == 0 {
		fmt.Println("No workflow has cinema's edits; nothing to undo.")
		return 0
	}
	fmt.Println("cinema setup --undo would restore:")
	for _, c := range changes {
		rel, _ := filepath.Rel(root, c.path)
		fmt.Println("  " + rel)
	}
	if !yes && !confirm("\nRestore these files? [y/N] ") {
		fmt.Println("Nothing written.")
		return 0
	}
	for _, c := range changes {
		if err := os.WriteFile(c.path, c.body, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "cinema setup: %v\n", err)
			return exitInternal
		}
	}
	fmt.Println("Restored (see git diff). .cinema/tasks.yaml is still there; delete it if you're done with cinema.")
	return 0
}
