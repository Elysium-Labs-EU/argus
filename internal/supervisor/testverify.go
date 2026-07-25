package supervisor

import (
	"bytes"
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

// maxCapturedOutput caps how much of a failing re-run's combined output rides
// into the mismatch string (and from there into verdict.json/status). A
// multi-stage command like `make ci` can produce megabytes of build+race+lint
// output; only the tail is ever useful for diagnosing the actual failure, and
// an unbounded capture would bloat every downstream artifact that carries it.
const maxCapturedOutput = 4000

// VerifyTests re-runs every test a worker claimed passed, inside worktree, and
// returns a mismatch string for each one whose actual exit code disagrees —
// the tests equivalent of MeasureDiff: unlike DiffStat, a claimed test result
// has no git ground truth to cross-check, so the gate must reproduce it
// itself rather than trust status.json. A claimed fail or skip is left alone:
// Assess already escalates on any reported failure regardless of this check,
// and a claimed skip makes no pass assertion to falsify.
//
// A single failing sample is not trusted on its own: a slow, multi-stage
// command (build, race tests, lint, coverage) sharing a machine with other
// load can fail from resource contention rather than a real regression, and
// this check is unwaivable — so before reporting a mismatch, a failing
// re-run is confirmed with one retry. Only two failures in a row, with both
// runs' captured output, are reported.
func VerifyTests(ctx context.Context, worktree string, tests []protocol.TestRun, timeout time.Duration) []string {
	var mismatches []string
	for _, t := range tests {
		if t.Cmd == "" || t.Result != protocol.ResultPass {
			continue
		}

		first := runVerify(ctx, worktree, t.Cmd, timeout)
		if first.ok {
			continue
		}
		if first.timedOut {
			mismatches = append(mismatches, fmt.Sprintf(
				"could not verify claimed pass of %q: re-run exceeded %s and was killed", t.Cmd, timeout))
			continue
		}

		second := runVerify(ctx, worktree, t.Cmd, timeout)
		if second.ok {
			continue
		}
		if second.timedOut {
			mismatches = append(mismatches, fmt.Sprintf(
				"could not verify claimed pass of %q: re-run exceeded %s and was killed", t.Cmd, timeout))
			continue
		}

		mismatches = append(mismatches, fmt.Sprintf(
			"worker claimed %q passed, but re-running it failed twice in a row: %v\n--- attempt 1 output (tail) ---\n%s\n--- attempt 2 output (tail) ---\n%s",
			t.Cmd, second.err, tail(first.output, maxCapturedOutput), tail(second.output, maxCapturedOutput)))
	}
	return mismatches
}

// verifyResult is one re-run attempt's outcome: whether it matched the
// worker's claimed pass, whether it was killed for running too long, and
// (when it failed for either reason) the combined stdout/stderr it produced —
// otherwise a bare exit-status error is the only diagnosis anyone gets.
type verifyResult struct {
	err      error
	output   []byte
	ok       bool
	timedOut bool
}

func runVerify(ctx context.Context, worktree, cmdStr string, timeout time.Duration) verifyResult {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "sh", "-c", cmdStr) //nolint:gosec // re-running the worker's own reported test command, in its own worktree
	cmd.Dir = worktree
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)

	return verifyResult{ok: err == nil, timedOut: timedOut, err: err, output: buf.Bytes()}
}

// tail returns the last max bytes of b, so a truncated capture keeps the end
// of the output — where a build/test/lint pipeline's actual failure is,
// rather than its early, usually-uninteresting setup steps.
func tail(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return "...(truncated)...\n" + string(b[len(b)-max:])
}
