package supervisor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/forge"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

// fakePruneForge is a minimal forge.Forge stub: only FindPR is exercised by
// prune, the rest are unused no-ops. It records the owner/repo/branch each
// FindPR call was made with (lastOwner/lastRepo/lastBranch), so a test can
// assert prune queried using the lifecycle record's own identity rather than
// whatever the caller happened to pass in.
type fakePruneForge struct {
	err        error
	lastOwner  string
	lastRepo   string
	lastBranch string
	pr         forge.PR
	calls      int
	found      bool
}

func (f *fakePruneForge) Host() string { return "fake" }
func (f *fakePruneForge) OpenPR(context.Context, *forge.PRRequest) (forge.PR, error) {
	return forge.PR{}, nil
}
func (f *fakePruneForge) FetchIssue(context.Context, string, string, int) (forge.Issue, error) {
	return forge.Issue{}, nil
}
func (f *fakePruneForge) FindPR(_ context.Context, owner, repo, branch string) (forge.PR, bool, error) {
	f.calls++
	f.lastOwner, f.lastRepo, f.lastBranch = owner, repo, branch
	return f.pr, f.found, f.err
}

func TestParseWorktreePorcelain(t *testing.T) {
	out := `worktree /repo
HEAD abc123
branch refs/heads/main

worktree /repo/.claude/worktrees/feat-a
HEAD def456
branch refs/heads/feat-a

worktree /repo/.claude/worktrees/feat-b
HEAD 789abc
branch refs/heads/feat-b
prunable gitdir file points to non-existent location

worktree /repo/.claude/worktrees/detached
HEAD 111222
detached
`
	entries := parseWorktreePorcelain(out)
	if len(entries) != 4 {
		t.Fatalf("want 4 entries, got %d: %+v", len(entries), entries)
	}
	if entries[0].Branch != "main" || entries[0].Prunable {
		t.Errorf("entry 0: %+v", entries[0])
	}
	if entries[1].Branch != "feat-a" || entries[1].Prunable {
		t.Errorf("entry 1: %+v", entries[1])
	}
	if entries[2].Branch != "feat-b" || !entries[2].Prunable {
		t.Errorf("entry 2 (prunable) mismatch: %+v", entries[2])
	}
	if entries[3].Branch != "" {
		t.Errorf("detached worktree should have empty Branch, got %q", entries[3].Branch)
	}
}

// initRepoWithWorktree builds a tiny real repo (origin + a local clone) with
// one linked worktree checked out on branch, its commit already pushed so the
// branch has a real upstream — the same shape ship leaves behind.
func initRepoWithWorktree(t *testing.T, branch string) (repoRoot, worktree string) {
	t.Helper()
	repoRoot, base := initGitRepo(t)
	gitDo(t, repoRoot, "checkout", "-q", "-b", branch)
	gitDo(t, repoRoot, "push", "-q", "-u", "origin", branch)
	gitDo(t, repoRoot, "checkout", "-q", base)

	worktree = filepath.Join(t.TempDir(), branch)
	gitDo(t, repoRoot, "worktree", "add", "-q", worktree, branch)
	return repoRoot, worktree
}

func TestListLinkedWorktreesExcludesMainAndReportsLinked(t *testing.T) {
	repoRoot, worktree := initRepoWithWorktree(t, "feat-x")
	ctx := context.Background()

	entries, err := ListLinkedWorktrees(ctx, repoRoot)
	if err != nil {
		t.Fatalf("ListLinkedWorktrees: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 linked worktree (main excluded), got %d: %+v", len(entries), entries)
	}
	if entries[0].Branch != "feat-x" {
		t.Errorf("branch = %q, want feat-x", entries[0].Branch)
	}
	if !strings.HasSuffix(entries[0].Path, worktree) && entries[0].Path != worktree {
		t.Errorf("path = %q, want %q", entries[0].Path, worktree)
	}
}

// TestHasUncommittedChangesIgnoresControlPlaneOnlyDirectory guards a real
// regression: writing only a lifecycle.json into a worktree makes the whole
// .claude/argus directory untracked, and git's default `git status
// --porcelain` collapses an entirely-untracked directory to a single "??
// .claude/" line rather than listing the file — indistinguishable, by path
// alone, from some unrelated untracked directory. Without
// --untracked-files=all (see hasUncommittedChanges), isControlPlanePath can
// never match that collapsed line, so every worktree a worker ever ran in —
// which is all of them — would report dirty forever.
func TestHasUncommittedChangesIgnoresControlPlaneOnlyDirectory(t *testing.T) {
	_, worktree := initRepoWithWorktree(t, "feat-control-plane-only")
	if err := protocol.WriteLifecycle(worktree, &protocol.Lifecycle{State: protocol.LifecycleShipped}); err != nil {
		t.Fatalf("WriteLifecycle: %v", err)
	}

	dirty, err := hasUncommittedChanges(context.Background(), worktree)
	if err != nil {
		t.Fatalf("hasUncommittedChanges: %v", err)
	}
	if dirty {
		t.Error("a worktree whose only untracked content is argus's own control-plane directory should not report dirty")
	}
}

func TestHasUncommittedChangesStashUnpushed(t *testing.T) {
	_, worktree := initRepoWithWorktree(t, "feat-y")
	ctx := context.Background()

	dirty, err := hasUncommittedChanges(ctx, worktree)
	if err != nil || dirty {
		t.Fatalf("clean worktree: dirty=%v err=%v", dirty, err)
	}
	stashed, err := hasStash(ctx, worktree)
	if err != nil || stashed {
		t.Fatalf("no stash yet: stashed=%v err=%v", stashed, err)
	}
	if hasUnpushedCommits(ctx, worktree) {
		t.Fatal("freshly pushed branch should not report unpushed commits")
	}

	if werr := os.WriteFile(filepath.Join(worktree, "f.txt"), []byte("line1\nline2\n"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	dirty, err = hasUncommittedChanges(ctx, worktree)
	if err != nil || !dirty {
		t.Fatalf("modified worktree: dirty=%v err=%v", dirty, err)
	}

	gitDo(t, worktree, "stash", "-q")
	stashed, err = hasStash(ctx, worktree)
	if err != nil || !stashed {
		t.Fatalf("after stash: stashed=%v err=%v", stashed, err)
	}
	dirty, err = hasUncommittedChanges(ctx, worktree)
	if err != nil || dirty {
		t.Fatalf("worktree clean again after stash: dirty=%v err=%v", dirty, err)
	}

	gitDo(t, worktree, "commit", "-q", "--allow-empty", "-m", "local only")
	if !hasUnpushedCommits(ctx, worktree) {
		t.Fatal("a local-only commit should report unpushed commits")
	}
}

func TestHasUnpushedCommitsNoUpstreamIsUnsafe(t *testing.T) {
	repoRoot, base := initGitRepo(t)
	gitDo(t, repoRoot, "checkout", "-q", "-b", "no-upstream")
	_ = base
	if !hasUnpushedCommits(context.Background(), repoRoot) {
		t.Error("a branch with no upstream tracking ref should be treated as unpushed (unsafe)")
	}
}

func TestEvaluateCandidateSafeWhenMergedAndClean(t *testing.T) {
	_, worktree := initRepoWithWorktree(t, "feat-safe")
	f := &fakePruneForge{found: true, pr: forge.PR{HTMLURL: "https://fake/pr/1", State: "closed", MergedAt: mergedNow()}}

	c, err := EvaluateCandidate(context.Background(), f, "o", "r", worktree, "feat-safe", false, false)
	if err != nil {
		t.Fatalf("EvaluateCandidate: %v", err)
	}
	if !c.SafeToClean {
		t.Errorf("want safe to clean, reasons: %v", c.Reasons)
	}
	if !c.Merged {
		t.Error("want Merged true")
	}
}

func TestEvaluateCandidateUnsafeWhenNotMerged(t *testing.T) {
	_, worktree := initRepoWithWorktree(t, "feat-open")
	f := &fakePruneForge{found: true, pr: forge.PR{HTMLURL: "https://fake/pr/2", State: "open"}}

	c, err := EvaluateCandidate(context.Background(), f, "o", "r", worktree, "feat-open", false, false)
	if err != nil {
		t.Fatalf("EvaluateCandidate: %v", err)
	}
	if c.SafeToClean {
		t.Error("an open PR should never be safe to clean")
	}
	if len(c.Reasons) == 0 {
		t.Error("want a reason explaining why it's unsafe")
	}
}

func TestEvaluateCandidateUnsafeWhenDirty(t *testing.T) {
	_, worktree := initRepoWithWorktree(t, "feat-dirty")
	if err := os.WriteFile(filepath.Join(worktree, "f.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakePruneForge{found: true, pr: forge.PR{MergedAt: mergedNow()}}

	c, err := EvaluateCandidate(context.Background(), f, "o", "r", worktree, "feat-dirty", false, false)
	if err != nil {
		t.Fatalf("EvaluateCandidate: %v", err)
	}
	if c.SafeToClean {
		t.Error("uncommitted changes should never be safe to clean")
	}
}

func TestEvaluateCandidateNoPRFound(t *testing.T) {
	_, worktree := initRepoWithWorktree(t, "feat-nopr")
	f := &fakePruneForge{found: false}

	c, err := EvaluateCandidate(context.Background(), f, "o", "r", worktree, "feat-nopr", false, false)
	if err != nil {
		t.Fatalf("EvaluateCandidate: %v", err)
	}
	if c.SafeToClean {
		t.Error("no PR found should never be safe to clean")
	}
}

func TestEvaluateCandidateForgeErrorPropagates(t *testing.T) {
	_, worktree := initRepoWithWorktree(t, "feat-err")
	f := &fakePruneForge{err: context.DeadlineExceeded}

	if _, err := EvaluateCandidate(context.Background(), f, "o", "r", worktree, "feat-err", false, false); err == nil {
		t.Error("want a forge lookup failure to propagate, not be swallowed into a reason")
	}
}

func TestEvaluateCandidateSkipsDirtyChecksWhenDirGone(t *testing.T) {
	f := &fakePruneForge{found: true, pr: forge.PR{MergedAt: mergedNow()}}

	c, err := EvaluateCandidate(context.Background(), f, "o", "r", "/does/not/exist", "feat-gone", true, false)
	if err != nil {
		t.Fatalf("EvaluateCandidate: %v", err)
	}
	if !c.SafeToClean {
		t.Errorf("a merged PR whose directory is already gone should be safe to clean, reasons: %v", c.Reasons)
	}
}

func TestEvaluateCandidateTrustsCachedMergedLifecycleWithoutForgeCall(t *testing.T) {
	_, worktree := initRepoWithWorktree(t, "feat-cached")
	if err := protocol.WriteLifecycle(worktree, &protocol.Lifecycle{
		State: protocol.LifecycleMerged, Host: "fake", Owner: "o", Repo: "r", Branch: "feat-cached",
		PRURL: "https://fake/pr/9", PRNumber: 9,
	}); err != nil {
		t.Fatalf("WriteLifecycle: %v", err)
	}
	f := &fakePruneForge{} // found defaults to false: any FindPR call would surface as "no PR found"

	c, err := EvaluateCandidate(context.Background(), f, "o", "r", worktree, "feat-cached", false, false)
	if err != nil {
		t.Fatalf("EvaluateCandidate: %v", err)
	}
	if f.calls != 0 {
		t.Errorf("a cached LifecycleMerged record should skip the forge round-trip entirely, got %d call(s)", f.calls)
	}
	if !c.Merged || c.PRURL != "https://fake/pr/9" {
		t.Errorf("want the cached lifecycle's merge state trusted directly, got merged=%v prURL=%q", c.Merged, c.PRURL)
	}
	if !c.SafeToClean {
		t.Errorf("clean worktree with a cached merged PR should be safe to clean, reasons: %v", c.Reasons)
	}
}

func TestEvaluateCandidateAdvancesShippedLifecycleToMergedUsingItsOwnIdentity(t *testing.T) {
	_, worktree := initRepoWithWorktree(t, "feat-advance")
	if err := protocol.WriteLifecycle(worktree, &protocol.Lifecycle{
		State: protocol.LifecycleShipped, Host: "fake", Owner: "lc-owner", Repo: "lc-repo", Branch: "lc-branch",
		PRURL: "https://fake/pr/stale", PRNumber: 5,
	}); err != nil {
		t.Fatalf("WriteLifecycle: %v", err)
	}
	f := &fakePruneForge{found: true, pr: forge.PR{HTMLURL: "https://fake/pr/5", Number: 5, MergedAt: mergedNow()}}

	c, err := EvaluateCandidate(context.Background(), f, "caller-owner", "caller-repo", worktree, "feat-advance", false, false)
	if err != nil {
		t.Fatalf("EvaluateCandidate: %v", err)
	}
	if !c.Merged {
		t.Errorf("a freshly confirmed merge should report Merged, reasons: %v", c.Reasons)
	}
	// The lookup should use the lifecycle's own recorded identity, not
	// whatever owner/repo the caller happened to pass for the sweep.
	if f.lastOwner != "lc-owner" || f.lastRepo != "lc-repo" || f.lastBranch != "lc-branch" {
		t.Errorf("FindPR called with %s/%s@%s, want the lifecycle's own lc-owner/lc-repo@lc-branch", f.lastOwner, f.lastRepo, f.lastBranch)
	}

	got, found, lerr := protocol.LoadLifecycle(worktree)
	if lerr != nil || !found {
		t.Fatalf("LoadLifecycle: found=%v err=%v", found, lerr)
	}
	if got.State != protocol.LifecycleMerged {
		t.Errorf("lifecycle state should advance shipped -> merged, got %q", got.State)
	}
	if got.PRURL != "https://fake/pr/5" {
		t.Errorf("lifecycle PRURL should update to the confirmed PR, got %q", got.PRURL)
	}
}

// TestEvaluateCandidateDryRunDoesNotMutateLifecycleFile is the regression
// test for a real bug: --dry-run's documented "confirm first, no changes"
// contract was violated because EvaluateCandidate/resolveMergeState wrote the
// shipped -> merged lifecycle transition to disk unconditionally, before
// cmd/worktree.go's dryRun branch ever got a say. Same setup as
// TestEvaluateCandidateAdvancesShippedLifecycleToMergedUsingItsOwnIdentity
// (a shipped record, a forge confirming the merge) except dryRun=true: the
// call must still report Merged so --dry-run's plan is accurate, but
// lifecycle.json on disk must come out byte-identical to what was there
// before the call.
func TestEvaluateCandidateDryRunDoesNotMutateLifecycleFile(t *testing.T) {
	_, worktree := initRepoWithWorktree(t, "feat-dry-run-no-mutate")
	if err := protocol.WriteLifecycle(worktree, &protocol.Lifecycle{
		State: protocol.LifecycleShipped, Host: "fake", Owner: "lc-owner", Repo: "lc-repo", Branch: "lc-branch",
		PRURL: "https://fake/pr/stale", PRNumber: 5,
	}); err != nil {
		t.Fatalf("WriteLifecycle: %v", err)
	}
	before, err := os.ReadFile(protocol.LifecyclePath(worktree))
	if err != nil {
		t.Fatalf("reading lifecycle.json before: %v", err)
	}
	f := &fakePruneForge{found: true, pr: forge.PR{HTMLURL: "https://fake/pr/5", Number: 5, MergedAt: mergedNow()}}

	c, err := EvaluateCandidate(context.Background(), f, "caller-owner", "caller-repo", worktree, "feat-dry-run-no-mutate", false, true)
	if err != nil {
		t.Fatalf("EvaluateCandidate: %v", err)
	}
	if !c.Merged {
		t.Errorf("--dry-run should still report the confirmed merge for an accurate plan, reasons: %v", c.Reasons)
	}

	after, err := os.ReadFile(protocol.LifecyclePath(worktree))
	if err != nil {
		t.Fatalf("reading lifecycle.json after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("--dry-run must not mutate lifecycle.json; before:\n%s\nafter:\n%s", before, after)
	}
}

func TestEvaluateCandidateFallsBackToBranchLookupWithNoLifecycle(t *testing.T) {
	_, worktree := initRepoWithWorktree(t, "feat-nolifecycle")
	f := &fakePruneForge{found: true, pr: forge.PR{HTMLURL: "https://fake/pr/11", MergedAt: mergedNow()}}

	c, err := EvaluateCandidate(context.Background(), f, "caller-owner", "caller-repo", worktree, "feat-nolifecycle", false, false)
	if err != nil {
		t.Fatalf("EvaluateCandidate: %v", err)
	}
	if !c.Merged {
		t.Errorf("want merge confirmed via the fallback branch lookup, reasons: %v", c.Reasons)
	}
	if f.lastOwner != "caller-owner" || f.lastRepo != "caller-repo" || f.lastBranch != "feat-nolifecycle" {
		t.Errorf("FindPR called with %s/%s@%s, want the caller-supplied caller-owner/caller-repo@feat-nolifecycle", f.lastOwner, f.lastRepo, f.lastBranch)
	}
	// No lifecycle existed before the call, and none should be fabricated —
	// only an *existing* record is advanced (see resolveMergeState).
	if _, found, lerr := protocol.LoadLifecycle(worktree); lerr != nil || found {
		t.Errorf("no lifecycle should be written when none existed: found=%v err=%v", found, lerr)
	}
}

func TestCleanWorktreeMarksLifecyclePrunedInRelocatedCopy(t *testing.T) {
	repoRoot, worktree := initRepoWithWorktree(t, "feat-prune-mark")
	if err := protocol.WriteLifecycle(worktree, &protocol.Lifecycle{
		State: protocol.LifecycleMerged, Host: "fake", Owner: "o", Repo: "r", Branch: "feat-prune-mark",
		PRURL: "https://fake/pr/13", PRNumber: 13,
	}); err != nil {
		t.Fatalf("WriteLifecycle: %v", err)
	}
	c := &PruneCandidate{Path: worktree, Branch: "feat-prune-mark", SafeToClean: true}

	dest, err := CleanWorktree(context.Background(), repoRoot, c)
	if err != nil {
		t.Fatalf("CleanWorktree: %v", err)
	}

	got, found, lerr := protocol.LoadLifecycle(dest)
	if lerr != nil || !found {
		t.Fatalf("LoadLifecycle on the relocated copy: found=%v err=%v", found, lerr)
	}
	if got.State != protocol.LifecyclePruned {
		t.Errorf("relocated lifecycle should be marked pruned, got %q", got.State)
	}
}

func TestCleanWorktreeRelocatesAndRemovesRegistration(t *testing.T) {
	repoRoot, worktree := initRepoWithWorktree(t, "feat-clean")
	ctx := context.Background()
	c := &PruneCandidate{Path: worktree, Branch: "feat-clean", SafeToClean: true}

	dest, err := CleanWorktree(ctx, repoRoot, c)
	if err != nil {
		t.Fatalf("CleanWorktree: %v", err)
	}
	if dest == "" {
		t.Fatal("want a non-empty relocation destination")
	}
	if _, statErr := os.Stat(dest); statErr != nil {
		t.Errorf("relocated content should exist at %s: %v", dest, statErr)
	}
	if _, statErr := os.Stat(worktree); !os.IsNotExist(statErr) {
		t.Errorf("original worktree path should be gone, stat err: %v", statErr)
	}

	entries, err := ListLinkedWorktrees(ctx, repoRoot)
	if err != nil {
		t.Fatalf("ListLinkedWorktrees: %v", err)
	}
	for _, e := range entries {
		if e.Branch == "feat-clean" {
			t.Errorf("worktree registration for feat-clean should be gone, still present: %+v", e)
		}
	}
}

func TestCleanWorktreeDirAlreadyGoneOnlyRemovesRegistration(t *testing.T) {
	repoRoot, worktree := initRepoWithWorktree(t, "feat-gone")
	ctx := context.Background()

	if err := os.RemoveAll(worktree); err != nil {
		t.Fatal(err)
	}
	c := &PruneCandidate{Path: worktree, Branch: "feat-gone", SafeToClean: true, DirGone: true}

	dest, err := CleanWorktree(ctx, repoRoot, c)
	if err != nil {
		t.Fatalf("CleanWorktree: %v", err)
	}
	if dest != "" {
		t.Errorf("nothing to relocate when the directory is already gone, got dest %q", dest)
	}

	entries, err := ListLinkedWorktrees(ctx, repoRoot)
	if err != nil {
		t.Fatalf("ListLinkedWorktrees: %v", err)
	}
	for _, e := range entries {
		if e.Branch == "feat-gone" {
			t.Errorf("worktree registration for feat-gone should be gone, still present: %+v", e)
		}
	}
}

func mergedNow() *time.Time {
	now := time.Now()
	return &now
}
