# 0004. Drop the shared ADR index; derive status via adr-find

Date: 2026-08-13
Status: Accepted

## Context

- `docs/adr/index.md` was a hand-maintained table every new ADR had to append a row to.
- Two branches adding an ADR concurrently each append after the same anchor line; git cannot order two insertions at the same point, so every concurrent pair of ADRs conflicts in the index regardless of numbering. argus routinely runs several workers in parallel, so this is the normal mode, not an edge case.
- The two failure modes are not equally bad. A filename collision (two branches both writing `0004-*.md`) is a loud add/add conflict git reports plainly, fixed by a rename. The index-row conflict is the expensive one: it fires on every concurrent pair, in a file whose content is pure derivation from the directory listing, and resolving it by hand invites silently dropping the other side's row because both sides look like a one-line addition.
- Every field in the index is already derivable: the number is the filename prefix, the title is the filename slug in prose, the status is the `Status` line inside each file.
- Unlike some sibling repos, an ADR here is referenced by number in code (a comment in `internal/supervisor/loop_test.go` cites ADR 0002); this does not depend on the index and is unaffected by dropping it.

## Decision

- Delete `docs/adr/index.md`.
- Extend `make adr-find` to print each matching file's status next to its path, so the one column with real structure survives as a query instead of a maintained file.
- This supersedes the index-discoverability clause of `0001` ("List every ADR in `docs/adr/index.md` by number, title, and status"); `0001`'s other decisions — one file per decision, sequential numbering, `adr-find` for lookup, immutability, no issue numbers — stand.

## Rejected

- Generating `index.md` from the directory listing plus a CI staleness gate. Rejected: permanent machinery to maintain a table nothing reads, and more code than the file it replaces.
- Date-prefixed filenames instead of sequential numbers. Rejected: removes only the cheap filename collision, which git already reports plainly and a rename fixes, and leaves the expensive index-row conflict untouched.

## Consequences

- Two branches adding ADRs concurrently no longer conflict on a shared file; each PR only touches the new file(s) it adds.
- Status is queried with `make adr-find Q="concept"` instead of read off a maintained table.
- A gap in the numbering sequence from an abandoned branch is normal and not a defect to repair.
- The one real code reference to an ADR by number still resolves to a stable, predictable filename; sequential numbering was never in question.
