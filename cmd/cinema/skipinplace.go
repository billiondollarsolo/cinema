package main

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/billiondollarsolo/cinema/internal/analyze"
)

// Skip-in-place: instead of a separate gate beside the project's CI, each
// workflow with covered jobs gets a small cinema job, and each covered job skips
// itself when cinema reused its tasks. The edit is textual (positions from the
// YAML parse), so everything else in the file stays byte-for-byte as it was.

var statusCall = regexp.MustCompile(`\b(success|failure|always|cancelled)\(\)`)

type skipPlan struct {
	file      string   // .github/workflows/<name>
	covered   []string // job ids that skip themselves, or their reused legs, when reused
	skippable []string // covered jobs that skip whole (every leg has a task)
	matrix    []string // covered matrix jobs that exclude their reused legs
	left      []string // "job: why" for ready jobs left alone
	out       []byte   // the edited workflow
}

// planSkips edits workflow src (file base name wf) so that jobs whose every
// matrix leg is in ready skip themselves when their tasks are reused.
func planSkips(wf string, src []byte, all, ready []analyze.Job, version string) (skipPlan, error) {
	p := skipPlan{file: ".github/workflows/" + wf}
	legs, readyLegs := map[string]int{}, map[string]int{}
	for _, j := range all {
		if j.Workflow == wf {
			legs[j.ID]++
		}
	}
	for _, j := range ready {
		if j.Workflow == wf {
			readyLegs[j.ID]++
		}
	}
	if len(readyLegs) == 0 {
		return p, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return p, err
	}
	if len(doc.Content) == 0 {
		return p, fmt.Errorf("%s: empty", wf)
	}
	jobs := mapValue(doc.Content[0], "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode || len(jobs.Content) == 0 {
		return p, fmt.Errorf("%s: no jobs", wf)
	}
	if mapValue(jobs, "cinema") != nil {
		return p, fmt.Errorf("%s already has a job named cinema", wf)
	}
	if triggers(doc.Content[0], "pull_request_target") {
		// Its checkout is the base branch, not the PR: receipts for the base tree
		// would skip jobs that test the PR head.
		return p, fmt.Errorf("%s runs on pull_request_target (checks out the base branch, not the PR)", wf)
	}
	needed := map[string]bool{} // jobs some other job needs: skipping them would skip their dependents
	for i := 1; i < len(jobs.Content); i += 2 {
		for _, n := range needsOf(jobs.Content[i]) {
			needed[n] = true
		}
	}
	lines := strings.SplitAfter(string(src), "\n")
	type edit struct {
		line    int      // 0-based line the edit applies at
		replace bool     // replace that line, else insert before it
		text    []string // full lines, each ending in \n
	}
	var edits []edit
	type expected struct {
		cond    string
		needs   []string
		exclude bool
	}
	want := map[string]expected{}
	ids := make([]string, 0, len(readyLegs))
	for id := range readyLegs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		key, job := mapKey(jobs, id), mapValue(jobs, id)
		partial := readyLegs[id] < legs[id]
		exAt, exIndent, whyNot := excludeSlot(job, legKeys(ready, wf, id))
		switch {
		case partial && whyNot != "":
			p.left = append(p.left, fmt.Sprintf("%s: %d of %d matrix legs are ready, and its matrix %s", id, readyLegs[id], legs[id], whyNot))
			continue
		case needed[id]:
			p.left = append(p.left, id+": other jobs need it (skipping it would skip them)")
			continue
		case job == nil || job.Kind != yaml.MappingNode || len(job.Content) == 0 || job.Line == key.Line:
			p.left = append(p.left, id+": written in a form cinema doesn't edit")
			continue
		}
		// A partly covered matrix only drops reused legs (exclude): the whole job
		// must never skip, since cinema doesn't see the legs it can't run.
		cond := "!cancelled() && needs.cinema.outputs.skip_" + id + " != 'true'"
		if partial {
			cond = "!cancelled()"
		}
		for _, n := range needsOf(job) { // what `needs` alone required: each dependency succeeded
			cond += " && needs." + n + ".result == 'success'"
		}
		indent := strings.Repeat(" ", job.Content[0].Column-1)
		var mine []edit // applied only if every check for this job passes
		var ins []string
		if ifNode := mapValue(job, "if"); ifNode != nil {
			orig := strings.TrimSpace(ifNode.Value)
			if strings.HasPrefix(orig, "${{") && strings.HasSuffix(orig, "}}") {
				orig = strings.TrimSpace(orig[3 : len(orig)-2])
			}
			if statusCall.MatchString(orig) || !oneLine(ifNode, mapKey(job, "if"), lines) {
				p.left = append(p.left, id+": its if: uses status functions or spans lines")
				continue
			}
			cond += " && (" + orig + ")"
			mine = append(mine, edit{line: ifNode.Line - 1, replace: true, text: []string{indent + "if: " + quoted("${{ "+cond+" }}") + "\n"}})
		} else {
			ins = append(ins, indent+"if: "+quoted("${{ "+cond+" }}")+"\n")
		}
		switch nd := mapValue(job, "needs"); {
		case nd == nil:
			ins = append([]string{indent + "needs: cinema\n"}, ins...)
		case nd.Kind == yaml.ScalarNode || (nd.Kind == yaml.SequenceNode && nd.Style&yaml.FlowStyle != 0):
			if !oneLine(nd, mapKey(job, "needs"), lines) {
				p.left = append(p.left, id+": its needs: is written in a form cinema doesn't edit")
				continue
			}
			mine = append(mine, edit{line: nd.Line - 1, replace: true, text: []string{indent + "needs: [cinema, " + strings.Join(needsOf(job), ", ") + "]\n"}})
		case nd.Kind == yaml.SequenceNode && len(nd.Content) > 0:
			mine = append(mine, edit{line: nd.Content[0].Line - 1, text: []string{strings.Repeat(" ", nd.Content[0].Column-3) + "- cinema\n"}})
		default:
			p.left = append(p.left, id+": its needs: is written in a form cinema doesn't edit")
			continue
		}
		if len(ins) > 0 {
			mine = append(mine, edit{line: job.Content[0].Line - 1, text: ins})
		}
		if exAt > 0 {
			mine = append(mine, edit{line: exAt - 1, text: []string{exIndent + "exclude: ${{ fromJSON(needs.cinema.outputs.exclude_" + id + " || '[]') }}\n"}})
			p.matrix = append(p.matrix, id)
		}
		edits = append(edits, mine...)
		want[id] = expected{cond: "${{ " + cond + " }}", needs: append([]string{"cinema"}, needsOf(job)...), exclude: exAt > 0}
		if !partial {
			p.skippable = append(p.skippable, id)
		}
		p.covered = append(p.covered, id)
	}
	if len(p.covered) == 0 {
		return p, nil
	}
	// The cinema job goes first under jobs:, indented like the other jobs.
	ji := strings.Repeat(" ", jobs.Content[0].Column-1)
	step := ji + "  "
	block := []string{ji + "cinema:\n", step + "runs-on: ubuntu-latest\n", step + "timeout-minutes: 10\n", step + "permissions: { contents: read }\n", step + "outputs:\n"}
	for _, id := range p.skippable {
		block = append(block, step+"  skip_"+id+": ${{ steps.verify.outputs.skip_"+id+" }}\n")
	}
	for _, id := range p.matrix {
		block = append(block, step+"  exclude_"+id+": ${{ steps.verify.outputs.exclude_"+id+" }}\n")
	}
	for _, l := range strings.Split(strings.TrimRight(fmt.Sprintf(cinemaJobSteps, version, wf), "\n"), "\n") {
		block = append(block, step+l+"\n")
	}
	at := jobs.Content[0].Line - 1
	if c := jobs.Content[0].HeadComment; c != "" { // keep the first job's comment with it
		at -= strings.Count(c, "\n") + 1
	}
	edits = append(edits, edit{line: at, text: append(block, "\n")})
	// Bottom-up so earlier positions stay valid; on one line, replace before
	// inserting above it (an insert first would shift the line being replaced).
	sort.SliceStable(edits, func(a, b int) bool {
		if edits[a].line != edits[b].line {
			return edits[a].line > edits[b].line
		}
		return edits[a].replace && !edits[b].replace
	})
	for _, e := range edits {
		if e.replace {
			lines = append(lines[:e.line], append(e.text, lines[e.line+1:]...)...)
		} else {
			lines = append(lines[:e.line], append(e.text, lines[e.line:]...)...)
		}
	}
	p.out = []byte(strings.Join(lines, ""))
	// Safety net: read the result back; every covered job must say exactly what
	// was meant, or the workflow is left as it was.
	var back struct {
		Jobs map[string]struct {
			If       string    `yaml:"if"`
			Needs    yaml.Node `yaml:"needs"`
			Strategy struct {
				Matrix map[string]any `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(p.out, &back); err != nil {
		return skipPlan{file: p.file}, fmt.Errorf("%s: the edit didn't parse (%v); left as is", wf, err)
	}
	for id, w := range want {
		j := back.Jobs[id]
		_, hasEx := j.Strategy.Matrix["exclude"]
		var needs []string
		if j.Needs.Kind == yaml.ScalarNode {
			needs = []string{j.Needs.Value}
		}
		for _, c := range j.Needs.Content {
			needs = append(needs, c.Value)
		}
		if j.If != w.cond || strings.Join(needs, ",") != strings.Join(w.needs, ",") || hasEx != w.exclude {
			return skipPlan{file: p.file}, fmt.Errorf("%s: job %s didn't come out as intended; left as is", wf, id)
		}
	}
	return p, nil
}

// quoted is s as a double-quoted YAML scalar (an if: may hold ": " or " #").
func quoted(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// oneLine reports whether value v of key k sits wholly on k's line: not a
// block scalar, and nothing of it continues on the next line.
func oneLine(v, k *yaml.Node, lines []string) bool {
	if v.Line != k.Line || v.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return false
	}
	line := strings.TrimRight(lines[v.Line-1], "\r\n")
	switch {
	case v.Kind == yaml.SequenceNode:
		return strings.Contains(line, "]")
	case v.Style&yaml.DoubleQuotedStyle != 0, v.Style&yaml.SingleQuotedStyle != 0:
		q := `"`
		if v.Style&yaml.SingleQuotedStyle != 0 {
			q = "'"
		}
		return strings.Count(line, q) >= 2
	}
	// A plain scalar continues if the next line is indented deeper and isn't a key.
	if v.Line < len(lines) {
		next := lines[v.Line]
		trimmed := strings.TrimLeft(next, " ")
		deeper := len(next)-len(trimmed) > k.Column-1
		if deeper && strings.TrimSpace(trimmed) != "" && !strings.HasPrefix(trimmed, "#") {
			return false
		}
	}
	return true
}

// triggers reports whether a workflow's on: names event ev.
func triggers(w *yaml.Node, ev string) bool {
	on := mapValue(w, "on")
	if on == nil {
		return false
	}
	switch on.Kind {
	case yaml.ScalarNode:
		return on.Value == ev
	case yaml.SequenceNode:
		for _, c := range on.Content {
			if c.Value == ev {
				return true
			}
		}
	case yaml.MappingNode:
		return mapValue(on, ev) != nil
	}
	return false
}

const cinemaJobSteps = `steps:
  - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
    with: { fetch-depth: 0, persist-credentials: false }
  - name: install cinema
    run: |
      go install github.com/billiondollarsolo/cinema/cmd/cinema@%s
      sudo mv "$(go env GOPATH)/bin/cinema" /usr/local/bin/cinema
  - id: verify
    name: skip jobs that already passed (signed cinema receipts)
    env:
      CINEMA_TRUSTED_KEYS: "${{ vars.CINEMA_TRUSTED_KEYS }}"
      CINEMA_FETCH_TOKEN: "${{ github.token }}"
    run: cinema verify --event "$GITHUB_EVENT_PATH" --skip-outputs %s
`

func mapKey(m *yaml.Node, k string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == k {
			return m.Content[i]
		}
	}
	return nil
}

func mapValue(m *yaml.Node, k string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == k {
			return m.Content[i+1]
		}
	}
	return nil
}

func needsOf(job *yaml.Node) []string {
	if job.Kind != yaml.MappingNode {
		return nil
	}
	n := mapValue(job, "needs")
	switch {
	case n == nil:
		return nil
	case n.Kind == yaml.ScalarNode:
		return []string{n.Value}
	}
	var out []string
	for _, c := range n.Content {
		out = append(out, c.Value)
	}
	return out
}

// legKeys is whether every ready leg of job id has a complete leg identity,
// and the matrix keys it uses.
func legKeys(ready []analyze.Job, wf, id string) []string {
	var keys []string
	for _, j := range ready {
		if j.Workflow == wf && j.ID == id {
			if legOf(j) == nil {
				return nil
			}
			keys = j.MatrixKeys
		}
	}
	return keys
}

// excludeSlot is where (1-based line) and how indented an exclude: line can go
// in job's matrix, or why not. Exclude matching is exact by value, so only a
// block matrix of plain string values, without its own exclude, qualifies;
// anything else stays as written (its reused legs just run).
func excludeSlot(job *yaml.Node, keys []string) (line int, indent, why string) {
	if len(keys) == 0 {
		return 0, "", "has no leg identity cinema can match (object values, or no matrix)"
	}
	strat := mapValue(job, "strategy")
	if strat == nil || strat.Kind != yaml.MappingNode {
		return 0, "", "isn't a strategy cinema can edit"
	}
	m := mapValue(strat, "matrix")
	switch {
	case m == nil || m.Kind != yaml.MappingNode || m.Style&yaml.FlowStyle != 0 || len(m.Content) == 0:
		return 0, "", "is written on one line or as an expression"
	case mapValue(m, "exclude") != nil:
		return 0, "", "already has an exclude:"
	case extendsBaseLeg(m, keys):
		// GitHub applies exclude before include: an include entry that extends a
		// leg (agrees with it on every shared key) would, with that leg excluded,
		// become a new, possibly incomplete leg. Entries that differ from every
		// leg already stand alone, and are fine (lab: eslint's test matrix).
		return 0, "", "has an include: entry that extends one of its legs"
	}
	for _, k := range keys {
		v := mapValue(m, k)
		if v == nil {
			return 0, "", "has a key cinema can't find (" + k + ")"
		}
		vals := []*yaml.Node{v}
		if v.Kind == yaml.SequenceNode {
			vals = v.Content
		}
		for _, x := range vals {
			if x.Kind != yaml.ScalarNode || x.Tag != "!!str" || strings.Contains(x.Value, "${{") {
				return 0, "", "has values that aren't plain strings (" + k + ")"
			}
		}
	}
	first := m.Content[0]
	return first.Line, strings.Repeat(" ", first.Column-1), ""
}

// extendsBaseLeg reports whether some include: entry of matrix m agrees with
// some combination of its own keys' values on every key they share. An entry
// cinema can't read counts as extending (fail closed).
func extendsBaseLeg(m *yaml.Node, keys []string) bool {
	inc := mapValue(m, "include")
	if inc == nil {
		return false
	}
	var entries []map[string]any
	if inc.Decode(&entries) != nil {
		return true
	}
	values := map[string][]string{}
	for _, k := range keys {
		var vs []string
		if v := mapValue(m, k); v != nil && v.Kind == yaml.SequenceNode {
			for _, x := range v.Content {
				vs = append(vs, x.Value)
			}
		} else if v != nil {
			vs = []string{v.Value}
		}
		values[k] = vs
	}
	for _, e := range entries {
		compatible := true // with some base leg: every shared key has a matching value
		for k, ev := range e {
			vs, shared := values[k]
			if !shared {
				continue
			}
			s, ok := ev.(string)
			if !ok {
				return true
			}
			compatible = compatible && slices.Contains(vs, s)
		}
		if compatible {
			return true
		}
	}
	return false
}
