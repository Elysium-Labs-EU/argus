package protocol

import (
	"slices"
	"testing"
)

func TestDeniedInPhase(t *testing.T) {
	if got := DeniedInPhase(PhasePlanning); !slices.Equal(got, AskGatedCommands) {
		t.Errorf("DeniedInPhase(planning) = %v, want %v", got, AskGatedCommands)
	}

	other := []Phase{Phase(""), PhaseWorking, PhaseSelfTest, PhaseAwaitingReview, PhaseBlocked, PhaseDone}
	for _, p := range other {
		if got := DeniedInPhase(p); got != nil {
			t.Errorf("DeniedInPhase(%q) = %v, want nil", p, got)
		}
	}
}

func TestMatchesDeniedCommand(t *testing.T) {
	denied := []string{"git commit", "git push"}

	matching := []string{
		"git commit -m foo",
		"git commit",
		"git push origin HEAD",
		"  git push  ",
	}
	for _, cmd := range matching {
		if _, ok := MatchesDeniedCommand(cmd, denied); !ok {
			t.Errorf("MatchesDeniedCommand(%q, %v) = false, want true", cmd, denied)
		}
	}

	nonMatching := []string{
		"git status",
		"git diff",
		"",
		"echo git commit", // prefix match is on the command itself, not a substring anywhere in it
	}
	for _, cmd := range nonMatching {
		if matched, ok := MatchesDeniedCommand(cmd, denied); ok {
			t.Errorf("MatchesDeniedCommand(%q, %v) = %q, true, want false", cmd, denied, matched)
		}
	}

	if matched, ok := MatchesDeniedCommand("git commit -m foo", denied); !ok || matched != "git commit" {
		t.Errorf("MatchesDeniedCommand returned (%q, %v), want (\"git commit\", true)", matched, ok)
	}

	if _, ok := MatchesDeniedCommand("anything", nil); ok {
		t.Error("MatchesDeniedCommand with nil denied list = true, want false")
	}
}

func TestResolvedDenyForPhase(t *testing.T) {
	if got := ResolvedDenyForPhase(PhaseWorking, nil); got != nil {
		t.Errorf("no project config, working phase = %v, want nil (floor is empty for working)", got)
	}

	project := PhaseConfig{PhaseWorking: {Deny: []string{"npm publish"}}}
	if got := ResolvedDenyForPhase(PhaseWorking, project); !slices.Contains(got, "npm publish") {
		t.Errorf("project addition = %v, want it to contain %q", got, "npm publish")
	}

	withAdd := PhaseConfig{PhasePlanning: {Deny: []string{"npm publish"}}}
	got := ResolvedDenyForPhase(PhasePlanning, withAdd)
	for _, want := range AskGatedCommands {
		if !slices.Contains(got, want) {
			t.Errorf("floor entry %q missing from planning + project addition: %v", want, got)
		}
	}
	if !slices.Contains(got, "npm publish") {
		t.Errorf("project addition missing from planning result: %v", got)
	}

	skip := PhaseConfig{PhasePlanning: {Skip: true, Deny: []string{"npm publish"}}}
	got = ResolvedDenyForPhase(PhasePlanning, skip)
	if slices.Contains(got, "npm publish") {
		t.Errorf("skip should drop project's own addition, got %v", got)
	}
	for _, want := range AskGatedCommands {
		if !slices.Contains(got, want) {
			t.Errorf("skip must never drop the floor: %q missing from %v", want, got)
		}
	}

	dup := PhaseConfig{PhasePlanning: {Deny: []string{"git commit"}}}
	got = ResolvedDenyForPhase(PhasePlanning, dup)
	count := 0
	for _, d := range got {
		if d == "git commit" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("duplicate floor/project entry should be deduped, got %d copies of %q in %v", count, "git commit", got)
	}
}
