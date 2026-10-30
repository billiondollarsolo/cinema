package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/billiondollarsolo/cinema/internal/plan"
)

const hookMarker = "# cinema pre-push hook (cinema hook uninstall removes it)"

// cmdHook installs a pre-push hook that warns, and never blocks, when a push
// goes out without evidence for the jobs cinema covers (plan §8.1): CI will
// then run them. cinema push itself sets CINEMA_PUSHING, so it isn't warned.
func cmdHook(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: cinema hook install | uninstall | check")
		return exitBadArgs
	}
	if args[0] == "check" {
		return hookCheck()
	}
	cwd, _ := os.Getwd()
	hooks, err := gitOut(cwd, "rev-parse", "--git-path", "hooks")
	if err != nil {
		fmt.Fprintf(os.Stderr, "cinema hook: %v\n", err)
		return exitBadArgs
	}
	if !filepath.IsAbs(hooks) {
		hooks = filepath.Join(cwd, hooks)
	}
	path := filepath.Join(hooks, "pre-push")
	cur, err := os.ReadFile(path)
	ours := err == nil && strings.Contains(string(cur), hookMarker)
	switch args[0] {
	case "install":
		if err == nil && !ours {
			fmt.Printf("%s already exists and isn't cinema's; add this line to it:\n  cinema hook check || true\n", path)
			return exitUnsatisfied
		}
		if err := os.MkdirAll(hooks, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "cinema hook: %v\n", err)
			return exitInternal
		}
		body := "#!/bin/sh\n" + hookMarker + "\ncommand -v cinema >/dev/null 2>&1 && cinema hook check || true\n"
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "cinema hook: %v\n", err)
			return exitInternal
		}
		fmt.Println("installed " + path + ": a plain git push now warns when CI will run jobs cinema could have skipped")
	case "uninstall":
		if !ours {
			fmt.Println("no cinema pre-push hook installed")
			return 0
		}
		if err := os.Remove(path); err != nil {
			fmt.Fprintf(os.Stderr, "cinema hook: %v\n", err)
			return exitInternal
		}
		fmt.Println("removed " + path)
	default:
		fmt.Fprintln(os.Stderr, "usage: cinema hook install | uninstall | check")
		return exitBadArgs
	}
	return 0
}

// hookCheck prints one warning if covered jobs have no evidence for HEAD. It
// never fails the push.
func hookCheck() int {
	if os.Getenv("CINEMA_PUSHING") != "" {
		return 0
	}
	defer stopProbeSandboxes()
	r, err := openRepo()
	if err != nil || r.cfg == nil {
		return 0
	}
	defer r.close()
	var missing []string
	for _, id := range sortedIDs(r.cfg) {
		t := r.cfg.Tasks[id]
		if t.Covers == "" || !t.Reuse.Allowed {
			continue
		}
		if ts, _, _ := decide(r, t); ts.Local && ts.State != plan.Reusable {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "cinema: %d job(s) cinema covers have no evidence for this commit, so CI will run them (%s).\n"+
			"        To have CI skip them, push with: cinema push --run\n", len(missing), strings.Join(missing, ", "))
	}
	return 0
}
