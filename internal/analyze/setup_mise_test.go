package analyze

import (
	"strings"
	"testing"
)

// miseReleases fakes the release lists floating mise specs resolve against.
func miseReleases(t *testing.T) {
	releases.m = map[string][]release{
		"Node":   {{Version: "23.1.0", Stable: true}, {Version: "22.11.0", LTS: true, Stable: true}, {Version: "22.1.0", LTS: true, Stable: true}},
		"Go":     {{Version: "1.23.4", Stable: true}},
		"python": {{Version: "3.12.8", Stable: true}, {Version: "3.12.7", Stable: true}},
		"java":   {{Version: "temurin-21.0.6+7", Stable: true}, {Version: "temurin-21.0.5+11", Stable: true}},
		"rust":   {{Version: "1.83.0", Stable: true}, {Version: "1.82.0", Stable: true}},
	}
	t.Cleanup(func() { releases.m = map[string][]release{} })
}

func toolJob(t *testing.T, step string, files map[string]string) Job {
	t.Helper()
	files[".github/workflows/ci.yml"] = "on: pull_request\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n" + step + "      - run: ./ci.sh\n"
	return repoJobs(t, files)["test"]
}

func TestMiseAndAsdf(t *testing.T) {
	miseReleases(t)
	const mise, nocache = "      - uses: jdx/mise-action@v3\n", "      - uses: jdx/mise-action@v3\n        with: {cache: false}\n"
	cases := []struct {
		name, step string
		files      map[string]string
		pins       map[string]string // runtime → pin (go and node included)
		floating   map[string]string
		review     string   // "" means a task
		body       []string // in the script
	}{
		{name: "mise.toml, floating, cached", step: mise,
			files:    map[string]string{"mise.toml": "[tools]\nnode = \"22\" # lts line\npython = \"3.12.7\"\njava = \"temurin-21\"\nrust = { version = \"1.82.0\", components = [\"clippy\"] }\n"},
			pins:     map[string]string{"node": "22.11.0", "python": "3.12.7", "java": "temurin-21.0.6+7", "rust": "1.82.0+clippy"},
			floating: map[string]string{"node": "22", "java": "temurin-21"}, review: "caches installed tools"},
		{name: "lts, no cache", step: nocache, files: map[string]string{"mise.toml": "[tools]\nnode = \"lts\"\n[tools.go]\nversion = \"1.23\"\n"},
			pins: map[string]string{"node": "22.11.0", "go": "1.23.4"}, floating: map[string]string{"node": "lts/*", "go": "1.23"}},
		{name: "tool-versions", step: nocache, files: map[string]string{".tool-versions": "# asdf\nnodejs 22.1.0\ngolang 1.23.4\n"},
			pins: map[string]string{"node": "22.1.0", "go": "1.23.4"}},
		{name: "precedence", step: mise, files: map[string]string{".tool-versions": "nodejs 20.0.0\npython 3.12.8\n", "mise.toml": "[tools]\nnode = \"22.1.0\"\n", ".mise.toml": "[tools]\nnode = \"23.1.0\"\n"},
			pins: map[string]string{"node": "23.1.0", "python": "3.12.8"}},
		{name: "lock", step: mise, files: map[string]string{"mise.toml": "[tools]\nnode = \"22\"\n", "mise.lock": "[[tools.node]]\nversion = \"22.1.0\"\nbackend = \"core:node\"\n[tools.node.platforms.linux-x64]\nchecksum = \"x\"\n"},
			pins: map[string]string{"node": "22.1.0"}},
		{name: "working directory", step: "      - uses: jdx/mise-action@v3\n        with: {working_directory: app}\n",
			files: map[string]string{"mise.toml": "[tools]\nnode = \"22.1.0\"\npython = \"3.12.7\"\n", "app/mise.toml": "[tools]\nnode = \"23.1.0\"\n"},
			pins:  map[string]string{"node": "23.1.0", "python": "3.12.7"}},
		{name: "tool_versions input", step: "      - uses: jdx/mise-action@v3\n        with: {tool_versions: \"nodejs 22.1.0\"}\n",
			files: map[string]string{".tool-versions": "nodejs 20.0.0\n"}, pins: map[string]string{"node": "22.1.0"}},
		{name: "install_args", step: "      - uses: jdx/mise-action@v3\n        with: {install_args: node}\n",
			files: map[string]string{"mise.toml": "[tools]\nnode = \"22.1.0\"\nterraform = \"1.9.0\"\n"}, pins: map[string]string{"node": "22.1.0"}},
		{name: "env", step: mise, files: map[string]string{"mise.toml": "[env]\nNODE_ENV = 'test'\nRETRIES = 3\n[tools]\nnode = \"22.1.0\"\n[tasks.test]\nrun = \"npm test\"\n"},
			pins: map[string]string{"node": "22.1.0"}, body: []string{`echo NODE_ENV=test >> "$GITHUB_ENV"`, `echo RETRIES=3 >> "$GITHUB_ENV"`}},
		{name: "unprovisioned", step: mise, files: map[string]string{"mise.toml": "[tools]\nnode = \"22.1.0\"\nterraform = \"1.9.0\"\n\"npm:prettier\" = \"3\"\n"},
			pins: map[string]string{"node": "22.1.0"}, review: "doesn't provision (npm:prettier 3, terraform 1.9.0)"},
		{name: "java without vendor", step: mise, files: map[string]string{"mise.toml": "[tools]\njava = \"21\"\n"}, review: "OpenJDK"},
		{name: "java other vendor", step: mise, files: map[string]string{".tool-versions": "java corretto-21.0.5.11.1\n"}, review: "Temurin only"},
		{name: "several versions", step: mise, files: map[string]string{"mise.toml": "[tools]\npython = [\n  \"3.12.7\",\n  \"3.11.10\",\n]\n"}, review: "several versions"},
		{name: "system", step: mise, files: map[string]string{".tool-versions": "nodejs system\n"}, review: "runner's preinstalled"},
		{name: "tool option", step: mise, files: map[string]string{"mise.toml": "[tools]\npython = { version = \"3.12.7\", virtualenv = \".venv\" }\n"}, review: "tool option virtualenv"},
		{name: "env directive", step: mise, files: map[string]string{"mise.toml": "[env]\n_.path = [\"bin\"]\n"}, review: "directive _.path"},
		{name: "env template", step: mise, files: map[string]string{"mise.toml": "[env]\nOUT = \"{{config_root}}/out\"\n"}, review: "template"},
		{name: "env off", step: "      - uses: jdx/mise-action@v3\n        with: {env: false}\n", files: map[string]string{"mise.toml": "[env]\nOUT = \"{{config_root}}/out\"\n"}, review: "template"},
		{name: "idiomatic files", step: mise, files: map[string]string{"mise.toml": "[settings]\nidiomatic_version_file_enable_tools = [\"node\"]\n"}, review: "idiomatic_version_file"},
		{name: "hooks", step: mise, files: map[string]string{"mise.toml": "[hooks]\npostinstall = \"npm i -g x\"\n"}, review: "[hooks]"},
		{name: "fragments", step: mise, files: map[string]string{".config/mise/conf.d/a.toml": "[tools]\nnode = \"22.1.0\"\n"}, review: "conf.d"},
		{name: "install false", step: "      - uses: jdx/mise-action@v3\n        with: {install: false}\n", files: map[string]string{}, review: "install: false"},
		{name: "asdf install", step: "      - uses: asdf-vm/actions/install@v4\n",
			files: map[string]string{".tool-versions": "java temurin-21.0.5+11.0.LTS\nnodejs 22.1.0\n", "mise.toml": "[tools]\nnode = \"23.1.0\"\n"},
			pins:  map[string]string{"java": "temurin-21.0.5+11", "node": "22.1.0"}}, // asdf reads .tool-versions only
		{name: "asdf before_install", step: "      - uses: asdf-vm/actions/install@v4\n        with: {before_install: ./x.sh}\n", files: map[string]string{}, review: "before_install"},
		{name: "asdf setup", step: "      - uses: asdf-vm/actions/setup@v4\n", files: map[string]string{}, review: "installs asdf only"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			j := toolJob(t, c.step, c.files)
			reasons := strings.Join(j.Reasons, "; ")
			if j.Proposal == nil {
				t.Fatalf("no proposal: %s %s", j.Verdict, reasons)
			}
			p := j.Proposal
			got := map[string]string{}
			for k, v := range p.Runtimes {
				got[k] = v
			}
			if p.Go != "" {
				got["go"] = p.Go
			}
			if p.Node != "" {
				got["node"] = p.Node
			}
			if len(got) != len(c.pins) {
				t.Errorf("pins %v, want %v (%s)", got, c.pins, reasons)
			}
			for k, v := range c.pins {
				if got[k] != v {
					t.Errorf("%s %q, want %q (%s)", k, got[k], v, reasons)
				}
			}
			for k, v := range c.floating {
				if p.Floating[k] != v {
					t.Errorf("floating %v, want %s: %s", p.Floating, k, v)
				}
			}
			if c.review == "" && j.Verdict != Task {
				t.Errorf("verdict %s: %s", j.Verdict, reasons)
			}
			if c.review != "" && !strings.Contains(reasons, c.review) {
				t.Errorf("reasons %q, want %q", reasons, c.review)
			}
			for _, b := range c.body {
				if !strings.Contains(p.Body, b) {
					t.Errorf("script lacks %q:\n%s", b, p.Body)
				}
			}
		})
	}
}
