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
	floorAllow := supervisor.ResolvedAllowForPhase(protocol.PhasePlanning, "/tmp/wt", nil, nil, nil)

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
	if !strings.Contains(reason, "report `blocked`") {
		t.Errorf("reason = %q, want it to tell the worker to report blocked", reason)
	}
	if strings.Contains(reason, "add it to phases") {
		t.Errorf("reason = %q, want it to NOT point the worker at editing .argus/config.yml — that has no effect from inside a worktree", reason)
	}

	// A worker whose repo grants `make *` but not bare `go` must see `make`
	// listed as an alternative, so it reaches for `make ci`/`make build`
	// instead of concluding make is denied too and self-blocking — the exact
	// wasted-round failure this fixes.
	makeOnlyAllow := supervisor.ResolvedAllowForPhase(protocol.PhaseWorking, "/tmp/wt", nil, []string{"Bash(make *)"}, nil)
	goDeniedReason, blocked := evaluateToolGate("go build ./...", protocol.PhaseWorking, denied, makeOnlyAllow)
	if !blocked {
		t.Error("evaluateToolGate(go build, working, make-only allow) = not blocked, want blocked")
	}
	if !strings.Contains(goDeniedReason, "make") {
		t.Errorf("reason = %q, want it to surface make as the allowed alternative", goDeniedReason)
	}

	project := protocol.PhaseConfig{protocol.PhaseWorking: {Allow: []string{"Bash(go test*)"}}}
	workingAllow := supervisor.ResolvedAllowForPhase(protocol.PhaseWorking, "/tmp/wt", project, nil, nil)
	if _, blocked := evaluateToolGate("go test ./...", protocol.PhaseWorking, denied, workingAllow); blocked {
		t.Error("evaluateToolGate(go test, working, phases.working.allow) = blocked, want not blocked")
	}

	// Resolving for a *different* phase against the same project config must
	// not carry the working-only entry along — the caller (runWorkerCheckTool)
	// is what keeps a live check phase-scoped, by always resolving allow
	// against the worker's *current* phase, never a stale or unrelated one.
	planningAllow := supervisor.ResolvedAllowForPhase(protocol.PhasePlanning, "/tmp/wt", project, nil, nil)
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

	t.Run("no status.json yet resolves as planning, not a fail-open blind spot", func(t *testing.T) {
		exitCode = 0
		wt := newWorktree(t, "")
		// git commit is denied in every phase, including planning — a missing
		// status.json must not fail open the way Phase("") used to.
		stdin := strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":"git commit -m x"}}`, wt))
		var stderr bytes.Buffer
		if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
			t.Fatalf("runWorkerCheckTool: %v", err)
		}
		if exitCode != 2 {
			t.Errorf("exit code = %d, want 2 — no status.json resolves as planning, and planning still denies commit/push", exitCode)
		}

		exitCode = 0
		// A command outside the structural floor (and no repo config to grant
		// it) must also be denied, the same as an explicit planning report —
		// not silently allowed because the file happens to be missing.
		stdin = strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":"npm publish"}}`, wt))
		stderr.Reset()
		if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
			t.Fatalf("runWorkerCheckTool: %v", err)
		}
		if exitCode != 2 {
			t.Errorf("exit code = %d, want 2 — no status.json resolves as planning, which never grants npm publish", exitCode)
		}

		exitCode = 0
		// The structural floor itself must still resolve — a missing
		// status.json is planning, not zero enforcement in either direction.
		stdin = strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":"git status"}}`, wt))
		stderr.Reset()
		if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
			t.Fatalf("runWorkerCheckTool: %v", err)
		}
		if exitCode != 0 {
			t.Errorf("exit code = %d, want 0 — git status is in the all-phase structural floor", exitCode)
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

	t.Run("blocks argus worker record-plan regardless of phase", func(t *testing.T) {
		exitCode = 0
		wt := newWorktree(t, protocol.PhaseWorking)
		stdin := strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":"argus worker record-plan"}}`, wt))
		var stderr bytes.Buffer
		if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
			t.Fatalf("runWorkerCheckTool: %v", err)
		}
		if exitCode != 2 {
			t.Errorf("exit code = %d, want 2 — record-plan only ever runs as a PostToolUse hook, never a worker's own Bash self-invocation", exitCode)
		}
		if !strings.Contains(stderr.String(), "PostToolUse hook") {
			t.Errorf("stderr = %q, want it to explain record-plan is hook-only", stderr.String())
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

// TestRunWorkerCheckToolRebaseDispatchAllowsFetchMergeDeniesCommitPush is the
// end-to-end acceptance test for the dontAsk rebase deadlock: it drives the
// real PreToolUse hook entrypoint (runWorkerCheckTool, the exact function
// `argus worker check-tool` runs) against a worktree shaped exactly like a
// freshly dispatched rebase worker — no .argus/config.yml (a default,
// unmigrated repo, so the operator never hand-added anything) and
// status.json stamped Phase: PhaseRebase with Base set, exactly as
// dispatchRebaseWorker now leaves a worktree before the worker's first
// report. Unlike the mechanism this replaced, no extraAllow is persisted at
// all — runWorkerCheckTool computes the grant live, from cur.Base, every
// call. It confirms both of RebaseBrief's instructed git commands pass, git
// commit/push stay denied, the structural floor's git ls-files entry (every
// worker brief's shared diff_stat instruction) also passes, and — since
// rebase is a mutation phase — Edit/Write is allowed too, so the worker can
// actually resolve a real content conflict rather than only ever auto-merge.
func TestRunWorkerCheckToolRebaseDispatchAllowsFetchMergeDeniesCommitPush(t *testing.T) {
	origExit := osExit
	t.Cleanup(func() { osExit = origExit })
	var exitCode int
	osExit = func(code int) { exitCode = code }

	repo := t.TempDir()
	initGitDirAt(t, repo)
	// No repoconfig.Save call: this repo's .argus/config.yml stays entirely
	// absent, the default/unmigrated state the acceptance criteria require.
	if err := protocol.Write(protocol.StatusPath(repo), &protocol.Status{Base: "main", Phase: protocol.PhaseRebase}); err != nil {
		t.Fatalf("seeding rebase-phase status: %v", err)
	}

	run := func(t *testing.T, cmd string) int {
		t.Helper()
		exitCode = 0
		stdin := strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":%q}}`, repo, cmd))
		var stderr bytes.Buffer
		if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
			t.Fatalf("runWorkerCheckTool(%q): %v", cmd, err)
		}
		return exitCode
	}

	for _, cmd := range []string{"git fetch origin main", "git merge origin/main --no-commit", "git ls-files --others --exclude-standard"} {
		if got := run(t, cmd); got != 0 {
			t.Errorf("cmd %q: exit code = %d, want 0 (allowed)", cmd, got)
		}
	}
	for _, cmd := range []string{"git commit -m x", "git push origin feat-x"} {
		if got := run(t, cmd); got != 2 {
			t.Errorf("cmd %q: exit code = %d, want 2 (denied — argus ship commits/pushes, not the worker)", cmd, got)
		}
	}

	exitCode = 0
	stdin := strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_name":"Edit","tool_input":{"file_path":"conflict.go"}}`, repo))
	var stderr bytes.Buffer
	if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
		t.Fatalf("runWorkerCheckTool(Edit): %v", err)
	}
	if exitCode != 0 {
		t.Errorf("Edit during rebase: exit code = %d, want 0 — rebase is a mutation phase, so a worker can actually resolve a real content conflict", exitCode)
	}
}

// TestRunWorkerCheckToolDeniesRebaseGitCommandsOutsideRebasePhase is the
// other half of the dontAsk-era rebase deadlock's acceptance criteria: now
// that provisionWorktree bakes RebasePhaseAllow's git fetch/merge grant into
// the *static* settings.local.json unconditionally (see ResolvedAllowSet's
// doc comment), the live check-tool hook is what must still deny those exact
// commands whenever the worker reports a phase other than rebase — the
// static file being broadly permissive must never leak into what a live
// call for the "working" phase actually resolves to.
func TestRunWorkerCheckToolDeniesRebaseGitCommandsOutsideRebasePhase(t *testing.T) {
	origExit := osExit
	t.Cleanup(func() { osExit = origExit })
	var exitCode int
	osExit = func(code int) { exitCode = code }

	wt := t.TempDir()
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{Base: "main", Phase: protocol.PhaseWorking}); err != nil {
		t.Fatalf("seeding working-phase status: %v", err)
	}

	for _, cmd := range []string{"git fetch origin main", "git merge origin/main --no-commit"} {
		exitCode = 0
		stdin := strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":%q}}`, wt, cmd))
		var stderr bytes.Buffer
		if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
			t.Fatalf("runWorkerCheckTool(%q): %v", cmd, err)
		}
		if exitCode != 2 {
			t.Errorf("cmd %q during working: exit code = %d, want 2 — rebase's git grant must not leak outside the rebase phase", cmd, exitCode)
		}
		if !strings.Contains(stderr.String(), "not in the resolved allow set") {
			t.Errorf("cmd %q: stderr = %q, want the allow-scope block reason", cmd, stderr.String())
		}
	}
}

// TestRunWorkerCheckToolRebaseDispatchGrantsConfiguredVerifyCommand confirms
// the rebase phase also grants whichever of the repo's ship_verify_command/
// gate_verify_command is configured, so the worker can actually confirm the
// merge RebaseBrief instructs it to re-verify — falling back to
// gate_verify_command when ship_verify_command is unset.
func TestRunWorkerCheckToolRebaseDispatchGrantsConfiguredVerifyCommand(t *testing.T) {
	origExit := osExit
	t.Cleanup(func() { osExit = origExit })
	var exitCode int
	osExit = func(code int) { exitCode = code }

	repo := t.TempDir()
	initGitDirAt(t, repo)
	if err := repoconfig.Save(repoconfig.Path(repo), &repoconfig.Config{
		Phases: protocol.PhaseConfig{protocol.PhaseAwaitingReview: {}},
	}); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	// Set gate_verify_command directly, since repoconfig.Save's encoder only
	// writes it under phases.awaiting_review — this test only cares that
	// runWorkerCheckTool reads it back and grants it during rebase.
	cfg, err := repoconfig.Load(repoconfig.Path(repo))
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	cfg.GateVerifyCommand = "make ci"
	if err := repoconfig.Save(repoconfig.Path(repo), &cfg); err != nil {
		t.Fatalf("Save config with gate_verify_command: %v", err)
	}
	if err := protocol.Write(protocol.StatusPath(repo), &protocol.Status{Base: "main", Phase: protocol.PhaseRebase}); err != nil {
		t.Fatalf("seeding rebase-phase status: %v", err)
	}

	stdin := strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":"make ci"}}`, repo))
	var stderr bytes.Buffer
	if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
		t.Fatalf("runWorkerCheckTool: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("exit code = %d, want 0 — the rebase phase should grant the repo's configured gate_verify_command: %s", exitCode, stderr.String())
	}
}

// TestRunWorkerCheckToolMutationGate exercises the Edit/Write live gate
// directly: mutation is denied outside working/self_test/rebase and allowed
// inside them, regardless of what the static settings.local.json Allow list
// — which can't itself narrow by phase — says.
func TestRunWorkerCheckToolMutationGate(t *testing.T) {
	origExit := osExit
	t.Cleanup(func() { osExit = origExit })
	var exitCode int
	osExit = func(code int) { exitCode = code }

	run := func(t *testing.T, phase protocol.Phase, toolName string) int {
		t.Helper()
		wt := t.TempDir()
		if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{Phase: phase}); err != nil {
			t.Fatalf("writing status: %v", err)
		}
		exitCode = 0
		stdin := strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_name":%q,"tool_input":{"file_path":"x.go"}}`, wt, toolName))
		var stderr bytes.Buffer
		if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
			t.Fatalf("runWorkerCheckTool: %v", err)
		}
		return exitCode
	}

	denyPhases := []protocol.Phase{protocol.PhasePlanning, protocol.PhaseAwaitingReview, protocol.PhaseBlocked}
	allowPhases := []protocol.Phase{protocol.PhaseWorking, protocol.PhaseSelfTest, protocol.PhaseRebase}
	for _, tool := range []string{"Edit", "Write"} {
		for _, p := range denyPhases {
			if got := run(t, p, tool); got != 2 {
				t.Errorf("%s during %q: exit code = %d, want 2", tool, p, got)
			}
		}
		for _, p := range allowPhases {
			if got := run(t, p, tool); got != 0 {
				t.Errorf("%s during %q: exit code = %d, want 0", tool, p, got)
			}
		}
	}
}

func TestLoadProjectConfig(t *testing.T) {
	t.Run("not a git repo fails open to a zero Config", func(t *testing.T) {
		cfg := loadProjectConfig(context.Background(), t.TempDir())
		if cfg.Phases != nil || cfg.Allow != nil {
			t.Errorf("loadProjectConfig(non-git dir) = %+v, want a zero Config", cfg)
		}
	})

	t.Run("malformed config.yml fails open to a zero Config", func(t *testing.T) {
		repo := t.TempDir()
		initGitDirAt(t, repo)
		if err := os.MkdirAll(filepath.Dir(repoconfig.Path(repo)), 0o755); err != nil {
			t.Fatalf("mkdir .argus: %v", err)
		}
		if err := os.WriteFile(repoconfig.Path(repo), []byte("not: [valid\nallow"), 0o600); err != nil {
			t.Fatalf("seeding malformed config: %v", err)
		}
		cfg := loadProjectConfig(context.Background(), repo)
		if cfg.Phases != nil || cfg.Allow != nil {
			t.Errorf("loadProjectConfig(malformed config) = %+v, want a zero Config", cfg)
		}
	})

	t.Run("valid config resolves phases, allow, and verify commands", func(t *testing.T) {
		repo := t.TempDir()
		initGitDirAt(t, repo)
		if err := repoconfig.Save(repoconfig.Path(repo), &repoconfig.Config{
			Allow:             []string{"Bash(make *)"},
			Phases:            protocol.PhaseConfig{protocol.PhaseWorking: {Allow: []string{"Bash(go test*)"}}},
			ShipVerifyCommand: "make ci",
		}); err != nil {
			t.Fatalf("Save config: %v", err)
		}
		cfg := loadProjectConfig(context.Background(), repo)
		if len(cfg.Allow) != 1 || cfg.Allow[0] != "Bash(make *)" {
			t.Errorf("Allow = %v, want the configured top-level allow", cfg.Allow)
		}
		if got := cfg.Phases[protocol.PhaseWorking].Allow; len(got) != 1 || got[0] != "Bash(go test*)" {
			t.Errorf("Phases[working].Allow = %v, want the configured entry", got)
		}
		if cfg.ShipVerifyCommand != "make ci" {
			t.Errorf("ShipVerifyCommand = %q, want %q", cfg.ShipVerifyCommand, "make ci")
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

	// TestRunWorkerCheckToolProjectConfig_DenyMessageSurfacesMakeAlternative
	// reproduces the exact wasted-round failure the issue this fixes
	// describes: a repo grants `make *` but not bare `go`, a worker runs `go
	// build ./...`, gets denied, and needs the deny message itself to point
	// it at `make` — not at hand-editing .argus/config.yml, which the worker
	// cannot reach from inside its worktree.
	t.Run("deny message for a make-only repo surfaces make as the allowed alternative to bare go", func(t *testing.T) {
		exitCode = 0
		writeConfig(t, &repoconfig.Config{Allow: []string{"Bash(make *)"}})
		writeStatus(t, protocol.PhaseWorking)
		stdin := strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":"go build ./..."}}`, repo))
		var stderr bytes.Buffer
		if err := runWorkerCheckTool(context.Background(), stdin, &stderr); err != nil {
			t.Fatalf("runWorkerCheckTool: %v", err)
		}
		if exitCode != 2 {
			t.Errorf("exit code = %d, want 2 — bare go was never granted", exitCode)
		}
		got := stderr.String()
		if !strings.Contains(got, "make") {
			t.Errorf("stderr = %q, want it to surface make as the allowed alternative", got)
		}
		if !strings.Contains(got, "report `blocked`") {
			t.Errorf("stderr = %q, want it to tell the worker to report blocked instead of editing config", got)
		}
		if strings.Contains(got, "add it to phases") {
			t.Errorf("stderr = %q, want it to NOT point the worker at editing .argus/config.yml", got)
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
