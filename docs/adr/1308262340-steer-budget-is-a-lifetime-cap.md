# Steer budget is a lifetime cap, not a per-phase-leg cap

Date: 2026-08-13
Status: Accepted

## Context

- The steer trace (`Status.Steers`) used to be erased on a worker's own next
  phase report — `runWorkerReport` never carried it forward the way it does
  `Base`/`Title`/`Question`/`Answer`, so each fresh phase leg started with an
  empty trace.
- `MaxSteersPerWorking` counts *delivered* entries in that trace. With the
  trace cleared on every report, the cap was in practice scoped to whichever
  phase leg was currently live: a worker crossing into a new leg reset the
  count to zero, even though the cap's own doc comment already called it a
  budget spanning "the worktree's whole lifetime."
- That mismatch is a durability bug: the trace an operator relies on for
  "how many times has this worktree been steered" silently loses history at
  every phase transition, and the budget resets alongside it.
- Fixing the durability bug — carrying `Steers` forward on report, the same
  way `Base` already is — necessarily also fixes the budget: once the trace
  is never cleared, `MaxSteersPerWorking` is counted against its full
  lifetime total, not a per-leg subset.

## Decision

- The steer trace persists across every phase report for a worktree's whole
  lifetime; a worker's own report body never clears or sets it.
- `MaxSteersPerWorking` is therefore a lifetime cap per worktree, counted
  against the full persisted trace, not a per-phase-leg cap that resets on
  report.
- This supersedes the "a worker's own next phase report resets the budget"
  clause of the prior steer design (recorded in `worker_steer.go`'s command
  help before this change).
- A lifetime cap follows necessarily from making the trace durable, and
  serves the same anti-abuse goal more strictly: steer must not become an
  unbounded side-channel a supervisor leans on instead of the
  phase-transition table, and a cap that resets every leg was always looser
  than one that doesn't.

## Rejected

- Reset the budget per leg by filtering `Steers` on phase-transition
  timestamps. More machinery for a weaker invariant — a lifetime cap is
  simpler and strictly serves the anti-abuse intent better.
- Keep clearing the trace on report to preserve the per-leg reset. Rejected:
  that is the exact durability bug this change fixes — an operator loses the
  steer history at every phase transition.

## Consequences

- An operator can always see the full steer history for a worktree, not just
  whatever survived the most recent phase transition.
- A worktree that has already spent its `MaxSteersPerWorking` budget on
  earlier legs stays capped on later ones; steering past the cap needs
  `argus rework` instead, the same as before, just scoped to the worktree
  rather than the leg.
- `worker_steer.go` and `status.go`'s doc comments already describe the
  lifetime cap; this ADR is the durable record of the decision they assume.
