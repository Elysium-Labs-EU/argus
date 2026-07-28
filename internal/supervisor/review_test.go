package supervisor

import (
	"strings"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

func TestAssess(t *testing.T) {
	pass := []protocol.TestRun{{Cmd: "make test", Result: protocol.ResultPass}}

	cases := []struct {
		policy     *ReviewPolicy
		name       string
		reasonHint string
		status     protocol.Status
		approve    bool
	}{
		{
			name: "clean small diff auto-approves",
			status: protocol.Status{
				Phase:        protocol.PhaseAwaitingReview,
				Tests:        pass,
				FilesTouched: []string{"cmd/env.go"},
				DiffStat:     protocol.DiffStat{Insertions: 40, Deletions: 5},
			},
			approve: true,
		},
		{
			name: "failing test escalates",
			status: protocol.Status{
				Phase: protocol.PhaseDone,
				Tests: []protocol.TestRun{{Cmd: "make test", Result: protocol.ResultFail}},
			},
			approve:    false,
			reasonHint: "test failed",
		},
		{
			name: "intentional failure alongside a clean pass auto-approves",
			status: protocol.Status{
				Phase: protocol.PhaseAwaitingReview,
				Tests: []protocol.TestRun{
					{Cmd: "make gate-check", Result: protocol.ResultFail, ExpectedResult: protocol.ResultFail},
					{Cmd: "make gate-check", Result: protocol.ResultPass},
				},
			},
			approve: true,
		},
		{
			name: "intentional failure with no clean pass still escalates",
			status: protocol.Status{
				Phase: protocol.PhaseAwaitingReview,
				Tests: []protocol.TestRun{
					{Cmd: "make gate-check", Result: protocol.ResultFail, ExpectedResult: protocol.ResultFail},
				},
			},
			approve:    false,
			reasonHint: "no clean-state passing test",
		},
		{
			name: "expected_result set to pass does not suppress a real failure",
			status: protocol.Status{
				Phase: protocol.PhaseAwaitingReview,
				Tests: []protocol.TestRun{
					{Cmd: "make test", Result: protocol.ResultFail, ExpectedResult: protocol.ResultPass},
				},
			},
			approve:    false,
			reasonHint: "test failed",
		},
		{
			name: "oversized diff escalates",
			status: protocol.Status{
				Phase:    protocol.PhaseAwaitingReview,
				Tests:    pass,
				DiffStat: protocol.DiffStat{Insertions: 500, Deletions: 100},
			},
			policy:     &ReviewPolicy{MaxDiffLines: 400},
			approve:    false,
			reasonHint: "exceeds max",
		},
		// This case used to be "shared path escalates", set SharedGlobs, and
		// asserted a distinct "shared path" reason. SharedGlobs was
		// deliberately folded into AlwaysReviewPaths (see ReviewPolicy's doc
		// comment) rather than kept separate, so this case now exercises the
		// same escalation through AlwaysReviewPaths instead of being dropped.
		{
			name: "always-review path escalates",
			status: protocol.Status{
				Phase:        protocol.PhaseAwaitingReview,
				Tests:        pass,
				FilesTouched: []string{"internal/config/config.go"},
			},
			policy:     &ReviewPolicy{AlwaysReviewPaths: []string{"internal/config"}},
			approve:    false,
			reasonHint: "behavior-critical",
		},
		{
			name: "OS-integration change without proof escalates",
			status: protocol.Status{
				Phase:        protocol.PhaseAwaitingReview,
				Tests:        pass,
				FilesTouched: []string{"internal/service/systemd.go"},
			},
			approve:    false,
			reasonHint: "no real-world proof",
		},
		{
			name: "OS-integration change with proof auto-approves",
			status: protocol.Status{
				Phase:          protocol.PhaseAwaitingReview,
				Tests:          pass,
				FilesTouched:   []string{"internal/service/systemd.go"},
				RealWorldProof: "ran on orb debian, systemctl status active",
			},
			approve: true,
		},
		{
			name: "argus config change escalates by default",
			status: protocol.Status{
				Phase:        protocol.PhaseAwaitingReview,
				Tests:        pass,
				FilesTouched: []string{".argus/config.yml"},
				DiffStat:     protocol.DiffStat{Insertions: 1, Deletions: 0},
			},
			approve:    false,
			reasonHint: "behavior-critical",
		},
		{
			name:       "blocked escalates",
			status:     protocol.Status{Phase: protocol.PhaseBlocked, BlockedReason: "needs prod path"},
			approve:    false,
			reasonHint: "blocked",
		},
		{
			name:       "not-ready phase escalates",
			status:     protocol.Status{Phase: protocol.PhaseWorking, Tests: pass},
			approve:    false,
			reasonHint: "not ready",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := Assess(&tc.status, tc.policy)
			if v.AutoApprove != tc.approve {
				t.Fatalf("AutoApprove: got %v want %v (reasons: %v)", v.AutoApprove, tc.approve, v.Reasons)
			}
			if tc.approve && len(v.Reasons) != 0 {
				t.Errorf("auto-approved verdict should have no reasons, got %v", v.Reasons)
			}
			if tc.reasonHint != "" {
				found := false
				for _, r := range v.Reasons {
					if strings.Contains(r, tc.reasonHint) {
						found = true
					}
				}
				if !found {
					t.Errorf("want a reason containing %q, got %v", tc.reasonHint, v.Reasons)
				}
			}
		})
	}
}

// TestAssessIntentionalFailureIsNotedNotEscalated pins the fix that lets a
// worker mark a deliberate break-then-revert test run as expected: the run
// itself must not appear in Reasons (it would read as a real regression to a
// reviewer skimming escalations), but it must still surface somewhere so the
// gate isn't silently swallowing information about what happened.
func TestAssessIntentionalFailureIsNotedNotEscalated(t *testing.T) {
	status := protocol.Status{
		Phase: protocol.PhaseAwaitingReview,
		Tests: []protocol.TestRun{
			{Cmd: "make gate-check", Result: protocol.ResultFail, ExpectedResult: protocol.ResultFail},
			{Cmd: "make gate-check", Result: protocol.ResultPass},
		},
	}
	v := Assess(&status, nil)
	if !v.AutoApprove {
		t.Fatalf("want auto-approve, got reasons %v", v.Reasons)
	}
	for _, r := range v.Reasons {
		if strings.Contains(r, "make gate-check") {
			t.Errorf("intentional failure must not appear in Reasons, got %v", v.Reasons)
		}
	}
	found := false
	for _, n := range v.Notes {
		if strings.Contains(n, "make gate-check") {
			found = true
		}
	}
	if !found {
		t.Errorf("want a note about the intentional failure, got %v", v.Notes)
	}
}
