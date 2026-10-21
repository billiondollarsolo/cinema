package task

import (
	"bytes"
	"regexp"
)

// goTestCached matches a package result `go test` replayed from its cache:
// "ok  \tpkg/path\t(cached)" (optionally followed by coverage info).
var goTestCached = regexp.MustCompile(`^ok\s+\S+\s+\(cached\)`)

// CacheDetector watches a command's stdout for results a tool replayed from its
// own cache (plan §4.5). It sees every byte, so a marker can't hide in output
// the bounded capture dropped, and it works whatever wraps the tool (env, bash -c,
// make). ponytail: knows go test's marker only; add others as tools need them.
type CacheDetector struct {
	line   []byte
	Cached bool
}

const maxLine = 4096

func (d *CacheDetector) Write(p []byte) (int, error) {
	for _, c := range p {
		if c == '\n' {
			d.check()
			d.line = d.line[:0]
			continue
		}
		if len(d.line) < maxLine {
			d.line = append(d.line, c)
		}
	}
	return len(p), nil
}

// Done checks a final line without a trailing newline.
func (d *CacheDetector) Done() { d.check() }

func (d *CacheDetector) check() {
	if !d.Cached && goTestCached.Match(bytes.TrimRight(d.line, "\r")) {
		d.Cached = true
	}
}
