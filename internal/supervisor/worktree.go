package supervisor

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

// WriteSettings renders defaultAgent's permission file into worktree. It must
// run before the worker's agent starts, since settings are read once at
// session launch. This is the generic (agent-agnostic) mechanics — path and
// content are the agent's own concern; see AgentAdapter.RenderSettings.
func WriteSettings(worktree string, repoAllow, extraAllow []string) error {
	relPath, content, err := defaultAgent.RenderSettings(worktree, repoAllow, extraAllow)
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
