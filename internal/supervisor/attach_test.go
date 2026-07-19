package supervisor

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

// noPanes is a herdr client whose pane list is empty, standing in for the real
// herdr the report enriches from — enough that renderReport runs without a live
// multiplexer.
func noPanes() herdr.Client {
	return herdr.NewWithRunner(func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte(`{"result":{"panes":[]}}`), nil
	})
}

// TestAttachSupervisesExistingWorker proves --attach's core: given a worktree
// whose worker has already reached a terminal phase, Attach watches its typed
// status, measures the real diff, gates, and reports — without spawning anything
// (no herdr Client is set) and without reading scrollback.
func TestAttachSupervisesExistingWorker(t *testing.T) {
	wt := gitWorktreeWithDiff(t) // real git checkout with an uncommitted change vs HEAD
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{
		Task:     "attached-worker",
		Phase:    protocol.PhaseAwaitingReview,
		DiffStat: protocol.DiffStat{Files: 1, Insertions: 2},
		Tests:    []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultPass}},
	}); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	var buf bytes.Buffer
	policy := DefaultReviewPolicy()
	cfg := &Config{
		Out:      &buf,
		Now:      time.Now,
		Client:   noPanes(),
		Base:     "HEAD",
		Home:     t.TempDir(),
		Interval: 2 * time.Millisecond,
		Timeout:  time.Second,
		Policy:   &policy,
	}
	workers := []Worker{{Task: "attached-worker", Branch: "feat", Worktree: wt}}

	if err := Attach(context.Background(), cfg, workers); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !strings.Contains(buf.String(), "supervise report") {
		t.Errorf("expected a report:\n%s", buf.String())
	}
	if _, found, _ := protocol.LoadApproval(wt); !found {
		t.Error("Attach did not record a verdict for the attached worker")
	}
}

// TestAttachTimesOutOnNonTerminalWorker: a worker that never reaches a terminal
// phase must not hang Attach forever — the per-worker deadline returns, and with
// no readable terminal status the worker is skipped by the gate (no verdict), not
// falsely approved.
func TestAttachTimesOutOnNonTerminalWorker(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{
		Task:  "slow",
		Phase: protocol.PhaseWorking, // non-terminal
	}); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	var buf bytes.Buffer
	policy := DefaultReviewPolicy()
	cfg := &Config{
		Out:      &buf,
		Now:      time.Now,
		Client:   noPanes(),
		Base:     "HEAD",
		Home:     t.TempDir(),
		Interval: 2 * time.Millisecond,
		Timeout:  30 * time.Millisecond,
		Policy:   &policy,
	}
	done := make(chan error, 1)
	go func() {
		done <- Attach(context.Background(), cfg, []Worker{{Task: "slow", Branch: "feat", Worktree: wt}})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Attach did not return after the worker's deadline passed")
	}
	// A worker stuck non-terminal was seen (hasFile) but is still gated normally;
	// it must not have been auto-approved on an unfinished phase.
	if a, found, _ := protocol.LoadApproval(wt); found && a.Approved {
		t.Errorf("a non-terminal worker must not be approved: %+v", a)
	}
}
