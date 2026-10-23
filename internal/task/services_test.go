package task

import (
	"slices"
	"strings"
	"testing"
)

func TestShellWords(t *testing.T) {
	got, err := shellWords(`--health-cmd "pg_isready -U postgres" --health-interval 10s --health-retries=5 --label a\ b`)
	want := []string{"--health-cmd", "pg_isready -U postgres", "--health-interval", "10s", "--health-retries=5", "--label", "a b"}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := shellWords(`--health-cmd "unterminated`); err == nil {
		t.Fatal("unterminated quote accepted")
	}
}

func TestServicesValidation(t *testing.T) {
	for svc, want := range map[string]string{
		"Bad_Name: {image: redis}":                                   "service name",
		"db: {env: {A: b}}":                                          "image is required",
		`db: {image: redis, options: "'x"}`:                          "unterminated",
		`db: {image: "--privileged"}`:                                "invalid image",
		`db: {image: redis, options: "--privileged"}`:                "isn't allowed",
		`db: {image: redis, options: "--health-cmd 'x' -v /:/host"}`: "isn't allowed",
		`db: {image: redis, options: "--health-cmd"}`:                "needs a value",
	} {
		_, err := Parse([]byte("schema_version: 1\ntasks:\n  t:\n    argv: [\"true\"]\n    platform: linux/amd64\n    services:\n      " + svc + "\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", svc, err, want)
		}
	}
}

// Resolving an unpinned image runs docker pull: only for a trusted tasks.yaml.
func TestImagesNeedTrust(t *testing.T) {
	pinned := "redis@sha256:" + strings.Repeat("a", 64)
	for _, c := range []struct {
		tk   Task
		want bool
	}{
		{Task{Container: "golang:1.27"}, true},
		{Task{Container: "golang@sha256:" + strings.Repeat("b", 64)}, false},
		{Task{Services: map[string]Service{"db": {Image: "postgres:16"}}}, true},
		{Task{Services: map[string]Service{"db": {Image: pinned}}}, false},
		{Task{}, false},
	} {
		if got := c.tk.RunsCommands(); got != c.want {
			t.Errorf("%+v: RunsCommands %v, want %v", c.tk, got, c.want)
		}
	}
	if _, err := Parse([]byte("schema_version: 1\ntasks:\n  t: {argv: [\"true\"], platform: linux/amd64, container: \"-v/:/x\"}\n")); err == nil {
		t.Fatal("container image starting with - accepted")
	}
}

func TestAllowedOptions(t *testing.T) {
	ok, _ := shellWords(`--health-cmd "pg_isready -U x" --health-interval=5s --no-healthcheck --tmpfs /var/lib/x -e A=b`)
	if err := allowedOptions(ok); err != nil {
		t.Fatal(err)
	}
}
