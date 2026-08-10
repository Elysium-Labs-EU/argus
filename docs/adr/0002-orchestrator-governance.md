# 0002. Orchestrator-session governance: worker-scoped, with a worker-identity signal

Date: 2026-08-10
Status: Accepted

## Context

- argus governs workers: it renders each worktree's settings.local.json, injects the check-tool PreToolUse hook, and enforces the deny floor.
- The orchestrator session, the one running supervise/ship/rework, is governed only by the operator's personal hooks under the user home, outside argus.
- Claude Code merges the user-global settings hooks into every session on the machine, workers included, so an orchestrator-intended global hook leaks into workers.
- A personal Stop hook that blocks turn-end while a typed task is open leaked into workers. Workers escaped only because their briefs used the todo tool, which that hook ignored, not the typed-task tool. That escape was an accident, not a boundary.

## Decision

- argus stays worker-scoped for behavioral governance. It does not install or sync orchestrator hooks.
- argus guarantees a stable, documented worker-identity signal: it exports ARGUS_WORKER=1 into every worker spawn environment.
- Orchestrator-global hooks gate on that signal to no-op inside workers, returning early when ARGUS_WORKER is set.
- Orchestrator permission allow-sets stay managed by argus config check --write, the existing precedent for orchestrator-side config.

## Rejected

- argus owns and installs an orchestrator profile, hooks plus permissions, into the operator's session. Rejected: it needs argus to write the operator's settings before argus is trusted (a bootstrap-trust loop), inherits the per-clone untracked-settings sync problem config check already has, and widens argus's blast radius from worktrees to the operator's own session.
- Relying on the todo-versus-typed-task accident to keep orchestrator hooks out of workers. Rejected: it is implicit and breaks the day a worker brief uses the typed-task tool.

## Consequences

- The leak becomes a contract: any orchestrator hook that must not run in a worker checks ARGUS_WORKER and exits early.
- Workers keep full use of both the todo tool and the typed-task tool. Plan evidence already accepts either marker, TodoWrite or TaskCreate, enforced at report time and at review time. The signal only suppresses orchestrator hooks, never the worker's own task tooling, so a worker using typed tasks is no longer blocked by a leaked guard.
- The operator still owns the orchestrator hook set. argus documents the ARGUS_WORKER guard idiom rather than shipping the hooks.
- No bootstrap or cross-clone sync burden: the signal rides the worker spawn line, which argus already controls.
