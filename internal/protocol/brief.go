package protocol

// WriterBrief is the instruction block argus injects into every worker pane's
// task brief. It is the writer half of this package's contract: it tells the
// worker to report exactly the Status shape that Load decodes through the
// guarded `argus worker report` subcommand (issue #92), after every phase
// transition. Keeping the writer spec in the same package as the reader and the
// transition table (see transition.go) is what stops the three from drifting —
// if the struct or the legal-transition table changes, this text is right next
// to them.
const WriterBrief = `## Status reporting (required)

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
      "title": "<conventional-commit-style PR/commit title, <=72 chars>",
      "files_touched": ["path/one.go", "path/two.go"],
      "plan": ["<todo item 1>", "<todo item 2>"],
      "tests": [
        {"cmd": "make test", "target": "./internal/...", "result": "pass|fail|skipped"}
      ],
      "diff_stat": {"files": 0, "insertions": 0, "deletions": 0}
    }
    JSON

` + "`title`" + ` becomes the PR and commit title argus ships with — write it
yourself, informed by the issue and by what you actually built (your diff),
not copied verbatim from the issue title. Use whichever conventional-commit
prefix actually fits the change (` + "`feat:`, `fix:`, `chore:`" + `, etc.) —
do not default to ` + "`fix:`" + ` for a change that isn't a fix. Keep it
<=72 chars; a longer title gets truncated or rejected at ship time, so a
tight, accurate summary beats a padded one. Leaving it empty is legal — ship
then falls back to the fetched issue title — but a title you write yourself,
grounded in the actual diff, is almost always more accurate than the issue
title alone.

Compute ` + "`diff_stat`" + ` the same way argus itself will: ` + "`git diff --stat HEAD`" + `
for tracked edits, plus every untracked, non-ignored file (` + "`git ls-files --others --exclude-standard`" + `)
counted as a touched file with its full line count added to insertions. Plain
` + "`git diff`" + ` alone is invisible to files you just created — a new source
file plus its test is the single most common cause of a spurious "under-reported
diff" escalation, and that escalation is unwaivable.

Do not write ` + "`.claude/argus/status.json`" + ` yourself — argus loads your
current status, rejects <phase> if it is not a legal move from it, and only
then stamps the timestamp itself and persists. You do not set (and should not
send) an updated_at; argus's clock is the only one that counts.

When you report phase "planning", ` + "`plan`" + ` must be a non-empty array of
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
prints the legal next phases in the error) and retry with the right one.`
