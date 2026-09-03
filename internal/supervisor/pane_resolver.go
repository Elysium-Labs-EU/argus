package supervisor

import (
	"context"

	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

// WorktreePaneResolver resolves a worktree's live pane ID. It first tries
// the persisted pane registry (written by registerSpawnedPane at spawn time),
// which correctly tracks panes even when they were moved into nested tabs
// via PaneMove (see createAndPlaceWorktree). If the worktree is not found in
// the registry, it falls back to asking herdr to open the worktree and
// return its root pane.
//
// This is the fix for argus#699: when worker_placement: tab is configured, a
// worker's pane is moved into a nested tab at spawn time and herdr's
// WorktreeOpen cannot find it later, so every later call to WorktreeOpen
// silently creates a decoy pane instead of reaching the real one. The
// registry records the moved pane's new ID, so looking it up there first
// avoids the decoy.
func WorktreePaneResolver(ctx context.Context, client herdr.Client, repoRoot, worktree string) (herdr.Worktree, error) {
	reg, err := protocol.LoadPaneRegistry(repoRoot)
	if err == nil {
		if paneID, ok := reg.Panes[worktree]; ok {
			return herdr.Worktree{Path: worktree, RootPaneID: paneID}, nil
		}
	}
	return client.WorktreeOpen(ctx, repoRoot, worktree)
}
