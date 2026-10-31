package analyze

import (
	"strings"
	"testing"
)

// Lab (gson): mvn verify with matrix.extra-mvn-args or else the empty string
// sent every leg to review ("an expression cinema can't evaluate"); it only
// reads the matrix.
func TestMatrixValueExpressionsLab(t *testing.T) {
	src := `on: pull_request
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        java: [17]
        include:
          - java: 11
            extra: -Denforcer.fail=false --projects '!proto'
    steps:
      - run: mvn verify ${{ matrix.extra || '' }} -Dj=${{ matrix.java == '11' && 'old' || 'new' }}
      - run: echo ${{ matrix.java || github.ref }}
`
	jobs, err := Workflow("ci.yml", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"17": "mvn verify  -Dj=new",
		"11": "mvn verify -Denforcer.fail=false --projects '!proto' -Dj=old",
	}
	if len(jobs) != 2 {
		t.Fatalf("%d legs, want 2", len(jobs))
	}
	for _, j := range jobs {
		w := want[j.Combo["java"]]
		if j.Proposal == nil || !strings.Contains(j.Proposal.Body, w) {
			t.Errorf("leg %v: want %q in the draft, got %+v", j.Combo, w, j.Proposal)
			continue
		}
		// || short-circuits as in Actions: github.ref is never read when matrix.java is set.
		if !strings.Contains(j.Proposal.Body, "echo "+j.Combo["java"]) {
			t.Errorf("leg %v: matrix.java || github.ref not evaluated:\n%s", j.Combo, j.Proposal.Body)
		}
	}
	for _, c := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"matrix.x || 'd'", "d", true},
		{"matrix.x", "", true},
		{"matrix.f || 'd'", "", false}, // YAML false or the string "false"?
		{"matrix.x || runner.os", "", false},
		{"matrix.x || 1", "1", true},
		{"matrix.x || github.ref", "", false}, // reads what the matrix doesn't know
	} {
		v, ok := evalValue(c.in, map[string]string{"f": "false"})
		if v != c.want || ok != c.ok {
			t.Errorf("evalValue(%q) = %q, %v; want %q, %v", c.in, v, ok, c.want, c.ok)
		}
	}
}
