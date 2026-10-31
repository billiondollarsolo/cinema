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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
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
//
// Pipeline variables can override predefined ones too, so none of these may
// be what lets a job skip: the trusted keys come from a reviewed file
// (gitlabTrust), a merge request's base must be on that file's branch
// (cmdVerify), and project IDs that are missing or disagree mean a fork.
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
		src, dst := get("CI_MERGE_REQUEST_SOURCE_PROJECT_ID"), get("CI_MERGE_REQUEST_PROJECT_ID")
		e.fork = src == "" || src != dst || dst != get("CI_PROJECT_ID")       // a fork, or can't tell
		e.head = cmp.Or(get("CI_MERGE_REQUEST_SOURCE_BRANCH_SHA"), candidate) // set in merged results pipelines
		e.base = get("CI_MERGE_REQUEST_TARGET_BRANCH_SHA")
		if t := get("CI_MERGE_REQUEST_TARGET_BRANCH_NAME"); e.base == "" && gitBranch.MatchString(t) {
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
		for _, c := range []string{e.base, e.head} {
			if !commitID.MatchString(c) { // they reach git's command line
				return ciEvent{}, fmt.Errorf("merge request pipeline: %q isn't a commit ID", c)
			}
		}
	default:
		e.name = src
		e.allRun = "pipeline source " + src + ": it can carry pipeline variables cinema can't see, so every task runs"
	}
	return e, nil
}

var commitID = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// GitLabTrustFile holds the trusted keys on GitLab: one `cinema keys export`
// line per machine, on the branch setup wrote into the cinema job.
const GitLabTrustFile = ".cinema/trusted-keys"

// gitBranch is a branch name that goes into the cinema job's script and a
// refspec: plain characters only.
var gitBranch = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]*$`)

// gitlabTrust reads the trusted keys from GitLabTrustFile at the tip of
// branch on remote, fetched now, and returns that tip. Not from the job's
// environment: pipeline variables (git push -o ci.variable=…, Run pipeline,
// the API) outrank project variables, so a Developer could pass their own
// CINEMA_TRUSTED_KEYS, unseen in any diff, and sign their own receipts. The
// branch is the one setup wrote into the cinema job, not $CI_DEFAULT_BRANCH
// (a pipeline variable overrides that too): moving it is an edit of
// .gitlab-ci.yml, and adding a key is a change merged into that branch, both
// reviewed like code.
func gitlabTrust(root, remote, branch string) (keys, tip string, err error) {
	if branch == "" {
		return "", "", fmt.Errorf("the cinema job doesn't name the branch that holds %s (--trust-branch): run cinema setup --undo, then cinema setup", GitLabTrustFile)
	}
	if !gitBranch.MatchString(branch) || strings.Contains(branch, "..") {
		return "", "", fmt.Errorf("--trust-branch %q isn't a plain branch name", branch)
	}
	// The branch is named in the candidate's own .gitlab-ci.yml: it must be the
	// project's default branch, as the server says (not an env variable).
	head, err := trustGit(root, "ls-remote", "--symref", remote, "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("the remote's default branch: %v", err)
	}
	if def, _, _ := strings.Cut(strings.TrimPrefix(head, "ref: refs/heads/"), "\t"); def != branch {
		return "", "", fmt.Errorf("--trust-branch %s isn't the project's default branch (%s)", branch, def)
	}
	if _, err := trustGit(root, "fetch", "--quiet", "--no-tags", remote, "refs/heads/"+branch); err != nil {
		return "", "", err
	}
	if tip, err = trustGit(root, "rev-parse", "--verify", "--quiet", "FETCH_HEAD^{commit}"); err != nil {
		return "", "", err
	}
	if keys, err = trustGit(root, "show", tip+":"+GitLabTrustFile); err != nil {
		return "", "", fmt.Errorf("no %s on %s", GitLabTrustFile, branch)
	}
	return keys, tip, nil
}

// jobEnv reads the job's variables as GitLab set them (scrubGitEnv keeps a
// copy from before it changed the process's environment).
var jobEnv = os.LookupEnv

// scrubGitEnv removes, for this whole process, what lets the job's variables
// steer git (pipeline variables reach the environment): every GIT_* variable
// (GIT_DIR, GIT_OBJECT_DIRECTORY, GIT_CONFIG_*, GIT_SSH_COMMAND, ...) and
// global or system config (HOME's or XDG_CONFIG_HOME's .gitconfig). Every git
// call the gate makes (the source fingerprint, receipts, tasks.yaml) then sees
// the checkout's own config only, which the runner wrote before the job ran.
//
// The job's variables as they were (GIT_* included) stay readable through
// jobEnv: GitLab evaluates rules: and the event against all of them, and so
// must the gate, or a rule reading $GIT_… could drop a job GitLab would run.
func scrubGitEnv() {
	snap := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		snap[k] = v
	}
	jobEnv = func(k string) (string, bool) { v, ok := snap[k]; return v, ok }
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "GIT_") || k == "XDG_CONFIG_HOME" {
			os.Unsetenv(k)
		}
	}
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Setenv("GIT_TERMINAL_PROMPT", "0")
}

// trustGit runs git for the trust root with an environment built from
// nothing: the job's variables (pipeline variables included) could otherwise
// redirect it (GIT_CONFIG_*, GIT_DIR, HOME or XDG_CONFIG_HOME to another
// global config, GIT_SSH_COMMAND, ...). Only proxies pass through: they can't
// forge HTTPS content. The checkout's own .git/config is the runner's, written
// before the job ran, and stays in effect (it holds the fetch credentials).
func trustGit(root string, args ...string) (string, error) {
	home, err := os.MkdirTemp("", "cinema-trust-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(home)
	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + home, "LANG=C", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0"}
	for _, k := range []string{"http_proxy", "https_proxy", "no_proxy", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	git := ""
	for _, d := range []string{"/usr/bin", "/usr/local/bin", "/bin"} {
		if fi, err := os.Stat(d + "/git"); err == nil && fi.Mode().IsRegular() {
			git = d + "/git"
			break
		}
	}
	if git == "" {
		return "", errors.New("no git in /usr/bin, /usr/local/bin or /bin")
	}
	cmd := exec.Command(git, append([]string{"-C", root}, args...)...)
	cmd.Env = env
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
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
	if id, _ := jobEnv("CI_PIPELINE_ID"); id != "" && sha256.Sum256([]byte(id))[0]%20 == 0 {
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
	if why := gl.Unreadable(); why != "" {
		// A remote, project or template include can change any job, and no job
		// digest covers what cinema can't read: nothing skips.
		fmt.Printf("  cinema can't see the whole pipeline (%s): every covered job runs\n", why)
		clear(reused)
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
	child, err := gl.Child(jobEnv, want)
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
		if child, err = gl.Child(jobEnv, want); err == nil {
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
// %[5]s the rules line (or "": branch and tag pipelines, as jobs without rules),
// %[6]s the branch that holds GitLabTrustFile (written here, so moving it
// is a reviewed edit, never a pipeline variable).
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
    - cinema verify --gitlab-child .cinema-child.yml --trust-branch %[6]s
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
	trust := gl.DefaultBranch()
	if !gitBranch.MatchString(trust) || strings.Contains(trust, "..") {
		return p, fmt.Errorf("the default branch (%q), which holds %s, isn't known or isn't a plain name (run git remote set-head origin --auto)", trust, GitLabTrustFile)
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
	out += "\n" + fmt.Sprintf(gitlabCinemaJobs, quoted(first), quoted(last), tagLine, version, rules, trust)
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
