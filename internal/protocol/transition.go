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
// Phase("") is the fresh-worktree case: no status.json exists yet, so Load
// returns a zero Status and the only legal first move is into planning.
//
// PhaseDone is deliberately absent from every value list: a worker report can
// never set it. "Done" means shipped, and only argus's own ship path (not a
// worker call) ever gets to declare that — see the IsTerminal doc comment.
var legalTransitions = map[Phase][]Phase{
	Phase(""):           {PhasePlanning},
	PhasePlanning:       {PhaseWorking},
	PhaseWorking:        {PhaseSelfTest, PhaseBlocked},
	PhaseSelfTest:       {PhaseAwaitingReview, PhaseWorking, PhaseBlocked},
	PhaseAwaitingReview: {PhaseWorking, PhaseBlocked},
	PhaseBlocked:        {PhaseWorking},
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

// RequiresPlanEvidence reports whether moving a status from cur to next is the
// one edge that additionally demands non-empty Plan evidence on cur, on top of
// being phase-legal: planning -> working. Every worker brief has
// long said "write a todo list before anything else," but that was prose a
// worker could (and, in two real sessions, did) ignore entirely — the
// legal-transition table above only ever checked the phase name, not whether
// planning did anything. Gating this one edge on cur.Plan turns "write a todo
// list" from an instruction into a checked precondition of the worker's own
// phase contract, the same way IsLegalTransition already turned "report phases
// in order" into one.
func RequiresPlanEvidence(cur, next Phase) bool {
	return cur == PhasePlanning && next == PhaseWorking
}
