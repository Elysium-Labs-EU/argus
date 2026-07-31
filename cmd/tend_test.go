package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/forge"
	"github.com/Elysium-Labs-EU/argus/internal/ownership"
)

func TestRunTendRequiresWorktree(t *testing.T) {
	cmd := &cobra.Command{}
	if err := runTend(cmd, &tendOpts{}); err == nil {
		t.Error("want an error when --worktree is empty")
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
