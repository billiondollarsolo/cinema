package main

import (
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

var (
	setupIf      = regexp.MustCompile(`^(\s*)if: "\$\{\{ !cancelled\(\)(.*) \}\}"\s*$`)
	setupExclude = regexp.MustCompile(`^\s*exclude: \$\{\{ fromJSON\(needs\.cinema\.outputs\.exclude_[A-Za-z0-9_-]+ \|\| '\[\]'\) \}\}\s*$`)
	setupNeedsIn = regexp.MustCompile(`^(\s*)needs: \[cinema, (.*)\]\s*$`)
)

// unsetup removes what setup added to a workflow: the cinema job, needs:
// cinema, the skip condition (restoring the job's own if:), and the matrix
// exclude. Everything else is left byte-for-byte. A workflow setup never
// touched comes back unchanged.
func unsetup(src []byte) []byte {
	var doc yaml.Node
	if yaml.Unmarshal(src, &doc) != nil || len(doc.Content) == 0 {
		return src
	}
	jobs := mapValue(doc.Content[0], "jobs")
	if jobs == nil || !generatedCinemaJob(mapValue(jobs, "cinema")) {
		return src // no cinema job, or the project's own: not setup's to remove
	}
	lines := strings.SplitAfter(string(src), "\n")
	drop := map[int]bool{}
	// The cinema job: from its key to the line before the next job (setup puts
	// a blank line after it).
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		if jobs.Content[i].Value != "cinema" {
			continue
		}
		end := len(lines)
		if i+2 < len(jobs.Content) {
			next := jobs.Content[i+2]
			end = next.Line - 1
			if c := next.HeadComment; c != "" {
				end -= strings.Count(c, "\n") + 1
			}
		}
		for l := jobs.Content[i].Line - 1; l < end; l++ {
			drop[l] = true
		}
	}
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		job := jobs.Content[i+1]
		if job.Kind != yaml.MappingNode || !slices.Contains(needsOf(job), "cinema") {
			continue
		}
		if k, n := mapKey(job, "needs"), mapValue(job, "needs"); n != nil {
			switch {
			case n.Kind == yaml.ScalarNode:
				drop[k.Line-1] = true
			case n.Style&yaml.FlowStyle != 0:
				if m := setupNeedsIn.FindStringSubmatch(lines[n.Line-1]); m != nil {
					// needs: x and needs: [x] both became [cinema, x]; [x] means the same.
					lines[n.Line-1] = m[1] + "needs: [" + m[2] + "]\n"
				}
			default:
				for _, c := range n.Content {
					if c.Value == "cinema" {
						drop[c.Line-1] = true
					}
				}
			}
		}
		if k := mapKey(job, "if"); k != nil {
			if m := setupIf.FindStringSubmatch(lines[k.Line-1]); m != nil {
				tail := strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(m[2])
				if orig := originalIfText(tail); orig != "" {
					lines[k.Line-1] = m[1] + "if: " + plainOrExpr(orig) + "\n"
				} else {
					drop[k.Line-1] = true
				}
			}
		}
		if s := mapValue(job, "strategy"); s != nil && s.Kind == yaml.MappingNode {
			if mx := mapValue(s, "matrix"); mx != nil && mx.Kind == yaml.MappingNode {
				if k := mapKey(mx, "exclude"); k != nil && setupExclude.MatchString(lines[k.Line-1]) {
					drop[k.Line-1] = true
				}
			}
		}
	}
	var out strings.Builder
	for i, l := range lines {
		if !drop[i] {
			out.WriteString(l)
		}
	}
	return []byte(out.String())
}

// originalIfText is the job's own condition inside setup's `!cancelled()...`
// tail: the `&& (ORIG)` part, or "" if it had none.
func originalIfText(tail string) string {
	if i := strings.Index(tail, " && ("); i >= 0 && strings.HasSuffix(tail, ")") {
		return tail[i+5 : len(tail)-1]
	}
	return ""
}

// generatedCinemaJob reports whether job is the one setup writes: it runs
// cinema verify --skip-outputs.
func generatedCinemaJob(job *yaml.Node) bool {
	if job == nil || job.Kind != yaml.MappingNode {
		return false
	}
	steps := mapValue(job, "steps")
	if steps == nil {
		return false
	}
	for _, st := range steps.Content {
		if st.Kind == yaml.MappingNode {
			if r := mapValue(st, "run"); r != nil && strings.Contains(r.Value, "cinema verify") && strings.Contains(r.Value, "--skip-outputs") {
				return true
			}
		}
	}
	return false
}

// plainOrExpr writes a restored condition so it stays valid YAML: as written if
// it's a safe plain scalar, else double-quoted.
func plainOrExpr(e string) string {
	if strings.ContainsAny(e[:1], "!&*[]{}|>%@`'\"#,?:-") || strings.Contains(e, ": ") || strings.Contains(e, " #") {
		return quoted(e)
	}
	return e
}
