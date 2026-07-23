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

// TestInvalidateStatusRemovesStaleFiles covers argus issue #50's fix: a rebase
// dispatch must not let a leftover status.json (or verdict.json) from an
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

func mustRemote(t *testing.T, worktree string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", worktree, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("remote get-url: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func TestRebaseBriefCarriesRebaseSteps(t *testing.T) {
	b := RebaseBrief("feat-x", "main")
	for _, want := range []string{"feat-x", "git rebase origin/main", "--force-with-lease", protocol.WriterBrief} {
		if !strings.Contains(b, want) {
			t.Errorf("rebase brief missing %q", want)
		}
	}
}
