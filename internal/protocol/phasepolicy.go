package protocol

import (
	"slices"
	"strings"
)

// ConfigurablePhases are the phases a worker report or a repo's phase policy
// may name. PhaseDone is excluded, same as reportablePhases always excluded
// it — only argus's own ship path ever sets it. PhaseRebase is excluded too,
// for a different reason: it is argus-stamped at dispatch time, never
// reported by a worker and never configurable via .argus/config.yml — its
// own grant (see supervisor.RebasePhaseAllow) is computed live from the
// worktree's recorded base and the repo's already-configured verify
// command, not something an operator writes phases.rebase.allow for.
var ConfigurablePhases = []Phase{PhasePlanning, PhaseWorking, PhaseSelfTest, PhaseAwaitingReview, PhaseBlocked}

// PhasePolicy is one repo's configured additions for a single phase: Deny
// adds Bash prefixes on top of DeniedInPhase's floor; Skip drops this
// policy's own Deny (never the floor). Allow holds Claude Code permission
// patterns (e.g. "Bash(go test *)") granted only while a worker reports this
// phase — see internal/supervisor's ResolvedAllowForPhase, the resolver that
// unions it with the structural floor and strips anything overlapping
// DenyFloor. Deny/Allow are ordered before Skip for struct alignment
// (fieldalignment-enforced), not logical order.
type PhasePolicy struct {
	Deny  []string
	Allow []string
	Skip  bool
}

// PhaseConfig is a repo's full per-phase policy, keyed by ConfigurablePhases
// values.
type PhaseConfig map[Phase]PhasePolicy

// AlwaysDeniedCommands are Bash prefixes denied in every phase, independent
// of DeniedInPhase's per-phase table: argus's own supervisor-side commands, a
// worker must never invoke on itself. A worker's only legitimate
// self-invocations are `argus worker report/answer/steer`; ship/rework/
// review/supervise are the operator's (or supervising session's) own tools —
// a worker running `argus ship --force` on itself would commit/push/open a
// PR and bypass the entire verdict-required gate, the same severity as
// committing before a plan exists, just for a different command set. Nothing
// prevented this before: an unlisted Bash command falls through to Claude
// Code's default ask-prompt, which just hangs a headless worker rather than
// actually blocking it.
//
// `argus worker record-plan` belongs here for a related but distinct reason:
// it is not an operator tool, but it is also not a worker's own
// self-invocation — it is wired exclusively as a PostToolUse hook Claude Code
// itself fires (see recordPlanHooks), never something a worker's own turn is
// meant to type at a Bash prompt. Denying it here closes the same hole
// stripDenyFloor closes for the others: without it, a repo's own
// phases.<name>.allow could grant a wide-enough Bash pattern to let a worker
// forge plan-log.jsonl entries by hand, defeating the whole point of a
// recorder the worker's own Bash allowlist can't reach.
var AlwaysDeniedCommands = []string{"argus ship", "argus rework", "argus review", "argus supervise", "argus worker record-plan"}

// DeniedInPhase returns the Bash command prefixes denied while a worker
// reports phase p, before any repo config is applied — the floor
// ResolvedDenyForPhase always includes and no config can remove. It is
// exactly DenyFloor() for every phase: a worker never commits or pushes at
// all, in any phase — only argus ship does, once a verdict exists — so there
// is no phase where allowing the underlying git command becomes safe. p is
// kept as a parameter (rather than dropped now that it's unused) because
// ResolvedDenyForPhase and every caller are phase-shaped call sites; a future
// phase-specific addition to the floor should not need to change every
// signature along the chain again.
func DeniedInPhase(_ Phase) []string {
	return DenyFloor()
}

// DenyFloor is the hardcoded, unremovable set of Bash command prefixes
// denied in every phase: AlwaysDeniedCommands plus AskGatedCommands (git
// commit, git push). Subtracted last from any resolved *allow* set (see
// internal/supervisor's ResolvedAllowForPhase) as well as being the floor
// DeniedInPhase always returns — the same two facts, read from the two
// different directions a repo's config can try to approach them from (an
// over-broad allow entry, or a config trying to Skip its way past a deny).
// No phases.<any>.allow entry, materialized toolchain command, or --allow
// flag can re-grant any of these: a worker edits files and reports; argus
// ship is the only path that ever runs git commit or git push.
func DenyFloor() []string {
	floor := slices.Clone(AlwaysDeniedCommands)
	return append(floor, AskGatedCommands...)
}

// ResolvedDenyForPhase merges DeniedInPhase's floor with project's own
// configured additions for p, deduped. project[p].Skip drops project's own
// Deny for p but never the floor — a repo can only add restrictions on top
// of the floor, never remove from it.
func ResolvedDenyForPhase(p Phase, project PhaseConfig) []string {
	denied := DeniedInPhase(p)
	policy, ok := project[p]
	if !ok || policy.Skip {
		return denied
	}
	for _, d := range policy.Deny {
		if !slices.Contains(denied, d) {
			denied = append(denied, d)
		}
	}
	return denied
}

// credentialDenyFloor is CredentialDenyFloor's backing data, seeded from
// Trail of Bits' trailofbits/claude-code-config: Claude Code permission
// rules are deny-wins with no re-allow, so a denylist is the only
// expressible defense for the Read/Edit tools, which read straight through
// the OS sandbox that gates every Bash subprocess (a worker inherits the
// orchestrator's HOME, so without this a worker's Read/Edit tools can reach
// ~/.ssh, ~/.aws, and similar operator credential files the sandbox never
// sees). Deliberately non-exhaustive: this narrows the common, well-known
// credential shapes, not every file that could ever hold a secret.
//
// The tilde-slash form is used literally rather than through
// absPathPattern's filesystem-absolute rendering: Read/Edit rules resolve
// "~/" themselves, with no HOME expansion needed on argus's side.
// ~/.claude/** is intentionally home-anchored only, never a bare wildcard —
// a worktree has its own .claude/ a worker must be able to read.
var credentialDenyFloor = []string{
	"Read(~/.ssh/**)", "Edit(~/.ssh/**)",
	"Read(~/.aws/**)",
	"Read(~/.azure/**)",
	"Read(~/.config/gh/**)",
	"Read(~/.git-credentials)",
	"Read(~/.gnupg/**)",
	"Read(~/.docker/config.json)",
	"Read(~/.kube/**)",
	"Read(~/.npmrc)",
	"Read(~/.npm/**)",
	"Read(~/.pypirc)",
	"Read(~/.gem/credentials)",
	"Read(~/.claude/**)",
	"Read(~/Library/Keychains/**)",
	"Read(~/Library/Application Support/**/metamask*/**)",
	"Read(~/Library/Application Support/**/electrum*/**)",
	"Read(~/Library/Application Support/**/exodus*/**)",
	"Read(~/Library/Application Support/**/phantom*/**)",
	"Read(~/Library/Application Support/**/solflare*/**)",
	"Edit(~/.bashrc)",
	"Edit(~/.zshrc)",
	"Read(**/.ssh/**)", "Edit(**/.ssh/**)",
	"Read(**/.aws/**)",
	"Read(**/.config/gh/**)",
	"Read(**/.git-credentials)",
	"Read(**/.netrc)",
}

// CredentialDenyFloor returns the Claude Code Read/Edit permission-deny
// patterns carving operator credential files out of every worker's
// settings.local.json — see credentialDenyFloor's doc comment.
func CredentialDenyFloor() []string {
	return slices.Clone(credentialDenyFloor)
}

// MatchesDeniedCommand reports whether cmd starts with one of denied's
// prefixes, so "git commit -m foo" matches "git commit".
func MatchesDeniedCommand(cmd string, denied []string) (matched string, ok bool) {
	trimmed := strings.TrimSpace(cmd)
	for _, prefix := range denied {
		if strings.HasPrefix(trimmed, prefix) {
			return prefix, true
		}
	}
	return "", false
}
