package analyze

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const dotnetIndexJSON = `{"releases-index":[
 {"channel-version":"12.0","latest-sdk":"12.0.100-preview.1.1","support-phase":"preview"},
 {"channel-version":"11.0","latest-sdk":"11.0.100-rc.3.26522.148","support-phase":"go-live"},
 {"channel-version":"10.0","latest-sdk":"10.0.403","support-phase":"active"},
 {"channel-version":"9.0","latest-sdk":"9.0.320","support-phase":"maintenance"},
 {"channel-version":"8.0","latest-sdk":"8.0.427","support-phase":"maintenance"}]}`

func fakeDotnetIndex(t *testing.T) {
	old := fetch
	t.Cleanup(func() { fetch = old; releases.m = map[string][]release{} })
	fetch = func(url string) ([]byte, error) {
		if url != dotnetIndex {
			t.Errorf("fetched %s", url)
		}
		return []byte(dotnetIndexJSON), nil
	}
	releases.m = map[string][]release{}
}

// What setup-dotnet resolves a spec to, from the releases index.
func TestDotnetResolve(t *testing.T) {
	fakeDotnetIndex(t)
	for spec, want := range map[string]string{"8.0.x": "8.0.427", "8.0": "8.0.427", "8.x": "8.0.427", "8": "8.0.427",
		"10.0.x": "10.0.403", "latest": "11.0.100-rc.3.26522.148", "12.0.x": "", "7.0": ""} {
		if got, _ := resolve("dotnet", spec); got != want {
			t.Errorf("%s: %q, want %q", spec, got, want)
		}
	}
}

func dotnetJob(t *testing.T, root, env, with string) Job {
	t.Helper()
	src := "on: pull_request\njobs:\n  test:\n    runs-on: ubuntu-24.04\n" + env +
		"    steps:\n      - uses: actions/setup-dotnet@v5\n        with: {" + with + "}\n      - run: dotnet test\n"
	jobs, err := workflowAt(root, "ci.yml", []byte(src))
	if err != nil || len(jobs) != 1 {
		t.Fatal(err, jobs)
	}
	return jobs[0]
}

const ownDir = "    env: {DOTNET_INSTALL_DIR: ~/.dotnet-sdk}\n"

// Next to the runner's preinstalled SDKs (lab finding 12): only a floating
// spec on the newest GA channel is the SDK dotnet uses there, and CI checks
// its runner has none newer (preinstalled:).
func TestDotnetNewestChannelShared(t *testing.T) {
	fakeDotnetIndex(t)
	for name, c := range map[string]struct{ with, globalJSON, review string }{
		"newest channel":                {"dotnet-version: 10.0.x", "", ""},
		"channel + latest":              {"dotnet-version: latest, dotnet-channel: \"10.0\"", "", ""},
		"with prepass off":              {"dotnet-version: 10.0.x, skip-lts-prepass: true", "", ""},
		"older channel":                 {"dotnet-version: 8.0.x", "", "isn't the newest GA SDK (10.0.403)"},
		"latest is an RC":               {"dotnet-version: latest", "", "isn't the newest GA SDK"},
		"exact":                         {"dotnet-version: 10.0.403", "", "only a single floating spec"},
		"several":                       {"dotnet-version: \"10.0.x\\n8.0.x\"", "", "several SDKs"},
		"global.json, test runner only": {"dotnet-version: 10.0.x", `{"test":{"runner":"Microsoft.Testing.Platform"}}`, ""}, // NSubstitute
		"global.json, latestFeature":    {"dotnet-version: 10.0.x", `{"sdk":{"version":"10.0.100","rollForward":"latestFeature"}}`, ""},
		"global.json, default":          {"dotnet-version: 10.0.x", `{"sdk":{"version":"10.0.100"}}`, ""}, // latestPatch: fails locally, never a pass
		"global.json, feature":          {"dotnet-version: 10.0.x", `{"sdk":{"version":"10.0.100","rollForward":"feature"}}`, "src/global.json can select an older SDK"},
		"global.json, comments":         {"dotnet-version: 10.0.x", "{// x\n}", "src/global.json can select"},
	} {
		d := t.TempDir()
		if c.globalJSON != "" {
			os.MkdirAll(filepath.Join(d, "src"), 0o755)
			os.WriteFile(filepath.Join(d, "src", "global.json"), []byte(c.globalJSON), 0o644)
		}
		j := dotnetJob(t, d, "", c.with)
		reasons := strings.Join(j.Reasons, "\n")
		if c.review != "" {
			if j.Verdict != Review || !strings.Contains(reasons, c.review) {
				t.Errorf("%s: %s %q", name, j.Verdict, reasons)
			}
			continue
		}
		if p := j.Proposal; j.Verdict != Task || p.Runtimes["dotnet"] != "10.0.403" || !p.Preinstalled["dotnet"] || p.Floating["dotnet"] == "" {
			t.Errorf("%s: %s %q %+v", name, j.Verdict, reasons, p)
		}
	}
	// In a directory of its own, the runner's SDKs don't matter.
	if j := dotnetJob(t, t.TempDir(), ownDir, "dotnet-version: 8.0.x, skip-lts-prepass: true"); j.Verdict != Task || j.Proposal.Preinstalled["dotnet"] {
		t.Errorf("own dir: %s %v preinstalled %v", j.Verdict, j.Reasons, j.Proposal.Preinstalled)
	}
}

func TestSetupDotnet(t *testing.T) {
	fakeDotnetIndex(t)
	root := t.TempDir()
	j := dotnetJob(t, root, ownDir, "dotnet-version: 8.0.404, skip-lts-prepass: true")
	if j.Verdict != Task || j.Proposal.Runtimes["dotnet"] != "8.0.404" || !strings.Contains(j.Proposal.Body, `"$HOME/.dotnet/tools" >> "$GITHUB_PATH"`) {
		t.Fatalf("%s %v %+v", j.Verdict, j.Reasons, j.Proposal)
	}

	os.WriteFile(filepath.Join(root, "global.json"), []byte("\xef\xbb\xbf{\"sdk\": {\"version\": \"8.0.404\"}}\n"), 0o644)
	if j := dotnetJob(t, root, ownDir, "skip-lts-prepass: true"); j.Verdict != Task || j.Proposal.Runtimes["dotnet"] != "8.0.404" {
		t.Fatalf("root global.json: %s %v", j.Verdict, j.Reasons)
	}

	// Floating specs pin today's SDK and are resolved again in CI (floating:).
	for name, c := range map[string]struct{ with, file, spec string }{
		"floating":      {"dotnet-version: 8.0.x, skip-lts-prepass: true", "", "8.0.x"},
		"latestFeature": {"global-json-file: sub/global.json, skip-lts-prepass: true", `{"sdk":{"version":"8.0.100","rollForward":"latestFeature"}}`, "8.0"},
	} {
		d := t.TempDir()
		if c.file != "" {
			os.MkdirAll(filepath.Join(d, "sub"), 0o755)
			os.WriteFile(filepath.Join(d, "sub", "global.json"), []byte(c.file), 0o644)
		}
		if j := dotnetJob(t, d, ownDir, c.with); j.Verdict != Task || j.Proposal.Runtimes["dotnet"] == "" || j.Proposal.Floating["dotnet"] != c.spec {
			t.Errorf("%s: %s %v %+v", name, j.Verdict, j.Reasons, j.Proposal)
		}
	}

	for name, c := range map[string]struct{ env, with, file, want string }{
		"preinstalled": {"", "dotnet-version: 8.0.404", "", "/usr/share/dotnet"},
		"system dir":   {"    env: {DOTNET_INSTALL_DIR: /usr/share/dotnet}\n", "dotnet-version: 8.0.404, skip-lts-prepass: true", "", "/usr/share/dotnet"},
		"lts prepass":  {ownDir, "dotnet-version: 8.0.404", "", "skip-lts-prepass"},
		"band":         {ownDir, "dotnet-version: 8.0.4xx, skip-lts-prepass: true", "", "can't check again"},
		"several":      {ownDir, "dotnet-version: \"8.0.404\\n9.0.320\", skip-lts-prepass: true", "", "several SDKs (8.0.404, 9.0.320)"},
		"preview":      {ownDir, "dotnet-version: 10.0.x, dotnet-quality: preview, skip-lts-prepass: true", "", "preview build"},
		"workloads":    {ownDir, "dotnet-version: 8.0.404, workloads: wasm-tools, skip-lts-prepass: true", "", "read-only"},
		"feed":         {ownDir, "dotnet-version: 8.0.404, source-url: 'https://nuget.pkg.github.com/o/index.json', skip-lts-prepass: true", "", "credentials"},
		"latestPatch":  {ownDir, "global-json-file: sub/global.json, skip-lts-prepass: true", `{"sdk":{"version":"8.0.400","rollForward":"latestPatch"}}`, `"8.0.4xx"`},
		"latestMajor":  {ownDir, "global-json-file: sub/global.json, skip-lts-prepass: true", `{"sdk":{"version":"8.0.400","rollForward":"latestMajor"}}`, "newest SDK on the runner"},
		"comments":     {ownDir, "global-json-file: sub/global.json, skip-lts-prepass: true", "{// pinned\n\"sdk\":{\"version\":\"8.0.400\"}}", "plain JSON"},
		"escapes":      {ownDir, "global-json-file: ../global.json, skip-lts-prepass: true", "", "can't be read"},
	} {
		d := t.TempDir()
		if c.file != "" {
			os.MkdirAll(filepath.Join(d, "sub"), 0o755)
			os.WriteFile(filepath.Join(d, "sub", "global.json"), []byte(c.file), 0o644)
		}
		j := dotnetJob(t, d, c.env, c.with)
		if j.Verdict != Review || !strings.Contains(strings.Join(j.Reasons, "\n"), c.want) {
			t.Errorf("%s: %s %v", name, j.Verdict, j.Reasons)
		}
	}

	// No version and no global.json: the runner's own SDKs, never a guess.
	if j := dotnetJob(t, t.TempDir(), ownDir, "skip-lts-prepass: true"); j.Verdict != Review || j.Proposal.Runtimes["dotnet"] != "" {
		t.Fatalf("nothing to pin: %s %v %+v", j.Verdict, j.Reasons, j.Proposal.Runtimes)
	}
	// latestFeature still drafts the resolved pin for the person reviewing it.
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "global.json"), []byte(`{"sdk":{"version":"8.0.100","rollForward":"latestFeature"}}`), 0o644)
	if j := dotnetJob(t, d, ownDir, "skip-lts-prepass: true"); j.Proposal.Runtimes["dotnet"] != "8.0.427" {
		t.Fatalf("latestFeature: %+v %v", j.Proposal.Runtimes, j.Reasons)
	}
}
