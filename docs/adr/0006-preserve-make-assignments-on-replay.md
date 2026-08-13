# 0006. Preserve make VAR=value assignments when replaying a worker's claimed command

Date: 2026-08-13
Status: Accepted

## Context

- The gate re-runs a worker's claimed `make <target>` command verbatim to confirm a claimed pass, via `replayCommands`.
- make treats every bare token after the target name as an additional build target, never as a recipe argument — a stray word turns into "No rule to make target", a fabricated failure unrelated to the worker's actual change.
- `replayCommands` stripped every token after the target to guard against that, but make also accepts `VAR=value` assignments anywhere on the command line, and a worker's claimed command routinely carries one (`make test TEST=Name`, `make adr-find Q=concept`).
- Stripping those assignments changes what the guarded target actually runs: a target whose recipe branches on the variable can pass with the assignment and fail without it, so the strip-all-after-target replay reported a fabricated mismatch against a worker's genuine pass.

## Decision

- Preserve `VAR=value` assignments when replaying a worker's claimed `make` command, while still dropping bare positional words.
- Distinguish the two with `isMakeAssignment`: a token matches only when it starts with a valid make identifier (letters, digits, underscore, not leading with a digit) immediately followed by `=`, the same rule make itself uses to read a token as an assignment rather than a target.
- The target name itself is always replayed bare, unaffected by this change.

## Rejected

- Keep stripping every token after the target. Rejected: mangles any parameterized target into a fabricated failure, escalating a worker's genuine pass as a mismatch.
- Fold Target back in as a positional argument appended to Cmd. Rejected: already documented as wrong in `replayCommands`'s own comment — Target is a descriptive label as often as an argument, and appending it produces neither valid shell nor a valid subcommand.

## Consequences

- A worker's claimed `make target VAR=value` command replays with its assignments intact, so the gate's re-run reflects the same recipe the worker actually exercised.
- A stray bare word after the target is still dropped, so the "No rule to make target" failure mode this replay logic exists to avoid stays closed.
- `isMakeAssignment` becomes the one place this identifier-vs-target distinction is made; any future replay change to the make branch should extend it rather than re-deriving the rule inline.
