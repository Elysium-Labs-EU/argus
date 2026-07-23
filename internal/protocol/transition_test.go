package protocol

import "testing"

func TestIsLegalTransition(t *testing.T) {
	legal := []struct{ cur, next Phase }{
		{"", PhasePlanning},
		{PhasePlanning, PhaseWorking},
		{PhaseWorking, PhaseSelfTest},
		{PhaseWorking, PhaseBlocked},
		{PhaseSelfTest, PhaseAwaitingReview},
		{PhaseSelfTest, PhaseWorking},
		{PhaseSelfTest, PhaseBlocked},
		{PhaseAwaitingReview, PhaseBlocked},
		{PhaseBlocked, PhaseWorking},
	}
	for _, tc := range legal {
		if !IsLegalTransition(tc.cur, tc.next) {
			t.Errorf("IsLegalTransition(%q, %q) = false, want true", tc.cur, tc.next)
		}
	}

	illegal := []struct{ cur, next Phase }{
		{"", PhaseWorking},                   // must start at planning
		{"", PhaseDone},                      // done is never a worker move
		{PhasePlanning, PhaseDone},           // straight to done from planning
		{PhasePlanning, PhaseAwaitingReview}, // skips working/self_test
		{PhaseWorking, PhaseAwaitingReview},  // skips self_test
		{PhaseWorking, PhaseDone},
		{PhaseSelfTest, PhaseDone},
		{PhaseAwaitingReview, PhaseWorking},        // review is terminal for the worker
		{PhaseAwaitingReview, PhaseDone},           // done comes from ship, not the worker
		{PhaseAwaitingReview, PhaseAwaitingReview}, // no self-loop
		{PhaseBlocked, PhaseAwaitingReview},
		{PhaseBlocked, PhaseDone},
		{PhaseDone, PhaseWorking}, // done has no legal exit at all
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
