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

func TestRequiresPlanEvidence(t *testing.T) {
	if !RequiresPlanEvidence(PhasePlanning, PhaseWorking) {
		t.Error("RequiresPlanEvidence(planning, working) = false, want true")
	}
	other := []struct{ cur, next Phase }{
		{PhaseWorking, PhaseSelfTest},
		{PhaseSelfTest, PhaseWorking},
		{PhaseBlocked, PhaseWorking},
		{Phase(""), PhasePlanning},
		{PhasePlanning, PhasePlanning},
	}
	for _, tc := range other {
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
