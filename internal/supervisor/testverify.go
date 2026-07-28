package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

		for _, cmdStr := range replayCommands(t.Cmd, t.Target) {
			first := runVerify(ctx, worktree, cmdStr, timeout)
			if first.ok {
				continue
			}
			if first.timedOut {
				mismatches = append(mismatches, fmt.Sprintf(
					"could not verify claimed pass of %q: re-run exceeded %s and was killed", cmdStr, timeout))
				break
			}

			second := runVerify(ctx, worktree, cmdStr, timeout)
			if second.ok {
				continue
			}
			if second.timedOut {
				mismatches = append(mismatches, fmt.Sprintf(
					"could not verify claimed pass of %q: re-run exceeded %s and was killed", cmdStr, timeout))
				break
			}

			mismatches = append(mismatches, fmt.Sprintf(
				"worker claimed %q passed, but re-running it failed twice in a row: %v\n--- attempt 1 output (tail) ---\n%s\n--- attempt 2 output (tail) ---\n%s",
				cmdStr, second.err, tail(first.output), tail(second.output)))
			break
		}
	}
	return mismatches
}

// replayCommands recomposes the exact command line(s) to re-run for a
// worker's self-reported Cmd/Target, in place of a naive Cmd+" "+Target join
// that trusts the worker's paraphrase of what it actually typed. That join
// breaks in three observed ways, each guarded here:
//
//   - `make <target>`: make treats every token after the target name as an
//     additional target to build, never as an argument to the recipe — a
//     stray word the worker appended (in Cmd or Target) turns into "No rule
//     to make target". The target is always replayed bare.
//   - Target already present in Cmd: a worker sometimes folds a path into
//     Cmd directly and repeats it in Target, so joining hands the tool the
//     same path twice.
//   - Target listing several comma-separated paths (mirroring how a task
//     brief lists target files): joined into one command line this can
//     overflow a tool that only accepts a single positional argument.
//     Replaying Cmd once per listed path reproduces what actually happened
//     without needing to know any given tool's arity.
func replayCommands(cmd, target string) []string {
	if fields := strings.Fields(cmd); len(fields) >= 2 && fields[0] == "make" {
		return []string{"make " + fields[1]}
	}

	if target == "" || strings.Contains(cmd, target) {
		return []string{cmd}
	}

	if strings.Contains(target, ",") {
		var cmds []string
		for p := range strings.SplitSeq(target, ",") {
			if p = strings.TrimSpace(p); p != "" {
				cmds = append(cmds, cmd+" "+p)
			}
		}
		if len(cmds) > 0 {
			return cmds
		}
	}

	return []string{cmd + " " + target}
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

	cmd := execArgvOrShell(runCtx, cmdStr)
	cmd.Dir = worktree
	out, err := runCombinedOutput(cmd)
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)

	return verifyResult{ok: err == nil, timedOut: timedOut, err: err, output: out}
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
