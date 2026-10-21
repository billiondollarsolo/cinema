package plan

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/billiondollarsolo/cinema/internal/task"
)

var now = time.Unix(1_800_000_000, 0)

func tk(vcs string, allowed bool) *task.Task {
	return &task.Task{ID: "t", VCSContext: vcs, Reuse: task.Reuse{Allowed: allowed, MaxAge: 72 * time.Hour}}
}

var cur = Current{Contract: "C", Source: "S", Env: "E", VCS: "abc", Now: now}

func good(id string) Run {
	return Run{ID: id, State: "passed", Started: now.Add(-time.Hour),
		Contract: "C", Env: "E", VCS: "abc", SourceBefore: "S", SourceAfter: "S"}
}

func with(r Run, f func(*Run)) Run { f(&r); return r }

func TestLocal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		t      *task.Task
		runs   []Run
		state  string
		reason string
	}{
		{"reusable", tk("none", true), []Run{good("a")}, Reusable, ""},
		{"no runs", tk("none", true), nil, Missing, NoReceipt},
		{"reuse disabled", tk("none", false), []Run{good("a")}, Always, ReuseDisabled},
		{"source changed after run", tk("none", true), []Run{with(good("a"), func(r *Run) { r.SourceBefore, r.SourceAfter = "S0", "S0" })}, Stale, InputsChanged},
		{"mutated during run", tk("none", true), []Run{with(good("a"), func(r *Run) { r.SourceAfter = "S2" })}, Stale, SourceChangedDuringRun},
		{"task edited", tk("none", true), []Run{with(good("a"), func(r *Run) { r.Contract = "C0" })}, Stale, TaskDefinitionChanged},
		{"toolchain changed", tk("none", true), []Run{with(good("a"), func(r *Run) { r.Env = "E0" })}, Stale, EnvironmentMismatch},
		{"too old", tk("none", true), []Run{with(good("a"), func(r *Run) { r.Started = now.Add(-73 * time.Hour) })}, Stale, ReceiptExpired},
		{"from the future", tk("none", true), []Run{with(good("a"), func(r *Run) { r.Started = now.Add(time.Hour) })}, Stale, ReceiptExpired},
		{"vcs exact, other commit", tk("exact", true), []Run{with(good("a"), func(r *Run) { r.VCS = "def" })}, Stale, VCSMismatch},
		{"vcs exact, dirty", tk("exact", true), []Run{with(good("a"), func(r *Run) { r.VCS = "dirty" })}, Stale, VCSMismatch},
		{"vcs none ignores commit", tk("none", true), []Run{with(good("a"), func(r *Run) { r.VCS = "def" })}, Reusable, ""},
		{"incomplete record", tk("none", true), []Run{with(good("a"), func(r *Run) { r.Env = "" })}, Stale, NoReceipt},
		{"fail on same inputs beats older pass", tk("none", true),
			[]Run{with(good("f"), func(r *Run) { r.State = "failed" }), good("a")}, Failed, RunFailed},
		{"fail on other tree doesn't block a qualifying pass", tk("none", true),
			[]Run{with(good("f"), func(r *Run) { r.State = "failed"; r.SourceBefore = "S9" }), good("a")}, Reusable, ""},
		{"latest failed elsewhere, nothing qualifies", tk("none", true),
			[]Run{with(good("f"), func(r *Run) { r.State = "failed"; r.SourceBefore = "S9" })}, Failed, RunFailed},
		{"older pass qualifies past a stale newer pass", tk("none", true),
			[]Run{with(good("new"), func(r *Run) { r.SourceBefore, r.SourceAfter = "S0", "S0" }), good("old")}, Reusable, ""},
	} {
		d := Local(tc.t, cur, tc.runs)
		if d.State != tc.state || d.Reason != tc.reason {
			t.Errorf("%s: got %s/%s, want %s/%s", tc.name, d.State, d.Reason, tc.state, tc.reason)
		}
	}
}

// Property (plan §14): Reusable only ever names a passed run that matches the
// current contract, environment and source exactly, didn't mutate the tree, is
// within max_age, and has no failure on the same inputs ahead of it.
func TestNeverFalseReusable(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	pick := func(xs ...string) string { return xs[rng.IntN(len(xs))] }
	for range 20000 {
		tsk := tk(pick("none", "exact"), rng.IntN(5) > 0)
		c := Current{Contract: pick("C", "C2"), Source: pick("S", "S2"), Env: pick("E", "E2"), VCS: pick("abc", "def", "dirty"), Now: now}
		runs := make([]Run, rng.IntN(5))
		for i := range runs {
			runs[i] = Run{
				ID: string(rune('a' + i)), State: pick("passed", "passed", "failed", "incomplete"),
				Started:  now.Add(time.Duration(rng.IntN(200)-100) * time.Hour),
				Contract: pick("C", "C2", ""), Env: pick("E", "E2", ""), VCS: pick("abc", "def", "dirty", ""),
				SourceBefore: pick("S", "S2", ""), SourceAfter: pick("S", "S2", ""),
			}
		}
		d := Local(tsk, c, runs)
		if d.State != Reusable {
			continue
		}
		var r *Run
		idx := 0
		for i := range runs {
			if runs[i].ID == d.RunID {
				r, idx = &runs[i], i
			}
		}
		age := now.Sub(r.Started)
		ok := tsk.Reuse.Allowed && r.State == "passed" && r.Contract == c.Contract && r.Env == c.Env &&
			r.SourceBefore == c.Source && r.SourceAfter == c.Source && age <= tsk.Reuse.MaxAge && age >= -time.Minute &&
			(tsk.VCSContext != "exact" || (r.VCS == c.VCS && r.VCS != "dirty" && r.VCS != ""))
		for _, ahead := range runs[:idx] {
			if ahead.State != "passed" && ahead.Contract == c.Contract && ahead.Env == c.Env && ahead.SourceBefore == c.Source {
				ok = false
			}
		}
		if !ok {
			t.Fatalf("false reusable:\ntask=%+v\ncur=%+v\nrun=%+v\nall=%+v", tsk, c, *r, runs)
		}
	}
}
