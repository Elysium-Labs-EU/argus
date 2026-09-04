---
name: argus
description: "Supervise parallel herdr worker agents through to a forge PR (GitHub, GitLab, Codeberg/Gitea) using the argus binary, which runs the mechanical half of supervision (spawn workers in worktrees, gate diffs against git, require a persisted approving verdict before ship) as plain Go instead of inside the LLM. Use when the user says 'supervise the panes with argus', 'run argus', 'gate these workers', 'ship with argus', or hands you parallel agent tasks and argus is on PATH. Prefer this over hand-running the supervise loop when argus is available — but know which parts below are still manual."
---

# argus — deterministic agent supervisor

Go CLI for multi-pane agent supervision. Discovers/opens herdr panes, gives each
worker its own git worktree, spawns it under Claude Code's `dontAsk`
permission mode with a curated per-phase permission-allow set (default-deny,
not auto-approve), tracks each worker's typed `status.json` (not scrollback).
A gate cross-checks
each worker's self-report against the real `git diff` and auto-approves the safe
majority; the LLM re-enters only for escalations. `ship` refuses to open a PR
without a recorded approving verdict.

**Drive `argus`, don't hand-run the supervise loop.** Hand-run loop = [[supervise-agents]]
skill, only when argus is not on PATH. With argus available, coordination
(discovery, worktrees, spawning, polling) is the binary's job.

**Hard guarantee vs. known gap — do not blur these.** Below, some behavior is
enforced in code; some is a real current gap where argus won't save you and a
manual workaround is required. Both are labeled explicitly. Treating a gap as if
it were already covered is how a stale verdict or unenforced instruction reaches
a PR.

## Quickstart — brand-new repo, first run

Five steps from nothing to a shipped PR; each is expanded later in this doc. Run
them from inside a herdr pane, in the target repo.

```bash
argus doctor --repo .                                                 # prerequisites in one check (see Preflight)
argus config check --repo . --write --entry "Bash(argus supervise *)" # once per clone: allowlist argus needs
argus init                                                            # detect toolchain, write .argus/config.yml
argus supervise --repo . --issues 42 --review --dry-run              # confirm the plan, then drop --dry-run
argus ship --worktree <path> --issue 42 --dry-run                   # then drop --dry-run to open the PR
```

If `argus doctor` fails a hard check (herdr or claude missing), fix that first —
nothing else runs without it. Everything below is the reference for these steps.

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
| Clean up a worktree once its PR merged | `argus worktree prune --branch <name> --dry-run` |
| See phase/owner/lifecycle for every linked worktree, read-only (default: this session's own + unowned worktrees) | `argus fleet --json` |
| Same, but every linked worktree regardless of owner, including idle ones | `argus fleet --json --all --include-idle` |
| See escalation rate / token cost | `argus stats` |
| Check/fix the Bash allowlist argus itself needs | `argus config check --write` |
| Set up a repo's own base branch/allow list/brief note (see `docs/repo-config.md`) | `argus init` |

`fleet` defaults to scoping the view by owner (this invocation's resolved
identity — `--owner` > `$ARGUS_OWNER_ID` > `$HERDR_WORKSPACE_ID` > a generated
id, the same chain `supervise` uses) plus any unowned worktree, and hides
worktrees with no `status.json` yet — a repo that accumulates worktrees
across many sessions is mostly noise otherwise. `--all` restores every linked
worktree regardless of owner; `--include-idle` restores untouched ones.
`--json`'s output is an envelope (`generated_at`/`scope`/`controller_id`/
`count`/`excluded_foreign_count`/`excluded_idle_count`/`worktrees`), so a
filtered-out row is always accounted for, never silently dropped.

If `argus` isn't on PATH, don't silently downgrade — **offer to install it first**:

```bash
curl -sSfL https://raw.githubusercontent.com/Elysium-Labs-EU/argus/main/scripts/install.sh | sh
argus doctor --repo .   # re-check readiness once installed, then proceed
```

Only fall back to [[supervise-agents]] (and say so) if the user declines the install
or it fails. Absence is a setup trigger, not a dead end.

**Non-Go/non-make repos**: `argus init` detects a repo's toolchain
(Taskfile.yml/Makefile/package.json/go.mod) and writes `.argus/config.yml` with a
suggested `base_branch`/`allow`/`brief_note` — see `docs/repo-config.md`. With no
such file, argus assumes nothing about a repo's toolchain.

## What argus guarantees today

Enforced in code, not conventions the worker is merely asked to follow.

- **Diff is measured, not trusted — three unwaivable checks.** The gate always
  computes the real `git diff` and cross-checks it against `status.json`. All
  three land in `Verdict.HardReasons` (`internal/supervisor/review.go`); even a
  `--review` "approve" on one of these is recorded as *not approved*
  (`reviewEscalations` in `internal/supervisor/loop.go` overrides it):
  1. Unmeasurable diff then escalates.
  2. Material under-report (claimed size vs. git-measured size) then escalates.
  3. **Zero measured files changed despite a claimed terminal phase**
     (`awaiting_review`/`done`) then escalates (`internal/supervisor/review.go:129-133`).
     Added after a headless (non-herdr) `supervise` spawn let a fresh worktree
     pick up a *stale, unrelated session's* `status.json` and got auto-approved
     with a fabricated verdict for zero real changes. Catches that symptom only
     — does **not** detect/refuse the headless spawn itself. Always spawn from
     inside a real herdr pane; if you ever must run headless, `git diff` every
     resulting worktree yourself before trusting any report.
- **Plan evidence enforced in two separate places** —
  the global Stop hook can't catch "never wrote a plan" on its own, since it
  only tracks tasks that were actually created:
  - `argus worker report` (`cmd/worker_report.go`, `runWorkerReport`) rejects the
    `planning` to `working` transition outright if the reported `plan` array is
    empty — immediate, at report time.
  - The gate (`internal/supervisor/planevidence.go`, `HasPlanEvidence`, called
    from `gateVerdict`) separately greps the worker's session transcript for a
    real `TodoWrite`-shaped call — only at `awaiting_review`/`done`, during
    `supervise`/`review`, not at report time.
  - A worker can satisfy check 1 with a token plan entry and still get caught by
    check 2 at review time.
- **`ship` refuses without a persisted approving verdict.** Strict — see "Known
  gaps" for exactly which actions do/don't produce a verdict `ship` can see.
- **Cross-session worktree collision guard — advisory lease, not a hard lock.**
  Every worktree `supervise` spawns gets `.claude/argus/owner.json` (`owner_id`,
  `owner_label`, `spawned_at`, `heartbeat_at`, written by `internal/ownership`);
  `supervise` re-stamps `heartbeat_at` every poll tick. `rework`, `rebase`,
  `ship`, `worker answer` each check the caller's resolved identity against the
  lease before touching an existing worktree. Identity resolves: `--owner` flag
  > `$ARGUS_OWNER_ID` > `$HERDR_WORKSPACE_ID` > generated id.
  - Live mismatch then refuses, names the actual owner.
  - Mismatch with `heartbeat_at` stale longer than `--owner-stale-after`
    (default 30m) then logs notice, proceeds.
  - `--force-foreign-owner` then explicit human override for anything else.
  - No lease at all (predates this feature, or never went through `supervise`
    spawn) then treated as unowned, never refused.
  - **Does not reap/clean a stale-lease worktree** — that's `argus worktree
    prune` (section 6). A stale lease only changes whether a mismatched caller
    may proceed.
  - `--owner`/`--force-foreign-owner`: per-invocation only, not in
    `.argus/config.yml`. `--owner-stale-after`: repo-wide, settable via config
    key `owner_stale_after` (docs/repo-config.md); explicit flag wins.
- **A malformed `--gate-verify-command`/bootstrap command fails before any
  worker spawns.** `supervise` shell-parses every configured command upfront
  (`internal/supervisor/preflight.go`); a syntax error is reported immediately
  against every planned worker at once, not just once that worker reaches the
  gate.
- **Workers launch under Claude Code's `dontAsk` permission mode: default-deny,
  not auto-approve-and-chase-a-denylist.** `dontAsk` never prompts a human — it
  resolves every call from the rendered `settings.local.json` alone
  (read-only tools stay auto-allowed by the mode itself), denying and feeding
  the reason back to the worker for anything else, no hang. The resolved
  allow set is a strict layering, floor authoritative:
  ```
  resolved allow(phase) = structural-floor ∪ allow ∪ phases.<phase>.allow ∪ --allow flags − deny-floor
  ```
  - **structural floor** (code, every phase, unremovable): read-only tools,
    read-only git only (`git status`/`git diff`/`git log`), and a worker's own
    `argus worker report`/`answer`/`steer` self-calls. A worker never runs
    `git add`, `git commit`, or `git push` — it edits files, leaves them
    uncommitted, and reports; the gate measures the *uncommitted* working-tree
    diff; `argus ship` is what stages/commits/pushes. No config file at all ⇒
    this floor is all a worker gets — skipping setup makes a worker *more*
    restricted, never less.
  - **`allow` / `phases.<name>.allow`** (config, genuinely editable): the
    materialized toolchain, written by `argus init` from toolchain detection
    and co-built interactively per phase
    (`planning`/`working`/`self_test`/`awaiting_review`/`blocked`); `argus
    init --refresh` re-materializes just this from the current default,
    leaving everything else untouched. `phases.<name>.deny`/`.skip` still work
    as before (unchanged by this — see below).
  - **deny floor** (code, every phase, subtracted last, unremovable): `argus
    ship`/`rework`/`review`/`supervise`, and `git commit`/`git push`. No
    `phases.<any>.allow` entry, materialized command, or `--allow` flag can
    re-grant any of these — an entry as broad as `"Bash(git push*)"` under
    `phases.working.allow` is stripped after the union, in *every* phase, not
    just `planning`. Prevents a worker calling `argus ship --force` on itself
    to bypass the verdict-required gate.
  - Because `settings.local.json` is written once at session launch and can't
    itself vary by phase, the rendered file is the union of every phase's own
    resolved allow — a live `PreToolUse` hook (`argus worker check-tool`,
    wired via `internal/supervisor/agentadapter.go`'s `checkToolHook`) is what
    narrows a call back down to the worker's *current* phase, re-reading
    `.argus/config.yml` fresh from the trusted main checkout on every matching
    Bash call (a worker editing its own worktree's copy has no effect). The
    older dotted deny form (`phase.<name>.deny`) still works as a deprecated
    alias. Full shape: `schemas/config.schema.json` `phases.*` blocks.
  - The effective allow set actually used is recorded on every `spawn` event
    in `~/.argus/runs/*.jsonl` — grep it for a run that spawned with an
    unexpectedly broad grant.
  - Gaps: no cross-repo config tier, no per-phase scripts-on-entry, matching is
    plain string-prefix (not hardened against shell-level evasion), and
    `Task`/`WebFetch` aren't yet pulled outside the convenience floor.

## Known gaps — still manual, no first-class command yet

- **A standalone `argus review` does NOT persist its verdict.** `ship
  --dry-run` can still fail citing an *older* stale verdict right after a fresh
  approve — the tool's own output says the verdict isn't saved and won't be
  seen by ship. Use it only to eyeball a worktree; never as your last step
  before shipping.
- **`argus rework` closes the request-changes loop but is a fresh holistic
  re-review each round, not a checklist against prior findings.** It
  re-dispatches the worktree's own worker in place (same branch, reuses its
  live pane the same way `argus rebase` does) with the last verdict's findings
  as its next brief, waits, re-runs the gate and reviewer, and **persists** the
  resulting verdict so `ship` sees it — no manual `herdr pane run`, no manual
  `supervise --attach --review` follow-up. Loops on further request-changes up
  to `--max-rounds` (default 3), then stops and prints an escalation. Stops
  immediately (no further rounds) if the worker reports `blocked` or the
  reviewer returns `needs-human`. Does **not** verify a specific prior finding
  was fixed — spot-check that exact location yourself once `rework` reports
  approved. `argus rebase` is not a substitute — it's scoped specifically to
  sibling-PR-merge-conflict handoff, not review feedback (`rework` is the
  general-rework analog, sharing its live-pane-reuse dispatch logic).
- **Pre-spawn failures leave zero trace in argus's own logs.** If `argus
  supervise` errors before any worker spawns (e.g. "error creating worktree
  ... already exists"), nothing is written to `~/.argus/runs/*.jsonl` — that
  log only ever contains events for runs where a worker actually started.
  **After any `supervise` call that errors before spawn, note the retry
  yourself immediately** — argus will not remind you, no log recovers it.
- **Any self-hosted forge needs `--forge` set to `gitlab` or `gitea`.**
  Auto-detection only knows `github.com`/`gitlab.com`/`codeberg.org` by exact
  host — a self-hosted GitLab or Gitea/Forgejo is as likely to be named
  `git.company.com` as anything mentioning "gitlab". Any other host makes
  `ship`, `supervise` (`--issues` fetch), and `worktree prune` refuse with a
  clear error instead of guessing — including under `--dry-run` (validates the
  forge shape with no token). Pass `--forge` on any of the three, or set this
  repo's `.argus/config.yml` `forge` key once (docs/repo-config.md); explicit
  flag wins over config.
- **A self-hosted forge also has no built-in status-page entry for a
  host-shaped request/push failure.** `internal/svcstatus`'s map only covers
  `github.com`/`gitlab.com`/`codeberg.org`. `ship`'s `--status-page-url` flag
  (or `.argus/config.yml` `status_page` key, same flag-wins precedence)
  points it at the right page; without one, the error just omits the hint.

## Preflight

One command runs every prerequisite check — herdr + claude on PATH (hard, exits
non-zero), forge token + Bash allowlist + repo `.argus/config.yml` (soft, warns):

```bash
argus doctor --repo .
```

Each failed line carries the exact fix. The individual checks, if you need to reason
about one in isolation:

- `herdr` on PATH — argus talks to it over its CLI. You're usually already inside herdr.
- `claude` on PATH — needed for `argus review` and `supervise --review`.
- **A forge token is needed for more than `ship`** — `supervise --issues`/
  `--jira-issues` also fetch from the forge/Jira API to build briefs, erroring
  clearly ("no API token for `<host>` (needed to fetch issues)") if none found.
  argus detects the forge from the git remote, resolves a token: env var first
  (`GITHUB_TOKEN`/`GH_TOKEN` for github.com, `GITLAB_TOKEN` for gitlab.com,
  `CODEBERG_TOKEN` for codeberg.org, `<HOST>_TOKEN`/`FORGE_TOKEN` otherwise),
  falling back to `gh`/`glab`/`git credential fill`. `--jira-issues`/
  `--jira-issue` additionally need `JIRA_BASE_URL`/`JIRA_EMAIL`/`JIRA_API_TOKEN`.

**Three config files, one map.** argus reads/writes three files with overlapping
names but no overlap in purpose. Don't confuse them — `config check` and `config
set` touch two *different* files, and neither is the `.argus/config.yml` that
`init` wrote:

| File | Written by | Format | Holds | Scope |
|---|---|---|---|---|
| `.argus/config.yml` | `argus init` | YAML | Per-repo defaults: base branch, toolchain, gate/ship keys (docs/repo-config.md) | Per repo, committed |
| `.claude/settings.json` | `argus config check --write` | JSON | Bash allow/deny entries argus needs | Per clone, untracked |
| `~/.argus/config.toml` | `argus config set` | TOML | Credential name to env-var overrides only | Per user |

**Bash-permission allowlist and herdr-pane denylist — do this once per clone**,
or every `argus` call prompts for manual approval and raw herdr pane mutation
stays uncoordinated with argus's own dispatch:

```bash
argus config check --repo . --write --entry "Bash(argus supervise *)"   # scoped: leaves ship gated
argus config check           # read-only: reports what's missing
```

- `check --write` also adds `permissions.deny` for `herdr pane
  send-text`/`send-keys`/`run`: those return as soon as herdr accepts the
  text, with no confirmation a live agent turn ever read it — use `argus
  worker steer`/`answer` instead (real receipt). Read-only `pane
  list`/`read`/`get` are left alone.
- `check` only reads/writes `permissions.allow`/`permissions.deny` — every
  other key (hooks, model, unrelated permissions) round-trips untouched.
- **`.claude/settings.json` is untracked, per-clone** — can't propagate via
  git. Every operator running argus from their own checkout needs to run
  `config check --write` once. Workers never call herdr directly so they don't
  need the deny entries; a supervising session does.
- **This is the orchestrator-Bash boundary, and it needs no `ARGUS_WORKER`
  guard.** `config check` reads/writes only the orchestrating session's own
  `.claude/settings.json`; a worker's Bash permissions come from its own
  per-worktree rendered `settings.local.json` (see "Workers launch under
  Claude Code's `dontAsk` permission mode" above) and never read
  `.claude/settings.json` at all. That's unlike a Claude Code hook, which
  Claude Code merges from the user-global config into every session,
  workers included — the reason the adopting repo's own `CLAUDE.md` documents
  an `ARGUS_WORKER=1` guard idiom for orchestrator-global hooks. No analogous
  leak exists here, so no analogous guard is needed for this file.

**Scope the entry away from `ship`, not just to a subcommand.** Bash
allow-glob matches only a command *prefix* — there's no syntax to permit
`argus ship` with safe flags while excluding `--force` specifically. A blanket
wildcard and a "tighter" `"Bash(argus ship *)"` entry both equally authorize
`argus ship --force` with **no separate approval prompt** — a prompt
injection that gets that string typed skips argus's own gate outright. `argus
config check` warns whenever the entry it's checking/writing covers this
case — don't ignore that warning. To require a human's explicit say-so for
`--force`: allowlist only the non-`ship` subcommand you call most (usually
`supervise`), leave `ship` prompting every time regardless of flags. `--write`
adds one entry per run and treats any existing argus entry as sufficient (no
stacking) — to cover more than one non-`ship` subcommand, list them yourself:

```json
{
  "permissions": {
    "allow": ["Bash(argus supervise *)", "Bash(argus review *)"]
  }
}
```

Blanket wildcard, for repos that accept the risk:

```json
{
  "permissions": {
    "allow": ["Bash(argus *)"]
  }
}
```

This is *not* a blanket bypass of judgment calls — `ship --force` and anything
else this skill says to hold for the user still needs their explicit say-so;
scoping away from `ship` is what backs that with a real prompt.

## 1. Supervise

Prefer `--tasks`/`--branches` (creates a worktree per worker). Use `--panes`
only to reuse panes that already exist. Always `--dry-run` first.

```bash
argus supervise --repo <path> \
  --tasks "add retry to sink,fix log rotation" \
  --branches feat-retry,fix-rotation \
  --dry-run

# then drop --dry-run to run for real
```

**Track spawned workers as Claude Code tasks, not just in your head.** On every
real (non-`--dry-run`) `supervise` call, `TaskCreate` one task per
worker/issue (description = checkable acceptance criterion, e.g. "`argus
ship` succeeds with an approved verdict and opens a PR closing #142"), then
`TaskUpdate` it `in_progress` — the session's Stop hook blocks ending the turn
on a task left `pending`. Mark `completed` only once `ship` actually opens
that worker's PR; escalation, `blocked`, or still-running all stay
`in_progress`. Ending the turn to wait on workers is a legitimate pause — say
so, don't go quiet.

`--tasks` is CSV-parsed: a bare unmatched `"` fails outright, but an unquoted
comma silently splits one brief into two (argus warns on leading/trailing
whitespace in a split item, the tell-tale sign, but doesn't block). Wrap a
comma-bearing brief in CSV quotes (`--tasks '"brief, with a comma"'`), or for
heavier punctuation put one brief per line in a file and pass `--tasks-file`
(appended after any `--tasks`):

```bash
argus supervise --repo <path> --tasks-file briefs.txt --branches feat-a,feat-b
```

LLM review path for escalations, `--review` (headless `claude -p`):

```bash
argus supervise --repo <path> --tasks "risky change" --branches feat-x --review
```

Briefs straight from forge issues or Jira:

```bash
argus supervise --repo <path> --issues 141,142,143 --review
argus supervise --repo <path> --jira-issues PROJ-123,PROJ-124 --review
```

Claim tickets on Jira's board the moment their workers spawn:

```bash
argus supervise --repo <path> --jira-issues PROJ-123,PROJ-124 --review \
  --jira-assign-on-spawn --jira-transition-on-spawn "In Progress"
```

**"error creating worktree ... already exists"** — a leftover worktree
dir/branch entry from a prior manual worktree or an uncleaned previous run. If
its PR already merged: `argus worktree prune --branch <name>` (section 6).
Otherwise clean up manually:

```bash
trash <path>            # or your repo's guarded delete flow
git worktree prune
```

Also check for a herdr pane still rooted in the removed path — `trash`/`rm`
never touches the pane's shell, so `agent_status` stays `idle`. Recreating a
worktree at that path and re-running supervise will then refuse to spawn
("already has a live agent session"). Find and close it first:

```bash
herdr pane list   # look for a pane whose cwd is under the path you just removed
herdr pane close <pane-id>
```

This failure happens *before* any worker spawns, so it will not appear in
`~/.argus/runs/*.jsonl` — note the retry yourself.

Useful flags (see `argus supervise --help` for all):

- `--base origin/main` — base ref new worktrees branch from.
- `--interval 15s` — status poll cadence.
- `--timeout 0` — per-worker deadline; `0` waits indefinitely.
- `--review-model <id>` — model for `--review`.
- `--review-concurrency <n>` — max concurrent `--review` calls on multi-worker escalation (default 4).
- `--worker-placement <workspace|tab>` (default `workspace`) — `tab` nests each worker's pane as a tab in the current herdr workspace; needs `HERDR_WORKSPACE_ID` set (running from inside a herdr pane).
- `--forge <gitlab|gitea>` — self-hosted API shape for the `--issues` forge fetch; `.argus/config.yml` `forge` key sets a default.
- `.argus/config.yml` `workspace_label_template` — overrides the herdr-visible workspace/tab label a `--issues`/`--jira-issues` spawned worker gets (default: bare `#<n>`/ticket key). Supports `{issue}`/`{project}`/`{summary}` placeholders; see `docs/repo-config.md`.
- `--allow <pattern,...>` — extra Claude Code permission patterns appended to every worker's resolved allow set (every phase), on top of `.argus/config.yml` `allow`/`phases.<name>.allow`, for a one-off run. See "Workers launch under Claude Code's `dontAsk` permission mode" above.
- Gate tuning:
  - `--max-diff-lines` (default 400, `0` disables) — insertions+deletions from the *measured* git diff; over the limit escalates regardless of test results. Pure size proxy, independent of the other checks. (Real diffs of 1178/1527/461 lines have all correctly escalated past 400.)
  - `--proof-required-path` — change needs real-world proof.
  - `--always-review-path` — behavior-critical, always escalates.
  - Both match a whole path segment/word, or a path substring if the value contains `/` — not shell wildcards (`*`/`?` have no special meaning).
  - All three settable once via `.argus/config.yml` `phases.awaiting_review.{max_diff_lines,proof_required_paths,always_review_paths}` (the old flat top-level names still work as deprecated aliases); explicit flag wins.
  - --shared-glob is gone, not renamed — folded into `--always-review-path` (identical behavior); an old invocation now fails with an unknown-flag error.
- `--gate-verify-command <shell command>` (renamed from `--verify-cmd`, old flag still accepted as deprecated alias; default: none) — closes the gap where the gate's checks pass but the repo's own pre-commit hooks (lint, build, fieldalignment, ...) fail at `ship`'s `git commit`. Runs once a worker reaches a terminal phase, in its worktree, one retry on failure; non-zero exit is an unwaivable escalation. Settable via `.argus/config.yml` `phases.awaiting_review.gate_verify_command` (the old flat top-level `gate_verify_command`/`verify_command` still work as deprecated aliases); explicit flag wins.

`--gate-verify-command`/`phases.awaiting_review.gate_verify_command` is the closest thing to a
custom-rule plugin point: `ReviewPolicy`'s built-in checks (`--max-diff-lines`,
`--proof-required-path`, `--always-review-path`) are a fixed set — no
code-free way to add a new one. Any other mechanical rule (custom lint,
forbidden-import check, required-file check) can be a script that exits
non-zero on violation, set here — its failure becomes the same unwaivable
hard reason. Limitation: runs once, at the gate, after a worker claims done —
can't catch a live violation during planning/working.

## 2. React to escalations

Gate auto-approves only when: worker is `awaiting_review`, every reported test
passed, diff is within `--max-diff-lines`, no always-review path touched, any
proof-required-path change carries real-world proof, and the
worker's transcript shows genuine plan evidence.

Diff is always measured from git, never trusted from `status.json`: an
unmeasurable diff, a material under-report, or zero files changed despite a
claimed terminal phase always escalates — `--review` cannot approve past any
of the three (an "approve" verdict on a hard reason is still recorded
not-approved).

**Verify once — read only the diffs argus surfaces for a human.** The
supervise report labels every worker with the source that cleared it:

- `gate-auto-approved` — deterministic gate cleared it on plain facts. Zero LLM cost. **Do not re-read.**
- `reviewer-approved` — gate escalated, `--review` approved. Already verified twice. **Do not re-read.**
- `surfaced-awaiting-human` — no approving verdict (gate escalated with no reviewer, reviewer returned request-changes, an unwaivable hard reason fired, or the worker is `blocked`). **This is the only kind you hand-read.**

```bash
git -C <worktree> diff origin/main
```

Approve read-only/build/test/own-worktree changes. Hold and ask the user for
anything touching shared or production state, force-pushes to shared
branches, or deletes outside a tempdir. For OS-integration changes (systemd,
launchd, install scripts), demand real-world proof, not mocked unit tests
plus a dry-run.

Exception: after `argus rework` reports approved, that pass is fresh and
holistic — it does not confirm a *specific* prior finding was fixed. Spot-check
that exact location, not the whole diff.

One-shot review of any worktree — read-only eyeballing, not a shippable
verdict (does not persist):

```bash
argus review --worktree <path> --base origin/main --task "issue 142" --reasons "touches sink dispatch"
```

Request-changes verdict then get a fresh, persisted verdict with `argus rework`
(section 4), not manual pane messaging.

## 3. Ship

`ship` refuses without an approving verdict from a prior gate or review. Opens
the PR via the detected forge's API (GitHub/GitLab/Codeberg/Gitea; self-hosted
needs `--forge gitlab`/`--forge gitea`) and unstages argus's own
control-plane files (`.claude/argus`, scoped permission files) so they never
reach the PR.

```bash
argus ship --worktree <path> --issue 42 --dry-run   # confirm first
argus ship --worktree <path> --issue 42
```

`--dry-run` is also the fastest verdict-presence check: the same approval
check runs before the dry-run branch, so a clean dry-run print is itself proof
a verdict exists.

Optional post-ship Jira hook (transitions/assigns/comments the linked issue
once the PR is open):

```bash
argus ship --worktree <path> --issue 42 \
  --jira-issue PROJ-123 --jira-transition "In Review" --jira-assignee <accountId>
```

Only use `--force` (skips the gate) when the user explicitly authorizes it.

Whether a PR merges automatically depends on the repo's own forge settings,
not on argus:

```bash
gh pr view <N> --json state,mergedAt
```

## 4. Getting a verdict `ship` will see, after rework

```bash
argus rework --worktree <path> --base origin/main
```

1. Reads the worktree's last recorded verdict (`.claude/argus/verdict.json`)
   for its findings. If findings only exist from a standalone `argus review`
   call (never persisted), pass explicitly: `--findings "finding one"
   --findings "finding two"` (repeat per finding, verbatim), or
   `--findings-file path` (one per line, appended after any `--findings`) for
   longer briefs.
2. Re-dispatches the worktree's own worker in place (reuses its live herdr
   pane, same as `argus rebase`) with those findings as its next brief.
3. Blocks and polls for the next report.
4. Re-runs the gate and, on escalation, the reviewer — then **persists** the
   resulting verdict, same as `supervise --attach --review`, so `ship` sees it.
5. Loops on further request-changes up to `--max-rounds` (default 3). Stops
   immediately — no more rounds — if the worker reports `blocked` or the
   reviewer returns `needs-human`. Stops and prints an escalation once the
   round cap is exhausted.

`--max-rounds` bounds one `rework` invocation's own loop. `--max-rework-budget`
(default `supervisor.DefaultMaxReworkBudget`) is a persisted, cross-invocation
restart budget for the worktree itself — total rework rounds it may ever be
dispatched for, across every separate `rework` call. `0` disables it. Without
the flag: `.argus/config.yml` `rework_budget` key, then the built-in default.
Exhausted budget then `rework` refuses regardless of `--max-rounds`.

Sanity-check with `argus ship --worktree <path> --issue <N> --dry-run` before
shipping for real. An approve doesn't mean every prior finding was fixed —
each round is a fresh holistic pass, not a checklist. Spot-check any
specifically-named prior defect yourself.

`supervise --attach --review` (the previous workaround) still works — useful
for a fresh persisted verdict *without* re-dispatching the worker (e.g. it
already pushed a fix on its own):

```bash
argus supervise --repo <path> --attach --worktrees <path> --base origin/main --review
```

## 5. Post-merge conflict handoff

```bash
argus rebase --worktree <path> --base main
```

Dispatches the worktree's own worker (full context) to rebase. Scoped
specifically to sibling-PR-merge-conflict handoff, not general rework (use
section 4 for that).

## 6. Post-ship cleanup

```bash
argus worktree prune --branch <name> --dry-run   # confirm first
argus worktree prune --branch <name>

argus worktree prune --merged                    # sweep every worktree under the repo
```

Prune checks deterministically (no LLM) whether each worktree's PR has merged
and whether it's otherwise safe to remove (no uncommitted changes, no
unpushed commits, no stash). Safe worktrees are cleaned automatically (a
recoverable relocation, never a raw `rm`); anything else is reported with the
reason and left alone. Also closes the herdr pane it spawned for that
worktree — and the workspace too, if that pane was the only one left in it.
Best-effort: a herdr-side failure is printed as a warning, not a reason to
leave the worktree uncleaned.

## 7. Post-ship CI polling

```bash
argus tend --worktree <path> --dry-run   # confirm the resolved PR and poll plan first
argus tend --worktree <path>
```

Polls the PR's head commit checks to a terminal state, re-stamping the
worktree's ownership lease heartbeat every tick (same as `supervise`'s watch
loop). Reports merge-ready, failed (naming the first failing check), or error
on `--timeout`/interrupt. **GitHub only** — GitLab and Gitea/Forgejo refuse
with a clear error, not a silent no-op. Does no dispatch of any kind — fixing
a failing check is on you.

Reads GitHub's Checks API (check-runs) only — a PR whose only CI posts
through the legacy Commit Status API never shows up here (looks falsely
idle). Confirm the repo's CI reports via check-runs before trusting a result.

## Inspect / update the binary

```bash
argus stats                  # escalation rate, review parse-fail rate, tokens per task
argus supervise ... --debug  # tee the typed event log to stderr; always persisted to ~/.argus/runs
argus system version         # confirm the installed version
argus system update [--pre]  # pull the latest (or latest pre-release) and self-replace
```

`~/.argus/runs` only records events for runs where a worker actually spawned
— a `supervise` call that errors out beforehand (e.g.
worktree-already-exists) leaves nothing here; track those failures yourself.

`system update` verifies both the release's `sha256sums.txt` and a detached
ECDSA signature over it before replacing the running binary, refusing
outright on a mismatch. A release with no `sha256sums.txt.sig` (only possible
on `--pre`) is a warning, not a refusal — checksum verification still runs.

## What NOT to do

- Don't hand-run the herdr pane loop when argus is on PATH — reintroduces the token cost argus exists to remove. [[supervise-agents]] is the no-argus fallback only.
- Don't run `supervise`'s spawn mode outside a real herdr pane — a headless spawn has been observed to leak a stale, unrelated session's state into a fresh worktree and get auto-approved. The zero-files-changed gate check catches this symptom, not the spawn-side root cause — verify with `git diff` yourself if you must run headless.
- Don't approve a ship off a worker's summary — argus gates on the measured diff; read it yourself too. A hard reason can no longer be waived by `--review`, but that doesn't make the reviewer's judgment infallible, and a rework re-review still doesn't re-check prior findings specifically.
- Don't treat a standalone `argus review` verdict as something `ship` will see — it isn't persisted. Use `supervise --attach --review`, then confirm with `ship --dry-run`.
- Don't reach for `--force` on `ship` unless the user explicitly authorized it.
- Don't `--dry-run`-skip on a first real run against an unfamiliar repo.
- Don't assume a supervise error before spawn is recorded anywhere — note the retry yourself immediately.
- Don't assume any self-hosted forge (GitLab, Gitea, or Forgejo) works without `--forge` — `ship`, `supervise`, and `worktree prune` all refuse any host outside github.com/gitlab.com/codeberg.org without it (or `.argus/config.yml` `forge` key).
