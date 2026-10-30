package plan

import (
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/billiondollarsolo/cinema/internal/evidence"
	"github.com/billiondollarsolo/cinema/internal/task"
)

func dg(s string) string { return "sha256:" + strings.Repeat(s, 64)[:64] }

var exp = Expected{RepositoryID: "github.com/o/r", Contract: dg("c"), Source: dg("5"), Env: dg("e"), Now: now}

func stmt(f func(*evidence.Predicate)) evidence.VerifiedStatement {
	p := evidence.Predicate{
		SchemaVersion: 1, RunID: "run_1", RepositoryID: exp.RepositoryID, Platform: "linux/amd64",
		Task: evidence.TaskRef{ID: "t", ContractDigest: exp.Contract}, EnvDigest: exp.Env,
		Source: evidence.SourceRef{Digest: exp.Source, Capture: "clean-checkout"},
		Result: evidence.Result{Status: "passed", Origin: "fresh"},
		Timing: evidence.Timing{StartedAt: now.Add(-time.Hour)},
	}
	if f != nil {
		f(&p)
	}
	key := task.ActionKey(p.Task.ContractDigest, p.Source.Digest, p.EnvDigest, p.VCS)
	return evidence.VerifiedStatement{Signer: "k1", Statement: evidence.Statement{
		Subject:   []evidence.Subject{{Name: "cinema-action:t", Digest: map[string]string{"sha256": strings.TrimPrefix(key, "sha256:")}}},
		Predicate: p,
	}}
}

func TestGateTask(t *testing.T) {
	tsk := tk("none", true)
	for _, tc := range []struct {
		name     string
		t        *task.Task
		stmts    []evidence.VerifiedStatement
		rejected []string
		decision string
		reason   string
	}{
		{"matching receipt", tsk, []evidence.VerifiedStatement{stmt(nil)}, nil, GateReuse, ""},
		{"no receipts", tsk, nil, nil, GateRun, NoReceipt},
		{"reuse disabled", tk("none", false), []evidence.VerifiedStatement{stmt(nil)}, nil, GateRun, ReuseDisabled},
		{"other repo", tsk, []evidence.VerifiedStatement{stmt(func(p *evidence.Predicate) { p.RepositoryID = "github.com/x/y" })}, nil, GateRun, WrongRepository},
		{"inputs changed (merge commit differs)", tsk, []evidence.VerifiedStatement{stmt(func(p *evidence.Predicate) { p.Source.Digest = dg("6") })}, nil, GateRun, InputsChanged},
		{"toolchain differs", tsk, []evidence.VerifiedStatement{stmt(func(p *evidence.Predicate) { p.EnvDigest = dg("f") })}, nil, GateRun, EnvironmentMismatch},
		{"task edited", tsk, []evidence.VerifiedStatement{stmt(func(p *evidence.Predicate) { p.Task.ContractDigest = dg("d") })}, nil, GateRun, TaskDefinitionChanged},
		{"expired", tsk, []evidence.VerifiedStatement{stmt(func(p *evidence.Predicate) { p.Timing.StartedAt = now.Add(-73 * time.Hour) })}, nil, GateRun, ReceiptExpired},
		{"from the future", tsk, []evidence.VerifiedStatement{stmt(func(p *evidence.Predicate) { p.Timing.StartedAt = now.Add(time.Hour) })}, nil, GateRun, ReceiptExpired},
		{"another task's receipt", tsk, []evidence.VerifiedStatement{stmt(func(p *evidence.Predicate) { p.Task.ID = "other" })}, nil, GateRun, NoReceipt},
		{"tampered-only evidence", tsk, nil, []string{SignatureInvalid}, GateRun, SignatureInvalid},
		{"workspace receipt, clean required", tsk, []evidence.VerifiedStatement{stmt(func(p *evidence.Predicate) { p.Source.Capture = "workspace-observed" })}, nil, GateRun, WorkspaceEvidence},
		{"workspace receipt, task opts in", workspaceTask(), []evidence.VerifiedStatement{stmt(func(p *evidence.Predicate) { p.Source.Capture = "workspace-observed" })}, nil, GateReuse, ""},
		{"one good among bad", tsk, []evidence.VerifiedStatement{stmt(func(p *evidence.Predicate) { p.EnvDigest = dg("f") }), stmt(nil)}, []string{SignerNotAllowed}, GateReuse, ""},
		// ACC-115: a failure of the same work from any trusted key outweighs passes.
		{"failed elsewhere", tsk, []evidence.VerifiedStatement{stmt(nil), stmt(func(p *evidence.Predicate) {
			p.Result = evidence.Result{Status: "failed", ExitCode: 1, Origin: "fresh"}
			p.RunID = "run_2"
		})}, nil, GateRun, FailedElsewhere},
		{"failure on other inputs doesn't block", tsk, []evidence.VerifiedStatement{stmt(nil), stmt(func(p *evidence.Predicate) {
			p.Result = evidence.Result{Status: "failed", ExitCode: 1, Origin: "fresh"}
			p.Source.Digest = dg("6")
		})}, nil, GateReuse, ""},
		{"expired failure doesn't block", tsk, []evidence.VerifiedStatement{stmt(nil), stmt(func(p *evidence.Predicate) {
			p.Result = evidence.Result{Status: "failed", ExitCode: 1, Origin: "fresh"}
			p.Timing.StartedAt = now.Add(-73 * time.Hour)
		})}, nil, GateReuse, ""},
		{"too close to the timeout", timeoutTask(30 * time.Minute), []evidence.VerifiedStatement{stmt(func(p *evidence.Predicate) { p.Timing.DurationMS = (16 * time.Minute).Milliseconds() })}, nil, GateRun, TooCloseToTimeout},
		{"well within the timeout", timeoutTask(30 * time.Minute), []evidence.VerifiedStatement{stmt(func(p *evidence.Predicate) { p.Timing.DurationMS = (10 * time.Minute).Milliseconds() })}, nil, GateReuse, ""},
		{"a failure is never reused", tsk, []evidence.VerifiedStatement{stmt(func(p *evidence.Predicate) {
			p.Result = evidence.Result{Status: "failed", ExitCode: 1, Origin: "fresh"}
		})}, nil, GateRun, FailedElsewhere},
	} {
		d := GateTask(tc.t, exp, tc.stmts, tc.rejected)
		if d.Decision != tc.decision || d.Reason != tc.reason {
			t.Errorf("%s: %s/%s, want %s/%s", tc.name, d.Decision, d.Reason, tc.decision, tc.reason)
		}
	}
	// Signed action key disagreeing with the fields is never trusted.
	bad := stmt(nil)
	bad.Subject[0].Digest["sha256"] = strings.Repeat("0", 64)
	if d := GateTask(tsk, exp, []evidence.VerifiedStatement{bad}, nil); d.Decision != GateRun {
		t.Fatal("mismatched subject digest accepted")
	}
}

// Properties (plan §14): the gate never passes unless every task has a matching
// receipt or a passing inline run, and extra rejected/untrusted evidence never
// turns a failing gate into a passing one.
func TestGateProperties(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	mutations := []func(*evidence.Predicate){
		nil,
		func(p *evidence.Predicate) { p.Source.Digest = dg("6") },
		func(p *evidence.Predicate) { p.EnvDigest = dg("f") },
		func(p *evidence.Predicate) { p.RepositoryID = "github.com/x/y" },
		func(p *evidence.Predicate) { p.Timing.StartedAt = now.Add(-100 * time.Hour) },
		func(p *evidence.Predicate) { p.Task.ID = "other" },
	}
	for range 5000 {
		n := 1 + rng.IntN(4)
		var decisions []GateDecision
		executed := map[string]bool{}
		ok := true
		for i := range n {
			tsk := tk("none", rng.IntN(4) > 0)
			tsk.ID = "t"
			var stmts []evidence.VerifiedStatement
			for range rng.IntN(3) {
				stmts = append(stmts, stmt(mutations[rng.IntN(len(mutations))]))
			}
			d := GateTask(tsk, exp, stmts, nil)
			d.Task = string(rune('a' + i))
			hasMatch := false
			for _, s := range stmts {
				if s.Predicate.Task.ID == "t" && s.Predicate.Source.Digest == exp.Source && s.Predicate.EnvDigest == exp.Env &&
					s.Predicate.RepositoryID == exp.RepositoryID && now.Sub(s.Predicate.Timing.StartedAt) <= tsk.Reuse.MaxAge {
					hasMatch = true
				}
			}
			if d.Decision == GateReuse && (!hasMatch || !tsk.Reuse.Allowed) {
				t.Fatalf("REUSE without a matching receipt: %+v %+v", d, stmts)
			}
			if d.Decision == GateRun && rng.IntN(3) > 0 {
				executed[d.Task] = rng.IntN(4) > 0
			}
			passedRun, ran := executed[d.Task]
			ok = ok && (d.Decision == GateReuse || (ran && passedRun))
			decisions = append(decisions, d)
		}
		if GatePasses(decisions, executed) != ok {
			t.Fatalf("gate=%v, want %v: %+v %+v", !ok, ok, decisions, executed)
		}
		// Extra rejected evidence on a RUN never makes it REUSE.
		for _, d := range decisions {
			if d.Decision == GateRun {
				tsk := tk("none", true)
				tsk.ID = "t"
				if GateTask(tsk, exp, nil, []string{SignatureInvalid, SignerNotAllowed, Malformed}).Decision != GateRun {
					t.Fatal("rejected evidence produced REUSE")
				}
			}
		}
	}
	if GatePasses(nil, nil) {
		t.Fatal("empty task set passed")
	}
}

func workspaceTask() *task.Task {
	t := tk("none", true)
	t.Reuse.Capture = "workspace"
	return t
}

func timeoutTask(d time.Duration) *task.Task {
	t := tk("none", true)
	t.Timeout = d
	return t
}
