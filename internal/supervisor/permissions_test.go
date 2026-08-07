package supervisor

import (
	"slices"
	"strings"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

// allReportedAndRebasePhases is every phase ResolvedAllowForPhase is ever
// actually called with in production: the five reportable phases plus the
// argus-stamped rebase phase. There is deliberately no Phase("") here any
// more — see internal/protocol/transition.go.
var allReportedAndRebasePhases = append(slices.Clone(protocol.ConfigurablePhases), protocol.PhaseRebase)

func TestResolvedAllowForPhase_StructuralFloorAlwaysPresent(t *testing.T) {
	for _, p := range allReportedAndRebasePhases {
		got := ResolvedAllowForPhase(p, "/tmp/wt", nil, nil, nil)
		for _, want := range allPhaseFloor {
			if !slices.Contains(got, want) {
				t.Errorf("phase %q: resolved allow %v missing all-phase floor entry %q", p, got, want)
			}
		}
	}
}

// TestStructuralFloorCoversWriterBriefCommands confirms every git command
// protocol.WriterBrief instructs (shared by every dispatch path: the initial
// spawn brief, RebaseBrief, and ReworkBrief) is covered by the structural
// floor alone, with no repo config or extraAllow needed — the same
// brief-instructs-a-command-nothing-grants gap RebaseBrief's git fetch/merge
// once hit, but for every worker's routine status report instead of one
// operation, so it must hold with zero config in every phase.
func TestStructuralFloorCoversWriterBriefCommands(t *testing.T) {
	brief := protocol.WriterBrief("origin/main")
	commands := []string{"git diff --stat origin/main", "git ls-files --others --exclude-standard"}
	for _, cmd := range commands {
		if !strings.Contains(brief, cmd) {
			t.Fatalf("test setup: WriterBrief does not actually instruct %q:\n%s", cmd, brief)
		}
	}
	for _, phase := range allReportedAndRebasePhases {
		allow := ResolvedAllowForPhase(phase, "/tmp/wt", nil, nil, nil)
		for _, cmd := range commands {
			if !AllowCoversCommand(allow, cmd) {
				t.Errorf("phase %q: structural floor does not cover WriterBrief command %q: %v", phase, cmd, allow)
			}
		}
	}
}

// TestResolvedAllowForPhase_EditWriteOnlyDuringMutationPhases is the phase-
// scoping guarantee at the center of this package: a worker may only mutate
// tracked files while actually building the change or resolving a rebase
// conflict.
func TestResolvedAllowForPhase_EditWriteOnlyDuringMutationPhases(t *testing.T) {
	worktree := "/tmp/wt"
	editEntry := "Edit(" + absPathPattern(worktree+"/**") + ")"
	writeEntry := "Write(" + absPathPattern(worktree+"/**") + ")"
	mutating := map[protocol.Phase]bool{
		protocol.PhaseWorking:  true,
		protocol.PhaseSelfTest: true,
		protocol.PhaseRebase:   true,
	}
	for _, p := range allReportedAndRebasePhases {
		got := ResolvedAllowForPhase(p, worktree, nil, nil, nil)
		has := slices.Contains(got, editEntry) && slices.Contains(got, writeEntry)
		if has != mutating[p] {
			t.Errorf("phase %q: Edit/Write present=%v, want %v (allow=%v)", p, has, mutating[p], got)
		}
	}
}

// TestResolvedAllowForPhase_RebaseGetsGitFetchMergeAndVerify confirms the
// rebase phase's own grant (computed by cmd/worker_check_tool.go from
// supervisor.RebasePhaseAllow and passed in as extraAllow) reaches
// ResolvedAllowForPhase only for protocol.PhaseRebase, never any other
// phase — the replacement for the old blanket extraAllow injection.
func TestResolvedAllowForPhase_RebaseGetsGitFetchMergeAndVerify(t *testing.T) {
	rebaseAllow := RebasePhaseAllow("main", "make ci", "")
	for _, want := range []string{"Bash(git fetch origin main)", "Bash(git merge origin/main --no-commit)", "Bash(make ci)"} {
		if !slices.Contains(rebaseAllow, want) {
			t.Fatalf("test setup: RebasePhaseAllow(%q, ...) missing %q: %v", "main", want, rebaseAllow)
		}
	}
	got := ResolvedAllowForPhase(protocol.PhaseRebase, "/tmp/wt", nil, nil, rebaseAllow)
	for _, want := range rebaseAllow {
		if !slices.Contains(got, want) {
			t.Errorf("phase rebase: resolved allow %v missing rebase grant %q", got, want)
		}
	}
	// The same rebaseAllow entries handed to a non-rebase phase's extraAllow
	// must never leak in from anywhere but the caller's own choice to pass
	// them — ResolvedAllowForPhase itself does not special-case rebase
	// commands, so a working-phase resolution that was never given them
	// simply doesn't have them.
	other := ResolvedAllowForPhase(protocol.PhaseWorking, "/tmp/wt", nil, nil, nil)
	for _, cmd := range rebaseAllow {
		if slices.Contains(other, cmd) {
			t.Errorf("phase working: rebase-only grant %q leaked in unasked: %v", cmd, other)
		}
	}
}

func TestResolvedAllowForPhase_UnionsBaseConfigAndExtra(t *testing.T) {
	project := protocol.PhaseConfig{protocol.PhaseWorking: {Allow: []string{"Bash(go test*)"}}}
	got := ResolvedAllowForPhase(protocol.PhaseWorking, "/tmp/wt", project, []string{"Bash(make *)"}, []string{"Bash(npm ci*)"})
	for _, want := range []string{"Bash(go test*)", "Bash(make *)", "Bash(npm ci*)"} {
		if !slices.Contains(got, want) {
			t.Errorf("resolved allow %v missing %q", got, want)
		}
	}

	// A phase's own allow entry must not leak into a different phase.
	other := ResolvedAllowForPhase(protocol.PhasePlanning, "/tmp/wt", project, nil, nil)
	if slices.Contains(other, "Bash(go test*)") {
		t.Errorf("phases.working.allow leaked into planning: %v", other)
	}
}

func TestResolvedAllowForPhase_DedupesPreservingOrder(t *testing.T) {
	project := protocol.PhaseConfig{protocol.PhaseWorking: {Allow: []string{"Bash(git status*)", "Bash(make *)"}}}
	got := ResolvedAllowForPhase(protocol.PhaseWorking, "/tmp/wt", project, nil, nil)
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
// never survive resolution — in every phase, not just planning, and not just
// the five reportable ones now that rebase is also governed.
func TestResolvedAllowForPhase_DenyFloorUnremovable(t *testing.T) {
	denyFloorGlobs := []string{
		"Bash(git push*)", "Bash(git push origin*)",
		"Bash(git commit*)", "Bash(git commit -m*)",
		"Bash(argus ship*)", "Bash(argus rework*)", "Bash(argus review*)", "Bash(argus supervise*)",
		"Bash(git *)", // broad enough to cover commit/push too
	}
	for _, p := range allReportedAndRebasePhases {
		project := protocol.PhaseConfig{p: {Allow: slices.Clone(denyFloorGlobs)}}
		got := ResolvedAllowForPhase(p, "/tmp/wt", project, denyFloorGlobs, denyFloorGlobs)
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

func TestResolvedAllowSet_UnionsAcrossPhases(t *testing.T) {
	project := protocol.PhaseConfig{
		protocol.PhaseWorking:  {Allow: []string{"Bash(go test*)"}},
		protocol.PhasePlanning: {Allow: []string{"Bash(go vet*)"}},
	}
	got := ResolvedAllowSet(project, []string{"Bash(make *)"}, []string{"Bash(npm ci*)"}, nil, "/tmp/wt")
	for _, want := range []string{"Bash(go test*)", "Bash(go vet*)", "Bash(make *)", "Bash(npm ci*)"} {
		if !slices.Contains(got, want) {
			t.Errorf("resolved allow set %v missing %q — must union across every configurable phase, not just one", got, want)
		}
	}
}

// TestResolvedAllowSet_ExcludesRebaseWhenNotGiven confirms
// protocol.ConfigurablePhases itself still excludes protocol.PhaseRebase — a
// nil rebaseAllow (no worktree base known yet) carries no rebase-only grant
// through the per-phase union.
func TestResolvedAllowSet_ExcludesRebaseWhenNotGiven(t *testing.T) {
	if slices.Contains(protocol.ConfigurablePhases, protocol.PhaseRebase) {
		t.Fatal("protocol.ConfigurablePhases must not include PhaseRebase — rebase is argus-stamped, never worker-reported or repo-configurable")
	}
	rebaseOnly := RebasePhaseAllow("main", "", "")
	got := ResolvedAllowSet(nil, nil, nil, nil, "/tmp/wt")
	for _, cmd := range rebaseOnly {
		if slices.Contains(got, cmd) {
			t.Errorf("ResolvedAllowSet unexpectedly includes rebase-only grant %q with no rebaseAllow given: %v", cmd, got)
		}
	}
}

// TestResolvedAllowSet_IncludesGivenRebaseAllow is the regression test for
// the dontAsk pre-hook deadlock: the check-tool PreToolUse hook is deny-only
// (see RebasePhaseAllow's own doc comment), so the rebase phase's git grant
// must be present in the static Allow list ResolvedAllowSet builds — a live
// hook recompute can narrow it back down per phase but can never add it in
// the first place.
func TestResolvedAllowSet_IncludesGivenRebaseAllow(t *testing.T) {
	rebaseAllow := RebasePhaseAllow("main", "", "make ci")
	got := ResolvedAllowSet(nil, nil, nil, rebaseAllow, "/tmp/wt")
	for _, cmd := range rebaseAllow {
		if !slices.Contains(got, cmd) {
			t.Errorf("ResolvedAllowSet missing given rebaseAllow entry %q: %v", cmd, got)
		}
	}
}

func TestResolvedAllowSet_MatchesSettingsForAllow(t *testing.T) {
	project := protocol.PhaseConfig{protocol.PhaseWorking: {Allow: []string{"Bash(go test*)"}}}
	rebaseAllow := RebasePhaseAllow("main", "", "")
	settings := settingsFor("/tmp/wt", project, []string{"Bash(make *)"}, nil, rebaseAllow)
	for _, want := range ResolvedAllowSet(project, []string{"Bash(make *)"}, nil, rebaseAllow, "/tmp/wt") {
		if !slices.Contains(settings.Permissions.Allow, want) {
			t.Errorf("settingsFor's rendered Allow %v missing %q from ResolvedAllowSet — brief and settings file must read the same resolved set", settings.Permissions.Allow, want)
		}
	}
}

func TestAllowSetBrief(t *testing.T) {
	got := AllowSetBrief([]string{"Bash(go test*)", "Bash(git status)", "Edit(/tmp/**)"})
	want := "go test*, git status"
	if got != want {
		t.Errorf("AllowSetBrief = %q, want %q", got, want)
	}

	if got := AllowSetBrief(nil); got != "(none)" {
		t.Errorf("AllowSetBrief(nil) = %q, want %q", got, "(none)")
	}
	if got := AllowSetBrief([]string{"Edit(/tmp/**)"}); got != "(none)" {
		t.Errorf("AllowSetBrief(non-Bash only) = %q, want %q", got, "(none)")
	}
}

func TestPhaseAllowBrief_StatesEachPhaseAndMutationCallout(t *testing.T) {
	project := protocol.PhaseConfig{protocol.PhaseWorking: {Allow: []string{"Bash(go test*)"}}}
	got := PhaseAllowBrief(project, nil, nil, "/tmp/wt")
	for _, p := range protocol.ConfigurablePhases {
		if !strings.Contains(got, string(p)+":") {
			t.Errorf("PhaseAllowBrief missing a line for phase %q:\n%s", p, got)
		}
	}
	if !strings.Contains(got, "go test*") {
		t.Errorf("PhaseAllowBrief missing project-configured working allow entry:\n%s", got)
	}
	workingLine := lineFor(got, "working")
	if !strings.Contains(workingLine, "file edits allowed") {
		t.Errorf("working line should call out file edits allowed: %q", workingLine)
	}
	planningLine := lineFor(got, "planning")
	if !strings.Contains(planningLine, "no file edits") {
		t.Errorf("planning line should call out no file edits: %q", planningLine)
	}
}

func lineFor(block, phase string) string {
	for l := range strings.SplitSeq(block, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), phase+":") {
			return l
		}
	}
	return ""
}

func TestPhaseAllowsMutation(t *testing.T) {
	cases := map[protocol.Phase]bool{
		protocol.PhasePlanning:       false,
		protocol.PhaseWorking:        true,
		protocol.PhaseSelfTest:       true,
		protocol.PhaseAwaitingReview: false,
		protocol.PhaseBlocked:        false,
		protocol.PhaseRebase:         true,
	}
	for phase, want := range cases {
		if got := PhaseAllowsMutation(phase); got != want {
			t.Errorf("PhaseAllowsMutation(%q) = %v, want %v", phase, got, want)
		}
	}
}

// TestAbsPathPattern pins the exact rendering absPathPattern must produce:
// a "//"-prefixed path, the filesystem-absolute form Claude Code's file-path
// permission matcher requires — a single leading "/" is project-root-relative
// and matches nothing under dontAsk (see the helper's own doc comment).
func TestAbsPathPattern(t *testing.T) {
	got := absPathPattern("/tmp/wt/**")
	want := "//tmp/wt/**"
	if got != want {
		t.Errorf("absPathPattern(%q) = %q, want %q", "/tmp/wt/**", got, want)
	}
	if strings.HasPrefix(got, "///") {
		t.Errorf("absPathPattern(%q) = %q, has three or more leading slashes, want exactly two", "/tmp/wt/**", got)
	}
}

// TestStructuralFloorAllow_EditWriteUseDoubleSlash is the regression guard
// for the issue this package shipped without ever exercising end to end: a
// single-slash Edit/Write glob is silently project-root-relative to Claude
// Code, not filesystem-absolute, so it matches no real file under
// --permission-mode dontAsk and every worker edit is denied before argus's
// own check-tool hook is ever reached. The rendered allow glob must use the
// "//"-absolute form, not a bare single slash.
func TestStructuralFloorAllow_EditWriteUseDoubleSlash(t *testing.T) {
	worktree := "/tmp/wt"
	floor := structuralFloorAllow(protocol.PhaseWorking, worktree)
	wantEdit := "Edit(//tmp/wt/**)"
	wantWrite := "Write(//tmp/wt/**)"
	if !slices.Contains(floor, wantEdit) {
		t.Errorf("structuralFloorAllow(working, %q) = %v, missing //-absolute %q", worktree, floor, wantEdit)
	}
	if !slices.Contains(floor, wantWrite) {
		t.Errorf("structuralFloorAllow(working, %q) = %v, missing //-absolute %q", worktree, floor, wantWrite)
	}
	badEdit := "Edit(" + worktree + "/**)"
	badWrite := "Write(" + worktree + "/**)"
	if slices.Contains(floor, badEdit) {
		t.Errorf("structuralFloorAllow(working, %q) = %v, contains single-slash %q — Claude Code treats this as project-root-relative, never matches a real file", worktree, floor, badEdit)
	}
	if slices.Contains(floor, badWrite) {
		t.Errorf("structuralFloorAllow(working, %q) = %v, contains single-slash %q — Claude Code treats this as project-root-relative, never matches a real file", worktree, floor, badWrite)
	}
}

func TestStripDenyFloor_PassesThroughNonBashEntries(t *testing.T) {
	allow := []string{"Edit(/tmp/worktree/**)", "Write(/tmp/worktree/**)"}
	got := stripDenyFloor(allow)
	if !slices.Equal(got, allow) {
		t.Errorf("stripDenyFloor changed non-Bash entries: got %v, want %v", got, allow)
	}
}
