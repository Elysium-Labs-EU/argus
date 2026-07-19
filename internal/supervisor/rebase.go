package supervisor

import (
	"context"
	"errors"
	"fmt"
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

// WaitForStatus polls a worktree's status file until it reaches a terminal phase
// or ctx is canceled, returning the last status read and whether a file was seen.
// It is the single-worker analog of the supervise watch loop, for commands like
// rebase that dispatch one worker.
func WaitForStatus(ctx context.Context, worktree string, interval time.Duration) (protocol.Status, bool) {
	st := &workerState{plan: &WorkerPlan{Worker: Worker{Worktree: worktree}}}
	pollStatus(ctx, interval, 0, nil, st)
	return st.status, st.hasFile
}
