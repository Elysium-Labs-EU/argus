# Repo config: .argus/config.yml

argus hardcodes no build/test toolchain — it never itself runs a build, test,
or lint command; the only self-verifying gate is `ship`'s required approving
verdict (see `internal/supervisor/reviewer.go`). The only toolchain-flavored
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

All three keys are optional; a missing file is equivalent to an empty one.

## Keys

- **`base_branch`** — the branch new worktrees fork from and PRs target.
  Precedence (highest wins): an explicit `--base` flag on the command line,
  the base already persisted on a worktree's own `status.json` (set once by
  `supervise` at worktree-creation time), this key, the repo's detected
  `origin/HEAD`, then the literal `"main"`.
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
  exists to avoid. A fixed line ("Do NOT git commit or push; argus ships.")
  always follows it — that one is argus's own pipeline invariant, not
  something a repo can opt out of.

## Why this is safe from a worker

`.argus/config.yml` lives in the main repo checkout, never inside a worker's
worktree, so a spawned worker cannot reach or tamper with it — worktree
isolation provides this for free.
