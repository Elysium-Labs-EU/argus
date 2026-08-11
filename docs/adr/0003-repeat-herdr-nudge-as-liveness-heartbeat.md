# 0003. Treat a repeated herdr nudge as a liveness heartbeat, not a one-shot reminder

Date: 2026-08-11
Status: Accepted

## Context

- `checkHerdrStuck` (the main supervise loop) and `paneStuckTracker.check` (the shared `rebase`/`rework` single-worker wait, `WaitForStatus`) both escalate a worker once herdr reports its pane `agent_status="done"` for over `herdrStuckThreshold` (2 minutes) with `status.json` still not at a terminal phase.
- Herdr reports `agent_status="done"` whenever a pane's agent turn ends — indistinguishable, from that signal alone, between a worker that finished without reporting, one parked on an unrecognized prompt, and one that simply yielded its turn waiting on a long backgrounded command (e.g. this repo's own `gate_verify_command`).
- Both detectors already send one `AgentPrompt` nudge into a "done" pane as a liveness probe before escalating, and reset the stuck timer if it lands — but only once per stuck streak. A worker that answers the nudge and then goes quiet again waiting on the same still-running command crosses the next threshold window and gets escalated anyway.
- Observed repeatedly in production: a worker explicitly answered the nudge, said it was still waiting on a backgrounded `make test`/`make ci`, and the round was aborted seconds later regardless. The round was consumed and no gate or review ran, even though the worktree and the worker's actual progress were intact.
- `rebase` and `rework` both dispatch through the same `WaitForStatus`, so this was never a difference in their own recovery logic — the underlying detector's one-shot nudge escalates identically for both, and for the main supervise loop's own worker set.

## Decision

- Send a fresh `AgentPrompt` nudge every time a "done" pane re-crosses `herdrStuckThreshold`, not just once per stuck streak: a successful nudge resets the elapsed timer exactly like the first one already does, in both `checkHerdrStuck` and `paneStuckTracker.check`.
- Escalate only when a nudge attempt itself fails to land (`AgentPrompt` errors or times out) — that is the one signal a pane genuinely cannot respond, as opposed to a worker that keeps proving it is alive but simply has not finished yet.
- Leave the "blocked" and idle-without-report paths untouched: neither ever sends a nudge today, since both name a state (an unanswered permission prompt, or an interactive screen herdr's blocked-detector does not recognize) that a chat-level `AgentPrompt` cannot unstick.

## Rejected

- A flat, larger fixed timeout (10 or 30 minutes, say). Rejected: any fixed number is still wrong for some `gate_verify_command`, and wrong in the other direction for a repo with a fast one — the existing 2-minute threshold already flakes against this repo's own multi-minute `make ci && make test-supervision-orb`.
- A per-repo configurable timeout alongside `gate_verify_command`. Rejected as unnecessary once liveness is proven directly: the repeated nudge already asks the worker whether it is still going, and that scales automatically with however long the configured verify command actually takes, with no separate config surface that could itself drift out of sync with the command it is meant to bound.
- Reading pane output text directly to look for a natural-language "still working" reply. Rejected: `AgentPrompt` already gives a structural liveness signal (did this pane accept a new turn and start something) without argus having to parse and trust arbitrary chat text as a load-bearing state input.

## Consequences

- A worker legitimately waiting on a long backgrounded command survives indefinitely as long as it keeps accepting nudges, matching `WaitForStatus`'s own existing "real work can take arbitrarily long" philosophy for terminal-status polling.
- `rebase` and `rework` get the same relief automatically, with no dispatcher-specific retry wrapper needed for either — both already shared the one detector.
- A genuinely stuck worker (crashed, or parked on a prompt `AgentPrompt` cannot reach) is not slower to detect: the very next threshold crossing where the nudge itself fails still escalates immediately, same as today's first-failure path.
- A worker that is slow to accept the nudge now gets pinged every threshold window instead of once — acceptable added chat noise for the false-abort it prevents.
