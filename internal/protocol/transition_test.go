package protocol

import "testing"

func TestIsLegalTransition(t *testing.T) {
	legal := []struct{ cur, next Phase }{
		{PhasePlanning, PhasePlanning},
		{PhasePlanning, PhaseWorking},
		{PhaseWorking, PhaseSelfTest},
		{PhaseWorking, PhaseBlocked},
		{PhaseSelfTest, PhaseAwaitingReview},
		{PhaseSelfTest, PhaseWorking},
		{PhaseSelfTest, PhaseBlocked},
		{PhaseAwaitingReview, PhaseWorking},
		{PhaseAwaitingReview, PhaseBlocked},
		{PhaseBlocked, PhaseWorking},
		{PhaseRebase, PhaseAwaitingReview},
		{PhaseRebase, PhaseBlocked},
	}
	for _, tc := range legal {
		if !IsLegalTransition(tc.cur, tc.next) {
			t.Errorf("IsLegalTransition(%q, %q) = false, want true", tc.cur, tc.next)
		}
	}

	illegal := []struct{ cur, next Phase }{
		{"", PhasePlanning},                  // Phase("") has no legal move at all: a missing status.json resolves
		{"", PhaseWorking},                   // as planning at the call sites that read it (see loadCurrentPhase),
		{"", PhaseDone},                      // it is never itself a value IsLegalTransition is asked to move from
		{PhasePlanning, PhaseDone},           // straight to done from planning
		{PhasePlanning, PhaseAwaitingReview}, // skips working/self_test
		{PhaseWorking, PhaseAwaitingReview},  // skips self_test
		{PhaseWorking, PhaseDone},
		{PhaseSelfTest, PhaseDone},
		{PhaseAwaitingReview, PhaseDone},           // done comes from ship, not the worker
		{PhaseAwaitingReview, PhaseAwaitingReview}, // no self-loop
		{PhaseBlocked, PhaseAwaitingReview},
		{PhaseBlocked, PhaseDone},
		{PhaseDone, PhaseWorking},    // done has no legal exit at all
		{PhaseRebase, PhaseRebase},   // no self-loop: argus stamps it once, at dispatch
		{PhaseRebase, PhaseWorking},  // rebase is a dead end besides awaiting_review/blocked
		{PhaseRebase, PhasePlanning}, // rebase never falls back into the normal lifecycle
		{"", PhaseRebase},            // rebase is dispatch-stamped, never entered via a worker report
		{PhaseWorking, PhaseRebase},  // no phase can request its way into rebase
		{PhaseBlocked, PhaseRebase},
	}
	for _, tc := range illegal {
		if IsLegalTransition(tc.cur, tc.next) {
			t.Errorf("IsLegalTransition(%q, %q) = true, want false", tc.cur, tc.next)
		}
	}
}

// TestRequiresPlanEvidence table-tests RequiresPlanEvidence across every
// legal edge in legalTransitions (plus a couple of illegal ones, since
// RequiresPlanEvidence itself never checks legality) — the gate now covers
// three edges, not just planning -> working, so a table pins every one of
// them individually instead of a single spot check plus an "everything else
// is false" list that would silently go stale if a new edge were added
// without a matching test case.
func TestRequiresPlanEvidence(t *testing.T) {
	gated := []struct{ cur, next Phase }{
		{PhasePlanning, PhaseWorking},
		{PhaseWorking, PhaseSelfTest},
		{PhaseSelfTest, PhaseAwaitingReview},
	}
	for _, tc := range gated {
		if !RequiresPlanEvidence(tc.cur, tc.next) {
			t.Errorf("RequiresPlanEvidence(%q, %q) = false, want true", tc.cur, tc.next)
		}
	}

	ungated := []struct{ cur, next Phase }{
		{PhasePlanning, PhasePlanning},
		{PhaseSelfTest, PhaseWorking},
		{PhaseAwaitingReview, PhaseWorking},
		{PhaseAwaitingReview, PhaseBlocked},
		{PhaseBlocked, PhaseWorking},
		{PhaseWorking, PhaseBlocked},
		{PhaseSelfTest, PhaseBlocked},
		{PhaseRebase, PhaseAwaitingReview},
		{Phase(""), PhasePlanning},
		{PhasePlanning, PhaseDone},
		{PhaseWorking, PhaseAwaitingReview},
	}
	for _, tc := range ungated {
		if RequiresPlanEvidence(tc.cur, tc.next) {
			t.Errorf("RequiresPlanEvidence(%q, %q) = true, want false", tc.cur, tc.next)
		}
	}
}

func TestLegalNext(t *testing.T) {
	if got := LegalNext(PhaseWorking); len(got) != 2 || got[0] != PhaseSelfTest || got[1] != PhaseBlocked {
		t.Errorf("LegalNext(working) = %v, want [self_test blocked]", got)
	}
	if got := LegalNext(PhaseDone); len(got) != 0 {
		t.Errorf("LegalNext(done) = %v, want empty", got)
	}
}
