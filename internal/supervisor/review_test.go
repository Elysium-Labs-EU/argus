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
			name: "argus config change always escalates unconditionally",
			status: protocol.Status{
				Phase:        protocol.PhaseAwaitingReview,
				Tests:        pass,
				FilesTouched: []string{".argus/config.yml"},
				DiffStat:     protocol.DiffStat{Insertions: 1, Deletions: 0},
			},
			approve:    false,
			reasonHint: "always reviewed regardless",
		},
		{
			// Proves the fix: a policy whose AlwaysReviewPaths entirely
			// replaces the default list (per-key replace-not-merge semantics,
			// see repoconfig.Config.AlwaysReviewPaths) and omits
			// .argus/config.yml must still escalate on it — selfConfigPath is
			// checked unconditionally in Assess, not sourced from this list.
			name: "argus config change still escalates when a repo's own always_review_paths replaces the default list",
			status: protocol.Status{
				Phase:        protocol.PhaseAwaitingReview,
				Tests:        pass,
				FilesTouched: []string{".argus/config.yml"},
				DiffStat:     protocol.DiffStat{Insertions: 1, Deletions: 0},
			},
			policy:     &ReviewPolicy{AlwaysReviewPaths: []string{"internal/config"}},
			approve:    false,
			reasonHint: "always reviewed regardless",
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

// TestDefaultReviewPolicyExcludesSelfConfigPath is a regression guard against
// re-adding .argus/config.yml to the droppable AlwaysReviewPaths default list
// — it must stay checked unconditionally in Assess via selfConfigPath
// instead, so a repo's own always_review_paths can never silently drop it.
func TestDefaultReviewPolicyExcludesSelfConfigPath(t *testing.T) {
	for _, p := range DefaultReviewPolicy().AlwaysReviewPaths {
		if p == selfConfigPath {
			t.Fatalf("DefaultReviewPolicy().AlwaysReviewPaths contains %q, want it checked only via selfConfigPath in Assess", selfConfigPath)
		}
	}
}

// TestProofForReview pins the fix for issue #603's reviewer/gate disagreement
// over what "proof" means: the reviewer prompt must see real_world_proof only
// when the same proof-required-path match Assess itself uses is present, and
// never otherwise — including when a proof text happens to be set on a change
// that never touched such a path.
func TestProofForReview(t *testing.T) {
	cases := []struct {
		policy *ReviewPolicy
		name   string
		want   string
		status protocol.Status
	}{
		{
			name: "proof-required path with proof returns the proof text",
			status: protocol.Status{
				FilesTouched:   []string{"internal/service/systemd.go"},
				RealWorldProof: "ran on orb debian, systemctl status active",
			},
			want: "ran on orb debian, systemctl status active",
		},
		{
			name: "proof-required path with no proof returns empty",
			status: protocol.Status{
				FilesTouched: []string{"internal/service/systemd.go"},
			},
			want: "",
		},
		{
			name: "non-proof-required path omits proof even when set",
			status: protocol.Status{
				FilesTouched:   []string{"internal/config/config.go"},
				RealWorldProof: "ran on orb debian, systemctl status active",
			},
			want: "",
		},
		{
			name:   "nil policy falls back to DefaultReviewPolicy the same as Assess",
			policy: nil,
			status: protocol.Status{
				FilesTouched:   []string{"cmd/install/main.go"},
				RealWorldProof: "installed and verified the service starts",
			},
			want: "installed and verified the service starts",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProofForReview(&tc.status, tc.policy); got != tc.want {
				t.Errorf("ProofForReview() = %q, want %q", got, tc.want)
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
