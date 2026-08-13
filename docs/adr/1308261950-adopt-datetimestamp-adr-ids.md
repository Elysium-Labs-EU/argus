# Adopt datetimestamp ADR ids

Date: 2026-08-13
Status: Accepted

## Context

- `0001` numbers ADRs sequentially (`NNNN-short-title.md`); `0004` dropped the
  shared index but kept sequential numbering, noting the remaining pain was
  "only" a filename collision that git reports plainly and a rename fixes.
- In practice that collision recurs constantly: argus runs many workers in
  parallel, and any two of them picking the same next number both land on
  `NNNN-*.md` and conflict the moment either branch is reviewed against the
  other's already-merged file, forcing a rename after the fact.
- A worker can self-allocate a unique id at ADR-creation time without
  coordinating with any other worker, if the id is derived from something
  already unique per authoring moment: the creation timestamp.

## Decision

- New ADR ids are minute-precision datetimestamps in the form
  `DDMMYYHHMM` (day, month, two-digit year, hour, minute, 24h), e.g.
  `docs/adr/1308261950-slug.md`.
- This supersedes the sequential-numbering clause of `0001`
  ("numbered sequentially: `NNNN-short-title.md`"); `0001`'s other decisions —
  one file per decision, `adr-find` for lookup, immutability, no issue
  numbers — stand. `0004` already dropped the shared index, so there is no
  index to update for this change either.

## Rejected

- Keep sequential numbers. Rejected: collides under parallel authorship,
  which is the normal mode for this repo, not an edge case.
- Build an allocator command or lockfile to hand out numbers safely.
  Rejected: more machinery than the problem needs — a timestamp is already a
  self-allocating unique id, no coordination service required.
- Second-precision ids. Rejected: unnecessary — a minute-level collision
  between two ADRs authored by different workers is vanishingly rare, and
  even then it is still just a loud filename collision fixed by a rename,
  same as today.

## Consequences

- Existing ADRs `0001`-`0009` keep their sequential ids unchanged and are
  never renamed; an accepted ADR is immutable per `0001`.
- ADR ids no longer sort as a simple counter, but a datetimestamp id still
  sorts lexically in chronological order, and every existing sequential id
  sorts before any new datetimestamp id.
- Two workers authoring ADRs in the same minute is the only remaining
  collision window, down from the same probability across the entire
  lifetime of the sequential counter.
