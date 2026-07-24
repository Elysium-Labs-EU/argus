package supervisor

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestClaudeCodeAdapterDefaultLauncher(t *testing.T) {
	if got := (claudeCodeAdapter{}).DefaultLauncher(); got != DefaultLauncher {
		t.Errorf("DefaultLauncher() = %q, want %q", got, DefaultLauncher)
	}
}

func TestClaudeCodeAdapterRenderSettings(t *testing.T) {
	wt := "/repo/.claude/worktrees/feat-x"
	path, content, err := (claudeCodeAdapter{}).RenderSettings(wt, []string{"Bash(task *)"})
	if err != nil {
		t.Fatalf("RenderSettings: %v", err)
	}
	if want := filepath.Join(".claude", "settings.local.json"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}

	var round permissionSettings
	if err := json.Unmarshal(content, &round); err != nil {
		t.Fatalf("content not valid json: %v", err)
	}
	if round.Permissions.Allow[len(round.Permissions.Allow)-1] != "Bash(task *)" {
		t.Errorf("extraAllow not applied via RenderSettings; got %v", round.Permissions.Allow)
	}
}

func TestClaudeCodeAdapterPlanEvidenceDelegates(t *testing.T) {
	home := t.TempDir()
	wt := t.TempDir()

	ok, err := (claudeCodeAdapter{}).PlanEvidence(home, wt)
	if err != nil {
		t.Fatalf("PlanEvidence: %v", err)
	}
	if ok {
		t.Error("PlanEvidence should be false when no transcript exists")
	}
}
