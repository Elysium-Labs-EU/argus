package supervisor

import (
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

// TestAnswerMessage pins the exact rendered text for each of AnswerMessage's
// three input shapes: a worker's structured Question, only a freeform
// BlockedReason, and neither — so a future edit to the switch in
// AnswerMessage can't silently drop a case without a test noticing.
func TestAnswerMessage(t *testing.T) {
	cases := []struct {
		name          string
		question      *protocol.Question
		blockedReason string
		answer        string
		want          string
	}{
		{
			name:          "question text set",
			question:      &protocol.Question{Text: "wait and rebase, or cherry-pick now?", Options: []string{"wait", "cherry-pick"}},
			blockedReason: "needs a decision on the guard under test",
			answer:        "cherry-pick now",
			want: "The supervisor answered your blocked question.\n\n" +
				"Your question: wait and rebase, or cherry-pick now?\n" +
				"Supervisor's answer: cherry-pick now\n\n" +
				"Resume work based on this answer. Report your next phase via `argus worker report <phase>` once you've acted on it.\n",
		},
		{
			name:          "question nil, blocked reason set",
			question:      nil,
			blockedReason: "no forge configured for this repo",
			answer:        "use codeberg",
			want: "The supervisor answered your blocked question.\n\n" +
				"Your blocked_reason: no forge configured for this repo\n" +
				"Supervisor's answer: use codeberg\n\n" +
				"Resume work based on this answer. Report your next phase via `argus worker report <phase>` once you've acted on it.\n",
		},
		{
			name:          "question nil, blocked reason empty",
			question:      nil,
			blockedReason: "",
			answer:        "go ahead",
			want: "The supervisor answered your blocked question.\n\n" +
				"Supervisor's answer: go ahead\n\n" +
				"Resume work based on this answer. Report your next phase via `argus worker report <phase>` once you've acted on it.\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AnswerMessage(tc.question, tc.blockedReason, tc.answer)
			if got != tc.want {
				t.Errorf("AnswerMessage() =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// TestAnswerMessageQuestionWithEmptyTextFallsBackToBlockedReason confirms a
// non-nil Question whose Text is empty (a caller-constructed edge case, not
// something `argus worker answer` itself produces) is treated the same as no
// Question at all — the switch's guard is q != nil && q.Text != "", not just
// q != nil.
func TestAnswerMessageQuestionWithEmptyTextFallsBackToBlockedReason(t *testing.T) {
	got := AnswerMessage(&protocol.Question{}, "still needs a decision", "proceed")
	want := "The supervisor answered your blocked question.\n\n" +
		"Your blocked_reason: still needs a decision\n" +
		"Supervisor's answer: proceed\n\n" +
		"Resume work based on this answer. Report your next phase via `argus worker report <phase>` once you've acted on it.\n"
	if got != want {
		t.Errorf("AnswerMessage() =\n%q\nwant\n%q", got, want)
	}
}
