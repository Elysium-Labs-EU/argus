package protocol

import (
	"slices"
	"testing"
)

func TestDeniedInPhase(t *testing.T) {
	// git commit/push are denied in every phase now, not just planning — a
	// worker never commits or pushes at all; only argus ship does, once a
	// verdict exists. Test every phase, not just planning, to guard the
	// exact escalation this replaced: commit/push used to be merely
	// ask-gated outside planning.
	every := append([]Phase{Phase(""), PhaseDone, PhaseRebase}, ConfigurablePhases...)
	for _, p := range every {
		got := DeniedInPhase(p)
		if !slices.Equal(got, DenyFloor()) {
			t.Errorf("DeniedInPhase(%q) = %v, want exactly DenyFloor() %v", p, got, DenyFloor())
		}
		for _, want := range AskGatedCommands {
			if !slices.Contains(got, want) {
				t.Errorf("DeniedInPhase(%q) = %v, missing AskGatedCommands entry %q", p, got, want)
			}
		}
		for _, want := range AlwaysDeniedCommands {
			if !slices.Contains(got, want) {
				t.Errorf("DeniedInPhase(%q) = %v, missing AlwaysDeniedCommands entry %q", p, got, want)
			}
		}
	}
}

// TestDenyFloorDeniesRecordPlan pins the specific addition this issue makes
// to DenyFloor: `argus worker record-plan` only ever runs as a PostToolUse
// hook argus itself wires (see supervisor.recordPlanHooks), never a worker's
// own Bash self-invocation — without this, a repo's phases.<name>.allow could
// grant a wide-enough Bash pattern to let a worker forge plan-log.jsonl
// entries by hand.
func TestDenyFloorDeniesRecordPlan(t *testing.T) {
	if !slices.Contains(DenyFloor(), "argus worker record-plan") {
		t.Errorf("DenyFloor() = %v, want it to contain %q", DenyFloor(), "argus worker record-plan")
	}
	if _, ok := MatchesDeniedCommand("argus worker record-plan", DenyFloor()); !ok {
		t.Error("MatchesDeniedCommand(argus worker record-plan, DenyFloor()) = false, want true")
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

// TestCredentialDenyFloor pins the representative entries CredentialDenyFloor
// must carry — one home-anchored file, one home-anchored directory
// (Read+Edit), one location-agnostic wildcard, and ~/.claude/** — and guards
// against a wildcarded .claude form ever creeping in, since a worker must
// still be able to read its own worktree's .claude/ directory.
func TestCredentialDenyFloor(t *testing.T) {
	got := CredentialDenyFloor()

	want := []string{
		"Read(~/.ssh/**)",
		"Edit(~/.ssh/**)",
		"Read(~/.claude/**)",
		"Read(**/.git-credentials)",
	}
	for _, entry := range want {
		if !slices.Contains(got, entry) {
			t.Errorf("CredentialDenyFloor() = %v, missing %q", got, entry)
		}
	}

	for _, bad := range []string{"Read(**/.claude/**)", "Edit(**/.claude/**)"} {
		if slices.Contains(got, bad) {
			t.Errorf("CredentialDenyFloor() = %v, must not contain wildcard .claude form %q (a worktree's own .claude/ must stay readable)", got, bad)
		}
	}
}

// TestCredentialDenyFloorReturnsCopy guards the same mutation hazard
// DenyFloor's own slices.Clone protects against: a caller mutating the
// returned slice must never corrupt the package-level backing data other
// callers read next.
func TestCredentialDenyFloorReturnsCopy(t *testing.T) {
	got := CredentialDenyFloor()
	if len(got) == 0 {
		t.Fatal("CredentialDenyFloor() returned an empty slice")
	}
	got[0] = "mutated"

	again := CredentialDenyFloor()
	if len(again) == 0 {
		t.Fatal("CredentialDenyFloor() returned an empty slice")
	}
	if again[0] == "mutated" {
		t.Error("CredentialDenyFloor() leaked its backing array; want an independent copy each call")
	}
}

func TestResolvedDenyForPhase(t *testing.T) {
	if got := ResolvedDenyForPhase(PhaseWorking, nil); !slices.Equal(got, DenyFloor()) {
		t.Errorf("no project config, working phase = %v, want exactly the DenyFloor() floor %v", got, DenyFloor())
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
