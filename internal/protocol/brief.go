package protocol

import (
	"fmt"
	"strings"
)

// AskGatedCommands are the Bash command prefixes settingsFor's Ask list gates
// for every worker's worktree (internal/supervisor/agentadapter.go): argus
// runs these itself once a report lands, rather than let a worker publish
// argus's own commit before a verdict exists. Every brief-rendering function
// pulls its "do not run these yourself" sentence from this one slice via
// NeverRunBrief instead of hand-authoring its own version of the warning —
// otherwise a brief's wording and the worktree's actual permission grant are
// two independently maintained lists that can silently drift apart, which is
// exactly how a rebase dispatch once ended up mandating a `git push` its own
// worktree still asked a human to approve, with nobody watching to answer.
var AskGatedCommands = []string{"git commit", "git push"}

// NeverRunBrief renders the shared "do not run these yourself" clause every
// worker brief includes for commands (see AskGatedCommands). Callers append
// their own reason (why argus runs it instead) after the returned sentence.
// Empty commands renders "" — not every brief needs this clause.
func NeverRunBrief(commands []string) string {
	if len(commands) == 0 {
		return ""
	}
	return "Do NOT run " + strings.Join(commands, " or ") + " yourself"
}

// WriterBrief renders the instruction block argus injects into every worker
// pane's task brief, against base — the same ref MeasureDiff gates the
// worker's diff against. It is the writer half of this package's contract: it
// tells the worker to report exactly the Status shape that Load decodes
// through the guarded `argus worker report` subcommand, after every phase
// transition. Keeping the writer spec in the same package as the reader and
// the transition table (see transition.go) is what stops the three from
// drifting — if the struct or the legal-transition table changes, this text
// is right next to them.
//
// The diff_stat instruction below must diff against the same base the gate
// measures against, not HEAD: HEAD only equals base before a worker's first
// commit, so `git diff --stat HEAD` would silently miss every prior commit's
// lines on any later report (a rebase, a rework round, or just normal
// incremental work), causing the gate's unwaivable "worker under-reported
// diff" check to fire on an honest self-report.
func WriterBrief(base string) string {
	return fmt.Sprintf(writerBriefTemplate, base)
}

const writerBriefTemplate = `## Status reporting (required)

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
        {"cmd": "go test ./internal/protocol/...", "target": "protocol package unit tests", "result": "pass|fail|skipped"}
      ],
      "diff_stat": {"files": 0, "insertions": 0, "deletions": 0}
    }
    JSON

Each ` + "`tests[]`" + ` entry's ` + "`cmd`" + ` must be the exact, fully
self-contained, copy-paste-runnable command you ran — the gate re-runs it
byte-for-byte to reproduce a claimed pass, so a paraphrase or a command that
depends on ` + "`target`" + ` being appended will replay wrong and fail even
though your real run passed. ` + "`target`" + ` is only a descriptive label
naming what ` + "`cmd`" + ` exercised (a package, a VM, a service, a
Dockerfile) — it is never appended to ` + "`cmd`" + `. The one exception: if
` + "`target`" + ` names a real subdirectory of your worktree (a monorepo
with one Makefile/go.mod per module), the gate replays ` + "`cmd`" + ` from
inside that directory instead of the worktree root.

If a task asks you to prove a check catches a regression (deliberately break
something, confirm it fails, then revert), report that broken run as its own
` + "`tests[]`" + ` entry with ` + `"result": "fail", "expected_result": "fail"` + `
— the gate reports this informationally instead of escalating it as a real
regression. This only works alongside a normal passing entry for the
reverted, clean state; an intentional failure with no passing entry to show
the revert still escalates, since nothing then proves the break was undone.

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

Compute ` + "`diff_stat`" + ` the same way argus itself will: ` + "`git diff --stat %s`" + `
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
prints the legal next phases in the error) and retry with the right one.

When you report "blocked", set ` + "`question`" + ` alongside blocked_reason if
your question is a specific, answerable decision (e.g. "wait for branch X to
merge and rebase, or cherry-pick its commit now?") rather than only a
narrative reason — ` + "`options`" + ` is optional, for when the choice is
literally enumerable. A supervisor resolves it with
` + "`argus worker answer <worktree> <text>`" + ` (or ` + "`--option N`" + `
against your options), which is delivered into your pane as a chat message
and recorded on status.json as a durable trace — act on it, then report your
next phase as normal.

While you are still working, a supervisor who spots a wrong turn early may
send you a follow-up via ` + "`argus worker steer`" + ` as an ordinary chat
message instead of waiting for you to reach a terminal phase. It is not a new
task: keep your existing plan and context, fold the note into your current
turn, and continue reporting phases as normal.`
