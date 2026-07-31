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

	err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "double-check the timeout unit", fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error steering a worktree with no status report, got nil")
	}
}

func TestRunWorkerSteerRejectsNonWorkingWorker(t *testing.T) {
	wt := initGitDir(t)
	seed := protocol.Status{Phase: protocol.PhaseBlocked}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	client := fakeSteerClient(true, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "note", fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error steering a non-working worker, got nil")
	}
	if !strings.Contains(err.Error(), "not working") {
		t.Errorf("error = %q, want it to mention the worker is not working", err.Error())
	}
}

func TestRunWorkerSteerRejectsEmptyText(t *testing.T) {
	wt := initGitDir(t)
	seedWorkingStatus(t, wt, nil)
	client := fakeSteerClient(true, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "", fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error with no follow-up text, got nil")
	}
}

func TestRunWorkerSteerRejectsAtCap(t *testing.T) {
	wt := initGitDir(t)
	full := make([]protocol.Steer, protocol.MaxSteersPerWorking)
	for i := range full {
		full[i] = protocol.Steer{Text: fmt.Sprintf("note %d", i)}
	}
	seedWorkingStatus(t, wt, full)
	client := fakeSteerClient(true, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "one more", fixedNow(time.Now()))
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
	if err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "the retry cap is 30s not 30ms", fixedNow(now)); err != nil {
		t.Fatalf("runWorkerSteer: %v", err)
	}

	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Steers) != 1 || got.Steers[0].Text != "the retry cap is 30s not 30ms" {
		t.Fatalf("Steers = %+v, want a single recorded note", got.Steers)
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
// the durable trace is not" contract, mirroring the same test for answer.
func TestRunWorkerSteerNoLiveAgentStillRecords(t *testing.T) {
	wt := initGitDir(t)
	seedWorkingStatus(t, wt, nil)
	client := fakeSteerClient(false, nil)
	testCmdCtx, _ := testCmd()

	err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "note", fixedNow(time.Now()))
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

	if err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "note", fixedNow(time.Now())); err != nil {
		t.Fatalf("runWorkerSteer: want the pane-run fallback to succeed, got %v", err)
	}
	if paneRunText == "" {
		t.Error("want the steer delivered via a pane-run fallback, saw no `pane run` call")
	}
	if !sawEnterAfterPaneRun {
		t.Error("want a `pane send-keys ... enter` call submitting the pane-run text, saw none")
	}
}

func TestRunWorkerSteerDeliveryNonStalledPromptErrorPropagates(t *testing.T) {
	wt := initGitDir(t)
	seedWorkingStatus(t, wt, nil)
	client := fakeSteerClient(true, errors.New("herdr: socket unavailable"))
	testCmdCtx, _ := testCmd()

	err := runWorkerSteer(testCmdCtx, client, steerLogger(), wt, "note", fixedNow(time.Now()))
	if err == nil || !strings.Contains(err.Error(), "socket unavailable") {
		t.Fatalf("want the AgentPrompt error propagated, got %v", err)
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
}
