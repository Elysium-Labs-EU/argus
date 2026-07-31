package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
// and a claimed skip makes no pass assertion to falsify. A claimed git
// mutation (commit/push/merge/rebase) is also left alone: unlike a test,
// lint, or build, these commands are not safely repeatable — a second
// `git commit` has nothing left to stage (exit 1, not evidence the first
// commit never happened), and a second `git push` either no-ops or fails on
// the re-run subprocess's own credentials, neither of which says anything
// about whether the worker's original push landed. This check does have
// git ground truth for these specifically (the measured diff, the branch's
// state on origin), unlike the general case this function exists for.
//
// A single failing sample is not trusted on its own: a slow, multi-stage
// command (build, race tests, lint, coverage) sharing a machine with other
// load can fail from resource contention rather than a real regression, and
// this check is unwaivable — so before reporting a mismatch, a failing
// re-run is confirmed with one retry. Only two failures in a row, with both
// runs' captured output, are reported.
//
// A stripped/reshaped Cmd (see replayCommands) can still turn out not to be
// literal shell — a descriptive label this codebase doesn't yet know the
// shape of. sh -c's own diagnostic for that (exit 2, "syntax error") means
// the string was never actually re-executed, which is not evidence the
// underlying claim is false the way a real non-zero exit is — so unlike a
// reproduced failure, that case is not retried (a parse failure is
// deterministic) and is returned separately in unverifiable rather than
// mismatches, so the gate can treat it as a waivable reason instead of an
// unwaivable one (see gateVerdict).
func VerifyTests(ctx context.Context, worktree string, tests []protocol.TestRun, timeout time.Duration) (mismatches, unverifiable []string) {
	for _, t := range tests {
		if t.Cmd == "" || t.Result != protocol.ResultPass || isGitMutation(t.Cmd) {
			continue
		}

		for _, rc := range replayCommands(worktree, t.Cmd, t.Target) {
			first := runVerify(ctx, rc.dir, rc.cmd, timeout)
			if first.ok {
				continue
			}
			if first.timedOut {
				mismatches = append(mismatches, fmt.Sprintf(
					"could not verify claimed pass of %q: re-run exceeded %s and was killed", rc.cmd, timeout))
				break
			}
			if isShellSyntaxError(rc.cmd, first) {
				unverifiable = append(unverifiable, fmt.Sprintf(
					"could not verify claimed pass of %q: re-run could not be parsed as shell syntax (%v) rather than actually executed — %q reads as a descriptive label, not a literal command\n--- output (tail) ---\n%s",
					rc.cmd, first.err, rc.cmd, tail(first.output)))
				break
			}

			second := runVerify(ctx, rc.dir, rc.cmd, timeout)
			if second.ok {
				continue
			}
			if second.timedOut {
				mismatches = append(mismatches, fmt.Sprintf(
					"could not verify claimed pass of %q: re-run exceeded %s and was killed", rc.cmd, timeout))
				break
			}

			mismatches = append(mismatches, fmt.Sprintf(
				"worker claimed %q passed, but re-running it failed twice in a row: %v\n--- attempt 1 output (tail) ---\n%s\n--- attempt 2 output (tail) ---\n%s",
				rc.cmd, second.err, tail(first.output), tail(second.output)))
			break
		}
	}
	return mismatches, unverifiable
}

// replayCommands recomposes the exact command line(s) to re-run for a
// worker's self-reported Cmd/Target, in place of a naive Cmd+" "+Target join
// that trusts the worker's paraphrase of what it actually typed. That join
// breaks in five observed ways, each guarded here:
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
//   - Target holding a human-readable description of what Cmd exercises
//     (e.g. Cmd "task frontend:test", Target "frontend unit tests (vitest)")
//     rather than an argument Cmd expects: the writer brief never tells a
//     worker which shape Target is, so this is the normal case, not a
//     misuse. Appending it produces neither valid shell nor a valid
//     subcommand, and that failure was previously misread as a real
//     mismatch. A label reads as prose — multiple words — where a
//     positional argument reported this way is always one shell word, so
//     Cmd is replayed bare rather than guessing where in the phrase a
//     shell-safe split would even go.
//   - Cmd itself carrying a trailing parenthetical aside describing what it
//     triggers (e.g. `git commit (lefthook pre-commit: format, lint,
//     fieldalignment, test)`) rather than being purely literal shell — the
//     same prose-vs-argument confusion the Target case above guards against,
//     just folded into Cmd instead. Passed to sh -c verbatim, the stray
//     parens are a syntax error ("unexpected token `(`"), which is not
//     evidence the underlying claim is false — yet this mismatch is
//     unwaivable. Stripped before replay so the literal command underneath
//     is what actually gets re-run.
//   - Target naming a subdirectory the command must be run from rather than
//     an argument to append (e.g. a monorepo with one go.mod/Makefile per
//     plugin dir: Cmd "govulncheck ./..." or "make crap", Target
//     "eos-sink-logbench") — appending it produces "govulncheck ./...
//     eos-sink-logbench", a positional argument the tool never expected,
//     instead of the cwd the worker actually ran it from; for the make case
//     it's worse, since "make crap eos-sink-logbench" reads as a second,
//     nonexistent target and fails with "No rule to make target". Detected
//     by resolving target against worktree; only a real directory takes
//     this branch (both here and in the make branch above) — any other
//     single word (e.g. a package path like "./...") falls through
//     unchanged to the append case, since that's exactly what tools with a
//     real positional target expect.
func replayCommands(worktree, cmd, target string) []replayCmd {
	cmd = stripTrailingParenthetical(cmd)

	if fields := strings.Fields(cmd); len(fields) >= 2 && fields[0] == "make" {
		return []replayCmd{{cmd: "make " + fields[1], dir: targetDir(worktree, target)}}
	}

	if target == "" || strings.Contains(cmd, target) {
		return []replayCmd{{cmd: cmd, dir: worktree}}
	}

	if strings.Contains(target, ",") {
		var cmds []replayCmd
		for p := range strings.SplitSeq(target, ",") {
			if p = strings.TrimSpace(p); p != "" {
				cmds = append(cmds, replayCmd{cmd: cmd + " " + p, dir: worktree})
			}
		}
		if len(cmds) > 0 {
			return cmds
		}
	}

	if len(strings.Fields(target)) > 1 {
		return []replayCmd{{cmd: cmd, dir: worktree}}
	}

	if dir, ok := targetDirIfExists(worktree, target); ok {
		return []replayCmd{{cmd: cmd, dir: dir}}
	}

	return []replayCmd{{cmd: cmd + " " + target, dir: worktree}}
}

// targetDirIfExists resolves target against worktree and reports whether the
// result is a real directory — the shared test the make branch and the
// final directory-detection branch both need before treating target as a
// cwd rather than a positional argument.
func targetDirIfExists(worktree, target string) (string, bool) {
	dir := filepath.Join(worktree, target)
	info, err := os.Stat(dir)
	if err != nil {
		return dir, false
	}
	return dir, info.IsDir()
}

// targetDir is targetDirIfExists with the not-a-directory case folded to
// worktree itself, for call sites (like the make branch) that always need
// some dir and fall back to worktree when target isn't one.
func targetDir(worktree, target string) string {
	if dir, ok := targetDirIfExists(worktree, target); ok {
		return dir
	}
	return worktree
}

// replayCmd is one command line replayCommands recomposed, paired with the
// directory it must run from — a monorepo per-module Target changes cwd
// rather than becoming a positional argument, so a single command string is
// no longer enough to describe a replay.
type replayCmd struct {
	cmd string
	dir string
}

// gitMutationSubcommands are git subcommands whose whole point is to change
// repo state, so a second identical invocation is expected to behave
// differently from the first (nothing left to commit, nothing new to push)
// rather than reproduce it — see the isGitMutation call site in VerifyTests.
var gitMutationSubcommands = map[string]bool{
	"commit": true,
	"push":   true,
	"merge":  true,
	"rebase": true,
}

// isGitMutation reports whether cmd's first two words are "git" plus one of
// gitMutationSubcommands, regardless of any flags or trailing text after
// that (e.g. "git push --force-with-lease" or a Cmd carrying a trailing
// parenthetical aside, stripped separately by stripTrailingParenthetical).
func isGitMutation(cmd string) bool {
	fields := strings.Fields(cmd)
	return len(fields) >= 2 && fields[0] == "git" && gitMutationSubcommands[fields[1]]
}

// trailingParenthetical matches a space-separated, unnested "(...)" aside at
// the very end of a string, e.g. the " (lefthook pre-commit: format, lint)"
// in `git commit (lefthook pre-commit: format, lint)`. Deliberately narrow —
// only a *trailing* group is stripped, so parens genuinely part of a command
// (e.g. mid-string) are left untouched.
var trailingParenthetical = regexp.MustCompile(`\s+\([^()]*\)\s*$`)

// stripTrailingParenthetical removes a trailing descriptive aside from a
// worker-reported Cmd, leaving the literal command in front of it intact.
// A Cmd with no such aside is returned unchanged.
func stripTrailingParenthetical(cmd string) string {
	if loc := trailingParenthetical.FindStringIndex(cmd); loc != nil {
		return strings.TrimSpace(cmd[:loc[0]])
	}
	return cmd
}

// shellSyntaxErrorPattern matches the diagnostic a POSIX shell prints on a
// genuine parse failure — bash's "sh: -c: line 0: syntax error near
// unexpected token `(`" and dash/ash's "Syntax error: unexpected ..." both
// contain this substring, case differences aside.
var shellSyntaxErrorPattern = regexp.MustCompile(`(?i)syntax error`)

// isShellSyntaxError reports whether res is sh -c's own parse failure for
// cmdStr rather than a real command that ran and exited non-zero on its own.
// Exit code 2 alone is not enough signal — a worker's own script can
// legitimately exit 2 — so this also confirms cmdStr actually went through
// the sh -c fallback (directArgv false; see execArgvOrShell) before trusting
// the output's "syntax error" text: only together do they mean the string
// was rejected as unparsable, never executed at all.
func isShellSyntaxError(cmdStr string, res verifyResult) bool {
	if _, direct := directArgv(cmdStr); direct {
		return false
	}
	var exitErr *exec.ExitError
	if !errors.As(res.err, &exitErr) || exitErr.ExitCode() != 2 {
		return false
	}
	return shellSyntaxErrorPattern.Match(res.output)
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

func runVerify(ctx context.Context, dir, cmdStr string, timeout time.Duration) verifyResult {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := execArgvOrShell(runCtx, cmdStr)
	cmd.Dir = dir
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
