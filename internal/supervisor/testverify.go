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

		// Target is a separate field precisely so a worker can report e.g.
		// {Cmd: "go tool fieldalignment", Target: "./..."} without folding it
		// into Cmd — the re-run must recompose the same command line the
		// worker actually ran, or it silently exercises a different (and for
		// some tools, argument-less and thus meaningless) invocation.
		cmdStr := t.Cmd
		if t.Target != "" {
			cmdStr = t.Cmd + " " + t.Target
		}

		first := runVerify(ctx, worktree, cmdStr, timeout)
		if first.ok {
			continue
		}
		if first.timedOut {
			mismatches = append(mismatches, fmt.Sprintf(
				"could not verify claimed pass of %q: re-run exceeded %s and was killed", cmdStr, timeout))
			continue
		}

		second := runVerify(ctx, worktree, cmdStr, timeout)
		if second.ok {
			continue
		}
		if second.timedOut {
			mismatches = append(mismatches, fmt.Sprintf(
				"could not verify claimed pass of %q: re-run exceeded %s and was killed", cmdStr, timeout))
			continue
		}

		mismatches = append(mismatches, fmt.Sprintf(
			"worker claimed %q passed, but re-running it failed twice in a row: %v\n--- attempt 1 output (tail) ---\n%s\n--- attempt 2 output (tail) ---\n%s",
			cmdStr, second.err, tail(first.output), tail(second.output)))
	}
	return mismatches
}

// verifyCommandTimeout bounds one run of a repo's configured verify_command
// (see repoconfig.Config.VerifyCommand). It is longer than testVerifyTimeout
// because this command is repo-owner-chosen and may itself chain build+lint+
// test (e.g. "make ci"), not a single reported test target.
const verifyCommandTimeout = 10 * time.Minute

// RunVerifyCommand re-runs a repo's own configured verify_command inside
// worktree: the gate otherwise only reproduces a worker's *claimed* test
// passes (VerifyTests), never a repo's own lint/build/pre-commit, so a diff
// could earn a clean verdict and then fail at `argus ship`'s `git commit`
// when the repo's own pre-commit hook ran the very check that would have
// caught it. An empty cmdStr means no verify command is configured for this
// repo, so nothing runs and "" (no mismatch) is returned.
//
// Mirrors VerifyTests' one-retry treatment: a shared-machine build/lint step
// can fail from resource contention as easily as a test run can, and — like
// a reproduced test failure — this check is unwaivable by any reviewer
// verdict (see gateVerdict), so a single failing sample is not trusted on
// its own.
func RunVerifyCommand(ctx context.Context, worktree, cmdStr string) string {
	if cmdStr == "" {
		return ""
	}

	first := runVerify(ctx, worktree, cmdStr, verifyCommandTimeout)
	if first.ok {
		return ""
	}
	if first.timedOut {
		return fmt.Sprintf("verify command %q exceeded %s and was killed", cmdStr, verifyCommandTimeout)
	}

	second := runVerify(ctx, worktree, cmdStr, verifyCommandTimeout)
	if second.ok {
		return ""
	}
	if second.timedOut {
		return fmt.Sprintf("verify command %q exceeded %s and was killed", cmdStr, verifyCommandTimeout)
	}

	return fmt.Sprintf(
		"repo's verify command %q failed twice in a row: %v\n--- attempt 1 output (tail) ---\n%s\n--- attempt 2 output (tail) ---\n%s",
		cmdStr, second.err, tail(first.output), tail(second.output))
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

// tail returns the last maxCapturedOutput bytes of b, so a truncated capture
// keeps the end of the output — where a build/test/lint pipeline's actual
// failure is, rather than its early, usually-uninteresting setup steps.
func tail(b []byte) string {
	if len(b) <= maxCapturedOutput {
		return string(b)
	}
	return "...(truncated)...\n" + string(b[len(b)-maxCapturedOutput:])
}
