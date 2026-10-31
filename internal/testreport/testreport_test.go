package testreport

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func failedNames(t *testing.T, out string) ([]string, string) {
	t.Helper()
	return FailedNames(Read([]byte(out), false, nil).Summary)
}

func TestGoPlain(t *testing.T) {
	out := "=== RUN   TestA\n--- PASS: TestA (0.01s)\n=== RUN   TestB\n    --- FAIL: TestB/sub (0.00s)\n--- FAIL: TestB (0.02s)\n--- SKIP: TestC (0.00s)\nFAIL\nFAIL\texample.com/a\t0.1s\n" +
		"ok  \texample.com/b\t(cached)\n?   \texample.com/c\t[no test files]\n"
	r := Read([]byte(out), false, nil)
	s := r.Summary
	if s == nil || s.Passed != 1 || s.Failed != 2 || s.Skipped != 1 || s.Partial != "" {
		t.Fatalf("%+v", s)
	}
	names, why := FailedNames(s)
	if why != "" || !slices.Equal(names, []string{"go: example.com/a.TestB", "go: example.com/a.TestB/sub"}) {
		t.Fatalf("%q %q", names, why)
	}
	if r.Tests[0].DurationMS != 10 {
		t.Fatalf("duration: %+v", r.Tests[0])
	}
}

// A package that fails with no failing test (a build error, a timeout) hides
// which tests would have failed: the list can't be compared.
func TestGoPartial(t *testing.T) {
	out := "--- FAIL: TestB (0.02s)\nFAIL\texample.com/a\t0.1s\n# example.com/d\nd.go:3: undefined: x\nFAIL\texample.com/d [build failed]\n"
	if _, why := failedNames(t, out); !strings.Contains(why, "example.com/d") {
		t.Fatalf("got %q", why)
	}
}

func TestGoJSON(t *testing.T) {
	out := `{"Action":"run","Package":"p","Test":"TestA"}
{"Action":"output","Package":"p","Test":"TestA","Output":"--- FAIL: TestA (0.50s)\n"}
{"Action":"fail","Package":"p","Test":"TestA","Elapsed":0.5}
{"Action":"pass","Package":"p","Test":"TestB","Elapsed":0}
{"Action":"fail","Package":"p","Elapsed":0.6}
`
	r := Read([]byte(out), false, nil)
	if r.Summary.Failed != 1 || r.Summary.Passed != 1 || r.Summary.Failures[0].Name != "p.TestA" || r.Summary.Failures[0].DurationMS != 500 {
		t.Fatalf("%+v", r.Summary)
	}
}

func TestPytest(t *testing.T) {
	out := "============ test session starts ============\n" +
		"tests/test_a.py::test_ok PASSED                [ 25%]\n" +
		"tests/test_a.py::TestC::test_bad[1-2] FAILED   [ 50%]\n" +
		"tests/test_a.py::test_skip SKIPPED (why)       [ 75%]\n" +
		"tests/test_a.py::test_x XFAIL                  [100%]\n" +
		"========== short test summary info ==========\n" +
		"FAILED tests/test_a.py::TestC::test_bad[1-2] - assert 1 == 2\n" +
		"ERROR tests/test_b.py::test_fixture - RuntimeError\n" +
		"=== 1 failed, 1 passed, 2 skipped, 1 error in 0.12s ===\n"
	names, why := failedNames(t, out)
	if why != "" || !slices.Equal(names, []string{"pytest: tests/test_a.py::TestC::test_bad[1-2]", "pytest: tests/test_b.py::test_fixture"}) {
		t.Fatalf("%q %q", names, why)
	}
	// A collection error has no test id: the count says something is missing.
	out = "PASSED tests/test_a.py::test_ok\nERROR tests/test_c.py - ImportError: x\n=== 1 passed, 1 error in 0.1s ===\n"
	if _, why := failedNames(t, out); !strings.Contains(why, "pytest reported 1") {
		t.Fatalf("got %q", why)
	}
	// xdist, through a GitHub log's timestamps and colors.
	out = "2026-10-30T10:00:00.1234567Z [gw1] [ 50%] \x1b[31mFAILED\x1b[0m tests/test_a.py::test_bad \n"
	if names, _ := failedNames(t, out); !slices.Equal(names, []string{"pytest: tests/test_a.py::test_bad"}) {
		t.Fatalf("%q", names)
	}
}

func TestJest(t *testing.T) {
	out := "FAIL src/a.test.js\n  math\n    ✓ adds (3 ms)\n    ✕ subtracts (2 ms)\n    ○ skipped later\n\n" +
		"  ● math › subtracts\n\n    expect(received).toBe(expected)\n\n  ● Console\n\n" +
		"Test Suites: 1 failed, 1 total\nTests:       1 failed, 1 skipped, 1 passed, 3 total\n"
	r := Read([]byte(out), false, nil)
	names, why := FailedNames(r.Summary)
	if why != "" || !slices.Equal(names, []string{"jest: math › subtracts"}) || r.Summary.Passed != 1 || r.Summary.Skipped != 1 {
		t.Fatalf("%q %q %+v", names, why, r.Summary)
	}
	// ✓ lines without a test runner's summary are someone else's.
	if r := Read([]byte("✓ build\n  ✓ lint (2 ms)\n"), false, nil); r.Summary != nil {
		t.Fatalf("%+v", r.Summary)
	}
}

func TestVitest(t *testing.T) {
	out := " ✓ src/b.test.ts (2 tests) 4ms\n ❯ src/a.test.ts (2 tests | 1 failed) 6ms\n   ✓ math > adds 1ms\n   × math > subtracts 3ms\n\n" +
		"⎯⎯⎯ Failed Tests 1 ⎯⎯⎯\n\n FAIL  src/a.test.ts > math > subtracts\nAssertionError: expected 1 to be 2\n\n" +
		" Test Files  1 failed | 1 passed (2)\n      Tests  1 failed | 3 passed (4)\n"
	names, why := failedNames(t, out)
	if why != "" || !slices.Equal(names, []string{"vitest: src/a.test.ts > math > subtracts"}) {
		t.Fatalf("%q %q", names, why)
	}
}

func TestCargo(t *testing.T) {
	out := "     Running unittests src/lib.rs (target/debug/deps/x-abc)\n\nrunning 3 tests\n" +
		"test tests::ok ... ok\ntest tests::bad ... FAILED\ntest tests::slow ... ignored, needs network\n\n" +
		"failures:\n\n---- tests::bad stdout ----\npanicked\n\nfailures:\n    tests::bad\n\n" +
		"test result: FAILED. 1 passed; 1 failed; 1 ignored; 0 measured; 0 filtered out; finished in 0.00s\n"
	r := Read([]byte(out), false, nil)
	names, why := FailedNames(r.Summary)
	if why != "" || !slices.Equal(names, []string{"cargo: tests::bad"}) || r.Summary.Skipped != 1 {
		t.Fatalf("%q %q %+v", names, why, r.Summary)
	}
}

func TestJUnit(t *testing.T) {
	x := `<?xml version="1.0"?><testsuites><testsuite name="s">
<testcase classname="com.x.ATest" name="ok" time="0.25"/>
<testcase classname="com.x.ATest" name="bad"><failure message="no"/></testcase>
<testcase classname="com.x.ATest" name="err"><error/></testcase>
<testcase classname="com.x.ATest" name="skip"><skipped/></testcase>
</testsuite></testsuites>`
	r := Read(nil, false, [][]byte{[]byte(x), []byte("<html/>")})
	s := r.Summary
	if s == nil || s.Passed != 1 || s.Failed != 2 || s.Skipped != 1 || r.Tests[0].Name != "com.x.ATest.ok" || r.Tests[0].DurationMS != 250 {
		t.Fatalf("%+v %+v", s, r.Tests)
	}
	// Only a local run has JUnit files: they never take part in a comparison with CI's log.
	if _, why := FailedNames(s); why == "" {
		t.Fatal("JUnit failures compared")
	}
	// The same tests in the output and a report aren't counted twice: the output wins.
	if s := Read([]byte("--- PASS: TestA (0.00s)\nok  \tp\t0.1s\n"), false, [][]byte{[]byte(x)}).Summary; s.Passed != 1 || s.Failed != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestCutOutputIsPartial(t *testing.T) {
	if s := Read([]byte("--- FAIL: TestB (0.02s)\nFAIL\tp\t0.1s\n"), true, nil).Summary; !strings.Contains(s.Partial, "capture limit") {
		t.Fatalf("%+v", s)
	}
}

func TestJUnitFiles(t *testing.T) {
	root := t.TempDir()
	write := func(p string, at time.Time) {
		p = filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("<testsuite/>"), 0o644)
		os.Chtimes(p, at, at)
	}
	start := time.Now()
	old := start.Add(-time.Hour)
	write("target/surefire-reports/TEST-a.xml", start)
	write("build/test-results/test/b.xml", start)
	write("junit.xml", start)
	write("testdata/junit-fixture.xml", old) // tracked fixture: older than the run
	write("node_modules/x/junit.xml", start)
	write("pom.xml", start)
	if got := len(JUnitFiles(root, start)); got != 3 {
		t.Fatalf("got %d files", got)
	}
}
