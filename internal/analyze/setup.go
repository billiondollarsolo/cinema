package analyze

import (
	"fmt"
	"regexp"
	"strings"
)

// SetupCtx is one setup action step (actions/setup-python, …) being
// translated: a language file registers a SetupAction for it, which pins the
// runtime cinema provisions and adds any steps the action's side effects stand
// for (setup-go puts GOPATH/bin on PATH).
type SetupCtx struct {
	Root  string                    // the repo, for version files ("" when unknown)
	Ref   string                    // the action's ref (dtolnay/rust-toolchain@stable: "stable")
	With  func(key string) string   // the step's with: value, matrix-substituted
	has   func(key string) bool     // the input is given (even empty)
	Env   map[string]string         // the job's resolved env (workflow, job), for ${{ env.X }}
	pin   func(runtime, ver string) // sets the proposal's runtime pin (one version per task)
	step  func(run, dir string)     // adds a step right after this one
	probe func(argv []string)       // adds a version command to the environment fingerprint
	// Review: the draft has an issue a person must resolve (an unpinnable
	// version, an option cinema doesn't reproduce). Note: informational.
	Review, Note func(string)
}

// Has reports whether the step gives input key at all (an empty value counts).
func (c *SetupCtx) Has(key string) bool { return c.has != nil && c.has(key) }

// Pin pins runtime (a task.RuntimeKind name) at an exact version.
func (c *SetupCtx) Pin(runtime, ver string) { c.pin(runtime, ver) }

// Step adds a shell step after the setup step (in the repo root if dir is "").
func (c *SetupCtx) Step(run, dir string) { c.step(run, dir) }

// Exact turns a with: spec (or a version file the workflow names) into an
// exact version: as written if exact, else resolved to the newest matching
// release today through the runtime's registered resolver, recorded as
// floating: so CI resolves it again on every run. "" (with a review) if it
// can't be pinned.
func (c *SetupCtx) Exact(runtime, spec, file string, exact *regexp.Regexp) string {
	tool := runtime
	if t, ok := map[string]string{"go": "Go", "node": "Node"}[runtime]; ok {
		tool = t // their release lists' names
	}
	return exactVersion(c.Root, spec, exact.String(), tool, file, c.Review, c.Note)
}

// Probe fingerprints a tool the task doesn't pin by its version command
// ([python3, --version]). It runs where the task runs: in the runner image
// here, on the runner in CI.
func (c *SetupCtx) Probe(argv ...string) { c.probe(argv) }

// SetupAction translates one setup action's step.
type SetupAction func(c *SetupCtx)

// withoutSetup is, by runtime, what a job that uses it gets when no setup
// action sets it up: the runner's preinstalled toolchain, or a version file a
// tool reads itself (rustup, Go's GOTOOLCHAIN). bun, deno and uv have none:
// GitHub's Ubuntu images don't carry them, so such a step fails in CI too.
var withoutSetup = map[string]noSetup{}

type noSetup struct {
	uses    *regexp.Regexp
	actions []string
	f       SetupAction
}

// RegisterWithoutSetup: when steps matching uses run runtime and nothing set
// it up (no pin, none of actions ran), f pins what CI's job really gets,
// probes it, or reviews; else the task's receipts would match whatever is
// installed. f can't add steps (c.Step): they're translated by then.
func RegisterWithoutSetup(runtime string, uses *regexp.Regexp, f SetupAction, actions ...string) {
	if withoutSetup[runtime].f != nil {
		panic("runtime " + runtime + " without setup registered twice")
	}
	for i, a := range actions {
		actions[i] = strings.ToLower(a)
	}
	withoutSetup[runtime] = noSetup{uses, actions, f}
}

var setupActions = map[string]SetupAction{}

// RegisterSetupAction maps an action ("actions/setup-python", owner/repo) to
// its translation (from an init function). Matched case-insensitively, as
// GitHub resolves owner/repo.
func RegisterSetupAction(action string, f SetupAction) {
	action = strings.ToLower(action)
	if setupActions[action] != nil {
		panic("setup action " + action + " registered twice")
	}
	setupActions[action] = f
}

// Release is one published version of a runtime, for Resolve.
type Release struct {
	Version string // exact ("3.12.7")
	LTS     bool
	Stable  bool // not a prerelease
}

var lister = map[string]func() ([]Release, error){}

// RegisterReleases gives Resolve the releases of runtime (a task.RuntimeKind
// name), newest first, so floating specs ("3.12", "3.x", "stable") resolve.
func RegisterReleases(runtime string, f func() ([]Release, error)) {
	if lister[runtime] != nil {
		panic(fmt.Sprintf("releases of %s registered twice", runtime))
	}
	lister[runtime] = f
}
