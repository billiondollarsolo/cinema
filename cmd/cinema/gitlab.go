package main

// GitLab CI (plan §11.7): the gate's view of a GitLab pipeline, the child
// pipeline the cinema job writes, and cinema setup's edit of .gitlab-ci.yml.
//
// Skipping works through a dynamic child pipeline: setup hides each covered
// job in the parent (.cinema:<job>, a template), adds a cinema job that judges
// the receipts and writes a child pipeline of the covered jobs that still
// need to run, and a trigger job that runs that child and mirrors its result
// (strategy: depend). Proven jobs never get a runner. Everything cinema
// doesn't cover stays in the parent, byte-for-byte.

import (
	"cmp"
	"crypto/sha256"
	"fmt"
	"os"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/billiondollarsolo/cinema/internal/analyze"
	"github.com/billiondollarsolo/cinema/internal/plan"
	"github.com/billiondollarsolo/cinema/internal/task"
)

// gitlabEvent reads the pipeline from GitLab's predefined variables.
// Merge request pipelines are pull_request (merge trains merge_group), branch
// pushes push; any other source (web, api, trigger, schedule…) can carry
// pipeline variables cinema can't see, so every task runs.
func gitlabEvent(env func(string) (string, bool), root, candidate string) (ciEvent, error) {
	get := func(k string) string { v, _ := env(k); return v }
	if get("GITLAB_CI") != "true" {
		return ciEvent{}, fmt.Errorf("--gitlab-child runs in a GitLab CI job (GITLAB_CI isn't set)")
	}
	e := ciEvent{base: candidate, head: candidate, repo: strings.ToLower(get("CI_SERVER_HOST") + "/" + get("CI_PROJECT_PATH"))}
	if get("CI_SERVER_HOST") == "" || get("CI_PROJECT_PATH") == "" {
		return ciEvent{}, fmt.Errorf("CI_SERVER_HOST and CI_PROJECT_PATH aren't set")
	}
	switch src := get("CI_PIPELINE_SOURCE"); src {
	case "push":
		e.name = "push"
	case "merge_request_event":
		e.change, e.name = true, "pull_request"
		if get("CI_MERGE_REQUEST_EVENT_TYPE") == "merge_train" {
			e.name = "merge_group"
		}
		e.fork = get("CI_MERGE_REQUEST_SOURCE_PROJECT_ID") != get("CI_MERGE_REQUEST_PROJECT_ID")
		e.head = cmp.Or(get("CI_MERGE_REQUEST_SOURCE_BRANCH_SHA"), candidate) // set in merged results pipelines
		e.base = get("CI_MERGE_REQUEST_TARGET_BRANCH_SHA")
		if t := get("CI_MERGE_REQUEST_TARGET_BRANCH_NAME"); e.base == "" && t != "" {
			// A plain (detached) merge request pipeline fetches only the MR's ref.
			ref := "refs/remotes/origin/" + t
			if _, err := gitOut(root, "rev-parse", "--verify", "--quiet", ref); err != nil {
				_, _ = gitOut(root, "fetch", "--quiet", "--no-tags", "origin", "+refs/heads/"+t+":"+ref)
			}
			e.base, _ = gitOut(root, "rev-parse", "--verify", "--quiet", ref)
		}
		e.base = cmp.Or(e.base, get("CI_MERGE_REQUEST_DIFF_BASE_SHA"))
		if e.base == "" {
			return ciEvent{}, fmt.Errorf("merge request pipeline without a target branch commit")
		}
	default:
		e.name = src
		e.allRun = "pipeline source " + src + ": it can carry pipeline variables cinema can't see, so every task runs"
	}
	return e, nil
}

// writeGitLabChild decides which covered jobs still run and writes the child
// pipeline to path. Every job setup hid runs unless every one of its legs was
// reused by a graduated task drafted from exactly that leg (job_digest), as
// the checked-out config defines it.
func writeGitLabChild(cfg *task.Config, rep gateReport, root, path string) error {
	gl, err := analyze.LoadGitLab(root, nil)
	if err != nil {
		return err
	}
	reused, shadow := map[string]bool{}, map[string]bool{}
	why := map[string][]string{}
	spot := false
	if id := os.Getenv("CI_PIPELINE_ID"); id != "" && sha256.Sum256([]byte(id))[0]%20 == 0 {
		spot = true
		fmt.Println("  spot check: this pipeline skips nothing, so CI's results can be compared with cinema's")
	}
	for _, tr := range rep.Tasks {
		t := cfg.Tasks[tr.Task]
		_, job, _ := strings.Cut(t.Covers, "#")
		if tr.Decision == plan.GateReuse {
			why[job] = append(why[job], fmt.Sprintf("%s: passed on %s", tr.Task, cmp.Or(tr.Signer, "a trusted machine")))
			if t.JobDigest != "" && t.Graduates() && !spot {
				reused[t.JobDigest] = true
			} else if !t.Graduates() {
				shadow[job] = true
			}
		} else {
			why[job] = append(why[job], fmt.Sprintf("%s: %s", tr.Task, cmp.Or(tr.Reason, tr.Decision)))
		}
	}
	legs := candidateLegs(root, analyze.GitLabFile)
	var want []analyze.GitLabChildJob
	for _, name := range gl.Hidden() {
		all := len(legs[name]) > 0
		var rest []map[string]string
		matrix := false
		for _, l := range legs[name] {
			matrix = matrix || len(l.MatrixKeys) > 0
			if !reused[l.LegDigest()] {
				all = false
				rest = append(rest, legOf(l))
			}
		}
		state := "SKIP"
		switch {
		case all:
			fmt.Printf("  %-6s %s  (%s)\n", state, name, strings.Join(why[name], "; "))
			continue
		case matrix && len(rest) < len(legs[name]) && !slices.ContainsFunc(rest, func(l map[string]string) bool { return l == nil }):
			want = append(want, analyze.GitLabChildJob{Name: name, Legs: rest})
			state = fmt.Sprintf("RUN (%d of %d legs)", len(rest), len(legs[name]))
		default:
			want = append(want, analyze.GitLabChildJob{Name: name})
			state = "RUN"
		}
		if shadow[name] {
			state += " (shadow: reused, but not graduated; run cinema graduate)"
		}
		fmt.Printf("  %-6s %s  (%s)\n", state, name, cmp.Or(strings.Join(why[name], "; "), "no task covers it"))
	}
	child, err := gl.Child(os.LookupEnv, want)
	if err != nil {
		return err
	}
	for _, n := range child.Off {
		fmt.Printf("  %-6s %s  (its rules keep it out of this pipeline)\n", "OFF", n)
	}
	return os.WriteFile(path, child.YAML, 0o644)
}

// gitlabRunAll writes a child pipeline that runs every covered job, for
// when the gate can't judge (no base tasks.yaml yet, an unreadable config):
// unknown means run. Without it the covered jobs, hidden in the parent,
// wouldn't run at all.
func gitlabRunAll(path string) int {
	cwd, _ := os.Getwd()
	gl, err := analyze.LoadGitLab(cwd, nil)
	if err == nil {
		var want []analyze.GitLabChildJob
		for _, n := range gl.Hidden() {
			want = append(want, analyze.GitLabChildJob{Name: n})
		}
		var child analyze.GitLabChildPipeline
		if child, err = gl.Child(os.LookupEnv, want); err == nil {
			if err = os.WriteFile(path, child.YAML, 0o644); err == nil {
				fmt.Printf("cinema couldn't judge this pipeline: every covered job runs (%s)\n", strings.Join(child.Runs, ", "))
				return 0
			}
		}
	}
	fmt.Fprintf(os.Stderr, "cinema verify: no child pipeline: %v\n", err)
	return exitUnsatisfied
}

const (
	glBegin = "# cinema: begin (added by cinema setup; cinema setup --undo removes it)"
	glEnd   = "# cinema: end"
)

// gitlabCinemaJobs is what setup appends to .gitlab-ci.yml: %[1]s first
// stage, %[2]s last stage, %[3]s tags line (or ""), %[4]s cinema's version,
// %[5]s the rules line (or "": branch and tag pipelines, as jobs without rules).
const gitlabCinemaJobs = glBegin + `
cinema:
  stage: %[1]s
  image: golang:1.26
  needs: []
%[3]s  inherit: {default: false, variables: false}
%[5]s  variables: {GIT_DEPTH: "0"}
  timeout: 10m
  script:
    - go install github.com/billiondollarsolo/cinema/cmd/cinema@%[4]s
    - export PATH="$(go env GOPATH)/bin:$PATH"
    - cinema verify --gitlab-child .cinema-child.yml
  artifacts:
    paths: [.cinema-child.yml]
    expire_in: 1 week
cinema:child:
  stage: %[2]s
  needs: [cinema]
%[5]s  trigger:
    include:
      - artifact: .cinema-child.yml
        job: cinema
    strategy: depend
    forward: {yaml_variables: false, pipeline_variables: true}
` + glEnd + "\n"

// planGitLab edits .gitlab-ci.yml (src) so the ready jobs move into the
// child pipeline: each is hidden (.cinema:<job>), and the cinema and
// cinema:child jobs are appended. Nothing else changes.
func planGitLab(root string, src []byte, all, ready []analyze.Job, version string) (skipPlan, error) {
	p := skipPlan{file: analyze.GitLabFile}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return p, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return p, fmt.Errorf("%s: not a mapping", analyze.GitLabFile)
	}
	top := doc.Content[0]
	for _, k := range []string{"cinema", "cinema:child"} {
		if mapValue(top, k) != nil {
			return p, fmt.Errorf("%s already has a job named %s", analyze.GitLabFile, k)
		}
	}
	gl, err := analyze.LoadGitLab(root, src)
	if err != nil {
		return p, err
	}
	if why := gl.Unreadable(); why != "" {
		return p, fmt.Errorf("cinema can't see the whole pipeline (%s)", why)
	}
	deps := gl.Dependents()
	byID := map[string][]analyze.Job{}
	for _, j := range ready {
		if j.Workflow == analyze.GitLabFile {
			byID[j.ID] = append(byID[j.ID], j)
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var tags []string
	tagsSet := false
	lines := strings.SplitAfter(string(src), "\n")
	for _, id := range ids {
		key := mapKey(top, id)
		job := mapValue(top, id)
		switch {
		case key == nil:
			p.left = append(p.left, id+": defined in an included file (setup edits "+analyze.GitLabFile+" only)")
			continue
		case len(deps[id]) > 0:
			p.left = append(p.left, id+": "+strings.Join(deps[id], ", ")+" use it by name (extends, needs, dependencies or !reference)")
			continue
		case mapValue(top, analyze.HiddenPrefix+id) != nil:
			p.left = append(p.left, id+": "+analyze.HiddenPrefix+id+" exists already")
			continue
		case job == nil || job.Kind != yaml.MappingNode:
			p.left = append(p.left, id+": written in a form cinema doesn't edit")
			continue
		case key.Kind != yaml.ScalarNode || key.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0:
			p.left = append(p.left, id+": its key is written in a form cinema doesn't edit")
			continue
		}
		if n := mapValue(job, "needs"); n != nil && len(n.Content) > 0 {
			p.left = append(p.left, id+": it needs other jobs (a child pipeline can't wait for the parent's)")
			continue
		}
		if n := mapValue(job, "dependencies"); n != nil && len(n.Content) > 0 {
			p.left = append(p.left, id+": it uses other jobs' artifacts (dependencies:)")
			continue
		}
		t := gl.Tags(id)
		if !tagsSet {
			tags, tagsSet = t, true
		} else if !slices.Equal(t, tags) {
			p.left = append(p.left, fmt.Sprintf("%s: runs on runners tagged %v, the cinema job on %v (it measures its own runner)", id, t, tags))
			continue
		}
		line := lines[key.Line-1]
		col := key.Column - 1
		if key.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) != 0 {
			col++ // inside the quote
		}
		if col > len(line) || (key.Style == 0 && !strings.HasPrefix(line[col:], id)) {
			p.left = append(p.left, id+": its key is written in a form cinema doesn't edit")
			continue
		}
		lines[key.Line-1] = line[:col] + analyze.HiddenPrefix + line[col:]
		p.covered = append(p.covered, id)
		p.skippable = append(p.skippable, id)
	}
	if len(p.covered) == 0 {
		return p, nil
	}
	stages := gl.Stages()
	first, last := "test", "test"
	if len(stages) > 0 {
		first, last = stages[0], stages[len(stages)-1]
	}
	tagLine := ""
	if len(tags) > 0 {
		var q []string
		for _, t := range tags {
			q = append(q, quoted(t))
		}
		tagLine = "  tags: [" + strings.Join(q, ", ") + "]\n"
	}
	out := strings.Join(lines, "")
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	rules := "" // like the project's jobs without rules: branch and tag pipelines
	if gl.MergeRequestPipelines() {
		rules = "  rules: [{when: on_success}]\n" // in every pipeline the project has
	}
	out += "\n" + fmt.Sprintf(gitlabCinemaJobs, quoted(first), quoted(last), tagLine, version, rules)
	p.out = []byte(out)

	// Safety net: read it back. Each covered job must be hidden, and analyze
	// exactly as before (same job digest), or the file is left as it was.
	before := map[string]string{}
	for _, j := range all {
		if j.Workflow == analyze.GitLabFile {
			before[j.Name] = j.LegDigest()
		}
	}
	after, err := analyze.GitLab(root, p.out)
	if err != nil {
		return skipPlan{file: p.file}, fmt.Errorf("the edit didn't parse (%v); left as is", err)
	}
	seen := 0
	for _, j := range after {
		if d, ok := before[j.Name]; ok {
			seen++
			if d != j.LegDigest() {
				return skipPlan{file: p.file}, fmt.Errorf("job %s reads differently after the edit; left as is", j.Name)
			}
		}
	}
	gl2, err := analyze.LoadGitLab(root, p.out)
	if err != nil || seen != len(before) || !sameSet(gl2.Hidden(), p.covered) {
		return skipPlan{file: p.file}, fmt.Errorf("the edit didn't come out as intended; left as is")
	}
	return p, nil
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// unsetupGitLab undoes planGitLab: the appended block goes, hidden jobs get
// their names back. A file setup never touched comes back unchanged.
func unsetupGitLab(src []byte) []byte {
	s := string(src)
	b := strings.Index(s, "\n"+glBegin+"\n")
	e := strings.Index(s, "\n"+glEnd+"\n")
	if b < 0 || e < b {
		return src
	}
	start := b
	if start > 0 && s[start-1] == '\n' {
		start-- // the blank line setup put before it
	}
	s = s[:start+1] + s[e+len(glEnd)+2:]
	var doc yaml.Node
	if yaml.Unmarshal([]byte(s), &doc) != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return []byte(s)
	}
	top := doc.Content[0]
	lines := strings.SplitAfter(s, "\n")
	for i := 0; i+1 < len(top.Content); i += 2 {
		k := top.Content[i]
		name, ok := strings.CutPrefix(k.Value, analyze.HiddenPrefix)
		if !ok || mapValue(top, name) != nil {
			continue
		}
		col := k.Column - 1
		if k.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) != 0 {
			col++
		}
		if l := lines[k.Line-1]; strings.HasPrefix(l[col:], analyze.HiddenPrefix) {
			lines[k.Line-1] = l[:col] + l[col+len(analyze.HiddenPrefix):]
		}
	}
	return []byte(strings.Join(lines, ""))
}
