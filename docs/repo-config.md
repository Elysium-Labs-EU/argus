# Repo config: .argus/config.yml

argus hardcodes no build/test toolchain — it assumes no default build, test,
or lint command of its own. `verify_command` below is one such opt-in
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
- **`verify_command`** — a shell command the gate re-runs inside a worker's
  worktree once it reaches a terminal phase (e.g. `"make lint"`,
  `"golangci-lint run"`), closing the gap where a diff earns a clean gate
  verdict and then fails at `ship`'s `git commit` because the repo's own
  pre-commit hooks ran a check the gate never reproduced. A non-zero exit
  (after one retry, to absorb shared-machine flakiness — see
  `RunVerifyCommand`) is an unwaivable escalation: no reviewer verdict can
  approve past it, the same treatment a reproduced test-claim mismatch gets.
  Precedence: an explicit `--verify-cmd` flag, then this key, then unset (no
  command runs — today's prior behavior). Unset by default; a repo owner
  opts in.

## Why this is safe from a worker

`.argus/config.yml` lives in the main repo checkout, never inside a worker's
worktree, so a spawned worker cannot reach or tamper with it — worktree
isolation provides this for free.
