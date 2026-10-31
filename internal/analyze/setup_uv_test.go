package analyze

import (
	"strings"
	"testing"
)

func TestSetupUv(t *testing.T) {
	fakeReleases(t)
	jobs := repoJobs(t, map[string]string{
		"svc/uv.toml":         "# uv settings\nrequired-version = \"==0.8.24\"\n",
		"svc/.python-version": "3.11.10\n",
		"lib/pyproject.toml":  "[project]\nname = \"lib\"\n\n[tool.uv]\nrequired-version = '>=0.5.0'\n",
		"dep/pyproject.toml":  "[project]\nname = \"dep\"\ndependencies = [\"uv==0.9.5\"]\n",
		"req.txt":             "ruff==0.6.0\nuv == 0.9.5 ; python_version > '3'\n",
		".github/workflows/ci.yml": `on: pull_request
jobs:
  exact:
    runs-on: ubuntu-latest
    steps:
      - uses: astral-sh/setup-uv@v7
        with: {version: 0.9.5, python-version: 3.12.7, activate-environment: true, enable-cache: true, cache-dependency-glob: uv.lock}
      - run: uv sync && uv run pytest
  latest:
    runs-on: ubuntu-latest
    steps: [{uses: astral-sh/setup-uv@v7}, {run: uv run pytest}]
  range:
    runs-on: ubuntu-latest
    steps: [{uses: astral-sh/setup-uv@v7, with: {version: "0.9.x"}}, {run: uv run pytest}]
  uv-toml:
    runs-on: ubuntu-latest
    steps: [{uses: astral-sh/setup-uv@v7, with: {working-directory: svc}}, {run: uv run pytest, working-directory: svc}]
  minimum:
    runs-on: ubuntu-latest
    steps: [{uses: astral-sh/setup-uv@v7, with: {working-directory: lib}}, {run: uv run pytest}]
  dependency:
    runs-on: ubuntu-latest
    steps: [{uses: astral-sh/setup-uv@v7, with: {working-directory: dep}}, {run: uv run pytest}]
  version-file:
    runs-on: ubuntu-latest
    steps: [{uses: astral-sh/setup-uv@v7, with: {version-file: req.txt}}, {run: uv run pytest}]
  pep440:
    runs-on: ubuntu-latest
    steps: [{uses: astral-sh/setup-uv@v7, with: {version: ">=0.5,<0.9"}}, {run: uv run pytest}]
  python-range:
    runs-on: ubuntu-latest
    steps: [{uses: astral-sh/setup-uv@v7, with: {version: 0.9.5, python-version: "3.12"}}, {run: uv run pytest}]
  tool-dir:
    runs-on: ubuntu-latest
    steps: [{uses: astral-sh/setup-uv@v7, with: {version: 0.9.5, tool-bin-dir: /opt/bin}}, {run: uv run pytest}]
  outside:
    runs-on: ubuntu-latest
    steps: [{uses: astral-sh/setup-uv@v7, with: {version: 0.9.5, working-directory: ../other}}, {run: uv run pytest}]
`})
	for id, want := range map[string]struct {
		verdict, uv, python, reason string
	}{
		"exact":        {Task, "0.9.5", "3.12.7", ""},
		"latest":       {Review, "0.9.6", "", `floating: {uv: "latest"}`},
		"range":        {Review, "0.9.6", "", `floating: {uv: "0.9.x"}`},
		"uv-toml":      {Task, "0.8.24", "3.11.10", ""},
		"minimum":      {Review, "0.9.6", "", `floating: {uv: "latest"}`},
		"dependency":   {Review, "", "", "from dep/pyproject.toml"},
		"version-file": {Task, "0.9.5", "", ""},
		"pep440":       {Review, "", "", `">=0.5,<0.9" isn't exact`},
		"python-range": {Review, "0.9.5", "", "python-version 3.12 isn't exact"},
		"tool-dir":     {Review, "", "", "tool-bin-dir"},
		"outside":      {Review, "", "", "repo-relative"},
	} {
		j := jobs[id]
		var rt map[string]string
		if j.Proposal != nil {
			rt = j.Proposal.Runtimes
		}
		if j.Verdict != want.verdict || rt["uv"] != want.uv || rt["python"] != want.python || !strings.Contains(strings.Join(j.Reasons, "\n"), want.reason) {
			t.Errorf("%s: %s %v %q", id, j.Verdict, rt, j.Reasons)
		}
	}
	body := jobs["exact"].Proposal.Body
	for _, s := range []string{`echo "$HOME/.local/bin" >>"$GITHUB_PATH"`, "export UV_PYTHON=3.12.7", "uv venv .venv --clear", `echo "VIRTUAL_ENV=$PWD/.venv" >>"$GITHUB_ENV"`} {
		if !strings.Contains(body, s) {
			t.Errorf("exact: no %s:\n%s", s, body)
		}
	}
	if body := jobs["uv-toml"].Proposal.Body; !strings.Contains(body, "cd svc\n") {
		t.Errorf("uv-toml: the step runs in working-directory:\n%s", body)
	}
}

func TestTOMLString(t *testing.T) {
	text := "required-version = \"top\"\n[tool.ruff]\nrequired-version = \"ruff\"\n[tool.uv]\n  required-version = '>=0.5' # min\n"
	if v, ok := tomlString(text, "tool.uv", "required-version"); !ok || v != ">=0.5" {
		t.Errorf("tool.uv: %q %v", v, ok)
	}
	if v, ok := tomlString(text, "", "required-version"); !ok || v != "top" {
		t.Errorf("top: %q %v", v, ok)
	}
	if _, ok := tomlString("[tool.uv]\nrequired-version = [\"x\"]\n", "tool.uv", "required-version"); ok {
		t.Error("an array read as a string")
	}
}
