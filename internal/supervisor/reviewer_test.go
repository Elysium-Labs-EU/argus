package supervisor

import (
	"context"
	"strings"
	"testing"
)

func fakeReviewRunner(reply string) reviewRunner {
	return func(_ context.Context, _, _ string, _ ...string) ([]byte, error) {
		return []byte(reply), nil
	}
}

func TestCLIReviewerParsesEnvelopeVerdict(t *testing.T) {
	// claude -p --output-format json wraps the model text in .result; the model
	// replied with a fenced JSON verdict.
	envelope := `{"type":"result","subtype":"success","result":"Here is my review:\n` +
		"```json\\n" + `{\"decision\":\"request-changes\",\"summary\":\"missing UPDATE path\",\"findings\":[\"only INSERT fixed\"]}` + "\\n```" + `"}`
	r := NewReviewerWithRunner(fakeReviewRunner(envelope))
	res, err := r.Review(context.Background(), &ReviewRequest{Task: "t", Diff: "x"})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if res.Decision != "request-changes" {
		t.Errorf("decision: got %q", res.Decision)
	}
	if res.Summary != "missing UPDATE path" || len(res.Findings) != 1 {
		t.Errorf("unexpected verdict: %+v", res)
	}
}

func TestCLIReviewerParsesBareVerdict(t *testing.T) {
	// Fallback: stdout is the verdict JSON directly (not the claude envelope).
	r := NewReviewerWithRunner(fakeReviewRunner(`{"decision":"approve","summary":"looks correct","findings":[]}`))
	res, err := r.Review(context.Background(), &ReviewRequest{Task: "t"})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if res.Decision != "approve" {
		t.Errorf("decision: got %q", res.Decision)
	}
}

func TestCLIReviewerErrorsOnNoVerdict(t *testing.T) {
	r := NewReviewerWithRunner(fakeReviewRunner(`{"result":"I couldn't produce a verdict, sorry."}`))
	_, err := r.Review(context.Background(), &ReviewRequest{Task: "t"})
	if err == nil {
		t.Fatal("want error when output has no JSON verdict")
	}
}

func TestReviewPromptCarriesReasonsAndDiff(t *testing.T) {
	p := reviewPrompt(&ReviewRequest{
		Task:    "fix #144",
		Branch:  "feat-x",
		Reasons: []string{"diff 650 lines exceeds max 400"},
		Diff:    "diff --git a/x b/x",
	})
	for _, want := range []string{"fix #144", "diff 650 lines exceeds max 400", "diff --git a/x b/x", "decision"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestReviewPromptVerifiesInCheckoutWhenWorktreeSet(t *testing.T) {
	with := reviewPrompt(&ReviewRequest{Task: "t", Worktree: "/wt", Diff: "d"})
	if !strings.Contains(with, "inside the change's worktree") || !strings.Contains(with, "do not judge from the diff alone") {
		t.Errorf("worktree prompt should instruct in-checkout verification:\n%s", with)
	}
	without := reviewPrompt(&ReviewRequest{Task: "t", Diff: "d"})
	if strings.Contains(without, "inside the change's worktree") {
		t.Errorf("no-worktree prompt should not claim checkout access")
	}
}

func TestExtractJSONObjectBalances(t *testing.T) {
	in := `noise {"a":{"b":"}"},"c":1} trailing`
	got := extractJSONObject(in)
	want := `{"a":{"b":"}"},"c":1}`
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}
