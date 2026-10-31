package analyze

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const ci = `
on: [push, pull_request]
env: {CI: "true", TOKEN: "${{ secrets.T }}"}
jobs:
  test:
    strategy:
      matrix:
        os: [ubuntu-latest, windows-latest]
        node: [20.19.0, 22.x]
        exclude: [{os: windows-latest, node: 22.x}]
        include: [{os: ubuntu-latest, node: 20.19.0, flags: --strict}]
    runs-on: ${{ matrix.os }}
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-node@v4
        with: {node-version: "${{ matrix.node }}"}
      - run: npm ci
      - run: |
          npm test ${{ matrix.flags }}
        working-directory: pkg
  lint:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/setup-go@v5
        with: {go-version: 1.26.10}
      - run: make lint
  db:
    runs-on: ubuntu-latest
    services: {postgres: {image: postgres}}
    steps: [{run: make itest}]
  analyze:
    runs-on: ubuntu-latest
    steps: [{uses: github/codeql-action/analyze@v3}]
`

func TestWorkflow(t *testing.T) {
	old := fetch
	defer func() { fetch = old }()
	fetch = func(string) ([]byte, error) { return nil, errors.New("offline") } // ranges stay unresolved
	releases.m = map[string][]release{}
	jobs, err := Workflow("ci.yml", []byte(ci))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Job{}
	for _, j := range jobs {
		key := j.ID
		if j.Combo != nil {
			key += "/" + j.Combo["os"] + "/" + j.Combo["node"]
		}
		got[key] = j
	}
	want := map[string]string{
		"analyze": Skip, "db": Review, "lint": Review, // db, lint: workflow env TOKEN is a secret (db's postgres service translates)
		"test/ubuntu-latest/20.19.0": Review, "test/ubuntu-latest/22.x": Review, "test/windows-latest/20.19.0": Skip,
	}
	if len(got) != len(want) {
		t.Fatalf("jobs: %v", keys(got))
	}
	for k, v := range want {
		if got[k].Verdict != v {
			t.Errorf("%s: %s %v, want %s", k, got[k].Verdict, got[k].Reasons, v)
		}
	}
	p := got["test/ubuntu-latest/20.19.0"].Proposal
	if p == nil || p.Node != "20.19.0" || p.ID != "test-strict-20.19.0-ubuntu-latest" {
		t.Fatalf("proposal: %+v", p)
	}
	for _, s := range []string{"export CI=true", "(\nnpm ci\n)", "cd pkg", "npm test --strict"} {
		if !strings.Contains(p.Body, s) {
			t.Errorf("script lacks %q:\n%s", s, p.Body)
		}
	}
	if strings.Contains(p.Body, "secrets") {
		t.Errorf("secret leaked into the script:\n%s", p.Body)
	}
	if r := got["test/ubuntu-latest/22.x"]; r.Proposal.Node != "" || !strings.Contains(strings.Join(r.Reasons, ";"), `"22.x" isn't exact`) ||
		strings.Contains(r.Proposal.Body, "matrix.") || strings.Contains(strings.Join(r.Reasons, ";"), "expression") {
		t.Errorf("22.x: node=%q reasons=%v", r.Proposal.Node, r.Reasons)
	}
	if got["lint"].Proposal.Go != "1.26.10" {
		t.Errorf("lint go: %+v", got["lint"].Proposal)
	}
}

func TestNotCI(t *testing.T) {
	jobs, _ := Workflow("rel.yml", []byte("on: {release: {}}\njobs: {x: {runs-on: ubuntu-latest, steps: [{run: make}]}}\n"))
	if len(jobs) != 0 {
		t.Fatalf("release workflow analyzed: %v", jobs)
	}
}

func keys(m map[string]Job) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestEvalIf(t *testing.T) {
	combo := map[string]string{"node": "14", "os": "ubuntu-latest", "COV": "true"}
	for src, want := range map[string]bool{
		"${{ matrix.node == '14' || matrix.node == '16' }}":    true,
		"matrix.node != '14' && matrix.os != 'windows-latest'": false,
		"${{ !matrix.COV }}": false,
		"matrix.missing":     false,
		"(matrix.node == '18' || matrix.os == 'UBUNTU-LATEST')":         true,
		"success() && matrix.node == 14":                                true,
		"failure()":                                                     false,
		"runner.os != 'Windows'":                                        true,
		"runner.os == 'Windows' || runner.arch == 'ARM64'":              false,
		"github.event_name == 'push'":                                   false,
		"format('{0}-{{x}}', matrix.os) == 'ubuntu-latest-{x}'":         true,
		"contains(matrix.os, 'UBUNTU')":                                 true,
		"startsWith(matrix.node, '1') && endsWith(matrix.os, 'latest')": true,
		// prometheus's build: on a pull request every push clause is false, so
		// startsWith(github.ref, ...) never needs a value
		"!(github.event_name == 'push' && startsWith(github.ref, 'refs/tags/v2.'))\n&&\n!(github.event_name == 'push' && startsWith(github.ref, 'refs/tags/v3.'))": true,
	} {
		if v, ok := evalIf(src, combo); !ok || v != want {
			t.Errorf("%s: %v ok=%v, want %v", src, v, ok, want)
		}
	}
	for _, src := range []string{"github.ref == 'refs/heads/main'", "steps.x.outputs.y", "startsWith(github.ref, 'refs/tags/')", "format('{1}', matrix.os) == 'x'", "matrix.node ="} {
		if _, ok := evalIf(src, combo); ok {
			t.Errorf("%s: evaluated, want unsupported", src)
		}
	}
}

func TestVersionFile(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "go.mod"), []byte("module x\n\ngo 1.26.0\n\ntoolchain go1.26.10\n"), 0o644)
	os.WriteFile(filepath.Join(d, ".nvmrc"), []byte("v22.23.4\n"), 0o644)
	if v := versionFromFile(filepath.Join(d, "go.mod")); v != "1.26.10" {
		t.Errorf("go.mod: %q", v)
	}
	if v := versionFromFile(filepath.Join(d, ".nvmrc")); v != "22.23.4" {
		t.Errorf(".nvmrc: %q", v)
	}
}

func TestResolve(t *testing.T) {
	old := fetch
	defer func() { fetch = old }()
	fetch = func(url string) ([]byte, error) {
		if strings.Contains(url, "nodejs") {
			return []byte(`[{"version":"v26.1.0","lts":false},{"version":"v24.22.1","lts":"Krypton"},{"version":"v22.23.4","lts":"Jod"},{"version":"v22.23.3","lts":"Jod"}]`), nil
		}
		return []byte(`[{"version":"go1.27rc1","stable":false},{"version":"go1.26.10","stable":true},{"version":"go1.26.9","stable":true}]`), nil
	}
	releases.m = map[string][]release{}
	for spec, want := range map[string]string{"22": "22.23.4", "22.x": "22.23.4", "lts/*": "24.22.1", "22.23.3": "22.23.3", "21": ""} {
		if got, _ := resolve("Node", spec); got != want {
			t.Errorf("node %s: %q, want %q", spec, got, want)
		}
	}
	for spec, want := range map[string]string{"1.26": "1.26.10", "stable": "1.26.10", "1.27": ""} {
		if got, _ := resolve("Go", spec); got != want {
			t.Errorf("go %s: %q, want %q", spec, got, want)
		}
	}
}

func TestPrintable(t *testing.T) {
	if got := Printable("a\nb\x1b[31mc d‮e"); strings.ContainsAny(got, "\n\x1b ‮") {
		t.Fatalf("%q", got)
	}
}

func TestVersionFileUntrusted(t *testing.T) {
	d := t.TempDir()
	os.Symlink("/dev/zero", filepath.Join(d, ".nvmrc"))
	if v := versionFromFile(filepath.Join(d, ".nvmrc")); v != "" {
		t.Fatalf("followed a symlink: %q", v)
	}
	if v := exactVersion(d, "", `^x$`, "Node", "../../etc/passwd", func(string) {}, func(string) {}); v != "" {
		t.Fatalf("read outside the repo: %q", v)
	}
}

func TestNetworkInputsFlagged(t *testing.T) {
	src := "on: push\njobs:\n  a: {runs-on: ubuntu-latest, steps: [{run: 'curl -sfL https://x/install.sh | sh -s -- v1'}]}\n" +
		"  b: {runs-on: ubuntu-latest, steps: [{run: 'go install golang.org/x/vuln/cmd/govulncheck@latest'}]}\n" +
		"  c: {runs-on: ubuntu-latest, steps: [{run: 'make test'}]}\n" +
		"  d: {runs-on: ubuntu-latest, steps: [{run: 'sudo apt-get install -y shellcheck'}]}\n"
	jobs, _ := Workflow("w.yml", []byte(src))
	for _, j := range jobs {
		if want := j.ID != "c"; (j.Verdict == Review) != want {
			t.Errorf("%s: %s %v", j.ID, j.Verdict, j.Reasons)
		}
		if j.Proposal != nil && j.Proposal.Exclusive != (j.ID == "d") {
			t.Errorf("%s: exclusive=%v (only the sudo job should be)", j.ID, j.Proposal.Exclusive)
		}
	}
}

func TestNotChecks(t *testing.T) {
	for name, src := range map[string]string{
		"publish.yml":             "on: {push: {branches: [main]}}\njobs: {x: {runs-on: ubuntu-latest, steps: [{run: make}]}}\n",
		"tags.yml":                "on: {push: {tags: ['v*']}}\njobs: {x: {runs-on: ubuntu-latest, steps: [{run: make}]}}\n",
		"copilot-setup-steps.yml": "on: [push, pull_request]\njobs: {x: {runs-on: ubuntu-latest, steps: [{run: make}]}}\n",
	} {
		if jobs, _ := Workflow(name, []byte(src)); len(jobs) != 0 {
			t.Errorf("%s analyzed: %v", name, jobs)
		}
	}
	jobs, _ := Workflow("ci.yml", []byte("on: {push: {branches: ['**']}}\njobs:\n"+
		"  test: {runs-on: ubuntu-latest, steps: [{run: make}]}\n"+
		"  failed: {needs: test, if: '(!cancelled() && failure())', runs-on: ubuntu-latest, steps: [{run: echo failed}]}\n"))
	for _, j := range jobs {
		if want := map[string]string{"test": Task, "failed": Skip}[j.ID]; j.Verdict != want {
			t.Errorf("%s: %s %v, want %s", j.ID, j.Verdict, j.Reasons, want)
		}
	}
}

// The emulated step files behave like Actions': env and path written by one
// step are visible to the next, heredoc values included, and nothing is left behind.
func TestGitHubEnvEmulated(t *testing.T) {
	src := "on: pull_request\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n" +
		"      - run: |\n          echo FOO=bar >> \"$GITHUB_ENV\"\n          printf 'ML<<EOF\\nl1\\nl2\\nEOF\\n' >> $GITHUB_ENV\n          echo /opt/x/bin >> $GITHUB_PATH\n          echo \"$GITHUB_ENV\" > envfile\n" +
		"      - run: |\n          test \"$FOO\" = bar && test \"$ML\" = \"$(printf 'l1\\nl2')\" && case :$PATH: in *:/opt/x/bin:*) ;; *) exit 9;; esac\n"
	jobs, err := Workflow("ci.yml", []byte(src))
	if err != nil || len(jobs) != 1 || jobs[0].Verdict != Task || jobs[0].Proposal == nil {
		t.Fatalf("%v %+v", err, jobs)
	}
	d := t.TempDir()
	cmd := exec.Command("bash", "-eo", "pipefail", "-c", jobs[0].Proposal.Body)
	cmd.Dir = d
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, jobs[0].Proposal.Body)
	}
	b, _ := os.ReadFile(filepath.Join(d, "envfile"))
	if _, err := os.Stat(strings.TrimSpace(string(b))); !os.IsNotExist(err) {
		t.Errorf("temp file %s left behind", b)
	}
}

// Step scripts are copied verbatim: a heredoc's terminator must stay at column 0.
func TestHeredocStep(t *testing.T) {
	src := "on: pull_request\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n" +
		"      - run: |\n          cat <<EOF > out\n          one\n            two\n          EOF\n          test \"$(cat out)\" = \"$(printf 'one\\n  two')\"\n"
	jobs, err := Workflow("ci.yml", []byte(src))
	if err != nil || len(jobs) != 1 || jobs[0].Proposal == nil {
		t.Fatalf("%v %+v", err, jobs)
	}
	cmd := exec.Command("bash", "-eo", "pipefail", "-c", jobs[0].Proposal.Body)
	cmd.Dir = t.TempDir()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, jobs[0].Proposal.Body)
	}
}

func TestUniqueIDs(t *testing.T) {
	long := strings.Repeat("packages/eslint-config-", 4)
	jobs := []Job{
		{Workflow: "a.yml", Proposal: &Proposal{ID: "verify", Body: "x"}},
		{Workflow: "a.yml", Proposal: &Proposal{ID: "verify", Body: "x"}}, // same task twice: kept as one
		{Workflow: "b.yml", Proposal: &Proposal{ID: "verify", Body: "y"}},
		{Workflow: "c.yml", Proposal: &Proposal{ID: taskID("types", map[string]string{"dir": long}), Body: "z"}},
		{Workflow: "c.yml", Proposal: &Proposal{ID: taskID("types", map[string]string{"dir": long + "x"}), Body: "w"}},
	}
	uniqueIDs(jobs)
	valid := regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	seen := map[string]string{}
	for _, j := range jobs {
		id := j.Proposal.ID
		if !valid.MatchString(id) {
			t.Errorf("invalid id %q", id)
		}
		if b, ok := seen[id]; ok && b != j.Proposal.Body {
			t.Errorf("id %q shared by different scripts", id)
		}
		seen[id] = j.Proposal.Body
	}
	if jobs[2].Proposal.ID != "verify-b" || len(seen) != 4 {
		t.Errorf("ids: %v", seen)
	}
}

func TestObjectMatrixAndForeignCheckout(t *testing.T) {
	src := `on: pull_request
jobs:
  types:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        package: [{name: a, directory: pkgs/a}]
    steps:
      - run: cd ${{ matrix.package.directory }} && echo ${{ matrix.package.name }}
  neighbour:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/checkout@v4
        with: {repository: other/repo, path: other}
      - run: npm test
`
	jobs, err := Workflow("ci.yml", []byte(src))
	if err != nil || len(jobs) != 2 {
		t.Fatalf("%v %+v", err, jobs)
	}
	for _, j := range jobs {
		switch j.ID {
		case "types":
			if j.Proposal == nil || !strings.Contains(j.Proposal.Body, "cd pkgs/a && echo a") || j.Proposal.ID != "types-pkgs-a-a" {
				t.Errorf("object matrix not substituted: %+v %+v", j, j.Proposal)
			}
		case "neighbour":
			if j.Verdict != Skip {
				t.Errorf("foreign checkout not skipped: %+v", j)
			}
		}
	}
}

func TestNotChecksPrometheus(t *testing.T) {
	src := `on: pull_request
jobs:
  status:
    runs-on: ubuntu-latest
    needs: [build]
    if: always() && github.event_name == 'pull_request' && startsWith(github.base_ref, 'release-')
    steps:
      - if: ${{ contains(needs.*.result, 'failure') }}
        run: exit 1
  publish:
    runs-on: ubuntu-latest
    permissions: {contents: read, id-token: write}
    steps: [{run: make assets}]
  notes:
    runs-on: ubuntu-latest
    steps:
      - env: {BODY: "${{ github.event.pull_request.body }}"}
        run: ./check "$BODY"
  ui:
    runs-on: ubuntu-latest
    steps:
      - uses: prometheus/promci-artifacts/restore@v1
      - run: tar xf .tarballs/ui.tgz
  push-only:
    if: github.event_name == 'push'
    runs-on: ubuntu-latest
    steps: [{run: make}]
  preview:
    runs-on: ubuntu-latest
    steps: [{run: "pnpm build\npnpm dlx pkg-pr-new@0.0 publish --pnpm ./packages/vite"}]
  pack-check:
    runs-on: ubuntu-latest
    steps: [{run: npm publish --dry-run}]
`
	jobs, err := Workflow("ci.yml", []byte(src))
	if err != nil || len(jobs) != 7 {
		t.Fatalf("%v %+v", err, jobs)
	}
	for _, j := range jobs {
		if j.ID == "pack-check" {
			if j.Verdict == Skip {
				t.Errorf("a dry-run publish is a check: %q", j.Reasons)
			}
			continue
		}
		if j.Verdict != Skip {
			t.Errorf("%s: want skip, got %s %q", j.ID, j.Verdict, j.Reasons)
		}
	}
}

func TestShortCircuitAndOutputs(t *testing.T) {
	if v, ok := evalIf("failure() && steps.zizmor.outcome == 'failure'", nil); !ok || v {
		t.Errorf("false && unknown: %v ok=%v, want false", v, ok)
	}
	if v, ok := evalIf("always() || steps.x.outputs.y", nil); !ok || !v {
		t.Errorf("true || unknown: %v ok=%v, want true", v, ok)
	}
	if _, ok := evalIf("success() && steps.x.outputs.y", nil); ok {
		t.Error("true && unknown evaluated")
	}
	src := `on: pull_request
jobs:
  ui:
    runs-on: ubuntu-latest
    steps:
      - run: make tarball
      - uses: prometheus/promci-artifacts/save@v1
        with: {directory: .tarballs}
      - uses: actions/upload-artifact@v4
        with:
          path: |
            coverage
            /tmp/abs
            dist/*.tgz
      - if: failure()
        run: echo only on failure
`
	jobs, err := Workflow("ci.yml", []byte(src))
	if err != nil || len(jobs) != 1 || jobs[0].Proposal == nil {
		t.Fatalf("%v %+v", err, jobs)
	}
	p := jobs[0].Proposal
	if !slices.Equal(p.Outputs, []string{".tarballs", "coverage"}) || strings.Contains(p.Body, "only on failure") {
		t.Errorf("outputs %q, body %q", p.Outputs, p.Body)
	}
}

func TestWorkflowDefaultsWorkingDirectory(t *testing.T) {
	src := `on: pull_request
defaults: {run: {working-directory: scripts/release}}
jobs:
  test:
    runs-on: ubuntu-latest
    steps: [{run: yarn test}]
  own:
    runs-on: ubuntu-latest
    defaults: {run: {working-directory: web}}
    steps: [{run: yarn test}]
`
	jobs, err := Workflow("ci.yml", []byte(src))
	if err != nil || len(jobs) != 2 {
		t.Fatalf("%v %+v", err, jobs)
	}
	for _, j := range jobs {
		want := map[string]string{"own": "cd web\n", "test": "cd scripts/release\n"}[j.ID]
		if j.Proposal == nil || !strings.Contains(j.Proposal.Body, want) {
			t.Errorf("%s: want %q in %+v", j.ID, want, j.Proposal)
		}
	}
}

// A job that uses Go or Node without a setup action still gets an exact pin
// from the repo's version file, so its receipts are bound to a toolchain.
func TestPinWithoutSetupAction(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "go.mod"), []byte("module x\n\ngo 1.26.0\n\ntoolchain go1.26.10\n"), 0o644)
	os.WriteFile(filepath.Join(d, ".nvmrc"), []byte("22.23.4\n"), 0o644)
	os.MkdirAll(filepath.Join(d, ".github", "workflows"), 0o755)
	os.WriteFile(filepath.Join(d, ".github", "workflows", "ci.yml"), []byte(`on: pull_request
jobs:
  test-go:
    runs-on: ubuntu-latest
    container: {image: quay.io/prometheus/golang-builder:1.27-base}
    steps: [{run: make test}]
  build:
    runs-on: ubuntu-latest
    steps: [{run: make build}]
  ui:
    runs-on: ubuntu-latest
    steps: [{run: npm ci && npm test}]
  docs:
    runs-on: ubuntu-latest
    steps: [{run: ./check-links.sh}]
`), 0o644)
	jobs, err := Repo(d)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, j := range jobs {
		if j.Proposal != nil {
			got[j.ID] = j.Proposal.Go + "/" + j.Proposal.Node + "/" + j.Proposal.Container
		}
	}
	// test-go runs in its container (the image is the toolchain), build on the runner.
	if got["test-go"] != "//quay.io/prometheus/golang-builder:1.27-base" || got["build"] != "1.26.10//" || got["ui"] != "/22.23.4/" || got["docs"] != "//" {
		t.Fatalf("pins: %v", got)
	}
}

func TestContainerJobs(t *testing.T) {
	src := `on: pull_request
jobs:
  plain:
    runs-on: ubuntu-latest
    container: golang:1.27
    steps: [{run: go test ./...}]
  with-options:
    runs-on: ubuntu-latest
    container: {image: golang:1.27, options: --privileged}
    steps: [{run: go test ./...}]
`
	jobs, err := Workflow("ci.yml", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		switch j.ID {
		case "plain":
			if j.Verdict != Task || j.Proposal.Container != "golang:1.27" {
				t.Errorf("plain: %s %q %+v", j.Verdict, j.Reasons, j.Proposal)
			}
		case "with-options":
			if j.Verdict != Review || !strings.Contains(strings.Join(j.Reasons, " "), "with options") {
				t.Errorf("with-options: %s %q", j.Verdict, j.Reasons)
			}
		}
	}
}

func TestServicesTranslated(t *testing.T) {
	src := `on: pull_request
jobs:
  db:
    runs-on: ubuntu-latest
    services:
      postgres:
        image: postgres:16
        env: {POSTGRES_PASSWORD: pw}
        ports: [5432:5432]
        options: --health-cmd pg_isready --health-interval 5s
    steps: [{run: make test-db}]
  private:
    runs-on: ubuntu-latest
    services:
      app: {image: ghcr.io/x/app:1, credentials: {username: u, password: "${{ secrets.P }}"}}
    steps: [{run: make e2e}]
  privileged:
    runs-on: ubuntu-latest
    services:
      dind: {image: docker:dind, options: --privileged}
    steps: [{run: make e2e}]
`
	jobs, err := Workflow("ci.yml", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		switch j.ID {
		case "db":
			s := j.Proposal.Services["postgres"]
			if j.Verdict != Task || s.Image != "postgres:16" || s.Env["POSTGRES_PASSWORD"] != "pw" || len(s.Ports) != 1 || !strings.Contains(s.Options, "pg_isready") {
				t.Errorf("db: %s %q %+v", j.Verdict, j.Reasons, s)
			}
		case "private":
			if j.Verdict != Review || !strings.Contains(strings.Join(j.Reasons, " "), "credentials") {
				t.Errorf("private: %s %q", j.Verdict, j.Reasons)
			}
		case "privileged":
			if j.Verdict != Review || !strings.Contains(strings.Join(j.Reasons, " "), "won't pass to docker") {
				t.Errorf("privileged: %s %q", j.Verdict, j.Reasons)
			}
		}
	}
}

func TestARMAndSelfHostedStayInCI(t *testing.T) {
	src := "on: pull_request\njobs:\n  arm: {runs-on: ubuntu-24.04-arm, steps: [{run: make}]}\n  self: {runs-on: [self-hosted, linux], steps: [{run: make}]}\n"
	jobs, _ := Workflow("ci.yml", []byte(src))
	for _, j := range jobs {
		if j.Verdict != Skip {
			t.Errorf("%s: %s %q, want skip", j.ID, j.Verdict, j.Reasons)
		}
	}
}

// Publishing with JVM, .NET and Deno tools is publishing: those jobs stay in CI.
func TestPublishesJVMDotnet(t *testing.T) {
	for _, run := range []string{"./mvnw -B deploy", "mvn -s s.xml clean deploy -DskipTests", "./gradlew publish", "gradle publishToMavenCentral", "dotnet nuget push out/*.nupkg", "deno publish"} {
		if !publishes.MatchString(run) {
			t.Errorf("%q isn't seen as publishing", run)
		}
	}
	for _, run := range []string{"./mvnw -B verify", "./gradlew build", "dotnet test"} {
		if publishes.MatchString(run) {
			t.Errorf("%q seen as publishing", run)
		}
	}
}

// click's tests.yaml: runs-on, the name and an env value are matrix
// expressions with defaults. Each leg's values are known; other expressions
// (the event, steps) still aren't.
func TestMatrixValueExpressions(t *testing.T) {
	src := `on: pull_request
jobs:
  tests:
    name: ${{ matrix.name || matrix.python }}
    runs-on: ${{ matrix.os || 'ubuntu-latest' }}
    strategy:
      matrix:
        include:
          - {python: '3.13'}
          - {name: Mac, python: '3.13', os: macos-latest}
          - {name: PyPy, python: pypy, tox: pypy3}
    steps:
      - run: tox run
        env:
          TOX_ENV: ${{ matrix.tox || format('py{0}', matrix.python) }}
      - run: echo ${{ github.event_name || matrix.python }} ${{ success() || 'x' }}
`
	jobs, err := Workflow("tests.yaml", []byte(src))
	if err != nil || len(jobs) != 3 {
		t.Fatalf("%v %+v", err, jobs)
	}
	for _, j := range jobs {
		switch j.Name {
		case "3.13":
			if j.RunsOn != "ubuntu-latest" || j.Proposal == nil || !strings.Contains(j.Proposal.Body, "TOX_ENV=py3.13") {
				t.Errorf("leg 3.13: %+v %+v", j, j.Proposal)
			}
			if !strings.Contains(strings.Join(j.Reasons, ";"), "github.event_name") {
				t.Errorf("github.event_name substituted: %v", j.Reasons)
			}
		case "Mac":
			if j.Verdict != Skip {
				t.Errorf("Mac leg not skipped: %+v", j)
			}
		case "PyPy":
			if j.RunsOn != "ubuntu-latest" || !strings.Contains(j.Proposal.Body, "TOX_ENV=pypy3") {
				t.Errorf("leg PyPy: %+v %+v", j, j.Proposal)
			}
		default:
			t.Errorf("job name %q", j.Name)
		}
	}
}
