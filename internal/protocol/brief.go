package protocol

// WriterBrief is the instruction block argus injects into every worker pane's
// task brief. It is the writer half of this package's contract: it tells the
// worker to persist exactly the Status shape that Load decodes, at exactly the
// StatusPath location, after every phase transition. Keeping the writer spec in
// the same package as the reader is what stops the two halves from drifting —
// if the struct changes, this text is right next to it.
const WriterBrief = `## Status reporting (required)

After each phase of your work, write your current status to
` + "`.claude/argus/status.json`" + ` in your worktree, overwriting it each time.
Your supervisor (argus) reads this file instead of your terminal output, so keep
it accurate. Write valid JSON in exactly this shape:

    {
      "updated_at": "2026-07-18T12:00:00Z",   // RFC3339, current time
      "task": "<issue id or one-line brief>",
      "branch": "<your branch name>",
      "phase": "planning|working|self_test|awaiting_review|done|blocked",
      "real_world_proof": "<how you verified against a real target, or \"\" if n/a>",
      "pr_url": "<set once the PR exists, else \"\">",
      "blocked_reason": "<set only when phase is blocked, else \"\">",
      "files_touched": ["path/one.go", "path/two.go"],
      "tests": [
        {"cmd": "make test", "target": "./internal/...", "result": "pass|fail|skipped"}
      ],
      "diff_stat": {"files": 0, "insertions": 0, "deletions": 0}
    }

Set phase to "awaiting_review" when you want the diff reviewed, "blocked" (with
blocked_reason) when you need a decision only the supervisor can make, and "done"
only once the change is shipped. Update the file at every transition, not just at
the end.`
