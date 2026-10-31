package analyze

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeReleases serves setup-python's and setup-uv's release lists.
func fakeReleases(t *testing.T) {
	old := fetch
	fetch = func(url string) ([]byte, error) {
		switch url {
		case pythonManifest:
			return []byte(`[{"version":"3.13.0-rc.1","stable":false},{"version":"3.12.8","stable":true},{"version":"3.12.7","stable":true},{"version":"3.11.10","stable":true}]`), nil
		case uvManifest:
			return []byte(`{"version":"0.9.6","artifacts":[]}` + "\n" + `{"version":"0.9.5","artifacts":[]}` + "\n" + `{"version":"0.8.24","artifacts":[]}` + "\n"), nil
		}
		return nil, errors.New("offline")
	}
	releases.m = map[string][]release{}
	t.Cleanup(func() { fetch, releases.m = old, map[string][]release{} })
}

func repoJobs(t *testing.T, files map[string]string) map[string]Job {
	d := t.TempDir()
	for name, body := range files {
		p := filepath.Join(d, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	jobs, err := Repo(d)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Job{}
	for _, j := range jobs {
		got[j.ID] = j
	}
	return got
}

func TestSetupPython(t *testing.T) {
	fakeReleases(t)
	jobs := repoJobs(t, map[string]string{
		".python-version": "# pyenv\n3.11.10/envs/dev\n",
		".tool-versions":  "nodejs 22.1.0\npython 3.12.7\n",
		".github/workflows/ci.yml": `on: pull_request
jobs:
  exact:
    runs-on: ubuntu-latest
    steps: [{uses: actions/setup-python@v5, with: {python-version: 3.12.7, cache: pip, check-latest: true}}, {run: pip install -r requirements.txt && pytest}]
  float:
    runs-on: ubuntu-latest
    steps: [{uses: actions/setup-python@v5, with: {python-version: "3.12", allow-prereleases: true}}, {run: pytest}]
  default-file:
    runs-on: ubuntu-latest
    steps: [{uses: actions/setup-python@v5}, {run: pytest}]
  tool-versions:
    runs-on: ubuntu-latest
    steps: [{uses: actions/setup-python@v5, with: {python-version-file: .tool-versions}}, {run: pytest}]
  input-wins:
    runs-on: ubuntu-latest
    steps: [{uses: actions/setup-python@v5, with: {python-version: 3.12.8, python-version-file: .tool-versions}}, {run: pytest}]
  pip:
    runs-on: ubuntu-latest
    steps: [{uses: actions/setup-python@v5, with: {python-version: 3.12.7, pip-version: "24.0"}}, {run: pytest}]
  several:
    runs-on: ubuntu-latest
    steps: [{uses: actions/setup-python@v5, with: {python-version: "3.11\n3.12\n"}}, {run: pytest}]
  pypy:
    runs-on: ubuntu-latest
    steps: [{uses: actions/setup-python@v5, with: {python-version: pypy3.10}}, {run: pytest}]
  freethreaded:
    runs-on: ubuntu-latest
    steps: [{uses: actions/setup-python@v5, with: {python-version: 3.13t}}, {run: pytest}]
  no-update:
    runs-on: ubuntu-latest
    steps: [{uses: actions/setup-python@v5, with: {python-version: 3.12.7, update-environment: false}}, {run: pytest}]
  x86:
    runs-on: ubuntu-latest
    steps: [{uses: actions/setup-python@v5, with: {python-version: 3.12.7, architecture: x86}}, {run: pytest}]
  pyproject:
    runs-on: ubuntu-latest
    steps: [{uses: actions/setup-python@v5, with: {python-version-file: pyproject.toml}}, {run: pytest}]
  prerelease:
    runs-on: ubuntu-latest
    steps: [{uses: actions/setup-python@v5, with: {python-version: "3.13", allow-prereleases: true}}, {run: pytest}]
`})
	for id, want := range map[string]struct {
		verdict, python, reason string
	}{
		"exact":         {Task, "3.12.7", ""},
		"float":         {Task, "3.12.8", ""},
		"default-file":  {Task, "3.11.10", ""},
		"tool-versions": {Task, "3.12.7", ""},
		"input-wins":    {Task, "3.12.8", ""},
		"pip":           {Task, "3.12.7", ""},
		"several":       {Review, "", "several Pythons"},
		"pypy":          {Review, "", "CPython only"},
		"freethreaded":  {Review, "", "free-threaded"},
		"no-update":     {Review, "", "update-environment"},
		"x86":           {Review, "", "architecture x86"},
		"pyproject":     {Review, "", "pyproject.toml"},
		"prerelease":    {Review, "", `"3.13" isn't exact`},
	} {
		j := jobs[id]
		python := ""
		if j.Proposal != nil {
			python = j.Proposal.Runtimes["python"]
		}
		if j.Verdict != want.verdict || python != want.python || !strings.Contains(strings.Join(j.Reasons, "\n"), want.reason) {
			t.Errorf("%s: %s python=%q %q", id, j.Verdict, python, j.Reasons)
		}
	}
	if got := jobs["float"].Proposal.Floating["python"]; got != "3.12" {
		t.Errorf("floating python %q, want 3.12 (CI resolves it again)", got)
	}
	body := jobs["exact"].Proposal.Body
	if !strings.Contains(body, `cp -a "${pythonLocation:?}/." "$d"`) || strings.Index(body, "cinema-python") > strings.Index(body, "pip install -r") {
		t.Errorf("no writable copy before the job's steps:\n%s", body)
	}
	if body := jobs["pip"].Proposal.Body; !strings.Contains(body, `"$d/bin/python" -m pip install --upgrade pip==24.0`) {
		t.Errorf("pip-version:\n%s", body)
	}

	// Without a version or .python-version, setup-python leaves the runner's python3.
	jobs = repoJobs(t, map[string]string{".github/workflows/ci.yml": `on: pull_request
jobs:
  bare:
    runs-on: ubuntu-latest
    steps: [{uses: actions/setup-python@v5}, {run: pytest}]
`})
	if j := jobs["bare"]; j.Verdict != Task || len(j.Proposal.Runtimes) != 0 {
		t.Errorf("bare: %s %v %v", j.Verdict, j.Reasons, j.Proposal.Runtimes)
	}
}

// The writable copy works: pythonLocation and PATH point at it for later steps.
func TestSetupPythonCopy(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	fakeReleases(t)
	jobs := repoJobs(t, map[string]string{".github/workflows/ci.yml": `on: pull_request
jobs:
  t:
    runs-on: ubuntu-latest
    steps: [{uses: actions/setup-python@v5, with: {python-version: 3.12.7}}, {run: 'echo "$pythonLocation" > where && touch "$pythonLocation/lib/written"'}]
`})
	d := t.TempDir()
	ro := filepath.Join(d, "ro")
	os.MkdirAll(filepath.Join(ro, "bin"), 0o755)
	os.MkdirAll(filepath.Join(ro, "lib"), 0o755)
	os.WriteFile(filepath.Join(ro, "bin", "python3"), []byte("#!/bin/sh\n"), 0o755)
	cmd := exec.Command("bash", "-eo", "pipefail", "-c", jobs["t"].Proposal.Body)
	cmd.Dir = d
	cmd.Env = append(os.Environ(), "pythonLocation="+ro, "RUNNER_TEMP="+filepath.Join(d, "tmp"))
	os.Mkdir(filepath.Join(d, "tmp"), 0o755)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	where, _ := os.ReadFile(filepath.Join(d, "where"))
	if got := strings.TrimSpace(string(where)); got != filepath.Join(d, "tmp", "cinema-python") {
		t.Fatalf("pythonLocation %q", got)
	}
	if _, err := os.Stat(filepath.Join(d, "tmp", "cinema-python", "bin", "python3")); err != nil {
		t.Fatal(err)
	}
}
