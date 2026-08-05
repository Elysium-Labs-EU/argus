package protocol

import "slices"

// legalTransitions is the single source of truth for which phase moves a worker
// report may make. Before this table, pollStatus/IsTerminal accepted any Phase
// string a worker wrote — every edge in the phase graph was reachable from every
// other phase in practice, the same shape of gap that let a worker's bad
// UpdatedAt silently break argus for 51 minutes. Only forward
// progress, plus recovery to working from self_test, awaiting_review, or
// blocked, is legal. Awaiting_review's recovery edge exists because a worker
// can receive corrective feedback out-of-band (e.g. a human reviewing
// directly in chat rather than through `argus review`/`rework`) while it is
// still the live process in its worktree — it needs a way to reopen its own
// status and resubmit without a second process being dispatched into the same
// worktree.
//
// There is deliberately no Phase("") row: a worker is always in a named
// phase. A fresh worktree with no status.json yet resolves as planning (see
// cmd/worker_report.go's runWorkerReport and cmd/worker_check_tool.go's
// loadCurrentPhase) — the same phase a worker's first real actions (reading
// its brief, building its plan) already are — rather than an ungoverned
// blind spot no config key could ever target.
//
// Planning also self-loops: RequiresPlanEvidence blocks planning -> working
// when the planning report on file carried an empty plan, and self-loop is
// the only way for a worker to refile that same phase with a filled-in plan
// — without it, an empty first planning report would be a dead end with no
// legal move at all. A missing status.json resolving as planning relies on
// this same self-loop for a worker's very first report.
//
// Rebase has no self-loop and no recovery edge of its own: argus stamps it
// once, at dispatch (see supervisor.RebasePhaseAllow's caller,
// dispatchRebaseWorker), and RebaseBrief instructs the worker to move
// straight to awaiting_review or blocked — there is no "report rebase
// again" or "resume rebase from blocked" step in that brief to gate.
//
// PhaseDone is deliberately absent from every value list: a worker report can
// never set it. "Done" means shipped, and only argus's own ship path (not a
// worker call) ever gets to declare that — see the IsTerminal doc comment.
var legalTransitions = map[Phase][]Phase{
	PhasePlanning:       {PhasePlanning, PhaseWorking},
	PhaseWorking:        {PhaseSelfTest, PhaseBlocked},
	PhaseSelfTest:       {PhaseAwaitingReview, PhaseWorking, PhaseBlocked},
	PhaseAwaitingReview: {PhaseWorking, PhaseBlocked},
	PhaseBlocked:        {PhaseWorking},
	PhaseRebase:         {PhaseAwaitingReview, PhaseBlocked},
}

// IsLegalTransition reports whether a worker report may move a worktree's
// status from cur to next. Both the write path (worker report) and any future
// read-side validation should call this instead of re-deriving the phase graph,
// so the two can't drift apart the way the reader and writer once did.
func IsLegalTransition(cur, next Phase) bool {
	return slices.Contains(legalTransitions[cur], next)
}

// LegalNext returns the phases next legally reachable from cur, for use in error
// messages telling a caller what it should have sent instead.
func LegalNext(cur Phase) []Phase {
	return legalTransitions[cur]
}

// planEvidenceEdge is one (cur, next) pair RequiresPlanEvidence gates.
type planEvidenceEdge struct{ cur, next Phase }

// planEvidenceEdges are the phase moves that demand plan evidence: not just
// planning -> working, but every later checkpoint a worker reports through —
// working -> self_test and self_test -> awaiting_review. A single TodoWrite
// at the start of planning used to be cheap-to-fake evidence for every later
// phase too, since the only signal was "does Plan exist on the planning
// report," checked once. Widening the gate to these edges is what makes the
// live per-phase recorder (see internal/supervisor/planlog.go) actually bite:
// each edge needs its own fresh activity, not evidence spent on an earlier
// one — see runWorkerReport's fresh-evidence check and AdvancePlanCheckpoint.
var planEvidenceEdges = []planEvidenceEdge{
	{PhasePlanning, PhaseWorking},
	{PhaseWorking, PhaseSelfTest},
	{PhaseSelfTest, PhaseAwaitingReview},
}

// RequiresPlanEvidence reports whether moving a status from cur to next is one
// of planEvidenceEdges, which additionally demands plan evidence on top of
// being phase-legal. Every worker brief has long said "write a todo list
// before anything else," but that was prose a worker could (and, in real
// sessions, did) ignore entirely — the legal-transition table above only ever
// checked the phase name, not whether planning (or any later phase) actually
// did anything. Gating these edges turns "write a todo list" from an
// instruction into a checked precondition of the worker's own phase contract,
// the same way IsLegalTransition already turned "report phases in order" into
// one.
func RequiresPlanEvidence(cur, next Phase) bool {
	return slices.Contains(planEvidenceEdges, planEvidenceEdge{cur, next})
}
