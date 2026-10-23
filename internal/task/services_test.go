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
		"Bad_Name: {image: redis}":          "service name",
		"db: {env: {A: b}}":                 "image is required",
		`db: {image: redis, options: "'x"}`: "unterminated",
	} {
		_, err := Parse([]byte("schema_version: 1\ntasks:\n  t:\n    argv: [\"true\"]\n    platform: linux/amd64\n    services:\n      " + svc + "\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", svc, err, want)
		}
	}
}
