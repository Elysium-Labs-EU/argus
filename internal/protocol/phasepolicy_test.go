package protocol

import (
	"slices"
	"testing"
)

func TestDeniedInPhase(t *testing.T) {
	planning := DeniedInPhase(PhasePlanning)
	for _, want := range AskGatedCommands {
		if !slices.Contains(planning, want) {
			t.Errorf("DeniedInPhase(planning) = %v, missing AskGatedCommands entry %q", planning, want)
		}
	}
	for _, want := range AlwaysDeniedCommands {
		if !slices.Contains(planning, want) {
			t.Errorf("DeniedInPhase(planning) = %v, missing AlwaysDeniedCommands entry %q", planning, want)
		}
	}

	other := []Phase{Phase(""), PhaseWorking, PhaseSelfTest, PhaseAwaitingReview, PhaseBlocked, PhaseDone}
	for _, p := range other {
		got := DeniedInPhase(p)
		if !slices.Equal(got, AlwaysDeniedCommands) {
			t.Errorf("DeniedInPhase(%q) = %v, want exactly AlwaysDeniedCommands %v", p, got, AlwaysDeniedCommands)
		}
	}
}

func TestAlwaysDeniedCommandsBlockedEveryPhase(t *testing.T) {
	for _, p := range append([]Phase{Phase(""), PhaseDone}, ConfigurablePhases...) {
		for _, cmd := range AlwaysDeniedCommands {
			if _, ok := MatchesDeniedCommand(cmd, DeniedInPhase(p)); !ok {
				t.Errorf("phase %q: AlwaysDeniedCommands entry %q not blocked", p, cmd)
			}
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
	if got := ResolvedDenyForPhase(PhaseWorking, nil); !slices.Equal(got, AlwaysDeniedCommands) {
		t.Errorf("no project config, working phase = %v, want exactly the AlwaysDeniedCommands floor %v", got, AlwaysDeniedCommands)
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
