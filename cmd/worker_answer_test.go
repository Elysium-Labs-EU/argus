package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

const workerAnswerTestPaneID = "w1:p1"

// fakeAnswerClient models a worktree whose pane already has a live agent
// (the normal case once a worker has reported blocked): "agent get" always
// reports live, and "agent prompt" either succeeds or returns promptErr (used
// to exercise the no-live-agent and stalled-fallback paths). Every test here
// dispatches into the same single worktree/pane pair (workerAnswerTestPaneID).
func fakeAnswerClient(live bool, promptErr error) herdr.Client {
	return herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "worktree":
			return fmt.Appendf(nil, `{"result":{"root_pane":{"pane_id":%q}}}`, workerAnswerTestPaneID), nil
		case len(args) > 1 && args[0] == "agent" && args[1] == "get":
			if !live {
				return nil, fmt.Errorf("herdr agent get: %w", herdr.ErrAgentNotFound)
			}
			return fmt.Appendf(nil, `{"result":{"agent":{"pane_id":%q,"agent":"claude","agent_status":"working"}}}`, workerAnswerTestPaneID), nil
		case len(args) > 1 && args[0] == "agent" && args[1] == "prompt":
			if promptErr != nil {
				return nil, promptErr
			}
			return []byte(`{"result":{}}`), nil
		default:
			return []byte(`{"result":{}}`), nil
		}
	})
}

func answerLogger() *eventlog.Logger {
	return eventlog.New(nil, "worker-answer", "test-run", nil)
}

func seedBlockedStatus(t *testing.T, wt string, q *protocol.Question) {
	t.Helper()
	seed := protocol.Status{
		Phase:         protocol.PhaseBlocked,
		BlockedReason: "needs a decision",
		Question:      q,
	}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding blocked status: %v", err)
	}
}

func TestRunWorkerAnswerRejectsMissingStatus(t *testing.T) {
	wt := initGitDir(t)
	client := fakeAnswerClient(true, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerAnswer(testCmdCtx, client, answerLogger(), wt, "go ahead", 0, fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error answering a worktree with no status report, got nil")
	}
}

func TestRunWorkerAnswerRejectsNonBlockedWorker(t *testing.T) {
	wt := initGitDir(t)
	seed := protocol.Status{Phase: protocol.PhaseWorking}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	client := fakeAnswerClient(true, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerAnswer(testCmdCtx, client, answerLogger(), wt, "go ahead", 0, fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error answering a non-blocked worker, got nil")
	}
	if !strings.Contains(err.Error(), "not blocked") {
		t.Errorf("error = %q, want it to mention the worker is not blocked", err.Error())
	}
}

func TestRunWorkerAnswerRejectsBothTextAndOption(t *testing.T) {
	wt := initGitDir(t)
	seedBlockedStatus(t, wt, &protocol.Question{Text: "which base?", Options: []string{"wait", "cherry-pick"}})
	client := fakeAnswerClient(true, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerAnswer(testCmdCtx, client, answerLogger(), wt, "cherry-pick", 2, fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error when both TEXT and --option are given, got nil")
	}
}

func TestRunWorkerAnswerRejectsEmptyAnswer(t *testing.T) {
	wt := initGitDir(t)
	seedBlockedStatus(t, wt, nil)
	client := fakeAnswerClient(true, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerAnswer(testCmdCtx, client, answerLogger(), wt, "", 0, fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error with no TEXT and no --option, got nil")
	}
}

func TestRunWorkerAnswerRejectsOptionOutOfRange(t *testing.T) {
	wt := initGitDir(t)
	seedBlockedStatus(t, wt, &protocol.Question{Text: "which base?", Options: []string{"wait", "cherry-pick"}})
	client := fakeAnswerClient(true, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerAnswer(testCmdCtx, client, answerLogger(), wt, "", 3, fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error for an out-of-range --option, got nil")
	}
}

func TestRunWorkerAnswerRejectsOptionWithNoQuestion(t *testing.T) {
	wt := initGitDir(t)
	seedBlockedStatus(t, wt, nil)
	client := fakeAnswerClient(true, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerAnswer(testCmdCtx, client, answerLogger(), wt, "", 1, fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error picking --option with no reported question, got nil")
	}
}

func TestRunWorkerAnswerRecordsFreeTextAndDelivers(t *testing.T) {
	wt := initGitDir(t)
	q := &protocol.Question{Text: "wait and rebase, or cherry-pick now?", Options: []string{"wait", "cherry-pick"}}
	seedBlockedStatus(t, wt, q)
	client := fakeAnswerClient(true, nil)
	testCmdCtx, _ := testCmd()

	now := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	if err := runWorkerAnswer(testCmdCtx, client, answerLogger(), wt, "cherry-pick now", 0, fixedNow(now)); err != nil {
		t.Fatalf("runWorkerAnswer: %v", err)
	}

	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Answer == nil || got.Answer.Text != "cherry-pick now" {
		t.Fatalf("Answer = %+v, want text %q", got.Answer, "cherry-pick now")
	}
	if got.Answer.Option != 0 {
		t.Errorf("Answer.Option = %d, want 0 for free-form text", got.Answer.Option)
	}
	if !got.Answer.AnsweredAt.Equal(now) {
		t.Errorf("Answer.AnsweredAt = %v, want argus's clock %v", got.Answer.AnsweredAt, now)
	}
	if got.Question == nil || got.Question.Text != q.Text {
		t.Errorf("Question = %+v, want it preserved as %+v", got.Question, q)
	}
	if got.Phase != protocol.PhaseBlocked {
		t.Errorf("Phase = %q, want unchanged blocked — the worker itself reports its next phase", got.Phase)
	}
}

func TestRunWorkerAnswerRecordsOptionIndex(t *testing.T) {
	wt := initGitDir(t)
	q := &protocol.Question{Text: "which base?", Options: []string{"wait and rebase", "cherry-pick now"}}
	seedBlockedStatus(t, wt, q)
	client := fakeAnswerClient(true, nil)
	testCmdCtx, _ := testCmd()

	if err := runWorkerAnswer(testCmdCtx, client, answerLogger(), wt, "", 2, fixedNow(time.Now())); err != nil {
		t.Fatalf("runWorkerAnswer: %v", err)
	}

	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Answer == nil || got.Answer.Text != "cherry-pick now" || got.Answer.Option != 2 {
		t.Fatalf("Answer = %+v, want text %q option 2", got.Answer, "cherry-pick now")
	}
}

// TestRunWorkerAnswerNoLiveAgentStillRecords pins the "delivery is best-effort,
// the durable trace is not" contract: a pane whose worker session already
// exited must not lose the recorded answer just because it could not be
// delivered.
func TestRunWorkerAnswerNoLiveAgentStillRecords(t *testing.T) {
	wt := initGitDir(t)
	seedBlockedStatus(t, wt, nil)
	client := fakeAnswerClient(false, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerAnswer(testCmdCtx, client, answerLogger(), wt, "go ahead", 0, fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error when the worker's pane has no live agent, got nil")
	}

	got, loadErr := protocol.Load(protocol.StatusPath(wt))
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if got.Answer == nil || got.Answer.Text != "go ahead" {
		t.Fatalf("Answer = %+v, want it recorded despite the delivery failure", got.Answer)
	}
}

// TestRunWorkerAnswerDeliveryStalledFallsBackToPaneRun mirrors
// TestDispatchIntoPaneAgentPromptStalledFallsBackToPaneRun (rebase_test.go)
// for deliverAnswerToPane: an AgentPrompt herdr reports stalled recovers via
// PaneRun + an explicit PaneSendKeys "enter" instead of failing outright.
func TestRunWorkerAnswerDeliveryStalledFallsBackToPaneRun(t *testing.T) {
	wt := initGitDir(t)
	seedBlockedStatus(t, wt, nil)

	var paneRunText string
	var sawEnterAfterPaneRun bool
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "worktree":
			return fmt.Appendf(nil, `{"result":{"root_pane":{"pane_id":%q}}}`, workerAnswerTestPaneID), nil
		case len(args) > 1 && args[0] == "agent" && args[1] == "get":
			return fmt.Appendf(nil, `{"result":{"agent":{"pane_id":%q,"agent":"claude","agent_status":"done"}}}`, workerAnswerTestPaneID), nil
		case len(args) > 1 && args[0] == "agent" && args[1] == "prompt":
			return nil, fmt.Errorf("herdr agent: exit status 1: %w", herdr.ErrAgentPromptStalled)
		case len(args) > 1 && args[0] == "agent" && args[1] == "wait":
			return fmt.Appendf(nil, `{"result":{"agent":{"pane_id":%q,"agent":"claude","agent_status":"working"}}}`, workerAnswerTestPaneID), nil
		case len(args) > 1 && args[0] == "pane" && args[1] == "run":
			paneRunText = args[3]
			return []byte(`{"result":{}}`), nil
		case len(args) > 1 && args[0] == "pane" && args[1] == "send-keys":
			if paneRunText != "" && args[2] == workerAnswerTestPaneID && len(args) > 3 && args[3] == "enter" {
				sawEnterAfterPaneRun = true
			}
			return []byte(`{"result":{}}`), nil
		default:
			return []byte(`{"result":{}}`), nil
		}
	})
	testCmdCtx, _ := testCmd()

	if err := runWorkerAnswer(testCmdCtx, client, answerLogger(), wt, "go ahead", 0, fixedNow(time.Now())); err != nil {
		t.Fatalf("runWorkerAnswer: want the pane-run fallback to succeed, got %v", err)
	}
	if paneRunText == "" {
		t.Error("want the answer delivered via a pane-run fallback, saw no `pane run` call")
	}
	if !sawEnterAfterPaneRun {
		t.Error("want a `pane send-keys ... enter` call submitting the pane-run text, saw none")
	}
}

// TestRunWorkerAnswerDeliveryNonStalledPromptErrorPropagates confirms a
// genuine (non-stalled) AgentPrompt error is surfaced directly rather than
// falling back to PaneRun, which fallback is only a recovery for herdr's own
// "stalled" signal.
func TestRunWorkerAnswerDeliveryNonStalledPromptErrorPropagates(t *testing.T) {
	wt := initGitDir(t)
	seedBlockedStatus(t, wt, nil)
	client := fakeAnswerClient(true, errors.New("herdr: socket unavailable"))
	testCmdCtx, _ := testCmd()

	err := runWorkerAnswer(testCmdCtx, client, answerLogger(), wt, "go ahead", 0, fixedNow(time.Now()))
	if err == nil || !strings.Contains(err.Error(), "socket unavailable") {
		t.Fatalf("want the AgentPrompt error propagated, got %v", err)
	}
}

func TestNewWorkerAnswerCmdArgValidation(t *testing.T) {
	cmd := newWorkerAnswerCmd()
	cmd.SetArgs([]string{})
	if err := cmd.Args(cmd, []string{}); err == nil {
		t.Error("want an error with no worktree argument, got nil")
	}
	if err := cmd.Args(cmd, []string{"wt", "text", "extra"}); err == nil {
		t.Error("want an error with more than 2 positional args, got nil")
	}
	if f := cmd.Flags().Lookup("option"); f == nil {
		t.Fatal("want an --option flag registered")
	}
}
