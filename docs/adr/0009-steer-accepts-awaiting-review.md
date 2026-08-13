# 0009. Let steer reach a worker reporting awaiting_review

Date: 2026-08-13
Status: Accepted

## Context

- `argus worker steer` refused every phase but `working`, pointing the caller at `argus rework` for anything else.
- A worker in `awaiting_review` still has a live pane and running agent — the same pane a `working` steer delivers into.
- A report-only defect caught at review time forced a full rework round just to correct something the worker's own live pane could fix in one turn.
- `MaxSteersPerWorking` and the per-report `Steers` reset already scope a steer to "whichever phase leg is live," not literally to `PhaseWorking`.

## Decision

- `runWorkerSteer` accepts a target reporting `working` or `awaiting_review`; both have a live pane a note can reach.
- Delivery is unchanged for either phase: same `supervisor.SteerMessage`, same live-pane path, same `MaxSteersPerWorking` budget and reset.
- Steer still never changes phase — a steered `awaiting_review` worker stays `awaiting_review` and reports its own next phase.

## Rejected

- Working-only. Too strict — forces a needless rework round for a defect the live pane could fix directly.
- Any phase, including `blocked`/`done`. Wrong — `blocked` needs a decision via `argus worker answer`, not a nudge, and a shipped/done worker has no live pane left.

## Consequences

- A report-only defect found at review time costs one steer instead of a full rework dispatch.
- `blocked` and terminal phases stay refused; steer still isn't a substitute for `argus worker answer` or the phase table.
- Steer not reaching a `--worker-placement` tab pane is a separate addressing bug, left out of scope here.
