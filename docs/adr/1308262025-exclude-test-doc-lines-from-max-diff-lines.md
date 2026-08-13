# Exclude test/doc lines from max_diff_lines

Date: 2026-08-13
Status: Accepted

## Context

- The review gate's `max_diff_lines` ceiling (`ReviewPolicy.MaxDiffLines`,
  default 400) escalates a worker's change to review when
  `Insertions + Deletions` exceeds it, counting every changed line
  regardless of what kind of file it touched.
- A repo that requires tests and an ADR for every change routinely produces
  a 300-line code change plus its tests plus a 30-line ADR — comfortably
  over 400 lines even though the code portion alone was well within bounds.
  The ceiling then fires on essentially every compliant change, forcing a
  needless review of nothing but mandated bulk.
- The ceiling exists to bound reviewable code size, not to penalize a
  worker for doing what the repo's own policy already requires of it.

## Decision

- `protocol.DiffStat` gains `CodeInsertions`/`CodeDeletions`, the subset of
  `Insertions`/`Deletions` that excludes files classified as TEST or DOCS.
  `MeasureDiff` (`internal/supervisor/measure.go`) populates them alongside
  the existing totals, classifying each changed file (tracked and
  untracked) with a new `isTestOrDocPath` helper:
  - TEST: a path matching `*_test.go`, or under a `testdata/` directory.
  - DOCS: a path ending `.md` or `.txt`, or under a `docs/` directory.
- `Assess`'s `MaxDiffLines` check (`internal/supervisor/review.go`) now
  compares against `CodeInsertions + CodeDeletions` instead of the full
  total. The full total is unchanged and still reported everywhere else
  (worker report summaries, the under-report honesty check, etc.) — only
  the ceiling comparison changes.

## Rejected

- Count everything (status quo). Rejected: fires on every compliant change
  in a repo that mandates tests/docs, which is exactly the bug this ADR
  fixes.
- Raise the flat ceiling to absorb typical test/doc bulk. Rejected: a
  higher flat number still doesn't distinguish a 1000-line code change from
  a 300-line code change with 700 lines of mandated test/doc bulk — it just
  moves the false-negative/false-positive tradeoff around instead of fixing
  the underlying signal.

## Consequences

- A large test/docs-only diff no longer trips the ceiling; an equivalently
  large code diff still does.
- `DiffStat`'s two new fields are populated only by `MeasureDiff` (i.e. the
  git-measured ground truth `gateVerdict` uses via `workerState.effective`).
  A hand-constructed `protocol.Status` that never went through `MeasureDiff`
  — as in-repo unit tests do — leaves them zero, which reads as "no code
  lines" for the ceiling check; test authors must set them explicitly when
  exercising this check directly.
