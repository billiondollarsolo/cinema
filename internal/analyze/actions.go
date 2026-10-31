package analyze

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Translations of common actions into pinned, verifiable commands, so jobs that
// use them can be tasks instead of review drafts (or hollow ones, finding 48).

var (
	exactSemver = regexp.MustCompile(`^v?(\d+\.\d+\.\d+)$`)
	envRef      = regexp.MustCompile(`\$\{\{\s*env\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)
)

// withEnv substitutes ${{ env.X }} from literal workflow/job env values.
func withEnv(s string, env map[string]string) string {
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := env[envRef.FindStringSubmatch(m)[1]]; ok && !strings.Contains(v, "${{") {
			return v
		}
		return m
	})
}

// golangciStep runs golangci-lint the way golangci-lint-action does, with the
// official release binary of the exact version, checked against the release's
// checksums (finding 14: the same version built from source can report other
// issues), and a private cache (finding 15).
func golangciStep(root string, with map[string]any, env map[string]string, note func(string)) (string, string) {
	v := withEnv(str(with["version"]), env)
	if f := str(with["version-file"]); v == "" && f != "" && filepath.IsLocal(filepath.FromSlash(f)) {
		v = versionFromFile(root, f)
	}
	m := exactSemver.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return "", fmt.Sprintf("uses golangci-lint-action with version %q: pin an exact version (vX.Y.Z) to translate it", v)
	}
	ver := m[1]
	tarball := "golangci-lint-" + ver + "-linux-amd64.tar.gz"
	base := "https://github.com/golangci/golangci-lint/releases/download/v" + ver + "/"
	sums, err := fetchAction(base + "golangci-lint-" + ver + "-checksums.txt")
	if err != nil {
		return "", "uses golangci-lint-action " + ver + ": can't read the release checksums (" + err.Error() + ")"
	}
	sha := ""
	for _, l := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[1] == tarball {
			sha = f[0]
		}
	}
	if len(sha) != 64 {
		return "", "uses golangci-lint-action " + ver + ": no checksum for " + tarball
	}
	note("golangci-lint-action → the official golangci-lint " + ver + " release binary, SHA-256 checked")
	args := strings.TrimSpace(str(with["args"]))
	return fmt.Sprintf(`gl=$(mktemp -d)
curl -sSfL -o "$gl/gl.tgz" %s%s
echo "%s  $gl/gl.tgz" | sha256sum -c - >/dev/null
tar -xzf "$gl/gl.tgz" -C "$gl" --strip-components=1
GOLANGCI_LINT_CACHE="$gl/cache" "$gl/golangci-lint" run %s`, base, tarball, sha, args), ""
}

// pnpmStep installs the pnpm pnpm/action-setup would (its version input, else
// package.json's packageManager), into the run's scratch dir (PrivateGlobals).
func pnpmStep(root string, with map[string]any, env map[string]string, note func(string)) (string, string) {
	v := strings.TrimPrefix(strings.TrimSpace(withEnv(str(with["version"]), env)), "v")
	if v == "" {
		var pkg struct {
			PackageManager string `json:"packageManager"`
		}
		if b, err := readRepo(root, "package.json"); err == nil && json.Unmarshal(b, &pkg) == nil {
			if pm, ok := strings.CutPrefix(pkg.PackageManager, "pnpm@"); ok {
				v, _, _ = strings.Cut(pm, "+")
			}
		}
	}
	if v == "latest" {
		b, err := fetchAction("https://registry.npmjs.org/pnpm/latest")
		var latest struct {
			Version string `json:"version"`
		}
		if err != nil || json.Unmarshal(b, &latest) != nil || exactSemver.FindString(latest.Version) == "" {
			return "", "uses pnpm/action-setup with version latest: can't resolve it today"
		}
		note(`pnpm "latest" resolved to ` + latest.Version + ", the newest release today: CI will move on; this pin won't")
		v = latest.Version
	}
	if exactSemver.FindString(v) == "" {
		return "", fmt.Sprintf("uses pnpm/action-setup with version %q: pin an exact version to translate it", v)
	}
	script := "npm install -g pnpm@" + v
	if b := strings.TrimSpace(str(with["run_install"])); b == "true" {
		script += "\npnpm install"
	} else if b != "" && b != "false" {
		return "", "uses pnpm/action-setup with run_install options: translate them"
	}
	return script, ""
}
