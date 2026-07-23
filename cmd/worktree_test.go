package cmd

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/forge"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
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

func TestPrunePlanDryRunReportsSafeWithoutDeleting(t *testing.T) {
	repoRoot, worktree := repoWithWorktree(t, "feat-a")
	merged := time.Now()
	f := &fakeForge{findPRFound: true, findPR: forge.PR{HTMLURL: "https://fake/pr/1", MergedAt: &merged}}
	entries := []supervisor.WorktreeEntry{{Path: worktree, Branch: "feat-a"}}

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)

	prunePlan(cmd, context.Background(), nil, f, "o", "r", repoRoot, entries, true)
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

	prunePlan(cmd, context.Background(), nil, f, "o", "r", repoRoot, entries, true)
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

	prunePlan(cmd, context.Background(), nil, f, "o", "r", repoRoot, entries, false)
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Errorf("worktree should be relocated away from its original path, stat err: %v", err)
	}
	if !strings.Contains(buf.String(), "relocated to") {
		t.Errorf("output missing relocation confirmation:\n%s", buf.String())
	}
}

func TestPrunePlanLeavesUnsafeWorktreeInPlace(t *testing.T) {
	repoRoot, worktree := repoWithWorktree(t, "feat-c")
	f := &fakeForge{findPRFound: true, findPR: forge.PR{HTMLURL: "https://fake/pr/3", State: "open"}}
	entries := []supervisor.WorktreeEntry{{Path: worktree, Branch: "feat-c"}}

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)

	prunePlan(cmd, context.Background(), nil, f, "o", "r", repoRoot, entries, false)
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

	prunePlan(cmd, context.Background(), nil, f, "o", "r", "/repo", entries, true)
	if strings.Contains(buf.String(), "not safe") || strings.Contains(buf.String(), "safe to clean") {
		t.Errorf("a branch-less entry should be skipped entirely:\n%s", buf.String())
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
