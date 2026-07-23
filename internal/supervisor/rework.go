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
func ReworkBrief(task, branch string, findings []string, round, maxRounds int) string {
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
	b.WriteString(protocol.WriterBrief)
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
func JudgeOne(ctx context.Context, cfg *Config, plan *WorkerPlan, status *protocol.Status, paneID string, dispatchedAt time.Time) JudgeResult {
	st := &workerState{plan: plan, paneID: paneID, started: dispatchedAt, status: *status, hasFile: true}
	states := []*workerState{st}
	reconcile(ctx, cfg, states)
	reviewEscalations(ctx, cfg, states)
	return JudgeResult{Gate: gateVerdict(st, cfg.Policy), Review: st.review, ReviewErr: st.reviewErr}
}
