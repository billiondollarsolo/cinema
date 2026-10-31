// Package testreport reads per-test results out of a run's output (go test,
// pytest, jest, vitest, cargo test) and the JUnit XML files it wrote.
//
// A parse is advisory metadata, never evidence: it explains a failure and lets
// `cinema graduate` compare which tests failed, and nothing is ever approved
// because of it. When the output can't say every failure (cut, a package that
// failed without a failing test, counts that don't add up), Partial says why.
package testreport

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	Passed  = "passed"
	Failed  = "failed"
	Skipped = "skipped"
)

// Test is one test's result. Name is as its tool prints it, qualified where the
// output says more (go: package, vitest: file, JUnit: classname).
type Test struct {
	Name       string `json:"name"`
	Format     string `json:"format"` // go | pytest | jest | vitest | cargo | junit
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// Summary is what a run record keeps for as long as the run: counts and every failure.
type Summary struct {
	Passed   int    `json:"passed"`
	Failed   int    `json:"failed"`
	Skipped  int    `json:"skipped"`
	Failures []Test `json:"failures,omitempty"`
	Partial  string `json:"partial,omitempty"` // why Failures may be missing some; "" = all the output shows
}

// Report is a run's parsed results: every test, and the summary.
type Report struct {
	Tests   []Test
	Summary *Summary // nil: no tests found
}

const maxFailures = 1000 // a summary stays small; past this the list is cut (and Partial)

// Read parses output (a run's stdout then stderr, or a CI job's log) and the
// contents of JUnit XML files. cut: the output's middle was dropped.
//
// JUnit files are read only when the output names no test: they usually
// report the same tests (pytest --junitxml, go-junit-report), and counting
// both would double them. They're what surefire and gradle leave.
func Read(output []byte, cut bool, junit [][]byte) Report {
	tests, partial := parseText(output)
	if cut && len(tests) > 0 {
		partial = strings.TrimPrefix(partial+"; the output was over the capture limit and its middle was dropped", "; ")
	}
	for _, x := range junit {
		if len(tests) > 0 && tests[0].Format != "junit" {
			break
		}
		if ts, err := JUnit(x); err == nil {
			tests = append(tests, ts...)
		}
	}
	tests = dedupe(tests)
	if len(tests) == 0 {
		return Report{}
	}
	s := &Summary{Partial: partial}
	for _, t := range tests {
		switch t.Status {
		case Passed:
			s.Passed++
		case Skipped:
			s.Skipped++
		case Failed:
			s.Failed++
			if len(s.Failures) == maxFailures {
				s.Partial = fmt.Sprintf("over %d failures: the list was cut", maxFailures)
				continue
			}
			s.Failures = append(s.Failures, t)
		}
	}
	return Report{Tests: tests, Summary: s}
}

// FailedNames is the sorted names of s's failures parsed from output (not
// JUnit files, which only a local run has), and why they can't be compared ("" = they can).
func FailedNames(s *Summary) ([]string, string) {
	if s == nil {
		return nil, "no test results in the output"
	}
	if s.Partial != "" {
		return nil, s.Partial
	}
	var names []string
	for _, f := range s.Failures {
		if f.Format != "junit" {
			names = append(names, f.Format+": "+f.Name)
		}
	}
	if len(names) == 0 {
		return nil, "no failed tests in the output (it failed before or outside its tests?)"
	}
	slices.Sort(names)
	return names, ""
}

// dedupe keeps one result per test: a failure wins (a retried or repeated test
// that failed once failed), then a pass. Order of first appearance is kept.
func dedupe(tests []Test) []Test {
	rank := map[string]int{Skipped: 0, Passed: 1, Failed: 2}
	at := map[string]int{}
	var out []Test
	for _, t := range tests {
		k := t.Format + "\x00" + t.Name
		i, seen := at[k]
		if !seen {
			at[k] = len(out)
			out = append(out, t)
			continue
		}
		if rank[t.Status] > rank[out[i].Status] {
			out[i].Status = t.Status
		}
		out[i].DurationMS = max(out[i].DurationMS, t.DurationMS)
	}
	return out
}

var (
	ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	// GitHub's job logs prefix every line with a timestamp.
	ghStamp = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z ?`)

	goResult = regexp.MustCompile(`^\s*--- (PASS|FAIL|SKIP): (\S+) \((\d+(?:\.\d+)?)s\)`)
	goPkg    = regexp.MustCompile(`^(ok|FAIL)\s*\t(\S+)(?:\s|$)`) // a tab: jest's "FAIL src/a.test.js" isn't one

	pyVerbose = regexp.MustCompile(`^(\S+\.py::[^\s\[]+(?:\[[^\]]*\])?) (PASSED|FAILED|ERROR|SKIPPED|XFAIL|XPASS)\b`)
	pyXdist   = regexp.MustCompile(`^\[gw\d+\] \[\s*\d+%\] (PASSED|FAILED|ERROR|SKIPPED|XFAIL|XPASS) (\S+\.py::[^\s\[]+(?:\[[^\]]*\])?)`)
	pyShort   = regexp.MustCompile(`^(PASSED|FAILED|ERROR) (\S+\.py::[^\s\[]+(?:\[[^\]]*\])?)(?: - .*)?$`)
	pyFinal   = regexp.MustCompile(`^=+ (.*\d+ (?:passed|failed|errors?|skipped).*) in [\d.]+s\b.*=+$`)
	pyCount   = regexp.MustCompile(`(\d+) (failed|errors?)\b`)

	jsPass     = regexp.MustCompile(`^\s+[✓✔√] (.+?)(?: \((\d+(?:\.\d+)?) ?(ms|s)\)| (\d+)ms)?$`)
	jsSkip     = regexp.MustCompile(`^\s+[○↓] (?:skipped )?(.+?)$`)
	jsFileLine = regexp.MustCompile(`\(\d+ tests?[ |)]`) // vitest's per-file line, not a test
	jestFail   = regexp.MustCompile(`^\s+● (.+)$`)
	jestTotal  = regexp.MustCompile(`^Tests:\s+(.*)\d+ total`)
	vitestFail = regexp.MustCompile(`^\s*FAIL\s+(\S+) > (.+?)\s*$`)
	vitestTot  = regexp.MustCompile(`^\s*Tests\s+(.*)\(\d+\)\s*$`)
	jsCount    = regexp.MustCompile(`(\d+) failed`)

	cargoTest   = regexp.MustCompile(`^test (.+) \.\.\. (ok|FAILED|ignored)\b`)
	cargoResult = regexp.MustCompile(`^test result: (?:ok|FAILED)\. \d+ passed; (\d+) failed;`)
)

// parseText runs every text format over the output. Each recognizes only its
// own lines, so a log with several tools (a CI job running go and jest) is fine.
func parseText(out []byte) ([]Test, string) {
	var lines []string
	for l := range strings.SplitSeq(string(out), "\n") {
		l = strings.TrimRight(l, "\r")
		if i := strings.LastIndexByte(l, '\r'); i >= 0 {
			l = l[i+1:] // a progress line redrawn in place: its last state
		}
		lines = append(lines, ghStamp.ReplaceAllString(ansi.ReplaceAllString(l, ""), ""))
	}
	var tests []Test
	var partial []string
	for _, p := range []func([]string) ([]Test, string){goText, pytest, jsTests, cargo} {
		ts, why := p(lines)
		tests = append(tests, ts...)
		if why != "" {
			partial = append(partial, why)
		}
	}
	return tests, strings.Join(partial, "; ")
}

func status(s string) string {
	switch s {
	case "PASS", "PASSED", "XPASS", "ok":
		return Passed
	case "FAIL", "FAILED", "ERROR":
		return Failed
	}
	return Skipped
}

func ms(v, unit string) int64 {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0
	}
	if unit == "s" {
		f *= 1000
	}
	return int64(f + 0.5)
}

// goText reads `go test` output, plain (`--- FAIL: TestX (0.01s)`, qualified
// by the package line that follows) and -json. A package that failed without a
// failing test (build error, panic in init, timeout) makes the list partial.
func goText(lines []string) ([]Test, string) {
	var tests, pending []Test
	var bad []string
	failedPkgs := map[string]bool{} // -json: package → saw a failing test
	for _, l := range lines {
		if strings.HasPrefix(l, "{") && strings.Contains(l, `"Action"`) {
			var ev struct {
				Action, Package, Test string
				Elapsed               float64
			}
			if json.Unmarshal([]byte(l), &ev) != nil {
				continue
			}
			switch {
			case ev.Test != "" && (ev.Action == "pass" || ev.Action == "fail" || ev.Action == "skip"):
				tests = append(tests, Test{Name: ev.Package + "." + ev.Test, Format: "go", Status: status(strings.ToUpper(ev.Action)),
					DurationMS: int64(ev.Elapsed*1000 + 0.5)})
				if ev.Action == "fail" {
					failedPkgs[ev.Package] = true
				}
			case ev.Test == "" && ev.Action == "fail" && !failedPkgs[ev.Package]:
				bad = append(bad, ev.Package)
			}
			continue
		}
		if m := goResult.FindStringSubmatch(l); m != nil {
			pending = append(pending, Test{Name: m[2], Format: "go", Status: status(m[1]), DurationMS: ms(m[3], "s")})
			continue
		}
		if m := goPkg.FindStringSubmatch(l); m != nil {
			failing := false
			for i := range pending {
				pending[i].Name = m[2] + "." + pending[i].Name
				failing = failing || pending[i].Status == Failed
			}
			if m[1] == "FAIL" && !failing {
				bad = append(bad, m[2])
			}
			tests, pending = append(tests, pending...), nil
		}
	}
	tests = append(tests, pending...) // no package line (a test binary run directly): unqualified
	if len(bad) > 0 {
		return tests, "go package(s) failed without a failing test (build error, panic, timeout?): " + strings.Join(bad, ", ")
	}
	return tests, ""
}

// pytest reads -v lines (also pytest-xdist's), and the short summary
// (`FAILED path::test - …`, shown by default). The final line's counts check
// that every failure and error was seen: a collection error has no test id.
func pytest(lines []string) ([]Test, string) {
	var tests []Test
	want := -1
	for _, l := range lines {
		if m := pyVerbose.FindStringSubmatch(l); m != nil {
			tests = append(tests, Test{Name: m[1], Format: "pytest", Status: status(m[2])})
		} else if m := pyXdist.FindStringSubmatch(l); m != nil {
			tests = append(tests, Test{Name: m[2], Format: "pytest", Status: status(m[1])})
		} else if m := pyShort.FindStringSubmatch(l); m != nil {
			tests = append(tests, Test{Name: m[2], Format: "pytest", Status: status(m[1])})
		} else if m := pyFinal.FindStringSubmatch(l); m != nil {
			want = 0
			for _, c := range pyCount.FindAllStringSubmatch(m[1], -1) {
				n, _ := strconv.Atoi(c[1])
				want += n
			}
		}
	}
	if got := failures(dedupe(tests)); got < want {
		return tests, fmt.Sprintf("pytest reported %d failed or errored, %d named", want, got)
	}
	return tests, ""
}

// jsTests reads jest and vitest, only in output with their summary line
// ("Tests: …"), since ✓ lines are common elsewhere. Failures come from the
// full names in the failure details (jest `● A › b`, vitest `FAIL file > A > b`);
// passes need the verbose reporter (jest prints them for a single file).
func jsTests(lines []string) ([]Test, string) {
	format, want := "", -1
	for _, l := range lines {
		if m := jestTotal.FindStringSubmatch(l); m != nil {
			format, want = "jest", max(want, 0)+count(jsCount, m[1])
		} else if m := vitestTot.FindStringSubmatch(l); m != nil {
			format, want = "vitest", max(want, 0)+count(jsCount, m[1])
		}
	}
	if format == "" {
		return nil, ""
	}
	var tests []Test
	for _, l := range lines {
		if m := jsPass.FindStringSubmatch(l); m != nil && !jsFileLine.MatchString(l) {
			d := ms(m[2], m[3])
			if m[4] != "" {
				d = ms(m[4], "ms")
			}
			tests = append(tests, Test{Name: m[1], Format: format, Status: Passed, DurationMS: d})
		} else if m := jsSkip.FindStringSubmatch(l); m != nil && !jsFileLine.MatchString(l) {
			tests = append(tests, Test{Name: m[1], Format: format, Status: Skipped})
		} else if m := jestFail.FindStringSubmatch(l); m != nil && format == "jest" {
			if m[1] == "Console" || strings.HasPrefix(m[1], "Test suite failed to run") {
				continue
			}
			tests = append(tests, Test{Name: m[1], Format: format, Status: Failed})
		} else if m := vitestFail.FindStringSubmatch(l); m != nil && format == "vitest" {
			tests = append(tests, Test{Name: m[1] + " > " + m[2], Format: format, Status: Failed})
		}
	}
	tests = dedupe(tests)
	if got := failures(tests); got < want {
		return tests, fmt.Sprintf("%s reported %d failed, %d named", format, want, got)
	}
	return tests, ""
}

// cargo reads `test path ... ok|FAILED|ignored`; the `test result:` lines'
// counts check that every failure was seen.
func cargo(lines []string) ([]Test, string) {
	var tests []Test
	want := 0
	for _, l := range lines {
		if m := cargoTest.FindStringSubmatch(l); m != nil {
			tests = append(tests, Test{Name: m[1], Format: "cargo", Status: status(m[2])})
		} else if m := cargoResult.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[1])
			want += n
		}
	}
	if got := failures(dedupe(tests)); got < want {
		return tests, fmt.Sprintf("cargo reported %d failed, %d named", want, got)
	}
	return tests, ""
}

func failures(ts []Test) int {
	n := 0
	for _, t := range ts {
		if t.Status == Failed {
			n++
		}
	}
	return n
}

func count(re *regexp.Regexp, s string) int {
	n := 0
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		k, _ := strconv.Atoi(m[1])
		n += k
	}
	return n
}

type xmlSuite struct {
	XMLName xml.Name
	Suites  []xmlSuite `xml:"testsuite"`
	Cases   []struct {
		Class   string    `xml:"classname,attr"`
		Name    string    `xml:"name,attr"`
		Time    string    `xml:"time,attr"`
		Failure *struct{} `xml:"failure"`
		Error   *struct{} `xml:"error"`
		Skipped *struct{} `xml:"skipped"`
	} `xml:"testcase"`
}

// JUnit reads a JUnit XML report (<testsuites> or <testsuite> at the root;
// surefire, gradle, pytest --junitxml, jest-junit, go-junit-report).
func JUnit(data []byte) ([]Test, error) {
	var root xmlSuite
	if err := xml.NewDecoder(bytes.NewReader(data)).Decode(&root); err != nil {
		return nil, err
	}
	if n := root.XMLName.Local; n != "testsuites" && n != "testsuite" {
		return nil, fmt.Errorf("not a JUnit report: <%s>", n)
	}
	var tests []Test
	var walk func(s xmlSuite)
	walk = func(s xmlSuite) {
		for _, c := range s.Cases {
			name := c.Name
			if c.Class != "" && !strings.HasPrefix(name, c.Class) {
				name = c.Class + "." + name
			}
			st := Passed
			switch {
			case c.Failure != nil || c.Error != nil:
				st = Failed
			case c.Skipped != nil:
				st = Skipped
			}
			tests = append(tests, Test{Name: name, Format: "junit", Status: st, DurationMS: ms(c.Time, "s")})
		}
		for _, sub := range s.Suites {
			walk(sub)
		}
	}
	walk(root)
	return tests, nil
}
