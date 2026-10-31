// Package analyze reads a repo's GitHub Actions workflows and proposes CInema
// tasks for the jobs a Linux evidence machine could run (plan §16 P3: analyze).
// Deterministic and advisory: it never writes tasks.yaml and never claims a
// proposal is equivalent to the job. `cinema run` on the proposal is the check.
package analyze

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode"

	"go.yaml.in/yaml/v3"

	"github.com/billiondollarsolo/cinema/internal/task"
)

// Verdicts.
const (
	Task   = "task"   // proposable as is
	Review = "review" // proposable, but something needs a human (see Reasons)
	Skip   = "skip"   // not something a Linux evidence machine should claim
)

// Job is one workflow job, per matrix combination.
type Job struct {
	Workflow string            `json:"workflow"`
	ID       string            `json:"job"`
	Combo    map[string]string `json:"matrix,omitempty"`
	Name     string            `json:"name"` // as Actions shows it: name: with matrix values substituted, else the id
	// MatrixKeys are the matrix's own keys (not include/exclude): a leg is
	// identified by their values (skip-in-place excludes reused legs by them).
	MatrixKeys []string  `json:"matrix_keys,omitempty"`
	RunsOn     string    `json:"runs_on"`
	Verdict    string    `json:"verdict"`
	Reasons    []string  `json:"reasons,omitempty"`
	Notes      []string  `json:"notes,omitempty"` // decisions made for the draft (e.g. a resolved version)
	Proposal   *Proposal `json:"proposal,omitempty"`
}

// Proposal is a suggested task: the job's run steps, in order.
type Proposal struct {
	ID   string `json:"id"`
	Go   string `json:"go,omitempty"`   // exact versions only (runtime:)
	Node string `json:"node,omitempty"` // exact versions only (runtime:)
	// Runtimes: other pinned runtimes (python, java, …), by task.RuntimeKind name.
	Runtimes map[string]string `json:"runtimes,omitempty"`
	// Probes: version commands of tools the job uses unpinned (the runner's
	// python3), fingerprinted where the task runs (toolchain:).
	Probes [][]string `json:"probes,omitempty"`
	Body   string     `json:"script"` // bash script; each step in its own subshell
	// Outputs: paths the job uploads as artifacts, so it writes them (outputs:).
	Outputs []string `json:"outputs,omitempty"`
	// Container: the job's container image; cinema runs the steps inside it.
	Container string `json:"container,omitempty"`
	// Exclusive: run alone under -j (the draft changes the machine, e.g. sudo apt-get).
	Exclusive bool `json:"exclusive,omitempty"`
	// Services: the job's service containers, started beside the task.
	Services map[string]Service `json:"services,omitempty"`
	// Event: the job's steps depend on the triggering event (github.event_name,
	// github.ref, GITHUB_REF…), so the draft is only evidence for the event it
	// was drafted for (plan §11.2).
	Event string `json:"event,omitempty"`
	// Floating: the version specs the pins were resolved from ("Go": "stable"),
	// so the gate re-resolves them (plan §11.2).
	Floating map[string]string `json:"floating,omitempty"`
	// TimeoutMinutes: the job's timeout-minutes (GitHub's default is 360).
	TimeoutMinutes int `json:"timeout_minutes,omitempty"`
	// Shell: the argv the script is run with, before the script itself (empty:
	// bash -eo pipefail -c, Actions' default). GitLab jobs (gitlab.go).
	Shell []string `json:"shell,omitempty"`
	// CPUs, MemoryGB: the job's runner, when its tags name one (GitLab); 0:
	// the repo's default runner size.
	CPUs     int `json:"cpus,omitempty"`
	MemoryGB int `json:"memory_gb,omitempty"`
	// DefDigest: the job's whole resolved definition (GitLab: after includes,
	// extends, !reference and default:), so any change to it is a new job.
	DefDigest string `json:"definition,omitempty"`
}

type workflow struct {
	On       yaml.Node       `yaml:"on"`
	Env      map[string]any  `yaml:"env"`
	Defaults defaults        `yaml:"defaults"`
	Jobs     map[string]*job `yaml:"jobs"`
}

type defaults struct {
	Run struct {
		WorkingDirectory string `yaml:"working-directory"`
	} `yaml:"run"`
}

type job struct {
	Name      string               `yaml:"name"`
	Needs     yaml.Node            `yaml:"needs"`
	RunsOn    yaml.Node            `yaml:"runs-on"`
	If        string               `yaml:"if"`
	Uses      string               `yaml:"uses"` // reusable workflow
	Timeout   string               `yaml:"timeout-minutes"`
	Container yaml.Node            `yaml:"container"`
	Services  map[string]yaml.Node `yaml:"services"`
	Env       map[string]any       `yaml:"env"`
	Strategy  struct {
		Matrix yaml.Node `yaml:"matrix"`
	} `yaml:"strategy"`
	Defaults    defaults  `yaml:"defaults"`
	Steps       []step    `yaml:"steps"`
	Permissions yaml.Node `yaml:"permissions"`
}

type step struct {
	Uses string         `yaml:"uses"`
	Run  string         `yaml:"run"`
	With map[string]any `yaml:"with"`
	Env  map[string]any `yaml:"env"`
	Dir  string         `yaml:"working-directory"`
	If   string         `yaml:"if"`
	at   string         // shell run before the step: a producer's steps run in its own copy (artifact.go)
}

// Repo analyzes every workflow under root/.github/workflows.
func Repo(root string) ([]Job, error) {
	var files []string
	for _, pat := range []string{"*.yml", "*.yaml"} {
		m, _ := filepath.Glob(filepath.Join(root, ".github", "workflows", pat))
		files = append(files, m...)
	}
	sort.Strings(files)
	var out []Job
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		jobs, err := workflowAt(root, filepath.Base(f), b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		out = append(out, jobs...)
	}
	if _, err := os.Lstat(filepath.Join(root, GitLabFile)); err == nil { // GitLab CI (gitlab.go)
		jobs, err := GitLab(root, nil)
		if err != nil { // never the GitHub jobs' problem: one entry that says why
			jobs = []Job{{Workflow: GitLabFile, ID: "(pipeline)", Name: GitLabFile, Verdict: Review, Reasons: []string{"can't be read: " + err.Error()}}}
		}
		out = append(out, jobs...)
	}
	uniqueIDs(out)
	return out, nil
}

// uniqueIDs makes proposal ids valid task ids (at most 64 characters: long ones
// keep a hash of the full id) and distinct: a job whose id is taken by a
// different script gets its workflow's name appended, then a counter.
func uniqueIDs(jobs []Job) {
	fit := func(id string) string {
		if len(id) <= 64 {
			return id
		}
		sum := sha256.Sum256([]byte(id))
		return strings.TrimRight(id[:55], "-.") + "-" + hex.EncodeToString(sum[:4])
	}
	body := map[string]string{}
	for _, j := range jobs {
		p := j.Proposal
		if p == nil {
			continue
		}
		id := fit(p.ID)
		if b, ok := body[id]; ok && b != p.Body {
			stem := strings.TrimSuffix(strings.TrimSuffix(j.Workflow, ".yml"), ".yaml")
			base := fit(taskID(p.ID+"-"+stem, nil))
			id = base
			for n := 2; body[id] != "" && body[id] != p.Body; n++ {
				id = fit(fmt.Sprintf("%s-%d", base, n))
			}
		}
		p.ID, body[id] = id, p.Body
	}
}

// Workflow analyzes one workflow file's jobs. Workflows that don't run on
// push or pull_request (releases, schedules, issue bots) are not CI checks.
func Workflow(name string, src []byte) ([]Job, error) {
	jobs, err := workflowAt("", name, src)
	uniqueIDs(jobs)
	return jobs, err
}

// workflowAt is Workflow with the repo root, so version files can be read.
func workflowAt(root, name string, src []byte) ([]Job, error) {
	var w workflow
	if err := yaml.Unmarshal(src, &w); err != nil {
		return nil, err
	}
	if !triggersCI(&w.On) || strings.HasPrefix(filepath.Base(name), "copilot-setup-steps.") {
		return nil, nil // copilot-setup-steps: the Copilot agent's environment, not a check
	}
	var out []Job
	for _, id := range sortedKeys(w.Jobs) {
		j := w.Jobs[id]
		if slices.ContainsFunc(j.Steps, func(s step) bool { return cinemaGate.MatchString(s.Run) }) {
			continue // cinema's own job (the gate, or setup's skip-in-place job)
		}
		j.If = originalIf(j.If, needsCinema(&j.Needs))
		if j.Defaults.Run.WorkingDirectory == "" {
			// Lab: prettier's release-script job ran the whole suite from the root.
			j.Defaults = w.Defaults
		}
		combos, err := expand(&j.Strategy.Matrix)
		if err != nil {
			out = append(out, Job{Workflow: name, ID: id, Name: cmp.Or(j.Name, id), Verdict: Review, Reasons: []string{"matrix: " + err.Error()}})
			continue
		}
		var keys []string
		if m := &j.Strategy.Matrix; m.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(m.Content); i += 2 {
				if k := m.Content[i].Value; k != "include" && k != "exclude" {
					keys = append(keys, k)
				}
			}
		}
		for _, c := range combos {
			r := analyzeJob(root, name, id, j, &w, c)
			r.Notes = slices.DeleteFunc(r.Notes, func(n string) bool { return strings.HasPrefix(n, floatingNote) })
			r.MatrixKeys = keys
			out = append(out, r)
		}
	}
	return out, nil
}

func analyzeJob(root, wf, id string, j *job, w *workflow, combo map[string]string) Job {
	wfEnv := w.Env
	r := Job{Workflow: wf, ID: id, Combo: combo, Verdict: Task}
	skip := func(why string) { r.Verdict, r.Reasons = Skip, append(r.Reasons, why) }
	review := func(why string) {
		if r.Verdict == Task {
			r.Verdict = Review
		}
		r.Reasons = append(r.Reasons, why)
	}
	sub := func(s string) string { return substitute(s, combo) }
	note := func(s string) { r.Notes = append(r.Notes, s) }
	r.Name = sub(j.Name)
	if r.Name == "" {
		r.Name = id
	}

	if j.Uses != "" {
		skip("calls reusable workflow " + j.Uses)
		return r
	}
	if j.If != "" {
		switch v, ok := evalIf(j.If, combo); {
		case ok && !v:
			skip("job condition is false for this combination (if: " + j.If + ")")
			return r
		case statusFn.MatchString(j.If) || reportsNeeds(j):
			skip("reports other jobs' results (if: " + j.If + "); not a check")
			return r
		case !ok:
			review("the job is conditional (if: " + j.If + ")")
		}
	}
	if j.If == "" && reportsNeeds(j) {
		skip("reports other jobs' results (needs.*.result); not a check")
		return r
	}
	if idToken(&j.Permissions) {
		skip("requests an OIDC token (permissions: id-token: write): publishes or deploys; not a check")
		return r
	}
	r.RunsOn = sub(scalarOrList(&j.RunsOn))
	switch os := strings.ToLower(r.RunsOn); {
	case strings.Contains(os, "windows"), strings.Contains(os, "macos"):
		skip("runs on " + r.RunsOn)
	case strings.Contains(os, "arm"), strings.Contains(os, "aarch64"):
		skip("runs on " + r.RunsOn + " (ARM, not linux/amd64)")
	case strings.Contains(os, "self-hosted"):
		skip("runs on a self-hosted runner (" + r.RunsOn + "): its machine isn't known")
	case strings.Contains(os, "${{"):
		review("runs-on is an expression: " + r.RunsOn)
	case !strings.Contains(os, "ubuntu") && !strings.Contains(os, "linux"):
		skip("runs on " + r.RunsOn + " (not a GitHub Linux runner)")
	}
	image := ""
	if !j.Container.IsZero() {
		img := sub(scalarOrMap(&j.Container, "image"))
		if extra := containerExtras(&j.Container); img == "" || strings.Contains(img, "${{") || extra != "" {
			review("runs in a container (" + img + extra + "): cinema can run a literal image with no options; translate the rest")
		} else {
			image = img
		}
	}
	services := map[string]Service{}
	for _, name := range sortedKeys(j.Services) {
		n := j.Services[name]
		svc, why := service(&n, sub)
		if why != "" {
			review("service " + name + " " + why + ": cinema runs services with a literal image, env, ports and options")
			continue
		}
		services[name] = svc
	}

	p := &Proposal{ID: taskID(id, combo), Container: image}
	if len(services) > 0 {
		p.Services = services
		note("starts " + strings.Join(sortedKeys(services), ", ") + " as in CI (cinema runs the service containers and fingerprints their image digests)")
	}
	if image != "" {
		note("runs in " + image + " as in CI: cinema runs the steps inside it, as root, and fingerprints the image digest")
	}
	var body []string
	plumbing := false // a step writes $GITHUB_ENV & co.: emulate them between steps
	env := literalEnv(wfEnv, combo, review)
	env = append(env, literalEnv(j.Env, combo, review)...)
	envVals := map[string]string{} // literal workflow/job env, for ${{ env.X }} in action inputs
	for _, m := range []map[string]any{wfEnv, j.Env} {
		for k, v := range m {
			envVals[k] = sub(str(v))
		}
	}
	// pin sets a runtime pin from a setup action (one version per task).
	pin := func(rt, ver string) {
		switch rt {
		case "go":
			p.Go = onePin(p.Go, ver, "Go", review)
		case "node":
			p.Node = onePin(p.Node, ver, "Node", review)
		default:
			if p.Runtimes == nil {
				p.Runtimes = map[string]string{}
			}
			if v := onePin(p.Runtimes[rt], ver, rt, review); v != "" {
				p.Runtimes[rt] = v
			}
		}
	}
	probe := func(argv []string) {
		if !slices.ContainsFunc(p.Probes, func(a []string) bool { return slices.Equal(a, argv) }) {
			p.Probes = append(p.Probes, argv)
		}
	}
	ran := map[string]bool{} // actions the job uses, lowercased
	steps, inlined := j.Steps, 0
	for i := 0; i < len(steps); i++ {
		s := steps[i]
		if s.If != "" {
			switch v, ok := evalIf(s.If, combo); {
			case !ok:
				review("a step is conditional (if: " + s.If + "); it is included unconditionally")
			case !v:
				continue // false for this matrix combination: Actions skips it
			}
		}
		uses := sub(s.Uses)
		action, _, _ := strings.Cut(uses, "@")
		ran[strings.ToLower(action)] = true
		switch {
		case action == "":
		case action == "actions/checkout" && !selfRepo(sub(str(s.With["repository"]))):
			skip("checks out another repository (" + sub(str(s.With["repository"])) + "): tests code outside this commit")
		case action == "actions/checkout" && str(s.With["path"]) != "":
			review("checks out into path: " + str(s.With["path"]) + "; cinema runs at the repository root: adjust working directories")
		case strings.Contains(action, "artifact") && (strings.Contains(action, "upload") || strings.Contains(action, "save")):
			for _, path := range strings.Split(sub(str(s.With["path"]))+"\n"+sub(str(s.With["directory"])), "\n") {
				path = strings.TrimSpace(path)
				if path != "" && filepath.IsLocal(path) && !strings.ContainsAny(path, "*?[!$") && !slices.Contains(p.Outputs, path) {
					p.Outputs = append(p.Outputs, path)
				}
			}
		case action == "actions/checkout", action == "actions/cache", strings.HasPrefix(action, "actions/cache/"),
			strings.HasSuffix(action, "/harden-runner"):
			// checkout: cinema runs in a clean checkout; caches and uploads don't change results
		case action == "actions/download-artifact":
			pre, sub, producer, why := artifactSteps(root, w, j, s.With, len(r.Notes))
			if why != "" {
				skip(why)
				break
			}
			body = slices.Insert(body, 0, pre) // before the job's own steps touch the tree
			steps = slices.Insert(slices.Clone(steps), i+1, sub...)
			note("builds artifact " + str(s.With["name"]) + " as job " + producer + " does, in a fresh copy of the commit (CI builds it once and shares it)")
		case strings.Contains(action, "artifact") && !strings.Contains(action, "upload") && !strings.Contains(action, "save"):
			skip("needs artifacts from another job (download-artifact)")
		case action == "actions/setup-go":
			p.Go = onePin(p.Go, exactVersion(root, sub(str(s.With["go-version"])), `^1\.[0-9]+\.[0-9]+$`, "Go", str(s.With["go-version-file"]), review, note), "Go", review)
			// setup-go also puts GOPATH/bin on PATH: tools a job `go install`s are
			// then found (P4-1: cli's go-licenses; the VPS's login PATH hid it).
			steps = slices.Insert(slices.Clone(steps), i+1, step{Run: `echo "$(go env GOPATH)/bin" >> "$GITHUB_PATH"`, Dir: "."})
		case action == "actions/setup-node":
			p.Node = onePin(p.Node, exactVersion(root, sub(str(s.With["node-version"])), `^[0-9]+\.[0-9]+\.[0-9]+$`, "Node", str(s.With["node-version-file"]), review, note), "Node", review)
		case action == "pnpm/setup":
			// pnpm and a runtime in one step (runtime: node@24): the Node pin as
			// setup-node's, then pnpm as pnpm/action-setup's.
			name, ver, _ := strings.Cut(sub(str(s.With["runtime"])), "@")
			switch name {
			case "":
			case "node":
				if ver == "lts" {
					ver = "lts/*"
				}
				p.Node = onePin(p.Node, exactVersion(root, ver, `^[0-9]+\.[0-9]+\.[0-9]+$`, "Node", "", review, note), "Node", review)
			default:
				review("uses pnpm/setup with runtime " + name + ": cinema provisions Go and Node only so far")
			}
			run, why := pnpmStep(root, s.With, envVals, note)
			if why != "" {
				review(strings.Replace(why, "pnpm/action-setup", "pnpm/setup", 1))
				break
			}
			steps = slices.Insert(slices.Clone(steps), i+1, step{Run: run, Dir: "."})
		case action == "golangci/golangci-lint-action", action == "pnpm/action-setup":
			tr := golangciStep
			if action == "pnpm/action-setup" {
				tr = pnpmStep
			}
			run, why := tr(root, s.With, envVals, note)
			if why != "" {
				review(why)
				break
			}
			steps = slices.Insert(slices.Clone(steps), i+1, step{Run: run, Dir: str(s.With["working-directory"])})
		case setupActions[strings.ToLower(action)] != nil:
			_, ref, _ := strings.Cut(uses, "@")
			c := &SetupCtx{Root: root, Ref: ref, With: func(k string) string { return sub(str(s.With[k])) }, Env: envVals, Review: review, Note: note,
				has: func(k string) bool { _, ok := s.With[k]; return ok }, pin: pin, probe: probe,
				step: func(run, dir string) {
					steps = slices.Insert(slices.Clone(steps), i+1, step{Run: run, Dir: cmp.Or(dir, ".")})
				},
			}
			setupActions[strings.ToLower(action)](c)
		case strings.HasPrefix(action, "github/codeql-action"):
			skip("CodeQL analysis uploads results; not a pass/fail check")
		default:
			sub, err := compositeSteps(uses, s.With)
			if err != nil || inlined+len(sub) > 100 {
				review("uses action " + uses + ": no local equivalent known; reproduce it with commands")
				break
			}
			// A composite action is steps like the job's own: translate them in place.
			steps, inlined = slices.Insert(slices.Clone(steps), i+1, sub...), inlined+len(sub)
			note("inlined composite action " + uses)
		}
		if s.Run == "" {
			continue
		}
		run := sub(s.Run)
		if strings.Contains(run, "secrets.") || anyEnvMentions(s.Env, "secrets.") {
			skip("a step uses secrets")
		}
		if what := publishing(run); what != "" && !strings.Contains(run, "--dry-run") {
			skip("a step publishes or releases (" + what + "): not a check")
		}
		if strings.Contains(run, "github.event.") || anyEnvMentions(s.Env, "github.event.") {
			skip("a step reads the triggering event (github.event.*, e.g. the PR description): depends on the PR, not the commit")
		}
		if ghFiles.MatchString(run) {
			plumbing = true
		}
		if sudo.MatchString(run) {
			review("a step uses sudo (installs or changes the machine): make it a prerequisite of the evidence machine, not part of the task")
			p.Exclusive = true // machine-wide state (apt's lock): never alongside other tasks (lab: prometheus)
		}
		if pipeToShell.MatchString(run) {
			review("a step pipes a downloaded script into a shell: an unpinned network input; pin a release and check its SHA-256")
		} else if floating.MatchString(run) {
			review("a step fetches a floating version (@latest / HEAD / master): results can change without a source change")
		}
		if strings.Contains(run, "${{") {
			review("a step uses an expression cinema can't evaluate: " + firstExpr(run))
		}
		stepEnv := literalEnv(s.Env, combo, review)
		dir := s.Dir
		if dir == "" {
			dir = j.Defaults.Run.WorkingDirectory
		}
		var b strings.Builder
		b.WriteString("(\n")
		if plumbing {
			b.WriteString("  _gh_load\n")
		}
		if s.at != "" {
			fmt.Fprintf(&b, "  %s\n", s.at)
		}
		if dir != "" {
			fmt.Fprintf(&b, "  cd %s\n", shellQuote(sub(dir)))
		}
		for _, e := range stepEnv {
			fmt.Fprintf(&b, "  export %s\n", e)
		}
		b.WriteString(strings.TrimRight(run, "\n") + "\n") // verbatim: indenting would break heredocs and multi-line strings
		b.WriteString(")")
		body = append(body, b.String())
	}
	// Steps that use a runtime with no setup action (the runner's preinstalled
	// toolchain) would leave the task with no environment fingerprint: its
	// receipts would match whatever is installed (lab: prometheus). Each
	// runtime's hook says what CI's job really gets.
	all := strings.Join(body, "\n")
	if p.Container != "" {
		// The image provides the toolchain (runtime: can't be combined with container:).
		p.Go, p.Node, p.Runtimes, p.Probes, all = "", "", nil, nil, ""
	}
	for _, rt := range sortedKeys(withoutSetup) {
		h := withoutSetup[rt]
		pinned := map[string]string{"go": p.Go, "node": p.Node}[rt] != "" || p.Runtimes[rt] != ""
		if pinned || !h.uses.MatchString(all) || slices.ContainsFunc(h.actions, func(a string) bool { return ran[a] }) {
			continue
		}
		h.f(&SetupCtx{Root: root, With: func(string) string { return "" }, Env: envVals, Review: review, Note: note, pin: pin, probe: probe})
	}
	if len(body) == 0 {
		if r.Verdict == Review {
			// A Linux check made only of actions: claimable once someone writes the commands.
			r.Reasons = append(r.Reasons, "no shell steps: translate the actions into commands to make a task")
			return r
		}
		skip("no shell steps")
	}
	if r.Verdict != Skip {
		var b strings.Builder
		for _, e := range env {
			b.WriteString("export " + e + "\n")
		}
		if plumbing {
			b.WriteString(ghPreamble)
			note("$GITHUB_ENV, $GITHUB_PATH and $GITHUB_OUTPUT are emulated with temp files, loaded before each step")
		}
		b.WriteString(strings.Join(body, "\n"))
		p.Body = b.String()
		var notes []string
		for _, n := range r.Notes {
			if kv, ok := strings.CutPrefix(n, floatingNote); ok {
				tool, spec, _ := strings.Cut(kv, "=")
				spec, got, _ := strings.Cut(spec, "\x00")
				// Only the spec the pin came from: with several (setup-dotnet's
				// 10.0.x, 9.0.x, 8.0.x; setup-node twice) the first pin is kept.
				if pinned := map[string]string{"Go": p.Go, "Node": p.Node}[tool]; cmp.Or(pinned, p.Runtimes[tool]) == got {
					if p.Floating == nil {
						p.Floating = map[string]string{}
					}
					p.Floating[strings.ToLower(tool)] = spec
				}
				continue
			}
			notes = append(notes, n)
		}
		r.Notes = notes
		p.TimeoutMinutes = 360 // GitHub's default
		if n, err := strconv.Atoi(strings.TrimSpace(sub(j.Timeout))); err == nil && n > 0 {
			p.TimeoutMinutes = n
		}
		if eventDependent(j) {
			p.Event = "pull_request" // drafts evaluate github.event_name as the PR gate's
		}
		r.Proposal = p
	}
	return r
}

// ghPreamble gives the steps Actions' step-to-step files: lines appended to
// $GITHUB_ENV (NAME=value or NAME<<DELIM heredocs) and $GITHUB_PATH are
// applied before each later step; $GITHUB_OUTPUT is written but only read
// through ${{ steps.* }}, which is flagged separately.
const ghPreamble = `export GITHUB_ENV=$(mktemp) GITHUB_PATH=$(mktemp) GITHUB_OUTPUT=$(mktemp) GITHUB_STEP_SUMMARY=/dev/null
trap 'rm -f "$GITHUB_ENV" "$GITHUB_PATH" "$GITHUB_OUTPUT"' EXIT
_gh_load() {
  local l n d v x p
  while IFS= read -r l; do
    n=${l%%=*}
    if [[ $n == *'<<'* ]]; then
      n=${l%%<<*} d=${l#*<<} v=
      while IFS= read -r x && [[ $x != "$d" ]]; do v+=${v:+$'\n'}$x; done
      export "$n=$v"
    elif [[ $l == *=* ]]; then
      export "$n=${l#*=}"
    fi
  done <"$GITHUB_ENV"
  while IFS= read -r p; do [[ -n $p ]] && PATH=$p:$PATH; done <"$GITHUB_PATH"
}
`

// triggersCI: on pull_request, or on push to any branch. A workflow that runs
// only on pushes to named branches or tags (no pull_request) is post-merge:
// releases and deploys, not checks a PR waits for.
func triggersCI(n *yaml.Node) bool {
	var names []string
	var push *yaml.Node
	switch n.Kind {
	case yaml.ScalarNode:
		names = []string{n.Value}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			names = append(names, c.Value)
		}
	case yaml.MappingNode:
		for i := 0; i < len(n.Content); i += 2 {
			names = append(names, n.Content[i].Value)
			if n.Content[i].Value == "push" {
				push = n.Content[i+1]
			}
		}
	}
	if slices.Contains(names, "pull_request") || slices.Contains(names, "pull_request_target") {
		return true
	}
	if !slices.Contains(names, "push") {
		return false
	}
	if push != nil && push.Kind == yaml.MappingNode {
		for i := 0; i < len(push.Content); i += 2 {
			k, v := push.Content[i].Value, push.Content[i+1]
			if k == "tags" || k == "branches" && !slices.ContainsFunc(v.Content, func(b *yaml.Node) bool { return b.Value == "**" || b.Value == "*" }) {
				return false
			}
		}
	}
	return true
}

// expand implements GitHub's matrix expansion: the cartesian product of list
// keys, minus exclude entries, with include entries extending matching combos
// or adding new ones. A matrix given as an expression can't be expanded.
func expand(n *yaml.Node) ([]map[string]string, error) {
	if n.IsZero() {
		return []map[string]string{nil}, nil
	}
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("an expression (%s)", n.Value)
	}
	combos := []map[string]string{{}}
	var include, exclude []map[string]string
	for i := 0; i < len(n.Content); i += 2 {
		k, v := n.Content[i].Value, n.Content[i+1]
		switch {
		case k == "exclude" && v.Kind == yaml.ScalarNode && strings.Contains(v.Value, "needs.cinema.outputs.exclude_"):
			// cinema setup's reused-leg exclude: the job's legs are as written
		case k == "include" || k == "exclude":
			var list []map[string]any
			if err := v.Decode(&list); err != nil {
				return nil, fmt.Errorf("%s: %v", k, err)
			}
			for _, m := range list {
				s := map[string]string{}
				for mk, mv := range m {
					flatten(mk, mv, s)
				}
				if k == "include" {
					include = append(include, s)
				} else {
					exclude = append(exclude, s)
				}
			}
		case v.Kind == yaml.SequenceNode:
			var next []map[string]string
			for _, c := range combos {
				for _, item := range v.Content {
					var val any
					_ = item.Decode(&val)
					m := clone(c)
					flatten(k, val, m)
					next = append(next, m)
				}
			}
			combos = next
		default:
			return nil, fmt.Errorf("key %s is an expression (%s)", k, v.Value)
		}
	}
	combos = slices.DeleteFunc(combos, func(c map[string]string) bool {
		return slices.ContainsFunc(exclude, func(e map[string]string) bool { return subset(e, c) })
	})
	if len(combos) == 1 && len(combos[0]) == 0 {
		combos = nil
	}
	// As in Actions, an include entry extends every original combination
	// whose original values it doesn't overwrite (values earlier entries
	// added may be overwritten), or else adds a combination of its own; the
	// combinations entries add are never extended.
	original := combos
	var base map[string]string
	if len(original) > 0 {
		base = clone(original[0]) // its keys, before entries add any
	}
	for _, inc := range include {
		matched := false
		for _, c := range original {
			if agrees(inc, c, base) {
				for k, v := range inc {
					c[k] = v
				}
				matched = true
			}
		}
		if !matched {
			combos = append(combos, clone(inc))
		}
	}
	if len(combos) == 0 {
		combos = []map[string]string{nil}
	}
	return combos, nil
}

var needsResult = regexp.MustCompile(`needs\.[A-Za-z0-9_*-]+\.result`)

// reportsNeeds: the job's condition or steps look at other jobs' results, an
// aggregator status job (build_all_status), not a check of its own.
func reportsNeeds(j *job) bool {
	if needsResult.MatchString(j.If) {
		return true
	}
	for _, s := range j.Steps {
		if needsResult.MatchString(s.If) || needsResult.MatchString(s.Run) {
			return true
		}
	}
	return false
}

// idToken: permissions grant id-token: write (OIDC, used to publish or deploy).
func idToken(n *yaml.Node) bool {
	var m map[string]string
	return n.Decode(&m) == nil && m["id-token"] == "write"
}

// selfRepo: a checkout of the repository the workflow belongs to.
func selfRepo(r string) bool {
	return r == "" || strings.Contains(r, "github.repository")
}

// flatten stores a matrix value under its key; an object value becomes one
// key per field (package.directory), as ${{ matrix.package.directory }} reads it.
func flatten(k string, v any, into map[string]string) {
	m, ok := v.(map[string]any)
	if !ok {
		into[k] = str(v)
		return
	}
	for mk, mv := range m {
		flatten(k+"."+mk, mv, into)
	}
}

// subset: every key of e has the same value in c.
func subset(e, c map[string]string) bool {
	for k, v := range e {
		if c[k] != v {
			return false
		}
	}
	return true
}

// agrees: inc's values for the original matrix keys (base's) match c's.
func agrees(inc, c, base map[string]string) bool {
	for k, v := range inc {
		if _, orig := base[k]; orig && c[k] != v {
			return false
		}
	}
	return true
}

var (
	statusFn = regexp.MustCompile(`\b(failure|cancelled)\(\)`)
	ghFiles  = regexp.MustCompile(`\$\{?GITHUB_(ENV|PATH|OUTPUT)\b`)
	sudo     = regexp.MustCompile(`(^|[\s;&|(])sudo\s`)
	// Lab: vite's preview job ran `pkg-pr-new publish` (it refused outside Actions).
	publishes   = regexp.MustCompile(`\b(npm|pnpm|yarn|bun|pkg-pr-new|changeset|cargo|gem)(@[\w.^~-]+)? publish\b|\bgoreleaser release\b|\bdocker push\b|\btwine upload\b|\bgh release (create|upload)\b|\bmvn(w)?\b[^\n;&|]*\sdeploy(:deploy)?(\s|$)|\bgradle(w)?\b[^\n;&|]*\s:?([\w-]+:)*publish\w*|\bdotnet nuget push\b|\bdeno publish\b`)
	usesGo      = regexp.MustCompile(`(^|[\s;&|(])(go (build|test|vet|run|install|generate|mod|tool|list)|make)\b`)
	usesNode    = regexp.MustCompile(`(^|[\s;&|(])(node|npm|npx|pnpm|yarn|corepack)\b`)
	pipeToShell = regexp.MustCompile(`(curl|wget)\b[^|\n]*\|\s*(sudo\s+)?(ba)?sh\b`)
	floating    = regexp.MustCompile(`@latest\b|/HEAD/|/(master|main)/[^\s]*\.(sh|py)\b`)
)

// Go and Node without a setup action: pinned from the repo's version file, as
// setup-go/setup-node would (go.mod's toolchain is also what GOTOOLCHAIN=auto
// switches to).
func init() {
	RegisterWithoutSetup("go", usesGo, func(c *SetupCtx) { versionFilePin(c, "go", "Go", `^1\.[0-9]+\.[0-9]+$`, ".go-version", "go.mod") })
	RegisterWithoutSetup("node", usesNode, func(c *SetupCtx) {
		versionFilePin(c, "node", "Node", `^[0-9]+\.[0-9]+\.[0-9]+$`, ".nvmrc", ".node-version")
	})
}

func versionFilePin(c *SetupCtx, rt, name, exact string, files ...string) {
	if f := firstFile(c.Root, files...); f != "" {
		if v := c.Exact(rt, "", f, regexp.MustCompile(exact)); v != "" {
			c.Pin(rt, v)
			c.Note(name + " isn't set up by the job (container or preinstalled); pinned from " + f)
		}
	}
}

// publishing is the first command in run that publishes, or "". Gradle's
// publish*ToMavenLocal only installs into ~/.m2 (lab: gradle-versions-plugin's
// build job runs its examples against it): a build step, not a release.
func publishing(run string) string {
	for _, m := range publishes.FindAllString(run, -1) {
		if !strings.Contains(m, "gradle") {
			return m
		}
		for _, f := range strings.Fields(m) {
			t := f[strings.LastIndex(f, ":")+1:]
			if strings.HasPrefix(t, "publish") && !strings.HasSuffix(t, "ToMavenLocal") {
				return m
			}
		}
	}
	return ""
}

var cinemaGate = regexp.MustCompile(`(^|[\s/])cinema verify\b`)

var (
	matrixRef = regexp.MustCompile(`\$\{\{\s*matrix\.([A-Za-z0-9_.-]+)\s*\}\}`)
	anyExpr   = regexp.MustCompile(`\$\{\{(.*?)\}\}`)
)

// substitute replaces ${{ matrix.X }}, and expressions made only of matrix
// values, literals and pure functions (${{ matrix.os || 'ubuntu-latest' }},
// ${{ format('py{0}', matrix.python) }}). Like Actions, a key this
// combination doesn't define (set only by some include entries) is the empty
// string. Anything else (github.*, steps.*, status functions) stays as is.
func substitute(s string, combo map[string]string) string {
	if combo == nil {
		return s
	}
	s = matrixRef.ReplaceAllStringFunc(s, func(m string) string {
		return combo[matrixRef.FindStringSubmatch(m)[1]]
	})
	return anyExpr.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := evalValue(anyExpr.FindStringSubmatch(m)[1], combo); ok {
			return v
		}
		return m
	})
}

// evalValue evaluates an expression for its value from the matrix alone:
// unlike in evalIf, the event, the runner and status functions are unknown.
func evalValue(src string, combo map[string]string) (string, bool) {
	s := strings.TrimSpace(src)
	toks := ifTokens.FindAllString(s, -1)
	if strings.Join(toks, "") != strings.Join(strings.Fields(s), "") {
		return "", false
	}
	for _, t := range toks {
		// YAML false and 0 reach the combo as "false" and "0": falsy in Actions,
		// truthy as strings. Ambiguous here, so not evaluated (lab: gson).
		if k, ok := strings.CutPrefix(t, "matrix."); ok && (combo[k] == "false" || combo[k] == "0") {
			return "", false
		}
	}
	p := &ifParser{toks: toks, combo: combo, ok: true, valueOnly: true}
	val := p.or()
	if !p.ok || p.i != len(toks) {
		return "", false
	}
	return val, true
}

func exactVersion(root, v, pattern, tool, file string, review, note func(string)) string {
	if v == "" && file != "" && root != "" && filepath.IsLocal(filepath.FromSlash(file)) {
		v = versionFromFile(filepath.Join(root, filepath.FromSlash(file)))
		if filepath.Base(file) == ".tool-versions" {
			v = toolVersionsEntry(v, tool)
		}
	}
	exact := regexp.MustCompile(pattern)
	if exact.MatchString(v) {
		return v
	}
	if v != "" && !strings.Contains(v, "${{") {
		// What setup-go/setup-node would pick today, pinned so evidence and gate agree.
		if got, err := resolve(tool, v); err == nil && exact.MatchString(got) {
			note(fmt.Sprintf("%s %q resolved to %s, the newest matching release today; CI checks it again on every run (floating:)", tool, v, got))
			note(floatingNote + tool + "=" + v + "\x00" + got)
			return got
		}
	}
	switch {
	case file != "":
		review(fmt.Sprintf("%s version comes from %s: pin the exact version in runtime:", tool, file))
	case v == "":
		review(fmt.Sprintf("%s version not set: pin an exact version in runtime:", tool))
	default:
		review(fmt.Sprintf("%s version %q isn't exact: pin one (e.g. the runner's current release) in runtime:", tool, v))
	}
	return ""
}

// versionFromFile reads a version file the way setup-go/setup-node do:
// .go-version / .nvmrc / .node-version hold the version; go.mod's toolchain
// directive wins over its go directive. "" if absent or unreadable.
// The workflow names the file, so it's untrusted: a symlink to /dev/zero or a
// FIFO must not hang or exhaust memory (no symlinks, regular files, capped read).
func versionFromFile(path string) string {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil || len(b) > 1<<20 {
		return ""
	}
	if filepath.Base(path) == "go.mod" {
		var goDir string
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) == 2 && f[0] == "toolchain" {
				return strings.TrimPrefix(f[1], "go")
			}
			if len(f) == 2 && f[0] == "go" {
				goDir = f[1]
			}
		}
		return goDir
	}
	return strings.TrimPrefix(strings.TrimSpace(string(b)), "v")
}

// literalEnv turns literal env values into `K='V'` exports; values with
// expressions other than matrix refs are flagged.
func literalEnv(env map[string]any, combo map[string]string, review func(string)) []string {
	var out []string
	for _, k := range sortedKeys(env) {
		v := substitute(str(env[k]), combo)
		if strings.Contains(v, "${{") {
			if strings.Contains(v, "secrets.") {
				review("env " + k + " comes from secrets")
			} else {
				review("env " + k + " uses an expression: " + firstExpr(v))
			}
			continue
		}
		out = append(out, k+"="+shellQuote(v))
	}
	return out
}

func anyEnvMentions(env map[string]any, s string) bool {
	for _, v := range env {
		if strings.Contains(str(v), s) {
			return true
		}
	}
	return false
}

func taskID(job string, combo map[string]string) string {
	id := job
	for _, k := range sortedKeys(combo) {
		id += "-" + combo[k]
	}
	id = strings.ToLower(regexp.MustCompile(`[^A-Za-z0-9.]+`).ReplaceAllString(id, "-"))
	return strings.Trim(id, "-")
}

func firstExpr(s string) string {
	i := strings.Index(s, "${{")
	j := strings.Index(s[i:], "}}")
	if j < 0 {
		return s[i:]
	}
	return s[i : i+j+2]
}

func scalarOrList(n *yaml.Node) string {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Value
	case yaml.SequenceNode:
		var parts []string
		for _, c := range n.Content {
			parts = append(parts, c.Value)
		}
		return strings.Join(parts, ",")
	case yaml.MappingNode: // runs-on: {group, labels}
		var m map[string]any
		_ = n.Decode(&m)
		return str(m["labels"]) + str(m["group"])
	}
	return ""
}

func scalarOrMap(n *yaml.Node, key string) string {
	if n.Kind == yaml.ScalarNode {
		return n.Value
	}
	var m map[string]any
	_ = n.Decode(&m)
	return str(m[key])
}

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		var parts []string
		for _, x := range t {
			parts = append(parts, str(x))
		}
		return strings.Join(parts, ",")
	}
	return fmt.Sprint(v)
}

func shellQuote(s string) string {
	if regexp.MustCompile(`^[A-Za-z0-9_./:=@%+-]+$`).MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func clone(m map[string]string) map[string]string {
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// evalIf evaluates the subset of Actions expressions that depends only on the
// matrix: matrix.X, 'strings', numbers, true/false, == != (case-insensitive,
// like Actions), ! && || and parentheses, plus success()/always() (true: we
// only care about runs where every step passes) and failure()/cancelled().
// ok is false for anything else (github.*, steps.*, needs.*, other functions).
func evalIf(src string, combo map[string]string) (v, ok bool) {
	s := strings.TrimSpace(src)
	if strings.HasPrefix(s, "${{") && strings.HasSuffix(s, "}}") {
		s = strings.TrimSpace(s[3 : len(s)-2])
	}
	toks := ifTokens.FindAllString(s, -1)
	if strings.Join(toks, "") != strings.Join(strings.Fields(s), "") {
		return false, false // characters the tokenizer doesn't know
	}
	p := &ifParser{toks: toks, combo: combo, ok: true}
	val := p.or()
	if !p.ok || p.i != len(toks) {
		return false, false
	}
	return truthy(val), true
}

var ifTokens = regexp.MustCompile(`'(?:[^']|'')*'|==|!=|&&|\|\||[!(),]|[A-Za-z_][A-Za-z0-9_.-]*\(\)|[A-Za-z_][A-Za-z0-9_.-]*|[0-9.]+`)

type ifParser struct {
	toks      []string
	i         int
	combo     map[string]string
	ok        bool
	valueOnly bool // evalValue: matrix, literals and pure functions only
}

func (p *ifParser) peek() string {
	if p.i < len(p.toks) {
		return p.toks[p.i]
	}
	return ""
}

func (p *ifParser) or() string {
	l := p.and()
	for p.peek() == "||" {
		p.i++
		ok := p.ok
		r := p.and()
		if truthy(l) {
			p.ok = ok // short-circuit: true || anything, even what we can't evaluate
		} else {
			l = r
		}
	}
	return l
}

func (p *ifParser) and() string {
	l := p.cmp()
	for p.peek() == "&&" {
		p.i++
		ok := p.ok
		r := p.cmp()
		if truthy(l) {
			l = r
		} else {
			p.ok = ok // short-circuit: false && anything (if: failure() && steps.x.outcome == ...)
		}
	}
	return l
}

func (p *ifParser) cmp() string {
	l := p.unary()
	if op := p.peek(); op == "==" || op == "!=" {
		p.i++
		r := p.unary()
		eq := strings.EqualFold(l, r)
		if (op == "==") == eq {
			return "true"
		}
		return "false"
	}
	return l
}

func (p *ifParser) unary() string {
	switch t := p.peek(); {
	case t == "!":
		p.i++
		if truthy(p.unary()) {
			return "false"
		}
		return "true"
	case t == "(":
		p.i++
		v := p.or()
		if p.peek() != ")" {
			p.ok = false
		}
		p.i++
		return v
	case strings.HasPrefix(t, "'"):
		p.i++
		return strings.ReplaceAll(t[1:len(t)-1], "''", "'")
	case strings.HasPrefix(t, "matrix."):
		p.i++
		return p.combo[strings.TrimPrefix(t, "matrix.")]
	case p.valueOnly && (strings.HasPrefix(t, "github.") || strings.HasPrefix(t, "runner.") || strings.HasSuffix(t, "()")):
		p.ok = false
		p.i++
		return ""
	case t == "github.event_name": // drafts are evidence for the pull request gate
		p.i++
		return "pull_request"
	case t == "runner.os", t == "runner.arch": // drafts are linux/amd64 tasks
		p.i++
		return map[string]string{"runner.os": "Linux", "runner.arch": "X64"}[t]
	case t == "true", t == "false", t == "success()", t == "always()":
		p.i++
		if t == "false" {
			return "false"
		}
		return "true"
	case t == "failure()", t == "cancelled()":
		p.i++
		return "false"
	case t != "" && (t[0] >= '0' && t[0] <= '9'):
		p.i++
		return t
	case p.i+1 < len(p.toks) && p.toks[p.i+1] == "(" && identifier.MatchString(t):
		return p.call(t)
	}
	p.ok = false
	p.i++
	return ""
}

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// call evaluates contains/startsWith/endsWith on strings (case-insensitive, as
// Actions does). Any other function, or an argument cinema can't know, makes
// the result unknown (short-circuiting can still decide the expression).
func (p *ifParser) call(name string) string {
	p.i += 2 // name (
	var args []string
	for p.peek() != ")" && p.peek() != "" {
		args = append(args, p.or())
		if p.peek() == "," {
			p.i++
		}
	}
	p.i++ // )
	if name == "format" && len(args) > 0 {
		return formatExpr(args[0], args[1:], &p.ok)
	}
	if len(args) != 2 {
		p.ok = false
		return ""
	}
	a, b := strings.ToLower(args[0]), strings.ToLower(args[1])
	var v bool
	switch name {
	case "contains":
		v = strings.Contains(a, b)
	case "startsWith":
		v = strings.HasPrefix(a, b)
	case "endsWith":
		v = strings.HasSuffix(a, b)
	default:
		p.ok = false
		return ""
	}
	return strconv.FormatBool(v)
}

// formatExpr is Actions' format(): {N} is the Nth argument, {{ and }} are
// literal braces. Anything else (an index out of range) is unknown.
func formatExpr(f string, args []string, ok *bool) string {
	var b strings.Builder
	for i := 0; i < len(f); i++ {
		c := f[i]
		switch {
		case (c == '{' || c == '}') && i+1 < len(f) && f[i+1] == c:
			b.WriteByte(c)
			i++
		case c == '{':
			j := strings.IndexByte(f[i:], '}')
			if j < 0 {
				*ok = false
				return ""
			}
			n, err := strconv.Atoi(f[i+1 : i+j])
			if err != nil || n < 0 || n >= len(args) {
				*ok = false
				return ""
			}
			b.WriteString(args[n])
			i += j
		case c == '}':
			*ok = false
			return ""
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func truthy(v string) bool { return v != "" && v != "false" && v != "0" }

// Printable makes workflow-derived text safe to show: control characters
// (newlines, ESC, CR, C1) and Unicode line/paragraph separators and bidi
// controls become spaces, so a crafted workflow can't drive the terminal or
// break out of a YAML comment into the draft.
func Printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' || unicode.Is(unicode.Bidi_Control, r) {
			return ' '
		}
		return r
	}, s)
}

// firstFile is the first of names that exists as a regular file in root ("" if none).
func firstFile(root string, names ...string) string {
	for _, n := range names {
		if fi, err := os.Lstat(filepath.Join(root, n)); root != "" && err == nil && fi.Mode().IsRegular() {
			return n
		}
	}
	return ""
}

// containerExtras names container: settings cinema doesn't reproduce yet
// (options, volumes, ports, credentials, env); "" if there are none.
func containerExtras(n *yaml.Node) string {
	if n.Kind != yaml.MappingNode {
		return ""
	}
	var extra []string
	for i := 0; i+1 < len(n.Content); i += 2 {
		if k := n.Content[i].Value; k != "image" {
			extra = append(extra, k)
		}
	}
	if len(extra) == 0 {
		return ""
	}
	return "; with " + strings.Join(extra, ", ")
}

// Service is a job's service container as a task can run it.
type Service struct {
	Image   string            `json:"image" yaml:"image"`
	Env     map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	Ports   []string          `json:"ports,omitempty" yaml:"ports,omitempty,flow"`
	Options string            `json:"options,omitempty" yaml:"options,omitempty"`
}

// service translates one services: entry, or says why it can't.
func service(n *yaml.Node, sub func(string) string) (Service, string) {
	var raw struct {
		Image       string         `yaml:"image"`
		Env         map[string]any `yaml:"env"`
		Ports       []any          `yaml:"ports"`
		Options     string         `yaml:"options"`
		Credentials any            `yaml:"credentials"`
		Volumes     any            `yaml:"volumes"`
	}
	if n.Kind == yaml.ScalarNode {
		raw.Image = n.Value
	} else if err := n.Decode(&raw); err != nil {
		return Service{}, "can't be read"
	}
	switch {
	case raw.Credentials != nil:
		return Service{}, "needs registry credentials"
	case raw.Volumes != nil:
		return Service{}, "mounts volumes"
	}
	svc := Service{Image: sub(raw.Image), Options: sub(raw.Options)}
	if svc.Image == "" || strings.Contains(svc.Image+svc.Options, "${{") {
		return Service{}, "has an image or options cinema can't evaluate"
	}
	if err := task.CheckServiceOptions(svc.Options); err != nil {
		return Service{}, "has options cinema won't pass to docker (" + err.Error() + ")"
	}
	for k, v := range raw.Env {
		if svc.Env == nil {
			svc.Env = map[string]string{}
		}
		val := sub(str(v))
		if strings.Contains(val, "${{") {
			return Service{}, "has env " + k + " cinema can't evaluate"
		}
		svc.Env[k] = val
	}
	for _, p := range raw.Ports {
		svc.Ports = append(svc.Ports, sub(fmt.Sprint(p)))
	}
	return svc, ""
}

// originalIf undoes cinema setup's skip-in-place condition on a job that
// needs: cinema,
//
//	${{ !cancelled() [&& needs.cinema.outputs.skip_X != 'true'] [&& needs.Y.result == 'success']... [&& (ORIG)] }}
//
// so the job is analyzed as written.
func originalIf(s string, cinema bool) string {
	e := strings.TrimSpace(s)
	if !cinema || !strings.HasPrefix(e, "${{ !cancelled()") {
		return s
	}
	e = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(e, "${{"), "}}"))
	if i := strings.Index(e, " && ("); i >= 0 && strings.HasSuffix(e, ")") {
		return e[i+5 : len(e)-1]
	}
	return ""
}

func needsCinema(n *yaml.Node) bool {
	if n.Kind == yaml.ScalarNode {
		return n.Value == "cinema"
	}
	return slices.ContainsFunc(n.Content, func(c *yaml.Node) bool { return c.Value == "cinema" })
}

// LegDigest identifies the job leg a draft task stands in for: its workflow,
// job, runner, matrix values and translated steps (with runtime, container and
// services). verify recomputes it from the workflow under test, so a task can
// only skip the exact job it was drafted from: a changed step, if:, runner or
// a new matrix leg means the job runs.
func (j Job) LegDigest() string {
	if j.Proposal == nil {
		return ""
	}
	b, _ := json.Marshal(struct {
		Workflow, Job, RunsOn string
		Combo                 map[string]string
		Proposal              Proposal
	}{j.Workflow, j.ID, j.RunsOn, j.Combo, *j.Proposal})
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// onePin keeps a task to one version of a tool: a second setup step (a rebuilt
// artifact's producer, or the job itself) that wants another can't share its
// runtime.
func onePin(had, v, tool string, review func(string)) string {
	if had != "" && v != "" && v != had {
		review(tool + " " + had + " and " + v + " in one task (the job sets " + tool + " up twice, or a rebuilt artifact's producer uses another version): cinema provisions one")
		return had
	}
	return cmp.Or(v, had)
}

var eventRef = regexp.MustCompile(`github\.(event_name|ref|ref_name|ref_type|base_ref|head_ref)\b|\$\{?GITHUB_(EVENT_NAME|REF|REF_NAME|REF_TYPE|BASE_REF|HEAD_REF)\b`)

// eventDependent: the job's conditions or scripts read which event or ref
// triggered it, so its work can differ between a PR and a push.
func eventDependent(j *job) bool {
	if eventRef.MatchString(j.If) {
		return true
	}
	for _, v := range j.Env {
		if eventRef.MatchString(str(v)) {
			return true
		}
	}
	for _, s := range j.Steps {
		if eventRef.MatchString(s.If) || eventRef.MatchString(s.Run) {
			return true
		}
		for _, v := range s.Env {
			if eventRef.MatchString(str(v)) {
				return true
			}
		}
		for _, v := range s.With {
			if eventRef.MatchString(str(v)) {
				return true
			}
		}
	}
	return false
}

const floatingNote = "\x00floating:" // internal marker in Notes, turned into Proposal.Floating
