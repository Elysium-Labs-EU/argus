Task: Implement the plan at /Users/rtuerlings/.claude/plans/quiet-soaring-ember.md in full. Read that file first -- it has exact function signatures, exact doc-comment and schema.json text, and exact test additions for every front; do not improvise names or wording it already specifies. Implement the seven fronts in this exact order (the plan's own "Implementation order" section): Front 2 (rename ship_lint/verify_command/worktree_setup_cmd to ship_verify_command/gate_verify_command/worktree_setup_command with deprecated-alias backward compat), Front 6 (WorktreePath safety checks, breaking signature change to return an error), Front 5 (new --worktree-dir flag, separate commit from Front 6), Front 7 (doc-only: worktree_dir + worktree_setup_command interaction), Front 4 (.argus/config.yml unconditional self-protection check in the review gate), Front 1 (argus init prompts for every Config field, reflection-based completeness test), Front 3 (doc-only: gate_verify_command as the custom-rule escape hatch). Make each front its own commit, in that order, following the plan's own dependency notes -- Front 1 depends on Front 2's new Deprecated field existing so its field-count test is correct; Front 5 and 6 touch the same file but must stay separate commits. After every front, run go build ./... and go vet ./... and go test ./cmd/... ./internal/supervisor/... ./internal/repoconfig/... -- keep it green throughout, not just at the end, per the plan's own Verification section. Run golangci-lint run (or make lint) before considering any commit done. Write a status report before finishing that maps each of the plan's 7 fronts to the commit(s) that implemented it, so the supervising side can cross-check task completion.
Branch: config-surface-rename-and-safety

Work only inside /Users/rtuerlings/Coding/elysium-labs/argus/.claude/worktrees/config-surface-rename-and-safety. Never delete, reset, or touch files outside it; another
agent may share the parent repo. Write a todo list before anything else.

Do the work and verify it (build + tests). Do NOT git commit or git push — argus
handles shipping. When the change is complete and tests pass, set your status
phase to "awaiting_review" (not "done"); use "blocked" if you need a decision only
the supervisor can make.

## Status reporting (required)

After each phase of your work, report your status by running:

    argus worker report <phase>

piping the rest of the status as a JSON body on stdin, in exactly this shape:

    argus worker report working <<'JSON'
    {
      "task": "<issue id or one-line brief>",
      "branch": "<your branch name>",
      "real_world_proof": "<how you verified against a real target, or \"\" if n/a>",
      "pr_url": "<set once the PR exists, else \"\">",
      "blocked_reason": "<set only when phase is blocked, else \"\">",
      "question": {"text": "<optional: a specific decision only the supervisor can make>", "options": ["<optional choice 1>", "<optional choice 2>"]},
      "title": "<conventional-commit-style PR/commit title, <=72 chars>",
      "files_touched": ["path/one.go", "path/two.go"],
      "plan": ["<todo item 1>", "<todo item 2>"],
      "tests": [
        {"cmd": "make test", "target": "./internal/...", "result": "pass|fail|skipped"}
      ],
      "diff_stat": {"files": 0, "insertions": 0, "deletions": 0}
    }
    JSON

If a task asks you to prove a check catches a regression (deliberately break
something, confirm it fails, then revert), report that broken run as its own
`tests[]` entry with "result": "fail", "expected_result": "fail"
— the gate reports this informationally instead of escalating it as a real
regression. This only works alongside a normal passing entry for the
reverted, clean state; an intentional failure with no passing entry to show
the revert still escalates, since nothing then proves the break was undone.

`title` becomes the PR and commit title argus ships with — write it
yourself, informed by the issue and by what you actually built (your diff),
not copied verbatim from the issue title. Use whichever conventional-commit
prefix actually fits the change (`feat:`, `fix:`, `chore:`, etc.) —
do not default to `fix:` for a change that isn't a fix. Keep it
<=72 chars; a longer title gets truncated or rejected at ship time, so a
tight, accurate summary beats a padded one. Leaving it empty is legal — ship
then falls back to the fetched issue title — but a title you write yourself,
grounded in the actual diff, is almost always more accurate than the issue
title alone.

Compute `diff_stat` the same way argus itself will: `git diff --stat main`
for tracked edits, plus every untracked, non-ignored file (`git ls-files --others --exclude-standard`)
counted as a touched file with its full line count added to insertions. Plain
`git diff` alone is invisible to files you just created — a new source
file plus its test is the single most common cause of a spurious "under-reported
diff" escalation, and that escalation is unwaivable.

Do not write `.claude/argus/status.json` yourself — argus loads your
current status, rejects <phase> if it is not a legal move from it, and only
then stamps the timestamp itself and persists. You do not set (and should not
send) an updated_at; argus's clock is the only one that counts.

When you report phase "planning", `plan` must be a non-empty array of
your actual todo items (write them with your todo-list tool first, then list
them here) — argus rejects the move from "planning" to "working" if the
planning report on file has no plan evidence, and independently checks your
own session transcript for a real todo-list tool call before approving your
work. A plan array with no matching tool call does not count.

<phase> is one of: planning, working, self_test, awaiting_review, blocked. Set
it to "awaiting_review" when you want the diff reviewed, "blocked" (with
blocked_reason in the body) when you need a decision only the supervisor can
make. "done" is never a phase you report — argus sets that once the change is
shipped. Report again at every transition, not just at the end; a rejected
transition means you called it out of order — check your current phase (argus
prints the legal next phases in the error) and retry with the right one.

When you report "blocked", set `question` alongside blocked_reason if
your question is a specific, answerable decision (e.g. "wait for branch X to
merge and rebase, or cherry-pick its commit now?") rather than only a
narrative reason — `options` is optional, for when the choice is
literally enumerable. A supervisor resolves it with
`argus worker answer <worktree> <text>` (or `--option N`
against your options), which is delivered into your pane as a chat message
and recorded on status.json as a durable trace — act on it, then report your
next phase as normal.
