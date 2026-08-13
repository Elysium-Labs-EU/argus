# 0005. Measure and review a worker's diff against the merge-base, not base's moving tip

Date: 2026-08-13
Status: Accepted

## Context

- `MeasureDiff` (the gate's ground-truth size/files measurement) ran `git diff --numstat -z <base>`, and `DiffFor` (what the LLM reviewer reads) ran `git diff <base>` — both a plain two-dot diff against `base`'s current tip.
- A worker never commits or rebases while it works, so its worktree HEAD stays wherever it branched. `base` (e.g. `origin/main`) is a live ref that keeps moving as other work merges to it.
- Once `base` advances past the worktree's spawn point, a two-dot diff against `base`'s tip includes a revert of every intervening merge: everything `base` gained since the branch point shows up as deletions the branch never made, and the reviewer sees phantom reverts of already-merged work alongside the worker's real change.
- The inflated size regularly exceeded the worker's own honest self-report, which `underReportReason` (review.go) treats as an unwaivable hard reason — a change with no real defect could be permanently unshippable, blocked on staleness with no route through `--review` to override it.
- `MeasureDiff` and `DiffFor` derived their own diff target independently, so even a partial fix to one would leave the other showing a different diff than what the gate actually measured.

## Decision

- Add one helper, `ResolveEffectiveDiffBase`, that resolves `git merge-base(base, HEAD)` — the three-dot equivalent a merge would actually apply — and route both `MeasureDiff` and `DiffFor` through it instead of diffing against `base` directly.
- Every existing call site (the gate's `measureReconcileDiffs`, `captureReviewDiffs`, `rework`'s `preRoundContentHash`, `ship`'s `checkApproved` and `writePRChangeSection`) needs no change: they already just pass whatever `base` string they had, bare or `origin/`-prefixed, straight through to `MeasureDiff`/`DiffFor` — the merge-base resolution is the one thing that now happens in a single place instead of nowhere.
- Extend `translateGitFailure` to recognize `git merge-base`'s own failure phrasing ("Not a valid object name"), distinct from `git diff`'s "ambiguous argument"/"bad revision", so a bad `--base` still surfaces as the same friendly, ref-naming error it always did.
- Add `CommitsBehindBase`, a separate, informational-only signal (`git rev-list --count HEAD..base`) surfaced as a gate `Note` ("branch is N commit(s) behind base") — visible to a human/reviewer, never escalating and never a hard reason, so staleness is named for what it is instead of masquerading as an under-report.

## Rejected

- Fetching/pinning `base` to a fixed SHA at worker-spawn time and diffing against that forever. Rejected: a worker legitimately wants to know it's building on a moving target for staleness purposes (see `CommitsBehindBase`), and a pinned SHA would need its own invalidation story once `base` is deleted or force-pushed; merge-base derives the right comparison point fresh every time with no extra state to keep in sync.
- Suppressing the under-report hard reason whenever `base` has moved, instead of fixing the diff itself. Rejected: that would blind the gate to a real under-report that happens to coincide with `base` moving — merge-base fixes the measurement itself rather than special-casing around a wrong one.
- Folding `CommitsBehindBase` into `protocol.DiffStat`. Rejected: `DiffStat` is also the shape of a worker's own self-report, which has no "commits behind" concept; a separate signal keeps that struct's meaning unchanged for every existing consumer.

## Consequences

- A worker's diff and the gate's measured size are always the merge-base delta — a worker's own change — regardless of how much unrelated work has landed on `base` since it branched.
- The reviewer never sees phantom reverts of merged work; the diff it reads matches exactly what the gate sized.
- Staleness is now visible as its own line, separate from and never confused with a self-report mismatch.
- A genuinely dishonest or buggy self-report still trips the (now-correct) under-report hard reason exactly as before — this changes what counts as evidence, not whether the check exists.
