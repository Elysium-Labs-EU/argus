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
- **`allow`** — the base Claude Code permission-allow list `supervise` writes
  into each worker's generated `settings.local.json`, replacing (not just
  appending to) argus's old hardcoded Go/make list. A CLI `--allow` still
  appends to this on top, for a one-off run. With no config file, no
  build/test tooling is pre-cleared for anyone — only toolchain-neutral git
  read/write plumbing (`git status`/`diff`/`log`/`add`) and edits confined to
  the worktree.
- **`brief_note`** — a single opaque string appended verbatim to a
  generated worker brief when the brief comes from `--issues`/`--jira-issues`.
  argus assigns it no meaning beyond that — it is never parsed or enforced,
  so it can't grow the same never-ending toolchain-adapter problem this file
  exists to avoid. Fixed lines ("Do NOT git commit or push; argus ships." and
  diff-counting guidance that mirrors `MeasureDiff`'s own untracked-file
  handling) always follow it — those are argus's own pipeline invariants, not
  something a repo can opt out of.
- **`gate_verify_command`** — a shell command the gate re-runs inside a worker's
  worktree once it reaches a terminal phase (e.g. `"make lint"`,
  `"golangci-lint run"`), closing the gap where a diff earns a clean gate
  verdict and then fails at `ship`'s `git commit` because the repo's own
  pre-commit hooks ran a check the gate never reproduced. A non-zero exit
  (after one retry, to absorb shared-machine flakiness — see
  `RunGateVerifyCommand`) is an unwaivable escalation: no reviewer verdict can
  approve past it, the same treatment a reproduced test-claim mismatch gets.
  Precedence: an explicit `--verify-cmd` flag, then this key, then unset (no
  command runs — today's prior behavior). Unset by default; a repo owner
  opts in.
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
  Precedence: an explicit `--worktree-setup-cmd` flag, then this key, then
  unset (no command runs — today's prior behavior). Unset by default; a repo
  owner opts in.
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

## Why this is safe from a worker

`.argus/config.yml` lives in the main repo checkout, never inside a worker's
worktree, so a spawned worker cannot reach or tamper with it — worktree
isolation provides this for free.
