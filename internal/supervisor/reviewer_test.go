package supervisor

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/ui"
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
	r := NewCLIReviewer("sonnet", "high")
	if r.model != "sonnet" || r.effort != "high" || r.run == nil {
		t.Fatalf("NewCLIReviewer did not wire model/effort/runner: %+v", r)
	}
	// WithLog returns a copy carrying the logger; the original is unchanged.
	withLog := r.WithLog(nil)
	if withLog.model != "sonnet" || withLog.effort != "high" {
		t.Errorf("WithLog dropped the model/effort")
	}
}

func TestReviewAppendsEffortArgWhenSet(t *testing.T) {
	var gotArgs []string
	run := func(_ context.Context, _, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"decision":"approve","summary":"ok","findings":[]}`), nil
	}
	r := CLIReviewer{run: run, model: "sonnet", effort: "xhigh"}
	if _, err := r.Review(context.Background(), &ReviewRequest{Task: "t", Diff: "x"}); err != nil {
		t.Fatalf("Review: %v", err)
	}
	want := []string{"-p", "--output-format", "json", "--allowedTools", "Read,Grep,Glob", "--model", "sonnet", "--effort", "xhigh"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", gotArgs, want)
	}
}

func TestReviewLeavesArgsUnchangedWhenEffortUnset(t *testing.T) {
	var gotArgs []string
	run := func(_ context.Context, _, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"decision":"approve","summary":"ok","findings":[]}`), nil
	}
	r := CLIReviewer{run: run}
	if _, err := r.Review(context.Background(), &ReviewRequest{Task: "t", Diff: "x"}); err != nil {
		t.Fatalf("Review: %v", err)
	}
	want := []string{"-p", "--output-format", "json", "--allowedTools", "Read,Grep,Glob"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v (unset effort must not change today's argv)", gotArgs, want)
	}
}

func TestExtractJSONObjectsBalances(t *testing.T) {
	in := `noise {"a":{"b":"}"},"c":1} trailing`
	got := extractJSONObjects(in)
	want := []string{`{"a":{"b":"}"},"c":1}`}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestExtractJSONObjectsFindsEachTopLevelObject(t *testing.T) {
	in := `first {"a":1} middle {"b":2} last`
	got := extractJSONObjects(in)
	want := []string{`{"a":1}`, `{"b":2}`}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestParseReviewOutputUsesLastObjectWithDecision(t *testing.T) {
	out := []byte(`I first considered {"decision":"approve","summary":"lgtm"} but no.
Final verdict: {"decision":"request-changes","summary":"missing nil check","findings":["x"]}`)
	res, err := parseReviewOutput(out)
	if err != nil {
		t.Fatalf("parseReviewOutput: %v", err)
	}
	if res.Decision != "request-changes" {
		t.Errorf("Decision = %q, want %q (preamble verdict must not win over the final one)", res.Decision, "request-changes")
	}
	if res.Summary != "missing nil check" {
		t.Errorf("Summary = %q, want %q", res.Summary, "missing nil check")
	}
}

func TestParseReviewOutputSingleObject(t *testing.T) {
	out := []byte(`{"decision":"approve","summary":"ok","findings":[]}`)
	res, err := parseReviewOutput(out)
	if err != nil {
		t.Fatalf("parseReviewOutput: %v", err)
	}
	if res.Decision != "approve" {
		t.Errorf("Decision = %q, want %q", res.Decision, "approve")
	}
}

func TestParseReviewOutputNoObjectErrors(t *testing.T) {
	out := []byte(`I could not form a verdict.`)
	if _, err := parseReviewOutput(out); err == nil {
		t.Fatal("want error for output with no JSON object")
	}
}

func TestParseReviewOutputEscapedBracesAndQuotesInLastObject(t *testing.T) {
	out := []byte(`{"decision":"approve","summary":"stray"} then reconsidered:
{"decision":"request-changes","summary":"has a brace \"}\" and a quote \\\" in a string","findings":["uses literal { and } inside strings"]}`)
	res, err := parseReviewOutput(out)
	if err != nil {
		t.Fatalf("parseReviewOutput: %v", err)
	}
	if res.Decision != "request-changes" {
		t.Errorf("Decision = %q, want %q", res.Decision, "request-changes")
	}
	if res.Findings[0] != "uses literal { and } inside strings" {
		t.Errorf("Findings = %v, escaped braces inside the string broke balancing", res.Findings)
	}
}

func TestParseReviewOutputRejectsUnrecognizedDecision(t *testing.T) {
	out := []byte(`{"decision":"looks-good-to-me","summary":"ok","findings":[]}`)
	if _, err := parseReviewOutput(out); err == nil {
		t.Fatal("want error for a decision outside {approve, request-changes, needs-human}")
	}
}

// TestDiffForDistinguishesBadWorktreeFromBadBase is the regression test for
// issue #393's `review`-specific symptom: a bad --worktree and a bad --base
// used to both collapse into the identical, undiagnosable "exit status 128"
// (DiffFor discarded git's stderr entirely). Routed through the shared
// git() helper, the two now produce distinct messages naming the actual bad
// input.
func TestDiffForDistinguishesBadWorktreeFromBadBase(t *testing.T) {
	worktree, base := initGitRepo(t)

	t.Run("bad worktree", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "does-not-exist")
		_, err := DiffFor(context.Background(), bad, base)
		if err == nil {
			t.Fatal("want an error for a nonexistent worktree path")
		}
		var uerr *ui.UserError
		if !errors.As(err, &uerr) {
			t.Fatalf("want *ui.UserError, got %T: %v", err, err)
		}
		if !strings.Contains(uerr.Error(), bad) {
			t.Errorf("error %q does not name the bad worktree path %q", uerr.Error(), bad)
		}
	})

	t.Run("bad base", func(t *testing.T) {
		_, err := DiffFor(context.Background(), worktree, "nonexistent-base-ref")
		if err == nil {
			t.Fatal("want an error for a nonexistent base ref")
		}
		var uerr *ui.UserError
		if !errors.As(err, &uerr) {
			t.Fatalf("want *ui.UserError, got %T: %v", err, err)
		}
		if !strings.Contains(uerr.Error(), "nonexistent-base-ref") {
			t.Errorf("error %q does not name the bad base ref", uerr.Error())
		}
	})
}
