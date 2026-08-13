# Orchestrator-Bash-permission boundary needs no ARGUS_WORKER guard

Date: 2026-08-13
Status: Accepted

## Context

- `0002-orchestrator-governance.md` established that a Claude Code *hook*
  configured in the operator's user-global settings merges into every
  session on the machine, workers included — so an orchestrator-only hook
  must guard on `ARGUS_WORKER=1` to no-op inside a worker.
- `argus config check`/`config check --write` reads/writes a different
  mechanism entirely: `.claude/settings.json`'s `permissions.allow`/`deny`,
  the Bash allowlist/denylist the *orchestrating* session (the one running
  `supervise`/`ship`/`rework`) needs so its own `argus`/herdr-pane calls
  don't prompt.
- Nothing in SKILL.md's config-check section stated whether this file is
  subject to the same cross-session merge risk as a hook — a reasonable
  reader of `0002` could assume it needs an equivalent `ARGUS_WORKER` guard,
  or worse, assume it silently doesn't apply to workers without knowing why.

## Decision

- Document explicitly, next to config-check's own section in SKILL.md and
  in `config check --write`'s own pane-deny success output, that
  `.claude/settings.json` is not the hook-merge case: a worker's Bash
  permissions come entirely from its own per-worktree rendered
  `settings.local.json` (`internal/supervisor/permissions.go`,
  `agentadapter.go`), a file a worker never reads `.claude/settings.json`
  to produce. `config check` never touches a worktree's
  `settings.local.json` at all.
- No `ARGUS_WORKER`-style guard is needed for this file, because there is no
  leak to guard against — the two settings live in structurally separate
  files, not a single merged one.

## Rejected

- Add an `ARGUS_WORKER` guard note to `config check`'s output/docs "to be
  consistent" with the hook guidance. Rejected: there is nothing to guard —
  a worker never reads `.claude/settings.json`, so a guard here would
  document a non-existent hazard and imply the two mechanisms share a risk
  they don't.

## Consequences

- An operator who read `0002` and then reads `config check`'s docs no
  longer has to guess whether the same leak applies here — the boundary is
  named explicitly, with the reason it doesn't recur.
- `internal/permission` (the package backing `config check`) and
  `internal/supervisor/permissions.go` (the package backing a worker's
  rendered `settings.local.json`) remain independent, as they already were;
  this ADR records why that independence is the intended design, not an
  oversight to reconcile.
