package analyze

// The GitLab CI analyzer: the jobs of .gitlab-ci.yml, as the same Job and
// Proposal shapes the GitHub Actions analyzer produces, so tasks, runs,
// receipts and the gate don't care which forge a job came from (plan §11.7).
// Each job names its image: the task is a container task in that image, and
// before_script + script run in one shell, as the runner runs them.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// glShell runs the script with bash where the image has it (GitLab's runner
// does, with errexit and pipefail) and sh -e otherwise (alpine and busybox
// images have no bash).
var glShell = []string{"sh", "-c", `if command -v bash >/dev/null 2>&1; then exec bash -eo pipefail -c "$1"; fi; exec sh -ec "$1"`, "sh"}

// GitLabRepo is what the analyzer knows about the project from its checkout.
type GitLabRepo struct {
	Host, Path    string // gitlab.com, group/sub/project ("" if origin isn't known)
	DefaultBranch string // from origin/HEAD; "" if unknown
}

// gitlabRepo reads origin's URL and default branch (a var for tests). In a
// GitLab job, the pipeline's own variables say (the runner's checkout has no
// origin/HEAD), so the gate analyzes jobs exactly as the developer's machine.
var gitlabRepo = func(root string) GitLabRepo {
	var r GitLabRepo
	if os.Getenv("GITLAB_CI") == "true" {
		return GitLabRepo{Host: os.Getenv("CI_SERVER_HOST"), Path: os.Getenv("CI_PROJECT_PATH"), DefaultBranch: os.Getenv("CI_DEFAULT_BRANCH")}
	}
	if root == "" {
		return r
	}
	git := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	if u := git("remote", "get-url", "origin"); u != "" {
		if i := strings.Index(u, "@"); i >= 0 && !strings.Contains(u, "://") {
			h, p, _ := strings.Cut(u[i+1:], ":")
			u = "ssh://" + h + "/" + p
		}
		if p, err := url.Parse(u); err == nil {
			r.Host, r.Path = p.Hostname(), strings.Trim(strings.TrimSuffix(p.Path, ".git"), "/")
		}
	}
	r.DefaultBranch = strings.TrimPrefix(git("symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"), "refs/remotes/origin/")
	return r
}

// glRunner is a GitLab.com hosted runner by tag: its machine.
type glRunner struct {
	cpus, memGB int
	arch        string
}

// gitlab.com's hosted Linux runners (docs.gitlab.com/ci/runners/hosted_runners/linux/).
var glRunners = map[string]glRunner{
	"saas-linux-small-amd64":   {2, 8, "amd64"},
	"saas-linux-medium-amd64":  {4, 16, "amd64"},
	"saas-linux-large-amd64":   {8, 32, "amd64"},
	"saas-linux-xlarge-amd64":  {16, 64, "amd64"},
	"saas-linux-2xlarge-amd64": {32, 128, "amd64"},
	"saas-linux-small-arm64":   {2, 8, "arm64"},
	"saas-linux-medium-arm64":  {4, 16, "arm64"},
	"saas-linux-large-arm64":   {8, 32, "arm64"},
}

// GitLabDefaultRunner is the runner a gitlab.com job without tags gets.
const GitLabDefaultRunner = "saas-linux-small-amd64"

// GitLab analyzes root's .gitlab-ci.yml (src, if not nil). Jobs come out in
// file order, one per parallel leg.
func GitLab(root string, src []byte) ([]Job, error) {
	if src == nil {
		b, err := readRepoFile(root, GitLabFile)
		if err != nil {
			return nil, err
		}
		src = b
	}
	c, err := loadGitLab(root, src)
	if err != nil {
		return nil, err
	}
	repo := gitlabRepo(root)
	a := &glAnalysis{c: c, repo: repo}
	a.scenarios = []glScenario{
		{source: "merge_request_event", defaultBranch: repo.DefaultBranch, project: repo.vars()},
		{source: "push", defaultBranch: repo.DefaultBranch, project: repo.vars()},
	}
	if repo.DefaultBranch == "" {
		c.reviewAll = append(c.reviewAll, "the default branch isn't known (no origin/HEAD: run git remote set-head origin --auto)")
	}
	var out []Job
	for _, name := range c.order {
		out = append(out, a.job(name)...)
	}
	return out, nil
}

func (r GitLabRepo) vars() map[string]string {
	if r.Path == "" {
		return nil
	}
	ns, name := "", r.Path
	if i := strings.LastIndex(r.Path, "/"); i >= 0 {
		ns, name = r.Path[:i], r.Path[i+1:]
	}
	return map[string]string{"CI_SERVER_HOST": r.Host, "CI_PROJECT_PATH": r.Path, "CI_PROJECT_NAMESPACE": ns,
		"CI_PROJECT_NAME": name, "CI_PROJECT_ROOT_NAMESPACE": strings.Split(r.Path, "/")[0]}
}

type glAnalysis struct {
	c         *glConfig
	repo      GitLabRepo
	scenarios []glScenario
}

// glLeg is one job of a parallel: keyword (or the job itself).
type glLeg struct {
	name  string            // as GitLab shows it: "test 1/3", "test: [ruby, 3.3]"
	vars  map[string]string // matrix variables
	keys  []string          // matrix variable names, in order
	index int               // CI_NODE_INDEX (0: not parallel)
	total int
}

func (a *glAnalysis) job(name string) []Job {
	base := Job{Workflow: GitLabFile, ID: name, Name: name, Verdict: Task}
	if e, bad := a.c.jobErr[name]; bad {
		base.Verdict, base.Reasons = Review, []string{"the job can't be resolved: " + e}
		return []Job{base}
	}
	j := a.c.jobs[name]
	legs, why := glLegs(name, j)
	if why != "" {
		base.Verdict, base.Reasons = Review, []string{why}
		return []Job{base}
	}
	var out []Job
	for _, l := range legs {
		out = append(out, a.leg(name, j, l))
	}
	return out
}

func glLegs(name string, j *gmap) ([]glLeg, string) {
	p, ok := j.get("parallel")
	if !ok || p == nil {
		return []glLeg{{name: name}}, ""
	}
	if s, ok := p.(string); ok {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 200 {
			return nil, "parallel: " + s + " isn't a number of jobs (1-200)"
		}
		var out []glLeg
		for i := 1; i <= n; i++ {
			out = append(out, glLeg{name: fmt.Sprintf("%s %d/%d", name, i, n), vars: map[string]string{"CI_NODE_INDEX": strconv.Itoa(i)}, index: i, total: n})
		}
		return out, ""
	}
	pm, _ := p.(*gmap)
	mx, _ := pm.get("matrix")
	entries, ok := mx.([]any)
	if !ok {
		return nil, "parallel: is neither a number nor a matrix: list"
	}
	var out []glLeg
	for _, e := range entries {
		em, ok := e.(*gmap)
		if !ok {
			return nil, "a parallel: matrix: entry isn't a mapping"
		}
		combos := []map[string]string{{}}
		for _, k := range em.keys {
			var next []map[string]string
			vals := flatList(em.m[k])
			for _, c := range combos {
				for _, v := range vals {
					if _, isMap := v.(*gmap); isMap {
						return nil, "parallel: matrix: " + k + " has a mapping value"
					}
					m := clone(c)
					m[k] = str(v)
					next = append(next, m)
				}
			}
			combos = next
		}
		for _, c := range combos {
			var vals []string
			for _, k := range em.keys {
				vals = append(vals, c[k])
			}
			out = append(out, glLeg{name: name + ": [" + strings.Join(vals, ", ") + "]", vars: c, keys: slices.Clone(em.keys)})
		}
	}
	if len(out) > 200 {
		return nil, fmt.Sprintf("parallel: matrix: expands to %d jobs (GitLab allows 200)", len(out))
	}
	for i := range out {
		out[i].index, out[i].total = i+1, len(out)
	}
	return out, ""
}

var (
	glVarRef = regexp.MustCompile(`\$\$|\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)
	// Shell-side assignments: a script that sets a variable isn't reading the project's.
	glAssigned   = regexp.MustCompile(`(?:^|[\s;&|(])(?:export\s+|local\s+|readonly\s+|declare\s+(?:-\w+\s+)*)?([A-Za-z_][A-Za-z0-9_]*)=|\bread\s+(?:-\w+\s+)*([A-Za-z_][A-Za-z0-9_]*)|\bfor\s+([A-Za-z_][A-Za-z0-9_]*)\s+in\b|\bexport\s+([A-Za-z_][A-Za-z0-9_]*)\b`)
	glSecretName = regexp.MustCompile(`(?i)token|password|passwd|secret|private|credential|api_?key|_key$|^key$|auth`)
	glUploads    = regexp.MustCompile(`/api/v4/projects/[^\s]*/(packages|releases|registry)|\brelease-cli\b|\bglab release (create|upload)\b|--upload-file\b`)
	glDockerCLI  = regexp.MustCompile(`(^|[\s;&|(])docker (build|run|compose|buildx|login)\b`)
)

// glShellVars are names a script reads that come from the shell or the
// container, not from the project.
var glShellVars = map[string]bool{"HOME": true, "PATH": true, "PWD": true, "OLDPWD": true, "USER": true, "SHELL": true, "IFS": true,
	"HOSTNAME": true, "TERM": true, "TMPDIR": true, "LANG": true, "LC_ALL": true, "RANDOM": true, "SECONDS": true, "LINENO": true,
	"UID": true, "EUID": true, "PPID": true, "BASH_SOURCE": true, "BASH_VERSION": true, "BASHPID": true, "FUNCNAME": true,
	"PIPESTATUS": true, "REPLY": true, "OPTARG": true, "OPTIND": true, "GOPATH": true, "GOCACHE": true, "JAVA_HOME": true}

// glProvided are the predefined variables a cinema run sets (the preamble).
var glProvided = map[string]bool{"CI": true, "GITLAB_CI": true, "CI_SERVER": true, "CI_PROJECT_DIR": true, "CI_BUILDS_DIR": true,
	"CI_JOB_NAME": true, "CI_JOB_NAME_SLUG": true, "CI_JOB_STAGE": true, "CI_NODE_INDEX": true, "CI_NODE_TOTAL": true,
	"CI_CONCURRENT_ID": true, "CI_CONCURRENT_PROJECT_ID": true}

var glEventVars = regexp.MustCompile(`^(CI_PIPELINE_SOURCE|CI_MERGE_REQUEST_\w+|CI_COMMIT_BRANCH|CI_COMMIT_REF_NAME|CI_COMMIT_REF_SLUG|CI_COMMIT_REF_PROTECTED|CI_COMMIT_TAG\w*|CI_OPEN_MERGE_REQUESTS|CI_EXTERNAL_PULL_REQUEST_\w+|CI_PIPELINE_TRIGGERED)$`)
var glCommitVars = regexp.MustCompile(`^CI_COMMIT_(SHA|SHORT_SHA|BEFORE_SHA|TIMESTAMP|MESSAGE|TITLE|DESCRIPTION|AUTHOR)$`)
var glCredVars = regexp.MustCompile(`^(CI_JOB_TOKEN|CI_JOB_JWT\w*|CI_REGISTRY_PASSWORD|CI_DEPLOY_PASSWORD|CI_DEPLOY_USER|CI_REGISTRY_USER)$`)

func (a *glAnalysis) leg(name string, j *gmap, l glLeg) Job {
	r := Job{Workflow: GitLabFile, ID: name, Name: l.name, Verdict: Task, MatrixKeys: l.keys}
	if len(l.vars) > 0 {
		r.Combo = l.vars
	}
	skip := func(why string) { r.Verdict, r.Reasons = Skip, append(r.Reasons, why) }
	review := func(why string) {
		if !slices.Contains(r.Reasons, why) {
			if r.Verdict == Task {
				r.Verdict = Review
			}
			r.Reasons = append(r.Reasons, why)
		}
	}
	note := func(s string) { r.Notes = append(r.Notes, s) }
	for _, why := range a.c.reviewAll {
		review(why)
	}
	get := func(k string) any { v, _ := j.get(k); return v }

	switch {
	case get("trigger") != nil:
		skip("triggers a downstream pipeline (trigger:): the jobs of that pipeline are the checks")
		return r
	case name == "pages" || get("pages") != nil:
		skip("publishes GitLab Pages: not a check")
		return r
	case get("release") != nil:
		skip("creates a release (release:): not a check")
		return r
	case get("environment") != nil:
		skip("deploys to an environment (environment: " + glEnvName(get("environment")) + "): not a check")
		return r
	case get("id_tokens") != nil || get("identity") != nil:
		skip("requests an OIDC token (id_tokens:/identity:): publishes or deploys; not a check")
		return r
	case get("secrets") != nil:
		skip("reads secrets from a secrets manager (secrets:)")
		return r
	}
	if get("resource_group") != nil {
		review("uses a resource_group (one at a time across pipelines): usually a deploy")
	}

	// When does it run? Per scenario: merge request pipelines, feature-branch pushes.
	vars := glVarsOf(get("variables"))
	for k, v := range l.vars {
		vars.set(k, v)
	}
	_, hasRules := j.get("rules")
	_, hasOnly := j.get("only")
	_, hasExcept := j.get("except")
	if hasRules && (hasOnly || hasExcept) {
		review("uses rules: with only:/except: (GitLab rejects that)")
	}
	type outcome struct {
		d   glDecision
		sc  glScenario
		env map[string]string
	}
	var runs []outcome
	unknown := ""
	for _, sc := range a.scenarios {
		wd, d := a.decide(j, sc, vars)
		if wd.runs == triFalse {
			continue
		}
		switch d.runs {
		case triUnknown:
			unknown = cmpOr(unknown, d.why)
			fallthrough
		case triTrue:
			ev := map[string]string{}
			if wd.vars != nil {
				for _, k := range wd.vars.keys {
					ev[k] = str(wd.vars.m[k])
				}
			}
			if d.vars != nil {
				for _, k := range d.vars.keys {
					ev[k] = str(d.vars.m[k])
				}
			}
			runs = append(runs, outcome{d, sc, ev})
		}
	}
	if unknown != "" {
		review("when it runs depends on what cinema can't evaluate (" + unknown + ")")
	}
	if len(runs) == 0 {
		skip("doesn't run in merge request or branch pipelines (by its rules): a tag, schedule, default-branch or manual-pipeline job, not a check")
		return r
	}
	for _, o := range runs {
		switch o.d.when {
		case "manual":
			skip("a manual job (when: manual): it runs only when someone starts it")
			return r
		case "on_failure":
			skip("runs only when an earlier job fails (when: on_failure): not a check")
			return r
		case "delayed":
			review("a delayed job (when: delayed)")
		}
	}
	af := cmpOr(runs[0].d.allowFailure, str(get("allow_failure")))
	if af == "true" {
		note("allow_failure: true: its failure doesn't fail the pipeline")
	} else if af != "" && af != "false" {
		review("allow_failure: exit_codes: cinema counts any non-zero exit as a failure")
	}
	eventDep := false
	if len(runs) == 2 && !maps2Equal(runs[0].env, runs[1].env) {
		eventDep = true // rules set different variables for merge requests and pushes
	}
	chosen := runs[0] // merge request pipelines first: what a change is checked by
	for k, v := range chosen.env {
		vars.set(k, v)
	}

	// The runner.
	tags := []string{}
	for _, t := range flatList(get("tags")) {
		tags = append(tags, a.expandStatic(str(t), vars))
	}
	cpus, mem := 0, 0
	switch {
	case len(tags) == 0 && a.repo.Host == "gitlab.com":
		r.RunsOn = GitLabDefaultRunner
		cpus, mem = 2, 8
	case len(tags) == 0:
		r.RunsOn = "default"
		review("runs on the instance's default runner (self-managed GitLab: its machine isn't known): set cpus: and memory: in the task")
	default:
		r.RunsOn = strings.Join(tags, ",")
		lower := strings.ToLower(r.RunsOn)
		gr, saas := glRunners[strings.ToLower(tags[0])]
		switch {
		case strings.Contains(lower, "macos") || strings.Contains(lower, "windows"):
			skip("runs on " + r.RunsOn)
			return r
		case strings.Contains(lower, "gpu"):
			skip("runs on a GPU runner (" + r.RunsOn + ")")
			return r
		case saas && gr.arch == "arm64", strings.Contains(lower, "arm64"), strings.Contains(lower, "aarch64"):
			skip("runs on " + r.RunsOn + " (ARM, not linux/amd64)")
			return r
		case saas && len(tags) == 1:
			cpus, mem = gr.cpus, gr.memGB
		case strings.Contains(lower, "$"):
			review("runner tags use a variable: " + r.RunsOn)
		default:
			review("runs on runners tagged " + r.RunsOn + " (self-managed: cinema doesn't know the machine): set cpus: and memory: in the task")
		}
	}

	// The image: the job's environment.
	image := ""
	switch img := get("image").(type) {
	case nil:
		review("names no image: it runs in the runner's default image; name one (image:) so the job's environment is known")
	case string:
		image = a.expandStatic(img, vars)
	case *gmap:
		n, _ := img.get("name")
		image = a.expandStatic(str(n), vars)
		for _, k := range img.keys {
			switch k {
			case "name", "pull_policy":
			case "entrypoint":
				if ep := flatList(img.m[k]); len(ep) == 1 && str(ep[0]) == "" {
					continue // entrypoint: [""]: the script runs directly, as cinema runs it
				}
				review("overrides the image's entrypoint: cinema runs the script with the entrypoint cleared")
			case "docker":
				if dm, ok := img.m[k].(*gmap); ok {
					if p, ok := dm.get("platform"); ok && str(p) != "linux/amd64" {
						skip("runs its image for platform " + str(p))
						return r
					}
					if _, ok := dm.get("user"); ok {
						review("runs its image as another user (image: docker: user:)")
					}
				}
			default:
				review("image: " + k + ": isn't something cinema reproduces")
			}
		}
	}
	if image != "" && !glImageRef.MatchString(image) {
		review("image " + image + " isn't a literal image reference (variables cinema can't resolve?)")
		image = ""
	}

	// Services: every job variable goes to every service, as on GitLab.
	services := map[string]Service{}
	jobEnv := a.yamlVals(vars, chosen.sc, nil)
	for _, s := range flatList(get("services")) {
		svc, alias, why := a.service(s, vars, jobEnv)
		if why != "" {
			review(why)
			continue
		}
		services[alias] = svc
	}

	p := &Proposal{ID: taskID(name, l.vars), Container: image, Shell: glShell, CPUs: cpus, MemoryGB: mem}
	if len(services) > 0 {
		p.Services = services
		note("starts " + strings.Join(sortedKeys(services), ", ") + " as in CI, with the job's variables (cinema fingerprints their image digests)")
	}
	if image != "" {
		note("runs in " + image + " as in CI: cinema fingerprints the image digest")
	}

	// Variables of the job's own.
	for _, k := range vars.keys {
		switch v := strings.ToUpper(str(vars.m[k])); k {
		case "GIT_STRATEGY":
			if v == "NONE" || v == "EMPTY" {
				review("GIT_STRATEGY: " + strings.ToLower(v) + ": the job works without the repository; cinema always checks it out")
			}
		case "GIT_SUBMODULE_STRATEGY":
			if v == "NORMAL" || v == "RECURSIVE" {
				review("checks out submodules (GIT_SUBMODULE_STRATEGY: " + strings.ToLower(v) + ")")
			}
		}
	}

	// The script: before_script and script in one shell; after_script apart.
	var lines []string
	for _, k := range []string{"before_script", "script"} {
		for _, x := range flatList(get(k)) {
			if _, isMap := x.(*gmap); isMap {
				review(k + ": has an entry that isn't a command")
				continue
			}
			lines = append(lines, str(x))
		}
	}
	if get("script") == nil {
		review("has no script:")
	}
	if get("after_script") != nil {
		note("after_script isn't part of the task: on GitLab it runs apart and doesn't change the job's result")
	}
	if hooks, ok := j.get("hooks"); ok && hooks != nil {
		review("hooks: (pre_get_sources_script) run before the checkout: cinema doesn't run them")
	}
	script := strings.Join(lines, "\n")
	if m := publishes.FindString(script); m != "" && !strings.Contains(script, "--dry-run") {
		skip("publishes or releases (" + m + "): not a check")
	}
	if m := glUploads.FindString(script); m != "" {
		skip("uploads to GitLab (" + m + "): not a check")
	}
	if sudo.MatchString(script) {
		review("uses sudo (changes the machine)")
		p.Exclusive = true
	}
	if pipeToShell.MatchString(script) {
		review("pipes a downloaded script into a shell: an unpinned network input; pin a release and check its SHA-256")
	} else if floating.MatchString(script) {
		review("fetches a floating version (@latest / HEAD / master): results can change without a source change")
	}
	if glDockerCLI.MatchString(script) {
		review("runs docker in the job (Docker-in-Docker): cinema's container tasks have no Docker daemon")
	}

	// What the script reads that the job doesn't define.
	defined := map[string]bool{}
	for _, k := range vars.keys {
		defined[k] = true
	}
	for _, m := range glAssigned.FindAllStringSubmatch(script, -1) {
		for _, g := range m[1:] {
			if g != "" {
				defined[g] = true
			}
		}
	}
	var unknownVars, ciVars, secretVars []string
	readsEvent := false
	addOnce := func(list *[]string, s string) {
		if !slices.Contains(*list, s) {
			*list = append(*list, s)
		}
	}
	allRefs := script
	for _, k := range vars.keys {
		if v, ok := vars.m[k].(string); ok {
			allRefs += "\n" + v
		}
	}
	for _, m := range glVarRef.FindAllStringSubmatch(allRefs, -1) {
		n := m[1] + m[2]
		switch {
		case n == "" || defined[n] || glShellVars[n] || glProvided[n]:
		case glCredVars.MatchString(n):
			skip("uses CI credentials ($" + n + "): publishes, deploys or reads private resources")
		case n == "CI_PIPELINE_SOURCE":
			readsEvent = true
			review("reads $CI_PIPELINE_SOURCE: in cinema's child pipeline it is parent_pipeline, not the parent's")
		case glEventVars.MatchString(n):
			readsEvent = true
			addOnce(&ciVars, n)
		case glCommitVars.MatchString(n):
			addOnce(&ciVars, n)
		case strings.HasPrefix(n, "CI_") || strings.HasPrefix(n, "GITLAB_"):
			if _, ok := a.repo.vars()[n]; !ok {
				addOnce(&ciVars, n)
			}
		case glSecretName.MatchString(n):
			addOnce(&secretVars, n)
		default:
			addOnce(&unknownVars, n)
		}
	}
	if len(secretVars) > 0 {
		skip("uses a secret ($" + strings.Join(secretVars, ", $") + ": not defined in " + GitLabFile + ", so a CI/CD variable from the project's settings)")
	}
	if len(ciVars) > 0 {
		review("reads predefined variables a local run doesn't have ($" + strings.Join(ciVars, ", $") + "): the branch, merge request or commit, not just the tree")
	}
	if len(unknownVars) > 0 {
		review("reads $" + strings.Join(unknownVars, ", $") + ", which " + GitLabFile + " doesn't define (CI/CD variables from the project's settings?)")
	}
	if readsEvent {
		eventDep = true
	}

	// Artifacts from other jobs.
	needs, hasNeeds := j.get("needs")
	deps, hasDeps := j.get("dependencies")
	var fromNeeds []string
	for _, n := range flatList(needs) {
		switch x := n.(type) {
		case string:
			fromNeeds = append(fromNeeds, x)
		case *gmap:
			if _, ok := x.get("pipeline"); ok {
				review("needs artifacts from another pipeline (needs: pipeline:)")
				continue
			}
			if _, ok := x.get("project"); ok {
				review("needs artifacts from another project (needs: project:)")
				continue
			}
			jn, _ := x.get("job")
			if art, ok := x.get("artifacts"); ok && str(art) == "false" {
				continue
			}
			fromNeeds = append(fromNeeds, str(jn))
		}
	}
	if len(fromNeeds) > 0 {
		review("needs " + strings.Join(fromNeeds, ", ") + " and their artifacts: cinema doesn't rebuild another job's artifacts yet")
	}
	if hasDeps {
		if d := flatList(deps); len(d) > 0 {
			var ds []string
			for _, x := range d {
				ds = append(ds, str(x))
			}
			review("uses the artifacts of " + strings.Join(ds, ", ") + " (dependencies:)")
		}
	} else if !hasNeeds {
		if earlier := a.earlierArtifacts(j); len(earlier) > 0 {
			review("may use the artifacts of earlier-stage jobs " + strings.Join(earlier, ", ") + " (GitLab passes them by default): add dependencies: [] if it doesn't")
		}
	}

	// What it uploads: outputs.
	if art, ok := get("artifacts").(*gmap); ok {
		for _, x := range flatList(art.m["paths"]) {
			pth := strings.TrimSuffix(str(x), "/")
			if pth != "" && filepath.IsLocal(pth) && !strings.ContainsAny(pth, "*?[$") && !slices.Contains(p.Outputs, pth) {
				p.Outputs = append(p.Outputs, pth)
			}
		}
	}

	if r.Verdict == Skip {
		return r
	}
	if len(lines) == 0 {
		r.Reasons = append(r.Reasons, "no script lines")
		if r.Verdict == Task {
			r.Verdict = Review
		}
		return r
	}

	// The body: predefined variables, the job's variables, the script.
	if eventDep {
		p.Event = map[string]string{"merge_request_event": "pull_request", "push": "push"}[chosen.sc.source]
	}
	var b strings.Builder
	stage := cmpOr(str(get("stage")), "test")
	fmt.Fprintf(&b, "export CI=true GITLAB_CI=true CI_SERVER=yes CI_PROJECT_DIR=\"$PWD\" CI_BUILDS_DIR=\"${PWD%%/*}\" CI_JOB_NAME=%s CI_JOB_NAME_SLUG=%s CI_JOB_STAGE=%s\n",
		shellQuote(l.name), glSlug(l.name), shellQuote(stage))
	if l.total > 0 {
		fmt.Fprintf(&b, "export CI_NODE_INDEX=%d CI_NODE_TOTAL=%d\n", l.index, l.total)
	}
	if p.Event != "" {
		fmt.Fprintf(&b, "export CI_PIPELINE_SOURCE=%s\n", chosen.sc.source)
	}
	if a.repo.DefaultBranch != "" {
		fmt.Fprintf(&b, "export CI_DEFAULT_BRANCH=%s\n", shellQuote(a.repo.DefaultBranch))
	}
	for _, k := range sortedKeys(a.repo.vars()) {
		fmt.Fprintf(&b, "export %s=%s\n", k, shellQuote(a.repo.vars()[k]))
	}
	for _, k := range vars.keys {
		if k == "CI_NODE_INDEX" {
			continue
		}
		if !validEnvName.MatchString(k) {
			review("variable " + k + " isn't a valid shell name")
			continue
		}
		fmt.Fprintf(&b, "export %s=%s\n", k, a.shellValue(k, vars, map[string]bool{}))
	}
	b.WriteString(script)
	p.Body = b.String()

	p.TimeoutMinutes = 60 // GitLab's default job timeout
	if t := str(get("timeout")); t != "" {
		if m, ok := glDuration(t); ok {
			p.TimeoutMinutes = m
		} else {
			review("timeout " + t + " isn't a duration cinema reads")
		}
	}
	def, _ := json.Marshal(struct {
		Job      any `json:"job"`
		Workflow any `json:"workflow"`
	}{plain(j), plain(a.c.top.m["workflow"])})
	sum := sha256.Sum256(def)
	p.DefDigest = "sha256:" + hex.EncodeToString(sum[:])
	r.Proposal = p
	return r
}

// decide is whether job j runs in a scenario's pipeline: the workflow's
// decision, then the job's (its rules:, or only:/except:).
func (a *glAnalysis) decide(j *gmap, sc glScenario, vars *gmap) (wd, d glDecision) {
	wd = glDecision{runs: triTrue, when: "always"}
	if wf, ok := a.c.top.get("workflow"); ok {
		if wr, ok := wf.(*gmap).get("rules"); ok {
			wd = evalRules(wr, sc.env(a.yamlVals(glVarsOf(a.globalVars()), sc, nil)), "", a.exists)
		}
	}
	jobWhen := str(j.m["when"])
	switch {
	case wd.runs == triFalse:
		return wd, glDecision{runs: triFalse}
	case wd.runs == triUnknown:
		return wd, glDecision{runs: triUnknown, why: "workflow: " + wd.why}
	}
	if rules, ok := j.get("rules"); ok {
		return wd, evalRules(rules, sc.env(a.yamlVals(vars, sc, nil)), jobWhen, a.exists)
	}
	t, why := onlyExcept(j, sc, sc.env(a.yamlVals(vars, sc, nil)))
	d = glDecision{runs: t, when: cmpOr(jobWhen, "on_success"), why: why}
	if v, ok := j.get("allow_failure"); ok {
		d.allowFailure = str(v)
	}
	return wd, d
}

// MergeRequestPipelines says whether the project's pipeline runs anything in
// merge request pipelines (or might): setup's cinema jobs then run there too.
// Without workflow: rules, jobs without rules: run in branch pipelines only,
// and a cinema job that ran in merge request pipelines would create them.
func (g *GitLabConfig) MergeRequestPipelines() bool {
	repo := gitlabRepo(g.c.root)
	a := &glAnalysis{c: g.c, repo: repo}
	sc := glScenario{source: "merge_request_event", defaultBranch: repo.DefaultBranch, project: repo.vars()}
	for _, n := range g.c.order {
		j := g.c.jobs[n]
		if j == nil {
			return true // can't be resolved: assume it might
		}
		if _, d := a.decide(j, sc, glVarsOf(j.m["variables"])); d.runs != triFalse {
			return true
		}
	}
	return false
}

var validEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// glImageRef: a literal image reference (no variables, no spaces).
var glImageRef = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]*(@sha256:[0-9a-f]{64})?$`)

func glEnvName(v any) string {
	if m, ok := v.(*gmap); ok {
		n, _ := m.get("name")
		return str(n)
	}
	return str(v)
}

func maps2Equal(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// glSlug is GitLab's $CI_JOB_NAME_SLUG: lowercased, non-alphanumerics to -,
// at most 63 characters, no leading or trailing -.
func glSlug(s string) string {
	s = strings.ToLower(regexp.MustCompile(`[^a-zA-Z0-9]+`).ReplaceAllString(s, "-"))
	if len(s) > 63 {
		s = s[:63]
	}
	return strings.Trim(s, "-")
}

// glVarsOf is a variables: mapping as name → value or {value, expand}.
func glVarsOf(v any) *gmap {
	out := newGmap()
	m, _ := v.(*gmap)
	if m == nil {
		return out
	}
	for _, k := range m.keys {
		out.set(k, m.m[k])
	}
	return out
}

func (a *glAnalysis) globalVars() any {
	v, _ := a.c.top.get("variables")
	return v
}

// glVarValue is a variable's value and whether $refs in it expand.
func glVarValue(v any) (string, bool) {
	if m, ok := v.(*gmap); ok {
		val, _ := m.get("value")
		exp, _ := m.get("expand")
		return str(val), str(exp) != "false"
	}
	return str(v), true
}

// yamlVals are the job's variables as rule-evaluation values, expanded.
func (a *glAnalysis) yamlVals(vars *gmap, sc glScenario, _ map[string]bool) map[string]glVal {
	out := map[string]glVal{}
	env := sc.env(nil)
	var expand func(k string, depth int) glVal
	expand = func(k string, depth int) glVal {
		v, expands := glVarValue(vars.m[k])
		if !expands {
			return knownVal(v)
		}
		unknown := false
		s := glVarRef.ReplaceAllStringFunc(v, func(ref string) string {
			if ref == "$$" {
				return "$"
			}
			n := strings.Trim(ref, "${}")
			if _, own := vars.m[n]; own && n != k && depth < 10 {
				x := expand(n, depth+1)
				if x.state == vSet {
					return x.s
				}
				unknown = true
				return ""
			}
			x := env(n)
			switch x.state {
			case vSet:
				return x.s
			case vUnset:
				return ""
			}
			unknown = true
			return ""
		})
		if unknown {
			return glVal{state: vUnknown}
		}
		return knownVal(s)
	}
	for _, k := range vars.keys {
		out[k] = expand(k, 0)
	}
	return out
}

// expandStatic expands $VARS in an image name or tag with the job's
// variables; what it can't resolve stays (and then isn't a literal).
func (a *glAnalysis) expandStatic(s string, vars *gmap) string {
	return glVarRef.ReplaceAllStringFunc(s, func(ref string) string {
		n := strings.Trim(ref, "${}")
		if v, ok := vars.m[n]; ok {
			val, _ := glVarValue(v)
			if !strings.Contains(val, "$") {
				return val
			}
		}
		if v, ok := a.repo.vars()[n]; ok {
			return v
		}
		return ref
	})
}

// shellValue is variable k as a shell word: references to other job
// variables expanded here (GitLab expands them all, in any order), references
// to predefined variables a run sets left to the shell (the preamble exports
// them), anything else empty, as on GitLab.
func (a *glAnalysis) shellValue(k string, vars *gmap, _ map[string]bool) string {
	type seg struct {
		lit string
		ref string
	}
	var segs []seg
	var walk func(k string, seen map[string]bool)
	walk = func(k string, seen map[string]bool) {
		v, expands := glVarValue(vars.m[k])
		if !expands {
			segs = append(segs, seg{lit: v})
			return
		}
		last := 0
		for _, m := range glVarRef.FindAllStringSubmatchIndex(v, -1) {
			segs = append(segs, seg{lit: v[last:m[0]]})
			last = m[1]
			ref := v[m[0]:m[1]]
			if ref == "$$" {
				segs = append(segs, seg{lit: "$"})
				continue
			}
			n := strings.Trim(ref, "${}")
			switch {
			case vars.m[n] != nil && !seen[n]:
				seen[n] = true
				walk(n, seen)
				delete(seen, n)
			case a.repo.vars()[n] != "":
				segs = append(segs, seg{lit: a.repo.vars()[n]})
			case glProvided[n] || n == "CI_DEFAULT_BRANCH" || n == "HOME" || n == "PATH":
				segs = append(segs, seg{ref: n})
			}
		}
		segs = append(segs, seg{lit: v[last:]})
	}
	walk(k, map[string]bool{k: true})
	var b strings.Builder
	lit := ""
	flush := func() {
		if lit != "" {
			b.WriteString(shellQuote(lit))
			lit = ""
		}
	}
	for _, s := range segs {
		if s.ref == "" {
			lit += s.lit
			continue
		}
		flush()
		b.WriteString(`"${` + s.ref + `}"`)
	}
	flush()
	if b.Len() == 0 {
		return "''"
	}
	return b.String()
}

// service is one services: entry as a task service, its network alias, or why not.
func (a *glAnalysis) service(s any, vars *gmap, jobEnv map[string]glVal) (Service, string, string) {
	var img, alias string
	var own *gmap
	switch x := s.(type) {
	case string:
		img = x
	case *gmap:
		n, _ := x.get("name")
		img = str(n)
		al, _ := x.get("alias")
		alias = strings.TrimSpace(strings.Split(str(al), ",")[0])
		for _, k := range x.keys {
			switch k {
			case "name", "alias", "pull_policy":
			case "variables":
				own = glVarsOf(x.m[k])
			default:
				return Service{}, "", "service " + img + " sets " + k + ": cinema runs services with their image, variables and alias only"
			}
		}
	default:
		return Service{}, "", "a services: entry cinema can't read"
	}
	img = a.expandStatic(img, vars)
	if !glImageRef.MatchString(img) {
		return Service{}, "", "service " + img + " isn't a literal image reference"
	}
	if strings.Contains(img, "dind") {
		return Service{}, "", "uses Docker-in-Docker (" + img + "): it needs a privileged container"
	}
	if alias == "" {
		repo, _, _ := strings.Cut(img, "@")
		if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
			repo = repo[:i]
		}
		alias = strings.ReplaceAll(repo, "/", "-")
	}
	alias = strings.ToLower(alias)
	svc := Service{Image: img}
	add := func(k string, v glVal) string {
		if v.state != vSet {
			return "service " + alias + " gets variable " + k + ", whose value cinema can't resolve"
		}
		if svc.Env == nil {
			svc.Env = map[string]string{}
		}
		svc.Env[k] = v.s
		return ""
	}
	for _, k := range vars.keys {
		if why := add(k, jobEnv[k]); why != "" {
			return Service{}, "", why
		}
	}
	if own != nil {
		for k, v := range a.yamlVals(own, a.scenarios[0], nil) {
			if why := add(k, v); why != "" {
				return Service{}, "", why
			}
		}
	}
	return svc, alias, ""
}

// earlierArtifacts names jobs in earlier stages that upload artifacts, which
// GitLab hands to a job without needs: or dependencies:.
func (a *glAnalysis) earlierArtifacts(j *gmap) []string {
	stage := func(m *gmap) int {
		s, _ := m.get("stage")
		n := cmpOr(str(s), "test")
		switch n {
		case ".pre":
			return -1
		case ".post":
			return len(a.c.stages)
		}
		return slices.Index(a.c.stages, n)
	}
	mine := stage(j)
	var out []string
	for _, n := range a.c.order {
		o := a.c.jobs[n]
		if o == nil || o == j || stage(o) >= mine {
			continue
		}
		if art, ok := o.get("artifacts"); ok {
			if am, ok := art.(*gmap); ok {
				_, paths := am.get("paths")
				rep, _ := am.get("reports")
				rm, _ := rep.(*gmap)
				_, dotenv := rm.get("dotenv")
				if paths || dotenv {
					out = append(out, n)
				}
			}
		}
	}
	return out
}

// exists decides rules: exists: for literal paths in the checkout; patterns
// with wildcards are unknown.
func (a *glAnalysis) exists(pats []string) tri {
	if a.c.root == "" {
		return triUnknown
	}
	return GitLabExists(a.c.root, pats)
}

// GitLabExists: does any of pats name a file in root? Wildcards: unknown.
func GitLabExists(root string, pats []string) tri {
	for _, p := range pats {
		if strings.ContainsAny(p, "*?[{$") {
			return triUnknown
		}
		if fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(p, "/")))); err == nil && !fi.IsDir() {
			return triTrue
		}
	}
	return triFalse
}

var glDurPart = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*(d|days?|h|hrs?|hours?|m|mins?|minutes?|s|secs?|seconds?)?\b`)

// glDuration reads GitLab's human durations ("1h 30m", "90 minutes", "3600")
// as whole minutes, rounded up.
func glDuration(s string) (int, bool) {
	s = strings.TrimSpace(s)
	secs := 0.0
	rest := s
	for _, m := range glDurPart.FindAllStringSubmatch(s, -1) {
		n, _ := strconv.ParseFloat(m[1], 64)
		unit := strings.ToLower(m[2])
		switch {
		case unit == "" || strings.HasPrefix(unit, "s"):
			secs += n
		case strings.HasPrefix(unit, "m"):
			secs += n * 60
		case strings.HasPrefix(unit, "h"):
			secs += n * 3600
		case strings.HasPrefix(unit, "d"):
			secs += n * 86400
		}
		rest = strings.Replace(rest, m[0], "", 1)
	}
	if strings.Trim(rest, " ,and") != "" || secs <= 0 {
		return 0, false
	}
	return int((secs + 59) / 60), true
}
