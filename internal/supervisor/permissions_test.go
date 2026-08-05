package supervisor

import (
	"slices"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

func TestResolvedAllowForPhase_StructuralFloorAlwaysPresent(t *testing.T) {
	for _, p := range append([]protocol.Phase{protocol.Phase(""), protocol.PhaseDone}, protocol.ConfigurablePhases...) {
		got := ResolvedAllowForPhase(p, nil, nil, nil)
		for _, want := range structuralFloorAllow {
			if !slices.Contains(got, want) {
				t.Errorf("phase %q: resolved allow %v missing structural floor entry %q", p, got, want)
			}
		}
	}
}

func TestResolvedAllowForPhase_UnionsBaseConfigAndExtra(t *testing.T) {
	project := protocol.PhaseConfig{protocol.PhaseWorking: {Allow: []string{"Bash(go test*)"}}}
	got := ResolvedAllowForPhase(protocol.PhaseWorking, project, []string{"Bash(make *)"}, []string{"Bash(npm ci*)"})
	for _, want := range []string{"Bash(go test*)", "Bash(make *)", "Bash(npm ci*)"} {
		if !slices.Contains(got, want) {
			t.Errorf("resolved allow %v missing %q", got, want)
		}
	}

	// A phase's own allow entry must not leak into a different phase.
	other := ResolvedAllowForPhase(protocol.PhasePlanning, project, nil, nil)
	if slices.Contains(other, "Bash(go test*)") {
		t.Errorf("phases.working.allow leaked into planning: %v", other)
	}
}

func TestResolvedAllowForPhase_DedupesPreservingOrder(t *testing.T) {
	project := protocol.PhaseConfig{protocol.PhaseWorking: {Allow: []string{"Bash(git status*)", "Bash(make *)"}}}
	got := ResolvedAllowForPhase(protocol.PhaseWorking, project, nil, nil)
	count := 0
	for _, e := range got {
		if e == "Bash(git status*)" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("Bash(git status*) appears %d times in %v, want exactly once", count, got)
	}
}

// TestResolvedAllowForPhase_DenyFloorUnremovable is the regression guard for
// the exact privilege-escalation shape a prior attempt shipped: a repo
// config granting git push/commit (or argus's own supervisor commands)
// under some phase's own allow list, or via an operator --allow flag, must
// never survive resolution — in every configurable phase, not just planning.
func TestResolvedAllowForPhase_DenyFloorUnremovable(t *testing.T) {
	denyFloorGlobs := []string{
		"Bash(git push*)", "Bash(git push origin*)",
		"Bash(git commit*)", "Bash(git commit -m*)",
		"Bash(argus ship*)", "Bash(argus rework*)", "Bash(argus review*)", "Bash(argus supervise*)",
		"Bash(git *)", // broad enough to cover commit/push too
	}
	for _, p := range protocol.ConfigurablePhases {
		project := protocol.PhaseConfig{p: {Allow: slices.Clone(denyFloorGlobs)}}
		got := ResolvedAllowForPhase(p, project, denyFloorGlobs, denyFloorGlobs)
		for _, bad := range denyFloorGlobs {
			if slices.Contains(got, bad) {
				t.Errorf("phase %q: deny-floor-conflicting entry %q survived resolution: %v", p, bad, got)
			}
		}
		// git status/diff/log must still be present — stripping must not be
		// so broad it removes the structural floor's own read-only git entries.
		for _, want := range []string{"Bash(git status*)", "Bash(git diff*)", "Bash(git log*)"} {
			if !slices.Contains(got, want) {
				t.Errorf("phase %q: stripping the deny floor over-removed read-only git entry %q: %v", p, want, got)
			}
		}
	}
}

func TestParseBashEntry(t *testing.T) {
	cases := []struct {
		entry      string
		prefix     string
		ok         bool
		wildcarded bool
	}{
		{entry: "Bash(git status*)", prefix: "git status", ok: true, wildcarded: true},
		{entry: "Bash(git status:*)", prefix: "git status", ok: true, wildcarded: true},
		{entry: "Bash(git status)", prefix: "git status", ok: true, wildcarded: false},
		{entry: "Edit(/tmp/**)", ok: false},
		{entry: "not-bash-shaped", ok: false},
		{entry: "Bash(no closing paren", ok: false},
	}
	for _, c := range cases {
		be, ok := parseBashEntry(c.entry)
		if ok != c.ok {
			t.Errorf("parseBashEntry(%q) ok = %v, want %v", c.entry, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if be.prefix != c.prefix || be.wildcarded != c.wildcarded {
			t.Errorf("parseBashEntry(%q) = %+v, want prefix=%q wildcarded=%v", c.entry, be, c.prefix, c.wildcarded)
		}
	}
}

func TestBashEntryMatchesCommand(t *testing.T) {
	wildcarded := bashEntry{prefix: "go test", wildcarded: true}
	if !wildcarded.matchesCommand("go test ./...") {
		t.Error("wildcarded entry should match a longer command sharing its prefix")
	}
	if wildcarded.matchesCommand("go testify") {
		t.Error("wildcarded entry matched across a non-word-boundary (\"go testify\" vs \"go test\")")
	}
	if !wildcarded.matchesCommand("go test") {
		t.Error("wildcarded entry should match its own bare prefix too")
	}

	bare := bashEntry{prefix: "go test", wildcarded: false}
	if bare.matchesCommand("go test ./...") {
		t.Error("bare (unwildcarded) entry matched a longer command; should only match the exact literal")
	}
	if !bare.matchesCommand("go test") {
		t.Error("bare entry should match its own exact literal command")
	}
}

func TestAllowCoversCommand(t *testing.T) {
	allow := []string{"Bash(go test*)", "Edit(/tmp/**)"}
	if !AllowCoversCommand(allow, "go test ./...") {
		t.Error("AllowCoversCommand should match a covered command")
	}
	if AllowCoversCommand(allow, "go build ./...") {
		t.Error("AllowCoversCommand matched an uncovered command")
	}
	if AllowCoversCommand(allow, "") {
		t.Error("AllowCoversCommand matched an empty command against a non-empty allow list")
	}
}

func TestBashGlobEntries(t *testing.T) {
	got := bashGlobEntries([]string{"git commit", "git push"})
	want := []string{"Bash(git commit:*)", "Bash(git push:*)"}
	if !slices.Equal(got, want) {
		t.Errorf("bashGlobEntries = %v, want %v", got, want)
	}
}

func TestStripDenyFloor_PassesThroughNonBashEntries(t *testing.T) {
	allow := []string{"Edit(/tmp/worktree/**)", "Write(/tmp/worktree/**)"}
	got := stripDenyFloor(allow)
	if !slices.Equal(got, allow) {
		t.Errorf("stripDenyFloor changed non-Bash entries: got %v, want %v", got, allow)
	}
}
