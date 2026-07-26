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

// sequenceReviewRunner returns each reply in turn on successive calls, and the
// last reply for any calls beyond the list. It also records how many times it ran.
func sequenceReviewRunner(replies ...string) (reviewRunner, *int) {
	calls := 0
	run := func(_ context.Context, _, _ string, _ ...string) ([]byte, error) {
		i := calls
		calls++
		if i >= len(replies) {
			i = len(replies) - 1
		}
		return []byte(replies[i]), nil
	}
	return run, &calls
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

func TestCLIReviewerReAsksOnceThenParses(t *testing.T) {
	// First reply has unquoted keys (the real #148 failure: 'S' from {Summary:...});
	// the re-ask returns strict JSON. Review must recover without erroring.
	run, calls := sequenceReviewRunner(
		`{decision: approve, Summary: looks fine}`,
		`{"decision":"approve","summary":"looks fine","findings":[]}`,
	)
	r := NewReviewerWithRunner(run)
	res, err := r.Review(context.Background(), &ReviewRequest{Task: "t", Diff: "x"})
	if err != nil {
		t.Fatalf("Review after re-ask: %v", err)
	}
	if res.Decision != "approve" {
		t.Errorf("decision: got %q", res.Decision)
	}
	if *calls != 2 {
		t.Errorf("want exactly one re-ask (2 runs), got %d", *calls)
	}
}

func TestCLIReviewerErrorsAfterFailedReAsk(t *testing.T) {
	// Both the initial reply and the re-ask are unparseable: Review gives up with
	// an error and does not loop forever.
	run, calls := sequenceReviewRunner(
		`{decision: approve}`,
		`still not json`,
	)
	r := NewReviewerWithRunner(run)
	if _, err := r.Review(context.Background(), &ReviewRequest{Task: "t"}); err == nil {
		t.Fatal("want error when re-ask also fails to parse")
	}
	if *calls != 2 {
		t.Errorf("want exactly 2 runs (one re-ask, no more), got %d", *calls)
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

func TestReviewPromptCarriesPriorFindings(t *testing.T) {
	with := reviewPrompt(&ReviewRequest{
		Task:          "t",
		Diff:          "d",
		PriorFindings: []string{"--dry-run mutates lifecycle.json on disk"},
	})
	for _, want := range []string{
		"prior review",
		"--dry-run mutates lifecycle.json on disk",
		"confirmed every",
	} {
		if !strings.Contains(strings.ToLower(with), strings.ToLower(want)) {
			t.Errorf("prompt missing %q:\n%s", want, with)
		}
	}
	without := reviewPrompt(&ReviewRequest{Task: "t", Diff: "d"})
	if strings.Contains(strings.ToLower(without), "prior review") {
		t.Errorf("no-PriorFindings prompt should not mention a prior review")
	}
}

func TestReviewPromptCarriesReviewNote(t *testing.T) {
	with := reviewPrompt(&ReviewRequest{
		Task:       "t",
		Diff:       "d",
		ReviewNote: "Flag any new dependency.",
	})
	for _, want := range []string{"Repo-specific review criteria", "Flag any new dependency."} {
		if !strings.Contains(with, want) {
			t.Errorf("prompt missing %q:\n%s", want, with)
		}
	}
	without := reviewPrompt(&ReviewRequest{Task: "t", Diff: "d"})
	if strings.Contains(without, "Repo-specific review criteria") {
		t.Errorf("no-ReviewNote prompt should not mention repo-specific criteria")
	}
}

func TestNewCLIReviewerAndWithLog(t *testing.T) {
	r := NewCLIReviewer("sonnet")
	if r.model != "sonnet" || r.run == nil {
		t.Fatalf("NewCLIReviewer did not wire model/runner: %+v", r)
	}
	// WithLog returns a copy carrying the logger; the original is unchanged.
	withLog := r.WithLog(nil)
	if withLog.model != "sonnet" {
		t.Errorf("WithLog dropped the model")
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
