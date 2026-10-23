package record

import (
	"context"
	"strings"
	"testing"
)

func TestCPUsLimitWhatTheChildSees(t *testing.T) {
	res, err := Run(context.Background(), Options{Argv: []string{"sh", "-c", "nproc; sh -c nproc"}, CPUs: []int{0}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(res.Stdout.Bytes())); len(got) != 2 || got[0] != "1" || got[1] != "1" {
		t.Fatalf("nproc in child and grandchild: %q, want 1 1", got)
	}
}
