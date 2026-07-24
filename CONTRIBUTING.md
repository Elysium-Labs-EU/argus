# Contributing to argus

## Prerequisites

Go 1.26.5 or later and `make` are required. Verify with `go version` and `make --version`.

## Setup

```bash
git clone https://github.com/Elysium-Labs-EU/argus
cd argus
make setup
```

`make setup` installs the development toolchain (golangci-lint, nilaway, go-crap, ast-grep). Run `make help` to see all available targets; always prefer a make target over raw `go` or tool commands.

## Making Changes

Before touching any function or method, read [STYLE.md](STYLE.md) for the coding conventions that apply to all changes.

Open an issue before starting work on a non-trivial change. This avoids duplicate effort and makes sure the direction fits the project. Small fixes and documentation improvements can go straight to a PR.

Branch from `main` and name the branch after the change: `feat/rework-max-rounds`, `fix/gate-diff-ceiling`, `test/worktree-prune`.

## Running Tests

```bash
make ci
```

This runs the full local gate: tests with the race detector, golangci-lint, nilaway nil-safety analysis, the ast-grep scan, a coverage floor, the go-crap change-risk check, the eventlog-open gate, and the pubkey-sync check. It must pass before opening a PR. If lint reports violations, `make fix` resolves most of them automatically (gofmt and struct field alignment); run `make ci` again after.

## Commit Format

argus uses [Conventional Commits](https://www.conventionalcommits.org). The prefix determines which section of the changelog the commit appears in.

```
feat: add rework loop for request-changes verdicts
fix: clamp gate diff ceiling to configured max
test: cover worktree prune under merged branch
refactor: extract forge client selection to pure func
docs: document ARGUS_INSTALL_DIR env variable
chore: bump golangci-lint to v2.11.0
```

Breaking changes go in the commit footer: `BREAKING CHANGE: <description>`.

## Opening a Pull Request

Explain *why* the change is needed, not just what it does. Link the issue it resolves with `Closes #N`.

All CI checks must be green. A PR that breaks `make ci` will not be reviewed until it is fixed.
