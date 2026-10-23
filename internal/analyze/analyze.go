// Package analyze reads a repo's GitHub Actions workflows and proposes CInema
// tasks for the jobs a Linux evidence machine could run (plan §16 P3: analyze).
// Deterministic and advisory: it never writes tasks.yaml and never claims a
// proposal is equivalent to the job. `cinema run` on the proposal is the check.
package analyze

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"unicode"

	"go.yaml.in/yaml/v3"
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
	RunsOn   string            `json:"runs_on"`
	Verdict  string            `json:"verdict"`
	Reasons  []string          `json:"reasons,omitempty"`
	Notes    []string          `json:"notes,omitempty"` // decisions made for the draft (e.g. a resolved version)
	Proposal *Proposal         `json:"proposal,omitempty"`
}

// Proposal is a suggested task: the job's run steps, in order.
type Proposal struct {
	ID   string `json:"id"`
	Go   string `json:"go,omitempty"`   // exact versions only (runtime:)
	Node string `json:"node,omitempty"` // exact versions only (runtime:)
	Body string `json:"script"`         // bash script; each step in its own subshell
	// Outputs: paths the job uploads as artifacts, so it writes them (outputs:).
	Outputs []string `json:"outputs,omitempty"`
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
	RunsOn    yaml.Node      `yaml:"runs-on"`
	If        string         `yaml:"if"`
	Uses      string         `yaml:"uses"` // reusable workflow
	Container yaml.Node      `yaml:"container"`
	Services  map[string]any `yaml:"services"`
	Env       map[string]any `yaml:"env"`
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
	for _, j := range w.Jobs {
		for _, s := range j.Steps {
			if cinemaGate.MatchString(s.Run) {
				return nil, nil // the CInema gate workflow itself
			}
		}
	}
	for _, id := range sortedKeys(w.Jobs) {
		j := w.Jobs[id]
		if j.Defaults.Run.WorkingDirectory == "" {
			// Lab: prettier's release-script job ran the whole suite from the root.
			j.Defaults = w.Defaults
		}
		combos, err := expand(&j.Strategy.Matrix)
		if err != nil {
			out = append(out, Job{Workflow: name, ID: id, Verdict: Review, Reasons: []string{"matrix: " + err.Error()}})
			continue
		}
		for _, c := range combos {
			out = append(out, analyzeJob(root, name, id, j, w.Env, c))
		}
	}
	return out, nil
}

func analyzeJob(root, wf, id string, j *job, wfEnv map[string]any, combo map[string]string) Job {
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
	case strings.Contains(os, "${{"):
		review("runs-on is an expression: " + r.RunsOn)
	case !strings.Contains(os, "ubuntu") && !strings.Contains(os, "linux"):
		skip("runs on " + r.RunsOn + " (not a GitHub Linux runner)")
	}
	if !j.Container.IsZero() {
		review("runs in a container (" + sub(scalarOrMap(&j.Container, "image")) + "): run the steps inside it, e.g. docker run, and fingerprint the image")
	}
	if len(j.Services) > 0 {
		skip("needs service containers (" + strings.Join(sortedKeys(j.Services), ", ") + ")")
	}

	p := &Proposal{ID: taskID(id, combo)}
	var body []string
	plumbing := false // a step writes $GITHUB_ENV & co.: emulate them between steps
	env := literalEnv(wfEnv, combo, review)
	env = append(env, literalEnv(j.Env, combo, review)...)
	for _, s := range j.Steps {
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
		case action == "actions/download-artifact",
			strings.Contains(action, "artifact") && !strings.Contains(action, "upload") && !strings.Contains(action, "save"):
			skip("needs artifacts from another job (download-artifact)")
		case action == "actions/setup-go":
			p.Go = exactVersion(root, sub(str(s.With["go-version"])), `^1\.[0-9]+\.[0-9]+$`, "Go", str(s.With["go-version-file"]), review, note)
		case action == "actions/setup-node":
			p.Node = exactVersion(root, sub(str(s.With["node-version"])), `^[0-9]+\.[0-9]+\.[0-9]+$`, "Node", str(s.With["node-version-file"]), review, note)
		case action == "pnpm/action-setup", action == "oven-sh/setup-bun", action == "actions/setup-python", action == "actions/setup-java":
			review("uses " + action + ": its tool version must be pinned and fingerprinted (toolchain:)")
		case strings.HasPrefix(action, "github/codeql-action"):
			skip("CodeQL analysis uploads results; not a pass/fail check")
		default:
			review("uses action " + uses + ": no local equivalent known; reproduce it with commands")
		}
		if s.Run == "" {
			continue
		}
		run := sub(s.Run)
		if strings.Contains(run, "secrets.") || anyEnvMentions(s.Env, "secrets.") {
			skip("a step uses secrets")
		}
		if publishes.MatchString(run) && !strings.Contains(run, "--dry-run") {
			skip("a step publishes or releases (" + publishes.FindString(run) + "): not a check")
		}
		if strings.Contains(run, "github.event.") || anyEnvMentions(s.Env, "github.event.") {
			skip("a step reads the triggering event (github.event.*, e.g. the PR description): depends on the PR, not the commit")
		}
		if ghFiles.MatchString(run) {
			plumbing = true
		}
		if sudo.MatchString(run) {
			review("a step uses sudo (installs or changes the machine): make it a prerequisite of the evidence machine, not part of the task")
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
	// Steps that use Go or Node with no setup action (a container job, the
	// runner's preinstalled toolchain) would leave the task with no environment
	// fingerprint: its receipts would match whatever is installed. Pin from the
	// repo's version file, as setup-go/setup-node would (lab: prometheus).
	all := strings.Join(body, "\n")
	if p.Go == "" && usesGo.MatchString(all) {
		if f := firstFile(root, ".go-version", "go.mod"); f != "" {
			if p.Go = exactVersion(root, "", `^1\.[0-9]+\.[0-9]+$`, "Go", f, review, note); p.Go != "" {
				note("Go isn't set up by the job (container or preinstalled); pinned from " + f)
			}
		}
	}
	if p.Node == "" && usesNode.MatchString(all) {
		if f := firstFile(root, ".nvmrc", ".node-version"); f != "" {
			if p.Node = exactVersion(root, "", `^[0-9]+\.[0-9]+\.[0-9]+$`, "Node", f, review, note); p.Node != "" {
				note("Node isn't set up by the job (container or preinstalled); pinned from " + f)
			}
		}
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
	for _, inc := range include {
		matched := false
		for _, c := range combos {
			if agrees(inc, c) {
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

// agrees: inc's keys that c already has (original matrix keys) match.
func agrees(inc, c map[string]string) bool {
	any := false
	for k, v := range inc {
		if cv, ok := c[k]; ok {
			if cv != v {
				return false
			}
			any = true
		}
	}
	return any
}

var (
	statusFn = regexp.MustCompile(`\b(failure|cancelled)\(\)`)
	ghFiles  = regexp.MustCompile(`\$\{?GITHUB_(ENV|PATH|OUTPUT)\b`)
	sudo     = regexp.MustCompile(`(^|[\s;&|(])sudo\s`)
	// Lab: vite's preview job ran `pkg-pr-new publish` (it refused outside Actions).
	publishes   = regexp.MustCompile(`\b(npm|pnpm|yarn|bun|pkg-pr-new|changeset|cargo|gem)(@[\w.^~-]+)? publish\b|\bgoreleaser release\b|\bdocker push\b|\btwine upload\b|\bgh release (create|upload)\b`)
	usesGo      = regexp.MustCompile(`(^|[\s;&|(])(go (build|test|vet|run|install|generate|mod|tool|list)|make)\b`)
	usesNode    = regexp.MustCompile(`(^|[\s;&|(])(node|npm|npx|pnpm|yarn|corepack)\b`)
	pipeToShell = regexp.MustCompile(`(curl|wget)\b[^|\n]*\|\s*(sudo\s+)?(ba)?sh\b`)
	floating    = regexp.MustCompile(`@latest\b|/HEAD/|/(master|main)/[^\s]*\.(sh|py)\b`)
)

var cinemaGate = regexp.MustCompile(`(^|[\s/])cinema verify\b`)

var matrixRef = regexp.MustCompile(`\$\{\{\s*matrix\.([A-Za-z0-9_.-]+)\s*\}\}`)

// substitute replaces ${{ matrix.X }}. Like Actions, a key this combination
// doesn't define (set only by some include entries) is the empty string.
func substitute(s string, combo map[string]string) string {
	if combo == nil {
		return s
	}
	return matrixRef.ReplaceAllStringFunc(s, func(m string) string {
		return combo[matrixRef.FindStringSubmatch(m)[1]]
	})
}

func exactVersion(root, v, pattern, tool, file string, review, note func(string)) string {
	if v == "" && file != "" && root != "" && filepath.IsLocal(filepath.FromSlash(file)) {
		v = versionFromFile(filepath.Join(root, filepath.FromSlash(file)))
	}
	exact := regexp.MustCompile(pattern)
	if exact.MatchString(v) {
		return v
	}
	if v != "" && !strings.Contains(v, "${{") {
		// What setup-go/setup-node would pick today, pinned so evidence and gate agree.
		if got, err := resolve(tool, v); err == nil && exact.MatchString(got) {
			note(fmt.Sprintf("%s %q resolved to %s, the newest matching release today: CI will move on; this pin won't", tool, v, got))
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

var ifTokens = regexp.MustCompile(`'(?:[^']|'')*'|==|!=|&&|\|\||[!()]|[A-Za-z_][A-Za-z0-9_.-]*\(\)|[A-Za-z_][A-Za-z0-9_.-]*|[0-9.]+`)

type ifParser struct {
	toks  []string
	i     int
	combo map[string]string
	ok    bool
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
	}
	p.ok = false
	p.i++
	return ""
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
