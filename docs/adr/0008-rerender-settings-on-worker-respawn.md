# 0008. Re-render settings.local.json on worker respawn

Date: 2026-08-13
Status: Accepted

## Context

- `settings.local.json` is rendered once, at session launch, by `provisionWorktree` (`internal/supervisor/loop.go`) when a worktree is first spawned — Claude Code reads it only at startup, never again mid-session.
- `dispatchIntoPane` (`cmd/rebase.go`), shared by `rework` and `rebase`, re-dispatches into an existing worktree's pane: a live agent is re-tasked in place, but a pane with no live agent falls back to spawning a fresh Claude Code session in the same worktree.
- That spawn-new-agent fallback used to launch straight into whatever `settings.local.json` the worktree's *original* spawn baked in, with no re-render — a respawned worker ran under a permission set that could be arbitrarily stale relative to the repo's current `.argus/config.yml` or argus's own schema.
- `rework`'s and `rebase`'s dispatch paths already resolve the repo's config from the trusted main checkout (`supervisor.RepoRoot`, i.e. `git --git-common-dir`) for their own gate/review logic — that same resolved config was reachable at the dispatch site but wasn't being used to refresh the pane it was about to relaunch.

## Decision

- `dispatchIntoPane`'s spawn-new-agent branch calls `reRenderSettingsBeforeSpawn`, which re-renders `settings.local.json` via `supervisor.WriteSettings`, immediately before the fresh session launches.
- The config fed into that render is always the one resolved through `supervisor.RepoRoot` at the main checkout — never the worktree's own editable `.argus/config.yml` copy.
- The rebase phase's git fetch/merge grant is preserved across the re-render: `rebase.go` passes its own freshly computed grant, and `rework.go`'s `respawnRebaseAllow` recomputes the equivalent grant from the worktree's persisted spawn base when a rework round respawns.
- Deny-floor subtraction is left entirely to `stripDenyFloor`, already applied inside `WriteSettings`'s own allow resolution — this decision only concerns the source and timing of the render, not the floor logic itself.

## Rejected

- Trust the original spawn's baked settings on respawn. Rejected: leaves a relaunched session permanently stale after any schema or config change, with no way to pick up a later fix short of tearing down the worktree.
- Render from the worktree's own `.argus/config.yml`. Rejected: that copy sits inside the worker's own writable tree — trusting it on respawn would let a worker widen its own grants by editing a file it controls.
- Re-render in the live-agent-reuse branch too. Rejected: pointless — `settings.local.json` is read only at launch, and reusing a live agent never relaunches the session that would read it.

## Consequences

- A respawned worker always launches under the repo's current, main-checkout-resolved permission set, closing the staleness gap between a worktree's original spawn and any later config or schema change.
- The trust boundary is explicit: only the main checkout's config can ever widen a respawned worker's grants, never the worktree's own copy.
- Every spawn-new-agent respawn now costs one extra config read and settings render, negligible next to the pane spawn it precedes.
