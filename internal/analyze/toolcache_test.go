package analyze

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeToolCache lays out a tool cache as @actions/tool-cache does:
// <folder>/<version>/<arch> plus <arch>.complete, made at the image's build
// (long before boot) unless fresh.
func fakeToolCache(t *testing.T, entries map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
	old := time.Now().Add(-72 * time.Hour)
	for e, state := range entries {
		p := filepath.Join(dir, filepath.FromSlash(e), arch)
		os.MkdirAll(p, 0o755)
		if state == "incomplete" {
			continue
		}
		os.WriteFile(p+".complete", nil, 0o644)
		if state != "fresh" {
			os.Chtimes(p+".complete", old, old)
		}
	}
	return dir
}

func TestToolCache(t *testing.T) {
	oldBoot := bootTime
	t.Cleanup(func() { bootTime = oldBoot })
	bootTime = func() time.Time { return time.Now().Add(-time.Hour) }
	dir := fakeToolCache(t, map[string]string{
		"go/1.26.9": "", "go/1.25.14": "", "go/1.26.10": "incomplete", "go/1.27.0-rc.1": "", "go/1.26.11": "fresh", "go/notes": "",
		"node/22.23.3": "", "Python/3.14.9": "",
		"Java_Temurin-Hotspot_jdk/17.0.21-9": "", "Java_Temurin-Hotspot_jdk/21.0.13-9": "", "Java_Temurin-Hotspot_jdk/26.0.0-ea.3": "",
	})
	for rt, want := range map[string][]string{
		// 1.26.10 is incomplete, 1.27.0-rc.1 a prerelease, 1.26.11 installed
		// by this job (after boot): none is what a fresh runner gives.
		"go":     {"1.25.14", "1.26.9"},
		"node":   {"22.23.3"},
		"python": {"3.14.9"},
		"java":   {"temurin-17.0.21+9", "temurin-21.0.13+9"},
		"dotnet": nil,
		"bun":    nil,
	} {
		got, err := ToolCache(dir, rt)
		slices.Sort(got)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%s: %v %v, want %v", rt, got, err, want)
		}
	}
	// The JDK's release file names a respin, which the directory can't.
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
	os.WriteFile(filepath.Join(dir, "Java_Temurin-Hotspot_jdk", "17.0.21-9", arch, "release"), []byte("JAVA_VERSION=\"17.0.21\"\nJAVA_RUNTIME_VERSION=\"17.0.21+9-LTS\"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "Java_Temurin-Hotspot_jdk", "21.0.13-9", arch, "release"), []byte("JAVA_RUNTIME_VERSION=\"21.0.13.1+1\"\n"), 0o644)
	if got, _ := ToolCache(dir, "java"); !slices.Contains(got, "temurin-17.0.21+9") || !slices.Contains(got, "temurin-21.0.13+101") {
		t.Errorf("java from release files: %v", got)
	}
	if got, err := ToolCache(t.TempDir(), "go"); err != nil || got != nil {
		t.Errorf("no folder: %v %v (the action downloads)", got, err)
	}
}

// What each action picks on a runner with this cache (lab logs, 2026-10).
func TestOnRunner(t *testing.T) {
	cache := map[string][]string{
		"go":     {"1.24.13", "1.25.14", "1.26.9"},
		"node":   {"22.23.3", "24.21.0"},
		"python": {"3.12.15", "3.14.9", "3.13.17"},
		"java":   {"temurin-17.0.21+9", "temurin-21.0.12+7", "temurin-21.0.13+9"},
	}
	for _, c := range []struct {
		rt, spec, newest string
		checkLatest      bool
		want             string
	}{
		{"go", "1.26", "1.26.10", false, "1.26.9"},     // setup-go: tc.find('go', '1.26')
		{"go", "1.26.x", "1.26.10", false, "1.26.9"},   // cobra's 1.24.x found 1.24.13 in the cache
		{"go", "1.26", "1.26.10", true, "1.26.10"},     // check-latest: the newest, cached or not
		{"go", "stable", "1.27.3", false, "1.27.3"},    // resolved before the cache
		{"go", "1.23", "1.23.12", false, "1.23.12"},    // not cached: downloaded
		{"node", "lts/*", "24.22.2", false, "24.21.0"}, // express: lts/* found 24.21.0 in the cache
		{"node", "22", "22.23.4", false, "22.23.3"},
		{"node", "26", "26.13.0", false, "26.13.0"},
		{"node", "latest", "26.13.0", false, "26.13.0"},
		{"python", "3.14", "3.14.10", false, "3.14.9"}, // click
		{"python", "3.1", "3.1.4", false, "3.1.4"},     // 3.1 isn't a prefix of 3.14.9
		{"python", "3.x", "3.14.10", false, "3.14.9"},
		{"java", "temurin-21", "temurin-21.0.13+9", false, "temurin-21.0.13+9"},
		{"java", "temurin-21.0", "temurin-21.0.14+7", false, "temurin-21.0.13+9"},
		{"java", "temurin-17", "temurin-17.0.22+5", true, "temurin-17.0.22+5"},
		{"java", "temurin-25", "temurin-25.0.5+8", false, "temurin-25.0.5+8"},
	} {
		if got := OnRunner(c.rt, c.spec, c.newest, c.checkLatest, cache[c.rt]); got != c.want {
			t.Errorf("%s %s (check-latest %v): %s, want %s", c.rt, c.spec, c.checkLatest, got, c.want)
		}
	}
}

const readme2604 = `# Ubuntu 26.04
- OS Version: 26.04 LTS
- Image Version: 20261026.180.1

### Cached Tools

#### Go
- 1.24.13
- 1.25.14
- 1.26.9

#### Node.js
- 22.23.3
- 24.21.0

#### Python
- 3.12.7
- 3.14.9

#### PyPy
- 3.11.17 [PyPy 8.0.1]

### Java
| Version            | Environment Variable |
| ------------------ | -------------------- |
| 11.0.32+1          | JAVA_HOME_11_X64     |
| 17.0.21+9          | JAVA_HOME_17_X64     |
| 21.0.5+11 (default) | JAVA_HOME_21_X64    |

### .NET Tools
- .NET Core SDK: 8.0.133, 10.0.403
`

// fakeImageReport serves the runner image report for 26.04 (ubuntu-latest).
func fakeImageReport(url string) ([]byte, bool) {
	if strings.Contains(url, "runner-images") && strings.HasSuffix(url, "Ubuntu2604-Readme.md") {
		return []byte(readme2604), true
	}
	return nil, false
}

func TestImageCached(t *testing.T) {
	old := fetch
	t.Cleanup(func() { fetch, releases.m = old, map[string][]release{} })
	releases.m = map[string][]release{}
	fetch = func(url string) ([]byte, error) {
		if b, ok := fakeImageReport(url); ok {
			return b, nil
		}
		if strings.Contains(url, "adoptium") && strings.Contains(url, "page=0") {
			return []byte(`{"versions":[{"semver":"21.0.5+11.0.LTS"},{"semver":"17.0.21+9"},{"semver":"11.0.32+101"},{"semver":"11.0.32+9"}]}`), nil
		}
		return nil, errors.New("offline")
	}
	for rt, want := range map[string][]string{
		"go": {"1.24.13", "1.25.14", "1.26.9"}, "node": {"22.23.3", "24.21.0"}, "python": {"3.12.7", "3.14.9"},
		// The report's 11.0.32+1 is the respin 11.0.32.1+1, Adoptium's 11.0.32+101 (gson's CI, 2026-10-30).
		"java": {"temurin-11.0.32+101", "temurin-17.0.21+9", "temurin-21.0.5+11"},
	} {
		got, image, err := imageCached("ubuntu-latest", rt)
		if err != nil || image != "20261026.180.1" || !slices.Equal(got, want) {
			t.Errorf("%s: %v %q %v, want %v", rt, got, image, err, want)
		}
	}
	if _, _, err := imageCached("ubuntu-24.04", "go"); err == nil {
		t.Error("24.04's report is offline here: an error")
	}
	if _, _, err := imageCached("my-big-runner", "go"); err == nil {
		t.Error("a label with no image report: an error")
	}
}

func TestNewerSDKs(t *testing.T) {
	d := t.TempDir()
	for _, v := range []string{"8.0.427", "9.0.320", "10.0.114", "10.0.403"} {
		os.Mkdir(filepath.Join(d, v), 0o755)
	}
	if got, err := NewerSDKs(d, "10.0.403"); err != nil || len(got) != 0 {
		t.Fatalf("nothing newer: %v %v", got, err)
	}
	os.Mkdir(filepath.Join(d, "10.0.404"), 0o755)
	os.Mkdir(filepath.Join(d, "11.0.100-rc.2.26502.107"), 0o755)
	if got, err := NewerSDKs(d, "10.0.403"); err != nil || !slices.Equal(got, []string{"10.0.404", "11.0.100-rc.2.26502.107"}) {
		t.Fatalf("newer: %v %v", got, err)
	}
	if _, err := NewerSDKs(filepath.Join(d, "none"), "10.0.403"); err == nil {
		t.Fatal("no SDK directory: an error (the job runs)")
	}
}

// setup-go and setup-node without check-latest: pinned to the runner image's
// cached version (CI checks its own tool cache); with it, or for stable, the
// newest release.
func TestSetupGoNodeToolCache(t *testing.T) {
	old := fetch
	t.Cleanup(func() { fetch, releases.m = old, map[string][]release{} })
	releases.m = map[string][]release{}
	fetch = func(url string) ([]byte, error) {
		if b, ok := fakeImageReport(url); ok {
			return b, nil
		}
		switch {
		case strings.Contains(url, "nodejs"):
			return []byte(`[{"version":"v26.13.0","lts":false},{"version":"v24.22.2","lts":"Krypton"},{"version":"v24.21.0","lts":"Krypton"},{"version":"v22.23.4","lts":"Jod"}]`), nil
		case strings.Contains(url, "go.dev"):
			return []byte(`[{"version":"go1.27.3","stable":true},{"version":"go1.26.10","stable":true},{"version":"go1.26.9","stable":true}]`), nil
		}
		return nil, errors.New("offline")
	}
	jobs, err := Workflow("ci.yml", []byte(`on: pull_request
jobs:
  go:
    runs-on: ubuntu-latest
    steps: [{uses: actions/setup-go@v6, with: {go-version: "1.26"}}, {run: go test -count=1 ./...}]
  go-latest:
    runs-on: ubuntu-latest
    steps: [{uses: actions/setup-go@v6, with: {go-version: "1.26", check-latest: true}}, {run: go test -count=1 ./...}]
  go-stable:
    runs-on: ubuntu-latest
    steps: [{uses: actions/setup-go@v6, with: {go-version: stable}}, {run: go test -count=1 ./...}]
  go-expr:
    runs-on: ubuntu-latest
    steps: [{uses: actions/setup-go@v6, with: {go-version: "1.26", check-latest: "${{ inputs.fresh }}"}}, {run: go test -count=1 ./...}]
  node-lts:
    runs-on: ubuntu-latest
    steps: [{uses: actions/setup-node@v6, with: {node-version: lts/*}}, {run: npm test}]
  node-26:
    runs-on: ubuntu-latest
    steps: [{uses: actions/setup-node@v6, with: {node-version: 26}}, {run: npm test}]
  node-offline:
    runs-on: ubuntu-24.04
    steps: [{uses: actions/setup-node@v6, with: {node-version: 22}}, {run: npm test}]
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.ID == "go-expr" {
			if j.Verdict != Review || !strings.Contains(strings.Join(j.Reasons, ";"), "check-latest uses an expression") {
				t.Errorf("go-expr: %s %v", j.Verdict, j.Reasons)
			}
			continue
		}
		want := map[string]struct {
			pin string
			pre bool
		}{
			"go": {"1.26.9", true}, "go-latest": {"1.26.10", false}, "go-stable": {"1.27.3", true},
			"node-lts": {"24.21.0", true}, "node-26": {"26.13.0", true},
			"node-offline": {"22.23.4", true}, // 24.04's image report unreadable: the newest, and CI checks
		}[j.ID]
		p := j.Proposal
		pin := p.Go + p.Node
		if j.Verdict != Task || pin != want.pin || p.Preinstalled[strings.SplitN(j.ID, "-", 2)[0]] != want.pre {
			t.Errorf("%s: %s %v pin %s preinstalled %v, want %s %v", j.ID, j.Verdict, j.Reasons, pin, p.Preinstalled, want.pin, want.pre)
		}
	}
}
