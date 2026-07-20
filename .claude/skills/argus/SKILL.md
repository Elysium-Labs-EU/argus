---
name: argus
description: "Supervise parallel herdr worker agents through to a Codeberg PR using the argus binary, which runs the mechanical half of supervision (spawn workers in worktrees, gate diffs against git, ship only on an approving verdict) as plain Go instead of inside the LLM. Use when the user says 'supervise the panes with argus', 'run argus', 'gate these workers', 'ship with argus', or hands you parallel agent tasks and argus is on PATH. Prefer this over hand-running the supervise loop when argus is available."
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

## Preflight

```bash
command -v argus herdr claude    # all three must resolve
echo "$CODEBERG_TOKEN"           # required only for `argus ship`
```

- `herdr` on PATH — argus talks to it over its CLI. You are usually already inside herdr.
- `claude` on PATH — needed for `argus review` and `supervise --review`.
- `CODEBERG_TOKEN` in env — needed for `argus ship` only.

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

Turn on the LLM review path for escalations with `--review` (headless `claude -p`):

```bash
argus supervise --repo <path> --tasks "risky change" --branches feat-x --review
```

Fetch briefs straight from forge issues instead of writing them by hand:

```bash
argus supervise --repo <path> --issues 141,142,143 --review
```

Useful flags (see `argus supervise --help` for all):

- `--base origin/main` — base ref new worktrees branch from.
- `--interval 15s` — status poll cadence.
- `--timeout 0` — per-worker deadline; `0` waits indefinitely.
- `--review-model <id>` — model for `--review`.
- Gate tuning: `--max-diff-lines`, `--shared-glob`, `--os-glob`, `--always-review-glob`.

## 2. React to escalations

The gate is the cheap path: it auto-approves only when the worker is `awaiting_review`,
every reported test passed, the diff is within `--max-diff-lines`, no shared path was
touched, and any OS-integration change carries real-world proof. **Two checks a worker
cannot talk past** — argus measures the diff from git, so an unmeasurable diff or a
material under-report escalates regardless of what `status.json` claims.

When argus escalates without `--review`, it surfaces the decision to you. Read the diff
yourself before approving — same discipline as the hand-run skill:

```bash
git -C <worktree> diff origin/main
```

Approve read-only/build/test/own-worktree changes. Hold and ask the user for anything on
shared or production state, force-pushes to shared branches, or deletes outside a tempdir.
For OS-integration changes (systemd, launchd, install scripts), demand real-world proof,
not mocked unit tests plus a dry-run.

Run a one-shot review of any worktree on demand:

```bash
argus review --worktree <path> --base origin/main --task "issue 142" --reasons "touches sink dispatch"
```

## 3. Ship

`ship` refuses without an approving verdict from a prior gate or review — that is the
point, so a request-changes actually blocks the PR. It opens the PR via the Codeberg API
and unstages argus's own control-plane files (`.claude/argus`, scoped permission files)
so they never reach the PR.

```bash
argus ship --worktree <path> --issue 42 --dry-run   # confirm first
argus ship --worktree <path> --issue 42
```

Only use `--force` (skip the gate) when the user explicitly authorizes shipping an
unverified change.

## 4. Post-merge conflict handoff

When a sibling PR merges first and leaves another worktree conflicting, dispatch that
worktree's own worker to rebase — it already has full context:

```bash
argus rebase --worktree <path> --base main
```

## Inspect the run

```bash
argus stats                 # escalation rate, review parse-fail rate, tokens per task
argus supervise ... --debug # tee the typed event log to stderr; always persisted to ~/.argus/runs
```

## What NOT to do

- Don't hand-run the herdr pane loop when argus is on PATH — that reintroduces the token
  cost argus exists to remove. Use [[supervise-agents]] only as the no-argus fallback.
- Don't approve a ship off a worker's summary — argus gates on the measured diff; you
  read it too.
- Don't reach for `--force` on `ship` unless the user explicitly authorized it.
- Don't `--dry-run`-skip on a first real run against an unfamiliar repo.
