package supervisor

import (
	"context"
	"encoding/json"
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

// ScratchHomePath returns a worker's per-worktree scratch HOME directory,
// alongside argus's own scaffolding (status.json, brief.md) so it is cleaned
// up whenever the worktree is pruned and never shows up in a worker's own
// tracked-file diff.
func ScratchHomePath(worktree string) string {
	return filepath.Join(worktree, ".claude", "argus", "home")
}

// ProvisionScratchHome creates worktree's scratch HOME and symlinks its
// .claude to realHome's .claude, so a worker spawned with HOME set to the
// scratch dir (see scratchHomeEnv) keeps its own Claude Code auth/config and
// still writes its session transcript under realHome's .claude/projects —
// exactly where HasPlanEvidence already looks, so that check needs no
// change. Every other credential path a real $HOME exposes by convention
// (~/.ssh, ~/.netrc, ~/.git-credentials, ~/.config/gh, ~/.aws, ...) simply
// does not exist under the scratch tree; that absence, not an explicit deny
// rule, is what closes the read vector for the default unwrapped worker.
//
// It also copies realHome's top-level ~/.claude.json state (see
// provisionScratchClaudeConfig) — the .claude symlink alone carries the
// projects/ transcript dir but not this file, so without it a scratch HOME
// looks like a brand-new install and Claude Code opens its first-run
// onboarding wizard instead of reading the worker's brief.
//
// realHome == "" (a Config that never resolved a home directory) leaves the
// scratch dir without a .claude symlink or .claude.json rather than
// erroring — the same soft fallback HasPlanEvidence already applies to an
// empty home.
func ProvisionScratchHome(worktree, realHome string) error {
	scratch := ScratchHomePath(worktree)
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return fmt.Errorf("creating scratch home: %w", err)
	}
	if realHome == "" {
		return nil
	}
	target := filepath.Join(realHome, ".claude")
	// A dangling symlink (target missing) blocks a later `mkdir` through it —
	// the worker's own Claude Code session would then fail to create its
	// projects/ dir on a host that has never run `claude` before.
	if err := os.MkdirAll(target, 0o755); err != nil { //nolint:gosec // real home's own config dir, standard perms
		return fmt.Errorf("creating real home .claude dir: %w", err)
	}
	link := filepath.Join(scratch, ".claude")
	switch _, statErr := os.Lstat(link); {
	case statErr == nil:
		// already provisioned by an earlier attempt at this worktree
	case os.IsNotExist(statErr):
		if err := os.Symlink(target, link); err != nil {
			return fmt.Errorf("symlinking scratch home .claude: %w", err)
		}
	default:
		return fmt.Errorf("checking scratch home .claude symlink: %w", statErr)
	}
	return provisionScratchClaudeConfig(scratch, realHome)
}

// claudeConfigOmitTopLevelKeys withholds top-level ~/.claude.json keys from
// a worker's scratch-home copy. "projects" carries every other project's
// full session history, MCP config, and allowed-tools list keyed by
// absolute path — none of it applies to a worktree path Claude Code has
// never seen, and copying it would leak unrelated project data into a
// worker's environment.
var claudeConfigOmitTopLevelKeys = map[string]bool{
	"projects": true,
}

// provisionScratchClaudeConfig copies realHome's top-level ~/.claude.json
// fields (oauthAccount, userID, hasCompletedOnboarding, theme, and any
// other identity/onboarding-gate field Claude Code adds later) into
// scratch/.claude.json, omitting claudeConfigOmitTopLevelKeys. Copying the
// raw top-level map rather than a hardcoded field list means a future
// onboarding gate is carried forward automatically, with no code change
// here. A missing real ~/.claude.json is a soft no-op, not an error — a
// realHome that has never run `claude` leaves the worker no worse off than
// before this function existed.
func provisionScratchClaudeConfig(scratch, realHome string) error {
	raw, err := os.ReadFile(filepath.Join(realHome, ".claude.json")) //nolint:gosec // realHome is operator-controlled, not user input
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading real .claude.json: %w", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("parsing real .claude.json: %w", err)
	}
	for key := range claudeConfigOmitTopLevelKeys {
		delete(fields, key)
	}
	// map[string]json.RawMessage.MarshalJSON never fails: every value was
	// already validated as JSON by the Unmarshal above.
	out, _ := json.Marshal(fields)

	if err := os.WriteFile(filepath.Join(scratch, ".claude.json"), out, 0o600); err != nil {
		return fmt.Errorf("writing scratch .claude.json: %w", err)
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
