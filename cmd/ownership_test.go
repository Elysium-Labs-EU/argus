package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/ownership"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

// seedOwnerLeaseOwnerID is the owner_id every seedOwnerLease call below
// records; every test either matches it (owner "sess-1") or deliberately
// mismatches it (owner "sess-2") to exercise the refusal path.
const seedOwnerLeaseOwnerID = "sess-1"

// seedOwnerLease writes an owner.json directly (bypassing ownership.Spawn's
// "now == spawnedAt" convenience) so a test can pin an exact heartbeat,
// including one deliberately stale.
func seedOwnerLease(t *testing.T, worktree string, heartbeatAt time.Time) {
	t.Helper()
	if err := ownership.Write(worktree, &ownership.Owner{
		OwnerID: seedOwnerLeaseOwnerID, OwnerLabel: "test-owner", SpawnedAt: heartbeatAt, HeartbeatAt: heartbeatAt,
	}); err != nil {
		t.Fatalf("seeding owner lease: %v", err)
	}
}

// failingHerdrClient fails the test outright if any of its methods are
// called — used across the enforcement-point tests below to prove a refused
// (or not-yet-reached) command never touches herdr.
func failingHerdrClient(t *testing.T) herdr.Client {
	t.Helper()
	return herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		t.Fatalf("unexpected herdr call: %v", args)
		return nil, nil
	})
}

// --- enforceOwnership itself ---

func TestEnforceOwnershipMissingLeaseSilent(t *testing.T) {
	wt := t.TempDir()
	var buf bytes.Buffer
	if err := enforceOwnership(context.Background(), &buf, wt, ownerFlags{owner: "anyone"}, time.Now()); err != nil {
		t.Fatalf("a worktree with no recorded lease should never refuse, got: %v", err)
	}
	if buf.String() != "" {
		t.Errorf("want no notice for a worktree with no recorded lease, got: %q", buf.String())
	}
}

func TestEnforceOwnershipSameOwnerSilent(t *testing.T) {
	wt := t.TempDir()
	now := time.Now()
	seedOwnerLease(t, wt, now)
	var buf bytes.Buffer
	if err := enforceOwnership(context.Background(), &buf, wt, ownerFlags{owner: "sess-1"}, now); err != nil {
		t.Fatalf("the owning caller should never refuse, got: %v", err)
	}
	if buf.String() != "" {
		t.Errorf("want no notice for the owning caller, got: %q", buf.String())
	}
}

func TestEnforceOwnershipMismatchRefuses(t *testing.T) {
	wt := t.TempDir()
	now := time.Now()
	seedOwnerLease(t, wt, now)
	var buf bytes.Buffer
	err := enforceOwnership(context.Background(), &buf, wt, ownerFlags{owner: "sess-2"}, now)
	if err == nil {
		t.Fatal("want a mismatched, still-fresh lease to refuse")
	}
	if _, ok := errors.AsType[*ui.UserError](err); !ok {
		t.Errorf("err = %v (%T), want a *ui.UserError", err, err)
	}
}

func TestEnforceOwnershipForceBypassesMismatch(t *testing.T) {
	wt := t.TempDir()
	now := time.Now()
	seedOwnerLease(t, wt, now)
	var buf bytes.Buffer
	if err := enforceOwnership(context.Background(), &buf, wt, ownerFlags{owner: "sess-2", forceForeignOwner: true}, now); err != nil {
		t.Fatalf("--force-foreign-owner should bypass a mismatch, got: %v", err)
	}
}

func TestEnforceOwnershipStaleMismatchPrintsNoticeAndProceeds(t *testing.T) {
	wt := t.TempDir()
	spawned := time.Now().Add(-time.Hour)
	seedOwnerLease(t, wt, spawned)
	var buf bytes.Buffer
	if err := enforceOwnership(context.Background(), &buf, wt, ownerFlags{owner: "sess-2", ownerStaleAfter: 30 * time.Minute}, time.Now()); err != nil {
		t.Fatalf("a stale mismatched lease should not refuse, got: %v", err)
	}
	if !strings.Contains(buf.String(), "gone quiet") {
		t.Errorf("want a stale-lease notice printed, got: %q", buf.String())
	}
}

// initGitRepoWithRawConfig makes a real git checkout with .argus/config.yml
// containing raw verbatim, so resolveOwnerStaleAfterForWorktree's
// supervisor.RepoRoot call has a real repo to resolve — mirrors
// attachWorktree's own local git helper closure.
func initGitRepoWithRawConfig(t *testing.T, raw string) string {
	t.Helper()
	wt := t.TempDir()
	git := func(args ...string) {
		if out, err := exec.Command("git", append([]string{"-C", wt}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "t@t")
	git("config", "user.name", "t")
	git("commit", "-q", "--allow-empty", "-m", "base")
	cfgPath := filepath.Join(wt, ".argus", "config.yml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatalf("mkdir .argus: %v", err)
	}
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatalf("writing config.yml: %v", err)
	}
	return wt
}

// initOwnerConfigRepo makes a real git checkout with .argus/config.yml
// containing owner_stale_after: ownerStaleAfterYAML.
func initOwnerConfigRepo(t *testing.T, ownerStaleAfterYAML string) string {
	t.Helper()
	return initGitRepoWithRawConfig(t, "owner_stale_after: \""+ownerStaleAfterYAML+"\"\n")
}

func TestEnforceOwnershipUsesRepoConfigOwnerStaleAfterWhenFlagNotExplicit(t *testing.T) {
	wt := initOwnerConfigRepo(t, "1m")
	// Stale by the repo's own 1m threshold, but well inside the flag's own
	// 30m default — proves the config value, not the flag default, decided
	// this.
	spawned := time.Now().Add(-5 * time.Minute)
	seedOwnerLease(t, wt, spawned)
	var buf bytes.Buffer
	err := enforceOwnership(context.Background(), &buf, wt, ownerFlags{owner: "sess-2", ownerStaleAfter: 30 * time.Minute}, time.Now())
	if err != nil {
		t.Fatalf("a lease stale by the repo config's 1m threshold should not refuse, got: %v", err)
	}
	if !strings.Contains(buf.String(), "gone quiet") {
		t.Errorf("want a stale-lease notice printed, got: %q", buf.String())
	}
}

func TestEnforceOwnershipExplicitFlagWinsOverRepoConfigOwnerStaleAfter(t *testing.T) {
	wt := initOwnerConfigRepo(t, "1m")
	// Only 5m stale: past the repo's 1m config threshold, but inside an
	// explicit --owner-stale-after 30m the operator passed on the command
	// line — the flag must win, so this should still refuse.
	spawned := time.Now().Add(-5 * time.Minute)
	seedOwnerLease(t, wt, spawned)
	var buf bytes.Buffer
	err := enforceOwnership(context.Background(), &buf, wt, ownerFlags{
		owner: "sess-2", ownerStaleAfter: 30 * time.Minute, ownerStaleAfterExplicit: true,
	}, time.Now())
	if err == nil {
		t.Fatal("want an explicit --owner-stale-after to win over the repo config's shorter threshold and still refuse")
	}
}

func TestResolveOwnerStaleAfterForWorktreeOutsideRepoFallsBackToFlag(t *testing.T) {
	wt := t.TempDir()
	got, err := resolveOwnerStaleAfterForWorktree(context.Background(), wt, false, 30*time.Minute)
	if err != nil {
		t.Fatalf("a worktree outside any git repo should fall back, not error: %v", err)
	}
	if got != 30*time.Minute {
		t.Errorf("resolveOwnerStaleAfterForWorktree = %v, want the flag's own default", got)
	}
}

// TestResolveOwnerStaleAfterForWorktreeMalformedConfigReturnsUserError covers
// ownership.go's other repoconfig.Load failure branch: unlike
// resolveOwnerStaleAfterForWorktree's own missing-repo-root fallback (silent,
// via nilerr), a config file that exists but fails to parse is a real,
// user-facing error — a colon-less line has no recognized "key: value" split.
func TestResolveOwnerStaleAfterForWorktreeMalformedConfigReturnsUserError(t *testing.T) {
	wt := initGitRepoWithRawConfig(t, "this is not valid yaml\n")
	_, err := resolveOwnerStaleAfterForWorktree(context.Background(), wt, false, 30*time.Minute)
	if err == nil {
		t.Fatal("want a malformed config.yml to error, got nil")
	}
	if _, ok := errors.AsType[*ui.UserError](err); !ok {
		t.Errorf("err = %v (%T), want a *ui.UserError", err, err)
	}
}

// TestEnforceOwnershipPropagatesResolveOwnerStaleAfterForWorktreeErr covers
// enforceOwnership's own early return (ownership.go L57-59): a malformed
// repo config must fail the whole call before ever reaching
// ownership.ResolveOwnerID/Enforce — proven here by an owner mismatch that
// would otherwise refuse for a different reason if config resolution didn't
// short-circuit first.
func TestEnforceOwnershipPropagatesResolveOwnerStaleAfterForWorktreeErr(t *testing.T) {
	wt := initGitRepoWithRawConfig(t, "this is not valid yaml\n")
	seedOwnerLease(t, wt, time.Now())
	var buf bytes.Buffer
	err := enforceOwnership(context.Background(), &buf, wt, ownerFlags{owner: "sess-2"}, time.Now())
	if err == nil {
		t.Fatal("want a malformed repo config to fail enforceOwnership, got nil")
	}
	if _, ok := errors.AsType[*ui.UserError](err); !ok {
		t.Errorf("err = %v (%T), want a *ui.UserError", err, err)
	}
	if buf.String() != "" {
		t.Errorf("want no notice written when config resolution fails before the lease check, got: %q", buf.String())
	}
}

// --- rework ---

func TestRunReworkOwnershipMismatchRefuses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	seedOwnerLease(t, dir, time.Now())
	cmd, _ := testCmd()

	err := runRework(cmd, failingHerdrClient(t), &fakeReviewer{}, reworkLogger(), &reworkOpts{
		worktree: dir, base: "feat-x", maxRounds: 3,
		owner: ownerFlags{owner: "sess-2", ownerStaleAfter: 30 * time.Minute},
	})
	if _, ok := errors.AsType[*ui.UserError](err); !ok {
		t.Fatalf("want a *ui.UserError for a mismatched owner, got %v", err)
	}
}

func TestRunReworkOwnershipSameOwnerProceeds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	seedOwnerLease(t, dir, time.Now())
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: true, Source: "review", Summary: "ok"}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	cmd, buf := testCmd()

	err := runRework(cmd, failingHerdrClient(t), &fakeReviewer{}, reworkLogger(), &reworkOpts{
		worktree: dir, base: "feat-x", maxRounds: 3, owner: ownerFlags{owner: "sess-1"},
	})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	if !strings.Contains(buf.String(), "nothing to rework") {
		t.Errorf("expected a nothing-to-rework message:\n%s", buf.String())
	}
}

func TestRunReworkOwnershipMismatchWithForceProceeds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	seedOwnerLease(t, dir, time.Now())
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: true, Source: "review", Summary: "ok"}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	cmd, buf := testCmd()

	err := runRework(cmd, failingHerdrClient(t), &fakeReviewer{}, reworkLogger(), &reworkOpts{
		worktree: dir, base: "feat-x", maxRounds: 3,
		owner: ownerFlags{owner: "sess-2", forceForeignOwner: true},
	})
	if err != nil {
		t.Fatalf("runRework with --force-foreign-owner: %v", err)
	}
	if !strings.Contains(buf.String(), "nothing to rework") {
		t.Errorf("expected a nothing-to-rework message:\n%s", buf.String())
	}
}

func TestRunReworkOwnershipStaleLeaseDoesNotRefuse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	seedOwnerLease(t, dir, time.Now().Add(-time.Hour))
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: true, Source: "review", Summary: "ok"}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	cmd, buf := testCmd()

	err := runRework(cmd, failingHerdrClient(t), &fakeReviewer{}, reworkLogger(), &reworkOpts{
		worktree: dir, base: "feat-x", maxRounds: 3,
		owner: ownerFlags{owner: "sess-2", ownerStaleAfter: 30 * time.Minute},
	})
	if err != nil {
		t.Fatalf("runRework against a stale lease: %v", err)
	}
	if !strings.Contains(buf.String(), "nothing to rework") {
		t.Errorf("expected a nothing-to-rework message:\n%s", buf.String())
	}
}

// --- rebase ---

func TestRunRebaseOwnershipMismatchRefuses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	seedOwnerLease(t, dir, time.Now())
	cmd, _ := testCmd()

	err := runRebase(cmd, failingHerdrClient(t), &rebaseOpts{
		worktree: dir, base: "main",
		owner: ownerFlags{owner: "sess-2", ownerStaleAfter: 30 * time.Minute},
	})
	if _, ok := errors.AsType[*ui.UserError](err); !ok {
		t.Fatalf("want a *ui.UserError for a mismatched owner, got %v", err)
	}
}

// rebaseSelfReferentialRepo builds a repo (via initGitDirAt) whose only
// branch and whose origin remote both point at itself, so rebasing that
// branch onto itself resolves to "no conflict, already up to date" —
// runRebase's own no-op path, reached without runRebase ever calling herdr.
func rebaseSelfReferentialRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	initGitDirAt(t, dir)
	return dir
}

func TestRunRebaseOwnershipSameOwnerProceeds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := rebaseSelfReferentialRepo(t)
	seedOwnerLease(t, dir, time.Now())
	cmd, buf := testCmd()

	err := runRebase(cmd, failingHerdrClient(t), &rebaseOpts{worktree: dir, base: "feat-x", owner: ownerFlags{owner: "sess-1"}})
	if err != nil {
		t.Fatalf("runRebase: %v", err)
	}
	if !strings.Contains(buf.String(), "nothing to rebase") {
		t.Errorf("expected a nothing-to-rebase message:\n%s", buf.String())
	}
}

func TestRunRebaseOwnershipMismatchWithForceProceeds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := rebaseSelfReferentialRepo(t)
	seedOwnerLease(t, dir, time.Now())
	cmd, buf := testCmd()

	err := runRebase(cmd, failingHerdrClient(t), &rebaseOpts{
		worktree: dir, base: "feat-x",
		owner: ownerFlags{owner: "sess-2", forceForeignOwner: true},
	})
	if err != nil {
		t.Fatalf("runRebase with --force-foreign-owner: %v", err)
	}
	if !strings.Contains(buf.String(), "nothing to rebase") {
		t.Errorf("expected a nothing-to-rebase message:\n%s", buf.String())
	}
}

func TestRunRebaseOwnershipStaleLeaseDoesNotRefuse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := rebaseSelfReferentialRepo(t)
	seedOwnerLease(t, dir, time.Now().Add(-time.Hour))
	cmd, buf := testCmd()

	err := runRebase(cmd, failingHerdrClient(t), &rebaseOpts{
		worktree: dir, base: "feat-x",
		owner: ownerFlags{owner: "sess-2", ownerStaleAfter: 30 * time.Minute},
	})
	if err != nil {
		t.Fatalf("runRebase against a stale lease: %v", err)
	}
	if !strings.Contains(buf.String(), "nothing to rebase") {
		t.Errorf("expected a nothing-to-rebase message:\n%s", buf.String())
	}
}

// --- ship ---

func shipTestArgs(wt string, of ownerFlags) *shipArgs {
	return &shipArgs{worktree: wt, base: "main", issue: 1, force: true, dryRun: true, owner: of}
}

func TestRunShipOwnershipMismatchRefuses(t *testing.T) {
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@codeberg.org:acme/widget.git"})
	seedOwnerLease(t, wt, time.Now())
	cmd := newShipCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	err := runShip(cmd, shipTestArgs(wt, ownerFlags{owner: "sess-2", ownerStaleAfter: 30 * time.Minute}))
	if _, ok := errors.AsType[*ui.UserError](err); !ok {
		t.Fatalf("want a *ui.UserError for a mismatched owner, got %v", err)
	}
}

func TestRunShipOwnershipSameOwnerProceeds(t *testing.T) {
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@codeberg.org:acme/widget.git"})
	seedOwnerLease(t, wt, time.Now())
	cmd := newShipCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	err := runShip(cmd, shipTestArgs(wt, ownerFlags{owner: "sess-1"}))
	if err != nil {
		t.Fatalf("runShip: %v", err)
	}
	if !strings.Contains(buf.String(), "ship plan (dry run)") {
		t.Errorf("expected a dry-run plan:\n%s", buf.String())
	}
}

func TestRunShipOwnershipMismatchWithForceProceeds(t *testing.T) {
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@codeberg.org:acme/widget.git"})
	seedOwnerLease(t, wt, time.Now())
	cmd := newShipCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	err := runShip(cmd, shipTestArgs(wt, ownerFlags{owner: "sess-2", forceForeignOwner: true}))
	if err != nil {
		t.Fatalf("runShip with --force-foreign-owner: %v", err)
	}
	if !strings.Contains(buf.String(), "ship plan (dry run)") {
		t.Errorf("expected a dry-run plan:\n%s", buf.String())
	}
}

func TestRunShipOwnershipStaleLeaseDoesNotRefuse(t *testing.T) {
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@codeberg.org:acme/widget.git"})
	seedOwnerLease(t, wt, time.Now().Add(-time.Hour))
	cmd := newShipCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	err := runShip(cmd, shipTestArgs(wt, ownerFlags{owner: "sess-2", ownerStaleAfter: 30 * time.Minute}))
	if err != nil {
		t.Fatalf("runShip against a stale lease: %v", err)
	}
	if !strings.Contains(buf.String(), "ship plan (dry run)") {
		t.Errorf("expected a dry-run plan:\n%s", buf.String())
	}
}

// --- worker answer ---

func TestRunWorkerAnswerOwnershipMismatchRefuses(t *testing.T) {
	wt := initGitDir(t)
	seedOwnerLease(t, wt, time.Now())
	client := fakeAnswerClient(true, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerAnswer(testCmdCtx, client, answerLogger(), wt, "go ahead", 0,
		ownerFlags{owner: "sess-2", ownerStaleAfter: 30 * time.Minute}, fixedNow(time.Now()))
	if _, ok := errors.AsType[*ui.UserError](err); !ok {
		t.Fatalf("want a *ui.UserError for a mismatched owner, got %v", err)
	}
}

func TestRunWorkerAnswerOwnershipSameOwnerProceeds(t *testing.T) {
	wt := initGitDir(t)
	seedOwnerLease(t, wt, time.Now())
	seedBlockedStatus(t, wt, nil)
	client := fakeAnswerClient(true, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerAnswer(testCmdCtx, client, answerLogger(), wt, "go ahead", 0, ownerFlags{owner: "sess-1"}, fixedNow(time.Now()))
	if err != nil {
		t.Fatalf("runWorkerAnswer: %v", err)
	}
}

func TestRunWorkerAnswerOwnershipMismatchWithForceProceeds(t *testing.T) {
	wt := initGitDir(t)
	seedOwnerLease(t, wt, time.Now())
	seedBlockedStatus(t, wt, nil)
	client := fakeAnswerClient(true, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerAnswer(testCmdCtx, client, answerLogger(), wt, "go ahead", 0,
		ownerFlags{owner: "sess-2", forceForeignOwner: true}, fixedNow(time.Now()))
	if err != nil {
		t.Fatalf("runWorkerAnswer with --force-foreign-owner: %v", err)
	}
}

func TestRunWorkerAnswerOwnershipStaleLeaseDoesNotRefuse(t *testing.T) {
	wt := initGitDir(t)
	seedOwnerLease(t, wt, time.Now().Add(-time.Hour))
	seedBlockedStatus(t, wt, nil)
	client := fakeAnswerClient(true, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerAnswer(testCmdCtx, client, answerLogger(), wt, "go ahead", 0,
		ownerFlags{owner: "sess-2", ownerStaleAfter: 30 * time.Minute}, fixedNow(time.Now()))
	if err != nil {
		t.Fatalf("runWorkerAnswer against a stale lease: %v", err)
	}
}

// --- worker steer ---

func TestRunWorkerSteerOwnershipMismatchRefuses(t *testing.T) {
	wt := initGitDir(t)
	seedOwnerLease(t, wt, time.Now())
	client := fakeSteerClient(true, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "note",
		ownerFlags{owner: "sess-2", ownerStaleAfter: 30 * time.Minute}, fixedNow(time.Now()))
	if _, ok := errors.AsType[*ui.UserError](err); !ok {
		t.Fatalf("want a *ui.UserError for a mismatched owner, got %v", err)
	}
}

func TestRunWorkerSteerOwnershipSameOwnerProceeds(t *testing.T) {
	wt := initGitDir(t)
	seedOwnerLease(t, wt, time.Now())
	seedWorkingStatus(t, wt, nil)
	client := fakeSteerClient(true, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "note", ownerFlags{owner: "sess-1"}, fixedNow(time.Now()))
	if err != nil {
		t.Fatalf("runWorkerSteer: %v", err)
	}
}

func TestRunWorkerSteerOwnershipMismatchWithForceProceeds(t *testing.T) {
	wt := initGitDir(t)
	seedOwnerLease(t, wt, time.Now())
	seedWorkingStatus(t, wt, nil)
	client := fakeSteerClient(true, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "note",
		ownerFlags{owner: "sess-2", forceForeignOwner: true}, fixedNow(time.Now()))
	if err != nil {
		t.Fatalf("runWorkerSteer with --force-foreign-owner: %v", err)
	}
}

func TestRunWorkerSteerOwnershipStaleLeaseDoesNotRefuse(t *testing.T) {
	wt := initGitDir(t)
	seedOwnerLease(t, wt, time.Now().Add(-time.Hour))
	seedWorkingStatus(t, wt, nil)
	client := fakeSteerClient(true, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "note",
		ownerFlags{owner: "sess-2", ownerStaleAfter: 30 * time.Minute}, fixedNow(time.Now()))
	if err != nil {
		t.Fatalf("runWorkerSteer against a stale lease: %v", err)
	}
}
