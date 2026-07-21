package supervisor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

// permissionSettings mirrors the shape Claude Code reads from
// .claude/settings.local.json. argus generates it per worktree so containment is
// a technical fact, not an instruction a worker has to remember — the allow list
// pre-clears routine read/build/test/edit-in-own-worktree calls, and the deny
// list makes "never leave or destroy your worktree" enforced.
type permissionSettings struct {
	Permissions permissionBlock `json:"permissions"`
}

type permissionBlock struct {
	Allow []string `json:"allow"`
	Ask   []string `json:"ask"`
	Deny  []string `json:"deny"`
}

// SettingsFor builds the worktree-scoped permission settings. This is the single
// source of truth for worker permissions (lifted from the supervise-agents
// skill): edits/writes are confined to the worktree, build/test/read commands
// are pre-cleared, commit/push stay gated behind ask, and destructive or
// out-of-worktree operations are denied outright.
//
// extraAllow appends operator-supplied patterns (e.g. "Bash(task *)" for a repo
// whose runner isn't make) after the Go/make defaults, so a non-Go repo isn't
// stuck hitting a permission prompt on every command its own AGENTS.md mandates.
func settingsFor(worktree string, extraAllow []string) permissionSettings {
	glob := worktree + "/**"
	allow := []string{
		"Edit(" + glob + ")",
		"Write(" + glob + ")",
		"Bash(go build *)",
		"Bash(go test *)",
		"Bash(go vet *)",
		"Bash(go get *)",
		"Bash(make *)",
		"Bash(git status*)",
		"Bash(git diff*)",
		"Bash(git log*)",
		"Bash(git add*)",
	}
	allow = append(allow, extraAllow...)
	return permissionSettings{
		Permissions: permissionBlock{
			Allow: allow,
			Ask: []string{
				"Bash(git commit:*)",
				"Bash(git push:*)",
			},
			Deny: []string{
				"Bash(rm -rf *)",
				"Bash(git worktree remove*)",
				"Bash(git worktree prune*)",
				"Bash(git clean -f*)",
				"Bash(git reset --hard*)",
				"Bash(trash *)",
				"Bash(sudo *)",
			},
		},
	}
}

// WriteSettings renders SettingsFor(worktree) to worktree/.claude/settings.local.json.
// It must run before the worker's claude session starts, since settings are read
// once at session launch. extraAllow is forwarded to settingsFor unchanged.
func WriteSettings(worktree string, extraAllow []string) error {
	settings := settingsFor(worktree, extraAllow)
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding settings: %w", err)
	}
	dir := filepath.Join(worktree, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // worktree-local config dir, standard perms
		return fmt.Errorf("creating .claude dir: %w", err)
	}
	path := filepath.Join(dir, "settings.local.json")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil { //nolint:gosec // local settings file, not a secret
		return fmt.Errorf("writing settings.local.json: %w", err)
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
