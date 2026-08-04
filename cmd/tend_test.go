package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/forge"
	"github.com/Elysium-Labs-EU/argus/internal/ownership"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

func TestRunTendRequiresWorktree(t *testing.T) {
	cmd := &cobra.Command{}
	err := runTend(cmd, &tendOpts{})
	uerr, ok := errors.AsType[*ui.UserError](err)
	if !ok {
		t.Fatalf("runTend with no --worktree = %v, want a *ui.UserError", err)
	}
	if !strings.Contains(uerr.Hint, "argus tend --worktree") {
		t.Errorf("UserError hint = %q, want it to point at --worktree", uerr.Hint)
	}
}

// TestRunTendSuccessThreadsAbsoluteWorktree pins runTend's reassignment of
// opts.worktree to supervisor.ResolveWorktree's result: every downstream call
// (starting with currentBranch, the first one runTend's own chain makes) must
// see an absolute path even when --worktree was given relative to argus's own
// cwd, mirroring TestShipUsesAbsoluteWorktree (cmd/ship_test.go) via the same
// injectable currentBranch var.
func TestRunTendSuccessThreadsAbsoluteWorktree(t *testing.T) {
	base := t.TempDir()
	repoDir := filepath.Join(base, "featx")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", repoDir, err)
	}
	child := filepath.Join(base, "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", child, err)
	}
	t.Chdir(child)

	var captured string
	original := currentBranch
	currentBranch = func(_ context.Context, worktree string) (string, error) {
		captured = worktree
		return "", errors.New("stop here")
	}
	t.Cleanup(func() { currentBranch = original })

	cmd, _ := newTendTestCmd(t)
	err := runTend(cmd, tendTargetOpts(filepath.Join("..", "featx")))
	if err == nil {
		t.Fatal("want the stub currentBranch error to surface")
	}
	if !filepath.IsAbs(captured) {
		t.Errorf("currentBranch received worktree %q, want an absolute path", captured)
	}
	wantAbs, aerr := filepath.Abs(repoDir)
	if aerr != nil {
		t.Fatalf("filepath.Abs(%q): %v", repoDir, aerr)
	}
	if captured != wantAbs {
		t.Errorf("currentBranch received worktree %q, want %q", captured, wantAbs)
	}
}

// TestNewTendCmdDispatchesToRunTend exercises newTendCmd's RunE wiring end to
// end through cobra (not just runTend called directly): resolveCredentialOverrides
// must succeed and its result must reach runTend for the UserError from an
// empty --worktree to ever surface.
func TestNewTendCmdDispatchesToRunTend(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cmd := newTendCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if _, ok := errors.AsType[*ui.UserError](err); !ok {
		t.Fatalf("newTendCmd with no --worktree = %v, want it to reach runTend's *ui.UserError", err)
	}
}

// TestNewTendCmdResolveCredentialOverridesErrors covers RunE's other branch:
// a resolveCredentialOverrides failure must short-circuit before runTend is
// ever called. Pointing ARGUS_CONFIG_FILE at a directory makes config.Load's
// os.ReadFile fail with something other than "not exist".
func TestNewTendCmdResolveCredentialOverridesErrors(t *testing.T) {
	t.Setenv("ARGUS_CONFIG_FILE", t.TempDir())
	cmd := newTendCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--worktree", t.TempDir()})
	if err := cmd.Execute(); err == nil {
		t.Fatal("want an error when the argus config file path is a directory")
	}
}

// tendTargetOpts builds a tendOpts with the ownership-lease defaults
// resolveOwnerStaleAfter expects a real `argus tend` invocation to already
// carry (see newTendCmd's --owner-stale-after flag default) — a test
// constructing tendOpts by hand bypasses cobra's flag defaults entirely, so
// this fills in the same default explicitly rather than leaving it at
// time.Duration's zero value, which Stale() would read as "always stale."
func tendTargetOpts(worktree string) *tendOpts {
	return &tendOpts{
		worktree: worktree,
		owner:    ownerFlags{ownerStaleAfter: ownership.DefaultStaleAfter},
	}
}

func TestResolveTendTargetHappyPath(t *testing.T) {
	wt := gitRepo(t,
		[]string{"checkout", "-q", "-b", "feat-x"},
		[]string{"remote", "add", "origin", "git@github.com:acme/widget.git"},
	)
	cmd, _ := newTendTestCmd(t)
	client, branch, owner, repo, err := resolveTendTarget(cmd, tendTargetOpts(wt))
	if err != nil {
		t.Fatalf("resolveTendTarget: %v", err)
	}
	if branch != "feat-x" || owner != "acme" || repo != "widget" {
		t.Errorf("branch/owner/repo = %q/%q/%q, want feat-x/acme/widget", branch, owner, repo)
	}
	if client.Host() != "github.com" {
		t.Errorf("client.Host() = %q, want github.com", client.Host())
	}
}

func TestResolveTendTargetNoOriginRemoteErrors(t *testing.T) {
	wt := gitRepo(t, []string{"checkout", "-q", "-b", "feat-x"}) // no `remote add origin`
	cmd, _ := newTendTestCmd(t)
	if _, _, _, _, err := resolveTendTarget(cmd, tendTargetOpts(wt)); err == nil {
		t.Error("want an error when the worktree has no origin remote")
	}
}

func TestResolveTendTargetBadForgeKindErrors(t *testing.T) {
	wt := gitRepo(t,
		[]string{"checkout", "-q", "-b", "feat-x"},
		[]string{"remote", "add", "origin", "git@github.com:acme/widget.git"},
	)
	cmd, _ := newTendTestCmd(t)
	opts := tendTargetOpts(wt)
	opts.forgeKind = "bogus"
	opts.forgeKindExplicit = true
	if _, _, _, _, err := resolveTendTarget(cmd, opts); err == nil {
		t.Error("want an error for an unrecognized --forge value")
	}
}

func TestResolveTendTargetUnsupportedHostErrors(t *testing.T) {
	wt := gitRepo(t,
		[]string{"checkout", "-q", "-b", "feat-x"},
		[]string{"remote", "add", "origin", "https://git.company.internal/acme/widget.git"},
	)
	cmd, _ := newTendTestCmd(t)
	if _, _, _, _, err := resolveTendTarget(cmd, tendTargetOpts(wt)); err == nil {
		t.Error("want an error for a self-hosted host outside the auto-detected allowlist with no --forge override")
	}
}

// TestResolveTendTargetCurrentBranchErrors covers the CurrentBranch failure
// branch: a worktree that isn't a git repository at all (as opposed to
// gitRepo's fixtures, which always are) fails rev-parse before resolveRepo
// ever runs.
func TestResolveTendTargetCurrentBranchErrors(t *testing.T) {
	wt := t.TempDir() // no `git init`
	cmd, _ := newTendTestCmd(t)
	if _, _, _, _, err := resolveTendTarget(cmd, tendTargetOpts(wt)); err == nil {
		t.Error("want an error when the worktree is not a git repository")
	}
}

func TestResolveTendTargetOwnershipConflictErrors(t *testing.T) {
	wt := gitRepo(t,
		[]string{"checkout", "-q", "-b", "feat-x"},
		[]string{"remote", "add", "origin", "git@github.com:acme/widget.git"},
	)
	if err := ownership.Spawn(wt, "sess-1", "host-a (pid 1)", time.Now()); err != nil {
		t.Fatalf("ownership.Spawn: %v", err)
	}
	cmd, _ := newTendTestCmd(t)
	opts := tendTargetOpts(wt)
	opts.owner.owner = "sess-2"
	if _, _, _, _, err := resolveTendTarget(cmd, opts); err == nil {
		t.Error("want an error when a different, still-fresh owner lease already holds this worktree")
	}
}

// newTendTestCmd builds a bare cobra.Command wired the way cmd.Execute()
// would for tendChange's tests: a context (tendChange reads cmd.Context())
// and an output buffer to assert against.
func newTendTestCmd(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	return cmd, &buf
}

func TestTendChangeNoPRFoundErrors(t *testing.T) {
	cmd, _ := newTendTestCmd(t)
	f := &fakeForge{findPRFound: false}
	if err := tendChange(cmd, f, &tendOpts{worktree: t.TempDir()}, "feat-x", "o", "r"); err == nil {
		t.Fatal("want an error when no PR exists for the branch")
	}
}

func TestTendChangeFindPRErrors(t *testing.T) {
	cmd, _ := newTendTestCmd(t)
	f := &fakeForge{findPRErr: errors.New("network unreachable")}
	err := tendChange(cmd, f, &tendOpts{worktree: t.TempDir()}, "feat-x", "o", "r")
	if err == nil {
		t.Fatal("want an error when FindPR itself fails")
	}
	if !strings.Contains(err.Error(), "looking up PR for branch feat-x") || !strings.Contains(err.Error(), "network unreachable") {
		t.Errorf("error should wrap FindPR's failure with the branch it was looking up, got: %v", err)
	}
}

func TestTendChangeDryRunPrintsPlanWithoutPolling(t *testing.T) {
	cmd, buf := newTendTestCmd(t)
	f := &fakeForge{
		findPRFound: true,
		findPR:      forge.PR{Number: 12, HTMLURL: "https://github.com/o/r/pull/12", State: "open"},
	}
	err := tendChange(cmd, f, &tendOpts{worktree: t.TempDir(), dryRun: true, interval: 30 * time.Second}, "feat-x", "o", "r")
	if err != nil {
		t.Fatalf("tendChange: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "dry run") || !strings.Contains(out, "https://github.com/o/r/pull/12") || !strings.Contains(out, "feat-x") {
		t.Errorf("dry-run should print the resolved PR and plan:\n%s", out)
	}
	if f.prChecksCall != 0 {
		t.Errorf("dry-run must not poll checks, got %d PRChecks call(s)", f.prChecksCall)
	}
}

func TestTendChangeMergeReadyWhenAllChecksPass(t *testing.T) {
	cmd, buf := newTendTestCmd(t)
	f := &fakeForge{
		findPRFound: true,
		findPR:      forge.PR{Number: 12, HTMLURL: "https://github.com/o/r/pull/12"},
		prChecksByPR: map[int][][]forge.Check{
			12: {
				{{Name: "build", State: "in_progress"}},
				{{Name: "build", State: "completed", Conclusion: "success"}},
			},
		},
	}
	err := tendChange(cmd, f, &tendOpts{worktree: t.TempDir(), interval: time.Millisecond}, "feat-x", "o", "r")
	if err != nil {
		t.Fatalf("tendChange: %v", err)
	}
	if !strings.Contains(buf.String(), "merge-ready") {
		t.Errorf("want a merge-ready report, got:\n%s", buf.String())
	}
}

func TestTendChangeReportsFailingCheckByName(t *testing.T) {
	cmd, buf := newTendTestCmd(t)
	f := &fakeForge{
		findPRFound: true,
		findPR:      forge.PR{Number: 12, HTMLURL: "https://github.com/o/r/pull/12"},
		prChecksByPR: map[int][][]forge.Check{
			12: {{
				{Name: "build", State: "completed", Conclusion: "success"},
				{Name: "test", State: "completed", Conclusion: "failure"},
			}},
		},
	}
	err := tendChange(cmd, f, &tendOpts{worktree: t.TempDir(), interval: time.Millisecond}, "feat-x", "o", "r")
	if err != nil {
		t.Fatalf("tendChange: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "failed") || !strings.Contains(out, `"test"`) {
		t.Errorf("want a failed report naming \"test\", got:\n%s", out)
	}
}

func TestTendChangeHeartbeatsOwnershipLeaseEachTick(t *testing.T) {
	wt := t.TempDir()
	start := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	if err := ownership.Spawn(wt, "sess-1", "host-a (pid 1)", start); err != nil {
		t.Fatalf("ownership.Spawn: %v", err)
	}

	cmd, _ := newTendTestCmd(t)
	f := &fakeForge{
		findPRFound: true,
		findPR:      forge.PR{Number: 12, HTMLURL: "https://github.com/o/r/pull/12"},
		prChecksByPR: map[int][][]forge.Check{
			12: {
				{{Name: "build", State: "in_progress"}},
				{{Name: "build", State: "in_progress"}},
				{{Name: "build", State: "completed", Conclusion: "success"}},
			},
		},
	}
	if err := tendChange(cmd, f, &tendOpts{worktree: wt, interval: time.Millisecond}, "feat-x", "o", "r"); err != nil {
		t.Fatalf("tendChange: %v", err)
	}

	got, found, err := ownership.Load(wt)
	if err != nil || !found {
		t.Fatalf("ownership.Load: found=%v err=%v", found, err)
	}
	if !got.HeartbeatAt.After(start) {
		t.Errorf("HeartbeatAt = %v, want it advanced past %v after tend polled", got.HeartbeatAt, start)
	}
}

func TestTendChangeContextCanceledReturnsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetContext(ctx)

	f := &fakeForge{
		findPRFound: true,
		findPR:      forge.PR{Number: 12, HTMLURL: "https://github.com/o/r/pull/12"},
	}
	err := tendChange(cmd, f, &tendOpts{worktree: t.TempDir(), interval: time.Hour}, "feat-x", "o", "r")
	if err == nil {
		t.Fatal("want an error when the context is already canceled")
	}
}

func TestTendChangeTimeoutElapsesReturnsError(t *testing.T) {
	cmd, _ := newTendTestCmd(t)
	f := &fakeForge{
		findPRFound: true,
		findPR:      forge.PR{Number: 12, HTMLURL: "https://github.com/o/r/pull/12"},
		prChecksByPR: map[int][][]forge.Check{
			12: {{{Name: "build", State: "in_progress"}}}, // never reaches terminal
		},
	}
	err := tendChange(cmd, f, &tendOpts{
		worktree: t.TempDir(), interval: time.Millisecond, timeout: 5 * time.Millisecond,
	}, "feat-x", "o", "r")
	if err == nil {
		t.Fatal("want an error once --timeout elapses without a terminal state")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("want a timeout-shaped error, got: %v", err)
	}
}

func TestEvaluateChecksNoChecksYetKeepsWaiting(t *testing.T) {
	if _, done := evaluateChecks(forge.PR{}, nil); done {
		t.Error("want done=false when the host has reported no checks yet")
	}
}

func TestEvaluateChecksNeutralAndSkippedCountAsPassing(t *testing.T) {
	checks := []forge.Check{
		{Name: "a", State: "completed", Conclusion: "success"},
		{Name: "b", State: "completed", Conclusion: "neutral"},
		{Name: "c", State: "completed", Conclusion: "skipped"},
	}
	outcome, done := evaluateChecks(forge.PR{}, checks)
	if !done || !outcome.MergeReady {
		t.Errorf("want merge-ready when only success/neutral/skipped checks are present, got done=%v outcome=%+v", done, outcome)
	}
}

func TestEvaluateChecksNonTerminalCheckKeepsWaiting(t *testing.T) {
	checks := []forge.Check{
		{Name: "a", State: "completed", Conclusion: "success"},
		{Name: "b", State: "in_progress"},
	}
	if _, done := evaluateChecks(forge.PR{}, checks); done {
		t.Error("want done=false while any check is still in flight, even with others already terminal")
	}
}

// TestEvaluateChecksNamesFirstFailingCheck pins FailedCheck to report order,
// not conclusion severity or alphabetical order: the second, later-declared
// failure ("c") must never shadow the first ("a").
func TestEvaluateChecksNamesFirstFailingCheck(t *testing.T) {
	checks := []forge.Check{
		{Name: "a", State: "completed", Conclusion: "failure"},
		{Name: "b", State: "completed", Conclusion: "success"},
		{Name: "c", State: "completed", Conclusion: "failure"},
	}
	outcome, done := evaluateChecks(forge.PR{}, checks)
	if !done || outcome.MergeReady {
		t.Fatalf("want a failed, done outcome, got done=%v outcome=%+v", done, outcome)
	}
	if outcome.FailedCheck != "a" {
		t.Errorf("FailedCheck = %q, want the first failing check (\"a\")", outcome.FailedCheck)
	}
}

// TestEvaluateChecksTerminalFailureConclusionsCountAsFailing covers non-success
// terminal conclusions GitHub reports beyond plain "failure" — all fall
// through Failed's default case.
func TestEvaluateChecksTerminalFailureConclusionsCountAsFailing(t *testing.T) {
	conclusions := []string{"cancelled", "timed_out", "action_required"} //nolint:misspell // "cancelled" (double L) is GitHub's own Checks API conclusion spelling, not a typo
	for _, conclusion := range conclusions {
		t.Run(conclusion, func(t *testing.T) {
			checks := []forge.Check{{Name: "build", State: "completed", Conclusion: conclusion}}
			outcome, done := evaluateChecks(forge.PR{}, checks)
			if !done || outcome.MergeReady || outcome.FailedCheck != "build" {
				t.Errorf("conclusion %q: want a failed outcome naming \"build\", got done=%v outcome=%+v", conclusion, done, outcome)
			}
		})
	}
}

func TestPollChecksPRChecksErrorWraps(t *testing.T) {
	f := &fakeForge{prChecksErr: errors.New("host unreachable")}
	_, err := pollChecks(context.Background(), f, nil, t.TempDir(), "o", "r", forge.PR{Number: 12}, time.Millisecond, 0)
	if err == nil {
		t.Fatal("want an error when PRChecks itself fails")
	}
	if !strings.Contains(err.Error(), "polling checks for PR #12") || !strings.Contains(err.Error(), "host unreachable") {
		t.Errorf("error should wrap PRChecks's failure, got: %v", err)
	}
}

// TestPollChecksHeartbeatErrorLoggedButContinues pins pollChecks's
// best-effort heartbeat contract: a Heartbeat write failure on one tick must
// be logged, not abort the poll loop, so a transient filesystem hiccup
// doesn't turn into a spurious tend failure while checks are still running.
func TestPollChecksHeartbeatErrorLoggedButContinues(t *testing.T) {
	wt := t.TempDir()
	if err := ownership.Spawn(wt, "sess-1", "host-a (pid 1)", time.Now()); err != nil {
		t.Fatalf("ownership.Spawn: %v", err)
	}
	leaseDir := filepath.Join(wt, ".claude", "argus")
	if err := os.Chmod(leaseDir, 0o500); err != nil {
		t.Fatalf("chmod %s: %v", leaseDir, err)
	}
	t.Cleanup(func() { _ = os.Chmod(leaseDir, 0o700) }) // let t.TempDir's own cleanup remove it

	var logBuf bytes.Buffer
	logger := eventlog.New(&logBuf, "tend", "test-run", nil)
	f := &fakeForge{prChecksByPR: map[int][][]forge.Check{
		12: {{{Name: "build", State: "completed", Conclusion: "success"}}},
	}}

	outcome, err := pollChecks(context.Background(), f, logger, wt, "o", "r", forge.PR{Number: 12}, time.Millisecond, 0)
	if err != nil {
		t.Fatalf("pollChecks: %v", err)
	}
	if !outcome.MergeReady {
		t.Errorf("want a merge-ready outcome despite the heartbeat failure, got %+v", outcome)
	}
	if !strings.Contains(logBuf.String(), `"action":"owner_heartbeat"`) || !strings.Contains(logBuf.String(), `"outcome":"error"`) {
		t.Errorf("want the heartbeat failure logged as an error event, got log:\n%s", logBuf.String())
	}
}
