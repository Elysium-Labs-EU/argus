package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

// TestInvalidateStatusRemovesStaleFiles verifies InvalidateStatus's fix: a
// rebase dispatch must not let a leftover status.json (or verdict.json) from an
// earlier, unrelated task in the same worktree survive to be misread as this
// dispatch's outcome.
func TestInvalidateStatusRemovesStaleFiles(t *testing.T) {
	wt := t.TempDir()
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{Phase: protocol.PhaseAwaitingReview}); err != nil {
		t.Fatalf("seeding status.json: %v", err)
	}
	if err := protocol.WriteApproval(wt, &protocol.Approval{Approved: true}); err != nil {
		t.Fatalf("seeding verdict.json: %v", err)
	}

	if err := InvalidateStatus(wt); err != nil {
		t.Fatalf("InvalidateStatus: %v", err)
	}

	if _, err := os.Stat(protocol.StatusPath(wt)); !os.IsNotExist(err) {
		t.Errorf("status.json should be removed, stat err: %v", err)
	}
	if _, err := os.Stat(protocol.VerdictPath(wt)); !os.IsNotExist(err) {
		t.Errorf("verdict.json should be removed, stat err: %v", err)
	}
}

// TestInvalidateStatusMissingFilesOK confirms a worktree with no prior status
// or verdict files (the common case: a fresh worker, not a re-dispatch) is not
// an error.
func TestInvalidateStatusMissingFilesOK(t *testing.T) {
	if err := InvalidateStatus(t.TempDir()); err != nil {
		t.Fatalf("InvalidateStatus on a clean worktree: %v", err)
	}
}

// retryOnError calls fn until it succeeds or attempts is exhausted, pausing
// wait between calls, and returns the last error seen.
func retryOnError(attempts int, wait time.Duration, fn func() error) error {
	var err error
	for range attempts {
		if err = fn(); err == nil {
			return nil
		}
		time.Sleep(wait)
	}
	return err
}

// removeAllTolerant retries os.RemoveAll on a transient ENOTEMPTY: a
// concurrent writer still touching a subdirectory (e.g. a git background
// process finishing a write into .git/objects/pack) can create a new entry
// in the narrow gap between RemoveAll's last empty directory listing and its
// final rmdir, which surfaces as an ENOTEMPTY that RemoveAll itself does not
// retry.
func removeAllTolerant(path string, attempts int, wait time.Duration) error {
	return retryOnError(attempts, wait, func() error { return os.RemoveAll(path) })
}

// gitTempDir is t.TempDir() for a directory a real git subprocess will write
// into. It layers a retrying removal ahead of Go's own TempDir cleanup so a
// still-finishing background git writer doesn't turn into a cleanup failure
// that fails the test despite its assertions having already passed.
func gitTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		_ = removeAllTolerant(dir, 10, 50*time.Millisecond)
	})
	return dir
}

// TestRemoveAllTolerantSurvivesTransientWriter reproduces the shape of the
// original flake: a concurrent writer keeps recreating a file in the target
// directory for a bounded window, so a single os.RemoveAll pass fails with
// ENOTEMPTY, and asserts the retry loop succeeds once the writer stops
// within its retry budget.
func TestRemoveAllTolerantSurvivesTransientWriter(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "objects", "pack")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		deadline := time.Now().Add(80 * time.Millisecond)
		for i := 0; time.Now().Before(deadline); i++ {
			_ = os.WriteFile(filepath.Join(sub, fmt.Sprintf("tmp-%d.pack", i)), []byte("x"), 0o644)
			time.Sleep(2 * time.Millisecond)
		}
	}()
	<-writerDone

	if err := removeAllTolerant(dir, 10, 20*time.Millisecond); err != nil {
		t.Fatalf("removeAllTolerant did not survive the transient writer: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("dir should be removed, stat err: %v", err)
	}
}

// TestRetryOnErrorSucceedsAfterTransientFailures confirms the retry loop
// keeps calling fn past early failures and returns nil once fn recovers,
// within its attempt budget.
func TestRetryOnErrorSucceedsAfterTransientFailures(t *testing.T) {
	calls := 0
	err := retryOnError(5, time.Millisecond, func() error {
		calls++
		if calls < 3 {
			return fmt.Errorf("transient failure %d", calls)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retryOnError: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

// TestRetryOnErrorReturnsLastErrorWhenExhausted confirms the retry loop
// gives up and reports the last error, rather than blocking forever, once fn
// never recovers within the attempt budget.
func TestRetryOnErrorReturnsLastErrorWhenExhausted(t *testing.T) {
	calls := 0
	err := retryOnError(3, time.Millisecond, func() error {
		calls++
		return fmt.Errorf("failure %d", calls)
	})
	if err == nil {
		t.Fatal("expected an error once retries are exhausted")
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

// initGitRepo builds a tiny real git repo with an origin/<base> remote so the
// merge-tree conflict check runs against actual git plumbing.
func initGitRepo(t *testing.T) (worktree, base string) {
	t.Helper()
	base = "main"
	origin := gitTempDir(t)
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}

	// Bare origin with a main branch holding one file. gc.auto=0 keeps
	// receive-pack from ever spawning a detached `git gc --auto` on push: left
	// enabled, that background repack can still be rewriting origin's pack
	// files the moment the local clone below raw-copies them, vanishing a
	// .tmp-*-pack-*.rev mid-copy and failing the clone — the same
	// background-writer hazard gitTempDir's cleanup already retries around,
	// but the clone step has no equivalent protection.
	run(origin, "init", "-q", "--bare", "-b", base, ".")
	run(origin, "config", "gc.auto", "0")
	seed := gitTempDir(t)
	run(seed, "init", "-q", "-b", base, ".")
	if err := os.WriteFile(filepath.Join(seed, "f.txt"), []byte("line1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(seed, "add", "-A")
	run(seed, "commit", "-q", "-m", "seed")
	run(seed, "remote", "add", "origin", origin)
	run(seed, "push", "-q", "origin", base)

	// Worktree clone; its branch will diverge from origin/main.
	worktree = gitTempDir(t)
	run(filepath.Dir(worktree), "clone", "-q", origin, filepath.Base(worktree))
	return worktree, base
}

func gitDo(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

func TestConflictsWithDetectsCleanAndConflicting(t *testing.T) {
	ctx := context.Background()

	// Clean case: branch edits a different line region than origin.
	wt, base := initGitRepo(t)
	if err := os.WriteFile(filepath.Join(wt, "new.txt"), []byte("independent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitDo(t, wt, "add", "-A")
	gitDo(t, wt, "commit", "-q", "-m", "add independent file")
	if err := FetchBase(ctx, wt, base); err != nil {
		t.Fatalf("FetchBase: %v", err)
	}
	conflicts, err := ConflictsWith(ctx, wt, base)
	if err != nil {
		t.Fatalf("ConflictsWith(clean): %v", err)
	}
	if conflicts {
		t.Errorf("independent change should not conflict")
	}

	// Conflicting case: origin and the branch edit the same line differently.
	wt2, base2 := initGitRepo(t)
	origin := mustRemote(t, wt2)
	// Advance origin/main to change f.txt line1.
	other := gitTempDir(t)
	gitDo(t, filepath.Dir(other), "clone", "-q", origin, filepath.Base(other))
	if werr := os.WriteFile(filepath.Join(other, "f.txt"), []byte("origin-change\n"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	gitDo(t, other, "add", "-A")
	gitDo(t, other, "commit", "-q", "-m", "origin edits line1")
	gitDo(t, other, "push", "-q", "origin", base2)
	// Branch edits the same line differently.
	if werr := os.WriteFile(filepath.Join(wt2, "f.txt"), []byte("branch-change\n"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	gitDo(t, wt2, "add", "-A")
	gitDo(t, wt2, "commit", "-q", "-m", "branch edits line1")
	if ferr := FetchBase(ctx, wt2, base2); ferr != nil {
		t.Fatalf("FetchBase: %v", ferr)
	}
	conflicts, err = ConflictsWith(ctx, wt2, base2)
	if err != nil {
		t.Fatalf("ConflictsWith(conflict): %v", err)
	}
	if !conflicts {
		t.Errorf("same-line divergent edits should conflict")
	}
}

// TestFuncNameInContext covers the declaration shapes funcNameRe's plain
// "identifier immediately before (" rule can't extract a name from — generic
// funcs, arrow-fn consts, and class/type/interface/struct decls — plus the
// plain-func case it already handled and the conservative whole-line
// fallback for anything none of the patterns recognize.
func TestFuncNameInContext(t *testing.T) {
	tests := []struct {
		name    string
		context string
		want    string
	}{
		{"plain receiver method", "func (s *Supervisor) reconcile(cfg *Config) error {", "reconcile"},
		{"plain top-level func", "func unrelated() int {", "unrelated"},
		{"generic top-level func", "func Map[T any](items []T) {", "Map"},
		{"generic receiver method", "func (s *Server[T]) Foo[U any](x U) {", "Foo"},
		{"arrow-fn const", "const fn = (a) => {", "fn"},
		{"arrow-fn let, single arg no parens", "let fn = a => {", "fn"},
		{"class decl", "class Foo {", "Foo"},
		{"struct decl via type", "type Config struct {", "Config"},
		{"interface decl", "interface Bar {", "Bar"},
		{"unrecognized shape falls back to whole line", "??? not a declaration ???", "??? not a declaration ???"},
		{"empty context stays empty", "", ""},
		{"whitespace-only context stays empty", "   ", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := funcNameInContext(tt.context); got != tt.want {
				t.Errorf("funcNameInContext(%q) = %q, want %q", tt.context, got, tt.want)
			}
		})
	}
}

// reconcileBase reproduces a diff-adjacency hazard: reconcile() has two
// structurally near-identical "for _, st := range states"
// loops (mirroring the real internal/supervisor/loop.go), which is what leads
// git's diff to anchor a change to the wrong loop and consider two branches'
// edits to the second loop non-adjacent even though they aren't.
const reconcileBase = `package supervisor

func reconcile(cfg *Config, states []*workerState) {
	for _, st := range states {
		if !st.hasFile {
			continue
		}
		measure(st)
	}

	for _, st := range states {
		if !st.hasFile {
			continue
		}
		ok, err := HasPlanEvidence(cfg.Home, st.Worktree)
		if err != nil {
			continue
		}
		st.hasPlanEvidence = ok
	}
}

func unrelated() int {
	return 1
}
`

// reconcileWithGuard is one half of a same-function, non-overlapping-lines
// edit pair: it inserts a guard clause between the two loops, directly above
// the second loop, without touching the HasPlanEvidence line itself.
const reconcileWithGuard = `package supervisor

func reconcile(cfg *Config, states []*workerState) {
	for _, st := range states {
		if !st.hasFile {
			continue
		}
		measure(st)
	}

	if !usesDefaultLauncher(cfg.Launcher) {
		return
	}

	for _, st := range states {
		if !st.hasFile {
			continue
		}
		ok, err := HasPlanEvidence(cfg.Home, st.Worktree)
		if err != nil {
			continue
		}
		st.hasPlanEvidence = ok
	}
}

func unrelated() int {
	return 1
}
`

// reconcileWithRename is the other half of the edit pair: it renames the
// HasPlanEvidence call to route through a seam, without touching any other
// line reconcileWithGuard touches.
const reconcileWithRename = `package supervisor

func reconcile(cfg *Config, states []*workerState) {
	for _, st := range states {
		if !st.hasFile {
			continue
		}
		measure(st)
	}

	for _, st := range states {
		if !st.hasFile {
			continue
		}
		ok, err := defaultAgent.PlanEvidence(cfg.Home, st.Worktree)
		if err != nil {
			continue
		}
		st.hasPlanEvidence = ok
	}
}

func unrelated() int {
	return 1
}
`

// reconcileUnrelatedEdit touches only the unrelated() function, leaving
// reconcile() byte-for-byte identical to reconcileBase.
const reconcileUnrelatedEdit = `package supervisor

func reconcile(cfg *Config, states []*workerState) {
	for _, st := range states {
		if !st.hasFile {
			continue
		}
		measure(st)
	}

	for _, st := range states {
		if !st.hasFile {
			continue
		}
		ok, err := HasPlanEvidence(cfg.Home, st.Worktree)
		if err != nil {
			continue
		}
		st.hasPlanEvidence = ok
	}
}

func unrelated() int {
	return 2
}
`

// initGoRepo builds a tiny real git repo — bare origin plus a clone — seeded
// with content on main, so a test can diverge the clone's branch and origin's
// main independently and inspect the real git merge-tree/diff plumbing
// ConflictsWith runs against.
func initGoRepo(t *testing.T, seedContent string) (worktree, base string) {
	t.Helper()
	base = "main"
	origin := gitTempDir(t)
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}

	run(origin, "init", "-q", "--bare", "-b", base, ".")
	seed := gitTempDir(t)
	run(seed, "init", "-q", "-b", base, ".")
	if err := os.WriteFile(filepath.Join(seed, "loop.go"), []byte(seedContent), 0o644); err != nil {
		t.Fatal(err)
	}
	run(seed, "add", "-A")
	run(seed, "commit", "-q", "-m", "seed")
	run(seed, "remote", "add", "origin", origin)
	run(seed, "push", "-q", "origin", base)

	worktree = gitTempDir(t)
	run(filepath.Dir(worktree), "clone", "-q", origin, filepath.Base(worktree))
	return worktree, base
}

// writeAndCommit overwrites loop.go in dir with content and commits it.
func writeAndCommit(t *testing.T, dir, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "loop.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitDo(t, dir, "add", "-A")
	gitDo(t, dir, "commit", "-q", "-m", msg)
}

// TestConflictsWithCatchesSameFunctionEditedByBothSides reproduces a hazard
// git's own merge-tree check misses: one side (merged into origin/main first)
// inserts a guard clause directly above a line the other side's branch
// renames. The two edits never touch the same line, so git's merge-tree
// considers the merge clean — but the merge silently keeps only one side's
// edit to reconcile(). ConflictsWith must report a conflict anyway, even
// though the underlying git merge-tree check alone would not.
func TestConflictsWithCatchesSameFunctionEditedByBothSides(t *testing.T) {
	ctx := context.Background()
	wt, base := initGoRepo(t, reconcileBase)

	// origin/main advances first: the guard-clause edit lands there.
	other := gitTempDir(t)
	gitDo(t, filepath.Dir(other), "clone", "-q", mustRemote(t, wt), filepath.Base(other))
	writeAndCommit(t, other, reconcileWithGuard, "145: add guard")
	gitDo(t, other, "push", "-q", "origin", base)

	// The worktree's own branch, diverged from the same base, carries the rename edit.
	writeAndCommit(t, wt, reconcileWithRename, "146: rename call via seam")

	if err := FetchBase(ctx, wt, base); err != nil {
		t.Fatalf("FetchBase: %v", err)
	}

	textConflict, err := gitMergeConflicts(ctx, wt, base, "HEAD")
	if err != nil {
		t.Fatalf("gitMergeConflicts: %v", err)
	}
	if textConflict {
		t.Fatal("test setup should reproduce a textually clean merge (git's own false negative) — got a real conflict instead")
	}

	conflicts, err := ConflictsWith(ctx, wt, base)
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if !conflicts {
		t.Error("ConflictsWith should flag a conflict when both sides edit reconcile(), even though git's own merge-tree reports clean")
	}
}

// TestConflictsWithIgnoresEditsToDifferentFunctions confirms the same-function
// heuristic doesn't over-fire: two branches editing different functions in the
// same file, with no textual conflict, should still report no conflict.
func TestConflictsWithIgnoresEditsToDifferentFunctions(t *testing.T) {
	ctx := context.Background()
	wt, base := initGoRepo(t, reconcileBase)

	other := gitTempDir(t)
	gitDo(t, filepath.Dir(other), "clone", "-q", mustRemote(t, wt), filepath.Base(other))
	writeAndCommit(t, other, reconcileWithGuard, "145: add guard")
	gitDo(t, other, "push", "-q", "origin", base)

	// The worktree's branch only touches unrelated(), never reconcile().
	writeAndCommit(t, wt, reconcileUnrelatedEdit, "unrelated: change return value")

	if err := FetchBase(ctx, wt, base); err != nil {
		t.Fatalf("FetchBase: %v", err)
	}

	conflicts, err := ConflictsWith(ctx, wt, base)
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if conflicts {
		t.Error("ConflictsWith should not flag a conflict for edits to unrelated functions")
	}
}

// TestConflictsWithDetectsUncommittedConflictAtZeroCommitsAhead reproduces
// argus#624: a worktree whose branch has zero commits ahead of base — the
// default state of every finished argus worker, since workers are forbidden
// from committing until ship — but whose uncommitted working tree genuinely
// conflicts with a sibling change that already landed on base. Before
// effectiveHeadRef, ConflictsWith tested HEAD alone, which is identical to
// origin/base here, so it always reported clean.
func TestConflictsWithDetectsUncommittedConflictAtZeroCommitsAhead(t *testing.T) {
	ctx := context.Background()
	wt, base := initGitRepo(t)
	origin := mustRemote(t, wt)

	if n, err := CommitsAheadOfBase(ctx, wt, base); err != nil || n != 0 {
		t.Fatalf("test setup: want zero commits ahead of base right after clone, got %d (err %v)", n, err)
	}

	// A sibling change lands on base first.
	other := gitTempDir(t)
	gitDo(t, filepath.Dir(other), "clone", "-q", origin, filepath.Base(other))
	if err := os.WriteFile(filepath.Join(other, "f.txt"), []byte("origin-change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitDo(t, other, "add", "-A")
	gitDo(t, other, "commit", "-q", "-m", "origin edits line1")
	gitDo(t, other, "push", "-q", "origin", base)

	// wt's own change is left uncommitted, exactly as a finished-but-unshipped
	// argus worker would leave it.
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("branch-change\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := FetchBase(ctx, wt, base); err != nil {
		t.Fatalf("FetchBase: %v", err)
	}
	if n, err := CommitsAheadOfBase(ctx, wt, base); err != nil || n != 0 {
		t.Fatalf("test setup: HEAD must still be zero commits ahead of base (only the working tree changed), got %d (err %v)", n, err)
	}

	conflicts, err := ConflictsWith(ctx, wt, base)
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if !conflicts {
		t.Error("ConflictsWith must detect a conflict from uncommitted working-tree state, even at zero commits ahead of base")
	}
}

// TestConflictsWithCatchesUncommittedSameFunctionEditAtZeroCommitsAhead is
// the uncommitted-worktree analog of
// TestConflictsWithCatchesSameFunctionEditedByBothSides: the worker's own
// edit is never committed, so a HEAD-only check would see wt's HEAD as
// identical to base and never even reach the same-function heuristic.
func TestConflictsWithCatchesUncommittedSameFunctionEditAtZeroCommitsAhead(t *testing.T) {
	ctx := context.Background()
	wt, base := initGoRepo(t, reconcileBase)

	other := gitTempDir(t)
	gitDo(t, filepath.Dir(other), "clone", "-q", mustRemote(t, wt), filepath.Base(other))
	writeAndCommit(t, other, reconcileWithGuard, "145: add guard")
	gitDo(t, other, "push", "-q", "origin", base)

	// The worktree's own rename edit is left uncommitted.
	if err := os.WriteFile(filepath.Join(wt, "loop.go"), []byte(reconcileWithRename), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := FetchBase(ctx, wt, base); err != nil {
		t.Fatalf("FetchBase: %v", err)
	}
	if n, err := CommitsAheadOfBase(ctx, wt, base); err != nil || n != 0 {
		t.Fatalf("test setup: want zero commits ahead of base, got %d (err %v)", n, err)
	}

	textConflict, err := gitMergeConflicts(ctx, wt, base, "HEAD")
	if err != nil {
		t.Fatalf("gitMergeConflicts: %v", err)
	}
	if textConflict {
		t.Fatal("test setup should reproduce a textually clean merge against plain HEAD")
	}

	conflicts, err := ConflictsWith(ctx, wt, base)
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if !conflicts {
		t.Error("ConflictsWith should flag a conflict from the uncommitted same-function edit, even at zero commits ahead of base")
	}
}

// TestConflictsWithCleanZeroCommitsAheadReportsNoConflict confirms a
// worktree with zero commits ahead of base *and* no uncommitted changes —
// the case where "nothing to rebase" is actually true — still reports no
// conflict, so effectiveHeadRef's dirty check doesn't turn every clean
// worker into a false positive.
func TestConflictsWithCleanZeroCommitsAheadReportsNoConflict(t *testing.T) {
	ctx := context.Background()
	wt, base := initGitRepo(t)

	if err := FetchBase(ctx, wt, base); err != nil {
		t.Fatalf("FetchBase: %v", err)
	}

	conflicts, err := ConflictsWith(ctx, wt, base)
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if conflicts {
		t.Error("a genuinely clean worktree with zero commits ahead of base must not report a conflict")
	}
}

// TestEffectiveHeadRefCleanWorktreeReturnsHead confirms the cheap, common
// path: no uncommitted changes means no synthetic commit is built at all.
func TestEffectiveHeadRefCleanWorktreeReturnsHead(t *testing.T) {
	ctx := context.Background()
	wt, _ := initGitRepo(t)

	ref, err := effectiveHeadRef(ctx, wt)
	if err != nil {
		t.Fatalf("effectiveHeadRef: %v", err)
	}
	if ref != "HEAD" {
		t.Errorf("effectiveHeadRef on a clean worktree = %q, want %q", ref, "HEAD")
	}
}

// TestEffectiveHeadRefPropagatesDirtyCheckError confirms a
// hasUncommittedChanges failure (a non-git worktree) is wrapped and returned
// rather than silently treated as clean.
func TestEffectiveHeadRefPropagatesDirtyCheckError(t *testing.T) {
	ctx := context.Background()
	_, err := effectiveHeadRef(ctx, t.TempDir())
	if err == nil {
		t.Fatal("want an error checking uncommitted changes for a non-git worktree")
	}
	if !strings.Contains(err.Error(), "checking worktree for uncommitted changes") {
		t.Errorf("error should wrap the underlying failure, got %v", err)
	}
}

// TestSnapshotWorktreeCommitCapturesUncommittedState confirms the synthetic
// commit's tree actually reflects the working tree's uncommitted edits and
// new untracked files — not just whatever HEAD already had — that its sole
// parent is the worktree's real HEAD, and that building it never disturbs
// worktree's real HEAD or index.
func TestSnapshotWorktreeCommitCapturesUncommittedState(t *testing.T) {
	ctx := context.Background()
	wt, _ := initGitRepo(t)

	head, err := HeadSHA(ctx, wt)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}

	if werr := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("modified\n"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	if werr := os.WriteFile(filepath.Join(wt, "new.txt"), []byte("untracked\n"), 0o644); werr != nil {
		t.Fatal(werr)
	}

	sha, err := snapshotWorktreeCommit(ctx, wt)
	if err != nil {
		t.Fatalf("snapshotWorktreeCommit: %v", err)
	}

	parent, err := git(ctx, wt, "rev-parse", sha+"^")
	if err != nil {
		t.Fatalf("resolving snapshot commit's parent: %v", err)
	}
	if parent != head {
		t.Errorf("snapshot commit's parent = %s, want worktree's real HEAD %s", parent, head)
	}

	for name, want := range map[string]string{"f.txt": "modified", "new.txt": "untracked"} {
		got, gerr := git(ctx, wt, "show", sha+":"+name)
		if gerr != nil {
			t.Fatalf("reading %s from snapshot commit: %v", name, gerr)
		}
		if got != want {
			t.Errorf("%s in snapshot commit = %q, want %q", name, got, want)
		}
	}

	if realHead, herr := HeadSHA(ctx, wt); herr != nil || realHead != head {
		t.Errorf("snapshotWorktreeCommit must not move worktree's real HEAD, got %s want %s (err %v)", realHead, head, herr)
	}
	status, serr := git(ctx, wt, "status", "--porcelain")
	if serr != nil {
		t.Fatalf("git status: %v", serr)
	}
	if status == "" {
		t.Error("worktree's real index/working tree should still show the uncommitted changes after taking a snapshot")
	}
}

// TestSnapshotWorktreeCommitTempFileErrors confirms a failure creating the
// throwaway index file (a TMPDIR pointing nowhere, standing in for any
// os.CreateTemp failure) is wrapped and returned rather than panicking or
// silently producing a bogus commit.
func TestSnapshotWorktreeCommitTempFileErrors(t *testing.T) {
	ctx := context.Background()
	wt, _ := initGitRepo(t)
	if err := os.WriteFile(filepath.Join(wt, "new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "does-not-exist"))

	_, err := snapshotWorktreeCommit(ctx, wt)
	if err == nil {
		t.Fatal("want an error when the throwaway index file can't be created")
	}
	if !strings.Contains(err.Error(), "creating temporary index for worktree snapshot") {
		t.Errorf("error should name the CreateTemp failure, got %v", err)
	}
}

// TestSnapshotWorktreeCommitCommitTreeFailureWrapped confirms a commit-tree
// failure — a fake git stand-in that succeeds through read-tree/add/write-tree
// but fails commit-tree itself, a shape a real git repo can't be coaxed into
// producing on demand — surfaces wrapped with context rather than a bare,
// unchecked commit SHA.
func TestSnapshotWorktreeCommitCommitTreeFailureWrapped(t *testing.T) {
	ctx := context.Background()
	wt, _ := initGitRepo(t)
	if err := os.WriteFile(filepath.Join(wt, "new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	bin := t.TempDir()
	script := filepath.Join(bin, "git")
	content := `#!/bin/sh
case "$*" in
  *"commit-tree "*) echo "commit-tree failed" >&2; exit 1 ;;
  *"write-tree"*) echo "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef" ;;
  *) exit 0 ;;
esac
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("writing fake git: %v", err)
	}
	fakeBinPATH(t, bin)

	_, err := snapshotWorktreeCommit(ctx, wt)
	if err == nil {
		t.Fatal("want an error when commit-tree fails")
	}
	if !strings.Contains(err.Error(), "creating worktree snapshot commit") {
		t.Errorf("error should name the commit-tree failure, got %v", err)
	}
}

// TestSnapshotWorktreeCommitUnbornHeadErrors confirms a repo with no commits
// at all (so HEAD does not resolve) surfaces read-tree's own failure wrapped
// with context, rather than silently producing a bogus commit.
func TestSnapshotWorktreeCommitUnbornHeadErrors(t *testing.T) {
	ctx := context.Background()
	wt := t.TempDir()
	gitInit(t, wt)
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := snapshotWorktreeCommit(ctx, wt)
	if err == nil {
		t.Fatal("want an error snapshotting a worktree with no HEAD commit yet")
	}
	if !strings.Contains(err.Error(), "seeding worktree snapshot index from HEAD") {
		t.Errorf("error should name the read-tree failure, got %v", err)
	}
}

// TestSnapshotWorktreeCommitUnreadableFileErrors confirms a worktree
// containing a file `git add -A` itself can't read (a genuine OS-level
// failure, not merely a git-repo-shape problem) surfaces snapshotGit's own
// wrapped error rather than silently skipping the unreadable file.
func TestSnapshotWorktreeCommitUnreadableFileErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions, so this can't force a read failure")
	}
	ctx := context.Background()
	wt, _ := initGitRepo(t)

	bad := filepath.Join(wt, "unreadable.txt")
	if err := os.WriteFile(bad, []byte("secret\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0o644) })

	_, err := snapshotWorktreeCommit(ctx, wt)
	if err == nil {
		t.Fatal("want an error snapshotting a worktree containing a file git can't read")
	}
	if !strings.Contains(err.Error(), "snapshotting worktree into a temporary index") {
		t.Errorf("error should name the add -A failure, got %v", err)
	}
}

// TestSnapshotGitWrapsStderr confirms snapshotGit's error names the failing
// subcommand and carries its own stderr, not just the exec package's generic
// exit message.
func TestSnapshotGitWrapsStderr(t *testing.T) {
	ctx := context.Background()
	wt := t.TempDir()
	gitInit(t, wt)

	_, err := snapshotGit(ctx, wt, os.Environ(), "rev-parse", "--verify", "does-not-exist")
	if err == nil {
		t.Fatal("want an error resolving a nonexistent ref")
	}
	if !strings.Contains(err.Error(), "git rev-parse:") {
		t.Errorf("error should name the failing subcommand, got %v", err)
	}
}

// TestSnapshotGitFallsBackToGenericErrorWhenStderrEmpty confirms a failing
// subprocess with no stderr output still produces a non-empty wrapped error
// (falling back to err.Error() itself) instead of silently swallowing it.
func TestSnapshotGitFallsBackToGenericErrorWhenStderrEmpty(t *testing.T) {
	ctx := context.Background()
	bin := t.TempDir()
	writeFakeBinary(t, bin, "git", 1) // writes to stdout only, never stderr
	fakeBinPATH(t, bin)

	_, err := snapshotGit(ctx, t.TempDir(), os.Environ(), "write-tree")
	if err == nil {
		t.Fatal("want an error from a failing git stand-in")
	}
}

// mapBase is reconcileBase's generic-func counterpart: before the fix,
// funcNameRe returned "" for a generic func's declaration line (the "[T
// any]" sits between the name and "(", breaking "identifier immediately
// before ("), so parseTouchedFunctions silently dropped every hunk inside
// Map() and the same-function safety net never fired for it.
const mapBase = `package supervisor

func Map[T any](items []T, states []*workerState) []T {
	for _, st := range states {
		if !st.hasFile {
			continue
		}
		measure(st)
	}

	for _, st := range states {
		if !st.hasFile {
			continue
		}
		ok, err := HasPlanEvidence(cfg.Home, st.Worktree)
		if err != nil {
			continue
		}
		st.hasPlanEvidence = ok
	}
	return items
}
`

// mapWithGuard is mapBase's guard-clause half of the same non-overlapping-
// lines edit pair used by reconcileWithGuard.
const mapWithGuard = `package supervisor

func Map[T any](items []T, states []*workerState) []T {
	for _, st := range states {
		if !st.hasFile {
			continue
		}
		measure(st)
	}

	if !usesDefaultLauncher(cfg.Launcher) {
		return items
	}

	for _, st := range states {
		if !st.hasFile {
			continue
		}
		ok, err := HasPlanEvidence(cfg.Home, st.Worktree)
		if err != nil {
			continue
		}
		st.hasPlanEvidence = ok
	}
	return items
}
`

// mapWithRename is mapBase's rename half of the same edit pair.
const mapWithRename = `package supervisor

func Map[T any](items []T, states []*workerState) []T {
	for _, st := range states {
		if !st.hasFile {
			continue
		}
		measure(st)
	}

	for _, st := range states {
		if !st.hasFile {
			continue
		}
		ok, err := defaultAgent.PlanEvidence(cfg.Home, st.Worktree)
		if err != nil {
			continue
		}
		st.hasPlanEvidence = ok
	}
	return items
}
`

// TestConflictsWithCatchesGenericFuncEditedByBothSides is the generic-func
// analog of TestConflictsWithCatchesSameFunctionEditedByBothSides: it
// reproduces issue #493's repro exactly, confirming the fix actually closes
// the hole rather than just satisfying the unit-level table test above.
func TestConflictsWithCatchesGenericFuncEditedByBothSides(t *testing.T) {
	ctx := context.Background()
	wt, base := initGoRepo(t, mapBase)

	other := gitTempDir(t)
	gitDo(t, filepath.Dir(other), "clone", "-q", mustRemote(t, wt), filepath.Base(other))
	writeAndCommit(t, other, mapWithGuard, "add guard")
	gitDo(t, other, "push", "-q", "origin", base)

	writeAndCommit(t, wt, mapWithRename, "rename call via seam")

	if err := FetchBase(ctx, wt, base); err != nil {
		t.Fatalf("FetchBase: %v", err)
	}

	textConflict, err := gitMergeConflicts(ctx, wt, base, "HEAD")
	if err != nil {
		t.Fatalf("gitMergeConflicts: %v", err)
	}
	if textConflict {
		t.Fatal("test setup should reproduce a textually clean merge (git's own false negative) — got a real conflict instead")
	}

	conflicts, err := ConflictsWith(ctx, wt, base)
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if !conflicts {
		t.Error("ConflictsWith should flag a conflict when both sides edit a generic func's body, even though git's own merge-tree reports clean")
	}
}

func mustRemote(t *testing.T, worktree string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", worktree, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("remote get-url: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestVerifyPushLandedDetectsMismatchThenConfirmsAfterPush confirms a
// worktree whose local HEAD has advanced (the rebase) but never reached
// origin must fail VerifyPushLanded, and only pass once ForcePushBranch
// actually lands it — checking only local state, never origin, is exactly
// what let a rebase get reported as successful without ever landing.
func TestVerifyPushLandedDetectsMismatchThenConfirmsAfterPush(t *testing.T) {
	ctx := context.Background()
	wt, base := initGitRepo(t) // clone checked out on base ("main"), origin/main == local HEAD

	if err := VerifyPushLanded(ctx, wt, base); err != nil {
		t.Fatalf("VerifyPushLanded should pass right after a clone (origin already matches local HEAD): %v", err)
	}

	// Advance local HEAD without pushing — the "rebased locally, push never
	// landed" scenario from the bug report.
	writeAndCommit(t, wt, "line1\nline2\n", "local-only commit")

	if err := VerifyPushLanded(ctx, wt, base); err == nil {
		t.Fatal("want VerifyPushLanded to fail once local HEAD has diverged from origin without a push")
	}

	if err := ForcePushBranch(ctx, wt, base); err != nil {
		t.Fatalf("ForcePushBranch: %v", err)
	}
	if err := VerifyPushLanded(ctx, wt, base); err != nil {
		t.Errorf("VerifyPushLanded should pass once ForcePushBranch actually landed the commit: %v", err)
	}
}

// TestRemoteBranchSHAMissingBranchReturnsEmpty confirms a branch origin has
// never seen at all is reported as "" rather than an error — a caller needs
// to tell "not on origin yet" apart from "on origin at a different commit".
func TestRemoteBranchSHAMissingBranchReturnsEmpty(t *testing.T) {
	ctx := context.Background()
	wt, _ := initGitRepo(t)
	gitDo(t, wt, "checkout", "-q", "-b", "never-pushed")

	sha, err := RemoteBranchSHA(ctx, wt, "never-pushed")
	if err != nil {
		t.Fatalf("RemoteBranchSHA: %v", err)
	}
	if sha != "" {
		t.Errorf("want empty SHA for a branch never pushed to origin, got %q", sha)
	}
}

// TestCommitsAheadOfBaseCountsLocalOnlyCommits confirms CommitsAheadOfBase
// reports zero right after a clone (HEAD == origin/<base>, nothing of its
// own to publish) and counts up as local-only commits are added — the
// distinction a rebase dispatch needs to tell "this branch never diverged"
// apart from "this branch has history that must reach origin".
func TestCommitsAheadOfBaseCountsLocalOnlyCommits(t *testing.T) {
	ctx := context.Background()
	wt, base := initGitRepo(t)

	n, err := CommitsAheadOfBase(ctx, wt, base)
	if err != nil {
		t.Fatalf("CommitsAheadOfBase: %v", err)
	}
	if n != 0 {
		t.Errorf("commits ahead right after a clone = %d, want 0", n)
	}

	writeAndCommit2 := func(content, msg string) {
		if werr := os.WriteFile(filepath.Join(wt, "f.txt"), []byte(content), 0o644); werr != nil {
			t.Fatal(werr)
		}
		gitDo(t, wt, "add", "-A")
		gitDo(t, wt, "commit", "-q", "-m", msg)
	}
	writeAndCommit2("line1\nline2\n", "one local commit")

	n, err = CommitsAheadOfBase(ctx, wt, base)
	if err != nil {
		t.Fatalf("CommitsAheadOfBase after one commit: %v", err)
	}
	if n != 1 {
		t.Errorf("commits ahead after one local commit = %d, want 1", n)
	}
}

// TestCommitsAheadOfBaseSurvivesOriginAdvancingPastLocalFetch reproduces the
// hazard a live ls-remote resolution would hit: base gets a new commit on
// origin — plausible mid-dispatch, since sibling rebases are exactly why
// this command exists — after the worktree's own local origin/<base> ref was
// last refreshed, but before CommitsAheadOfBase runs. Because it counts
// against the local ref rather than re-querying origin, the object it
// compares against is guaranteed to already be in the local object DB, so
// this must succeed rather than fail with "unknown revision".
func TestCommitsAheadOfBaseSurvivesOriginAdvancingPastLocalFetch(t *testing.T) {
	ctx := context.Background()
	wt, base := initGitRepo(t)
	origin := mustRemote(t, wt)

	// A sibling clone advances origin/main past what wt's local origin/main
	// ref (populated by the initial clone) still points at.
	other := gitTempDir(t)
	gitDo(t, filepath.Dir(other), "clone", "-q", origin, filepath.Base(other))
	if werr := os.WriteFile(filepath.Join(other, "sibling.txt"), []byte("from-sibling\n"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	gitDo(t, other, "add", "-A")
	gitDo(t, other, "commit", "-q", "-m", "sibling advances main")
	gitDo(t, other, "push", "-q", "origin", base)

	n, err := CommitsAheadOfBase(ctx, wt, base)
	if err != nil {
		t.Fatalf("CommitsAheadOfBase must not fail when origin has advanced past the local fetch: %v", err)
	}
	if n != 0 {
		t.Errorf("commits ahead = %d, want 0 (wt's own HEAD never moved)", n)
	}
}

// TestRebaseBriefCarriesRebaseSteps confirms the brief tells the worker to
// merge (not `git rebase`, whose --continue would commit each replayed
// commit itself) with --no-commit, and never to run git commit or git push —
// argus does both once the worker reports, mirroring a normal dispatch.
func TestRebaseBriefCarriesRebaseSteps(t *testing.T) {
	b := RebaseBrief("feat-x", "main")
	for _, want := range []string{"feat-x", "git merge origin/main --no-commit", "Do NOT run git commit or git push yourself", protocol.WriterBrief("origin/main")} {
		if !strings.Contains(b, want) {
			t.Errorf("rebase brief missing %q:\n%s", want, b)
		}
	}
	for _, unwanted := range []string{"git rebase origin/main", "--force-with-lease"} {
		if strings.Contains(b, unwanted) {
			t.Errorf("rebase brief must not mandate %q — argus commits and pushes itself:\n%s", unwanted, b)
		}
	}
}

// TestRebaseBriefDoesNotInstructGitAdd guards a real regression: git add is
// not in the structural floor's allow set under the dontAsk permission
// model (only read-only git plumbing is), so a brief instructing a
// rebase-dispatched worker to run it would be denied by Claude Code's own
// permission engine. ship's CommitAll stages everything itself (`git add
// -A`) before committing, so the worker never needs to stage anything.
func TestRebaseBriefDoesNotInstructGitAdd(t *testing.T) {
	b := RebaseBrief("feat-x", "main")
	if strings.Contains(b, "git add") {
		t.Errorf("rebase brief instructs git add, which the worker's dontAsk permission set does not grant:\n%s", b)
	}
	if !strings.Contains(b, "denies both outright") {
		t.Errorf("rebase brief should describe commit/push as denied (dontAsk), not merely ask-gated:\n%s", b)
	}
}

// TestRebasePhaseAllowMatchesBriefCommands is the regression test for the
// dontAsk-era rebase deadlock: the rebase phase's own resolved allow set
// (the structural floor plus RebasePhaseAllow, exactly as
// cmd/worker_check_tool.go's runWorkerCheckTool folds it into extraAllow —
// see loadCurrentPhase) must cover every git command RebaseBrief actually
// instructs, plus the repo's configured verify command when set, and must
// never widen far enough to cover git commit/push. Unlike the blanket
// extraAllow injection this replaced, the grant only needs to be checked
// against the rebase phase now — every other phase simply never receives it
// (see TestResolvedAllowForPhase_RebaseGetsGitFetchMergeAndVerify).
func TestRebasePhaseAllowMatchesBriefCommands(t *testing.T) {
	base := "main"
	brief := RebaseBrief("feat-x", base)
	extra := RebasePhaseAllow(base, "make ci", "")

	if !slices.Contains(extra, "Bash(make ci)") {
		t.Fatalf("RebasePhaseAllow with shipVerifyCommand set should grant it: %v", extra)
	}

	for _, cmd := range rebaseGitCommands(base) {
		if !strings.Contains(brief, cmd) {
			t.Fatalf("test setup: RebaseBrief does not actually instruct %q:\n%s", cmd, brief)
		}
		allow := ResolvedAllowForPhase(protocol.PhaseRebase, "/tmp/wt", nil, nil, extra)
		if !AllowCoversCommand(allow, cmd) {
			t.Errorf("resolved rebase allow set does not cover rebase-brief command %q: %v", cmd, allow)
		}
	}

	for _, phase := range allReportedAndRebasePhases {
		allow := ResolvedAllowForPhase(phase, "/tmp/wt", nil, nil, extra)
		for _, denied := range []string{"git commit -m x", "git push origin feat-x"} {
			if AllowCoversCommand(allow, denied) {
				t.Errorf("phase %q: RebasePhaseAllow must never cover %q, got allow %v", phase, denied, allow)
			}
		}
	}
}

// TestRebasePhaseAllowFallsBackToGateVerifyCommand confirms a repo that only
// sets gate_verify_command (not ship.verify_command) still gets it granted —
// either configured command confirms the merge, so either is honored.
func TestRebasePhaseAllowFallsBackToGateVerifyCommand(t *testing.T) {
	got := RebasePhaseAllow("main", "", "make test && make lint")
	if !slices.Contains(got, "Bash(make test && make lint)") {
		t.Errorf("RebasePhaseAllow should grant gateVerifyCommand when shipVerifyCommand is unset: %v", got)
	}
}

// TestRebasePhaseAllowNoVerifyCommandConfigured confirms a repo with neither
// verify command configured still gets exactly the two git commands — no
// stray empty-string Bash entry.
func TestRebasePhaseAllowNoVerifyCommandConfigured(t *testing.T) {
	got := RebasePhaseAllow("main", "", "")
	want := []string{"Bash(git fetch origin main)", "Bash(git merge origin/main --no-commit)"}
	if !slices.Equal(got, want) {
		t.Errorf("RebasePhaseAllow with no verify command = %v, want %v", got, want)
	}
}

// TestGrantRebaseAllowPatchesRenderedSettings confirms GrantRebaseAllow adds
// RebasePhaseAllow's git fetch/merge and verify-command entries to an
// already-rendered settings.local.json — the dispatch-time defense-in-depth
// for a base differing from the worktree's spawn-time base (see its own doc
// comment).
func TestGrantRebaseAllowPatchesRenderedSettings(t *testing.T) {
	wt := t.TempDir()
	if err := WriteSettings(wt, nil, nil, nil, nil, false, nil); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}
	settingsPath := filepath.Join(wt, ".claude", "settings.local.json")

	before, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("reading rendered settings: %v", err)
	}
	for _, cmd := range rebaseGitCommands("main") {
		if strings.Contains(string(before), cmd) {
			t.Fatalf("test setup: rendered settings already cover %q before GrantRebaseAllow ran", cmd)
		}
	}

	if gerr := GrantRebaseAllow(wt, "main", "make ci", ""); gerr != nil {
		t.Fatalf("GrantRebaseAllow: %v", gerr)
	}

	after, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("reading patched settings: %v", err)
	}
	var settings permissionSettings
	if err := json.Unmarshal(after, &settings); err != nil {
		t.Fatalf("parsing patched settings: %v", err)
	}
	for _, want := range []string{"Bash(git fetch origin main)", "Bash(git merge origin/main --no-commit)", "Bash(make ci)"} {
		if !slices.Contains(settings.Permissions.Allow, want) {
			t.Errorf("patched settings allow missing %q: %v", want, settings.Permissions.Allow)
		}
	}
}

// TestGrantRebaseAllowIdempotent confirms a retried dispatch's second grant
// doesn't duplicate the entries.
func TestGrantRebaseAllowIdempotent(t *testing.T) {
	wt := t.TempDir()
	if err := WriteSettings(wt, nil, nil, nil, nil, false, nil); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}
	if err := GrantRebaseAllow(wt, "main", "", ""); err != nil {
		t.Fatalf("first GrantRebaseAllow: %v", err)
	}
	if err := GrantRebaseAllow(wt, "main", "", ""); err != nil {
		t.Fatalf("second GrantRebaseAllow: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(wt, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatalf("reading patched settings: %v", err)
	}
	var settings permissionSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("parsing patched settings: %v", err)
	}
	count := 0
	for _, a := range settings.Permissions.Allow {
		if a == "Bash(git fetch origin main)" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("want the git fetch entry exactly once after two grants, got %d in %v", count, settings.Permissions.Allow)
	}
}

// TestGrantRebaseAllowMissingSettingsFileNoop confirms a worktree never
// provisioned through argus's normal spawn path (no settings.local.json at
// all) is left alone rather than erroring: under dontAsk it can't run any
// Bash command regardless of this grant, so there is nothing to patch.
func TestGrantRebaseAllowMissingSettingsFileNoop(t *testing.T) {
	if err := GrantRebaseAllow(t.TempDir(), "main", "", ""); err != nil {
		t.Errorf("GrantRebaseAllow on a worktree with no settings.local.json should no-op, got %v", err)
	}
}

// TestGrantRebaseAllowCorruptSettingsFileErrors confirms a settings.local.json
// that exists but fails to parse surfaces as a real error rather than
// silently leaving the worker's grant unpatched.
func TestGrantRebaseAllowCorruptSettingsFileErrors(t *testing.T) {
	wt := t.TempDir()
	settingsPath := filepath.Join(wt, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	if err := os.WriteFile(settingsPath, []byte("not json"), 0o644); err != nil {
		t.Fatalf("seeding corrupt settings: %v", err)
	}

	err := GrantRebaseAllow(wt, "main", "", "")
	if err == nil {
		t.Fatal("want an error for a corrupt settings.local.json, got nil")
	}
	if !strings.Contains(err.Error(), "parsing settings") {
		t.Errorf("error should be wrapped with context, got: %v", err)
	}
}

// TestGrantRebaseAllowStripsDenyFloorOverlappingVerifyCommand is the
// regression test for the review finding that GrantRebaseAllow used to skip
// stripDenyFloor: a misconfigured gate_verify_command (or ship_verify_command)
// whose prefix overlaps a DenyFloor family — here, one starting with "git
// push" — must never end up granted in the patched static settings.local.json,
// the same backstop ResolvedAllowSet/ResolvedAllowForPhase already apply to
// every other allow source. Without the fix, this exact string would have
// been written unfiltered, reachable through the check-tool hook's own
// fail-open path (a corrupt/unreadable status.json) as well as any dontAsk
// call the static file itself gates.
func TestGrantRebaseAllowStripsDenyFloorOverlappingVerifyCommand(t *testing.T) {
	wt := t.TempDir()
	if err := WriteSettings(wt, nil, nil, nil, nil, false, nil); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}

	maliciousVerify := "git push origin main --force"
	if err := GrantRebaseAllow(wt, "main", "", maliciousVerify); err != nil {
		t.Fatalf("GrantRebaseAllow: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(wt, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatalf("reading patched settings: %v", err)
	}
	var settings permissionSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("parsing patched settings: %v", err)
	}

	overlapping := "Bash(" + maliciousVerify + ")"
	if slices.Contains(settings.Permissions.Allow, overlapping) {
		t.Errorf("patched settings allow must not carry a DenyFloor-overlapping verify command %q: %v", overlapping, settings.Permissions.Allow)
	}
	// The legitimate git fetch/merge grant for base must still land — only
	// the DenyFloor-overlapping verify entry should be stripped.
	for _, want := range []string{"Bash(git fetch origin main)", "Bash(git merge origin/main --no-commit)"} {
		if !slices.Contains(settings.Permissions.Allow, want) {
			t.Errorf("patched settings allow missing legitimate grant %q: %v", want, settings.Permissions.Allow)
		}
	}
}

// TestGitMergeConflictsNonConflictErrorPropagates confirms a merge-tree
// failure unrelated to a real conflict (any exit code other than 1) surfaces
// as an error, not a false "conflicts" result — a non-git worktree makes
// merge-tree fail this way without needing a real repo at all.
func TestGitMergeConflictsNonConflictErrorPropagates(t *testing.T) {
	ctx := context.Background()
	notGit := t.TempDir()
	conflicts, err := gitMergeConflicts(ctx, notGit, "main", "HEAD")
	if err == nil {
		t.Fatal("want an error from merge-tree against a non-git worktree")
	}
	if conflicts {
		t.Error("a merge-tree failure unrelated to conflicts must not be reported as a conflict")
	}
	if !strings.Contains(err.Error(), "checking merge conflicts") {
		t.Errorf("error should wrap the merge-tree failure, got %v", err)
	}
}

// TestConflictsWithPropagatesMergeTreeError confirms ConflictsWith propagates
// an error from its own preliminary checks (here, effectiveHeadRef's
// hasUncommittedChanges call against a non-git worktree) rather than
// swallowing it into a plain false.
func TestConflictsWithPropagatesMergeTreeError(t *testing.T) {
	ctx := context.Background()
	notGit := t.TempDir()
	conflicts, err := ConflictsWith(ctx, notGit, "main")
	if err == nil {
		t.Fatal("want ConflictsWith to propagate the underlying error")
	}
	if conflicts {
		t.Error("an error path must not also report a conflict")
	}
}

// TestSameFunctionTouchedByBothMergeBaseErrorPropagates confirms a
// merge-base failure (e.g. a non-git worktree) is wrapped and returned rather
// than treated as "no conflict".
func TestSameFunctionTouchedByBothMergeBaseErrorPropagates(t *testing.T) {
	ctx := context.Background()
	notGit := t.TempDir()
	_, err := sameFunctionTouchedByBoth(ctx, notGit, "main", "HEAD")
	if err == nil {
		t.Fatal("want an error resolving merge-base against a non-git worktree")
	}
	if !strings.Contains(err.Error(), "resolving merge base with origin/main") {
		t.Errorf("error should name what failed, got %v", err)
	}
}

// TestTouchedFunctionsDiffErrorWrapped confirms a `git diff` failure is
// wrapped with the from..to range rather than returned bare.
func TestTouchedFunctionsDiffErrorWrapped(t *testing.T) {
	ctx := context.Background()
	notGit := t.TempDir()
	_, err := touchedFunctions(ctx, notGit, "abc", "HEAD")
	if err == nil {
		t.Fatal("want an error diffing in a non-git worktree")
	}
	if !strings.Contains(err.Error(), "diffing abc..HEAD") {
		t.Errorf("error should name the diff range, got %v", err)
	}
}

const funcAOriginal = `package supervisor

func FuncA() int {
	return 1
}
`

const funcAEdited = `package supervisor

func FuncA() int {
	return 2
}
`

const funcBOriginal = `package supervisor

func FuncB() int {
	return 1
}
`

const funcBEdited = `package supervisor

func FuncB() int {
	return 2
}
`

// initMultiFileRepo is initGoRepo's multi-file counterpart: a bare origin
// plus a clone, seeded with every entry in files, so a test can diverge each
// side onto a different file entirely.
func initMultiFileRepo(t *testing.T, files map[string]string) (worktree, base string) {
	t.Helper()
	base = "main"
	origin := gitTempDir(t)
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}

	run(origin, "init", "-q", "--bare", "-b", base, ".")
	seed := gitTempDir(t)
	run(seed, "init", "-q", "-b", base, ".")
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(seed, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run(seed, "add", "-A")
	run(seed, "commit", "-q", "-m", "seed")
	run(seed, "remote", "add", "origin", origin)
	run(seed, "push", "-q", "origin", base)

	worktree = gitTempDir(t)
	run(filepath.Dir(worktree), "clone", "-q", origin, filepath.Base(worktree))
	return worktree, base
}

// writeFileAndCommit overwrites name in dir with content and commits it.
func writeFileAndCommit(t *testing.T, dir, name, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitDo(t, dir, "add", "-A")
	gitDo(t, dir, "commit", "-q", "-m", msg)
}

// TestSameFunctionTouchedByBothIgnoresFileOnlyOnOneSide confirms a file that
// only one side's diff touches at all — not merely a different function
// within a shared file — takes the "theirs has no entry for this file" path
// and never reaches the per-function comparison.
func TestSameFunctionTouchedByBothIgnoresFileOnlyOnOneSide(t *testing.T) {
	ctx := context.Background()
	wt, base := initMultiFileRepo(t, map[string]string{"a.go": funcAOriginal, "b.go": funcBOriginal})

	other := gitTempDir(t)
	gitDo(t, filepath.Dir(other), "clone", "-q", mustRemote(t, wt), filepath.Base(other))
	writeFileAndCommit(t, other, "b.go", funcBEdited, "edit FuncB")
	gitDo(t, other, "push", "-q", "origin", base)

	writeFileAndCommit(t, wt, "a.go", funcAEdited, "edit FuncA")

	if err := FetchBase(ctx, wt, base); err != nil {
		t.Fatalf("FetchBase: %v", err)
	}

	conflicts, err := sameFunctionTouchedByBoth(ctx, wt, base, "HEAD")
	if err != nil {
		t.Fatalf("sameFunctionTouchedByBoth: %v", err)
	}
	if conflicts {
		t.Error("edits to entirely different files should not conflict, even before comparing per-function")
	}
}

// TestParseTouchedFunctionsSkipsHunkBeforeFilename confirms a "@@" hunk
// header that appears before any "+++ " line — so file is still "" — is
// skipped rather than recorded under a phantom empty-string file key.
func TestParseTouchedFunctionsSkipsHunkBeforeFilename(t *testing.T) {
	diff := "@@ -1,1 +1,1 @@ func Orphan() {\n-old\n+new\n"
	got := parseTouchedFunctions(diff)
	if len(got) != 0 {
		t.Errorf("a hunk with no preceding +++ line should be skipped entirely, got %+v", got)
	}
}

// TestRemoteBranchSHATransportErrorWrapped confirms an ls-remote transport
// failure (a non-git worktree stands in for any origin-unreachable case) is
// wrapped with the branch it was querying for, not returned bare.
func TestRemoteBranchSHATransportErrorWrapped(t *testing.T) {
	ctx := context.Background()
	notGit := t.TempDir()
	_, err := RemoteBranchSHA(ctx, notGit, "main")
	if err == nil {
		t.Fatal("want an error querying a non-git worktree")
	}
	if !strings.Contains(err.Error(), "querying origin for main") {
		t.Errorf("error should wrap with branch context, got %v", err)
	}
}

// TestCommitsAheadOfBaseRevListErrorWrapped confirms a rev-list failure (a
// non-git worktree) is wrapped with the base it was counting against.
func TestCommitsAheadOfBaseRevListErrorWrapped(t *testing.T) {
	ctx := context.Background()
	notGit := t.TempDir()
	_, err := CommitsAheadOfBase(ctx, notGit, "main")
	if err == nil {
		t.Fatal("want an error counting commits in a non-git worktree")
	}
	if !strings.Contains(err.Error(), "counting commits ahead of origin/main") {
		t.Errorf("error should wrap with base context, got %v", err)
	}
}

// TestCommitsAheadOfBaseNonNumericParseErrorWrapped confirms a git stdout
// that isn't a plain integer (never happens with real `rev-list --count`, so
// a fake git stand-in is needed to reach this branch at all) reports a parse
// error rather than a bogus count.
func TestCommitsAheadOfBaseNonNumericParseErrorWrapped(t *testing.T) {
	ctx := context.Background()
	bin := t.TempDir()
	writeFakeBinary(t, bin, "git", 0)
	fakeBinPATH(t, bin)

	_, err := CommitsAheadOfBase(ctx, t.TempDir(), "main")
	if err == nil {
		t.Fatal("want a parse error when git's own output isn't numeric")
	}
	if !strings.Contains(err.Error(), "parsing commit count") {
		t.Errorf("error should name the parse failure, got %v", err)
	}
}

// TestPushAndVerifyHappyPath confirms the shared push-then-verify pair
// succeeds end to end once a real commit actually lands on origin.
func TestPushAndVerifyHappyPath(t *testing.T) {
	ctx := context.Background()
	wt, base := initGitRepo(t)
	writeAndCommit(t, wt, "line1\nline2\n", "advance")

	if err := PushAndVerify(ctx, wt, base); err != nil {
		t.Fatalf("PushAndVerify: %v", err)
	}
}

// TestPushAndVerifyForcePushErrorWrapped confirms a ForcePushBranch failure
// (no origin remote configured) is wrapped with branch context rather than
// returned bare.
func TestPushAndVerifyForcePushErrorWrapped(t *testing.T) {
	ctx := context.Background()
	wt := t.TempDir()
	run := gitInit(t, wt)
	run("commit", "-q", "--allow-empty", "-m", "init")

	err := PushAndVerify(ctx, wt, "feat-x")
	if err == nil {
		t.Fatal("want an error pushing from a worktree with no origin remote")
	}
	if !strings.Contains(err.Error(), "pushing feat-x to origin") {
		t.Errorf("error should wrap with branch context, got %v", err)
	}
}

// writeFakeGitDispatch drops an executable "git" stand-in into dir that
// unconditionally exits 0 for a push (simulating one that reports success
// without actually landing) while returning divergent SHAs for rev-parse
// HEAD and ls-remote — the exact shape PushAndVerify's own VerifyPushLanded
// call must catch, and one no real git repo can be coaxed into producing
// since a real push that exits 0 always does land.
func writeFakeGitDispatch(t *testing.T, dir string) {
	t.Helper()
	script := filepath.Join(dir, "git")
	content := `#!/bin/sh
case "$*" in
  *"push "*) exit 0 ;;
  *"rev-parse HEAD"*) echo aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ;;
  *"ls-remote "*) printf 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\trefs/heads/feat\n' ;;
esac
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("writing fake git dispatch: %v", err)
	}
}

// TestPushAndVerifyCatchesUnlandedPush confirms PushAndVerify surfaces
// VerifyPushLanded's own mismatch error when the push subprocess exits 0 but
// origin never actually moved.
func TestPushAndVerifyCatchesUnlandedPush(t *testing.T) {
	ctx := context.Background()
	bin := t.TempDir()
	writeFakeGitDispatch(t, bin)
	fakeBinPATH(t, bin)

	err := PushAndVerify(ctx, t.TempDir(), "feat")
	if err == nil {
		t.Fatal("want VerifyPushLanded to catch a push that reported success but did not land")
	}
	if !strings.Contains(err.Error(), "did not land") {
		t.Errorf("error should surface VerifyPushLanded's mismatch, got %v", err)
	}
}

// TestVerifyPushLandedHeadSHAErrorWrapped confirms a HeadSHA failure (a
// non-git worktree) is wrapped rather than returned bare.
func TestVerifyPushLandedHeadSHAErrorWrapped(t *testing.T) {
	ctx := context.Background()
	notGit := t.TempDir()
	err := VerifyPushLanded(ctx, notGit, "main")
	if err == nil {
		t.Fatal("want an error resolving local HEAD in a non-git worktree")
	}
	if !strings.Contains(err.Error(), "resolving local HEAD") {
		t.Errorf("error should wrap the HeadSHA failure, got %v", err)
	}
}

// TestVerifyPushLandedRemoteBranchSHAErrorPropagates confirms a
// RemoteBranchSHA failure (no origin remote configured, so HeadSHA succeeds
// but the origin query can't) propagates VerifyPushLanded's own error
// unwrapped further — RemoteBranchSHA already carries branch context.
func TestVerifyPushLandedRemoteBranchSHAErrorPropagates(t *testing.T) {
	ctx := context.Background()
	wt := t.TempDir()
	run := gitInit(t, wt)
	run("commit", "-q", "--allow-empty", "-m", "init")

	err := VerifyPushLanded(ctx, wt, "main")
	if err == nil {
		t.Fatal("want an error querying origin when no origin remote is configured")
	}
	if !strings.Contains(err.Error(), "querying origin for main") {
		t.Errorf("VerifyPushLanded should propagate RemoteBranchSHA's own wrapped error, got %v", err)
	}
}

// TestInvalidateStatusNonNotExistErrorWrapped confirms a removal failure
// other than "file doesn't exist" (a non-empty directory sitting at the
// status path, so os.Remove refuses regardless of the caller's permissions)
// is reported rather than swallowed like the ErrNotExist case.
func TestInvalidateStatusNonNotExistErrorWrapped(t *testing.T) {
	wt := t.TempDir()
	statusPath := protocol.StatusPath(wt)
	if err := os.MkdirAll(statusPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statusPath, "child"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := InvalidateStatus(wt)
	if err == nil {
		t.Fatal("want an error when the status path is a non-empty directory os.Remove can't delete")
	}
	if !strings.Contains(err.Error(), "removing") {
		t.Errorf("error should wrap the removal failure, got %v", err)
	}
}

// TestIsStaleZeroSinceReturnsFalse confirms a zero since (no dispatch time to
// compare against) never marks a file stale, regardless of its mtime.
func TestIsStaleZeroSinceReturnsFalse(t *testing.T) {
	if isStale(filepath.Join(t.TempDir(), "missing"), time.Time{}) {
		t.Error("a zero since should never be treated as making a file stale")
	}
}

// TestIsStaleStatErrorReturnsFalse confirms a path that can't be stat'd
// (e.g. no file written yet) is treated as not stale — the caller's own
// handling of a missing/unreadable status file decides that case instead.
func TestIsStaleStatErrorReturnsFalse(t *testing.T) {
	if isStale(filepath.Join(t.TempDir(), "does-not-exist"), time.Now()) {
		t.Error("a path that can't be stat'd should not be treated as stale")
	}
}

// setupConflictedMerge builds a worktree mid a real, genuinely conflicting
// `git merge --no-commit` (not just ConflictsWith's own merge-tree check,
// which never touches the working tree) — MERGE_HEAD is left set exactly as
// a rebase worker blocked before finishing conflict resolution (or any run
// that ends mid-conflict) would leave it for the next dispatch to find.
func setupConflictedMerge(t *testing.T) (worktree, base string) {
	t.Helper()
	ctx := context.Background()
	wt, base := initGitRepo(t)
	origin := mustRemote(t, wt)
	other := gitTempDir(t)
	gitDo(t, filepath.Dir(other), "clone", "-q", origin, filepath.Base(other))
	if err := os.WriteFile(filepath.Join(other, "f.txt"), []byte("origin-change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitDo(t, other, "add", "-A")
	gitDo(t, other, "commit", "-q", "-m", "origin edits line1")
	gitDo(t, other, "push", "-q", "origin", base)
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("branch-change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitDo(t, wt, "add", "-A")
	gitDo(t, wt, "commit", "-q", "-m", "branch edits line1")
	if err := FetchBase(ctx, wt, base); err != nil {
		t.Fatalf("FetchBase: %v", err)
	}

	// A real conflicting merge exits non-zero by design (that's what leaves
	// MERGE_HEAD set for the worker to resolve) — the non-zero exit here is
	// the expected outcome, not a test failure, so this bypasses gitDo's own
	// t.Fatalf-on-error wrapper. The identity env matches gitDo/initGitRepo:
	// without it, a CI runner with no configured git identity makes the merge
	// fail on "Committer identity unknown" before it ever writes MERGE_HEAD,
	// which reads as the expected conflict here but isn't one.
	mergeCmd := exec.CommandContext(ctx, "git", "-C", wt, "merge", "origin/"+base, "--no-commit")
	mergeCmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if mergeErr := mergeCmd.Run(); mergeErr == nil {
		t.Fatal("test setup: expected the merge to conflict, but it succeeded cleanly")
	}
	return wt, base
}

// TestMergeInProgressAndAbort confirms mergeInProgress sees a real
// conflicted merge and AbortInProgressMerge clears it back out.
func TestMergeInProgressAndAbort(t *testing.T) {
	ctx := context.Background()
	wt, _ := setupConflictedMerge(t)

	during, err := mergeInProgress(ctx, wt)
	if err != nil {
		t.Fatalf("mergeInProgress during conflict: %v", err)
	}
	if !during {
		t.Fatal("expected MERGE_HEAD to be set after a conflicting merge attempt")
	}

	if abortErr := AbortInProgressMerge(ctx, wt); abortErr != nil {
		t.Fatalf("AbortInProgressMerge: %v", abortErr)
	}

	after, err := mergeInProgress(ctx, wt)
	if err != nil {
		t.Fatalf("mergeInProgress after abort: %v", err)
	}
	if after {
		t.Error("expected AbortInProgressMerge to clear MERGE_HEAD")
	}
}

// TestAbortInProgressMergePropagatesMergeInProgressError confirms
// AbortInProgressMerge surfaces mergeInProgress's own error rather than
// masking it — the branch TestMergeInProgressNotGitRepoErrorWrapped exercises
// directly against mergeInProgress, exercised here through the caller that
// actually matters (dispatchRebaseWorker only ever calls AbortInProgressMerge,
// never mergeInProgress itself).
func TestAbortInProgressMergePropagatesMergeInProgressError(t *testing.T) {
	err := AbortInProgressMerge(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("expected an error for a non-git worktree")
	}
	if !strings.Contains(err.Error(), "resolving git dir") {
		t.Errorf("error should propagate mergeInProgress's own wrapped error, got %v", err)
	}
}

// TestAbortInProgressMergePropagatesAbortFailure forces `git merge --abort`
// itself to fail (not merely mergeInProgress's own check) by deleting the
// index file it needs to reset — confirmed empirically to make the abort
// fail deterministically ("Could not reset index file to revision 'HEAD'"),
// unlike a HEAD/ORIG_HEAD content corruption (git still resolves those via
// other state) or a permission-based trick (a no-op under a root test
// runner).
func TestAbortInProgressMergePropagatesAbortFailure(t *testing.T) {
	wt, _ := setupConflictedMerge(t)
	if err := os.Remove(filepath.Join(wt, ".git", "index")); err != nil {
		t.Fatalf("removing index: %v", err)
	}

	err := AbortInProgressMerge(context.Background(), wt)
	if err == nil {
		t.Fatal("expected AbortInProgressMerge to fail when git merge --abort can't reset the index")
	}
	if !strings.Contains(err.Error(), "aborting in-progress merge before rebase dispatch") {
		t.Errorf("error should wrap the git merge --abort failure, got %v", err)
	}
}

// TestAbortInProgressMergeNoopWhenNoneInProgress confirms a clean worktree
// (the common case — most dispatches aren't retries into a stuck merge) is
// left alone rather than erroring on "nothing to abort".
func TestAbortInProgressMergeNoopWhenNoneInProgress(t *testing.T) {
	wt, _ := initGitRepo(t)
	if err := AbortInProgressMerge(context.Background(), wt); err != nil {
		t.Fatalf("AbortInProgressMerge on a clean worktree: %v", err)
	}
}

// TestMergeInProgressNotGitRepoErrorWrapped confirms a directory that isn't
// a git repository at all surfaces a wrapped error rather than silently
// reporting no merge in progress — that would let AbortInProgressMerge fail
// open on a genuinely broken --worktree instead of surfacing it.
func TestMergeInProgressNotGitRepoErrorWrapped(t *testing.T) {
	_, err := mergeInProgress(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("expected an error resolving git dir for a non-git directory")
	}
	if !strings.Contains(err.Error(), "resolving git dir") {
		t.Errorf("error should wrap the git-dir resolution failure, got %v", err)
	}
}
