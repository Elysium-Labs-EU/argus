package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/forge"
	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

// repoWithWorktree builds a tiny real repo with one linked worktree checked
// out on branch, so prunePlan's git-status checks run against real plumbing.
func repoWithWorktree(t *testing.T, branch string) (repoRoot, worktree string) {
	t.Helper()
	repoRoot = gitRepo(t, []string{"remote", "add", "origin", "https://codeberg.org/o/r.git"})
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repoRoot}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("branch", branch)
	// Fake an upstream tracking ref pointing at the same commit, without a real
	// remote to push to, so hasUnpushedCommits sees a branch already "pushed"
	// (ship always sets a real one via `git push -u`; see supervisor.Push).
	run("update-ref", "refs/remotes/origin/"+branch, "HEAD")
	run("branch", "--set-upstream-to=origin/"+branch, branch)
	worktree = filepath.Join(t.TempDir(), branch)
	run("worktree", "add", "-q", worktree, branch)
	return repoRoot, worktree
}

// repoWithWorktreeIn adds a second linked worktree on branch to an
// already-established repoRoot (see repoWithWorktree), so a test can put
// more than one candidate worktree under the same repo. It returns only the
// new worktree's path — repoRoot is already the caller's own input, not
// something this needs to hand back.
func repoWithWorktreeIn(t *testing.T, repoRoot, branch string) string {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repoRoot}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("branch", branch)
	run("update-ref", "refs/remotes/origin/"+branch, "HEAD")
	run("branch", "--set-upstream-to=origin/"+branch, branch)
	worktree := filepath.Join(t.TempDir(), branch)
	run("worktree", "add", "-q", worktree, branch)
	return worktree
}

func TestRunWorktreePruneRequiresATarget(t *testing.T) {
	cmd := &cobra.Command{}
	if err := runWorktreePrune(cmd, &worktreePruneArgs{}); err == nil {
		t.Error("want an error when neither --branch nor --merged is given")
	}
}

func TestRunWorktreePruneRejectsBothBranchAndMerged(t *testing.T) {
	cmd := &cobra.Command{}
	if err := runWorktreePrune(cmd, &worktreePruneArgs{branch: "x", merged: true}); err == nil {
		t.Error("want an error when --branch and --merged are both given")
	}
}

// TestWorktreePruneCmdRegistersCredentialEnvFlag guards the flag wiring
// itself: --credential-env must reach forge.TokenForHost the same way it does
// for ship/rebase/supervise (see runWorktreePrune's forge.New call), so a host
// needing a custom credential-env override can authenticate. Driven through
// the real cobra command (not runWorktreePrune directly) so the RunE closure's
// resolveCredentialOverrides call is exercised too. The repo has zero linked
// worktrees, so the --merged sweep's loop body never runs and this makes no
// network call — it only proves the flag parses and the command reaches (and
// returns cleanly from) the forge-construction line.
func TestWorktreePruneCmdRegistersCredentialEnvFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // resolveCredentialOverrides reads ~/.argus/config.toml
	repoRoot := gitRepo(t, []string{"remote", "add", "origin", "git@codeberg.org:acme/widget.git"})

	cmd := newWorktreePruneCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{"--repo", repoRoot, "--merged", "--dry-run", "--credential-env", "codeberg.org=CUSTOM_CODEBERG_TOKEN"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("cmd.Execute: %v", err)
	}
	if !strings.Contains(buf.String(), "0 cleaned, 0 left in place") {
		t.Errorf("want an empty-sweep summary, got: %q", buf.String())
	}
}

// TestRunWorktreePruneRefusesAmbiguousSelfHostedHost pins issue #256's
// worktree-prune half: a self-hosted host argus can't shape-detect refuses
// without --forge or a repo config forge key, same as ship already does.
func TestRunWorktreePruneRefusesAmbiguousSelfHostedHost(t *testing.T) {
	t.Setenv("FORGE_TOKEN", "tok")
	repoRoot := gitRepo(t, []string{"remote", "add", "origin", "git@git.example.com:acme/widget.git"})

	cmd := newWorktreePruneCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{"--repo", repoRoot, "--merged", "--dry-run"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("want an error for a self-hosted host with no --forge/config default")
	}
}

// TestRunWorktreePruneForgeFlagUnblocksSelfHostedHost is the other half: an
// explicit --forge lets prune run against a self-hosted host.
func TestRunWorktreePruneForgeFlagUnblocksSelfHostedHost(t *testing.T) {
	t.Setenv("FORGE_TOKEN", "tok")
	repoRoot := gitRepo(t, []string{"remote", "add", "origin", "git@git.example.com:acme/widget.git"})

	cmd := newWorktreePruneCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{"--repo", repoRoot, "--merged", "--dry-run", "--forge", "gitea"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("cmd.Execute: %v", err)
	}
	if !strings.Contains(buf.String(), "0 cleaned, 0 left in place") {
		t.Errorf("want an empty-sweep summary, got: %q", buf.String())
	}
}

// TestRunWorktreePruneForgeConfigUnblocksSelfHostedHost mirrors the flag case
// but via this repo's .argus/config.yml forge key instead of --forge.
func TestRunWorktreePruneForgeConfigUnblocksSelfHostedHost(t *testing.T) {
	t.Setenv("FORGE_TOKEN", "tok")
	repoRoot := gitRepo(t, []string{"remote", "add", "origin", "git@git.example.com:acme/widget.git"})
	if err := repoconfig.Save(repoconfig.Path(repoRoot), &repoconfig.Config{Forge: "gitea"}); err != nil {
		t.Fatalf("seeding repo config: %v", err)
	}

	cmd := newWorktreePruneCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{"--repo", repoRoot, "--merged", "--dry-run"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("cmd.Execute: %v", err)
	}
	if !strings.Contains(buf.String(), "0 cleaned, 0 left in place") {
		t.Errorf("want an empty-sweep summary, got: %q", buf.String())
	}
}

func TestPrunePlanDryRunReportsSafeWithoutDeleting(t *testing.T) {
	repoRoot, worktree := repoWithWorktree(t, "feat-a")
	merged := time.Now()
	f := &fakeForge{findPRFound: true, findPR: forge.PR{HTMLURL: "https://fake/pr/1", MergedAt: &merged}}
	entries := []supervisor.WorktreeEntry{{Path: worktree, Branch: "feat-a"}}

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)

	prunePlan(cmd, context.Background(), nil, f, herdr.Client{}, "o", "r", repoRoot, entries, true)
	if _, err := os.Stat(worktree); err != nil {
		t.Errorf("dry run must not delete anything: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "safe to clean") || !strings.Contains(out, "dry run") {
		t.Errorf("output missing expected dry-run report:\n%s", out)
	}
}

// TestPrunePlanDryRunDoesNotMutateLifecycleFile is the end-to-end regression
// test (through the real prunePlan loop, not just EvaluateCandidate directly)
// for a real bug: --dry-run wrote the shipped -> merged lifecycle transition
// to disk before the dryRun branch ever got a say, violating --dry-run's
// documented "confirm first, no changes" contract. The prior dry-run test
// (TestPrunePlanDryRunReportsSafeWithoutDeleting) never caught this because
// it never wrote a lifecycle.json in the first place — only a worktree with
// an existing shipped record actually exercises the write this guards
// against.
func TestPrunePlanDryRunDoesNotMutateLifecycleFile(t *testing.T) {
	repoRoot, worktree := repoWithWorktree(t, "feat-dry-run-lifecycle")
	if err := protocol.WriteLifecycle(worktree, &protocol.Lifecycle{
		State: protocol.LifecycleShipped, Host: "fake", Owner: "o", Repo: "r", Branch: "feat-dry-run-lifecycle",
		PRURL: "https://fake/pr/stale", PRNumber: 5,
	}); err != nil {
		t.Fatalf("WriteLifecycle: %v", err)
	}
	before, err := os.ReadFile(protocol.LifecyclePath(worktree))
	if err != nil {
		t.Fatalf("reading lifecycle.json before: %v", err)
	}

	merged := time.Now()
	f := &fakeForge{findPRFound: true, findPR: forge.PR{HTMLURL: "https://fake/pr/5", Number: 5, MergedAt: &merged}}
	entries := []supervisor.WorktreeEntry{{Path: worktree, Branch: "feat-dry-run-lifecycle"}}

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)

	prunePlan(cmd, context.Background(), nil, f, herdr.Client{}, "o", "r", repoRoot, entries, true)
	if !strings.Contains(buf.String(), "safe to clean") {
		t.Errorf("want the confirmed merge reflected in the dry-run plan:\n%s", buf.String())
	}

	after, err := os.ReadFile(protocol.LifecyclePath(worktree))
	if err != nil {
		t.Fatalf("reading lifecycle.json after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("--dry-run must not mutate lifecycle.json; before:\n%s\nafter:\n%s", before, after)
	}
}

func TestPrunePlanCleansSafeWorktree(t *testing.T) {
	repoRoot, worktree := repoWithWorktree(t, "feat-b")
	merged := time.Now()
	f := &fakeForge{findPRFound: true, findPR: forge.PR{HTMLURL: "https://fake/pr/2", MergedAt: &merged}}
	entries := []supervisor.WorktreeEntry{{Path: worktree, Branch: "feat-b"}}

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)

	prunePlan(cmd, context.Background(), nil, f, herdr.Client{}, "o", "r", repoRoot, entries, false)
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Errorf("worktree should be relocated away from its original path, stat err: %v", err)
	}
	if !strings.Contains(buf.String(), "relocated to") {
		t.Errorf("output missing relocation confirmation:\n%s", buf.String())
	}
}

// TestPrunePlanClosesRecordedHerdrPane is the end-to-end regression test:
// prunePlan must close the herdr pane recorded in a cleaned worktree's
// lifecycle, not just remove the worktree itself.
func TestPrunePlanClosesRecordedHerdrPane(t *testing.T) {
	repoRoot, worktree := repoWithWorktree(t, "feat-d")
	if err := protocol.WritePaneRegistry(repoRoot, protocol.PaneRegistry{Panes: map[string]string{worktree: "w1:p1"}}); err != nil {
		t.Fatalf("WritePaneRegistry: %v", err)
	}
	merged := time.Now()
	f := &fakeForge{findPRFound: true, findPR: forge.PR{HTMLURL: "https://fake/pr/4", MergedAt: &merged}}
	entries := []supervisor.WorktreeEntry{{Path: worktree, Branch: "feat-d"}}

	const paneList = `{"result":{"panes":[{"pane_id":"w1:p1","workspace_id":"w1"}]}}`
	var calls [][]string
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		if args[0] == "pane" && args[1] == "list" {
			return []byte(paneList), nil
		}
		return []byte(`{"result":{}}`), nil
	})

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)

	prunePlan(cmd, context.Background(), nil, f, client, "o", "r", repoRoot, entries, false)

	var closedPane, closedWorkspace bool
	for _, c := range calls {
		if c[0] == "pane" && c[1] == "close" {
			closedPane = true
		}
		if c[0] == "workspace" && c[1] == "close" {
			closedWorkspace = true
		}
	}
	if !closedPane {
		t.Errorf("want the recorded pane closed, herdr calls: %v", calls)
	}
	if !closedWorkspace {
		t.Errorf("want the now-empty workspace closed too, herdr calls: %v", calls)
	}
}

func TestPrunePlanLeavesUnsafeWorktreeInPlace(t *testing.T) {
	repoRoot, worktree := repoWithWorktree(t, "feat-c")
	f := &fakeForge{findPRFound: true, findPR: forge.PR{HTMLURL: "https://fake/pr/3", State: "open"}}
	entries := []supervisor.WorktreeEntry{{Path: worktree, Branch: "feat-c"}}

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)

	prunePlan(cmd, context.Background(), nil, f, herdr.Client{}, "o", "r", repoRoot, entries, false)
	if _, err := os.Stat(worktree); err != nil {
		t.Errorf("an unmerged PR's worktree must never be auto-deleted: %v", err)
	}
	if !strings.Contains(buf.String(), "not safe") {
		t.Errorf("output missing the unsafe reason:\n%s", buf.String())
	}
}

func TestPrunePlanSkipsBareOrDetachedEntries(t *testing.T) {
	f := &fakeForge{}
	entries := []supervisor.WorktreeEntry{{Path: "/repo", Branch: ""}}

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)

	prunePlan(cmd, context.Background(), nil, f, herdr.Client{}, "o", "r", "/repo", entries, true)
	if strings.Contains(buf.String(), "not safe") || strings.Contains(buf.String(), "safe to clean") {
		t.Errorf("a branch-less entry should be skipped entirely:\n%s", buf.String())
	}
}

// TestRunWorktreePruneRejectsMalformedCredentialEnvOverride covers RunE's own
// resolveCredentialOverrides call (cmd/worktree.go:59-62): a malformed
// ~/.argus/config.toml must surface as a command error, not be silently
// swallowed before prune ever gets to resolve its targets.
func TestRunWorktreePruneRejectsMalformedCredentialEnvOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[credential]\nanthropic = \"\\x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGUS_CONFIG_FILE", path)
	repoRoot := gitRepo(t, []string{"remote", "add", "origin", "git@codeberg.org:acme/widget.git"})

	cmd := newWorktreePruneCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{"--repo", repoRoot, "--merged", "--dry-run"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("want an error for a malformed --credential-env config file")
	}
}

// TestResolvePruneTargetsBranchNoMatch covers resolvePruneTargets:123-127:
// --branch pinned to a branch with no linked worktree must fail with a
// UserError naming the branch, not an empty/nil-safe no-op.
func TestResolvePruneTargetsBranchNoMatch(t *testing.T) {
	repoRoot, _ := repoWithWorktree(t, "feat-exists")

	_, err := resolvePruneTargets(context.Background(), io.Discard, &worktreePruneArgs{repo: repoRoot, branch: "no-such-branch"})
	if err == nil {
		t.Fatal("want an error for a branch with no linked worktree")
	}
	if _, ok := errors.AsType[*ui.UserError](err); !ok {
		t.Fatalf("want a ui.UserError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "no worktree found for branch") {
		t.Errorf("error message missing branch context: %v", err)
	}
}

// TestResolvePruneTargetsRepoRootError covers resolvePruneTargets:114-117:
// a --repo path that isn't inside any git repository must surface RepoRoot's
// error wrapped with context, not panic or return a zero-value target set.
func TestResolvePruneTargetsRepoRootError(t *testing.T) {
	notARepo := t.TempDir()

	_, err := resolvePruneTargets(context.Background(), io.Discard, &worktreePruneArgs{repo: notARepo, merged: true})
	if err == nil {
		t.Fatal("want an error when --repo does not point inside a git repository")
	}
	if !strings.Contains(err.Error(), "resolving repo root") {
		t.Errorf("error missing resolvePruneTargets' own wrapping context: %v", err)
	}
}

// TestResolvePruneForgeClientResolveRepoError covers
// resolvePruneForgeClient:142-145: a repo with no "origin" remote can't be
// mapped to a forge host, and that must propagate rather than construct a
// client for an empty host.
func TestResolvePruneForgeClientResolveRepoError(t *testing.T) {
	repoRoot := gitRepo(t) // no remote configured

	_, _, _, err := resolvePruneForgeClient(context.Background(), io.Discard, repoRoot, &worktreePruneArgs{})
	if err == nil {
		t.Fatal("want an error when the repo has no origin remote")
	}
}

// TestResolvePruneForgeClientConfigLoadError covers
// resolvePruneForgeClient:146-149: a repo config path that can't be read as a
// file (here, a directory sitting where config.yml should be) must surface
// repoconfig.Load's error rather than fall back to an empty config.
func TestResolvePruneForgeClientConfigLoadError(t *testing.T) {
	repoRoot := gitRepo(t, []string{"remote", "add", "origin", "https://codeberg.org/o/r.git"})
	configPath := repoconfig.Path(repoRoot)
	if err := os.MkdirAll(configPath, 0o755); err != nil {
		t.Fatalf("seeding a directory at the config path: %v", err)
	}

	_, _, _, err := resolvePruneForgeClient(context.Background(), io.Discard, repoRoot, &worktreePruneArgs{})
	if err == nil {
		t.Fatal("want an error when the repo config path is unreadable")
	}
	if !strings.Contains(err.Error(), "loading") {
		t.Errorf("error missing resolvePruneForgeClient's own wrapping context: %v", err)
	}
}

// TestPrunePlanEvaluateCandidateErrorContinuesSweep covers prunePlan:208-211:
// one entry that fails EvaluateCandidate (here, a FindPR error on a
// lifecycle-less branch) must be logged and printed but must never abort the
// sweep — the next entry, resolved via a cached LifecycleMerged record that
// never calls the failing forge at all, still gets evaluated and reported.
func TestPrunePlanEvaluateCandidateErrorContinuesSweep(t *testing.T) {
	repoRoot, badWorktree := repoWithWorktree(t, "feat-err")
	goodWorktree := repoWithWorktreeIn(t, repoRoot, "feat-ok")
	if err := protocol.WriteLifecycle(goodWorktree, &protocol.Lifecycle{
		State: protocol.LifecycleMerged, Host: "fake", Owner: "o", Repo: "r", Branch: "feat-ok",
		PRURL: "https://fake/pr/ok",
	}); err != nil {
		t.Fatalf("WriteLifecycle: %v", err)
	}

	f := &fakeForge{findPRErr: errors.New("forge unreachable")}
	entries := []supervisor.WorktreeEntry{
		{Path: badWorktree, Branch: "feat-err"},
		{Path: goodWorktree, Branch: "feat-ok"},
	}

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)

	prunePlan(cmd, context.Background(), nil, f, herdr.Client{}, "o", "r", repoRoot, entries, true)
	out := buf.String()
	if !strings.Contains(out, "feat-err") || !strings.Contains(out, "forge unreachable") {
		t.Errorf("want the failing entry's error reported:\n%s", out)
	}
	if !strings.Contains(out, "feat-ok") || !strings.Contains(out, "safe to clean") {
		t.Errorf("want the sweep to continue past the failure and report the next entry:\n%s", out)
	}
}

// TestPrunePlanDryRunSafeCandidateWithPaneIDReportsPaneClose covers
// prunePlan:219-221: a dry-run's plan for a safe candidate that also has a
// recorded herdr pane must call that out, not just the worktree relocation.
func TestPrunePlanDryRunSafeCandidateWithPaneIDReportsPaneClose(t *testing.T) {
	repoRoot, worktree := repoWithWorktree(t, "feat-pane")
	if err := protocol.WritePaneRegistry(repoRoot, protocol.PaneRegistry{Panes: map[string]string{worktree: "w1:p1"}}); err != nil {
		t.Fatalf("WritePaneRegistry: %v", err)
	}
	merged := time.Now()
	f := &fakeForge{findPRFound: true, findPR: forge.PR{HTMLURL: "https://fake/pr/pane", MergedAt: &merged}}
	entries := []supervisor.WorktreeEntry{{Path: worktree, Branch: "feat-pane"}}

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)

	prunePlan(cmd, context.Background(), nil, f, herdr.Client{}, "o", "r", repoRoot, entries, true)
	out := buf.String()
	if !strings.Contains(out, "close herdr pane w1:p1") {
		t.Errorf("want the dry-run plan to mention closing the recorded pane:\n%s", out)
	}
}

// TestPrunePlanCleanWorktreeErrorContinuesSweep covers prunePlan:228-231: a
// CleanWorktree failure (forced here by pointing repoRoot at a directory
// that isn't a git repo, so recoverableRemove's git-common-dir lookup fails)
// must be logged and printed, and must leave the worktree untouched, rather
// than abort or half-clean it.
func TestPrunePlanCleanWorktreeErrorContinuesSweep(t *testing.T) {
	_, worktree := repoWithWorktree(t, "feat-cleanfail")
	merged := time.Now()
	f := &fakeForge{findPRFound: true, findPR: forge.PR{HTMLURL: "https://fake/pr/cf", MergedAt: &merged}}
	entries := []supervisor.WorktreeEntry{{Path: worktree, Branch: "feat-cleanfail"}}

	notARepo := t.TempDir()

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)

	// repoRoot passed to prunePlan is deliberately not the entry's real repo
	// root (only used here to make CleanWorktree's git call fail; EvaluateCandidate
	// itself runs its git checks against the worktree path, unaffected by it).
	prunePlan(cmd, context.Background(), nil, f, herdr.Client{}, "o", "r", notARepo, entries, false)
	out := buf.String()
	if !strings.Contains(out, "cleaning feat-cleanfail") {
		t.Errorf("want the clean failure reported:\n%s", out)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Errorf("a failed clean must leave the worktree in place: %v", err)
	}
}

// TestPrunePlanCleanWorktreeAlreadyGoneReportsRegistrationRemoved covers
// prunePlan:236-238: a worktree whose directory is already gone (dirGone)
// skips the relocation step entirely, so the confirmation message must say
// so instead of naming a relocation destination that never happened.
func TestPrunePlanCleanWorktreeAlreadyGoneReportsRegistrationRemoved(t *testing.T) {
	repoRoot, worktree := repoWithWorktree(t, "feat-gone")
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("simulating an already-gone worktree directory: %v", err)
	}
	merged := time.Now()
	f := &fakeForge{findPRFound: true, findPR: forge.PR{HTMLURL: "https://fake/pr/gone", MergedAt: &merged}}
	entries := []supervisor.WorktreeEntry{{Path: worktree, Branch: "feat-gone", Prunable: true}}

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)

	prunePlan(cmd, context.Background(), nil, f, herdr.Client{}, "o", "r", repoRoot, entries, false)
	out := buf.String()
	if !strings.Contains(out, "registration removed (directory was already gone)") {
		t.Errorf("want the already-gone confirmation message:\n%s", out)
	}
	if !strings.Contains(out, "1 cleaned, 0 left in place") {
		t.Errorf("want the entry still counted cleaned:\n%s", out)
	}
}

// TestPrunePlanClosePaneFailureWarnsButStillCountsCleaned covers
// prunePlan:239-242: a herdr-side pane-close failure must be surfaced as a
// warning (and logged) but must not undo the fact that the worktree itself
// was already successfully cleaned.
func TestPrunePlanClosePaneFailureWarnsButStillCountsCleaned(t *testing.T) {
	repoRoot, worktree := repoWithWorktree(t, "feat-panefail")
	if err := protocol.WritePaneRegistry(repoRoot, protocol.PaneRegistry{Panes: map[string]string{worktree: "w1:p1"}}); err != nil {
		t.Fatalf("WritePaneRegistry: %v", err)
	}
	merged := time.Now()
	f := &fakeForge{findPRFound: true, findPR: forge.PR{HTMLURL: "https://fake/pr/pf", MergedAt: &merged}}
	entries := []supervisor.WorktreeEntry{{Path: worktree, Branch: "feat-panefail"}}

	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "pane" && args[1] == "list" {
			return []byte(`{"result":{"panes":[{"pane_id":"w1:p1","workspace_id":"w1"}]}}`), nil
		}
		if args[0] == "pane" && args[1] == "close" {
			return nil, errors.New("herdr socket unreachable")
		}
		return []byte(`{"result":{}}`), nil
	})

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)

	prunePlan(cmd, context.Background(), nil, f, client, "o", "r", repoRoot, entries, false)
	out := buf.String()
	if !strings.Contains(out, "relocated to") {
		t.Errorf("want the worktree relocation confirmed despite the pane failure:\n%s", out)
	}
	if !strings.Contains(out, "closing herdr pane w1:p1 failed") {
		t.Errorf("want the pane-close failure surfaced as a warning:\n%s", out)
	}
	if !strings.Contains(out, "1 cleaned, 0 left in place") {
		t.Errorf("want the entry still counted cleaned despite the pane warning:\n%s", out)
	}
}

func TestFilterByBranch(t *testing.T) {
	entries := []supervisor.WorktreeEntry{{Path: "/a", Branch: "a"}, {Path: "/b", Branch: "b"}}
	got := filterByBranch(entries, "b")
	if len(got) != 1 || got[0].Path != "/b" {
		t.Errorf("filterByBranch: got %+v", got)
	}
	if got := filterByBranch(entries, "missing"); got != nil {
		t.Errorf("filterByBranch for an unknown branch: got %+v, want nil", got)
	}
}
