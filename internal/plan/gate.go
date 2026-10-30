package plan

import (
	"time"

	"github.com/billiondollarsolo/cinema/internal/evidence"
	"github.com/billiondollarsolo/cinema/internal/task"
)

// Gate decisions (plan §11).
const (
	GateReuse = "REUSE"
	GateRun   = "RUN"
)

// Expected is what a receipt must match for a task on the candidate CI is testing.
type Expected struct {
	RepositoryID string
	Contract     string // task contract digest, from the BASE branch's config
	Source       string // fingerprint of the checked-out candidate
	Env          string // this runner's environment fingerprint for the task
	VCS          string // candidate commit for vcs_context: exact, else ""
	Now          time.Time
}

type GateDecision struct {
	Task     string `json:"id"`
	Decision string `json:"decision"`
	Reason   string `json:"reason_code,omitempty"`
	RunID    string `json:"receipt_run_id,omitempty"` // the local run a REUSE rests on
	Signer   string `json:"signer,omitempty"`
}

// GateTask decides REUSE or RUN for one task from verified statements.
// rejected carries reason codes for receipts that failed verification (bad
// signature, untrusted signer, malformed), so a RUN can say why. A statement
// is accepted only if every bound field matches; anything else means RUN.
func GateTask(t *task.Task, exp Expected, stmts []evidence.VerifiedStatement, rejected []string) GateDecision {
	d := GateDecision{Task: t.ID, Decision: GateRun}
	if !t.Reuse.Allowed {
		d.Reason = ReuseDisabled
		return d
	}
	key := task.ActionKey(exp.Contract, exp.Source, exp.Env, exp.VCS)
	// A failure of exactly this work, signed by any trusted key, outweighs every
	// pass: reusing a pass would hide a flaky or broken check, or reward
	// retrying until green (plan §11.2).
	for _, vs := range stmts {
		p := vs.Predicate
		if p.Task.ID == t.ID && p.Result.Status == "failed" && p.RepositoryID == exp.RepositoryID &&
			"sha256:"+vs.Subject[0].Digest["sha256"] == key && exp.Now.Sub(p.Timing.StartedAt) <= t.Reuse.MaxAge {
			return GateDecision{Task: t.ID, Decision: GateRun, Reason: FailedElsewhere, RunID: p.RunID, Signer: vs.Signer}
		}
	}
	best := ""
	for _, vs := range stmts {
		p := vs.Predicate
		if p.Task.ID != t.ID || p.Result.Status != "passed" {
			continue // only passes are reused; failures are handled above
		}
		reason := ""
		started := p.Timing.StartedAt
		switch {
		case p.RepositoryID != exp.RepositoryID:
			reason = WrongRepository
		case p.Task.ContractDigest != exp.Contract:
			reason = TaskDefinitionChanged
		case p.EnvDigest != exp.Env:
			reason = EnvironmentMismatch
		case p.Source.Digest != exp.Source:
			reason = InputsChanged
		case p.VCS != exp.VCS:
			reason = VCSMismatch
		case "sha256:"+vs.Subject[0].Digest["sha256"] != key:
			reason = Malformed // fields agree but the signed action key doesn't: never trust it
		case p.Result.Origin != "fresh" && !(p.Result.Origin == "tool_cache" && t.AcceptToolCache):
			reason = ToolCacheRejected
		case !t.Reuse.AcceptsCapture(p.Source.Capture):
			reason = WorkspaceEvidence
		case exp.Now.Sub(started) > t.Reuse.MaxAge || started.After(exp.Now.Add(5*time.Minute)):
			reason = ReceiptExpired
		// A run that used over half the job's timeout could time out on a
		// slower CI runner (plan §11.2). ponytail: compares raw local time; scale
		// by a measured local:CI speed ratio once shadow pairs record one.
		case t.Timeout > 0 && time.Duration(p.Timing.DurationMS)*time.Millisecond > t.Timeout/2:
			reason = TooCloseToTimeout
		}
		if reason == "" {
			return GateDecision{Task: t.ID, Decision: GateReuse, RunID: p.RunID, Signer: vs.Signer}
		}
		if best == "" || rank(reason) < rank(best) {
			best = reason
		}
	}
	for _, r := range rejected {
		if best == "" || rank(r) < rank(best) {
			best = r
		}
	}
	if best == "" {
		best = NoReceipt
	}
	d.Reason = best
	return d
}

// rank orders reasons by how useful they are to the person reading the report:
// the closest miss first.
func rank(reason string) int {
	switch reason {
	case ReceiptExpired, ToolCacheRejected, WorkspaceEvidence:
		return 0
	case InputsChanged, VCSMismatch:
		return 1
	case EnvironmentMismatch:
		return 2
	case TaskDefinitionChanged:
		return 3
	case WrongRepository:
		return 4
	case SignerNotAllowed:
		return 5
	case SignatureInvalid, Malformed:
		return 6
	}
	return 7
}

// GatePasses: every required task is accounted for by a REUSE or a RUN that
// passed (plan §1). executed maps task ID → whether its inline run passed.
func GatePasses(decisions []GateDecision, executed map[string]bool) bool {
	if len(decisions) == 0 {
		return false // no tasks means a broken policy, never a green
	}
	for _, d := range decisions {
		if d.Decision == GateReuse {
			continue
		}
		if passed, ran := executed[d.Task]; !ran || !passed {
			return false
		}
	}
	return true
}
