package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

func TestVerifyTestsFlagsFabricatedPass(t *testing.T) {
	wt := t.TempDir()
	tests := []protocol.TestRun{
		{Cmd: "exit 1", Result: protocol.ResultPass},
	}
	mismatches, _ := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 1 {
		t.Fatalf("mismatches = %v, want 1 entry for a claimed pass that actually fails", mismatches)
	}
}

func TestVerifyTestsAcceptsGenuinePass(t *testing.T) {
	wt := t.TempDir()
	tests := []protocol.TestRun{
		{Cmd: "exit 0", Result: protocol.ResultPass},
	}
	mismatches, _ := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 0 {
		t.Fatalf("mismatches = %v, want none for a genuinely passing command", mismatches)
	}
}

// TestVerifyTestsNeverAppendsTargetToCmd is the regression for the gate
// fabricating a command the worker never wrote: Target is a descriptive
// label (and, at most, a cwd hint — see status.go's TestRun.Target doc), not
// an argument. Cmd here requires exactly one positional argument to pass;
// with Target never appended, it receives zero and must fail — a prior
// version that joined Cmd and Target into one command line would have
// received one argument and incorrectly passed.
func TestVerifyTestsNeverAppendsTargetToCmd(t *testing.T) {
	wt := t.TempDir()
	tests := []protocol.TestRun{
		{Cmd: `f() { [ "$#" -eq 1 ]; }; f`, Target: "marker.txt", Result: protocol.ResultPass},
	}
	mismatches, _ := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 1 {
		t.Fatalf("mismatches = %v, want 1 entry — Target must never be appended to Cmd", mismatches)
	}
}

// TestVerifyTestsFileTargetNotADirRunsCmdVerbatim is the acceptance case for
// the issue this guards: a worker uses Target exactly as documented (a
// descriptive label naming what Cmd exercises) — e.g. Cmd "docker buildx
// build -f docker/Dockerfile .", Target "frontend/docker/Dockerfile" to
// record which Dockerfile the build used. Target is a single token, isn't a
// substring of Cmd, isn't comma-separated, and isn't a directory (it's a
// file), so a prior version's fallback appended it as a second positional
// path argument. Cmd here takes no arguments, so that append would inflate
// $# and fail the re-run even though the worker's claimed pass was genuine.
func TestVerifyTestsFileTargetNotADirRunsCmdVerbatim(t *testing.T) {
	wt := t.TempDir()
	dockerDir := filepath.Join(wt, "frontend", "docker")
	if err := os.MkdirAll(dockerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dockerDir, "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []protocol.TestRun{
		{Cmd: `f() { [ "$#" -eq 0 ]; }; f`, Target: "frontend/docker/Dockerfile", Result: protocol.ResultPass},
	}
	mismatches, _ := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 0 {
		t.Fatalf("mismatches = %v, want none — a file-valued Target must not be appended as a positional argument", mismatches)
	}
}

// TestVerifyTestsMakeTargetIgnoresStrayTrailingTokens is the regression for
// the false-negative in the issue where a worker's self-reported Cmd carried
// a stray trailing word after the real make target (e.g. "make lint
// golangci-lint", the worker naming the tool it ran) — make interprets that
// extra token as a second, nonexistent target and fails with "No rule to
// make target", even though the real target on its own is clean.
func TestVerifyTestsMakeTargetIgnoresStrayTrailingTokens(t *testing.T) {
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "Makefile"), []byte("check:\n\t@true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []protocol.TestRun{
		{Cmd: "make check golangci-lint", Result: protocol.ResultPass},
	}
	mismatches, _ := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 0 {
		t.Fatalf("mismatches = %v, want none — the stray trailing token must not be replayed as a second make target", mismatches)
	}
}

// TestVerifyTestsMakeTargetDropsAppendedTarget covers the Target-field shape
// of the same failure (e.g. worker reports {Cmd: "make nilcheck", Target:
// "./..."}): joining would replay "make nilcheck ./...", which make also
// rejects as an unknown second target, even though "make nilcheck" alone is
// clean.
func TestVerifyTestsMakeTargetDropsAppendedTarget(t *testing.T) {
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "Makefile"), []byte("nilcheck:\n\t@true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []protocol.TestRun{
		{Cmd: "make nilcheck", Target: "./...", Result: protocol.ResultPass},
	}
	mismatches, _ := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 0 {
		t.Fatalf("mismatches = %v, want none — Target must not be appended to a make invocation", mismatches)
	}
}

// TestVerifyTestsMakeTargetPreservesAssignmentsDropsPositionals is the
// regression for the issue where the make branch stripped every token after
// the target, including VAR=value assignments a worker's claimed command
// legitimately needs (e.g. "make test TEST=Name", "make adr-find
// Q=concept"): make reads those anywhere on the line as variable
// assignments, not extra targets, so dropping one changes what the guarded
// recipe runs and can fail it. A bare positional word alongside the
// assignment must still be dropped, since make reads that one as a second,
// nonexistent target.
func TestVerifyTestsMakeTargetPreservesAssignmentsDropsPositionals(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
	}{
		{name: "single assignment preserved", cmd: "make check FOO=bar"},
		{name: "multiple assignments preserved", cmd: "make check FOO=bar BAZ=qux"},
		{name: "bare positional word still dropped", cmd: "make check FOO=bar golangci-lint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wt := t.TempDir()
			makefile := "check:\n" +
				"\ttest \"$(FOO)\" = bar\n" +
				"\ttest \"$(BAZ)\" = \"$${BAZ:-}\"\n"
			if err := os.WriteFile(filepath.Join(wt, "Makefile"), []byte(makefile), 0o600); err != nil {
				t.Fatal(err)
			}
			tests := []protocol.TestRun{{Cmd: tc.cmd, Result: protocol.ResultPass}}
			mismatches, _ := VerifyTests(context.Background(), wt, tests, time.Second)
			if len(mismatches) != 0 {
				t.Fatalf("mismatches = %v, want none — assignments must replay and reach the recipe while any stray positional word is dropped", mismatches)
			}
		})
	}
}

// TestVerifyTestsMakeTargetRunsFromTargetSubdirWhenItExists covers a
// monorepo with one Makefile per module (e.g. eos-plugins' eos-sink-* dirs):
// a worker reports {Cmd: "make crap", Target: "eos-sink-logbench"}, and the
// make branch must resolve Target as the subdirectory to run from, not
// discard it in favor of the worktree root — a root-level replay finds no
// such Makefile target and fails with "No rule to make target", even though
// the worker's own run, from inside the plugin dir, genuinely passed.
func TestVerifyTestsMakeTargetRunsFromTargetSubdirWhenItExists(t *testing.T) {
	wt := t.TempDir()
	plugin := filepath.Join(wt, "eos-sink-logbench")
	if err := os.Mkdir(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, "Makefile"), []byte("crap:\n\t@true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []protocol.TestRun{
		{Cmd: "make crap", Target: "eos-sink-logbench", Result: protocol.ResultPass},
	}
	mismatches, _ := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 0 {
		t.Fatalf("mismatches = %v, want none — a directory-valued Target must set cwd for a make replay too, not just the bare-append case", mismatches)
	}
}

// TestVerifyTestsSkipsRedundantTargetAlreadyInCmd is the regression for the
// doubled-path failure: a worker sometimes folds the path into Cmd directly
// and also repeats it in Target. Cmd here fails unless it receives exactly
// one argument, so this only passes if Target is never appended a second
// time.
func TestVerifyTestsSkipsRedundantTargetAlreadyInCmd(t *testing.T) {
	wt := t.TempDir()
	tests := []protocol.TestRun{
		{Cmd: `f() { [ "$#" -eq 1 ]; }; f marker.txt`, Target: "marker.txt", Result: protocol.ResultPass},
	}
	mismatches, _ := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 0 {
		t.Fatalf("mismatches = %v, want none — Target already present in Cmd must not be appended again", mismatches)
	}
}

// TestVerifyTestsRunsFromTargetSubdirWhenItExists is the regression for a
// monorepo per-module Target (e.g. eos-plugins: one go.mod per plugin dir,
// no root module) being appended as a positional argument instead of
// becoming the re-run's cwd. Without the fix, replaying from wt fails to
// find go.mod at all (or errors on the stray extra argument); the worker's
// own original run, from inside the plugin dir, genuinely passed.
func TestVerifyTestsRunsFromTargetSubdirWhenItExists(t *testing.T) {
	wt := t.TempDir()
	plugin := filepath.Join(wt, "eos-sink-logbench")
	if err := os.Mkdir(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, "go.mod"), []byte("module eos-sink-logbench\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []protocol.TestRun{
		{Cmd: "test -f go.mod", Target: "eos-sink-logbench", Result: protocol.ResultPass},
	}
	mismatches, _ := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 0 {
		t.Fatalf("mismatches = %v, want none — a directory-valued Target must set cwd, not become a positional argument", mismatches)
	}
}

// TestVerifyTestsSkipsCommaSeparatedTarget is the regression for the
// multiple-positional-args failure: a worker's Target sometimes lists
// several comma-separated files mirroring the brief's file list. A prior
// version split Target on commas and replayed Cmd once per piece as an
// appended argument; the fix instead never appends Target at all, so Cmd
// (which takes no arguments) must replay bare and pass exactly once.
func TestVerifyTestsSkipsCommaSeparatedTarget(t *testing.T) {
	wt := t.TempDir()
	tests := []protocol.TestRun{
		{Cmd: `f() { [ "$#" -eq 0 ]; }; f`, Target: "a.txt, b.txt", Result: protocol.ResultPass},
	}
	mismatches, _ := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 0 {
		t.Fatalf("mismatches = %v, want none — a comma-separated target must not be split and appended", mismatches)
	}
}

// TestVerifyTestsSkipsLabelShapedTarget is the regression for the gate
// misreading a human-readable, multi-word Target as a positional argument:
// Cmd here takes no arguments, so appending the label would inflate $# and
// fail the re-run even though the worker's claimed pass was genuine.
func TestVerifyTestsSkipsLabelShapedTarget(t *testing.T) {
	wt := t.TempDir()
	tests := []protocol.TestRun{
		{Cmd: `f() { [ "$#" -eq 0 ]; }; f`, Target: "frontend unit tests (vitest)", Result: protocol.ResultPass},
	}
	mismatches, _ := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 0 {
		t.Fatalf("mismatches = %v, want none — a human-readable label must not be appended to Cmd", mismatches)
	}
}

// TestVerifyTestsSkipsTrailingParentheticalInCmd is the regression for a
// worker folding a descriptive aside into Cmd itself rather than Target
// (e.g. `git commit (lefthook pre-commit: format, lint, fieldalignment,
// test)`): replayed verbatim through sh -c, the stray parens are a syntax
// error, which previously read as a reproduced failure even though the
// literal command in front of the aside genuinely passes.
func TestVerifyTestsSkipsTrailingParentheticalInCmd(t *testing.T) {
	wt := t.TempDir()
	tests := []protocol.TestRun{
		{Cmd: "exit 0 (lefthook pre-commit: format, lint, fieldalignment, test)", Result: protocol.ResultPass},
	}
	mismatches, _ := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 0 {
		t.Fatalf("mismatches = %v, want none — a trailing parenthetical aside in Cmd must be stripped before replay", mismatches)
	}
}

// TestVerifyTestsFlagsGenuineFailureWithTrailingParenthetical confirms the
// strip in TestVerifyTestsSkipsTrailingParentheticalInCmd doesn't also mask
// a real failure hiding behind the same shape of aside.
func TestVerifyTestsFlagsGenuineFailureWithTrailingParenthetical(t *testing.T) {
	wt := t.TempDir()
	tests := []protocol.TestRun{
		{Cmd: "exit 1 (lefthook pre-commit: format, lint, fieldalignment, test)", Result: protocol.ResultPass},
	}
	mismatches, _ := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 1 {
		t.Fatalf("mismatches = %v, want 1 entry — a genuine failure must still be flagged after stripping the aside", mismatches)
	}
}

// TestVerifyTestsReportsUnparsableCmdAsUnverifiable is the regression for the
// gate treating a shell *parse* failure the same as a real reproduced
// failure: a Cmd with a descriptive aside embedded before its end (so
// stripTrailingParenthetical's trailing-only match doesn't apply) is not
// literal shell, and sh -c rejects it with a syntax error rather than ever
// running it. That must land in unverifiable, not mismatches — it says
// nothing about whether the worker's underlying claim was true.
func TestVerifyTestsReportsUnparsableCmdAsUnverifiable(t *testing.T) {
	wt := t.TempDir()
	tests := []protocol.TestRun{
		{Cmd: "exit 0 (extra parenthetical) tail-word", Result: protocol.ResultPass},
	}
	mismatches, unverifiable := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 0 {
		t.Fatalf("mismatches = %v, want none — a shell parse failure is not a reproduced failure", mismatches)
	}
	if len(unverifiable) != 1 {
		t.Fatalf("unverifiable = %v, want 1 entry for an unparsable Cmd", unverifiable)
	}
	if !strings.Contains(unverifiable[0], "tail-word") {
		t.Errorf("unverifiable entry should name the offending cmd, got %q", unverifiable[0])
	}
}

// TestVerifyTestsSkipsGitMutationClaims is the regression for a worker
// (e.g. one dispatched by `argus rebase`) reporting `git commit`/`git push`
// as claimed-pass tests[] entries: both are real, one-shot mutations, not
// safely repeatable checks — re-running "git commit" a second time has
// nothing staged (exit 1), which is not evidence the original commit never
// happened. Cmd here would fail if actually re-run, proving VerifyTests
// skips it rather than happening to pass.
func TestVerifyTestsSkipsGitMutationClaims(t *testing.T) {
	wt := t.TempDir()
	tests := []protocol.TestRun{
		{Cmd: "git commit", Result: protocol.ResultPass},
		{Cmd: "git push --force-with-lease", Result: protocol.ResultPass},
		{Cmd: "git merge --ff-only origin/main", Result: protocol.ResultPass},
		{Cmd: "git rebase origin/main", Result: protocol.ResultPass},
	}
	mismatches, _ := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 0 {
		t.Fatalf("mismatches = %v, want none — git commit/push/merge/rebase claims must not be re-run", mismatches)
	}
}

func TestVerifyTestsSkipsFailAndSkippedClaims(t *testing.T) {
	wt := t.TempDir()
	tests := []protocol.TestRun{
		{Cmd: "exit 1", Result: protocol.ResultFail},
		{Cmd: "exit 1", Result: protocol.ResultSkipped},
	}
	mismatches, _ := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 0 {
		t.Fatalf("mismatches = %v, want none — only claimed-pass results are re-verified", mismatches)
	}
}

// TestVerifyTestsRetriesOnceBeforeFlagging is the regression for the
// under-load flake this check kept mistaking for a real regression: a
// command that fails on its first sample but passes on retry (the load-flake
// shape) must not be reported as a mismatch.
func TestVerifyTestsRetriesOnceBeforeFlagging(t *testing.T) {
	wt := t.TempDir()
	marker := filepath.Join(wt, "attempted")
	tests := []protocol.TestRun{
		{Cmd: "test -f " + marker + " && exit 0 || { touch " + marker + "; exit 1; }", Result: protocol.ResultPass},
	}
	mismatches, _ := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 0 {
		t.Fatalf("mismatches = %v, want none — a fail-then-pass pair is a flake, not a real mismatch", mismatches)
	}
}

// TestVerifyTestsFlagsRepeatedFailureWithBothOutputs proves a command that
// fails on both attempts is still reported (the retry only forgives a single
// bad sample), and that the mismatch string carries each attempt's captured
// output — the diagnosis a bare "exit status 2" never gave anyone.
func TestVerifyTestsFlagsRepeatedFailureWithBothOutputs(t *testing.T) {
	wt := t.TempDir()
	tests := []protocol.TestRun{
		{Cmd: "echo boom-output-here; exit 2", Result: protocol.ResultPass},
	}
	mismatches, _ := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 1 {
		t.Fatalf("mismatches = %v, want 1 entry for a command that fails on both attempts", mismatches)
	}
	if !strings.Contains(mismatches[0], "boom-output-here") {
		t.Errorf("mismatch should carry the failing command's captured output, got %q", mismatches[0])
	}
	if !strings.Contains(mismatches[0], "failed twice") {
		t.Errorf("mismatch should note both attempts failed, got %q", mismatches[0])
	}
}

func TestVerifyTestsReportsTimeout(t *testing.T) {
	wt := t.TempDir()
	tests := []protocol.TestRun{
		{Cmd: "sleep 5", Result: protocol.ResultPass},
	}
	mismatches, _ := VerifyTests(context.Background(), wt, tests, 50*time.Millisecond)
	if len(mismatches) != 1 {
		t.Fatalf("mismatches = %v, want 1 timeout entry", mismatches)
	}
}

// writeGoTestFixture drops a minimal go module with two always-passing test
// functions into a fresh temp dir, for regression tests that need a real
// `go test` invocation rather than a shell one-liner.
func writeGoTestFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fixture\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	testFile := `package fixture

import "testing"

func TestFoo(t *testing.T) {}

func TestBar(t *testing.T) {}
`
	if err := os.WriteFile(filepath.Join(dir, "fixture_test.go"), []byte(testFile), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestVerifyTestsReplaysUnquotedRunAlternationArgvStyle is the regression
// for the replay path misreading an unquoted `go test -run` regex
// alternation (e.g. TestFoo|TestBar) as a shell pipeline: sh -c has no way
// to tell that pipe apart from a real one, so it split the command into
// `go test -run TestFoo` piped into a nonexistent `TestBar ./...` and failed
// with exit 127, even though both tests genuinely pass. Argv-style replay
// hands the whole -run value to `go test` untouched.
func TestVerifyTestsReplaysUnquotedRunAlternationArgvStyle(t *testing.T) {
	dir := writeGoTestFixture(t)
	tests := []protocol.TestRun{
		{Cmd: "go test -run TestFoo|TestBar ./...", Result: protocol.ResultPass},
	}
	mismatches, _ := VerifyTests(context.Background(), dir, tests, 30*time.Second)
	if len(mismatches) != 0 {
		t.Fatalf("mismatches = %v, want none — an unquoted -run alternation must replay as one go test invocation, not a shell pipeline", mismatches)
	}
}

func TestGateEscalatesWhenClaimedTestPassDoesNotReproduce(t *testing.T) {
	st := &workerState{
		hasFile:        true,
		measuredOK:     true,
		measured:       protocol.DiffStat{Files: 1, Insertions: 3},
		measuredFiles:  []string{"cmd/root.go"},
		testMismatches: []string{`worker claimed "go test ./..." passed, but re-running it failed: exit status 1`},
		plan:           &WorkerPlan{Worker: Worker{Task: "fabricated-pass"}},
		status: protocol.Status{
			Phase:        protocol.PhaseAwaitingReview,
			Tests:        []protocol.TestRun{{Cmd: "go test ./...", Result: protocol.ResultPass}},
			FilesTouched: []string{"cmd/root.go"},
			DiffStat:     protocol.DiffStat{Insertions: 3},
		},
	}
	v := gateVerdict(st, nil)
	if v.AutoApprove {
		t.Fatal("gate must not auto-approve a small, clean diff whose claimed test pass did not reproduce")
	}
	if !hasReasonContaining(v.HardReasons, "re-running it failed") {
		t.Errorf("expected an unwaivable test-mismatch reason, got %v", v.HardReasons)
	}
}

// TestGateTreatsUnverifiableTestClaimAsWaivable is the regression for a
// claimed pass that could not even be re-run (a Cmd that doesn't parse as
// shell syntax): it must still escalate for review — the claim is
// unconfirmed — but as a waivable reason, not a HardReason, since a parse
// failure is not evidence the claim is false. A reviewer's "approve" must be
// enough to ship, unlike TestGateEscalatesWhenClaimedTestPassDoesNotReproduce
// above where the failure genuinely reproduced.
func TestGateTreatsUnverifiableTestClaimAsWaivable(t *testing.T) {
	st := &workerState{
		hasFile:          true,
		measuredOK:       true,
		measured:         protocol.DiffStat{Files: 1, Insertions: 3},
		measuredFiles:    []string{"cmd/root.go"},
		testUnverifiable: []string{`could not verify claimed pass of "git commit (lefthook pre-commit)": re-run could not be parsed as shell syntax`},
		plan:             &WorkerPlan{Worker: Worker{Task: "descriptive-cmd"}},
		status: protocol.Status{
			Phase:        protocol.PhaseAwaitingReview,
			Tests:        []protocol.TestRun{{Cmd: "git commit (lefthook pre-commit)", Result: protocol.ResultPass}},
			FilesTouched: []string{"cmd/root.go"},
			DiffStat:     protocol.DiffStat{Insertions: 3},
		},
	}
	v := gateVerdict(st, nil)
	if v.AutoApprove {
		t.Fatal("gate must not auto-approve while a test claim is unverified")
	}
	if !hasReasonContaining(v.Reasons, "could not be parsed") {
		t.Errorf("expected a reviewable reason for the unverifiable claim, got %v", v.Reasons)
	}
	if hasReasonContaining(v.HardReasons, "could not be parsed") {
		t.Errorf("an unverifiable (not reproduced-failing) claim must not be a HardReason, got %v", v.HardReasons)
	}
}

// TestReconcileWiresVerifyTestsIntoTestMismatches is the end-to-end companion
// to TestGateEscalatesWhenClaimedTestPassDoesNotReproduce: that test sets
// st.testMismatches by hand to prove gateVerdict reacts to it, but never
// proves reconcile itself actually populates the field from a real re-run —
// a wrong field name or an inverted phase check there would slip past it.
// This drives reconcile() directly against a real git worktree instead.
func TestReconcileWiresVerifyTestsIntoTestMismatches(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	states := []*workerState{{
		hasFile: true,
		plan:    &WorkerPlan{Worker: Worker{Task: "fabricated-pass", Worktree: wt}},
		status: protocol.Status{
			Phase: protocol.PhaseAwaitingReview,
			Tests: []protocol.TestRun{{Cmd: "exit 1", Result: protocol.ResultPass}},
		},
	}}
	reconcile(context.Background(), &Config{Base: "HEAD"}, states)

	if len(states[0].testMismatches) != 1 {
		t.Fatalf("reconcile should have wired VerifyTests' mismatch into st.testMismatches, got %v", states[0].testMismatches)
	}
}

// TestReconcileSkipsTestVerificationForNonTerminalPhase is the control for
// TestReconcileWiresVerifyTestsIntoTestMismatches: a worker still mid-flight
// has nothing final to re-verify yet, mirroring how the plan-evidence and
// diff-under-report checks stay silent until a terminal phase.
func TestReconcileSkipsTestVerificationForNonTerminalPhase(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	states := []*workerState{{
		hasFile: true,
		plan:    &WorkerPlan{Worker: Worker{Task: "still-working", Worktree: wt}},
		status: protocol.Status{
			Phase: protocol.PhaseWorking,
			Tests: []protocol.TestRun{{Cmd: "exit 1", Result: protocol.ResultPass}},
		},
	}}
	reconcile(context.Background(), &Config{Base: "HEAD"}, states)

	if states[0].testMismatches != nil {
		t.Fatalf("test verification must not run for a non-terminal phase, got %v", states[0].testMismatches)
	}
}

// TestReconcilePassesRealWorktreeToVerifyTests proves reconcile threads
// st.plan.Worktree (not argus's own cwd, not some other worker's worktree)
// through to VerifyTests: the command only succeeds when run inside wt.
func TestReconcilePassesRealWorktreeToVerifyTests(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	if err := os.WriteFile(filepath.Join(wt, "marker.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	states := []*workerState{{
		hasFile: true,
		plan:    &WorkerPlan{Worker: Worker{Task: "genuine-pass", Worktree: wt}},
		status: protocol.Status{
			Phase: protocol.PhaseAwaitingReview,
			Tests: []protocol.TestRun{{Cmd: "test -f marker.txt", Result: protocol.ResultPass}},
		},
	}}
	reconcile(context.Background(), &Config{Base: "HEAD"}, states)

	if len(states[0].testMismatches) != 0 {
		t.Fatalf("marker-file check should have succeeded inside the real worktree, got mismatches %v", states[0].testMismatches)
	}
}

// TestReconcileCapturesReviewDiffBeforeTestSideEffects is the regression for
// the ordering hazard: VerifyTests re-runs an arbitrary worker-supplied
// command, which can itself write to the worktree. If reviewOne's diff were
// fetched after that ran, the LLM reviewer could see the test run's own
// output instead of only the worker's actual change. reconcile must snapshot
// it first.
func TestReconcileCapturesReviewDiffBeforeTestSideEffects(t *testing.T) {
	wt := gitWorktreeWithDiff(t) // f.go already has an uncommitted tracked edit
	states := []*workerState{{
		hasFile: true,
		plan:    &WorkerPlan{Worker: Worker{Task: "side-effecting-test", Worktree: wt}},
		status: protocol.Status{
			Phase: protocol.PhaseAwaitingReview,
			Tests: []protocol.TestRun{{Cmd: "printf '\\nSIDEEFFECT\\n' >> f.go", Result: protocol.ResultPass}},
		},
	}}
	reconcile(context.Background(), &Config{Base: "HEAD"}, states)

	if states[0].reviewDiffErr != nil {
		t.Fatalf("reviewDiffErr: %v", states[0].reviewDiffErr)
	}
	if strings.Contains(states[0].reviewDiff, "SIDEEFFECT") {
		t.Errorf("reviewDiff must be captured before VerifyTests runs; it must not contain a test command's own side effect, got %q", states[0].reviewDiff)
	}
	onDisk, err := os.ReadFile(filepath.Join(wt, "f.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onDisk), "SIDEEFFECT") {
		t.Fatal("expected the test command to actually have run and mutated f.go — otherwise this test proves nothing")
	}
}

func TestRunGateVerifyCommandSkipsWhenUnconfigured(t *testing.T) {
	wt := t.TempDir()
	if m := RunGateVerifyCommand(context.Background(), wt, ""); m != "" {
		t.Errorf("mismatch = %q, want none — an empty verify command must not run anything", m)
	}
}

func TestRunGateVerifyCommandAcceptsCleanRun(t *testing.T) {
	wt := t.TempDir()
	if m := RunGateVerifyCommand(context.Background(), wt, "exit 0"); m != "" {
		t.Errorf("mismatch = %q, want none for a genuinely clean command", m)
	}
}

func TestRunGateVerifyCommandFlagsRepeatedFailure(t *testing.T) {
	wt := t.TempDir()
	m := RunGateVerifyCommand(context.Background(), wt, "echo lint-boom; exit 1")
	if m == "" {
		t.Fatal("mismatch = \"\", want a reason for a command that fails on both attempts")
	}
	if !strings.Contains(m, "lint-boom") {
		t.Errorf("mismatch should carry the failing command's captured output, got %q", m)
	}
}

func TestRunGateVerifyCommandRetriesOnceBeforeFlagging(t *testing.T) {
	wt := t.TempDir()
	marker := filepath.Join(wt, "verify-attempted")
	cmd := "test -f " + marker + " && exit 0 || { touch " + marker + "; exit 1; }"
	if m := RunGateVerifyCommand(context.Background(), wt, cmd); m != "" {
		t.Fatalf("mismatch = %q, want none — a fail-then-pass pair is a flake, not a real mismatch", m)
	}
}

// TestRunGateVerifyCommandReplaysUnquotedRunAlternationArgvStyle is the
// verify_command counterpart of
// TestVerifyTestsReplaysUnquotedRunAlternationArgvStyle: the same unquoted
// `-run` alternation reported as a repo's verify_command previously produced
// an unwaivable exit-127 hard-gate failure (see gateVerdict/verifyMismatch),
// even though the underlying tests genuinely pass.
func TestRunGateVerifyCommandReplaysUnquotedRunAlternationArgvStyle(t *testing.T) {
	dir := writeGoTestFixture(t)
	if m := RunGateVerifyCommand(context.Background(), dir, "go test -run TestFoo|TestBar ./..."); m != "" {
		t.Fatalf("mismatch = %q, want none — an unquoted -run alternation must replay as one go test invocation, not a shell pipeline", m)
	}
}

func TestGateEscalatesWhenGateVerifyCommandFails(t *testing.T) {
	st := &workerState{
		hasFile:        true,
		measuredOK:     true,
		measured:       protocol.DiffStat{Files: 1, Insertions: 3},
		measuredFiles:  []string{"cmd/root.go"},
		verifyMismatch: `repo's verify command "make lint" failed twice in a row: exit status 1`,
		plan:           &WorkerPlan{Worker: Worker{Task: "lint-failure"}},
		status: protocol.Status{
			Phase:        protocol.PhaseAwaitingReview,
			Tests:        []protocol.TestRun{{Cmd: "go test ./...", Result: protocol.ResultPass}},
			FilesTouched: []string{"cmd/root.go"},
			DiffStat:     protocol.DiffStat{Insertions: 3},
		},
	}
	v := gateVerdict(st, nil)
	if v.AutoApprove {
		t.Fatal("gate must not auto-approve a small, clean diff whose repo verify command failed")
	}
	if !hasReasonContaining(v.HardReasons, "make lint") {
		t.Errorf("expected an unwaivable verify-command reason, got %v", v.HardReasons)
	}
}

func TestReconcileWiresGateVerifyCommandIntoVerifyMismatch(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	states := []*workerState{{
		hasFile: true,
		plan:    &WorkerPlan{Worker: Worker{Task: "lint-failure", Worktree: wt}},
		status:  protocol.Status{Phase: protocol.PhaseAwaitingReview},
	}}
	reconcile(context.Background(), &Config{Base: "HEAD", GateVerifyCommand: "exit 1"}, states)

	if states[0].verifyMismatch == "" {
		t.Fatal("reconcile should have wired RunGateVerifyCommand's failure into st.verifyMismatch")
	}
}

func TestReconcileSkipsGateVerifyCommandWhenUnconfigured(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	states := []*workerState{{
		hasFile: true,
		plan:    &WorkerPlan{Worker: Worker{Task: "no-verify-cmd", Worktree: wt}},
		status:  protocol.Status{Phase: protocol.PhaseAwaitingReview},
	}}
	reconcile(context.Background(), &Config{Base: "HEAD"}, states)

	if states[0].verifyMismatch != "" {
		t.Fatalf("verifyMismatch = %q, want none — no verify command was configured", states[0].verifyMismatch)
	}
}

func TestReconcileSkipsGateVerifyCommandForNonTerminalPhase(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	states := []*workerState{{
		hasFile: true,
		plan:    &WorkerPlan{Worker: Worker{Task: "still-working", Worktree: wt}},
		status:  protocol.Status{Phase: protocol.PhaseWorking},
	}}
	reconcile(context.Background(), &Config{Base: "HEAD", GateVerifyCommand: "exit 1"}, states)

	if states[0].verifyMismatch != "" {
		t.Fatalf("verify command must not run for a non-terminal phase, got %q", states[0].verifyMismatch)
	}
}

func TestGateAutoApprovesWhenTestMismatchesEmpty(t *testing.T) {
	st := &workerState{
		hasFile:       true,
		measuredOK:    true,
		measured:      protocol.DiffStat{Files: 1, Insertions: 3},
		measuredFiles: []string{"cmd/root.go"},
		plan:          &WorkerPlan{Worker: Worker{Task: "genuine-pass"}},
		status: protocol.Status{
			Phase:        protocol.PhaseAwaitingReview,
			Tests:        []protocol.TestRun{{Cmd: "go test ./...", Result: protocol.ResultPass}},
			FilesTouched: []string{"cmd/root.go"},
			DiffStat:     protocol.DiffStat{Insertions: 3},
		},
	}
	if v := gateVerdict(st, nil); !v.AutoApprove {
		t.Errorf("gate must still auto-approve a clean worker once its test claim reproduces (testMismatches empty): reasons %v", v.Reasons)
	}
}
