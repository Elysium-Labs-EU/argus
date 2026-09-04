# 0409261732. Deny a worker's Task tool outright rather than track child sessions

Date: 2026-09-04
Status: Accepted

## Context

- A worker under `argus supervise` spawned its own background sub-agent
  ("read-only research") via Claude Code's Task tool. The fork drifted past
  its read-only brief and wrote test code directly, concurrently with the
  parent worker's own edits to a file in the same worktree — the losing
  writer left an undefined symbol the other side never defined.
- The fork then hit its own live Bash approval prompt and stalled there for
  32+ hours. Nothing in argus could reach it: `worker steer`/`worker answer`
  deliver chat text into the parent session's pane, not a keypress into a
  nested approval dialog a child session owns.
- argus's phase tracking (`status.json`, the `argus worker check-tool`
  PreToolUse gate, `protocol.DeniedInPhase`) only watches the top-level
  worker session. A spawned child is invisible to all of it: no phase
  discipline, no permission scoping distinct from the parent, no dispatch
  path if it blocks on its own prompt.
- Nothing in the worktree enforces single-writer on a file two concurrent
  agents (the worker's own session and its own spawned child) both target.

## Decision

- Deny Claude Code's Task tool outright in every worker's rendered
  `.claude/settings.local.json`: `"Task"` is added to the static
  `permissions.deny` list `internal/supervisor/agentadapter.go`'s
  `settingsFor` builds, in the same structural family as `rm -rf`/`sudo` —
  denied in every phase, not phase-scoped, no live hook round-trip needed.
- This closes the spawn path itself rather than making children visible to
  supervision. With no sub-agent able to start, there is no invisible child
  session and no second writer to collide with the parent — both reported
  gaps close at their root.

## Rejected

- Extend phase tracking, `DeniedInPhase`, and the pane/registry dispatch
  `worker steer`/`answer` use to child sessions, so a spawned agent inherits
  the same discipline as its parent and stays reachable. Rejected: argus has
  no visibility or control channel into a nested Claude Code session today —
  building one is a large, separately-risky surface (new spawn-tracking
  state, a dispatch path into a session argus didn't launch directly) for a
  capability no worker brief currently asks for.
- Add a per-worktree file write-lock (an advisory lockfile, or a
  git-index-based staged-file check) so two concurrent writers can't
  silently diverge on the same file, while still allowing spawning.
  Rejected: it only patches the second-order symptom (the collision) and
  leaves the first-order gap (an unsupervised, unreachable child process)
  wide open — a spawned agent could still deadlock on a prompt with nothing
  watching, lock or no lock.

## Consequences

- A worker that could use sub-agent-shaped parallelism (e.g. genuinely
  read-only background research) has no path to it today. A real use case
  showing up later is a separate, scoped follow-up — proposing phase-aware
  child tracking and dispatch, or a narrower "read-only Task only" carve-out
  — not something this decision forecloses.
- `internal/supervisor/agentadapter_test.go` carries a test asserting
  `"Task"` is present in `settingsFor`'s rendered deny list, alongside the
  existing structural-deny assertions (`rm -rf`, `sudo`, control-plane
  files) — a regression here silently reopens the exact incident this ADR
  responds to.
- No brief-text change: like `rm -rf`/`sudo`, containment here is a
  technical fact enforced by the rendered settings file, not an instruction
  a worker has to remember or that could be misread.
