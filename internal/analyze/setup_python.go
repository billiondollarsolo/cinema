package analyze

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/billiondollarsolo/cinema/internal/task"
)

func init() {
	RegisterReleases("python", pythonReleases)
	RegisterSetupAction("actions/setup-python", setupPython)
	RegisterWithoutSetup("python", usesPython, pythonPreinstalled)
}

var usesPython = regexp.MustCompile(`(^|[\s;&|(])(python3?|pip3?|pytest|tox|nox)\b`)

// pythonPreinstalled: with no setup-python, `python`/`pip` on GitHub's Ubuntu
// images are the release's apt python3 (python-is-python3, python3-pip; the
// toolcache's Pythons aren't on PATH, and nothing reads .python-version).
// cinema's runner image has the same apt python3, so the faithful draft
// pins nothing and fingerprints python3 --version on both sides.
func pythonPreinstalled(c *SetupCtx) {
	c.Probe("python3", "--version")
	c.Note("Python isn't set up by the job: it uses the runner's python3, Ubuntu's (3.10 on 22.04, 3.12 on 24.04, 3.14 on 26.04; .python-version isn't read), as the runner image does; python3 --version is fingerprinted")
}

// pythonManifest is the release list setup-python resolves version specs
// against (a variable so tests can point it elsewhere).
var pythonManifest = "https://raw.githubusercontent.com/actions/python-versions/main/versions-manifest.json"

func pythonReleases() ([]Release, error) {
	b, err := fetch(pythonManifest)
	if err != nil {
		return nil, err
	}
	var v []struct {
		Version string `json:"version"` // 3.14.0, 3.15.0-rc.3
		Stable  bool   `json:"stable"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("python versions manifest: %w", err)
	}
	exact := task.LookupRuntime("python").Version
	var out []Release // the manifest lists newest first
	for _, r := range v {
		out = append(out, Release{Version: r.Version, Stable: r.Stable && exact.MatchString(r.Version)})
	}
	return out, nil
}

var (
	pipVersion   = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(\.[0-9]+)?$`) // setup-python's pip-version format
	freethreaded = regexp.MustCompile(`^[0-9.]+t$`)                     // 3.13t
)

// setupPython translates actions/setup-python: CPython pinned at the version
// it would install, and a writable copy of it, as setup-python's toolcache
// install is: jobs pip install into the interpreter itself, and the runtime
// is mounted read-only (pip would fall back to ~/.local, uv pip install
// --system would fail). The copy goes in $RUNNER_TEMP, gone with the run.
func setupPython(c *SetupCtx) {
	switch a := c.With("architecture"); {
	case c.With("update-environment") == "false":
		c.Review("setup-python with update-environment: false (steps use its python-path output): cinema puts the pinned Python on PATH; translate those steps")
		return
	case a != "" && a != "x64":
		c.Review("setup-python architecture " + a + ": cinema runs linux/amd64 drafts with x64 Python")
		return
	case c.With("freethreaded") == "true":
		c.Review("setup-python freethreaded: cinema provisions the default (GIL) CPython build only")
		return
	}
	spec, file := c.With("python-version"), "" // the input wins over a file, as in the action
	if spec == "" {
		if file = c.With("python-version-file"); file == "" {
			file = ".python-version" // the action's default
			if spec = pythonVersionFile(c.Root, file); spec == "" {
				c.Note("setup-python has no version and the repo no .python-version: the job uses the image's python3, as CI uses the runner's")
				return
			}
		} else if spec = pythonVersionFile(c.Root, file); spec == "" {
			c.Review("setup-python reads the Python version from " + file + ": pin the exact version in runtime:")
			return
		}
	}
	versions := strings.FieldsFunc(spec, func(r rune) bool { return r == '\n' || r == ',' })
	if len(versions) > 1 {
		c.Review("setup-python installs several Pythons (" + strings.Join(versions, ", ") + "): one task per version (a matrix), pinned in runtime:")
		return
	}
	v := strings.TrimSpace(versions[0])
	switch {
	case strings.HasPrefix(v, "pypy"), strings.HasPrefix(v, "graalpy"):
		c.Review("setup-python " + v + ": cinema provisions CPython only")
		return
	case strings.HasSuffix(v, "-dev"), freethreaded.MatchString(v):
		c.Review("setup-python " + v + ": prerelease and free-threaded builds aren't provisioned; pin a release in runtime:")
		return
	}
	ver := exactPinned(c, "python", v, file)
	if ver == "" {
		return
	}
	c.Pin("python", ver)
	run := `d="${RUNNER_TEMP:-$HOME}/cinema-python"
rm -rf "$d" && cp -a "${pythonLocation:?}/." "$d"
printf '%s\n' "pythonLocation=$d" "Python_ROOT_DIR=$d" "Python2_ROOT_DIR=$d" "Python3_ROOT_DIR=$d" "PKG_CONFIG_PATH=$d/lib/pkgconfig" "LD_LIBRARY_PATH=$d/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}" >>"$GITHUB_ENV"
printf '%s\n' "$d" "$d/bin" >>"$GITHUB_PATH"`
	if pv := c.With("pip-version"); pv != "" {
		if !pipVersion.MatchString(pv) {
			c.Review("setup-python pip-version " + pv + " isn't major[.minor][.patch]")
			return
		}
		run += "\n\"$d/bin/python\" -m pip install --upgrade pip==" + pv + " --disable-pip-version-check --no-warn-script-location"
	}
	c.Step(run, "")
	c.Note("setup-python's Python is a writable copy in the toolcache: cinema copies the pinned one to $RUNNER_TEMP for the run, so pip installs into it as in CI")
}

// pythonVersionFile reads the version spec setup-python takes from file:
// .tool-versions' python entry, or a .python-version-style file's versions
// (comments skipped, pyenv's "3.12.7/envs/x" cut to the version), one per
// line. "" if absent or unreadable; pyproject.toml's requires-python is a
// range setup-python resolves by semver, left to a person.
func pythonVersionFile(root, file string) string {
	if root == "" || !filepath.IsLocal(filepath.FromSlash(file)) || filepath.Base(file) == "pyproject.toml" {
		return ""
	}
	text := versionFromFile(filepath.Join(root, filepath.FromSlash(file)))
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if filepath.Base(file) == ".tool-versions" {
			if f := strings.Fields(line); len(f) == 2 && f[0] == "python" {
				return strings.TrimPrefix(f[1], "v")
			}
			continue
		}
		v, _, _ := strings.Cut(line, "/")
		out = append(out, v)
	}
	return strings.Join(out, "\n")
}

// exactPinned is c.Exact with python's or uv's exact-version pattern.
func exactPinned(c *SetupCtx, runtime, spec, file string) string {
	return c.Exact(runtime, spec, file, task.LookupRuntime(runtime).Version)
}
