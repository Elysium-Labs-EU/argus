package supervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

	// Bare origin with a main branch holding one file.
	run(origin, "init", "-q", "--bare", "-b", base, ".")
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

	textConflict, err := gitMergeConflicts(ctx, wt, base)
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

	textConflict, err := gitMergeConflicts(ctx, wt, base)
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
