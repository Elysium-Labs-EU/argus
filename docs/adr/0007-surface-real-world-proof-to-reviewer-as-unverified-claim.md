# 0007. Surface real_world_proof to the reviewer as an unverified claim

Date: 2026-08-13
Status: Accepted

## Context

- `ReviewPolicy.Assess` (`review.go:134`) escalates a proof-required-path change when `status.RealWorldProof` is empty — a mechanical, non-emptiness gate.
- `reviewPrompt` (`reviewer.go`) built the LLM reviewer's prompt with no reference to `RealWorldProof` at all — the field reached the gate but never reached the model that renders the actual verdict.
- The two halves of the gate silently disagreed about what "proof" means: the deterministic half treated presence as sufficient to avoid escalation, while the reviewer, never shown the text, treated its own absence as "no proof supplied" — even against genuine, multi-kilobyte evidence — and burned a rework round asking the worker to resubmit proof it had already given.
- argus's own philosophy is that self-report is a hint, not truth (see `workerState.effective`, and the HardReasons split in `review.go`/`reviewer.go`) — any fix that closed this gap by having the reviewer simply believe the proof text would trade one failure mode for a worse one.

## Decision

- When a change touches a `ProofRequiredPaths` match, include the worker's `real_world_proof` in the reviewer prompt (`ProofForReview` in `review.go`, threaded through `ReviewRequest.RealWorldProof`).
- Delimit it with the diff block's own existing fenced-code convention and label it explicitly as an UNVERIFIED WORKER CLAIM, not established fact — the reviewer is told the gate only checked non-emptiness, and that judging whether the text actually demonstrates what it claims is the reviewer's job.
- Leave `review.go:134`'s non-emptiness check exactly as it is: it stays the cheap, mechanical presence gate. The reviewer is the only place that judges quality.

## Rejected

- Gate judges quality (e.g. keyword/length heuristics on `real_world_proof` before escalation). Rejected: `Assess` is a deterministic, LLM-free cheap path by design — hunting for what makes proof text convincing is exactly the kind of judgment call that belongs to the reviewer, not a string-matching gate.
- Reviewer trusts proof as fact once shown it. Rejected: this is the same self-report-as-truth failure the HardReasons split already exists to prevent elsewhere in this gate — a worker's own claim about its own testing is evidence to weigh, never a substitute for the reviewer verifying it against the actual diff and files.

## Consequences

- The reviewer stops reporting "no proof supplied" against genuine evidence it was simply never shown, cutting spurious rework rounds.
- Presence and quality stay split across two places by design: the gate never grows LLM-shaped logic, and the reviewer never gets to skip judgment just because a field is non-empty.
- A worker can still write proof text that satisfies the presence check but fails to demonstrate the claim on inspection — the reviewer requesting changes in that case is the fix working as intended, not a regression.
