# Repo config: .argus/config.yml

argus hardcodes no build/test toolchain — it assumes no default build, test,
or lint command of its own. `gate_verify_command` below is one such opt-in
exception: a repo owner sets a specific shell command via the key, and argus
re-runs exactly that command as part of the gate, alongside `ship`'s
required approving verdict (see `internal/supervisor/reviewer.go`). The only
other toolchain-flavored
assumptions argus ever made were a Go/make-shaped default permission allow
list and a `"main"` base branch, both hardcoded in Go. `.argus/config.yml` — a
single optional file in the *main* repo checkout, not inside any worker's
worktree — replaces that hardcoding with a declarative contract, so a new
toolchain (bazel, cargo, gradle, poetry, ...) never needs an argus release.

Run `argus init` to detect a repo's toolchain and write a starting point, or
hand-write the file yourself:

```yaml
# .argus/config.yml
base_branch: "develop"
allow:
  - "Bash(task *)"
  - "Bash(pnpm *)"
brief_note: "Add a focused test and keep task frontend:ci green. Follow the repo AGENTS.md."
```

All keys are optional; a missing file is equivalent to an empty one.

## Shape: three regions

Every key lives in one of three regions, grouped by *when it actually runs*,
and the schema (`schemas/config.schema.json`) enforces per-region/per-phase
validity at load — a key in the wrong place is a load-time error naming
where it belongs, not a silent no-op.

- **Top-level** — static repo facts and spawn-time keys that don't vary by
  worker phase: `base_branch`, `worker_placement`, `forge`, `status_page`,
  `worktree_dir`, `owner_stale_after`, `rework_budget`,
  `worktree_bootstrap_command`, `launcher`, `allow`, `brief_note`.
- **`ship:`** — argus-side actions that run after a worker is `done` and a
  verdict is recorded, initiated by the operator, not a worker phase:
  `verify_command`, `title_prefix_template`.
- **`phases:`** — per-worker-lifecycle-phase keys, one nested block per
  phase name (`planning`, `working`, `self_test`, `awaiting_review`,
  `blocked`). `allow`/`deny`/`skip` are live rules checked continuously
  while a worker reports that phase, valid on every phase; the gate/review
  cluster (`gate_verify_command`, `max_diff_lines`, `proof_required_paths`,
  `always_review_paths`, `review_note`, `review_effort`) fires once, on
  entering the terminal `awaiting_review` phase, and is valid only there.

```yaml
ship:
  verify_command: "make ci"          # was ship_verify_command

phases:
  working:
    deny: ["docker push"]
  awaiting_review:
    gate_verify_command: "make ci"   # was the top-level gate_verify_command
    max_diff_lines: 800
    review_note: "Pay extra attention to internal/supervisor changes."
    review_effort: high
```

The flat/dotted forms these keys used before this shape existed
(`ship_verify_command`, `gate_verify_command`, `phase.<name>.deny`, the flat
review-policy keys) keep parsing for one transition — each emits a one-line
deprecation warning pointing at its new nested location, never a hard break.

## Keys

- **`base_branch`** — the branch new worktrees fork from and PRs target.
  Precedence (highest wins): an explicit `--base` flag on the command line,
  the base already persisted on a worktree's own `status.json` (set once by
  `supervise` at worktree-creation time), this key, the repo's detected
  `origin/HEAD`, then the literal `"main"`.
- **`worktree_dir`** — where a spawned worker's worktree is created. Unset
  keeps argus's default, `<repo>/.claude/worktrees/<branch>`. A relative
  value is joined under the repo root, so `".."` points worktrees at a
  sibling-of-repo convention (`<parent>/<branch>`) instead of a nested one;
  an absolute value is used as-is. Set this when a repo already has its own
  worktree convention (e.g. pane-discovery or cleanup scripts that expect
  sibling directories) so argus-created worktrees land where that tooling
  already looks, instead of a second, uncoordinated set under
  `.claude/worktrees/`.
- **`forge`** — `"gitlab"` or `"gitea"`, the API shape for a repo whose forge
  is self-hosted and outside `forge.New`'s auto-detected allowlist
  (`github.com`, `gitlab.com`, `codeberg.org`); a host name alone can't say
  which shape a self-hosted instance actually speaks, so this is a static
  per-repo fact set once instead of repeated as `--forge gitlab`/`--forge
  gitea` on every `ship`/`supervise`/`worktree prune` invocation. Precedence:
  an explicit `--forge` flag, then this key, then auto-detect (which still
  refuses any host outside the three-host allowlist). `argus init` prompts
  for it, or pass `--forge` to `argus init` directly.
- **`status_page`** — the status page `internal/svcstatus` appends to a
  forge-request or push failure that looks host-shaped (5xx, or no response
  at all — see `svcstatus.WorthMentioning`), so an operator hitting one knows
  where to check instead of guessing it's an argus bug. svcstatus's built-in
  map only knows the three hosted forges' own status pages
  (`githubstatus.com`, `status.gitlab.com`, `status.codeberg.org`); a
  self-hosted host has no built-in entry and needs this key (or Atlassian
  Statuspage/Cachet/a static page an operator runs alongside it) to get any
  hint at all. Precedence: an explicit `--status-page-url` flag on `ship`,
  then this key, then svcstatus's built-in map (empty for an unrecognized
  host either way). `argus init` prompts for it.
- **`allow`** / **`phases.<name>.allow`** — see "Worker permissions: `dontAsk`
  and the per-phase allow set" below; this is the "materialized toolchain"
  layer of that model, the one bucket that's genuinely per-repo policy.
  `allow` is phase-independent (granted in every phase); `phases.<name>.allow`
  adds to it for one specific phase only. A CLI `--allow` flag appends to both
  on top, for a one-off run. With no config file, no build/test tooling is
  pre-cleared for anyone — a worker gets only the code-guaranteed structural
  floor (read-only git, argus's own status self-calls) and edits confined to
  the worktree.
- **`brief_note`** — a single opaque string appended verbatim to a
  generated worker brief when the brief comes from `--issues`/`--jira-issues`.
  argus assigns it no meaning beyond that — it is never parsed or enforced,
  so it can't grow the same never-ending toolchain-adapter problem this file
  exists to avoid. Fixed lines ("Do NOT git commit or push; argus ships." and
  diff-counting guidance that mirrors `MeasureDiff`'s own untracked-file
  handling) always follow it — those are argus's own pipeline invariants, not
  something a repo can opt out of.
- **`phases.awaiting_review.gate_verify_command`** — a shell command the gate
  re-runs inside a worker's worktree once it reaches the terminal
  `awaiting_review` phase (e.g. `"make lint"`, `"golangci-lint run"`),
  closing the gap where a diff earns a clean gate verdict and then fails at
  `ship`'s `git commit` because the repo's own pre-commit hooks ran a check
  the gate never reproduced. A non-zero exit (after one retry, to absorb
  shared-machine flakiness — see `RunGateVerifyCommand`) is an unwaivable
  escalation: no reviewer verdict can approve past it, the same treatment a
  reproduced test-claim mismatch gets. Precedence: an explicit
  `--gate-verify-command` flag (`--verify-cmd` still works as a deprecated
  alias), then this key, then unset (no command runs — today's prior
  behavior). Unset by default; a repo owner opts in. The flat top-level
  `gate_verify_command` (and its older alias `verify_command`) still parses
  as a deprecated form of this key.
- **`worktree_bootstrap_command`** — a shell command run once, synchronously, inside
  a freshly created worktree, right after `git worktree add` succeeds and
  before the worker's agent is spawned (see `RunWorktreeBootstrapCommand`). Use this
  when a repo's tasks depend on gitignored per-developer local config (env
  files, local settings) that exists only in the original checkout — a plain
  `git worktree add` never copies it, so without this hook a spawned worker
  hits a silent, confusing file-not-found failure instead of a clear signal
  that bootstrap state is expected but missing. A non-zero exit fails
  worktree creation the same way a `git worktree add` failure already does —
  no retry, since a bootstrap script is expected to be deterministic, not
  contend for shared-machine resources the way a build/test command might.
  Precedence: an explicit `--worktree-bootstrap-command` flag
  (`--worktree-setup-cmd` still works as a deprecated alias), then this key,
  then unset (no command runs — today's prior behavior). Unset by default; a
  repo owner opts in.
- **`owner_stale_after`** — how long a worktree's owner-lease heartbeat
  (`.claude/argus/owner.json`, written by `internal/ownership`) may go quiet
  before `rework`/`rebase`/`ship`/`worker answer` let a *mismatched* caller
  proceed anyway (with a logged notice) instead of refusing outright — see
  the ownership-lease guarantee in `.claude/skills/argus/SKILL.md`. A Go
  duration string, e.g. `"30m"` or `"1h"`. Precedence: an explicit
  `--owner-stale-after` flag, then this key, then the built-in default
  (`ownership.DefaultStaleAfter`, 30m). Unlike `--owner`/
  `--force-foreign-owner` — per-invocation identity/override, never
  config-able, since baking either into a repo-committed file would mean
  every session claims the same lease identity or silently bypasses every
  mismatch for every developer — this key is a repo-wide policy choice, the
  same shape as `max_diff_lines`.
- **`rework_budget`** — the restart budget for `argus rework`: the total
  number of rework rounds a worktree may ever be dispatched for, across every
  separate `rework` invocation over its lifetime — distinct from `rework`'s
  own `--max-rounds`, which only bounds one invocation's internal loop.
  Without this ceiling, a supervisor that keeps re-invoking `rework` after
  each invocation's own rounds are exhausted can loop the same worker
  indefinitely. Exceeding the budget records an unwaivable
  `rework-budget-exceeded` verdict (`.claude/argus/verdict.json`) instead of
  dispatching another round, distinct from a plain `surfaced-awaiting-human`
  escalation so a report shows *why* it needed a human. `0` disables the
  budget entirely (unbounded, the prior behavior); unset falls back to
  `supervisor.DefaultMaxReworkBudget`. Precedence: an explicit
  `--max-rework-budget` flag, then this key, then the built-in default.

## Worker permissions: `dontAsk` and the per-phase allow set

Workers launch under Claude Code's `--permission-mode dontAsk`: never prompt
a human, resolve every call from the rendered `settings.local.json` alone —
read-only tools stay auto-allowed by the mode itself, and anything else is
either in `permissions.allow` or it's denied and fed back to the worker, not
asked and not hung. This is default-deny, the opposite of the old
`--permission-mode auto` (auto-approve, chase a denylist that can never
close).

The resolved allow set for one phase is a strict layering, floor
authoritative:

```
resolved allow(phase) =
    structural-floor          (code-guaranteed, every phase)
  ∪ allow                     (config, phase-independent)
  ∪ phases.<phase>.allow      (config, this phase only)
  ∪ --allow flags             (one-off, every phase)
  − deny-floor                (subtracted last, unremovable)
```

- **structural floor** (code, every phase, cannot be removed by any config):
  read-only tools, read-only git only (`git status`/`git diff`/`git log`/
  `git ls-files`), and a worker's own `argus worker report`/`answer`/`steer`
  self-calls. `git ls-files` is here because every worker brief's shared
  status-reporting block instructs `git ls-files --others --exclude-standard`
  to compute `diff_stat`'s untracked-file count — an argus-authored
  instruction every worker needs, in every phase, not repo-specific policy. A
  worker never runs `git add`, `git commit`, or `git push` — it edits files,
  leaves them uncommitted, and reports; the gate measures the *uncommitted*
  working-tree diff; `argus ship` is what stages, commits, and pushes. With
  no config file at all, this floor is all a worker gets: enough to edit
  files and be gated, no repo toolchain command — skipping setup makes a
  worker *more* restricted, never less.
- **`allow` / `phases.<name>.allow`** (config, genuinely editable per repo):
  the materialized toolchain — the one bucket that's real per-repo policy.
  Written by `argus init` from toolchain detection (`Taskfile.yml`,
  `Makefile`, `package.json`, `go.mod`), then co-built interactively with the
  operator, one `phases.<name>` at a time
  (`planning`/`working`/`self_test`/`awaiting_review`/`blocked`). Edited
  freely by hand afterward.
- **`--allow` flags**: additive for one run, flat across every phase — the
  ad-hoc escape hatch and the no-`init` path.
- **deny floor** (code, every phase, subtracted last, unremovable): `argus
  ship`/`rework`/`review`/`supervise`, and `git commit`/`git push`. No
  `phases.<any>.allow` entry, materialized toolchain command, or `--allow`
  flag can re-grant any of these — even an entry as broad as
  `"Bash(git push*)"` sitting under `phases.working.allow` is stripped after
  the union, in every phase, not just `planning`.

Because `settings.local.json` is written once at session launch and can't
itself vary by the worker's *current* phase, the file that's actually
rendered is the union of every phase's own resolved allow — otherwise a
command legitimately scoped to `working` would simply be unavailable the
moment a worker reported `self_test`. A live `PreToolUse` hook (`argus worker
check-tool`) is what narrows a call back down to the worker's actual current
phase, re-reading `.argus/config.yml` fresh from the trusted main checkout on
every Bash call — a worker editing its own worktree's tracked copy has no
effect here, same as it has none on `ship`/`rework`/`review`'s own config
reads.

`argus init --refresh` re-materializes only the `allow`/`phases.<name>.allow`
suggestion from the current toolchain-detection default, leaving every other
key (including any hand-authored `deny`/`skip`) untouched — for a repo whose
`config.yml` predates an improved default set in a newer argus.

## Why this is safe from a worker

`.argus/config.yml` lives in the main repo checkout, never inside a worker's
worktree, so a spawned worker cannot reach or tamper with it — worktree
isolation provides this for free.
