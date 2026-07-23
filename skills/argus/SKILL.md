---
name: argus
description: "Supervise parallel herdr worker agents through to a forge PR (GitHub, GitLab, Codeberg/Gitea) using the argus binary, which runs the mechanical half of supervision (spawn workers in worktrees, gate diffs against git, require a persisted approving verdict before ship) as plain Go instead of inside the LLM. Use when the user says 'supervise the panes with argus', 'run argus', 'gate these workers', 'ship with argus', or hands you parallel agent tasks and argus is on PATH. Prefer this over hand-running the supervise loop when argus is available — but know which parts below are still manual."
---

# argus — deterministic agent supervisor

argus runs multi-pane agent supervision as a Go CLI. It discovers or opens herdr
panes, gives each worker its own git worktree, spawns it in auto mode with a scoped
permission file, and tracks each worker's typed `status.json` instead of scraping
scrollback. A gate cross-checks each worker's self-report against the real `git diff`
and auto-approves only the safe majority; the LLM re-enters only for the escalated
minority. `ship` refuses to open a PR without a recorded approving verdict.

**Your job with this skill is to drive `argus`, not to hand-run the supervise loop.**
The hand-run loop lives in the [[supervise-agents]] skill — reach for that only when
argus is not on PATH. When argus is available, coordination (discovery, worktrees,
spawning, polling) is the binary's job; you spend tokens only on the judgment calls
argus escalates to you.

**Read this before trusting argus with anything consequential.** Some of what follows
is a hard guarantee enforced by the binary. Some of it is a real, current gap where
argus will not save you and a manual workaround is required. They are labeled
explicitly below — do not blur the two. Treating a known gap as if argus already
covers it is how a stale verdict or an unenforced instruction reaches a PR.

## Quick reference

| Situation | Command |
|---|---|
| Spawn workers on new tasks | `argus supervise --repo <path> --tasks "..." --branches a,b --dry-run` |
| Spawn from forge issues | `argus supervise --repo <path> --issues 141,142 --review` |
| Spawn from Jira issues | `argus supervise --repo <path> --jira-issues PROJ-123,PROJ-124 --review` |
| One-off look at a worktree (does **not** feed `ship`) | `argus review --worktree <path> --base origin/main --task "..." --reasons "..."` |
| Get a verdict that `ship` will actually see, after rework | `argus supervise --repo <path> --attach --worktrees <path> --base origin/main --review` |
| Check whether `ship` will succeed right now | `argus ship --worktree <path> --issue <N> --dry-run` |
| Ship for real | `argus ship --worktree <path> --issue <N>` |
| Hand off a worktree after a sibling PR merged first | `argus rebase --worktree <path> --base main` |
| See escalation rate / token cost | `argus stats` |

If `argus` isn't on PATH, fall back to [[supervise-agents]] and say so.

## What argus guarantees today (rc.20)

These are enforced in code, not conventions the worker is merely asked to follow:

- **Diff is measured, not trusted — three checks a worker cannot talk past.**
  The gate always computes the real `git diff` itself and cross-checks it against
  `status.json`, regardless of what the worker claims:
  1. An unmeasurable diff escalates.
  2. A material under-report (worker's claimed size vs. git's measured size)
     escalates. *(Caveat below on where this still leaks once `--review` is in
     the loop.)*
  3. **Zero measured files changed despite a claimed terminal phase
     (`awaiting_review`/`done`) escalates** — `internal/supervisor/review.go:129-133`,
     added by PR #17 closing issue #15: a headless (non-herdr) `supervise` spawn let
     a fresh worktree pick up a *stale, unrelated Claude session's* `status.json`,
     and the gate auto-approved it as "6/6 tests passed" with a real, freshly-written
     `verdict.json` — for zero actual code changes. This check catches that exact
     symptom. **It does not fix the root cause**: nothing detects or refuses a
     non-herdr spawn itself, and no open issue currently tracks that root cause as
     distinct from this symptom guard. Always run `supervise`'s spawn mode from
     inside a real herdr pane; if you ever must run it headless, independently
     `git diff` every resulting worktree before trusting any report.
- **Plan evidence is now enforced — but in two separate places, not one (shipped in
  v0.1.0-rc.20, not a proposal).** Before rc.20, every worker brief said "write a
  todo list before anything else" as prose, and nothing enforced it — two full real
  worker sessions were observed making zero `TodoWrite` calls despite the
  instruction, and the global Stop hook that blocks incomplete task lists can't
  catch "never started one" because it only tracks tasks that were actually
  created. As of rc.20:
  - `argus worker report` itself (`cmd/worker_report.go`, `runWorkerReport`) rejects
    the `planning → working` transition outright if the worker's reported `plan`
    array is empty — immediate, at report time.
  - Separately, the gate (`internal/supervisor/planevidence.go`, `HasPlanEvidence`,
    invoked from `gateVerdict`) independently greps the worker's own session
    transcript for a real `TodoWrite`-shaped tool call — but this only runs later,
    when the worker reaches `awaiting_review`/`done` during `supervise`/`review`,
    not at `worker report` time. A worker could satisfy the first check with a
    token plan entry and still get caught by the second at review time.
  Treat both as real, current, shipped behavior — not a future plan.
- **`ship` refuses without a persisted approving verdict.** This is real and it is
  strict — see the gaps immediately below on exactly which actions do and do not
  produce a verdict `ship` can see.

## Known gaps — still manual, no first-class command yet

- **A standalone `argus review` does NOT persist its verdict.** Run it once, get a
  fresh approve, and `argus ship ... --dry-run` can still fail citing an *older*
  stale request-changes verdict — confirmed directly. The tool tells you this
  itself: its own output says the verdict isn't saved and that ship won't see it.
  Use it only to eyeball a worktree; never as your last step before shipping.
- **There is no first-class "address review feedback and re-verdict" command
  (tracked as argus issue #104).** The rework loop today is entirely manual:
  1. Message the worker's existing herdr pane directly with the review's specific
     findings as a new instruction (`herdr pane run <pane-id> '<findings as fix
     instructions>'`).
  2. Wait — there is no argus-native block-wait for this; observed real turnaround
     has ranged from a couple of minutes to the better part of an hour, depending
     on how much the worker has to redo.
  3. Get a fresh, *persisted* verdict via `--attach` (see section 4) — never via a
     standalone `argus review`.
  4. Even after that, the *review* re-check itself doesn't automatically re-verify
     that a specific prior finding was actually fixed — see the under-report
     caveat below for the same "review is a fresh holistic pass, not a checklist"
     pattern. Re-read the diff yourself at the specific location a prior verdict
     flagged, don't assume a new approve means that exact finding is resolved.

  `argus rebase` is not a substitute here — it is scoped specifically to
  sibling-PR-merge-conflict handoff, not general rework.
- **Diff under-report can still slip through once `--review` is in the loop
  (tracked as argus issue #105, not yet fixed).** Observed directly: the gate
  escalated for two reasons at once ("diff 461 lines exceeds max 400" *and*
  "worker under-reported diff: claimed 215 lines, git measured 461"), and
  `--review` still came back with an approve. The escalation itself is reliable;
  the LLM reviewer's judgment on an escalated under-report is not. Read the diff
  yourself on any under-report escalation — don't let `--review`'s verdict be the
  last word.
- **`argus worktree prune` does not exist yet.** A cleanup command for detecting
  merged PRs and safely removing stale worktrees is in progress but not shipped —
  it has already been through one request-changes round (dead lifecycle-wiring
  code, missing `--credential-env` parity) and a fresh review just caught a real
  bug (`--dry-run` silently mutating `lifecycle.json` despite claiming to be
  preview-only) that is still being fixed. Until it lands, treat worktree cleanup
  as fully manual (see below) — do not invoke a prune subcommand that doesn't
  exist.
- **Pre-spawn failures leave zero trace anywhere in argus's own logs.** If
  `argus supervise` errors before any worker spawns (e.g. "error creating worktree
  ... already exists"), nothing is written to `~/.argus/runs/*.jsonl` — that log
  only ever contains events for runs where a worker actually started. This is not
  hypothetical: a supervise call that failed on a worktree conflict was cleaned up
  and then genuinely forgotten for a long stretch of a real session because
  nothing external recorded it was still outstanding. **After any `supervise` call
  that errors before spawn, note the retry yourself immediately** — argus will not
  remind you, and there is no log to recover it from later.
- **GitLab support is gitlab.com only.** Self-hosted GitLab is silently treated as
  Gitea/Forgejo (`internal/forge/forge.go:67-71`: "self-hosted GitLab needs an
  explicit future flag, since hostname alone can't disambiguate a self-hosted
  GitLab from Gitea/Forgejo"). A self-hosted GitLab remote will get the wrong API
  calls with no error.

## Preflight

```bash
command -v argus herdr claude    # all three must resolve
gh auth status                   # or: [ -n "$GITHUB_TOKEN" ] (don't echo the token itself)
```

- `herdr` on PATH — argus talks to it over its CLI. You are usually already inside herdr.
- `claude` on PATH — needed for `argus review` and `supervise --review`.
- **A forge token is needed for more than just `ship`** — `supervise --issues`/
  `--jira-issues` also fetches from the forge/Jira API to build briefs, and errors
  clearly ("no API token for <host> (needed to fetch issues)") if none is found.
  argus detects the forge (GitHub, GitLab, Codeberg/Gitea) from the repo's git
  remote and resolves a token itself: env var first (`GITHUB_TOKEN`/`GH_TOKEN` for
  github.com, `GITLAB_TOKEN` for gitlab.com, `CODEBERG_TOKEN` for codeberg.org,
  `<HOST>_TOKEN`/`FORGE_TOKEN` otherwise), falling back to `gh`/`glab`/`git
  credential fill`. Nothing to export by hand if `gh auth login` (or the
  equivalent) is already done. `--jira-issues`/`--jira-issue` additionally need
  `JIRA_BASE_URL`/`JIRA_EMAIL`/`JIRA_API_TOKEN`.

**Bash-permission allowlist (do this once per repo, or every `argus` call prompts for
manual approval and defeats the point of using it):** add to `.claude/settings.json`:

```json
{
  "permissions": {
    "allow": ["Bash(argus *)"]
  }
}
```

This is *not* a blanket bypass of judgment calls — `ship --force` and anything this
skill tells you to hold for the user still needs their explicit say-so regardless of
what's allowlisted. There is no `argus config`/setup-check subcommand that does this
for you yet — it's a one-time manual edit per adopting repo.

If `argus` is missing, fall back to the [[supervise-agents]] skill and say so.

## 1. Supervise

Prefer `--tasks`/`--branches` (argus creates a worktree per worker). Use `--panes` only
to reuse panes that already exist. Always `--dry-run` first to confirm the plan.

```bash
argus supervise --repo <path> \
  --tasks "add retry to sink,fix log rotation" \
  --branches feat-retry,fix-rotation \
  --dry-run

# then drop --dry-run to run for real
```

`--tasks` is CSV-parsed, so a free-text brief containing commas or quotes will fail
to parse. For multi-sentence briefs with punctuation, put one brief per line in a
file and pass `--tasks-file` instead (appended after any `--tasks`):

```bash
argus supervise --repo <path> --tasks-file briefs.txt --branches feat-a,feat-b
```

Turn on the LLM review path for escalations with `--review` (headless `claude -p`):

```bash
argus supervise --repo <path> --tasks "risky change" --branches feat-x --review
```

Fetch briefs straight from forge issues (or Jira) instead of writing them by hand:

```bash
argus supervise --repo <path> --issues 141,142,143 --review
argus supervise --repo <path> --jira-issues PROJ-123,PROJ-124 --review
```

**If this errors with "error creating worktree ... already exists"** — a worktree
directory or git-registered worktree entry for that branch name is already there (a
leftover from a prior manual worktree, or a previous run's worktree/branch that was
never cleaned up). There is no argus command to clear this yet; clean it up manually
before retrying:

```bash
trash <path>            # or your repo's guarded delete flow, if it enforces one
git worktree prune
```

And remember: this failure mode happens *before* any worker spawns, so it will not
appear in `~/.argus/runs/*.jsonl` — note the retry yourself, don't rely on argus's own
logs to remind you it's outstanding.

Useful flags (see `argus supervise --help` for all):

- `--base origin/main` — base ref new worktrees branch from.
- `--interval 15s` — status poll cadence.
- `--timeout 0` — per-worker deadline; `0` waits indefinitely.
- `--review-model <id>` — model for `--review`.
- `--review-concurrency <n>` — max concurrent `--review` calls when the gate escalates several workers at once (default 4).
- Gate tuning — `--max-diff-lines` (default 400, `0` disables): counts
  insertions+deletions together from the *measured* git diff; over the limit
  escalates regardless of whether every test passed. It's a pure size-based risk
  proxy, independent of the shared-path/OS-integration/under-report checks — real
  diffs of 1178, 1527, and 461 lines have all correctly escalated past the 400
  default. `--shared-glob`, `--os-glob`, `--always-review-glob` are the
  content-based escalation triggers alongside it.

## 2. React to escalations

The gate is the cheap path: it auto-approves only when the worker is `awaiting_review`,
every reported test passed, the diff is within `--max-diff-lines`, no shared path was
touched, any OS-integration change carries real-world proof, and (as of v0.1.0-rc.20)
the worker's transcript shows genuine plan evidence — see "What argus guarantees
today" above for exactly which of these are hard, unfakeable checks vs. softer
content-based triggers.

Argus always measures the diff from git rather than trusting `status.json` — an
unmeasurable diff, a material under-report, or zero files changed despite a claimed
terminal phase always escalates. **What is not guaranteed:** once `--review` picks up
that escalation, its verdict can still be an approve even on a confirmed under-report
(argus issue #105, open), and a re-review after rework doesn't automatically re-check
whether a *specific* prior finding was actually addressed (it's a fresh holistic pass
each time). Escalation is reliable; the automated judgment on it is not always the
last word — read the diff yourself in either case:

```bash
git -C <worktree> diff origin/main
```

Approve read-only/build/test/own-worktree changes. Hold and ask the user for anything
touching shared or production state, force-pushes to shared branches, or deletes
outside a tempdir. For OS-integration changes (systemd, launchd, install scripts),
demand real-world proof, not mocked unit tests plus a dry-run.

You can run a one-shot review of any worktree on demand — but treat it as read-only
eyeballing, not a shippable verdict (see the gap above: it does not persist):

```bash
argus review --worktree <path> --base origin/main --task "issue 142" --reasons "touches sink dispatch"
```

If you need a verdict that `ship` will actually recognize after messaging a worker to
fix review feedback, use `--attach` instead (see section 4).

## 3. Ship

`ship` refuses without an approving verdict from a prior gate or review — that is the
point, so a request-changes actually blocks the PR. It opens the PR via the detected
forge's API (GitHub/GitLab/Codeberg/Gitea — GitLab only for the exact host
`gitlab.com`, see the known-gaps note above) and unstages argus's own control-plane
files (`.claude/argus`, scoped permission files) so they never reach the PR.

```bash
argus ship --worktree <path> --issue 42 --dry-run   # confirm first
argus ship --worktree <path> --issue 42
```

`--dry-run` is also the fastest verdict-presence diagnostic you have: the same
approval check runs before the dry-run branch, so an unapproved worktree fails
identically to a real ship — a clean `--dry-run` print is itself proof a verdict
exists, faster than inspecting `status.json` or run logs by hand.

Optional post-ship Jira hook (transitions/assigns/comments the linked issue once the
PR is open):

```bash
argus ship --worktree <path> --issue 42 \
  --jira-issue PROJ-123 --jira-transition "In Review" --jira-assignee <accountId>
```

Only use `--force` (skip the gate) when the user explicitly authorizes shipping an
unverified change.

Once a PR is open, whether it merges automatically depends on the repo's own forge
settings, not on argus — check before assuming either way:

```bash
gh pr view <N> --json state,mergedAt
```

## 4. Getting a verdict `ship` will actually see, after manual rework

After a request-changes verdict, there is no first-class rework command yet (tracked
as argus issue #104). The manual loop:

1. Message the worker's existing herdr pane with the review's specific findings as a
   new instruction:

   ```bash
   herdr pane run <pane-id> '<findings as fix instructions>'
   ```

2. Wait — no argus-native block-wait exists for this; real turnaround has ranged
   from a couple of minutes to the better part of an hour.
3. Get a fresh, **persisted** verdict by attaching argus to the worktree the worker is
   already running in (no spawn), with `--base` given explicitly since argus can't
   infer what an already-existing worktree branched from:

   ```bash
   argus supervise --repo <path> --attach --worktrees <path> --base origin/main --review
   ```

   This is the one that `ship` will actually recognize — a standalone `argus review`
   will not (see section 2). argus itself warns that `--attach` does not manage
   isolation: an attached worker keeps whatever credential proxy and runtime adapter
   (if any) it was already started with.
4. Sanity-check the new verdict actually persisted with
   `argus ship --worktree <path> --issue <N> --dry-run` before shipping for real — if
   it still cites the old request-changes reason, the re-review didn't take.
5. Don't assume an approve at this point means every prior finding was fixed — the
   review re-checks holistically, not against a checklist of what was previously
   flagged. If a prior verdict named a specific defect, spot-check that exact
   location in the diff yourself before shipping.

## 5. Post-merge conflict handoff

When a sibling PR merges first and leaves another worktree conflicting, dispatch that
worktree's own worker to rebase — it already has full context. This is scoped
specifically to this merge-conflict case, not general rework (use section 4 for that):

```bash
argus rebase --worktree <path> --base main
```

## 6. Post-ship cleanup

Once a PR merges, the worktree that produced it is dead weight. Prune checks
deterministically (no LLM) whether each worktree's PR has merged and whether it's
otherwise safe to remove (no uncommitted changes, no unpushed commits, no stash) —
safe worktrees are cleaned automatically (a recoverable relocation, never a raw rm),
anything else is reported with the reason and left alone:

```bash
argus worktree prune --branch <name> --dry-run   # confirm first
argus worktree prune --branch <name>

argus worktree prune --merged                    # sweep every worktree under the repo
```

## Inspect / update the binary

```bash
argus stats                  # escalation rate, review parse-fail rate, tokens per task
argus supervise ... --debug  # tee the typed event log to stderr; always persisted to ~/.argus/runs
argus system version         # confirm the installed version
argus system update [--pre]  # pull the latest (or latest pre-release) and self-replace
```

Remember: `~/.argus/runs` only records events for runs where a worker actually
spawned. A `supervise` call that errors out beforehand (e.g. worktree-already-exists)
leaves nothing here — track those failures yourself.

`system update` verifies the release's `sha256sums.txt` before replacing the running
binary (checksum-only, no signature verification yet).

## What NOT to do

- Don't hand-run the herdr pane loop when argus is on PATH — that reintroduces the
  token cost argus exists to remove. [[supervise-agents]] is the no-argus fallback only.
- Don't run `supervise`'s spawn mode outside a real herdr pane — a headless spawn has
  been observed to leak a stale, unrelated session's state into a fresh worktree and
  get auto-approved (issue #15). The zero-files-changed gate check catches this
  symptom, but the spawn-side root cause isn't fixed — verify with `git diff`
  yourself if you ever must run headless.
- Don't approve a ship off a worker's summary — argus gates on the measured diff; you
  read it too. This still applies even when `--review` approved an under-report case
  (issue #105), and even after a rework re-review (it doesn't re-check prior findings
  specifically).
- Don't treat a standalone `argus review` verdict as something `ship` will see — it
  isn't persisted. Use `supervise --attach --review` instead, then confirm with
  `ship --dry-run`.
- Don't invoke a worktree-prune/cleanup subcommand — it doesn't exist yet; clean up
  stale worktrees with `trash <path>` + `git worktree prune` instead.
- Don't reach for `--force` on `ship` unless the user explicitly authorized it.
- Don't `--dry-run`-skip on a first real run against an unfamiliar repo.
- Don't assume a supervise error before spawn is recorded anywhere — if it errors
  before a worker starts, note the retry yourself immediately.
- Don't assume self-hosted GitLab works — only `gitlab.com` gets the GitLab client;
  anything else is silently treated as Gitea/Forgejo.
