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
	// run; rebaseAllow is RebasePhaseAllow's own git-command grant for this
	// worktree's base, unioned in unconditionally (see ResolvedAllowSet's
	// doc comment for why it can't wait for a live PreToolUse decision). The
	// rendered allow list is the union of every configurable phase's own
	// resolved allow (see ResolvedAllowForPhase) plus rebaseAllow — the
	// static session-wide file can't itself be phase-conditional, so the
	// live PreToolUse hook (argus worker check-tool) narrows it back down to
	// the worker's actual current phase on every call. sandboxEnabled and
	// sandboxAllowWrite are the repo's (or --experimental-sandbox's)
	// experimental OS-sandbox toggle and its filesystem write-allow list —
	// see settingsFor.
	RenderSettings(worktree string, project protocol.PhaseConfig, baseAllow, extraAllow, rebaseAllow []string, sandboxEnabled bool, sandboxAllowWrite []string) (path string, content []byte, err error)

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

func (claudeCodeAdapter) RenderSettings(worktree string, project protocol.PhaseConfig, baseAllow, extraAllow, rebaseAllow []string, sandboxEnabled bool, sandboxAllowWrite []string) (string, []byte, error) {
	settings := settingsFor(worktree, project, baseAllow, extraAllow, rebaseAllow, sandboxEnabled, sandboxAllowWrite)
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
	Sandbox                    *sandboxSettings `json:"sandbox,omitempty"`
	Permissions                permissionBlock  `json:"permissions"`
	Hooks                      hooksBlock       `json:"hooks"`
	EnableAllProjectMcpServers bool             `json:"enableAllProjectMcpServers"`
}

// sandboxSettings mirrors the "sandbox" key Claude Code reads from
// settings.local.json to enable its own OS sandbox (seatbelt on macOS,
// bubblewrap on Linux) around a worker's Bash subprocesses — the vector
// protocol.CredentialDenyFloor's in-process Read/Edit deny does not cover
// (a worker can still `cat ~/.ssh/id_ed25519` via Bash without it). Rendered
// by settingsFor only when the repo (or --experimental-sandbox) opts in;
// omitted entirely otherwise, via permissionSettings.Sandbox's omitempty.
//
// Filesystem is deliberately narrow: no denyRead and no whole-home
// whitelist. A denyRead of the home directory was tested and breaks
// toolchain cache reads/writes with no security gain over Credentials'
// already-targeted deny list.
type sandboxSettings struct {
	Filesystem               *sandboxFilesystem `json:"filesystem,omitempty"`
	Credentials              sandboxCredentials `json:"credentials"`
	Enabled                  bool               `json:"enabled"`
	AutoAllowBashIfSandboxed bool               `json:"autoAllowBashIfSandboxed"`
	AllowUnsandboxedCommands bool               `json:"allowUnsandboxedCommands"`
	FailIfUnavailable        bool               `json:"failIfUnavailable"`
}

// sandboxCredentials is sandboxSettings' credential-file deny list — see
// credentialDenyPaths for the fixed set of paths it always carries.
type sandboxCredentials struct {
	Files []sandboxCredentialFile `json:"files"`
}

// sandboxCredentialFile is one denied credential path entry. Mode is always
// "deny": this block only ever blocks read access to a fixed, known-secret
// path, never grants one.
type sandboxCredentialFile struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
}

// sandboxFilesystem carries only the sandbox's write-allow list — see
// sandboxSettings' own doc comment for why denyRead and a home whitelist are
// deliberately absent. Referenced only via permissionSettings.Sandbox's own
// pointer field, and itself omitted (nil) whenever sandboxAllowWrite is
// empty, so an experimental-sandbox repo with no extra write paths renders
// no filesystem block at all rather than an empty one.
type sandboxFilesystem struct {
	AllowWrite []string `json:"allowWrite"`
}

// sandboxCredentialMode is the only Mode sandboxCredentialFile ever carries:
// this block exists to deny read access to known-secret paths, never to
// grant one.
const sandboxCredentialMode = "deny"

// credentialDenyPaths are the credential files/directories the experimental
// OS sandbox always denies read access to, regardless of
// sandboxAllowWrite — the same class of path
// protocol.CredentialDenyFloor already denies to the in-process Read/Edit
// tools, extended here to the Bash/subprocess vector those tools don't
// cover. Fixed and non-configurable: a repo opts the sandbox on or off as a
// whole, not path by path.
var credentialDenyPaths = []string{
	"~/.ssh",
	"~/.aws",
	"~/.azure",
	"~/.config/gh",
	"~/.git-credentials",
	"~/.gnupg",
	"~/.docker/config.json",
	"~/.kube",
	"~/.npmrc",
	"~/.pypirc",
	"~/.gem/credentials",
}

// sandboxBlockFor builds the sandbox settings block, or nil when enabled is
// false — the pointer settingsFor assigns straight to
// permissionSettings.Sandbox, whose own omitempty then drops the whole
// "sandbox" key from a disabled worker's rendered settings.local.json.
func sandboxBlockFor(enabled bool, allowWrite []string) *sandboxSettings {
	if !enabled {
		return nil
	}
	files := make([]sandboxCredentialFile, len(credentialDenyPaths))
	for i, p := range credentialDenyPaths {
		files[i] = sandboxCredentialFile{Path: p, Mode: sandboxCredentialMode}
	}
	s := &sandboxSettings{
		Enabled:                  true,
		AutoAllowBashIfSandboxed: true,
		AllowUnsandboxedCommands: false,
		FailIfUnavailable:        true,
		Credentials:              sandboxCredentials{Files: files},
	}
	if len(allowWrite) > 0 {
		s.Filesystem = &sandboxFilesystem{AllowWrite: allowWrite}
	}
	return s
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
// key. PreToolUse gates a call before it runs (see checkToolHooks);
// PostToolUse observes one after it already ran (see recordPlanHooks) and can
// never block it.
type hooksBlock struct {
	PreToolUse  []hookMatcher `json:"PreToolUse,omitempty"`
	PostToolUse []hookMatcher `json:"PostToolUse,omitempty"`
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

// checkToolHooks wires `argus worker check-tool` as a PreToolUse hook on
// every worker's Bash, Edit, and Write tool calls. The static allow/deny
// lists above are read once at session launch and can only ever be the
// union across every phase (dontAsk has no notion of "phase"), so a
// phase-conditional rule needs a hook that re-checks the worktree's current
// status.json live instead: DeniedInPhase's floor and ResolvedAllowForPhase's
// own per-phase scoping for Bash, and PhaseAllowsMutation for Edit/Write —
// the static Allow list can grant Edit/Write(worktree) for the whole
// session (it has to, since working/self_test/rebase genuinely need it and
// the file can't be re-rendered mid-session), so only this live check
// actually keeps a worker from mutating tracked files outside those phases.
// One hookMatcher per tool name rather than a single combined matcher
// pattern, since Claude Code's own matcher syntax is documented per exact
// tool name, not as a general regex.
func checkToolHooks() []hookMatcher {
	hooks := []hookEntry{{Type: "command", Command: "argus worker check-tool"}}
	matchers := []string{"Bash", "Edit", "Write"}
	out := make([]hookMatcher, len(matchers))
	for i, m := range matchers {
		out[i] = hookMatcher{Matcher: m, Hooks: hooks}
	}
	return out
}

// recordPlanHooks wires `argus worker record-plan` as a PostToolUse hook on
// every worker's TodoWrite and TaskCreate/TaskUpdate calls: the live,
// argus-owned recorder HasFreshPlanEvidence checks against, replacing the
// one-shot, whole-transcript grep HasPlanEvidence used to be the only signal
// for. PostToolUse fires after the tool already ran and can never block it —
// see runWorkerRecordPlan — so this is pure observation, the write half of
// the same "record live, enforce per phase" split checkToolHooks' PreToolUse
// gate implements for command/mutation denial. Two matchers, not one
// "TodoWrite|TaskCreate|TaskUpdate" combined pattern, for the same reason
// checkToolHooks registers one hookMatcher per tool name: Claude Code's own
// hook matcher syntax is documented per exact tool name (or a
// pipe-alternation within one), not as a single cross-tool regex.
func recordPlanHooks() []hookMatcher {
	hooks := []hookEntry{{Type: "command", Command: "argus worker record-plan"}}
	matchers := []string{"TodoWrite", "TaskCreate|TaskUpdate"}
	out := make([]hookMatcher, len(matchers))
	for i, m := range matchers {
		out[i] = hookMatcher{Matcher: m, Hooks: hooks}
	}
	return out
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
// one-off run. rebaseAllow is RebasePhaseAllow's own grant for this
// worktree's base, unioned in unconditionally rather than gated by phase —
// see ResolvedAllowSet's doc comment for why the rebase phase can't wait for
// a live PreToolUse decision the way every other phase does.
//
// The rendered Allow list is ResolvedAllowSet's union of every
// protocol.ConfigurablePhases value's own ResolvedAllowForPhase plus
// rebaseAllow, since this file is read once at session launch and can't
// itself vary by the worker's current phase — checkToolHook is what
// actually narrows a live Bash call down to what the *current* phase's own
// resolved set allows.
//
// sandboxEnabled and sandboxAllowWrite are the experimental, default-off OS
// sandbox toggle (see sandboxBlockFor): false renders no "sandbox" key at
// all, leaving today's behavior unchanged.
func settingsFor(worktree string, project protocol.PhaseConfig, baseAllow, extraAllow, rebaseAllow []string, sandboxEnabled bool, sandboxAllowWrite []string) permissionSettings {
	allow := ResolvedAllowSet(project, baseAllow, extraAllow, rebaseAllow, worktree)

	// Deny wins over allow in Claude Code regardless of pattern specificity
	// (deny/allow are checked in that order, first match wins), so these
	// entries carve the worker's own permission files out of the
	// Edit/Write(glob) allows folded into ResolvedAllowSet above: without
	// them a worker could rewrite its own settings.local.json mid-session
	// and grant itself capability the operator never approved. WriteSettings
	// only resets the file at the start of the *next* run, which leaves the
	// entire current session unprotected.
	ownSettings := []string{
		absPathPattern(worktree + "/.claude/settings.local.json"),
		absPathPattern(worktree + "/.claude/settings.json"),
	}
	argusControlPlaneGlob := absPathPattern(worktree + "/.claude/argus/**")
	deny := []string{
		"Bash(rm -rf *)",
		"Bash(git worktree remove*)",
		"Bash(git worktree prune*)",
		"Bash(git clean -f*)",
		"Bash(git reset --hard*)",
		"Bash(trash *)",
		"Bash(sudo *)",
		// A spawned sub-agent is invisible to argus's phase tracking and has
		// no dispatch path if it hits its own approval prompt (worker steer/
		// answer only reach the parent session) — closing the spawn path
		// itself, rather than extending phase tracking and pane dispatch to
		// child sessions, is a structural deny like the entries above it, not
		// a phase-scoped one.
		"Task",
		// .claude/argus/ holds status.json/verdict.json/lifecycle.json —
		// argus's own control plane, mutated only by its in-process writers
		// and by `argus worker report` (a Bash subprocess this deny does not
		// touch). Without this, a worker's Edit/Write tool can hand-edit
		// status.json straight to awaiting_review or write an approving
		// verdict.json, skipping IsLegalTransition entirely. Rendered via
		// absPathPattern like the allow glob it carves out of (see
		// structuralFloorAllow) — a single-slash deny here would match
		// nothing once the allow glob is filesystem-absolute, silently
		// reopening the control plane to Edit/Write.
		"Edit(" + argusControlPlaneGlob + ")",
		"Write(" + argusControlPlaneGlob + ")",
	}
	for _, p := range ownSettings {
		deny = append(deny, "Edit("+p+")", "Write("+p+")")
	}
	// Belt and suspenders alongside checkToolHook: DenyFloor's commands are
	// unremovable regardless of what any phase's allow config says, so they
	// are denied natively here too, not only caught live by the hook.
	deny = append(deny, bashGlobEntries(protocol.DenyFloor())...)
	// Read/Edit deny-wins-with-no-re-allow floor for operator credential
	// files (~/.ssh, ~/.aws, and similar) a worker's Edit/Write(worktree)
	// glob never grants but its Read tool could otherwise still reach — see
	// protocol.CredentialDenyFloor.
	deny = append(deny, protocol.CredentialDenyFloor()...)

	return permissionSettings{
		Permissions: permissionBlock{
			Allow: allow,
			Deny:  deny,
		},
		Hooks:                      hooksBlock{PreToolUse: checkToolHooks(), PostToolUse: recordPlanHooks()},
		Sandbox:                    sandboxBlockFor(sandboxEnabled, sandboxAllowWrite),
		EnableAllProjectMcpServers: true,
	}
}
