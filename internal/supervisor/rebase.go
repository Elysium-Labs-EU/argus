package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

// FetchBase updates the worktree's view of the base branch from origin, so a
// conflict check and rebase see the just-merged sibling changes.
func FetchBase(ctx context.Context, worktree, base string) error {
	_, err := git(ctx, worktree, "fetch", "origin", base)
	return err
}

// ConflictsWith reports whether the worktree's HEAD would conflict when rebased
// onto origin/<base>. It uses `git merge-tree --write-tree`, which computes the
// merge without touching the working tree and exits non-zero (code 1) when the
// merge has conflicts.
func ConflictsWith(ctx context.Context, worktree, base string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", worktree, "merge-tree", "--write-tree", "origin/"+base, "HEAD") //nolint:gosec // fixed git binary; worktree/base argus-derived
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return true, nil // conflicts
		}
		return false, fmt.Errorf("checking merge conflicts: %w", err)
	}
	return false, nil
}

// RebaseBrief is the task brief argus injects when dispatching a worker to
// resolve a post-merge conflict. The deterministic work (detecting the conflict,
// spawning the worker, verifying the result) is argus's; the conflict resolution
// itself needs the worker's judgment.
func RebaseBrief(branch, base string) string {
	return fmt.Sprintf(`Task: resolve a post-merge conflict on branch %s

A sibling change merged into %s first, so your branch now conflicts with it.
This is expected. Update your branch in place (do NOT open a new PR):

  git fetch origin %s
  git rebase origin/%s
  # resolve conflicts so BOTH your change and the merged change coexist
  # re-run the repo's checks (make ci, or make test + make lint)
  git push --force-with-lease

Confirm the checks pass and the branch is mergeable, then set your status phase to
"awaiting_review". Use "blocked" if the resolution needs a decision only the
supervisor can make.

%s`, branch, base, base, base, protocol.WriterBrief)
}

// InvalidateStatus removes a worktree's status and verdict files, if present,
// before a rebase worker is dispatched into it. Without this, a worker
// re-dispatched into a worktree that already carries a terminal status.json
// from an earlier, unrelated task (the normal supervise flow's own
// awaiting_review, written before this rebase was ever requested) leaves that
// leftover file for WaitForStatus's poller to read — and since the poller's
// first tick is immediate, it can report success before the newly spawned
// worker has done anything at all (argus issue #50). A missing file is not an
// error.
func InvalidateStatus(worktree string) error {
	for _, path := range []string{protocol.StatusPath(worktree), protocol.VerdictPath(worktree)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing %s: %w", path, err)
		}
	}
	return nil
}

// WaitForStatus polls a worktree's status file until it reports a phase written
// at or after since, reaches a terminal phase, or ctx is canceled, returning the
// last such status read and whether one was seen. A status.json whose
// updated_at is at or before since is a stale leftover — from before this
// dispatch, or from a race with InvalidateStatus — and is treated the same as
// no file at all, so a caller can never mistake it for this dispatch's outcome
// (argus issue #50). since should be no later than the moment the worker was
// dispatched. It is the single-worker analog of the supervise watch loop, for
// commands like rebase that dispatch one worker.
func WaitForStatus(ctx context.Context, worktree string, interval time.Duration, since time.Time) (protocol.Status, bool) {
	path := protocol.StatusPath(worktree)
	timer := time.NewTimer(0)
	defer timer.Stop()
	var last protocol.Status
	var hasFile bool
	for {
		select {
		case <-ctx.Done():
			return last, hasFile
		case <-timer.C:
			if s, err := protocol.Load(path); err == nil && s.UpdatedAt.After(since) {
				last = s
				hasFile = true
				if protocol.IsTerminal(s.Phase) {
					return last, hasFile
				}
			}
			timer.Reset(interval)
		}
	}
}
