package analyze

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestActionTranslations(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	fetchAction = func(url string) ([]byte, error) {
		switch url {
		case "https://github.com/golangci/golangci-lint/releases/download/v2.1.6/golangci-lint-2.1.6-checksums.txt":
			return []byte(sha + "  golangci-lint-2.1.6-linux-amd64.tar.gz\n" + strings.Repeat("0", 64) + "  golangci-lint-2.1.6-darwin-arm64.tar.gz\n"), nil
		case "https://registry.npmjs.org/pnpm/latest":
			return []byte(`{"version":"10.4.1"}`), nil
		}
		return nil, errors.New("404 " + url)
	}
	defer func() { fetchAction = func(string) ([]byte, error) { return nil, errors.New("offline") } }()
	d := t.TempDir()
	os.MkdirAll(filepath.Join(d, ".github", "workflows"), 0o755)
	os.WriteFile(filepath.Join(d, ".github", "golangci-lint-version"), []byte("v2.1.6\n"), 0o644)
	os.WriteFile(filepath.Join(d, "package.json"), []byte(`{"packageManager":"pnpm@9.15.0+sha512.abc"}`), 0o644)
	os.WriteFile(filepath.Join(d, ".github", "workflows", "ci.yml"), []byte(`on: pull_request
env: {GOLANGCI_LINT_VERSION: v2.1.6}
jobs:
  lint-literal:
    runs-on: ubuntu-latest
    steps: [{uses: golangci/golangci-lint-action@v9, with: {version: v2.1.6, args: --timeout 5m}}]
  lint-env:
    runs-on: ubuntu-latest
    steps: [{uses: golangci/golangci-lint-action@v9, with: {version: "${{ env.GOLANGCI_LINT_VERSION }}"}}]
  lint-file:
    runs-on: ubuntu-latest
    steps: [{uses: golangci/golangci-lint-action@v9, with: {version-file: .github/golangci-lint-version}}]
  lint-latest:
    runs-on: ubuntu-latest
    steps: [{uses: golangci/golangci-lint-action@v9, with: {version: latest}}]
  pnpm-latest:
    runs-on: ubuntu-latest
    steps: [{uses: pnpm/action-setup@v4, with: {version: latest}}, {run: pnpm test}]
  pnpm-pm:
    runs-on: ubuntu-latest
    steps: [{uses: pnpm/action-setup@v4, with: {run_install: true}}, {run: pnpm test}]
`), 0o644)
	jobs, err := Repo(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		body := ""
		if j.Proposal != nil {
			body = j.Proposal.Body
		}
		ok := func(cond bool) {
			if !cond {
				t.Errorf("%s: %s %q\n%s", j.ID, j.Verdict, j.Reasons, body)
			}
		}
		switch j.ID {
		case "lint-literal":
			ok(j.Verdict == Task && strings.Contains(body, sha+"  $gl/gl.tgz") && strings.Contains(body, "golangci-lint-2.1.6-linux-amd64.tar.gz") && strings.Contains(body, "run --timeout 5m"))
		case "lint-env", "lint-file":
			ok(j.Verdict == Task && strings.Contains(body, sha))
		case "lint-latest":
			ok(j.Verdict == Review && strings.Contains(strings.Join(j.Reasons, " "), "pin an exact version"))
		case "pnpm-latest":
			ok(j.Verdict == Task && strings.Contains(body, "npm install -g pnpm@10.4.1"))
		case "pnpm-pm":
			ok(j.Verdict == Task && strings.Contains(body, "npm install -g pnpm@9.15.0\npnpm install"))
		}
	}
}
