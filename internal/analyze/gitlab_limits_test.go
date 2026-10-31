package analyze

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// glFanOut is a config of n levels, each listing the one below ten times:
// level(i) writes the line for level i, given the name of level i-1.
func glFanOut(head string, n int, level func(i int, below string) string, tail string) string {
	var b strings.Builder
	b.WriteString(head)
	for i := 1; i <= n; i++ {
		b.WriteString(level(i, fmt.Sprint(i-1)))
	}
	b.WriteString(tail)
	return b.String()
}

func tenTimes(s string) string { return strings.TrimSuffix(strings.Repeat(s+", ", 10), ", ") }

// Billion-laughs configs: !reference, extends and variables fan out within
// their depth limits. Analysis ends quickly; what goes past the shared
// budget sends every job to review (with no draft to digest), and the gate
// sees it (Unreadable), never a hang.
func TestGitLabFanOut(t *testing.T) {
	t.Setenv("GITLAB_CI", "true")
	t.Setenv("CI_DEFAULT_BRANCH", "main")
	lint := "lint:\n  image: alpine:3.20\n  script: [make lint]\n"
	for name, c := range map[string]struct {
		src     string
		tooBig  bool
		varBomb bool // blows up only when jobs are analyzed, not when the config loads
	}{
		"!reference": {src: glFanOut(".r0: {s: [aaaaaaaaaaaaaaaa]}\n", 9, func(i int, below string) string {
			return fmt.Sprintf(".r%d: {s: [%s]}\n", i, tenTimes("!reference [.r"+below+", s]"))
		}, "unit:\n  image: alpine:3.20\n  script: !reference [.r9, s]\n"+lint), tooBig: true},
		"variables": {src: glFanOut("variables:\n  V0: aaaaaaaaaaaaaaaa\n", 9, func(i int, below string) string {
			return fmt.Sprintf("  V%d: %q\n", i, strings.Repeat("$V"+below, 10))
		}, "unit:\n  image: alpine:3.20\n  script: [echo $V9]\n"+lint), tooBig: true, varBomb: true},
		// extends results are remembered: a wide, deep extends tree resolves.
		"extends": {src: glFanOut(".e0: {image: alpine:3.20, script: [make]}\n", 9, func(i int, below string) string {
			return fmt.Sprintf(".e%d: {extends: [%s]}\n", i, tenTimes(".e"+below))
		}, "unit:\n  extends: [.e9, .e9, .e9]\n"+lint)},
	} {
		done := make(chan []Job, 1)
		start := time.Now()
		go func() {
			jobs, err := GitLab(t.TempDir(), []byte(c.src))
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			done <- jobs
		}()
		var jobs []Job
		select {
		case jobs = <-done:
		case <-time.After(60 * time.Second):
			t.Fatalf("%s: still resolving after a minute", name)
		}
		if len(jobs) != 2 {
			t.Fatalf("%s: %d jobs", name, len(jobs))
		}
		for _, j := range jobs {
			big := slices.Contains(j.Reasons, glTooLarge)
			if big != c.tooBig || c.tooBig && (j.Verdict != Review || j.Proposal != nil) || !c.tooBig && j.Proposal == nil {
				t.Errorf("%s: %s: %s %v (draft: %v)", name, j.Name, j.Verdict, j.Reasons, j.Proposal != nil)
			}
		}
		gl, err := LoadGitLab(t.TempDir(), []byte(c.src))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(gl.Unreadable(), glTooLarge) != (c.tooBig && !c.varBomb) {
			t.Errorf("%s: Unreadable() = %q", name, gl.Unreadable())
		}
		t.Logf("%s: %v", name, time.Since(start))
	}
}

// A job's digest covers what cinema can't read only by naming it: adding
// or changing an unreadable include changes it (and the gate skips nothing
// while there is one; see cmd/cinema).
func TestGitLabDigestUnreadableInclude(t *testing.T) {
	t.Setenv("GITLAB_CI", "true")
	t.Setenv("CI_DEFAULT_BRANCH", "main")
	job := "unit:\n  image: alpine:3.20\n  script: [make test]\n"
	digest := func(src string) string {
		jobs, err := GitLab(t.TempDir(), []byte(src))
		if err != nil || len(jobs) != 1 || jobs[0].Proposal == nil {
			t.Fatalf("%v %+v", err, jobs)
		}
		return jobs[0].LegDigest()
	}
	plain := digest(job)
	a := digest("include: [{remote: 'https://example.com/a.yml'}]\n" + job)
	b := digest("include: [{remote: 'https://example.com/b.yml'}]\n" + job)
	p := digest("include: [{project: group/ci, file: ci.yml}]\n" + job)
	if plain == a || a == b || a == p {
		t.Errorf("an unreadable include doesn't change the digest: %s %s %s %s", plain, a, b, p)
	}
}
