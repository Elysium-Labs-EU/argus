package supervisor

import (
	"fmt"
	"strings"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

// AnswerMessage is the chat message `argus worker answer` delivers into a
// blocked worker's live pane once it has recorded the supervisor's
// resolution to status.json. It is typed directly into the agent's own
// input (see cmd's deliverAnswerToPane) rather than routed through
// brief.md the way a full task brief is — a short answer is exactly the
// kind of one-line content a live agent would receive as its own next chat
// turn, not a fresh multi-line task to go read from disk.
func AnswerMessage(q *protocol.Question, blockedReason, answer string) string {
	var b strings.Builder
	b.WriteString("The supervisor answered your blocked question.\n\n")
	switch {
	case q != nil && q.Text != "":
		fmt.Fprintf(&b, "Your question: %s\n", q.Text)
	case blockedReason != "":
		fmt.Fprintf(&b, "Your blocked_reason: %s\n", blockedReason)
	}
	fmt.Fprintf(&b, "Supervisor's answer: %s\n\n", answer)
	b.WriteString("Resume work based on this answer. Report your next phase via `argus worker report <phase>` once you've acted on it.\n")
	return b.String()
}
