# Checkpoint/resume contract

argus itself is a thin, restartable supervisor process: the actual work — a
worker agent editing code in a git worktree — runs independently in its own
herdr pane. If the `argus supervise` process dies (killed, crashed, laptop
closed), every worker pane keeps running untouched. What argus needs on
restart is not "replay a log," but "figure out, from durable state, exactly
what already happened, and never repeat a side effect that can't be undone."

This document states that contract explicitly: what's durable, what
"resuming" means in practice, and which actions are guaranteed to run
at most once even across a crash-and-retry.

## The checkpoints

argus records state in a handful of small, typed files, never as an implicit
side effect of scrollback or process memory. All of them are written
atomically — a temp file in the same directory, then an OS rename — so a
reader (including a resumed argus process) only ever sees a complete file,
never a torn write.

| File (relative to worktree, unless noted) | Written by | What it durably answers |
|---|---|---|
| `.claude/argus/status.json` | the worker itself (`argus worker report`) | What phase is this worker in, and what has it self-reported (tests run, diff stat, PR title, ...)? |
| `.claude/argus/verdict.json` | argus's gate/reviewer (`recordApproval`) | Did argus actually approve this change, and against exactly what content (`ContentHash`)? |
| `.claude/argus/lifecycle.json` | `argus ship` / `argus worktree prune` | Has this worktree been shipped, merged, or pruned — and (new) has its Jira post-ship notification already gone out? |
| `<repo-root>/.claude/argus/panes.json` | `argus supervise` at spawn time | Which herdr pane belongs to which worktree, so a worktree can be found and cleaned up even after its own directory is gone. |

`status.json` and `verdict.json` answer different questions on purpose: the
worker's self-report is a hint, git and argus's own re-measurement are the
ground truth the gate actually judges (see `workerState.effective()` in
`internal/supervisor/loop.go`). A resumed argus process trusts its own prior
`verdict.json`, not a worker's claim.

The append-only `~/.argus/runs/*.jsonl` event log (`internal/eventlog`) is
**not** part of this contract. It's a record of what happened, read by
after-the-fact analysis (`argus stats`) — argus never reads it back to decide
what to do next. All resume decisions come from the typed files above.

## At-most-once guarantees

These are the concrete, already-enforced (and tested) guarantees for the
actions that would be genuinely bad to repeat — the ones with an external,
hard-to-undo side effect:

- **Never opens a second PR for the same branch.** `argus ship` calls
  `Forge.FindPR` before `OpenPR`; a retry after a crash between `git push`
  succeeding and `OpenPR` completing reuses the PR it finds instead of
  opening a duplicate (`cmd/ship.go:shipChange`, pinned by
  `TestShipChangeReusesExistingPRInsteadOfDuplicating`).
- **Never re-posts the Jira post-ship comment.** Unlike PR creation, Jira has
  no `FindPR`-equivalent lookup to de-dupe a comment against, so
  `lifecycle.json` tracks a `JiraNotified` flag explicitly: a ship retry that
  finds an already-open PR skips `postShipJira` entirely if a prior run's
  `lifecycle.json` already recorded a successful comment (pinned by
  `TestShipChangeSkipsDuplicateJiraNotificationOnRetry`). Jira's `Transition`
  and `Assign` calls, unlike `Comment`, move to an absolute state and stay
  safe to repeat regardless.
- **Never ships a stale approval onto changed content.** `checkApproved`
  recomputes the worktree's content hash and refuses to ship if it no longer
  matches the hash recorded at approval time — a worker (or a human) touching
  the worktree again after approval can't ride the old verdict.
- **Commit and push are safe no-ops on retry.** `CommitAll` returns
  `ErrNothingToCommit` (treated as success) when a prior run already
  committed everything; `git push` to an already-up-to-date branch is a
  no-op.
- **Merge state only ever advances, and a cached "merged" is trusted
  outright.** `resolveMergeState` treats `lifecycle.json`'s own
  `LifecycleMerged` as final — merges don't unmerge — so a repeated
  `argus worktree prune` never re-asks the forge for a worktree it already
  confirmed merged, and never regresses a worktree's recorded state backward.
- **A (re)dispatched worker never inherits a stale terminal status.**
  `prepareWorktree` calls `InvalidateStatus` to remove any leftover
  `status.json`/`verdict.json` before spawning into a worktree directory that
  may have been reused, and the watch loop independently ignores any status
  file whose `UpdatedAt` predates the dispatch — two independent guards
  against reading a previous, unrelated worker's terminal state as this one's.
- **argus refuses to spawn into a pane with a live agent session.**
  `ensureFreshPane` checks pane occupancy before typing a launch command in,
  so a crash-and-retry (or a reused `--panes` value) can't silently deliver
  that command as a chat message into someone else's running session.

## How to actually resume after argus crashes

Worker panes are independent processes; they don't stop just because
`argus supervise` did. Recovery is about re-attaching argus's *observation*
of them, not re-creating anything.

**Don't** re-run a plain, spawn-mode `argus supervise --tasks ... --branches
...` for a worker that already has a worktree. It calls `git worktree add`
for that branch/path again, which fails outright (the path or branch already
exists) rather than silently resuming or duplicating anything. That failure
is safe — no corruption, no double-spawn — but it is not resume.

**Do** re-attach to the existing worktree(s):

```
argus supervise --attach --worktrees <path>[,<path>...] --base <real-base-branch>
# or, to reattach every pane in a herdr workspace at once:
argus supervise --attach --workspace <id> --base <real-base-branch>
```

`--attach` creates nothing: it reads each worktree's current `status.json`
and `verdict.json`, resumes polling from wherever the worker actually is, and
re-judges from git ground truth exactly like a fresh run would — no worker
progress is lost, and nothing already recorded gets redone. `--base` is
required because an attached worktree wasn't created by this argus process,
so there's nothing to infer the real base branch from.

Once a worker reaches a terminal phase with an approving verdict, `argus
ship` is itself safe to retry to completion (see the guarantees above) — a
killed `ship` invocation is resumed by simply running `argus ship` again with
the same `--worktree`.

## Was there a real gap?

Yes, one: `argus ship`'s Jira post-ship hook (`--jira-issue`) had no
guard against a retry re-posting its "Opened `<PR URL>`" comment, since
Jira gives ship no lookup to check "did I already say this" against (unlike
`FindPR` for the PR itself). That's now closed by `lifecycle.json`'s
`JiraNotified` flag, described above. Everything else in the at-most-once
list above was already correctly guaranteed by existing code; this document
and its accompanying tests are what make that guarantee explicit and checked
rather than merely implied by reading the source.

Reusing plain `supervise` as a resume path was considered and rejected: it
would mean detecting an existing worktree and branching supervise's spawn
logic into a second, parallel "resume" code path with its own edge cases.
`--attach` already does exactly this job, is already tested, and keeps
spawn-mode `supervise` simple (create-only, fail loud on collision) rather
than overloading it with two different meanings.
