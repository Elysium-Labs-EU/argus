package supervisor

import "fmt"

// SteerMessage is the chat message `argus worker steer` delivers into a
// worker's live pane while it is in PhaseWorking or PhaseAwaitingReview. It
// is typed directly into the agent's own input rather than routed through
// brief.md the way a full task brief is: the note augments the worker's
// current turn and existing plan, it does not replace them, so it reads as
// one more line of input from the supervisor, not a fresh task to go read
// from disk.
func SteerMessage(text string) string {
	return fmt.Sprintf(
		"The supervisor sent a follow-up — this is not a new task.\n\n"+
			"Note: %s\n\n"+
			"Keep your existing plan and context; fold this into your current turn instead of restarting. Continue and report your next phase via `argus worker report <phase>` as normal.\n",
		text,
	)
}
