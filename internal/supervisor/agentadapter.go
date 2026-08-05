package supervisor

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
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
	// encoded content. project is the repo's own .argus/config.yml
	// phases.<name>.allow policy (see internal/repoconfig); baseAllow is its
	// phase-independent top-level allow list; extraAllow appends
	// operator-supplied CLI --allow patterns on top of both, for a one-off
	// run. The rendered allow list is the union of every configurable
	// phase's own resolved allow (see ResolvedAllowForPhase) — the static
	// session-wide file can't itself be phase-conditional, so the live
	// PreToolUse hook (argus worker check-tool) narrows it back down to the
	// worker's actual current phase on every call.
	RenderSettings(worktree string, project protocol.PhaseConfig, baseAllow, extraAllow []string) (path string, content []byte, err error)

	// PlanEvidence reports whether any session transcript for worktree
	// contains a real todo-list tool call — the unfakeable backstop for the
	// planning phase's self-reported plan (see HasPlanEvidence) — plus how
	// many transcripts were scanned to reach that verdict.
	PlanEvidence(home, worktree string) (found bool, transcriptsChecked int, err error)
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

func (claudeCodeAdapter) RenderSettings(worktree string, project protocol.PhaseConfig, baseAllow, extraAllow []string) (string, []byte, error) {
	settings := settingsFor(worktree, project, baseAllow, extraAllow)
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", nil, fmt.Errorf("encoding settings: %w", err)
	}
	return filepath.Join(".claude", "settings.local.json"), append(data, '\n'), nil
}

func (claudeCodeAdapter) PlanEvidence(home, worktree string) (bool, int, error) {
	return HasPlanEvidence(home, worktree)
}

// permissionSettings mirrors the shape Claude Code reads from
// .claude/settings.local.json under --permission-mode dontAsk: never prompt
// a human, resolve every call from Allow/Deny alone (read-only tools stay
// auto-allowed by the mode itself) — an unlisted mutating command is denied
// and fed back to the worker, not asked and not hung. argus generates this
// per worktree so containment is a technical fact, not an instruction a
// worker has to remember — Allow pre-clears routine read/build/test/
// edit-in-own-worktree calls, and Deny makes "never leave or destroy your
// worktree, never commit or push" enforced natively, on top of the live
// PreToolUse hook (see checkToolHook) that narrows Allow down to the
// worker's current phase.
//
// EnableAllProjectMcpServers pre-approves Claude Code's one-time interactive
// consent gate for any project-scoped MCP server (.mcp.json) it discovers —
// a separate screen from --permission-mode's own tool-permission checks,
// which does not cover it. A headless worker pane has no human to answer that
// prompt, so left unset it hangs at agent_status idle forever with
// status.json never written, indistinguishable from "hasn't started yet".
type permissionSettings struct {
	Permissions                permissionBlock `json:"permissions"`
	Hooks                      hooksBlock      `json:"hooks"`
	EnableAllProjectMcpServers bool            `json:"enableAllProjectMcpServers"`
}

// permissionBlock has no Ask field: dontAsk never prompts, so an "ask" list
// has no meaning to resolve against — a command is either in Allow (or
// read-only, auto-allowed by the mode) or it's denied. git commit/push and
// argus's own supervisor commands used to live in Ask, gated behind a human
// approval outside planning; they are now in Deny unconditionally, in every
// phase (see protocol.DenyFloor) — a worker never commits or pushes at all,
// so there is no phase where prompting a human through it was the right
// answer either.
type permissionBlock struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
}

// hooksBlock mirrors the shape Claude Code reads from settings.json's "hooks"
// key. Only PreToolUse is populated today — see checkToolHook.
type hooksBlock struct {
	PreToolUse []hookMatcher `json:"PreToolUse,omitempty"`
}

// hookMatcher pairs a tool-name matcher (Claude Code's own glob-like syntax,
// e.g. "Bash") with the commands to run when it fires.
type hookMatcher struct {
	Matcher string      `json:"matcher"`
	Hooks   []hookEntry `json:"hooks"`
}

type hookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// checkToolHook wires `argus worker check-tool` as a PreToolUse/Bash hook on
// every worker. The static allow/deny lists above are read once at session
// launch and can only ever be the union across every phase (dontAsk has no
// notion of "phase"), so a phase-conditional rule — both DeniedInPhase's
// floor and ResolvedAllowForPhase's own per-phase scoping — needs a hook
// that re-checks the worktree's current status.json live instead.
func checkToolHook() hookMatcher {
	return hookMatcher{
		Matcher: "Bash",
		Hooks: []hookEntry{
			{Type: "command", Command: "argus worker check-tool"},
		},
	}
}

// settingsFor builds the worktree-scoped permission settings for
// --permission-mode dontAsk. This is the single source of truth for worker
// permissions: edits/writes are confined to the worktree, the resolved allow
// set (see ResolvedAllowForPhase) covers every configurable phase's own
// materialized toolchain plus the structural floor, and destructive,
// out-of-worktree, or deny-floor operations are denied outright — both
// statically here (defense in depth) and live via checkToolHook.
//
// project is the repo's own .argus/config.yml phases.<name>.allow policy
// (see internal/repoconfig); baseAllow is its phase-independent top-level
// allow list — together they replace the build/test-tool entries argus used
// to hardcode for every repo regardless of toolchain. With neither set (no
// config file, or none present), a worker still gets exactly the structural
// floor: enough to edit files and be gated, no repo toolchain command —
// skipping setup makes a worker *more* restricted, never less. extraAllow
// appends operator-supplied CLI --allow patterns on top of both, for a
// one-off run.
//
// The rendered Allow list unions every protocol.ConfigurablePhases value's
// own ResolvedAllowForPhase, since this file is read once at session launch
// and can't itself vary by the worker's current phase — checkToolHook is
// what actually narrows a live Bash call down to what the *current* phase's
// own resolved set allows.
func settingsFor(worktree string, project protocol.PhaseConfig, baseAllow, extraAllow []string) permissionSettings {
	glob := worktree + "/**"
	allow := []string{
		"Edit(" + glob + ")",
		"Write(" + glob + ")",
	}
	var unioned []string
	for _, p := range protocol.ConfigurablePhases {
		unioned = append(unioned, ResolvedAllowForPhase(p, project, baseAllow, extraAllow)...)
	}
	allow = append(allow, dedupeStrings(unioned)...)

	// Deny wins over allow in Claude Code regardless of pattern specificity
	// (deny/allow are checked in that order, first match wins), so these
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
		// .claude/argus/ holds status.json/verdict.json/lifecycle.json —
		// argus's own control plane, mutated only by its in-process writers
		// and by `argus worker report` (a Bash subprocess this deny does not
		// touch). Without this, a worker's Edit/Write tool can hand-edit
		// status.json straight to awaiting_review or write an approving
		// verdict.json, skipping IsLegalTransition entirely.
		"Edit(" + worktree + "/.claude/argus/**)",
		"Write(" + worktree + "/.claude/argus/**)",
	}
	for _, p := range ownSettings {
		deny = append(deny, "Edit("+p+")", "Write("+p+")")
	}
	// Belt and suspenders alongside checkToolHook: DenyFloor's commands are
	// unremovable regardless of what any phase's allow config says, so they
	// are denied natively here too, not only caught live by the hook.
	deny = append(deny, bashGlobEntries(protocol.DenyFloor())...)

	return permissionSettings{
		Permissions: permissionBlock{
			Allow: allow,
			Deny:  deny,
		},
		Hooks:                      hooksBlock{PreToolUse: []hookMatcher{checkToolHook()}},
		EnableAllProjectMcpServers: true,
	}
}
