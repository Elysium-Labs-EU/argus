package supervisor

import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

// AgentAdapter isolates the pieces of argus that are hard-coupled to one
// worker agent's own conventions — its permission-file schema and its
// session-transcript layout — behind a single named seam, instead of leaving
// them implicit inside loop.go/worktree.go's otherwise agent-agnostic
// orchestration. Exactly one implementation is wired in today
// (claudeCodeAdapter): every pane in every fleet to date reports agent:
// claude. This is not a multi-agent dispatch mechanism — there is no --agent
// flag and no second implementation to design against — just a name for
// what is already Claude-specific, so that a second agent (if one is ever
// tried) is a new implementation of this interface plus a flag to select it,
// not a rewrite of the generic code that calls it.
type AgentAdapter interface {
	// DefaultLauncher is the shell command that starts this agent in a
	// freshly split pane.
	DefaultLauncher() string

	// RenderSettings builds the worktree-scoped permission file this agent
	// reads at session launch: its path relative to worktree, and its
	// encoded content. extraAllow appends operator-supplied permission
	// patterns after the agent's own defaults.
	RenderSettings(worktree string, extraAllow []string) (path string, content []byte, err error)

	// PlanEvidence reports whether any session transcript for worktree
	// contains a real todo-list tool call — the unfakeable backstop for the
	// planning phase's self-reported plan (see HasPlanEvidence).
	PlanEvidence(home, worktree string) (bool, error)
}

// defaultAgent is the single AgentAdapter argus uses. Not yet exposed via
// Config or a flag: with one implementation and zero second data point,
// wiring agent selection through Config would be speculative.
var defaultAgent AgentAdapter = claudeCodeAdapter{}

// claudeCodeAdapter is the AgentAdapter argus wires in today.
type claudeCodeAdapter struct{}

func (claudeCodeAdapter) DefaultLauncher() string {
	return DefaultLauncher
}

func (claudeCodeAdapter) RenderSettings(worktree string, extraAllow []string) (string, []byte, error) {
	settings := settingsFor(worktree, extraAllow)
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", nil, fmt.Errorf("encoding settings: %w", err)
	}
	return filepath.Join(".claude", "settings.local.json"), append(data, '\n'), nil
}

func (claudeCodeAdapter) PlanEvidence(home, worktree string) (bool, error) {
	return HasPlanEvidence(home, worktree)
}

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

// settingsFor builds the worktree-scoped permission settings. This is the single
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

	// Deny wins over allow in Claude Code regardless of pattern specificity
	// (deny/ask/allow are checked in that order, first match wins), so these
	// entries carve the worker's own permission files out of the broad
	// Edit/Write(glob) allows above: without them a worker could rewrite its
	// own settings.local.json mid-session and grant itself capability the
	// operator never approved. WriteSettings only resets the file at the
	// start of the *next* run, which leaves the entire current session
	// unprotected.
	ownSettings := []string{
		worktree + "/.claude/settings.local.json",
		worktree + "/.claude/settings.json",
	}
	deny := []string{
		"Bash(rm -rf *)",
		"Bash(git worktree remove*)",
		"Bash(git worktree prune*)",
		"Bash(git clean -f*)",
		"Bash(git reset --hard*)",
		"Bash(trash *)",
		"Bash(sudo *)",
	}
	for _, p := range ownSettings {
		deny = append(deny, "Edit("+p+")", "Write("+p+")")
	}

	return permissionSettings{
		Permissions: permissionBlock{
			Allow: allow,
			Ask: []string{
				"Bash(git commit:*)",
				"Bash(git push:*)",
			},
			Deny: deny,
		},
	}
}
