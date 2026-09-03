package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

func TestWorktreePaneResolver(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	worktree := filepath.Join(repoRoot, "wt")
	paneID := "w123:p456"

	t.Run("returns pane from registry when present", func(t *testing.T) {
		// A fake client that should NOT be called when registry has the answer
		fakeClient := herdr.NewWithRunner(func(ctx context.Context, args ...string) ([]byte, error) {
			t.Fatal("WorktreePaneResolver should use registry, not call client.WorktreeOpen")
			return nil, nil
		})

		// Write registry with the worktree -> paneID mapping
		reg := protocol.PaneRegistry{
			Panes: map[string]string{worktree: paneID},
		}
		if err := protocol.WritePaneRegistry(repoRoot, reg); err != nil {
			t.Fatalf("WritePaneRegistry() error = %v", err)
		}

		wt, err := WorktreePaneResolver(ctx, fakeClient, repoRoot, worktree)
		if err != nil {
			t.Fatalf("WorktreePaneResolver() error = %v", err)
		}
		if wt.RootPaneID != paneID {
			t.Errorf("WorktreePaneResolver() RootPaneID = %q, want %q", wt.RootPaneID, paneID)
		}
	})

	t.Run("falls back to WorktreeOpen when worktree not in registry", func(t *testing.T) {
		otherWorktree := filepath.Join(repoRoot, "other-wt")
		expectedPaneID := "w999:p999"

		// Write a registry with a different worktree
		reg := protocol.PaneRegistry{
			Panes: map[string]string{worktree: paneID},
		}
		if err := protocol.WritePaneRegistry(repoRoot, reg); err != nil {
			t.Fatalf("WritePaneRegistry() error = %v", err)
		}

		// Fake client that returns a pane ID for the other worktree
		fakeClient := herdr.NewWithRunner(func(ctx context.Context, args ...string) ([]byte, error) {
			return []byte(`{"result":{"root_pane":{"pane_id":"` + expectedPaneID + `"},"worktree":{"path":"` + otherWorktree + `"}}}`), nil
		})

		wt, err := WorktreePaneResolver(ctx, fakeClient, repoRoot, otherWorktree)
		if err != nil {
			t.Fatalf("WorktreePaneResolver() error = %v", err)
		}
		if wt.RootPaneID != expectedPaneID {
			t.Errorf("WorktreePaneResolver() RootPaneID = %q, want %q", wt.RootPaneID, expectedPaneID)
		}
	})

	t.Run("falls back to WorktreeOpen when registry file does not exist", func(t *testing.T) {
		emptyRoot := t.TempDir()
		// No registry file exists

		fakeClient := herdr.NewWithRunner(func(ctx context.Context, args ...string) ([]byte, error) {
			return []byte(`{"result":{"root_pane":{"pane_id":"w000:p000"},"worktree":{"path":"` + emptyRoot + `/wt"}}}`), nil
		})

		wt, err := WorktreePaneResolver(ctx, fakeClient, emptyRoot, filepath.Join(emptyRoot, "wt"))
		if err != nil {
			t.Fatalf("WorktreePaneResolver() error = %v", err)
		}
		if wt.RootPaneID != "w000:p000" {
			t.Errorf("WorktreePaneResolver() RootPaneID = %q, want %q", wt.RootPaneID, "w000:p000")
		}
	})

	t.Run("falls back to WorktreeOpen when registry file is corrupted", func(t *testing.T) {
		corruptRoot := t.TempDir()
		// Write a corrupted registry file
		regPath := protocol.PaneRegistryPath(corruptRoot)
		if err := os.MkdirAll(filepath.Dir(regPath), 0o750); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		if err := os.WriteFile(regPath, []byte("not valid json"), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}

		fakeClient := herdr.NewWithRunner(func(ctx context.Context, args ...string) ([]byte, error) {
			return []byte(`{"result":{"root_pane":{"pane_id":"wCorrupt:p1"},"worktree":{"path":"` + corruptRoot + `/wt"}}}`), nil
		})

		wt, err := WorktreePaneResolver(ctx, fakeClient, corruptRoot, filepath.Join(corruptRoot, "wt"))
		if err != nil {
			t.Fatalf("WorktreePaneResolver() error = %v", err)
		}
		if wt.RootPaneID != "wCorrupt:p1" {
			t.Errorf("WorktreePaneResolver() RootPaneID = %q, want %q", wt.RootPaneID, "wCorrupt:p1")
		}
	})
}
