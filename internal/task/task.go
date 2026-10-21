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
	return slices.ContainsFunc(t.Toolchain, func(tc Toolchain) bool { return len(tc.Cmd) > 0 })
}

type Task struct {
	ID              string      `yaml:"-"`
	Argv            []string    `yaml:"argv"`
	Cwd             string      `yaml:"cwd"`      // repo-relative; default "."
	Platform        string      `yaml:"platform"` // "goos/goarch" or "any"; required, never inferred
	Toolchain       []Toolchain `yaml:"toolchain"`
	Env             []string    `yaml:"env"`          // allowlisted env var names that are inputs
	ExtraInputs     []string    `yaml:"extra_inputs"` // ignored files the task reads
	VCSContext      string      `yaml:"vcs_context"`  // "none" (default) | "exact"
	AcceptToolCache bool        `yaml:"accept_tool_cache"`
	Reuse           Reuse       `yaml:"reuse"`
}

// Toolchain is one environment input: a command whose output identifies a tool
// version, or a file whose content does. Exactly one of Cmd / File.
type Toolchain struct {
	Cmd  []string `yaml:"cmd"`
	File string   `yaml:"file"`
}

// Reuse is acceptance policy. It is NOT part of the contract digest: a stricter
// policy rejects old evidence without changing what the work was (plan §6.2).
type Reuse struct {
	Allowed bool          `yaml:"allowed"`
	MaxAge  time.Duration `yaml:"-"`
	RawAge  string        `yaml:"max_age"`
}

var (
	idRE       = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	platformRE = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9]+$`)
	envNameRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Load reads and validates root/.cinema/tasks.yaml.
func Load(root string) (*Config, error) {
	f, err := os.Open(filepath.Join(root, ConfigPath))
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
		switch {
		case len(tc.Cmd) > 0 && tc.File == "":
			if err := validateProbe(tc.Cmd); err != nil {
				return fmt.Errorf("toolchain[%d]: %w", i, err)
			}
		case len(tc.Cmd) == 0 && tc.File != "":
			c, err := snapshot.CleanRel(tc.File)
			if err != nil {
				return fmt.Errorf("toolchain[%d]: %w", i, err)
			}
			t.Toolchain[i].File = c
		default:
			return fmt.Errorf("toolchain[%d]: exactly one of cmd or file", i)
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
	if t.Reuse.Allowed {
		d, err := time.ParseDuration(t.Reuse.RawAge)
		if err != nil || d <= 0 {
			return errors.New("reuse.max_age is required when reuse is allowed (e.g. 72h)")
		}
		t.Reuse.MaxAge = d
	}
	return nil
}

// Toolchain commands identify tool versions; they must not run repo code.
// Fail closed: a bare command from PATH, plain flag/word arguments only (no
// paths, dots or spaces, so no script files), no eval flags or code-running
// subcommands, and script runners (which read targets from repo files) get
// flags only. `cinema trust` still gates running them at all.
var (
	probeArgRE    = regexp.MustCompile(`^-{0,2}[A-Za-z0-9][A-Za-z0-9_=-]*$`)
	probeEvalArgs = []string{"-c", "-e", "-p", "-r", "-m", "-x", "-E", "--eval", "--print", "--require", "--exec", "--command"}
	probeRunWords = []string{"run", "exec", "eval", "x", "dlx", "tool", "generate", "test", "build", "start", "install", "i", "ci", "script", "shell"}
	scriptRunners = []string{"make", "gmake", "just", "task", "rake", "npm", "npx", "pnpm", "yarn", "bun", "bunx", "deno",
		"gradle", "gradlew", "mvn", "mvnw", "mise", "cargo", "sbt", "ant", "invoke", "nox", "tox"}
)

func validateProbe(cmd []string) error {
	if !envNameRE.MatchString(strings.ReplaceAll(strings.ReplaceAll(cmd[0], "-", "_"), ".", "_")) || strings.ContainsRune(cmd[0], '/') {
		return fmt.Errorf("cmd[0] must be a command name on PATH, like go or node (got %q)", cmd[0])
	}
	runner := slices.Contains(scriptRunners, cmd[0])
	for _, a := range cmd[1:] {
		switch {
		case !probeArgRE.MatchString(a):
			return fmt.Errorf("argument %q: version probes take plain flags and words only (no paths, dots or spaces)", a)
		case slices.Contains(probeEvalArgs, a), slices.Contains(probeRunWords, a):
			return fmt.Errorf("argument %q would run code; toolchain commands only report versions", a)
		case runner && !strings.HasPrefix(a, "-"):
			return fmt.Errorf("%s reads targets from repo files; use flags only (e.g. %s --version)", cmd[0], cmd[0])
		}
	}
	return nil
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
func (c *Config) Match(argv []string, cwdRel string) *Task {
	if cwdRel == "" {
		cwdRel = "."
	}
	for _, t := range c.Tasks {
		if t.Cwd == cwdRel && slices.Equal(t.Argv, argv) {
			return t
		}
	}
	return nil
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
	}
	e.list(sorted(t.Env))
	e.list(sorted(t.ExtraInputs))
	e.str(t.VCSContext)
	e.bool(t.AcceptToolCache)
	e.str(script)
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
