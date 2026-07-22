package protocol

import "slices"

// legalTransitions is the single source of truth for which phase moves a worker
// report may make. Before this table, pollStatus/IsTerminal accepted any Phase
// string a worker wrote — every edge in the phase graph was reachable from every
// other phase in practice, the same shape of gap that let a worker's bad
// UpdatedAt silently break argus for 51 minutes (issue #90). Only forward
// progress, plus recovery to working from self_test or blocked, is legal.
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
	PhaseAwaitingReview: {PhaseBlocked},
	PhaseBlocked:        {PhaseWorking},
}

// IsLegalTransition reports whether a worker report may move a worktree's
// status from cur to next. Both the write path (worker report) and any future
// read-side validation should call this instead of re-deriving the phase graph,
// so the two can't drift apart the way the reader and writer did before #92.
func IsLegalTransition(cur, next Phase) bool {
	return slices.Contains(legalTransitions[cur], next)
}

// LegalNext returns the phases next legally reachable from cur, for use in error
// messages telling a caller what it should have sent instead.
func LegalNext(cur Phase) []Phase {
	return legalTransitions[cur]
}
