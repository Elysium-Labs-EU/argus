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
		{
			name: "shared path escalates",
			status: protocol.Status{
				Phase:        protocol.PhaseAwaitingReview,
				Tests:        pass,
				FilesTouched: []string{"internal/config/config.go"},
			},
			policy:     &ReviewPolicy{SharedGlobs: []string{"internal/config"}},
			approve:    false,
			reasonHint: "shared path",
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
