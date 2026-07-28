package supervisor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

// DefaultMaxReworkRounds bounds how many times rework will re-dispatch the same
// worker on a further request-changes verdict before giving up and escalating
// to the operator, so an LLM reviewer that keeps finding (or inventing) fault
// can't loop the worker forever.
const DefaultMaxReworkRounds = 3

// ReworkBrief is the task brief argus injects when re-dispatching a worker to
// address a request-changes (or needs-human) verdict. Unlike RebaseBrief's
// fixed git recipe, the fix itself is exactly what the worker was already
// doing — the findings are the only new information — so this stays close to
// the original task text plus the reviewer's concrete feedback.
func ReworkBrief(task, branch, base string, findings []string, round, maxRounds int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Task: address review feedback on branch %s (rework round %d/%d)\n\n", branch, round, maxRounds)
	if task != "" {
		fmt.Fprintf(&b, "Original task:\n%s\n\n", task)
	}
	b.WriteString("A review of your change did not approve it. Address every finding below in\n")
	b.WriteString("place on this same branch — do NOT start over or open a new PR:\n\n")
	for _, f := range findings {
		fmt.Fprintf(&b, "  - %s\n", f)
	}
	b.WriteString("\nRe-run the repo's checks (make ci, or make test + make lint) once you believe\n")
	b.WriteString("every finding is addressed, then set your status phase to \"awaiting_review\"\n")
	b.WriteString("again. Use \"blocked\" if a finding needs a decision only the supervisor can make.\n\n")
	b.WriteString("Leave \"title\" empty in this round's report unless you are deliberately\n")
	b.WriteString("retitling the whole PR — a title describing only this round's fix (e.g. a\n")
	b.WriteString("small test-isolation nit) would replace the title that already describes the\n")
	b.WriteString("entire change. Empty carries the existing title forward unchanged.\n\n")
	b.WriteString(protocol.WriterBrief(base))
	return b.String()
}

// JudgeResult is what JudgeOne learned about a single already-terminal worker:
// the deterministic gate's own verdict, and — when the gate escalated and a
// reviewer was configured — the reviewer's verdict. Review is nil when the
// gate auto-approved (no reviewer call was needed) or no reviewer ran (not
// configured, or DiffFor/the reviewer itself errored — see ReviewErr).
type JudgeResult struct {
	Review    *ReviewResult
	ReviewErr error
	Gate      Verdict
}

// JudgeOne runs the same measure -> gate -> (reviewer on escalation) -> persist
// pipeline the main supervise loop runs per worker (reconcile, reviewEscalations)
// for exactly one worker whose terminal status was obtained outside Run/Attach's
// own spawn/watch path — e.g. rework, which re-dispatches an existing worker in
// place and then needs to judge its fresh report the same deterministic way. The
// resulting Approval is persisted to the worktree exactly as the main loop's
// gate does (see recordApproval), so ship sees it — closing the gap where a
// manual `argus review` verdict was never saved anywhere ship could check.
//
// prior is the worktree's verdict as it stood before this round's dispatch, or
// nil if none existed. It must be the caller's own snapshot, taken before
// anything that might invalidate the worktree's verdict.json (rework's
// InvalidateStatus deletes it ahead of every round so a stale terminal status
// left over from before that round can't be mistaken for this round's own
// report) — reconcile can no longer be trusted to read it back off disk by the
// time JudgeOne runs. Passing it explicitly is what lets gateVerdict's
// under-report check subtract only the delta since that prior verdict instead
// of the full cumulative diff since base.
//
// priorContentHash is a digest of the worktree's touched-file bytes as they
// stood before this round's dispatch (see cmd's preRoundContentHash) — the very
// state the prior verdict already found wanting. gateVerdict compares this round's
// post-dispatch content hash against it to catch a rework round that reaches a
// terminal phase having changed literally nothing. It is not read from prior
// because a non-approved verdict (the only kind rework acts on) never persisted a
// ContentHash; "" disables the check for that round.
func JudgeOne(ctx context.Context, cfg *Config, plan *WorkerPlan, status *protocol.Status, paneID string, dispatchedAt time.Time, prior *protocol.Approval, priorContentHash string) JudgeResult {
	st := &workerState{plan: plan, paneID: paneID, started: dispatchedAt, status: *status, hasFile: true, priorContentHash: priorContentHash}
	if prior != nil {
		st.priorMeasured = prior.MeasuredDiff
		st.priorMeasuredOK = true
	}
	states := []*workerState{st}
	reconcile(ctx, cfg, states)
	reviewEscalations(ctx, cfg, states, nil)
	return JudgeResult{Gate: gateVerdict(st, cfg.Policy), Review: st.review, ReviewErr: st.reviewErr}
}
