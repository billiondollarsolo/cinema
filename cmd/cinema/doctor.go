package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
)

// What a failed run tried to use that this machine doesn't have. Read from the
// run's output, not guessed from its argv: most CI scripts reach tools through
// make or a package script (lab: traefik's shellcheck, eslint's Xvfb).
var missingRe = []*regexp.Regexp{
	regexp.MustCompile(`([\w.+-]+): command not found`),                              // bash
	regexp.MustCompile(`: \d+: ([\w.+-]+): not found`),                               // dash: "sh: 1: yq: not found"
	regexp.MustCompile(`spawn ([\w.+-]+) ENOENT`),                                    // node
	regexp.MustCompile(`exec: "([\w.+-]+)": executable file not found`),              // go
	regexp.MustCompile(`"?No ([\w.+-]+) in \$?PATH`),                                 // make guards
	regexp.MustCompile(`make: ([\w.+-]+): No such file or directory`),                // make running a recipe command
	regexp.MustCompile(`error while loading shared libraries: ([\w.+-]+\.so[\w.]*)`), // ld.so
}

// missingTools lists the commands and shared libraries out says were missing,
// minus commands this machine does have (then the task's own PATH was the problem).
func missingTools(out []byte) []string {
	var found []string
	for _, re := range missingRe {
		for _, m := range re.FindAllSubmatch(out, -1) {
			name := string(m[1])
			if !strings.Contains(name, ".so") {
				if _, err := exec.LookPath(name); err == nil {
					continue
				}
			}
			if !slices.Contains(found, name) {
				found = append(found, name)
			}
		}
	}
	slices.Sort(found)
	return found
}

func missingNote(out []byte) string {
	if m := missingTools(out); len(m) > 0 {
		return "\n    not on this machine: " + strings.Join(m, ", ") + " (install on the evidence machine to run this task here)"
	}
	if bytes.Contains(out, []byte("Illegal instruction")) {
		// Lab: bun's AVX2 build under Rosetta on macOS 14.
		return "\n    illegal instruction: a binary needs CPU features this machine or its emulator lacks (Rosetta emulates AVX2 from macOS 15 on)"
	}
	return ""
}

// cmdDoctor: per task whose latest run failed, what the machine was missing.
func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return exitBadArgs
	}
	useConfig(*cfgPath)
	r, code := loadForReport("doctor", false)
	defer r.close()
	if code != 0 {
		return code
	}
	code = 0
	for _, id := range sortedIDs(r.cfg) {
		runs, err := r.st.TaskRuns(r.id, id, 1)
		if err != nil || len(runs) == 0 || runs[0].State == "passed" {
			continue
		}
		_, stdout, stderr, err := r.st.Run(r.id, runs[0].ID)
		if err != nil {
			continue
		}
		if m := missingTools(append(stdout, stderr...)); len(m) > 0 {
			fmt.Printf("%s: %s\n", id, strings.Join(m, ", "))
			code = exitUnsatisfied
		}
	}
	if code != 0 {
		fmt.Fprintln(os.Stderr, "install these on this machine (the GitHub runner image has most of them) to run those tasks here instead of in CI")
	}
	return code
}
