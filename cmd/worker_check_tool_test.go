package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
)

func TestEvaluateToolGate(t *testing.T) {
	if _, blocked := evaluateToolGate("git commit -m foo", protocol.PhasePlanning, protocol.DeniedInPhase(protocol.PhasePlanning)); !blocked {
		t.Error("evaluateToolGate(git commit, planning) = not blocked, want blocked")
	}
	if _, blocked := evaluateToolGate("git push origin HEAD", protocol.PhasePlanning, protocol.DeniedInPhase(protocol.PhasePlanning)); !blocked {
		t.Error("evaluateToolGate(git push, planning) = not blocked, want blocked")
	}
	if _, blocked := evaluateToolGate("git status", protocol.PhasePlanning, protocol.DeniedInPhase(protocol.PhasePlanning)); blocked {
		t.Error("evaluateToolGate(git status, planning) = blocked, want not blocked")
	}
	if _, blocked := evaluateToolGate("git commit -m foo", protocol.PhaseWorking, protocol.DeniedInPhase(protocol.PhaseWorking)); blocked {
		t.Error("evaluateToolGate(git commit, working) = blocked, want not blocked (ask-gated, not denied)")
	}
	if _, blocked := evaluateToolGate("npm publish", protocol.PhaseWorking, []string{"npm publish"}); !blocked {
		t.Error("evaluateToolGate(npm publish, working, configured deny) = not blocked, want blocked")
	}
	for _, p := range protocol.ConfigurablePhases {
		reason, blocked := evaluateToolGate("argus ship --force", p, protocol.DeniedInPhase(p))
		if !blocked {
			t.Errorf("evaluateToolGate(argus ship, %q) = not blocked, want blocked (AlwaysDeniedCommands)", p)
		}
		if !strings.Contains(reason, "supervising session") {
			t.Errorf("evaluateToolGate(argus ship, %q) reason = %q, want it to explain the always-denied case", p, reason)
		}
	}
}

func TestRunWorkerCheckTool(t *testing.T) {
	origExit := osExit
	t.Cleanup(func() { osExit = origExit })
	var exitCode int
	osExit = func(code int) { exitCode = code }

	newWorktree := func(t *testing.T, phase protocol.Phase) string {
		t.Helper()
		wt := t.TempDir()
		if phase == "" {
			return wt
		}
		if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{Phase: phase}); err != nil {
			t.Fatalf("writing status: %v", err)
		}
		return wt
	}

	t.Run("blocks git commit during planning", func(t *testing.T) {
		exitCode = 0
		wt := newWorktree(t, protocol.PhasePlanning)
		stdin := strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":"git commit -m x"}}`, wt))
		var stderr bytes.Buffer
		if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
			t.Fatalf("runWorkerCheckTool: %v", err)
		}
		if exitCode != 2 {
			t.Errorf("exit code = %d, want 2", exitCode)
		}
		if stderr.Len() == 0 {
			t.Error("expected a block reason on stderr, got none")
		}
	})

	t.Run("passes during working", func(t *testing.T) {
		exitCode = 0
		wt := newWorktree(t, protocol.PhaseWorking)
		stdin := strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":"git commit -m x"}}`, wt))
		var stderr bytes.Buffer
		if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
			t.Fatalf("runWorkerCheckTool: %v", err)
		}
		if exitCode != 0 {
			t.Errorf("exit code = %d, want 0", exitCode)
		}
	})

	t.Run("passes when no status.json yet", func(t *testing.T) {
		exitCode = 0
		wt := newWorktree(t, "")
		stdin := strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":"git commit -m x"}}`, wt))
		var stderr bytes.Buffer
		if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
			t.Fatalf("runWorkerCheckTool: %v", err)
		}
		if exitCode != 0 {
			t.Errorf("exit code = %d, want 0", exitCode)
		}
	})

	t.Run("passes on malformed stdin", func(t *testing.T) {
		exitCode = 0
		stdin := strings.NewReader("not json")
		var stderr bytes.Buffer
		if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
			t.Fatalf("runWorkerCheckTool: %v", err)
		}
		if exitCode != 0 {
			t.Errorf("exit code = %d, want 0", exitCode)
		}
	})

	t.Run("blocks argus ship regardless of phase", func(t *testing.T) {
		exitCode = 0
		wt := newWorktree(t, protocol.PhaseWorking)
		stdin := strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":"argus ship --force"}}`, wt))
		var stderr bytes.Buffer
		if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
			t.Fatalf("runWorkerCheckTool: %v", err)
		}
		if exitCode != 2 {
			t.Errorf("exit code = %d, want 2 — a worker must never invoke argus's own supervisor commands on itself", exitCode)
		}
	})

	t.Run("passes on non-matching command during planning", func(t *testing.T) {
		exitCode = 0
		wt := newWorktree(t, protocol.PhasePlanning)
		stdin := strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":"git status"}}`, wt))
		var stderr bytes.Buffer
		if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
			t.Fatalf("runWorkerCheckTool: %v", err)
		}
		if exitCode != 0 {
			t.Errorf("exit code = %d, want 0", exitCode)
		}
	})
}

// TestRunWorkerCheckToolProjectConfig exercises project-tier deny resolution
// end to end: a real git repo (RepoRoot needs one to resolve) with a
// .argus/config.yml, proving both that a repo's own configured deny fires
// and that a repo trying to skip the planning floor cannot unblock it.
func TestRunWorkerCheckToolProjectConfig(t *testing.T) {
	origExit := osExit
	t.Cleanup(func() { osExit = origExit })
	var exitCode int
	osExit = func(code int) { exitCode = code }

	repo := t.TempDir()
	initGitDirAt(t, repo)

	writeConfig := func(t *testing.T, cfg *repoconfig.Config) {
		t.Helper()
		if err := repoconfig.Save(repoconfig.Path(repo), cfg); err != nil {
			t.Fatalf("Save config: %v", err)
		}
	}
	writeStatus := func(t *testing.T, phase protocol.Phase) {
		t.Helper()
		if err := protocol.Write(protocol.StatusPath(repo), &protocol.Status{Phase: phase}); err != nil {
			t.Fatalf("writing status: %v", err)
		}
	}

	t.Run("project deny blocks a configured command", func(t *testing.T) {
		exitCode = 0
		writeConfig(t, &repoconfig.Config{
			Phases: protocol.PhaseConfig{protocol.PhaseWorking: {Deny: []string{"npm publish"}}},
		})
		writeStatus(t, protocol.PhaseWorking)
		stdin := strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":"npm publish"}}`, repo))
		var stderr bytes.Buffer
		if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
			t.Fatalf("runWorkerCheckTool: %v", err)
		}
		if exitCode != 2 {
			t.Errorf("exit code = %d, want 2", exitCode)
		}
	})

	t.Run("skip on the floor phase does not unblock the floor", func(t *testing.T) {
		exitCode = 0
		writeConfig(t, &repoconfig.Config{
			Phases: protocol.PhaseConfig{protocol.PhasePlanning: {Skip: true}},
		})
		writeStatus(t, protocol.PhasePlanning)
		stdin := strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":"git commit -m x"}}`, repo))
		var stderr bytes.Buffer
		if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
			t.Fatalf("runWorkerCheckTool: %v", err)
		}
		if exitCode != 2 {
			t.Errorf("exit code = %d, want 2 — skip must never unblock the hardcoded floor", exitCode)
		}
	})

	t.Run("no config file still resolves the repo and applies only the floor", func(t *testing.T) {
		exitCode = 0
		_ = os.Remove(repoconfig.Path(repo))
		writeStatus(t, protocol.PhaseWorking)
		stdin := strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":"npm publish"}}`, repo))
		var stderr bytes.Buffer
		if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
			t.Fatalf("runWorkerCheckTool: %v", err)
		}
		if exitCode != 0 {
			t.Errorf("exit code = %d, want 0 (no config, npm publish isn't in the floor)", exitCode)
		}
	})
}
