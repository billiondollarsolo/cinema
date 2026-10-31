package analyze

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/billiondollarsolo/cinema/internal/task"
)

func init() {
	RegisterReleases("uv", uvReleases)
	RegisterSetupAction("astral-sh/setup-uv", setupUv)
}

// uvManifest is the release list setup-uv resolves versions against, newest
// first (a variable so tests can point it elsewhere).
var uvManifest = "https://releases.astral.sh/github/versions/main/v1/uv.ndjson"

func uvReleases() ([]Release, error) {
	b, err := fetch(uvManifest)
	if err != nil {
		return nil, err
	}
	exact := task.LookupRuntime("uv").Version
	var out []Release
	dec := json.NewDecoder(strings.NewReader(string(b)))
	for {
		var r struct {
			Version string `json:"version"`
		}
		if err := dec.Decode(&r); errors.Is(err, io.EOF) {
			return out, nil
		} else if err != nil {
			return nil, fmt.Errorf("uv versions manifest: %w", err)
		}
		out = append(out, Release{Version: r.Version, Stable: exact.MatchString(r.Version)})
	}
}

var (
	minimumSpec = regexp.MustCompile(`^>=?\s*[0-9]+(\.[0-9]+)*$`)                                  // ">=0.5.0": setup-uv takes the latest
	uvRequire   = regexp.MustCompile(`^uv\s*((?:[=<>~!]+\s*[^\s,]+\s*,?\s*)+)`)                    // a uv requirement, as setup-uv reads one
	uvDepend    = regexp.MustCompile(`["']uv\s*[=<>~!]`)                                           // one in pyproject's dependency lists
	tomlValue   = regexp.MustCompile(`^([A-Za-z0-9_.-]+)\s*=\s*(?:"([^"]*)"|'([^']*)')\s*(#.*)?$`) // key = "string"
	plainPath   = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)                                         // safe unquoted in a script
)

// setupUv translates astral-sh/setup-uv: uv pinned at the version it would
// install, and its side effects as a step: ~/.local/bin (uv tool's bin dir)
// on PATH, UV_PYTHON from python-version, the venv activate-environment
// makes. Caching inputs don't change results; uv's cache is on the sandbox's
// cache volume (~/.cache/uv).
func setupUv(c *SetupCtx) {
	dir := path.Clean(cmp.Or(c.With("working-directory"), "."))
	if !filepath.IsLocal(filepath.FromSlash(dir)) || strings.Contains(dir, "${{") {
		c.Review("setup-uv working-directory " + c.With("working-directory") + ": cinema needs a repo-relative path")
		return
	}
	for _, k := range []string{"python-arch", "tool-dir", "tool-bin-dir", "manifest-file"} {
		if c.With(k) != "" {
			c.Review("setup-uv with " + k + ": not reproduced; translate it into commands")
			return
		}
	}
	if c.With("resolution-strategy") == "lowest" {
		c.Review("setup-uv resolution-strategy: lowest: pin the uv version in runtime:")
		return
	}
	spec, from := strings.TrimSpace(c.With("version")), "version"
	if spec == "" {
		var why string
		if spec, from, why = uvVersionFromFiles(c.Root, dir, c.With("version-file")); why != "" {
			c.Review(why)
			return
		}
	}
	spec = strings.TrimPrefix(spec, "==")
	if minimumSpec.MatchString(spec) {
		c.Note("setup-uv " + from + " " + spec + ": it installs the latest uv")
		spec = "latest"
	}
	ver := exactPinned(c, "uv", spec, "")
	if ver == "" {
		return
	}
	c.Pin("uv", ver)
	run := []string{`echo "$HOME/.local/bin" >>"$GITHUB_PATH"`}
	py := c.With("python-version")
	if py == "" && c.Root != "" {
		// uv reads .python-version: an exact one is the Python it'll download in
		// CI, pinned here so it's fingerprinted (a range: uv may take the
		// runner's own python3, as it may here the image's).
		if v := pythonVersionFile(c.Root, path.Join(dir, ".python-version")); task.LookupRuntime("python").Version.MatchString(v) {
			c.Pin("python", v)
			c.Note("uv takes Python " + v + " from .python-version: pinned")
		}
	}
	if py != "" {
		// UV_PYTHON=3.12 on the runner can be its preinstalled python3 or a
		// download of the newest build: only an exact version is known.
		if !task.LookupRuntime("python").Version.MatchString(py) {
			c.Review("setup-uv python-version " + py + " isn't exact: uv may use the runner's python3 or download the newest match; pin an exact version")
			return
		}
		c.Pin("python", py)
		run = append(run, "export UV_PYTHON="+shellQuote(py), `echo "UV_PYTHON=$UV_PYTHON" >>"$GITHUB_ENV"`)
	}
	if c.With("activate-environment") == "true" {
		venv := path.Clean(cmp.Or(c.With("venv-path"), ".venv"))
		if !filepath.IsLocal(filepath.FromSlash(venv)) || !plainPath.MatchString(venv) {
			c.Review("setup-uv venv-path " + venv + ": cinema needs a plain path inside the working directory")
			return
		}
		cmd := "uv venv " + shellQuote(venv) + " --clear"
		if c.With("no-project") == "true" {
			cmd += " --no-project"
		}
		run = append(run, cmd, `echo "VIRTUAL_ENV=$PWD/`+venv+`" >>"$GITHUB_ENV"`, `echo "$PWD/`+venv+`/bin" >>"$GITHUB_PATH"`)
	}
	c.Step(strings.Join(run, "\n"), dir)
}

// uvVersionFromFiles finds the uv version spec as setup-uv does without a
// version input: in version-file if given, else uv.toml's then pyproject.toml's
// required-version, else latest. why: the file holds a version cinema can't
// read (review).
func uvVersionFromFiles(root, dir, versionFile string) (spec, from, why string) {
	if root == "" {
		return "", "", "setup-uv takes its version from the repo's files: pin the uv version in runtime:"
	}
	read := func(name string) (string, string, bool) { // spec, why, found
		rel := path.Join(dir, name)
		if !filepath.IsLocal(filepath.FromSlash(rel)) {
			return "", "setup-uv version-file " + name + ": outside the repo", true
		}
		text := versionFromFile(filepath.Join(root, filepath.FromSlash(rel)))
		if text == "" {
			return "", "", false
		}
		switch base := path.Base(name); {
		case base == "uv.toml" || base == "pyproject.toml":
			table := ""
			if base == "pyproject.toml" {
				table = "tool.uv"
			}
			if v, ok := tomlString(text, table, "required-version"); ok {
				return v, "", true
			}
			if strings.Contains(text, "required-version") || (base == "pyproject.toml" && uvDepend.MatchString(text)) {
				return "", "setup-uv takes the uv version from " + rel + ": pin it in runtime:", true
			}
		case base == ".tool-versions":
			for _, line := range strings.Split(text, "\n") {
				f := strings.Fields(line)
				if len(f) >= 2 && f[0] == "python" {
					return "", "setup-uv also takes the Python version from " + rel + ": pin both in runtime:", true
				}
				if len(f) == 2 && f[0] == "uv" {
					return f[1], "", true
				}
			}
		case strings.HasSuffix(base, ".txt"):
			for _, line := range strings.Split(text, "\n") {
				line, _, _ = strings.Cut(line, ";")
				if m := uvRequire.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
					return strings.ReplaceAll(m[1], " ", ""), "", true
				}
			}
		default:
			return "", "setup-uv takes the uv version from " + rel + ": pin it in runtime:", true
		}
		return "", "", false
	}
	if versionFile != "" {
		v, why, found := read(versionFile)
		if why == "" && !found {
			why = "setup-uv version-file " + versionFile + ": no uv version found (the action fails)"
		}
		return v, versionFile, why
	}
	for _, f := range []string{"uv.toml", "pyproject.toml"} {
		if v, why, found := read(f); found {
			return v, f, why
		}
	}
	return "latest", "default", ""
}

// tomlString is key's string value in table ("" for the top level) of a TOML
// file, read line by line: enough for required-version. ok is false if the
// key isn't there as a plain string.
func tomlString(text, table, key string) (string, bool) {
	cur := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			cur = strings.TrimSpace(strings.Trim(line, "[]"))
			continue
		}
		if m := tomlValue.FindStringSubmatch(line); m != nil && cur == table && m[1] == key {
			return m[2] + m[3], true
		}
	}
	return "", false
}
