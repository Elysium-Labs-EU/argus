package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

// worktreeSetupCmdTimeout bounds one run of a repo's configured
// worktree_setup_cmd (see repoconfig.Config.WorktreeBootstrapCommand), so a hung
// bootstrap script fails worktree creation instead of blocking execute
// forever.
const worktreeSetupCmdTimeout = 5 * time.Minute

// RunWorktreeBootstrapCommand runs a repo's own configured worktree_setup_cmd once,
// synchronously, inside a freshly created worktree — the hook a repo whose
// task depends on gitignored per-developer local config (env files, local
// settings) needs to bootstrap that config into every new worktree, since
// those files exist only in the original checkout and a bare `git worktree
// add` never copies them. Called by prepareWorktree right after herdr's
// WorktreeCreate succeeds and before the worker's agent is spawned.
//
// An empty cmdStr means no command is configured, so nothing runs. A
// non-zero exit (or a run that exceeds worktreeSetupCmdTimeout) is returned
// as an error carrying the command's combined output, the same way a `git
// worktree add` failure already fails worktree creation — prepareWorktree's
// caller treats this identically, aborting before the worker is spawned.
func RunWorktreeBootstrapCommand(ctx context.Context, worktree, cmdStr string) error {
	if cmdStr == "" {
		return nil
	}

	runCtx, cancel := context.WithTimeout(ctx, worktreeSetupCmdTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "sh", "-c", cmdStr) //nolint:gosec // repo-owner-configured bootstrap command, run in its own freshly created worktree
	cmd.Dir = worktree
	out, err := cmd.CombinedOutput()
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("worktree_setup_cmd %q exceeded %s and was killed", cmdStr, worktreeSetupCmdTimeout)
	}
	if err != nil {
		return fmt.Errorf("worktree_setup_cmd %q failed: %w\n%s", cmdStr, err, tail(out))
	}
	return nil
}

// WriteSettings renders defaultAgent's permission file into worktree. It must
// run before the worker's agent starts, since settings are read once at
// session launch. This is the generic (agent-agnostic) mechanics — path and
// content are the agent's own concern; see AgentAdapter.RenderSettings.
// rebaseAllow is RebasePhaseAllow's own grant for this worktree's base (see
// ResolvedAllowSet's doc comment for why it must be baked in here rather
// than left to the live PreToolUse hook).
//
// It also persists extraAllow into the worktree (see protocol.SaveExtraAllow)
// so the live PreToolUse hook (argus worker check-tool) can fold it into its
// own phase-scoped allow check later — that hook runs as a fresh subprocess
// per Bash call, with no access to this invocation's own --allow flags.
//
// sandboxEnabled and sandboxAllowWrite are the experimental OS-sandbox
// toggle (see Config.ExperimentalSandbox) and its filesystem write-allow
// list, forwarded to RenderSettings unchanged.
func WriteSettings(worktree string, project protocol.PhaseConfig, baseAllow, extraAllow, rebaseAllow []string, sandboxEnabled bool, sandboxAllowWrite []string) error {
	relPath, content, err := defaultAgent.RenderSettings(worktree, project, baseAllow, extraAllow, rebaseAllow, sandboxEnabled, sandboxAllowWrite)
	if err != nil {
		return fmt.Errorf("rendering settings: %w", err)
	}
	path := filepath.Join(worktree, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // worktree-local config dir, standard perms
		return fmt.Errorf("creating settings dir: %w", err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil { //nolint:gosec // local settings file, not a secret
		return fmt.Errorf("writing settings file: %w", err)
	}
	if err := protocol.SaveExtraAllow(worktree, extraAllow); err != nil {
		return fmt.Errorf("persisting extra allow flags: %w", err)
	}
	return nil
}

// WriteBrief writes the worker's task brief to its worktree so the launch prompt
// can point the agent at a file instead of pasting a multi-line brief into its
// TUI. Written before the worker's agent starts.
func WriteBrief(worktree, brief string) error {
	dir := filepath.Join(worktree, ".claude", "argus")
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // worktree-local dir, standard perms
		return fmt.Errorf("creating argus dir: %w", err)
	}
	if err := os.WriteFile(protocol.BriefPath(worktree), []byte(brief+"\n"), 0o644); err != nil { //nolint:gosec // local brief file, not a secret
		return fmt.Errorf("writing brief.md: %w", err)
	}
	return nil
}

// EnsureDistinctWorktrees refuses to proceed when two workers would land in the
// same worktree — the real collision hazard, since two agents editing one
// checkout will clobber each other. This is the correct gate for argus's dispatch
// model: workers may start in a shared repo root (that's fine, each is moved into
// its own worktree), so what must be distinct is the target worktree, not the
// launch cwd. Paths collide only when two workers share a branch.
func EnsureDistinctWorktrees(paths []string) error {
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		if seen[p] {
			return fmt.Errorf("two workers target the same worktree %s: give each its own branch", p)
		}
		seen[p] = true
	}
	return nil
}
