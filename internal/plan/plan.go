// Package plan turns a task, the current state of the tree, and recorded runs
// into a decision with a stable reason code (plan §10, §12). Pure: no I/O.
package plan

import (
	"time"

	"github.com/billiondollarsolo/cinema/internal/task"
)

// Reason codes are a stable contract shared by CLI, JSON, dashboard and gate (plan §12).
const (
	NoReceipt              = "NO_RECEIPT"
	InputsChanged          = "INPUTS_CHANGED"
	TaskDefinitionChanged  = "TASK_DEFINITION_CHANGED"
	EnvironmentMismatch    = "ENVIRONMENT_MISMATCH"
	WrongRepository        = "WRONG_REPOSITORY"
	SignatureInvalid       = "SIGNATURE_INVALID"
	SignerNotAllowed       = "SIGNER_NOT_ALLOWED"
	Malformed              = "MALFORMED"
	ReceiptExpired         = "RECEIPT_EXPIRED"
	PartialCoverage        = "PARTIAL_COVERAGE"
	SourceChangedDuringRun = "SOURCE_CHANGED_DURING_RUN"
	ToolCacheRejected      = "TOOL_CACHE_REJECTED"
	ReuseDisabled          = "REUSE_DISABLED"
	ForkPR                 = "FORK_PR"
	PolicyUnavailable      = "POLICY_UNAVAILABLE"
	PolicyInvalid          = "POLICY_INVALID"
	TransportUnavailable   = "TRANSPORT_UNAVAILABLE"
	RunFailed              = "RUN_FAILED"
	VCSMismatch            = "VCS_MISMATCH"       // vcs_context: exact and the commit differs (or tree dirty)
	WorkspaceEvidence      = "WORKSPACE_EVIDENCE" // receipt from a working tree; CI needs a clean checkout (ADR 0002)
)

// Local states for `cinema status` (plan §10.4). Kept separate from "accepted by CI".
const (
	Reusable = "reusable" // a passed run qualifies for exactly this tree, task and environment
	Stale    = "stale"    // passed runs exist, none qualifies
	Missing  = "missing"  // never run here
	Failed   = "failed"   // the latest matching run failed
	Always   = "always"   // reuse disabled: always runs in CI
)

// Current is the state the decision is made against.
type Current struct {
	Contract, Source, Env, VCS string
	Now                        time.Time
}

// Run is a recorded run of the task, as the store returns it.
type Run struct {
	ID, State                 string // store states: passed, failed, ...
	Started                   time.Time
	Contract, Env, VCS        string
	SourceBefore, SourceAfter string
	Origin                    string // "fresh" | "tool_cache" (plan §4.5)
}

type Decision struct {
	Task   string `json:"id"`
	State  string `json:"state"`
	Reason string `json:"reason_code,omitempty"`
	RunID  string `json:"run_id,omitempty"` // the run that qualifies, or the one the reason describes
}

// Local decides whether t is reusable for cur from runs (newest first).
// Any doubt means not reusable: unknown means run, never approve (plan §1).
func Local(t *task.Task, cur Current, runs []Run) Decision {
	d := Decision{Task: t.ID}
	if !t.Reuse.Allowed {
		d.State, d.Reason = Always, ReuseDisabled
		return d
	}
	if len(runs) == 0 {
		d.State, d.Reason = Missing, NoReceipt
		return d
	}
	var best *Run
	bestReason := ""
	for i := range runs {
		r := &runs[i]
		if r.State == "interrupted" {
			continue // stopped by the user: neither evidence nor a failure
		}
		if r.State != "passed" {
			// A failure on these exact inputs outweighs any pass: reusing the pass
			// would hide a flaky or broken check. Failures elsewhere don't count.
			if r.Contract == cur.Contract && r.Env == cur.Env && r.SourceBefore == cur.Source && r.Contract != "" {
				return Decision{Task: t.ID, State: Failed, Reason: RunFailed, RunID: r.ID}
			}
			continue
		}
		reason := disqualify(t, cur, r)
		if reason == "" {
			return Decision{Task: t.ID, State: Reusable, RunID: r.ID}
		}
		if best == nil { // describe the newest passed run
			best, bestReason = r, reason
		}
	}
	switch {
	case runs[0].State != "passed" && runs[0].State != "interrupted": // latest attempt failed, on some other tree
		d.State, d.Reason, d.RunID = Failed, RunFailed, runs[0].ID
	case best != nil:
		d.State, d.Reason, d.RunID = Stale, bestReason, best.ID
	default:
		d.State, d.Reason = Missing, NoReceipt
	}
	return d
}

// disqualify returns why r can't stand in for a fresh run on cur, or "".
// Order: the most actionable explanation first.
func disqualify(t *task.Task, cur Current, r *Run) string {
	switch {
	case r.SourceBefore == "" || r.SourceAfter == "" || r.Contract == "" || r.Env == "":
		return NoReceipt // incomplete record: never evidence
	case r.SourceBefore != r.SourceAfter:
		return SourceChangedDuringRun
	case r.Origin != "fresh" && !(r.Origin == "tool_cache" && t.AcceptToolCache):
		return ToolCacheRejected // replayed (or unknown) results never pass as fresh
	case r.Contract != cur.Contract:
		return TaskDefinitionChanged
	case r.Env != cur.Env:
		return EnvironmentMismatch
	case r.SourceBefore != cur.Source:
		return InputsChanged
	case t.VCSContext == "exact" && (r.VCS == "" || r.VCS == "dirty" || r.VCS != cur.VCS):
		return VCSMismatch
	case cur.Now.Sub(r.Started) > t.Reuse.MaxAge || r.Started.After(cur.Now.Add(time.Minute)):
		return ReceiptExpired // a start time in the future is as untrustworthy as an old one
	}
	return ""
}
