package supervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestEnsureDistinctWorktreesRefusesCollision(t *testing.T) {
	// Two workers landing in the same worktree (same branch) is the real hazard.
	shared := []string{
		"/repo/.claude/worktrees/feat-a",
		"/repo/.claude/worktrees/feat-a",
	}
	if err := EnsureDistinctWorktrees(shared); err == nil {
		t.Fatal("want error for two workers sharing a worktree, got nil")
	}

	// Distinct worktrees are fine even if the workers launched from one repo root.
	distinct := []string{
		"/repo/.claude/worktrees/feat-a",
		"/repo/.claude/worktrees/feat-b",
	}
	if err := EnsureDistinctWorktrees(distinct); err != nil {
		t.Fatalf("distinct worktrees should pass, got %v", err)
	}
}

func TestSettingsForConfinesToWorktree(t *testing.T) {
	wt := "/repo/.claude/worktrees/feat-x"
	s := settingsFor(wt)

	wantAllow := "Edit(" + wt + "/**)"
	if !slices.Contains(s.Permissions.Allow, wantAllow) {
		t.Errorf("allow missing %q; got %v", wantAllow, s.Permissions.Allow)
	}
	if !slices.Contains(s.Permissions.Ask, "Bash(git commit:*)") {
		t.Errorf("commit should be gated behind ask; got %v", s.Permissions.Ask)
	}
	for _, want := range []string{"Bash(sudo *)", "Bash(rm -rf *)", "Bash(git reset --hard*)"} {
		if !slices.Contains(s.Permissions.Deny, want) {
			t.Errorf("deny missing %q; got %v", want, s.Permissions.Deny)
		}
	}
}

func TestWriteSettingsWritesConfinedFile(t *testing.T) {
	wt := t.TempDir()
	if err := WriteSettings(wt); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}
	path := filepath.Join(wt, ".claude", "settings.local.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading settings: %v", err)
	}
	var round permissionSettings
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("settings not valid json: %v", err)
	}
	if !strings.Contains(string(data), wt+"/**") {
		t.Errorf("settings should scope edits to the worktree path")
	}
}
