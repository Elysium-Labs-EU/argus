package protocol

import (
	"strings"
	"testing"
)

func TestIsTerminal(t *testing.T) {
	terminal := []Phase{PhaseAwaitingReview, PhaseDone, PhaseBlocked}
	nonTerminal := []Phase{PhasePlanning, PhaseWorking, PhaseSelfTest, PhaseRebase, Phase("")}
	for _, p := range terminal {
		if !IsTerminal(p) {
			t.Errorf("IsTerminal(%q) = false, want true", p)
		}
	}
	for _, p := range nonTerminal {
		if IsTerminal(p) {
			t.Errorf("IsTerminal(%q) = true, want false", p)
		}
	}
}

func TestStatusAndBriefPaths(t *testing.T) {
	wt := "/repo/.claude/worktrees/feat-x"
	if got := StatusPath(wt); !strings.HasSuffix(got, ".claude/argus/status.json") {
		t.Errorf("StatusPath = %q", got)
	}
	if got := BriefPath(wt); !strings.HasSuffix(got, ".claude/argus/brief.md") {
		t.Errorf("BriefPath = %q", got)
	}
}
