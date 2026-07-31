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
| Address a request-changes verdict and get a fresh, persisted one `ship` will see | `argus rework --worktree <path> --base origin/main` |
| Get a verdict that `ship` will actually see, without addressing feedback first | `argus supervise --repo <path> --attach --worktrees <path> --base origin/main --review` |
| Check whether `ship` will succeed right now | `argus ship --worktree <path> --issue <N> --dry-run` |
| Ship for real | `argus ship --worktree <path> --issue <N>` |
| Hand off a worktree after a sibling PR merged first | `argus rebase --worktree <path> --base main` |
| Poll a shipped PR's CI checks to a terminal state (GitHub only) | `argus tend --worktree <path> --dry-run` |
| See escalation rate / token cost | `argus stats` |
| Check/fix the Bash allowlist argus itself needs | `argus config check --write` |
| Set up a repo's own base branch/allow list/brief note (see `docs/repo-config.md`) | `argus init` |

If `argus` isn't on PATH, fall back to [[supervise-agents]] and say so.

**Non-Go/non-make repos**: `supervise`'s generated worker permission allowlist and
default base branch used to hardcode Go/make assumptions. `argus init` detects a
repo's toolchain (Taskfile.yml/Makefile/package.json/go.mod) and writes
`.argus/config.yml` with a suggested `base_branch`/`allow`/`brief_note` — see
`docs/repo-config.md`. With no such file, argus assumes nothing about any
repo's toolchain.

## What argus guarantees today (rc.20)

These are enforced in code, not conventions the worker is merely asked to follow:

- **Diff is measured, not trusted — three checks a worker cannot talk past, and
  none of the three is waivable by `--review`.** The gate always computes the
  real `git diff` itself and cross-checks it against `status.json`, regardless
  of what the worker claims. All three land in `Verdict.HardReasons`
  (`internal/supervisor/review.go`): even a `--review` verdict of "approve" on
  one of these is recorded as *not approved* (`reviewEscalations` in
  `internal/supervisor/loop.go` overrides it) — fixing a case where a
  real under-report ("claimed 215 lines, git measured 461") once got waved
  through anyway because the escalation was only a factor in the reviewer's
  holistic judgment, not a hard stop.
  1. An unmeasurable diff escalates.
  2. A material under-report (worker's claimed size vs. git's measured size)
     escalates.
  3. **Zero measured files changed despite a claimed terminal phase
     (`awaiting_review`/`done`) escalates** — `internal/supervisor/review.go:129-133`,
     added after a headless (non-herdr) `supervise` spawn let
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
- **Cross-session worktree collision has a guard — an advisory ownership lease,
  not a hard lock.** Every worktree `supervise` spawns gets a
  `.claude/argus/owner.json` lease (`owner_id`, `owner_label`, `spawned_at`,
  `heartbeat_at`, written by `internal/ownership`) at spawn time;
  `supervise`'s own poll loop re-stamps `heartbeat_at` every tick for as long
  as it keeps tracking that worktree. `rework`, `rebase`, `ship`, and
  `worker answer` each check the caller's resolved identity against the
  recorded lease before touching an existing worktree — identity resolves
  `--owner` flag > `$ARGUS_OWNER_ID` > `$HERDR_WORKSPACE_ID` > a generated id,
  the same chain `supervise` itself resolves once per run. A live mismatch
  refuses outright, naming the actual owner; a mismatch whose `heartbeat_at`
  has gone quiet longer than `--owner-stale-after` (default 30m) logs a
  notice and proceeds instead of blocking forever on a session that crashed;
  `--force-foreign-owner` is the explicit human override for anything else. A
  worktree with no lease at all (predates this feature, or never went through
  `supervise`'s own spawn path) is treated as unowned and never refused.
  **What this does not do**: reap or clean up a worktree with a stale lease —
  that's `argus worktree prune`, still not shipped (see below). A stale lease
  only ever changes whether a *mismatched* caller may proceed; it is not
  itself a cleanup mechanism. `--owner`/`--force-foreign-owner` are
  necessarily per-invocation flags (an override is a human decision, not
  something to default silently for a whole repo) and are not settable via
  `.argus/config.yml`. `--owner-stale-after` is different — a repo-wide
  policy knob, the same shape as `max_diff_lines` — so it *can* be set once
  as this repo's `.argus/config.yml` `owner_stale_after` key instead of
  repeated on every invocation (see `docs/repo-config.md`); an explicit
  `--owner-stale-after` flag still wins over the config key.

## Known gaps — still manual, no first-class command yet

- **A standalone `argus review` does NOT persist its verdict.** Run it once, get a
  fresh approve, and `argus ship ... --dry-run` can still fail citing an *older*
  stale request-changes verdict — confirmed directly. The tool tells you this
  itself: its own output says the verdict isn't saved and that ship won't see it.
  Use it only to eyeball a worktree; never as your last step before shipping.
- **`argus rework` closes the request-changes loop, but it is still a fresh
  holistic re-review each round, not a checklist against prior findings.** It
  re-dispatches the worktree's own worker (in place, same branch — reusing its
  live pane the same way `argus rebase` does) with the last verdict's findings
  as its next brief, waits for it, then re-runs the gate and reviewer and
  *persists* the resulting verdict so `ship` sees it — no manual
  `herdr pane run`, no manual `supervise --attach --review` follow-up. It loops on a
  further request-changes up to `--max-rounds` (default 3), then stops and
  prints an escalation instead of retrying forever; it also stops immediately
  (no further rounds) if the worker reports `blocked` or the reviewer comes
  back `needs-human`. What it does **not** do: verify that a specific prior
  finding was actually fixed — each round's review is a fresh pass over the
  whole diff, the same "holistic, not a checklist" caveat as any other review.
  If a prior verdict named a precise defect, spot-check that exact location
  yourself once `rework` reports approved, same as you would after a manual
  re-review.

  `argus rebase` is not a substitute here — it is scoped specifically to
  sibling-PR-merge-conflict handoff, not review feedback (`argus rework` is
  the general-rework analog, and shares its live-pane-reuse dispatch logic).
- **`argus worktree prune` does not exist yet.** A cleanup command for detecting
  merged PRs and safely removing stale worktrees is in progress but not shipped —
  it has already been through one request-changes round (dead lifecycle-wiring
  code, missing `--credential-env` parity) and a fresh review just caught a real
  bug (`--dry-run` silently mutating `lifecycle.json` despite claiming to be
  preview-only) that is still being fixed. Until it lands, treat worktree cleanup
  as fully manual (see below) — do not invoke a prune subcommand that doesn't
  exist. The owner-lease `heartbeat_at`/`Stale` check above is what a future
  `prune` would read to decide a worktree is abandoned — the lease already
  exists, only the reaping command itself does not yet.
- **Pre-spawn failures leave zero trace anywhere in argus's own logs.** If
  `argus supervise` errors before any worker spawns (e.g. "error creating worktree
  ... already exists"), nothing is written to `~/.argus/runs/*.jsonl` — that log
  only ever contains events for runs where a worker actually started. This is not
  hypothetical: a supervise call that failed on a worktree conflict was cleaned up
  and then genuinely forgotten for a long stretch of a real session because
  nothing external recorded it was still outstanding. **After any `supervise` call
  that errors before spawn, note the retry yourself immediately** — argus will not
  remind you, and there is no log to recover it from later.
- **Any self-hosted forge needs `--forge` set to `gitlab` or `gitea`.**
  Auto-detection only knows the three hosted forges by their exact host —
  `github.com`, `gitlab.com`, `codeberg.org` — because a host name is not a
  reliable signal for which REST shape a self-hosted instance actually speaks
  (a self-hosted GitLab and a self-hosted Gitea/Forgejo are exactly as likely
  to be named `git.company.com` as anything mentioning "gitlab"). Any other
  host now makes `ship`, `supervise` (its `--issues` forge fetch), and
  `worktree prune` refuse with a clear error instead of silently guessing —
  including under `--dry-run`, which validates the forge shape with no token
  so a clean dry-run actually proves the real ship's forge call will hit the
  right API. Pass `--forge` on any of the three to say which the host is, or
  set this repo's `.argus/config.yml` `forge` key once instead of repeating
  the flag on every invocation (see `docs/repo-config.md`) — an explicit
  `--forge` flag still wins over the config key.

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

**Bash-permission allowlist and herdr-pane denylist (do this once per clone, or
every `argus` call prompts for manual approval and raw herdr pane mutation stays
uncoordinated with argus's own dispatch):**

```bash
argus config check --repo . --write --entry "Bash(argus supervise *)"   # scoped: leaves ship gated
argus config check           # read-only: reports what's missing without touching the file
```

`check --write` also adds a `permissions.deny` entry for `herdr pane
send-text`/`send-keys`/`run`: those calls return as soon as herdr accepts the
text, whether or not a live agent turn ever reads it, so a supervising
session calling them directly gets no delivery confirmation at all — `argus
worker steer`/`answer` already cover every legitimate need to drive a live
pane, with a real receipt. Read-only `pane list`/`read`/`get` are left alone.
`check` only ever reads/writes `permissions.allow`/`permissions.deny` — every
other key in the file (hooks, model, unrelated permissions) is round-tripped
untouched.

**`.claude/settings.json` is untracked and per-clone, not per-repo** — it can't
propagate to a worker's worktree or a teammate's clone via git. Every operator
running argus from their own checkout needs to run `config check --write`
themselves once; workers never call herdr directly so they don't need the
deny entries, but a supervising session does.

**Scope the entry away from `ship`, not just to a subcommand.** Bash allow-glob
only matches a command *prefix* — there is no syntax to permit `argus ship`
with safe flags while excluding `--force` specifically, since the flag is just
more text after the same prefix. That means both the blanket wildcard *and* a
"tighter" `"Bash(argus ship *)"` entry equally authorize `argus ship --force`
with **no separate approval prompt** — a context that can get `argus ship
--force` typed (a prompt injection into an agent that already holds either
entry, say) skips argus's own gate outright. `argus config check` warns
whenever the entry it's about to check or write covers this case; don't ignore
that warning. If you want `--force` to always need a human's explicit say-so,
allowlist only the non-`ship` subcommand you call most (usually `supervise`,
since a spawn loop is where most of the per-call prompting happens) and leave
`ship` — forced or not — prompting every time. `--write` only ever adds one
entry per run and treats any existing argus entry (any scope) as already
sufficient, so it won't stack a second scoped entry on top of the first — to
cover more than one non-`ship` subcommand, list them yourself in
`.claude/settings.json`:

```json
{
  "permissions": {
    "allow": ["Bash(argus supervise *)", "Bash(argus review *)"]
  }
}
```

The blanket wildcard remains available for repos that accept the risk:

```json
{
  "permissions": {
    "allow": ["Bash(argus *)"]
  }
}
```

This is *not* a blanket bypass of judgment calls — `ship --force` and anything this
skill tells you to hold for the user still needs their explicit say-so, and scoping
your allow entry away from `ship` is what actually backs that with a real prompt
instead of just a documented intention.

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

**Track spawned workers as Claude Code tasks, not just in your head.** On every real
(non-`--dry-run`) `supervise` call, `TaskCreate` one task per worker/issue spawned —
description a checkable acceptance criterion, e.g. "`argus ship` succeeds with an
approved verdict and opens a PR closing #142" — then immediately `TaskUpdate` it to
`in_progress`: the session's own Stop hook blocks ending the turn on a task left
`pending`. Mark `completed` only once `ship` actually opens that worker's PR; an
escalation, a `blocked` phase, or a worker still running all stay `in_progress`.
Ending the turn to wait on workers rather than finishing them now is a legitimate
pause — say so, don't go quiet.

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

Claim tickets on Jira's board the moment their workers spawn, instead of only updating Jira after `ship` opens the PR:

```bash
argus supervise --repo <path> --jira-issues PROJ-123,PROJ-124 --review \
  --jira-assign-on-spawn --jira-transition-on-spawn "In Progress"
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

Also check for a herdr pane left rooted in the path you just removed — `trash`/`rm`
moves the directory but never touches the pane's shell, so its `agent_status` stays
`idle` instead of going away. Recreating a worktree at that same path and re-running
supervise will then refuse to spawn there ("already has a live agent session"),
because argus can't tell that pane apart from one genuinely mid-task. Find and close
it before retrying:

```bash
herdr pane list   # look for a pane whose cwd is under the path you just removed
herdr pane close <pane-id>
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
- `--worker-placement <workspace|tab>` (default `workspace`) — `tab` nests each worker's pane as a tab in your current herdr workspace instead of a new top-level one; needs `HERDR_WORKSPACE_ID` set (i.e. running `argus supervise` from inside a herdr pane).
- `--forge <gitlab|gitea>` — say which API shape a self-hosted host speaks for the `--issues` forge fetch (see the known-gaps note above); this repo's `.argus/config.yml` `forge` key sets a default instead of repeating the flag every run.
- Gate tuning — `--max-diff-lines` (default 400, `0` disables): counts
  insertions+deletions together from the *measured* git diff; over the limit
  escalates regardless of whether every test passed. It's a pure size-based risk
  proxy, independent of the always-review-path/proof-required-path/under-report
  checks — real diffs of 1178, 1527, and 461 lines have all correctly escalated
  past the 400 default. `--proof-required-path` (change needs real-world proof)
  and `--always-review-path` (behavior-critical, always escalates) are the
  content-based escalation triggers alongside it. Each matches a whole path
  segment/word, or — if the value contains `/` — a path substring; these are
  not shell wildcards, `*` and `?` have no special meaning. All three can also
  be set once in this repo's `.argus/config.yml` (`max_diff_lines`,
  `proof_required_paths`, `always_review_paths`) instead of repeating the flag
  every invocation — an explicitly passed flag still wins. There used to be a
  separate --shared-glob flag for shared/prod paths; it behaved identically
  to `--always-review-path` (unconditional escalation, differing only in the
  reported reason) and was deliberately folded into it rather than kept as a
  duplicate mechanism. That old flag is gone, not renamed — an invocation
  still passing it now fails with an unknown-flag error; use
  `--always-review-path` instead.
- `--gate-verify-command <shell command>` (renamed from `--verify-cmd`, old
  flag still accepted as a deprecated alias; default: none — runs nothing,
  matching argus's prior behavior) — closes the gap where the gate's
  tests/diff/path checks all pass but the target repo's own pre-commit hooks
  (lint, build, fieldalignment, ...) fail at `ship`'s `git commit`. Once a
  worker reaches a terminal phase, the gate re-runs this command in its
  worktree (one retry on failure, to absorb shared-machine flakiness); a
  non-zero exit is an unwaivable escalation, the same treatment a
  reproduced test-claim mismatch gets. Can also be set once via this repo's
  `.argus/config.yml` `gate_verify_command` key instead of repeating the
  flag — an explicitly passed flag still wins.

`--gate-verify-command`/`gate_verify_command` (both renamed — from
`--verify-cmd`/`verify_command` respectively, old names still accepted) is
also the closest thing argus has to a custom-rule plugin
point: ReviewPolicy's own checks (`--max-diff-lines`, `--proof-required-path`,
`--always-review-path`) are a fixed, closed set argus itself knows how to run
— there's no way to add a new one without a code change. Any other mechanical
rule a repo wants enforced (a custom lint, a forbidden-import check, a
required-file check) can be expressed as a script that exits non-zero on
violation and set here instead; its failure becomes the same unwaivable hard
reason a reproduced test-claim mismatch gets. The one limitation: it only
runs once, at the gate, after a worker claims to be done — it can't catch a
violation live during planning/working, only at review time.

## 2. React to escalations

The gate is the cheap path: it auto-approves only when the worker is `awaiting_review`,
every reported test passed, the diff is within `--max-diff-lines`, no always-review
path was touched, any proof-required-path change carries real-world proof, and (as of v0.1.0-rc.20)
the worker's transcript shows genuine plan evidence — see "What argus guarantees
today" above for exactly which of these are hard, unfakeable checks vs. softer
content-based triggers.

Argus always measures the diff from git rather than trusting `status.json` — an
unmeasurable diff, a material under-report, or zero files changed despite a claimed
terminal phase always escalates, and `--review` cannot approve past any of the
three: a "approve" verdict on a hard reason is still recorded as
not-approved.

**Verify once — read only the diffs argus surfaces for a human.** The supervise
report labels every worker with the source that cleared it, on an `approval:` line:

- `gate-auto-approved` — the deterministic gate cleared it on plain facts (right
  phase, tests passed, diff within the ceiling, no always-review/proof-required
  path, plan evidence present). Zero LLM cost, already verified. **Do not re-read it.**
- `reviewer-approved` — the gate escalated and the `--review` pass approved it.
  Already verified twice. **Do not re-read it.**
- `surfaced-awaiting-human` — no approving verdict: the gate escalated with no
  reviewer, the reviewer returned request-changes, an unwaivable hard reason fired,
  or the worker is `blocked`. **This is the only kind you hand-read.**

Re-reading an already-approved diff is the largest avoidable cost in a supervise
run — one full extra diff read per issue, scaling with issue count — and it
re-verifies exactly what the gate (and `--review`) already cleared. So hand-read
only `surfaced-awaiting-human` and `blocked` workers:

```bash
git -C <worktree> diff origin/main
```

For those, approve read-only/build/test/own-worktree changes. Hold and ask the user
for anything touching shared or production state, force-pushes to shared branches, or
deletes outside a tempdir. For OS-integration changes (systemd, launchd, install
scripts), demand real-world proof, not mocked unit tests plus a dry-run.

The one exception to "don't re-read an approved diff": after `argus rework` reports
approved, a re-review is a fresh holistic pass and does **not** confirm a *specific*
prior finding was fixed (see the gap above). If a prior verdict named a precise
defect, spot-check that exact location — not the whole diff — once `rework` clears.

You can run a one-shot review of any worktree on demand — but treat it as read-only
eyeballing, not a shippable verdict (see the gap above: it does not persist):

```bash
argus review --worktree <path> --base origin/main --task "issue 142" --reasons "touches sink dispatch"
```

If the verdict was request-changes, address it and get a fresh, persisted verdict with
`argus rework` (see section 4) instead of messaging the worker's pane by hand.

## 3. Ship

`ship` refuses without an approving verdict from a prior gate or review — that is the
point, so a request-changes actually blocks the PR. It opens the PR via the detected
forge's API (GitHub/GitLab/Codeberg/Gitea — any self-hosted instance needs `--forge
gitlab`/`--forge gitea`, see the known-gaps note above) and unstages argus's own
control-plane files (`.claude/argus`, scoped permission files) so they never reach
the PR.

```bash
argus ship --worktree <path> --issue 42 --dry-run   # confirm first
argus ship --worktree <path> --issue 42
```

`--dry-run` is also the fastest verdict-presence diagnostic you have: the same
approval check runs before the dry-run branch, so an unapproved worktree fails
identically to a real ship — a clean `--dry-run` print is itself proof a verdict
exists, faster than inspecting `status.json` or run logs by hand.

Optional post-ship Jira hook (transitions/assigns/comments the linked issue once the
PR is open) — the other end of this lifecycle from `supervise`'s pre-spawn
`--jira-assign-on-spawn`/`--jira-transition-on-spawn` above:

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

## 4. Getting a verdict `ship` will actually see, after rework

After a request-changes verdict, `argus rework` is the first-class continuation:

```bash
argus rework --worktree <path> --base origin/main
```

This one command does everything the old manual loop required by hand:

1. Reads the worktree's last recorded verdict (`.claude/argus/verdict.json`) for its
   findings — no need to re-paste them. If you only have findings from a standalone
   `argus review` call (which never persists — see the gap above), pass them
   explicitly instead: `--findings "finding one" --findings "finding two"` (repeat the
   flag per finding — each value is verbatim, so commas and quotes inside a finding are
   safe). For a longer, multi-sentence brief, put one finding per line in a file and pass
   `--findings-file path` instead (appended after any `--findings`).
2. Re-dispatches the worktree's own worker in place (reusing its live herdr pane the
   same way `argus rebase` does — no `herdr pane run` by hand) with those findings as
   its next brief.
3. Blocks and polls for the worker's next report itself — no manual wait/reminder.
4. Re-runs the gate and, on escalation, the reviewer — then **persists** the resulting
   verdict, exactly the same as `supervise --attach --review` does, so `ship` sees it.
5. On a further request-changes, loops back into another round automatically, up to
   `--max-rounds` (default 3). It stops immediately — no more rounds — if the worker
   reports `blocked`, or the reviewer comes back `needs-human`; either means a human
   decision is needed, not another automatic retry. After the round cap is exhausted
   it also stops and prints an escalation rather than looping forever.

Sanity-check the result with `argus ship --worktree <path> --issue <N> --dry-run`
before shipping for real. And don't assume an approve means every prior finding was
fixed — each round's review is a fresh holistic pass, not a checklist against what was
previously flagged (see the gap above). If a prior verdict named a specific defect,
spot-check that exact location in the diff yourself before shipping.

`supervise --attach --review` (the previous workaround) still works and is useful when you want
a fresh persisted verdict *without* first re-dispatching the worker — e.g. the worker
already pushed a fix on its own and you just need argus to record a verdict for it:

```bash
argus supervise --repo <path> --attach --worktrees <path> --base origin/main --review
```

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

Prune also closes the herdr pane it spawned for that worktree — and the workspace
too, if that pane was the only one left in it — so a cleaned worktree doesn't leave
an orphaned empty pane/workspace behind. Best-effort: a herdr-side failure here is
printed as a warning, not a reason to leave the worktree itself uncleaned.

## 7. Post-ship CI polling

Once `ship` opens a PR, `tend` polls its head commit's checks to a terminal state
instead of you watching the forge UI — re-stamping the worktree's ownership lease
heartbeat on every tick the same way `supervise`'s own watch loop does:

```bash
argus tend --worktree <path> --dry-run   # confirm the resolved PR and poll plan first
argus tend --worktree <path>
```

It reports merge-ready (every check passed), failed (naming the first check that
didn't), or an error if `--timeout` elapses or it's interrupted first. **GitHub
only for now** — GitLab and Gitea/Forgejo refuse with a clear error rather than a
silent no-op. It does no dispatch of any kind: fixing a failing check is still on you.

It reads GitHub's Checks API (check-runs) only — a PR whose only CI posts through
the legacy Commit Status API instead never shows up here, so it can look
falsely idle. Confirm the repo's CI actually reports via check-runs (GitHub
Actions and most modern integrations do) before trusting a `tend` result.

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
  get auto-approved. The zero-files-changed gate check catches this
  symptom, but the spawn-side root cause isn't fixed — verify with `git diff`
  yourself if you ever must run headless.
- Don't approve a ship off a worker's summary — argus gates on the measured diff; you
  read it too. A hard reason (unmeasurable diff, material under-report, zero files
  changed) can no longer be waived by `--review`, but that doesn't
  make the reviewer's judgment on the code itself infallible, and a rework
  re-review still doesn't re-check prior findings specifically.
- Don't treat a standalone `argus review` verdict as something `ship` will see — it
  isn't persisted. Use `supervise --attach --review` instead, then confirm with
  `ship --dry-run`.
- Don't invoke a worktree-prune/cleanup subcommand — it doesn't exist yet; clean up
  stale worktrees with `trash <path>` + `git worktree prune` instead.
- Don't reach for `--force` on `ship` unless the user explicitly authorized it.
- Don't `--dry-run`-skip on a first real run against an unfamiliar repo.
- Don't assume a supervise error before spawn is recorded anywhere — if it errors
  before a worker starts, note the retry yourself immediately.
- Don't assume any self-hosted forge (GitLab, Gitea, or Forgejo) works without
  `--forge` — `ship`, `supervise`, and `worktree prune` all refuse any host
  outside github.com/gitlab.com/codeberg.org without it (or this repo's
  `.argus/config.yml` `forge` key).
