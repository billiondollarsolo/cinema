package analyze

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBunDenoReleases serves both release lists offline.
func fakeBunDenoReleases(t *testing.T) {
	oldF, oldR := fetch, bunRefs
	t.Cleanup(func() { fetch, bunRefs = oldF, oldR; releases.m = map[string][]release{} })
	releases.m = map[string][]release{}
	fetch = func(url string) ([]byte, error) {
		if url != "https://dl.deno.land/versions.json" {
			return nil, errors.New("offline: " + url)
		}
		return []byte(`{"cli":["v2.6.0-rc.1","v2.5.6","v2.5.10","v2.4.5","v1.46.3"]}`), nil
	}
	bunRefs = func() ([]byte, error) {
		var b strings.Builder
		for _, r := range []string{"refs/tags/bun-v1.2.9", "refs/tags/bun-v1.2.23", "refs/tags/bun-v1.3.0", "refs/tags/bun-v0.0.0-8", "refs/tags/canary"} {
			b.WriteString(pktLine("0123456789012345678901234567890123456789 " + r + "\n"))
		}
		return []byte(b.String() + "0000"), nil
	}
}

func TestBunDenoReleases(t *testing.T) {
	fakeBunDenoReleases(t)
	for spec, want := range map[string]string{"latest": "1.3.0", "1.2": "1.2.23", "1.x": "1.3.0", "1.2.9": "1.2.9", "2": ""} {
		if got, _ := resolve("bun", spec); got != want {
			t.Errorf("bun %s: %q, want %q", spec, got, want)
		}
	}
	for spec, want := range map[string]string{"2.x": "2.5.10", "latest": "2.5.10", "2.5": "2.5.10", "1": "1.46.3", "2.6": ""} {
		if got, _ := resolve("deno", spec); got != want {
			t.Errorf("deno %s: %q, want %q", spec, got, want)
		}
	}
	bunRefs = func() ([]byte, error) { return []byte("zzzz"), nil }
	releases.m = map[string][]release{}
	if _, err := resolve("bun", "latest"); err == nil {
		t.Error("a garbled git response must be an error")
	}
}

// setup-bun and setup-deno translate to runtime pins, as the actions pick the
// version; what cinema can't reproduce or pin goes to review.
func TestSetupBunDeno(t *testing.T) {
	fakeBunDenoReleases(t)
	root := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".bun-version", "1.2.9\n")
	write(".tool-versions", "nodejs 22.1.0\nbun 1.2.23\ndeno 2.4.5\n")
	write(".dvmrc", "v2.5.6\n")
	write("package.json", `{"packageManager":"bun@1.2.9"}`)
	type want struct {
		verdict string
		pin     string
		why     string // in the reasons
	}
	cases := map[string]struct {
		with string
		want want
	}{
		"bun-exact":      {"oven-sh/setup-bun@v2\n        with: {bun-version: 1.2.23}", want{Task, "bun=1.2.23", ""}},
		"bun-v":          {"oven-sh/setup-bun@v2\n        with: {bun-version: v1.2.23}", want{Task, "bun=1.2.23", ""}},
		"bun-file":       {"oven-sh/setup-bun@v2\n        with: {bun-version-file: .bun-version}", want{Task, "bun=1.2.9", ""}},
		"bun-tools":      {"oven-sh/setup-bun@v2\n        with: {bun-version-file: .tool-versions}", want{Task, "bun=1.2.23", ""}},
		"bun-input-wins": {"oven-sh/setup-bun@v2\n        with: {bun-version: 1.3.0, bun-version-file: .bun-version}", want{Task, "bun=1.3.0", ""}},
		"bun-pkg":        {"oven-sh/setup-bun@v2", want{Task, "bun=1.2.9", ""}}, // package.json's packageManager
		"bun-missing":    {"oven-sh/setup-bun@v2\n        with: {bun-version-file: .nope}", want{Task, "bun=1.2.9", ""}},
		"bun-latest":     {"oven-sh/setup-bun@v2\n        with: {bun-version: latest}", want{Review, "bun=1.3.0", "floating: {bun: \"latest\"}"}},
		"bun-range":      {"oven-sh/setup-bun@v2\n        with: {bun-version: ^1.2.0}", want{Review, "", "isn't exact"}},
		"bun-canary":     {"oven-sh/setup-bun@v2\n        with: {bun-version: canary}", want{Review, "", "canary"}},
		"bun-url":        {"oven-sh/setup-bun@v2\n        with: {bun-download-url: 'https://example.com/bun.zip'}", want{Review, "", "bun-download-url"}},
		"bun-registries": {"oven-sh/setup-bun@v2\n        with: {bun-version: 1.2.23, registries: 'https://registry.npmjs.org/'}", want{Review, "bun=1.2.23", "bunfig.toml"}},
		"bun-nocache":    {"oven-sh/setup-bun@v2\n        with: {bun-version: 1.2.23, no-cache: true}", want{Task, "bun=1.2.23", ""}},
		"deno-default":   {"denoland/setup-deno@v2", want{Review, "deno=2.5.10", "floating: {deno: \"2.x\"}"}},
		"deno-exact":     {"denoland/setup-deno@v2\n        with: {deno-version: v2.4.5, cache: true}", want{Task, "deno=2.4.5", ""}},
		"deno-dvmrc":     {"denoland/setup-deno@v2\n        with: {deno-version-file: .dvmrc}", want{Task, "deno=2.5.6", ""}},
		"deno-file-wins": {"denoland/setup-deno@v2\n        with: {deno-version: 2.5.6, deno-version-file: .tool-versions}", want{Task, "deno=2.4.5", ""}},
		"deno-missing":   {"denoland/setup-deno@v2\n        with: {deno-version-file: .nope}", want{Review, "", "can't read"}},
		"deno-canary":    {"denoland/setup-deno@v2\n        with: {deno-version: canary}", want{Review, "", "canary"}},
		"deno-hash":      {"denoland/setup-deno@v2\n        with: {deno-version: 0123456789abcdef0123456789abcdef01234567}", want{Review, "", "canary"}},
		"deno-rc":        {"denoland/setup-deno@v2\n        with: {deno-version: rc}", want{Review, "", "exact release"}},
		"deno-name":      {"denoland/setup-deno@v2\n        with: {deno-version: 2.4.5, deno-binary-name: deno2}", want{Review, "", "deno-binary-name"}},
	}
	for name, tc := range cases {
		src := "on: pull_request\njobs:\n  t:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: " + tc.with + "\n      - run: bun test\n"
		jobs, err := workflowAt(root, "ci.yml", []byte(src))
		if err != nil || len(jobs) != 1 {
			t.Fatalf("%s: %v %v", name, err, jobs)
		}
		j := jobs[0]
		pin := ""
		if j.Proposal != nil {
			for k, v := range j.Proposal.Runtimes {
				pin = k + "=" + v
			}
		}
		reasons := strings.Join(j.Reasons, "; ")
		if j.Verdict != tc.want.verdict || pin != tc.want.pin || (tc.want.why != "" && !strings.Contains(reasons, tc.want.why)) {
			t.Errorf("%s: %v %q %q, want %+v", name, j.Verdict, pin, reasons, tc.want)
		}
		if pin != "" {
			path := `"$HOME/.bun/bin" >> "$GITHUB_PATH"`
			if strings.HasPrefix(pin, "deno") {
				path = `"${DENO_INSTALL_ROOT:-$HOME/.deno/bin}" >> "$GITHUB_PATH"`
			}
			if !strings.Contains(j.Proposal.Body, path) {
				t.Errorf("%s: global installs' bin dir isn't put on PATH:\n%s", name, j.Proposal.Body)
			}
		}
	}
	// Without the repo's files, setup-bun's default lookup can't be reproduced.
	jobs, _ := Workflow("ci.yml", []byte("on: pull_request\njobs:\n  t:\n    runs-on: ubuntu-latest\n    steps: [{uses: oven-sh/setup-bun@v2}, {run: bun test}]\n"))
	if jobs[0].Verdict != Review || jobs[0].Proposal.Runtimes["bun"] != "" {
		t.Errorf("no repo: %v %v", jobs[0].Verdict, jobs[0].Reasons)
	}
}
