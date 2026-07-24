package supervisor

import (
	"context"
	"fmt"

	"github.com/Elysium-Labs-EU/argus/internal/herdr"
)

// ClosePaneAndEmptyWorkspace closes paneID and, when it was the only pane left
// in its herdr workspace, closes that workspace too — the cleanup mirror of
// how a worker's worktree and pane are spawned together (see prepareWorktree).
// A pane herdr no longer recognizes (already closed by hand, or herdr
// restarted since) is treated as nothing left to do rather than an error.
//
// nested must be true when the pane was opened as a tab in a shared parent
// workspace (see herdr.WorktreeSpec.Workspace, protocol.PaneRegistry.Nested)
// rather than a fresh top-level workspace argus owns outright — in that case
// the workspace is never closed, even if this pane was the last one argus
// itself put there, since the parent session that owns it may still be live
// in another tab.
func ClosePaneAndEmptyWorkspace(ctx context.Context, client herdr.Client, paneID string, nested bool) error {
	panes, err := client.PaneList(ctx)
	if err != nil {
		return fmt.Errorf("listing panes: %w", err)
	}

	var workspaceID string
	found := false
	sameWorkspace := 0
	for i := range panes {
		if panes[i].PaneID == paneID {
			workspaceID = panes[i].WorkspaceID
			found = true
		}
	}
	if !found {
		return nil
	}
	for i := range panes {
		if panes[i].WorkspaceID == workspaceID {
			sameWorkspace++
		}
	}

	if err := client.PaneClose(ctx, paneID); err != nil {
		return fmt.Errorf("closing pane %s: %w", paneID, err)
	}
	if !nested && sameWorkspace == 1 {
		if err := client.WorkspaceClose(ctx, workspaceID); err != nil {
			return fmt.Errorf("closing workspace %s: %w", workspaceID, err)
		}
	}
	return nil
}
