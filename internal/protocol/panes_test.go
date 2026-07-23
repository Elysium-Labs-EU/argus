package protocol

import "testing"

func TestPaneRegistryRoundTrips(t *testing.T) {
	repoRoot := t.TempDir()
	in := PaneRegistry{Panes: map[string]string{"/repo/.claude/worktrees/feat-x": "w1:p1"}}
	if err := WritePaneRegistry(repoRoot, in); err != nil {
		t.Fatalf("WritePaneRegistry: %v", err)
	}
	got, err := LoadPaneRegistry(repoRoot)
	if err != nil {
		t.Fatalf("LoadPaneRegistry: %v", err)
	}
	if got.Panes["/repo/.claude/worktrees/feat-x"] != "w1:p1" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestLoadPaneRegistryMissingIsEmptyNotError(t *testing.T) {
	got, err := LoadPaneRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("missing registry should not error: %v", err)
	}
	if got.Panes == nil || len(got.Panes) != 0 {
		t.Errorf("want an empty, ready-to-use map, got %+v", got)
	}
}

// TestPaneRegistrySurvivesWorktreeDirectoryBeingGone is the exact scenario
// the registry exists for: the worktree directory it tracks a pane for has
// been deleted, but the registry itself lives at the repo root, not inside
// that directory, so it is still readable afterward.
func TestPaneRegistrySurvivesWorktreeDirectoryBeingGone(t *testing.T) {
	repoRoot := t.TempDir()
	worktree := repoRoot + "/.claude/worktrees/feat-trashed"
	if err := WritePaneRegistry(repoRoot, PaneRegistry{Panes: map[string]string{worktree: "w9:p1"}}); err != nil {
		t.Fatalf("WritePaneRegistry: %v", err)
	}
	// No such directory was ever created here — this reproduces someone
	// having `trash`ed the worktree directly instead of through git/argus.
	got, err := LoadPaneRegistry(repoRoot)
	if err != nil {
		t.Fatalf("LoadPaneRegistry: %v", err)
	}
	if got.Panes[worktree] != "w9:p1" {
		t.Errorf("registry entry should resolve independent of the worktree directory's existence, got %+v", got)
	}
}
