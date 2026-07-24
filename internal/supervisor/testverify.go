package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

// testVerifyTimeout bounds one re-run of a worker's claimed test command, so
// a hung suite escalates the worker instead of blocking judgeEach forever.
const testVerifyTimeout = 5 * time.Minute

// VerifyTests re-runs every test a worker claimed passed, inside worktree, and
// returns a mismatch string for each one whose actual exit code disagrees —
// the tests equivalent of MeasureDiff: unlike DiffStat, a claimed test result
// has no git ground truth to cross-check, so the gate must reproduce it
// itself rather than trust status.json. A claimed fail or skip is left alone:
// Assess already escalates on any reported failure regardless of this check,
// and a claimed skip makes no pass assertion to falsify.
func VerifyTests(ctx context.Context, worktree string, tests []protocol.TestRun, timeout time.Duration) []string {
	var mismatches []string
	for _, t := range tests {
		if t.Cmd == "" || t.Result != protocol.ResultPass {
			continue
		}
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		cmd := exec.CommandContext(runCtx, "sh", "-c", t.Cmd) //nolint:gosec // re-running the worker's own reported test command, in its own worktree
		cmd.Dir = worktree
		err := cmd.Run()
		timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
		cancel()
		switch {
		case err == nil:
			continue
		case timedOut:
			mismatches = append(mismatches, fmt.Sprintf(
				"could not verify claimed pass of %q: re-run exceeded %s and was killed", t.Cmd, timeout))
		default:
			mismatches = append(mismatches, fmt.Sprintf(
				"worker claimed %q passed, but re-running it failed: %v", t.Cmd, err))
		}
	}
	return mismatches
}
