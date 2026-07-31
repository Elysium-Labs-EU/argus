package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// shellSyntaxCheckTimeout bounds the `sh -n -c` syntax-only parse used by
// Preflight. -n makes sh read and parse without executing anything, so this
// is a fixed, generous ceiling against a stuck shell binary, not a real
// workload budget the way verifyCommandTimeout/worktreeSetupCmdTimeout are.
const shellSyntaxCheckTimeout = 10 * time.Second

// PreflightErrors aggregates every config problem Preflight finds so an
// operator sees the full list in one shot — the whole point of preflighting
// is not stopping at the first mistake the way spawning workers one at a
// time already does.
type PreflightErrors []error

func (e PreflightErrors) Error() string {
	lines := make([]string, len(e))
	for i, err := range e {
		lines[i] = err.Error()
	}
	return fmt.Sprintf("%d config problem(s) found before spawning any worker:\n  - %s",
		len(e), strings.Join(lines, "\n  - "))
}

// Preflight validates the settings a whole supervise run shares across every
// worker in it, before Run creates a single worktree or spawns a single
// agent. GateVerifyCommand and WorktreeBootstrapCommand are each one string
// applied to every worker in plans; left unchecked, a mistake in either is
// otherwise discovered independently, and late, by each worker in turn —
// WorktreeBootstrapCommand only fails once a worktree already exists for it
// to run in (RunWorktreeBootstrapCommand), and GateVerifyCommand not until a
// worker reaches a terminal phase, after it has already done all its work
// (RunGateVerifyCommand). EnsureDistinctWorktrees is folded in here too so a
// worktree collision reports alongside any command problems in the same
// consolidated list, rather than as a separate, earlier failure.
func Preflight(ctx context.Context, cfg *Config, plans []WorkerPlan) error {
	var errs PreflightErrors
	if err := EnsureDistinctWorktrees(worktreePaths(plans)); err != nil {
		errs = append(errs, err)
	}
	if err := checkShellSyntax(ctx, "gate_verify_command", cfg.GateVerifyCommand); err != nil {
		errs = append(errs, err)
	}
	if err := checkShellSyntax(ctx, "worktree_bootstrap_command", cfg.WorktreeBootstrapCommand); err != nil {
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

// checkShellSyntax reports a config problem when cmdStr, if actually run
// later via execArgvOrShell, would hit /bin/sh's own parse failure rather
// than ever executing — the same failure mode isShellSyntaxError detects
// post-hoc for a worker's reported test command, caught here instead before
// any worker exists to waste time discovering it.
//
// A cmdStr eligible for execArgvOrShell's direct-argv path (see directArgv)
// never reaches a shell at runtime, so checking it against sh -n would be
// checking a code path this cmdStr will never take — skipped entirely,
// mirroring execArgvOrShell's own choice of execution path exactly.
//
// `sh -n -c` reads and parses cmdStr without executing it regardless of
// outcome, so this is safe to run unconditionally, including during a
// --dry-run or an offline preflight: no side effect, no network.
func checkShellSyntax(ctx context.Context, key, cmdStr string) error {
	if cmdStr == "" {
		return nil
	}
	if _, direct := directArgv(cmdStr); direct {
		return nil
	}

	runCtx, cancel := context.WithTimeout(ctx, shellSyntaxCheckTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "sh", "-n", "-c", cmdStr) //nolint:gosec // syntax-check only (-n never executes cmdStr), repo-owner-configured
	out, err := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 && shellSyntaxErrorPattern.Match(out) {
		return fmt.Errorf("%s %q is not valid shell syntax: %s", key, cmdStr, strings.TrimSpace(tail(out)))
	}
	return nil
}
