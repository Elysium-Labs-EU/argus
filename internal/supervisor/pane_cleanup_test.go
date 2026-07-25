package supervisor

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/herdr"
)

func TestClosePaneAndEmptyWorkspaceClosesSoleWorkspace(t *testing.T) {
	const paneList = `{"result":{"panes":[
{"pane_id":"w1:p1","workspace_id":"w1"}
]}}`
	var calls [][]string
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		if args[0] == "pane" && args[1] == "list" {
			return []byte(paneList), nil
		}
		return []byte(`{"result":{}}`), nil
	})

	if err := ClosePaneAndEmptyWorkspace(context.Background(), client, "w1:p1", false); err != nil {
		t.Fatalf("ClosePaneAndEmptyWorkspace: %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("want pane list, pane close, workspace close (3 calls), got %d: %v", len(calls), calls)
	}
	if calls[1][0] != "pane" || calls[1][1] != "close" || calls[1][2] != "w1:p1" {
		t.Errorf("second call should close the pane, got %v", calls[1])
	}
	if calls[2][0] != "workspace" || calls[2][1] != "close" || calls[2][2] != "w1" {
		t.Errorf("third call should close the now-empty workspace, got %v", calls[2])
	}
}

func TestClosePaneAndEmptyWorkspaceNoOpWhenWorkspaceAlreadyClosed(t *testing.T) {
	// herdr can close a workspace itself the instant its last pane closes;
	// the explicit close issued right after then finds it already gone.
	const paneList = `{"result":{"panes":[
{"pane_id":"w1:p1","workspace_id":"w1"}
]}}`
	var calls [][]string
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		if args[0] == "pane" && args[1] == "list" {
			return []byte(paneList), nil
		}
		if args[0] == "workspace" && args[1] == "close" {
			return nil, fmt.Errorf("herdr workspace close: %w", herdr.ErrWorkspaceNotFound)
		}
		return []byte(`{"result":{}}`), nil
	})

	if err := ClosePaneAndEmptyWorkspace(context.Background(), client, "w1:p1", false); err != nil {
		t.Fatalf("a workspace herdr already closed on its own should not error: %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("want pane list, pane close, workspace close (3 calls) even though the last one 404s, got %d: %v", len(calls), calls)
	}
}

func TestClosePaneAndEmptyWorkspaceLeavesWorkspaceOpenWithSiblings(t *testing.T) {
	const paneList = `{"result":{"panes":[
{"pane_id":"w1:p1","workspace_id":"w1"},
{"pane_id":"w1:p2","workspace_id":"w1"}
]}}`
	var calls [][]string
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		if args[0] == "pane" && args[1] == "list" {
			return []byte(paneList), nil
		}
		return []byte(`{"result":{}}`), nil
	})

	if err := ClosePaneAndEmptyWorkspace(context.Background(), client, "w1:p1", false); err != nil {
		t.Fatalf("ClosePaneAndEmptyWorkspace: %v", err)
	}
	for _, c := range calls {
		if c[0] == "workspace" {
			t.Errorf("workspace with a surviving sibling pane should not be closed, got call %v", c)
		}
	}
}

func TestClosePaneAndEmptyWorkspaceNoOpWhenPaneAlreadyGone(t *testing.T) {
	const paneList = `{"result":{"panes":[]}}`
	var calls [][]string
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return []byte(paneList), nil
	})

	if err := ClosePaneAndEmptyWorkspace(context.Background(), client, "w1:p1", false); err != nil {
		t.Fatalf("a pane herdr no longer knows about should not error: %v", err)
	}
	if len(calls) != 1 {
		t.Errorf("only the initial pane list call should happen, got %v", calls)
	}
}

func TestClosePaneAndEmptyWorkspacePropagatesPaneListError(t *testing.T) {
	sentinel := errors.New("herdr: socket unavailable")
	client := herdr.NewWithRunner(func(_ context.Context, _ ...string) ([]byte, error) {
		return nil, sentinel
	})
	if err := ClosePaneAndEmptyWorkspace(context.Background(), client, "w1:p1", false); !errors.Is(err, sentinel) {
		t.Fatalf("want wrapped runner error, got %v", err)
	}
}

func TestClosePaneAndEmptyWorkspaceLeavesNestedWorkspaceOpenEvenWhenSoleTab(t *testing.T) {
	const paneList = `{"result":{"panes":[
{"pane_id":"w1:p1","workspace_id":"w1"}
]}}`
	var calls [][]string
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		if args[0] == "pane" && args[1] == "list" {
			return []byte(paneList), nil
		}
		return []byte(`{"result":{}}`), nil
	})

	if err := ClosePaneAndEmptyWorkspace(context.Background(), client, "w1:p1", true); err != nil {
		t.Fatalf("ClosePaneAndEmptyWorkspace: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("want pane list, pane close only (2 calls), got %d: %v", len(calls), calls)
	}
	for _, c := range calls {
		if c[0] == "workspace" {
			t.Errorf("a nested worker's shared parent workspace must never be closed, got call %v", c)
		}
	}
}
