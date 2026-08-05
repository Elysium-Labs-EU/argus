package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
)

// noAllow is passed to evaluateToolGate wherever a test only cares about the
// deny-floor branch — it fires before the allow check ever runs, so an empty
// allow set can't accidentally mask a deny bug as an allow-scope block
// instead.
var noAllow []string

func TestEvaluateToolGate(t *testing.T) {
	floorAllow := supervisor.ResolvedAllowForPhase(protocol.PhasePlanning, nil, nil, nil)

	if _, blocked := evaluateToolGate("git commit -m foo", protocol.PhasePlanning, protocol.DeniedInPhase(protocol.PhasePlanning), noAllow); !blocked {
		t.Error("evaluateToolGate(git commit, planning) = not blocked, want blocked")
	}
	if _, blocked := evaluateToolGate("git push origin HEAD", protocol.PhasePlanning, protocol.DeniedInPhase(protocol.PhasePlanning), noAllow); !blocked {
		t.Error("evaluateToolGate(git push, planning) = not blocked, want blocked")
	}
	if _, blocked := evaluateToolGate("git status", protocol.PhasePlanning, protocol.DeniedInPhase(protocol.PhasePlanning), floorAllow); blocked {
		t.Error("evaluateToolGate(git status, planning) = blocked, want not blocked (structural floor)")
	}
	// git commit is denied in every phase now, not just planning — a worker
	// never commits at all; argus ship does that once a verdict exists.
	commitReason, blocked := evaluateToolGate("git commit -m foo", protocol.PhaseWorking, protocol.DeniedInPhase(protocol.PhaseWorking), noAllow)
	if !blocked {
		t.Error("evaluateToolGate(git commit, working) = not blocked, want blocked (deny floor, every phase)")
	}
	if !strings.Contains(commitReason, "every phase") || !strings.Contains(commitReason, "commit") {
		t.Errorf("commit reason = %q, want it to explain the every-phase commit/push case", commitReason)
	}
	// A repo-configured phases.<name>.deny entry unrelated to commit/push,
	// scoped to one phase, must get a message naming that — not the
	// commit/push-specific "every phase" wording, which is both false (this
	// is scoped to one phase) and misleading (this has nothing to do with
	// git). Regression guard: an earlier round collapsed all three deny
	// families into the commit/push message.
	reason, blocked := evaluateToolGate("npm publish", protocol.PhaseWorking, []string{"npm publish"}, noAllow)
	if !blocked {
		t.Error("evaluateToolGate(npm publish, working, configured deny) = not blocked, want blocked")
	}
	if strings.Contains(reason, "commit") || strings.Contains(reason, "push") || strings.Contains(reason, "every phase") {
		t.Errorf("reason = %q, want it to NOT claim this is about commit/push or every phase (it's a phase-scoped, unrelated configured deny)", reason)
	}
	if want := `argus: "npm publish" is denied during phase "working"`; reason != want {
		t.Errorf("reason = %q, want %q", reason, want)
	}
	for _, p := range protocol.ConfigurablePhases {
		reason, blocked := evaluateToolGate("argus ship --force", p, protocol.DeniedInPhase(p), noAllow)
		if !blocked {
			t.Errorf("evaluateToolGate(argus ship, %q) = not blocked, want blocked (AlwaysDeniedCommands)", p)
		}
		if !strings.Contains(reason, "supervising session") {
			t.Errorf("evaluateToolGate(argus ship, %q) reason = %q, want it to explain the always-denied case", p, reason)
		}
	}
}

// TestEvaluateToolGate_AllowScoping is the allow-side counterpart to the
// deny-floor tests above: a command that clears deny but isn't in the
// resolved allow set for the current phase must still be blocked, with a
// reason distinct from a deny-floor block, and it must pass once it is
// actually in that set.
func TestEvaluateToolGate_AllowScoping(t *testing.T) {
	denied := protocol.DeniedInPhase(protocol.PhaseWorking)

	reason, blocked := evaluateToolGate("go test ./...", protocol.PhaseWorking, denied, noAllow)
	if !blocked {
		t.Error("evaluateToolGate(go test, working, empty allow) = not blocked, want blocked")
	}
	if !strings.Contains(reason, "not in the resolved allow set") {
		t.Errorf("reason = %q, want it to name the allow-scope block", reason)
	}

	project := protocol.PhaseConfig{protocol.PhaseWorking: {Allow: []string{"Bash(go test*)"}}}
	workingAllow := supervisor.ResolvedAllowForPhase(protocol.PhaseWorking, project, nil, nil)
	if _, blocked := evaluateToolGate("go test ./...", protocol.PhaseWorking, denied, workingAllow); blocked {
		t.Error("evaluateToolGate(go test, working, phases.working.allow) = blocked, want not blocked")
	}

	// Resolving for a *different* phase against the same project config must
	// not carry the working-only entry along — the caller (runWorkerCheckTool)
	// is what keeps a live check phase-scoped, by always resolving allow
	// against the worker's *current* phase, never a stale or unrelated one.
	planningAllow := supervisor.ResolvedAllowForPhase(protocol.PhasePlanning, project, nil, nil)
	if _, blocked := evaluateToolGate("go test ./...", protocol.PhasePlanning, protocol.DeniedInPhase(protocol.PhasePlanning), planningAllow); !blocked {
		t.Error("evaluateToolGate(go test, planning, phases.working.allow only) = not blocked, want blocked (must not leak across phases)")
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

	t.Run("blocks git commit during working too — deny floor, every phase", func(t *testing.T) {
		exitCode = 0
		wt := newWorktree(t, protocol.PhaseWorking)
		stdin := strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":"git commit -m x"}}`, wt))
		var stderr bytes.Buffer
		if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
			t.Fatalf("runWorkerCheckTool: %v", err)
		}
		if exitCode != 2 {
			t.Errorf("exit code = %d, want 2 — git commit is denied in every phase, not just planning", exitCode)
		}
	})

	t.Run("blocks an unallowed command outside the resolved allow set", func(t *testing.T) {
		exitCode = 0
		wt := newWorktree(t, protocol.PhaseWorking)
		stdin := strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":"npm publish"}}`, wt))
		var stderr bytes.Buffer
		if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
			t.Fatalf("runWorkerCheckTool: %v", err)
		}
		if exitCode != 2 {
			t.Errorf("exit code = %d, want 2 — no config, no floor entry, dontAsk default-denies", exitCode)
		}
		if !strings.Contains(stderr.String(), "not in the resolved allow set") {
			t.Errorf("stderr = %q, want it to name the allow-scope block", stderr.String())
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

	t.Run("passes on empty command", func(t *testing.T) {
		exitCode = 0
		wt := newWorktree(t, protocol.PhaseWorking)
		stdin := strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":""}}`, wt))
		var stderr bytes.Buffer
		if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
			t.Fatalf("runWorkerCheckTool: %v", err)
		}
		if exitCode != 0 {
			t.Errorf("exit code = %d, want 0 — nothing to enforce with no command", exitCode)
		}
	})

	t.Run("passes on empty cwd", func(t *testing.T) {
		exitCode = 0
		stdin := strings.NewReader(`{"cwd":"","tool_input":{"command":"git commit -m x"}}`)
		var stderr bytes.Buffer
		if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
			t.Fatalf("runWorkerCheckTool: %v", err)
		}
		if exitCode != 0 {
			t.Errorf("exit code = %d, want 0 — nothing to resolve a worktree against", exitCode)
		}
	})
}

func TestLoadProjectPolicy(t *testing.T) {
	t.Run("not a git repo fails open to zero values", func(t *testing.T) {
		phases, allow := loadProjectPolicy(context.Background(), t.TempDir())
		if phases != nil || allow != nil {
			t.Errorf("loadProjectPolicy(non-git dir) = (%v, %v), want (nil, nil)", phases, allow)
		}
	})

	t.Run("malformed config.yml fails open to zero values", func(t *testing.T) {
		repo := t.TempDir()
		initGitDirAt(t, repo)
		if err := os.MkdirAll(filepath.Dir(repoconfig.Path(repo)), 0o755); err != nil {
			t.Fatalf("mkdir .argus: %v", err)
		}
		if err := os.WriteFile(repoconfig.Path(repo), []byte("not: [valid\nallow"), 0o600); err != nil {
			t.Fatalf("seeding malformed config: %v", err)
		}
		phases, allow := loadProjectPolicy(context.Background(), repo)
		if phases != nil || allow != nil {
			t.Errorf("loadProjectPolicy(malformed config) = (%v, %v), want (nil, nil)", phases, allow)
		}
	})

	t.Run("valid config resolves phases and allow", func(t *testing.T) {
		repo := t.TempDir()
		initGitDirAt(t, repo)
		if err := repoconfig.Save(repoconfig.Path(repo), &repoconfig.Config{
			Allow:  []string{"Bash(make *)"},
			Phases: protocol.PhaseConfig{protocol.PhaseWorking: {Allow: []string{"Bash(go test*)"}}},
		}); err != nil {
			t.Fatalf("Save config: %v", err)
		}
		phases, allow := loadProjectPolicy(context.Background(), repo)
		if len(allow) != 1 || allow[0] != "Bash(make *)" {
			t.Errorf("allow = %v, want the configured top-level allow", allow)
		}
		if got := phases[protocol.PhaseWorking].Allow; len(got) != 1 || got[0] != "Bash(go test*)" {
			t.Errorf("phases.working.allow = %v, want the configured entry", got)
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
		// A repo-configured deny unrelated to commit/push must not be
		// misdescribed as the every-phase git commit/push case.
		if strings.Contains(stderr.String(), "commit") || strings.Contains(stderr.String(), "push") || strings.Contains(stderr.String(), "every phase") {
			t.Errorf("stderr = %q, want it to NOT claim this is about commit/push or every phase", stderr.String())
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

	t.Run("no config file degrades to structural-floor-only, not wide open", func(t *testing.T) {
		exitCode = 0
		_ = os.Remove(repoconfig.Path(repo))
		writeStatus(t, protocol.PhaseWorking)
		stdin := strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":"npm publish"}}`, repo))
		var stderr bytes.Buffer
		if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
			t.Fatalf("runWorkerCheckTool: %v", err)
		}
		if exitCode != 2 {
			t.Errorf("exit code = %d, want 2 — no config means structural-floor-only, npm publish was never granted", exitCode)
		}
	})

	t.Run("no config file still allows structural-floor commands", func(t *testing.T) {
		exitCode = 0
		writeStatus(t, protocol.PhaseWorking)
		stdin := strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":"git status"}}`, repo))
		var stderr bytes.Buffer
		if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
			t.Fatalf("runWorkerCheckTool: %v", err)
		}
		if exitCode != 0 {
			t.Errorf("exit code = %d, want 0 — read-only git is the structural floor, present with or without config", exitCode)
		}
	})

	t.Run("project allow grants a materialized toolchain command in its own phase only", func(t *testing.T) {
		exitCode = 0
		writeConfig(t, &repoconfig.Config{
			Phases: protocol.PhaseConfig{protocol.PhaseWorking: {Allow: []string{"Bash(go test*)"}}},
		})

		writeStatus(t, protocol.PhaseWorking)
		stdin := strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":"go test ./..."}}`, repo))
		var stderr bytes.Buffer
		if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
			t.Fatalf("runWorkerCheckTool: %v", err)
		}
		if exitCode != 0 {
			t.Errorf("exit code = %d, want 0 — go test is in phases.working.allow", exitCode)
		}

		exitCode = 0
		writeStatus(t, protocol.PhasePlanning)
		stdin = strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":"go test ./..."}}`, repo))
		stderr.Reset()
		if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
			t.Fatalf("runWorkerCheckTool: %v", err)
		}
		if exitCode != 2 {
			t.Errorf("exit code = %d, want 2 — phases.working.allow must not leak into planning", exitCode)
		}
	})

	// TestRunWorkerCheckToolProjectConfig_DenyFloorUnremovable is the exact
	// privilege-escalation shape a prior attempt shipped: a repo config
	// granting git push/commit under a non-floor-owning phase's own allow
	// list must still be denied — the deny floor is subtracted last and is
	// unremovable, in every phase, no exception for "but this phase allowed
	// it".
	t.Run("deny floor survives even when a phase's own allow tries to grant git push/commit", func(t *testing.T) {
		exitCode = 0
		writeConfig(t, &repoconfig.Config{
			Phases: protocol.PhaseConfig{protocol.PhaseWorking: {Allow: []string{"Bash(git push*)", "Bash(git commit*)"}}},
		})
		writeStatus(t, protocol.PhaseWorking)

		for _, cmd := range []string{"git push origin HEAD", "git commit -m x"} {
			exitCode = 0
			stdin := strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":%q}}`, repo, cmd))
			var stderr bytes.Buffer
			if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
				t.Fatalf("runWorkerCheckTool: %v", err)
			}
			if exitCode != 2 {
				t.Errorf("cmd %q: exit code = %d, want 2 — deny floor must survive an over-broad phases.working.allow", cmd, exitCode)
			}
		}
	})
}
