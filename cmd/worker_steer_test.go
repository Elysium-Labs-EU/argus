package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

const workerSteerTestPaneID = "w1:p1"

// fakeSteerClient mirrors fakeAnswerClient: a worktree whose pane already has
// a live agent, with "agent get" reporting live/not-live and "agent prompt"
// either succeeding or returning promptErr.
func fakeSteerClient(live bool, promptErr error) herdr.Client {
	return herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "worktree":
			return fmt.Appendf(nil, `{"result":{"root_pane":{"pane_id":%q}}}`, workerSteerTestPaneID), nil
		case len(args) > 1 && args[0] == "agent" && args[1] == "get":
			if !live {
				return nil, fmt.Errorf("herdr agent get: %w", herdr.ErrAgentNotFound)
			}
			return fmt.Appendf(nil, `{"result":{"agent":{"pane_id":%q,"agent":"claude","agent_status":"working"}}}`, workerSteerTestPaneID), nil
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

func steerLogger() *eventlog.Logger {
	return eventlog.New(nil, "worker-steer", "test-run", nil)
}

func seedWorkingStatus(t *testing.T, wt string, steers []protocol.Steer) {
	t.Helper()
	seed := protocol.Status{Phase: protocol.PhaseWorking, Steers: steers}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding working status: %v", err)
	}
}

func TestRunWorkerSteerRejectsMissingStatus(t *testing.T) {
	wt := initGitDir(t)
	client := fakeSteerClient(true, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "double-check the timeout unit", ownerFlags{}, fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error steering a worktree with no status report, got nil")
	}
}

func TestRunWorkerSteerRejectsBlockedWorker(t *testing.T) {
	wt := initGitDir(t)
	seed := protocol.Status{Phase: protocol.PhaseBlocked}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	client := fakeSteerClient(true, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "note", ownerFlags{}, fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error steering a blocked worker, got nil")
	}
	if !strings.Contains(err.Error(), "not steerable") {
		t.Errorf("error = %q, want it to mention the worker is not steerable", err.Error())
	}
	var ue *ui.UserError
	if !errors.As(err, &ue) {
		t.Fatalf("error = %T, want a *ui.UserError carrying a hint", err)
	}
	if !strings.Contains(ue.Hint, "argus worker answer") {
		t.Errorf("hint = %q, want it to point a blocked worker at `argus worker answer`", ue.Hint)
	}
}

// TestRunWorkerSteerAcceptsAwaitingReviewWorker pins the fix itself: a
// worker reporting awaiting_review still has a live pane, so a report-only
// defect can be corrected with a steer instead of costing a full rework
// round (see docs/adr/0009-steer-accepts-awaiting-review.md).
func TestRunWorkerSteerAcceptsAwaitingReviewWorker(t *testing.T) {
	wt := initGitDir(t)
	seed := protocol.Status{Phase: protocol.PhaseAwaitingReview}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	client := fakeSteerClient(true, nil)
	testCmdCtx, _ := testCmd()

	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	if err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "the summary table is missing a column", ownerFlags{}, fixedNow(now)); err != nil {
		t.Fatalf("runWorkerSteer: want an awaiting_review worker steerable, got %v", err)
	}

	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Steers) != 1 || !got.Steers[0].Delivered {
		t.Fatalf("Steers = %+v, want a single delivered note", got.Steers)
	}
	if got.Phase != protocol.PhaseAwaitingReview {
		t.Errorf("Phase = %q, want unchanged awaiting_review — steer never moves phase", got.Phase)
	}
}

func TestRunWorkerSteerRejectsEmptyText(t *testing.T) {
	wt := initGitDir(t)
	seedWorkingStatus(t, wt, nil)
	client := fakeSteerClient(true, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "", ownerFlags{}, fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error with no follow-up text, got nil")
	}
}

func TestRunWorkerSteerRejectsAtCap(t *testing.T) {
	wt := initGitDir(t)
	full := make([]protocol.Steer, protocol.MaxSteersPerWorking)
	for i := range full {
		full[i] = protocol.Steer{Text: fmt.Sprintf("note %d", i), Delivered: true}
	}
	seedWorkingStatus(t, wt, full)
	client := fakeSteerClient(true, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "one more", ownerFlags{}, fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error steering a worker already at its cap, got nil")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d steer messages", protocol.MaxSteersPerWorking)) {
		t.Errorf("error = %q, want it to mention the cap", err.Error())
	}

	got, loadErr := protocol.Load(protocol.StatusPath(wt))
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if len(got.Steers) != protocol.MaxSteersPerWorking {
		t.Errorf("Steers = %d entries, want the rejected call to leave the cap untouched at %d", len(got.Steers), protocol.MaxSteersPerWorking)
	}
}

func TestRunWorkerSteerRecordsAndDelivers(t *testing.T) {
	wt := initGitDir(t)
	seedWorkingStatus(t, wt, nil)
	client := fakeSteerClient(true, nil)
	testCmdCtx, _ := testCmd()

	now := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	if err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "the retry cap is 30s not 30ms", ownerFlags{}, fixedNow(now)); err != nil {
		t.Fatalf("runWorkerSteer: %v", err)
	}

	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Steers) != 1 || got.Steers[0].Text != "the retry cap is 30s not 30ms" {
		t.Fatalf("Steers = %+v, want a single recorded note", got.Steers)
	}
	if !got.Steers[0].Delivered {
		t.Error("Steers[0].Delivered = false, want true once delivery actually succeeds")
	}
	if !got.Steers[0].DeliveredAt.Equal(now) {
		t.Errorf("Steers[0].DeliveredAt = %v, want argus's clock %v", got.Steers[0].DeliveredAt, now)
	}
	if !got.UpdatedAt.Equal(now) {
		t.Errorf("UpdatedAt = %v, want argus's clock %v", got.UpdatedAt, now)
	}
	if got.Phase != protocol.PhaseWorking {
		t.Errorf("Phase = %q, want unchanged working — steer never moves phase", got.Phase)
	}
}

// TestRunWorkerSteerNoLiveAgentStillRecords pins the "delivery is best-effort,
// the durable trace is not" contract, mirroring the same test for answer —
// but unlike the trace, the budget must NOT count this attempt (issue: a
// failed delivery must not consume the per-phase steer budget).
func TestRunWorkerSteerNoLiveAgentStillRecords(t *testing.T) {
	wt := initGitDir(t)
	seedWorkingStatus(t, wt, nil)
	client := fakeSteerClient(false, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "note", ownerFlags{}, fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error when the worker's pane has no live agent, got nil")
	}

	got, loadErr := protocol.Load(protocol.StatusPath(wt))
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if len(got.Steers) != 1 || got.Steers[0].Text != "note" {
		t.Fatalf("Steers = %+v, want it recorded despite the delivery failure", got.Steers)
	}
	if got.Steers[0].Delivered {
		t.Error("Steers[0].Delivered = true, want false — delivery never succeeded, so this must not count against the budget")
	}
}

// TestRunWorkerSteerFailedDeliveryDoesNotConsumeBudget pins the fix itself:
// a worker whose agent is busy mid-turn returns delivery failures repeatedly
// (herdr "agent wait timed out" in production, "no live agent" here — both
// are delivery failures runWorkerSteer treats identically), and none of them
// may exhaust MaxSteersPerWorking before a single steer actually lands.
func TestRunWorkerSteerFailedDeliveryDoesNotConsumeBudget(t *testing.T) {
	wt := initGitDir(t)
	seedWorkingStatus(t, wt, nil)
	client := fakeSteerClient(false, nil)
	testCmdCtx, _ := testCmd()

	for i := range protocol.MaxSteersPerWorking + 1 {
		err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, fmt.Sprintf("note %d", i), ownerFlags{}, fixedNow(time.Now()))
		if err == nil {
			t.Fatalf("attempt %d: want a delivery error (no live agent), got nil", i)
		}
		if strings.Contains(err.Error(), "steer messages this working phase") {
			t.Fatalf("attempt %d: want the budget untouched by failed deliveries, got a cap rejection: %v", i, err)
		}
	}

	got, loadErr := protocol.Load(protocol.StatusPath(wt))
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if len(got.Steers) != protocol.MaxSteersPerWorking+1 {
		t.Fatalf("Steers = %d entries, want every failed attempt still recorded (%d)", len(got.Steers), protocol.MaxSteersPerWorking+1)
	}
	for i, s := range got.Steers {
		if s.Delivered {
			t.Errorf("Steers[%d].Delivered = true, want false since delivery never succeeded", i)
		}
	}
}

// TestRunWorkerSteerCapCountsOnlyDelivered pins that the cap looks at
// Delivered entries only — a mix of undelivered attempts alongside a full set
// of delivered ones must still reject, and by the delivered count, not the
// total trace length.
func TestRunWorkerSteerCapCountsOnlyDelivered(t *testing.T) {
	wt := initGitDir(t)
	seedWorkingStatus(t, wt, []protocol.Steer{
		{Text: "failed 1"},
		{Text: "delivered 1", Delivered: true},
		{Text: "delivered 2", Delivered: true},
		{Text: "delivered 3", Delivered: true},
	})
	client := fakeSteerClient(true, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "one more", ownerFlags{}, fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error steering a worker with 3 delivered steers already, got nil")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d steer messages", protocol.MaxSteersPerWorking)) {
		t.Errorf("error = %q, want it to mention the cap", err.Error())
	}
}

func TestRunWorkerSteerDeliveryStalledFallsBackToPaneRun(t *testing.T) {
	wt := initGitDir(t)
	seedWorkingStatus(t, wt, nil)

	var paneRunText string
	var sawEnterAfterPaneRun bool
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "worktree":
			return fmt.Appendf(nil, `{"result":{"root_pane":{"pane_id":%q}}}`, workerSteerTestPaneID), nil
		case len(args) > 1 && args[0] == "agent" && args[1] == "get":
			return fmt.Appendf(nil, `{"result":{"agent":{"pane_id":%q,"agent":"claude","agent_status":"done"}}}`, workerSteerTestPaneID), nil
		case len(args) > 1 && args[0] == "agent" && args[1] == "prompt":
			return nil, fmt.Errorf("herdr agent: exit status 1: %w", herdr.ErrAgentPromptStalled)
		case len(args) > 1 && args[0] == "agent" && args[1] == "wait":
			return fmt.Appendf(nil, `{"result":{"agent":{"pane_id":%q,"agent":"claude","agent_status":"working"}}}`, workerSteerTestPaneID), nil
		case len(args) > 1 && args[0] == "pane" && args[1] == "run":
			paneRunText = args[3]
			return []byte(`{"result":{}}`), nil
		case len(args) > 1 && args[0] == "pane" && args[1] == "send-keys":
			if paneRunText != "" && args[2] == workerSteerTestPaneID && len(args) > 3 && args[3] == "enter" {
				sawEnterAfterPaneRun = true
			}
			return []byte(`{"result":{}}`), nil
		default:
			return []byte(`{"result":{}}`), nil
		}
	})
	testCmdCtx, _ := testCmd()

	if err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "note", ownerFlags{}, fixedNow(time.Now())); err != nil {
		t.Fatalf("runWorkerSteer: want the pane-run fallback to succeed, got %v", err)
	}
	if paneRunText == "" {
		t.Error("want the steer delivered via a pane-run fallback, saw no `pane run` call")
	}
	if !sawEnterAfterPaneRun {
		t.Error("want a `pane send-keys ... enter` call submitting the pane-run text, saw none")
	}

	got, loadErr := protocol.Load(protocol.StatusPath(wt))
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if len(got.Steers) != 1 || !got.Steers[0].Delivered {
		t.Fatalf("Steers = %+v, want it marked delivered via the pane-run fallback", got.Steers)
	}
}

// TestRunWorkerSteerDeliveryWaitTimeoutFallsBackWhenPaneWasIdle pins the fix
// itself: a genuine-but-slow pickup on an idle/done pane can surface as the
// generic "agent wait timed out" code instead of the dedicated stalled one
// (both just mean "confirmation window closed before anything was
// observed"), and previously only the dedicated code triggered the pane-run
// recovery — so this exact, real delivery was reported as a failure. A pane
// that was idle before the prompt was ever sent has nothing else to
// attribute a later "working" to, so recovery here must succeed and count
// against the budget, exactly like the dedicated-stalled case above.
func TestRunWorkerSteerDeliveryWaitTimeoutFallsBackWhenPaneWasIdle(t *testing.T) {
	wt := initGitDir(t)
	seedWorkingStatus(t, wt, nil)

	var paneRunText string
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "worktree":
			return fmt.Appendf(nil, `{"result":{"root_pane":{"pane_id":%q}}}`, workerSteerTestPaneID), nil
		case len(args) > 1 && args[0] == "agent" && args[1] == "get":
			return fmt.Appendf(nil, `{"result":{"agent":{"pane_id":%q,"agent":"claude","agent_status":"done"}}}`, workerSteerTestPaneID), nil
		case len(args) > 1 && args[0] == "agent" && args[1] == "prompt":
			return nil, fmt.Errorf("herdr agent: exit status 1: %w", herdr.ErrWaitTimeout)
		case len(args) > 1 && args[0] == "agent" && args[1] == "wait":
			return fmt.Appendf(nil, `{"result":{"agent":{"pane_id":%q,"agent":"claude","agent_status":"working"}}}`, workerSteerTestPaneID), nil
		case len(args) > 1 && args[0] == "pane" && args[1] == "run":
			paneRunText = args[3]
			return []byte(`{"result":{}}`), nil
		case len(args) > 1 && args[0] == "pane" && args[1] == "send-keys":
			return []byte(`{"result":{}}`), nil
		default:
			return []byte(`{"result":{}}`), nil
		}
	})
	testCmdCtx, _ := testCmd()

	if err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "note", ownerFlags{}, fixedNow(time.Now())); err != nil {
		t.Fatalf("runWorkerSteer: want the wait-timeout fallback to succeed, got %v", err)
	}
	if paneRunText == "" {
		t.Error("want the steer delivered via a pane-run fallback, saw no `pane run` call")
	}

	got, loadErr := protocol.Load(protocol.StatusPath(wt))
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if len(got.Steers) != 1 || !got.Steers[0].Delivered {
		t.Fatalf("Steers = %+v, want it marked delivered via the pane-run fallback", got.Steers)
	}
}

// TestRunWorkerSteerBusyMidTurnNotRetypedAndNotCounted pins the asymmetry
// MaxSteersPerWorking's doc comment describes: a worker whose agent is busy
// mid-turn (already "working" before the steer was ever sent) drops the note
// rather than queuing it, so this must neither retype into that pane nor
// mark the steer delivered nor let it count against the budget — unlike the
// idle-pane case above, which shares the same herdr.ErrWaitTimeout but must
// recover.
func TestRunWorkerSteerBusyMidTurnNotRetypedAndNotCounted(t *testing.T) {
	wt := initGitDir(t)
	seedWorkingStatus(t, wt, nil)

	var paneRunCalled bool
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "worktree":
			return fmt.Appendf(nil, `{"result":{"root_pane":{"pane_id":%q}}}`, workerSteerTestPaneID), nil
		case len(args) > 1 && args[0] == "agent" && args[1] == "get":
			return fmt.Appendf(nil, `{"result":{"agent":{"pane_id":%q,"agent":"claude","agent_status":"working"}}}`, workerSteerTestPaneID), nil
		case len(args) > 1 && args[0] == "agent" && args[1] == "prompt":
			return nil, fmt.Errorf("herdr agent: exit status 1: %w", herdr.ErrWaitTimeout)
		case len(args) > 1 && args[0] == "pane" && args[1] == "run":
			paneRunCalled = true
			return []byte(`{"result":{}}`), nil
		default:
			return []byte(`{"result":{}}`), nil
		}
	})
	testCmdCtx, _ := testCmd()

	err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "note", ownerFlags{}, fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error when the worker's agent is busy mid-turn, got nil")
	}
	if paneRunCalled {
		t.Error("want no `pane run` retype into a pane already busy mid-turn — that text would be dropped, not queued")
	}

	got, loadErr := protocol.Load(protocol.StatusPath(wt))
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if len(got.Steers) != 1 || got.Steers[0].Delivered {
		t.Fatalf("Steers = %+v, want it recorded but not marked delivered", got.Steers)
	}

	// A run of these must never exhaust the budget: MaxSteersPerWorking only
	// counts delivered notes.
	for i := 1; i < protocol.MaxSteersPerWorking+2; i++ {
		if err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, fmt.Sprintf("note %d", i), ownerFlags{}, fixedNow(time.Now())); err == nil {
			t.Fatalf("attempt %d: want a delivery error (busy mid-turn), got nil", i)
		} else if strings.Contains(err.Error(), "steer messages this working phase") {
			t.Fatalf("attempt %d: want the budget untouched by undelivered busy-mid-turn attempts, got a cap rejection: %v", i, err)
		}
	}
}

func TestRunWorkerSteerDeliveryNonStalledPromptErrorPropagates(t *testing.T) {
	wt := initGitDir(t)
	seedWorkingStatus(t, wt, nil)
	client := fakeSteerClient(true, errors.New("herdr: socket unavailable"))
	testCmdCtx, _ := testCmd()

	err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "note", ownerFlags{}, fixedNow(time.Now()))
	if err == nil || !strings.Contains(err.Error(), "socket unavailable") {
		t.Fatalf("want the AgentPrompt error propagated, got %v", err)
	}

	got, loadErr := protocol.Load(protocol.StatusPath(wt))
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if len(got.Steers) != 1 || got.Steers[0].Delivered {
		t.Fatalf("Steers = %+v, want it recorded but not marked delivered", got.Steers)
	}
}

func TestRunWorkerSteerRejectsEmptyWorktree(t *testing.T) {
	client := fakeSteerClient(true, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerSteer(testCmdCtx, client, steerLogger(), "", "note", ownerFlags{}, fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error with no worktree given, got nil")
	}
	if !strings.Contains(err.Error(), "no worktree given") {
		t.Errorf("error = %q, want it to mention no worktree given", err.Error())
	}
}

// TestRunWorkerSteerRepoRootErrorAfterRecording pins that a steer already
// appended to status.json survives even when the follow-up RepoRoot lookup
// (needed only to deliver it into the pane) fails — the durable trace is not
// rolled back just because delivery couldn't proceed. Using a plain temp dir
// (no git init) is what makes the second, unguarded RepoRoot call fail: unlike
// enforceOwnership's best-effort RepoRoot lookup, this one propagates.
func TestRunWorkerSteerRepoRootErrorAfterRecording(t *testing.T) {
	wt := t.TempDir()
	seedWorkingStatus(t, wt, nil)
	client := fakeSteerClient(true, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "note", ownerFlags{}, fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error resolving repo root outside any git repo, got nil")
	}
	if !strings.Contains(err.Error(), "steer recorded, but resolving repo root") {
		t.Errorf("error = %q, want it to mention the steer was recorded despite the resolve failure", err.Error())
	}

	got, loadErr := protocol.Load(protocol.StatusPath(wt))
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if len(got.Steers) != 1 || got.Steers[0].Text != "note" {
		t.Fatalf("Steers = %+v, want it recorded despite the repo-root failure", got.Steers)
	}
	if got.Steers[0].Delivered {
		t.Error("Steers[0].Delivered = true, want false — delivery never got a chance to run")
	}
}

// TestRunWorkerSteerEmptyRootPaneIDAfterRecording pins the other
// recorded-but-undeliverable case: herdr opens the worktree but reports no
// root pane at all (as opposed to WorktreeOpen erroring outright).
func TestRunWorkerSteerEmptyRootPaneIDAfterRecording(t *testing.T) {
	wt := initGitDir(t)
	seedWorkingStatus(t, wt, nil)
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "worktree" {
			return []byte(`{"result":{"root_pane":{"pane_id":""}}}`), nil
		}
		return []byte(`{"result":{}}`), nil
	})
	testCmdCtx, _ := testCmd()

	err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "note", ownerFlags{}, fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error when herdr opens no root pane, got nil")
	}
	if !strings.Contains(err.Error(), "could not find a pane") {
		t.Errorf("error = %q, want it to mention could not find a pane", err.Error())
	}

	got, loadErr := protocol.Load(protocol.StatusPath(wt))
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if len(got.Steers) != 1 || got.Steers[0].Text != "note" {
		t.Fatalf("Steers = %+v, want it recorded despite the missing pane", got.Steers)
	}
	if got.Steers[0].Delivered {
		t.Error("Steers[0].Delivered = true, want false — could not find a pane to deliver to")
	}
}

// TestNewWorkerSteerCmdRunEEndToEnd drives newWorkerSteerCmd's RunE closure
// itself (flag wiring, openRunLog, herdr.New()) rather than just its
// extracted runWorkerSteer body — an empty worktree arg fails before the real
// herdr.New() client is ever dialed, so this needs no herdr binary on PATH.
func TestNewWorkerSteerCmdRunEEndToEnd(t *testing.T) {
	cmd := newWorkerSteerCmd()
	cmd.SetArgs([]string{"", "some note"})
	var out bytes.Buffer
	cmd.SetOut(&out)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("want an error with an empty worktree argument, got nil")
	}
	if !strings.Contains(err.Error(), "no worktree given") {
		t.Errorf("error = %q, want it to mention no worktree given", err.Error())
	}
}

func TestNewWorkerSteerCmdArgValidation(t *testing.T) {
	cmd := newWorkerSteerCmd()
	if err := cmd.Args(cmd, []string{}); err == nil {
		t.Error("want an error with no arguments, got nil")
	}
	if err := cmd.Args(cmd, []string{"wt"}); err == nil {
		t.Error("want an error with only a worktree argument, got nil")
	}
	if err := cmd.Args(cmd, []string{"wt", "text", "extra"}); err == nil {
		t.Error("want an error with more than 2 positional args, got nil")
	}
	if err := cmd.Args(cmd, []string{"wt", "text"}); err != nil {
		t.Errorf("want exactly 2 positional args accepted, got %v", err)
	}
	if f := cmd.Flags().Lookup("owner"); f == nil {
		t.Error("want an --owner flag registered")
	}
	if f := cmd.Flags().Lookup("force-foreign-owner"); f == nil {
		t.Error("want a --force-foreign-owner flag registered")
	}
	if f := cmd.Flags().Lookup("owner-stale-after"); f == nil {
		t.Error("want an --owner-stale-after flag registered")
	}
}
