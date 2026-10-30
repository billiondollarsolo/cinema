// Package task parses .cinema/tasks.yaml strictly, matches runs to tasks by
// exact argv + cwd, and computes the digests that make up an action key (plan §6).
package task

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/billiondollarsolo/cinema/internal/snapshot"
)

const (
	ConfigPath    = ".cinema/tasks.yaml"
	maxConfigSize = 1 << 20
	PlatformAny   = "any"
)

var ErrNoConfig = errors.New("task: no " + ConfigPath)

type Config struct {
	SchemaVersion int              `yaml:"schema_version"`
	Tasks         map[string]*Task `yaml:"tasks"`
	Digest        string           `yaml:"-"` // sha256 of the raw bytes; what `cinema trust` approves
}

// RunsCommands reports whether computing t's environment executes commands
// from the config (toolchain cmd entries). Those require a trusted config.
func (t *Task) RunsCommands() bool {
	unpinned := func(img string) bool { return img != "" && !strings.Contains(img, "@sha256:") } // resolving it runs docker pull
	if t.Runtime.set() || unpinned(t.Container) {
		return true
	}
	for _, svc := range t.Services {
		if unpinned(svc.Image) {
			return true
		}
	}
	return slices.ContainsFunc(t.Toolchain, func(tc Toolchain) bool {
		return len(tc.Cmd) > 0 || unpinned(tc.Image)
	})
}

type Task struct {
	ID              string      `yaml:"-"`
	Argv            []string    `yaml:"argv"`
	Cwd             string      `yaml:"cwd"`      // repo-relative; default "."
	Platform        string      `yaml:"platform"` // "goos/goarch" or "any"; required, never inferred
	Toolchain       []Toolchain `yaml:"toolchain"`
	Env             []string    `yaml:"env"`          // allowlisted env var names that are inputs
	ExtraInputs     []string    `yaml:"extra_inputs"` // ignored files the task reads
	Outputs         []string    `yaml:"outputs"`      // files/dirs the task writes; left out of its fingerprint (an explicit claim, like platform: any)
	VCSContext      string      `yaml:"vcs_context"`  // "none" (default) | "exact"
	AcceptToolCache bool        `yaml:"accept_tool_cache"`
	// Exclusive: never run alongside other tasks locally (-j), e.g. tests that
	// bind fixed ports or create a fixed docker network. Scheduling only: not
	// part of the contract digest.
	Exclusive bool `yaml:"exclusive"`
	// Event: the covered job's work depends on the triggering event, so a
	// receipt only counts for this event (pull_request | push | merge_group).
	// Empty: any. Metadata, read from the base branch like the task list.
	Event string `yaml:"event"`
	// Floating: the workflow's version specs the runtime: pins came from
	// ({go: stable, node: "lts/*"}). The gate resolves them again on every run
	// and runs the job if today's release isn't the pin (plan §11.2). Metadata.
	Floating map[string]string `yaml:"floating"`
	// Graduated: the stand-in agreed with CI for exactly this job definition
	// (plan §11.5). Without it, or for another job_digest, the covered job runs
	// in CI whatever the receipts say (shadow mode). Written by cinema graduate,
	// read from the base branch. Metadata, not part of the contract.
	Graduated *Graduation `yaml:"graduated"`
	// CPUs caps how many CPUs the task sees locally (Linux: CPU affinity), to
	// match the runner it replaces: tools size worker pools by core count, and
	// some suites deadlock or time out on a bigger box (lab: prettier's jest on
	// 16 cores). Scheduling only: not part of the contract digest.
	CPUs int `yaml:"cpus"`
	// Memory is the runner's memory, "16g" (a sandbox's limit). With cpus:, the
	// runner size a sandboxed run reports in its environment, which the CI runner
	// measures on its side (plan §11.2). Default 16g, cpus 4: ubuntu-latest on
	// public repos.
	Memory string `yaml:"memory"`
	// Container runs the task inside this image, as a GitHub container job does
	// (root inside it; files are handed back afterwards). The image's resolved
	// digest is part of the environment fingerprint, and the run uses exactly
	// that digest (container.go).
	Container string `yaml:"container"`
	// Services are sidecar containers started for the run (services.go), like a
	// GitHub job's services:. Their image digests are fingerprinted. Tasks with
	// services run alone under -j (fixed host ports).
	Services map[string]Service `yaml:"services"`
	// Covers names the CI job this task stands in for ("ci.yml#test"): the job
	// skips itself when the task is reused (verify --skip-outputs). Metadata,
	// not part of the contract.
	Covers string `yaml:"covers"`
	// Leg is the matrix leg of the covered job this task is (its matrix's own
	// keys → values): a reused leg is excluded from the job's matrix. Metadata.
	Leg map[string]string `yaml:"leg"`
	// JobDigest is the covered job leg as drafted (analyze.Job.LegDigest): the
	// job skips only if the workflow under test still has exactly that leg.
	JobDigest string `yaml:"job_digest"`
	// Runtime pins Go/Node versions that cinema provisions for the task and its
	// probes, on the evidence machine and in the gate alike (runtime.go).
	Runtime Runtime `yaml:"runtime"`
	// Timeout stops a run that takes longer (a deadlock is otherwise forever) and
	// counts it as failed. Optional; not part of the contract digest.
	Timeout    time.Duration `yaml:"-"`
	RawTimeout string        `yaml:"timeout"`
	Reuse      Reuse         `yaml:"reuse"`
}

// Toolchain is one environment input: a command whose output identifies a tool
// version, a file whose content does, or a container image (by resolved digest,
// so a floating tag that moves is a mismatch, not a silent false green).
// Exactly one of Cmd / File / Image.
type Toolchain struct {
	Cmd   []string `yaml:"cmd"`
	File  string   `yaml:"file"`
	Image string   `yaml:"image"` // container image; fingerprinted by its resolved digest
}

// Reuse is acceptance policy. It is NOT part of the contract digest: a stricter
// policy rejects old evidence without changing what the work was (plan §6.2).
type Reuse struct {
	Allowed bool          `yaml:"allowed"`
	MaxAge  time.Duration `yaml:"-"`
	RawAge  string        `yaml:"max_age"`
	// Capture: which runs count for CI. "clean" (default): only clean-checkout
	// runs (cinema run --commit), so ignored files can't leak in (ADR 0002).
	// "workspace": also working-tree runs; an explicit claim nothing ignored matters.
	Capture string `yaml:"capture"`
}

// AcceptsCapture reports whether a run captured this way may be CI evidence.
func (r Reuse) AcceptsCapture(capture string) bool {
	return capture == "clean" || capture == "clean-checkout" || r.Capture == "workspace"
}

var (
	idRE       = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	platformRE = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9]+$`)
	envNameRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	coversRE   = regexp.MustCompile(`^[A-Za-z0-9._-]+\.ya?ml#[A-Za-z_][A-Za-z0-9_-]*$`)
	imageRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(:[0-9]+)?(/[a-z0-9][a-z0-9._-]*)*(:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?(@sha256:[0-9a-f]{64})?$`)
)

// Load reads and validates root/.cinema/tasks.yaml.
func Load(root string) (*Config, error) { return LoadFile(filepath.Join(root, ConfigPath)) }

// LoadFile reads and validates a config at path.
func LoadFile(path string) (*Config, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoConfig
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxConfigSize+1))
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse decodes and validates a config. Unknown fields, duplicate keys,
// multiple documents and anything ambiguous are errors: fix the config, don't guess.
func Parse(b []byte) (*Config, error) {
	if len(b) > maxConfigSize {
		return nil, fmt.Errorf("task: config larger than %d bytes", maxConfigSize)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("task: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("task: config must be a single YAML document")
	}
	if c.SchemaVersion != 1 {
		return nil, fmt.Errorf("task: schema_version must be 1, got %d", c.SchemaVersion)
	}
	if len(c.Tasks) == 0 {
		return nil, errors.New("task: no tasks defined")
	}
	for id, t := range c.Tasks {
		if t == nil {
			return nil, fmt.Errorf("task %q: empty definition", id)
		}
		t.ID = id
		if err := t.validate(); err != nil {
			return nil, fmt.Errorf("task %q: %w", id, err)
		}
	}
	h := sha256.Sum256(b)
	c.Digest = "sha256:" + hex.EncodeToString(h[:])
	return &c, nil
}

func (t *Task) validate() error {
	if !idRE.MatchString(t.ID) {
		return errors.New("id must match " + idRE.String())
	}
	if len(t.Argv) == 0 || t.Argv[0] == "" {
		return errors.New("argv is required")
	}
	if t.Cwd == "" || t.Cwd == "." {
		t.Cwd = "."
	} else {
		c, err := snapshot.CleanRel(t.Cwd)
		if err != nil {
			return fmt.Errorf("cwd: %w", err)
		}
		t.Cwd = c
	}
	if t.Platform != PlatformAny && !platformRE.MatchString(t.Platform) {
		return errors.New(`platform is required: "goos/goarch" (e.g. linux/amd64) or "any"`)
	}
	for i, tc := range t.Toolchain {
		set := 0
		for _, ok := range []bool{len(tc.Cmd) > 0, tc.File != "", tc.Image != ""} {
			if ok {
				set++
			}
		}
		if set != 1 {
			return fmt.Errorf("toolchain[%d]: exactly one of cmd, file or image", i)
		}
		switch {
		case tc.Image != "":
			if !imageRE.MatchString(tc.Image) {
				return fmt.Errorf("toolchain[%d]: invalid image reference %q", i, tc.Image)
			}
		case len(tc.Cmd) > 0:
			if err := validateProbe(tc.Cmd); err != nil {
				return fmt.Errorf("toolchain[%d]: %w", i, err)
			}
		default:
			c, err := snapshot.CleanRel(tc.File)
			if err != nil {
				return fmt.Errorf("toolchain[%d]: %w", i, err)
			}
			t.Toolchain[i].File = c
		}
	}
	for _, e := range t.Env {
		if !envNameRE.MatchString(e) {
			return fmt.Errorf("env: invalid name %q", e)
		}
	}
	for i, p := range t.ExtraInputs {
		c, err := snapshot.CleanRel(p)
		if err != nil {
			return fmt.Errorf("extra_inputs: %w", err)
		}
		t.ExtraInputs[i] = c
	}
	for i, p := range t.Outputs {
		c, err := snapshot.CleanRel(p)
		if err != nil {
			return fmt.Errorf("outputs: %w", err)
		}
		if snapshot.IsUnder(c, t.ExtraInputs) || slices.ContainsFunc(t.ExtraInputs, func(x string) bool { return snapshot.IsUnder(x, []string{c}) }) {
			return fmt.Errorf("outputs: %q overlaps extra_inputs", c)
		}
		t.Outputs[i] = c
	}
	if t.Container != "" && !imageRE.MatchString(t.Container) {
		return fmt.Errorf("container: invalid image reference %q", t.Container)
	}
	if t.Container != "" {
		if t.Runtime.set() {
			return errors.New("container: and runtime: can't be combined: the image provides the toolchain")
		}
		for _, tc := range t.Toolchain {
			if len(tc.Cmd) > 0 {
				return errors.New("container: tasks can't have toolchain cmd probes (they'd run on the host); the image digest fingerprints the container")
			}
		}
	}
	if t.Covers != "" && !coversRE.MatchString(t.Covers) {
		return fmt.Errorf("covers must look like ci.yml#job, got %q", t.Covers)
	}
	if err := validateServices(t.Services); err != nil {
		return err
	}
	if err := t.Runtime.validate(); err != nil {
		return err
	}
	switch t.VCSContext {
	case "":
		t.VCSContext = "none"
	case "none", "exact":
	default:
		return errors.New(`vcs_context must be "none" or "exact"`)
	}
	if !t.AcceptToolCache && isGoTest(t.Argv) && !slices.ContainsFunc(t.Argv, disablesTestCache) {
		return errors.New("go test may replay cached results: add -count=1 to argv, or set accept_tool_cache: true")
	}
	switch t.Reuse.Capture {
	case "":
		t.Reuse.Capture = "clean"
	case "clean", "workspace":
	default:
		return errors.New(`reuse.capture must be "clean" or "workspace"`)
	}
	for k := range t.Floating {
		if k != "go" && k != "node" {
			return fmt.Errorf("floating: only go and node, got %q", k)
		}
	}
	switch t.Event {
	case "", "pull_request", "push", "merge_group":
	default:
		return fmt.Errorf("event must be pull_request, push or merge_group, got %q", t.Event)
	}
	if t.Memory != "" && t.MemoryGB() == 0 {
		return fmt.Errorf("memory must be whole gigabytes like 16g, got %q", t.Memory)
	}
	if t.CPUs < 0 {
		return errors.New("cpus must be a positive number")
	}
	if t.RawTimeout != "" {
		d, err := time.ParseDuration(t.RawTimeout)
		if err != nil || d <= 0 {
			return errors.New("timeout must be a duration like 30m")
		}
		t.Timeout = d
	}
	if t.Reuse.Allowed {
		d, err := time.ParseDuration(t.Reuse.RawAge)
		if err != nil || d <= 0 {
			return errors.New("reuse.max_age is required when reuse is allowed (e.g. 72h)")
		}
		t.Reuse.MaxAge = d
	}
	return nil
}

// Toolchain commands identify tool versions and must not run repo code. Fail
// closed with an allowlist instead of a denylist: a bare command name from PATH
// plus exactly one version invocation. Anything else (scripts, interpreters
// with arguments, wrappers) can't be expressed. `cinema trust` still gates
// running them at all; a tool that needs more belongs in a `file:` entry.
var (
	probeNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
	// Only `--version`: `version` is a build target for make/just/rake, `npm
	// version` edits package.json, and `-v` means a verbose BUILD for ninja.
	// Exact exceptions where the other form is the tool's own builtin:
	probeExceptions = map[string]string{"go": "version", "java": "-version"}
)

func validateProbe(cmd []string) error {
	if len(cmd) != 2 || !probeNameRE.MatchString(cmd[0]) {
		return fmt.Errorf("toolchain cmd must be [tool, --version] (or [go, version], [java, -version]); got %q", cmd)
	}
	// Two-value lookup: a missing key yields "", which must not match an empty argument.
	if exc, ok := probeExceptions[cmd[0]]; cmd[1] == "--version" || (ok && cmd[1] == exc) {
		return nil
	}
	return fmt.Errorf("toolchain cmd must be [tool, --version] (or [go, version], [java, -version]); got %q", cmd)
}

func isGoTest(argv []string) bool { return len(argv) >= 2 && argv[0] == "go" && argv[1] == "test" }

// disablesTestCache: go test caches only when every test flag is in a small
// cacheable set (go help test); these known flags are outside it. Unknown flags
// count as cacheable, so the worst case is asking for a needless -count=1.
func disablesTestCache(arg string) bool {
	if !strings.HasPrefix(arg, "-") {
		return false
	}
	name, _, _ := strings.Cut(strings.TrimLeft(arg, "-"), "=")
	switch strings.TrimPrefix(name, "test.") {
	case "count", "fuzz", "bench", "cpuprofile", "memprofile", "blockprofile", "mutexprofile", "trace":
		return true
	}
	return false
}

// Match returns the task whose argv and cwd equal the run's exactly (plan §6.3).
// A narrowed filter, an extra flag or another directory is different work.
// Tasks can share an argv (one per runtime version); then a bare command can't
// say which it was, and Match returns nil: history only, never a guess.
func (c *Config) Match(argv []string, cwdRel string) *Task {
	if cwdRel == "" {
		cwdRel = "."
	}
	var found *Task
	for _, t := range c.Tasks {
		if t.Cwd == cwdRel && slices.Equal(t.Argv, argv) {
			if found != nil {
				return nil
			}
			found = t
		}
	}
	return found
}

// ContractDigest identifies what the task does. It includes the resolved
// script body for `npm run X` / `pnpm run X`, so editing package.json
// invalidates old runs (plan §6.1, ACC-017).
func (t *Task) ContractDigest(root string) (string, error) {
	script, err := t.resolvedScript(root)
	if err != nil {
		return "", err
	}
	e := newEnc("cinema.task.v1")
	e.str(t.ID)
	e.list(t.Argv)
	e.str(t.Cwd)
	e.str(t.Platform)
	e.u64(uint64(len(t.Toolchain)))
	for _, tc := range t.Toolchain {
		e.list(tc.Cmd)
		e.str(tc.File)
		e.str(tc.Image)
	}
	e.list(sorted(t.Env))
	e.list(sorted(t.ExtraInputs))
	e.list(sorted(t.Outputs))
	e.str(t.VCSContext)
	e.bool(t.AcceptToolCache)
	e.str(t.Runtime.Go)
	e.str(t.Runtime.Node)
	e.str(script)
	if t.Container != "" { // only when set: existing contracts keep their digests
		e.str("container")
		e.str(t.Container)
	}
	for _, n := range sortedServices(t.Services) { // likewise only when set
		svc := t.Services[n]
		e.str("service")
		e.str(n)
		e.str(svc.Image)
		for _, k := range sortedKeys(svc.Env) {
			e.str(k + "=" + svc.Env[k])
		}
		e.list(svc.Ports)
		e.str(svc.Options)
	}
	return e.sum(), nil
}

// resolvedScript returns the package.json script body a package-manager run
// delegates to, or "" for other commands. ponytail: `npm|pnpm|yarn run X` only;
// yarn's bare `yarn X` form isn't recognized (cinema init writes `yarn run X`).
func (t *Task) resolvedScript(root string) (string, error) {
	a := t.Argv
	if len(a) < 3 || (a[0] != "npm" && a[0] != "pnpm" && a[0] != "yarn") || (a[1] != "run" && a[1] != "run-script") {
		return "", nil
	}
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(t.Cwd), "package.json"))
	if err != nil {
		return "", fmt.Errorf("task %q: %w", t.ID, err)
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		return "", fmt.Errorf("task %q: package.json: %w", t.ID, err)
	}
	s, ok := pkg.Scripts[a[2]]
	if !ok {
		return "", fmt.Errorf("task %q: package.json has no script %q", t.ID, a[2])
	}
	return s, nil
}

// ActionKey binds the work, the bytes, the environment and (optionally) the
// commit (plan §6.2). Acceptance policy is deliberately not an input.
func ActionKey(contractDigest, sourceDigest, envDigest, vcs string) string {
	e := newEnc("cinema.action.v1")
	e.str(contractDigest)
	e.str(sourceDigest)
	e.str(envDigest)
	e.str(vcs)
	return e.sum()
}

// enc is the domain-separated, length-prefixed encoding every digest uses (plan §3.1).
type enc struct{ b bytes.Buffer }

func newEnc(domain string) *enc { e := &enc{}; e.str(domain); return e }

func (e *enc) u64(n uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], n)
	e.b.Write(b[:])
}
func (e *enc) str(s string) { e.u64(uint64(len(s))); e.b.WriteString(s) }
func (e *enc) list(l []string) {
	e.u64(uint64(len(l)))
	for _, s := range l {
		e.str(s)
	}
}
func (e *enc) bool(v bool) {
	if v {
		e.str("1")
	} else {
		e.str("0")
	}
}
func (e *enc) sum() string {
	h := sha256.Sum256(e.b.Bytes())
	return "sha256:" + hex.EncodeToString(h[:])
}

func sorted(l []string) []string { c := slices.Clone(l); slices.Sort(c); return c }

// RunnerSize is the runner a sandbox imitates: cpus and memory, defaulting to
// GitHub's ubuntu-latest on public repos (4 CPUs, 16 GB).
func (t *Task) RunnerSize() (cpus, memGB int) {
	cpus, memGB = t.CPUs, t.MemoryGB()
	if cpus <= 0 {
		cpus = 4
	}
	if memGB <= 0 {
		memGB = 16
	}
	return
}

// MemoryGB parses memory: ("16g"); 0 if unset or malformed.
func (t *Task) MemoryGB() int {
	n := 0
	if v, ok := strings.CutSuffix(t.Memory, "g"); ok {
		if _, err := fmt.Sscan(v, &n); err != nil || n <= 0 || fmt.Sprint(n) != v {
			return 0
		}
	}
	return n
}

// Graduation records why a stand-in may skip its job (plan §11.5).
type Graduation struct {
	JobDigest  string `yaml:"job_digest"`  // the job definition it agreed for
	Backtested int    `yaml:"backtested"`  // past commits whose CI result it reproduced
	CIFailures int    `yaml:"ci_failures"` // of those, ones where CI failed and so did it
	At         string `yaml:"at"`          // date
}

// Graduates reports whether t may skip its covered job: graduated for the
// job_digest it covers now.
func (t *Task) Graduates() bool {
	return t.Graduated != nil && t.JobDigest != "" && t.Graduated.JobDigest == t.JobDigest
}
