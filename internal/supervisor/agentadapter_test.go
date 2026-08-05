package supervisor

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

func TestClaudeCodeAdapterDefaultLauncher(t *testing.T) {
	if got := (claudeCodeAdapter{}).DefaultLauncher(); got != DefaultLauncher {
		t.Errorf("DefaultLauncher() = %q, want %q", got, DefaultLauncher)
	}
}

func TestClaudeCodeAdapterRenderSettings(t *testing.T) {
	wt := "/repo/.claude/worktrees/feat-x"
	path, content, err := (claudeCodeAdapter{}).RenderSettings(wt, nil, []string{"Bash(pnpm *)"}, []string{"Bash(task *)"})
	if err != nil {
		t.Fatalf("RenderSettings: %v", err)
	}
	if want := filepath.Join(".claude", "settings.local.json"); want != path {
		t.Errorf("path = %q, want %q", path, want)
	}

	var round permissionSettings
	if err := json.Unmarshal(content, &round); err != nil {
		t.Fatalf("content not valid json: %v", err)
	}
	if !slices.Contains(round.Permissions.Allow, "Bash(task *)") {
		t.Errorf("extraAllow not applied via RenderSettings; got %v", round.Permissions.Allow)
	}
	if !slices.Contains(round.Permissions.Allow, "Bash(pnpm *)") {
		t.Errorf("baseAllow not applied via RenderSettings; got %v", round.Permissions.Allow)
	}
}

func TestSettingsForDeniesSelfEditOfOwnPermissionFiles(t *testing.T) {
	wt := "/repo/.claude/worktrees/feat-x"
	settings := settingsFor(wt, nil, nil, nil)

	want := []string{
		"Edit(" + wt + "/.claude/settings.local.json)",
		"Write(" + wt + "/.claude/settings.local.json)",
		"Edit(" + wt + "/.claude/settings.json)",
		"Write(" + wt + "/.claude/settings.json)",
	}
	for _, entry := range want {
		if !slices.Contains(settings.Permissions.Deny, entry) {
			t.Errorf("deny list missing %q; got %v", entry, settings.Permissions.Deny)
		}
	}
}

// TestSettingsForEnablesAllProjectMcpServers guards against a headless
// worker hanging forever on Claude Code's one-time MCP-server consent gate:
// with no human present to answer it, every project MCP server must already
// be pre-approved before the worker's launcher ever starts.
func TestSettingsForEnablesAllProjectMcpServers(t *testing.T) {
	settings := settingsFor("/repo/.claude/worktrees/feat-x", nil, nil, nil)
	if !settings.EnableAllProjectMcpServers {
		t.Error("expected EnableAllProjectMcpServers to be true so the first-run MCP consent gate never blocks a headless worker")
	}
}

func TestSettingsForDeniesEditOfControlPlaneFiles(t *testing.T) {
	wt := "/repo/.claude/worktrees/feat-x"
	settings := settingsFor(wt, nil, nil, nil)

	want := []string{
		"Edit(" + wt + "/.claude/argus/**)",
		"Write(" + wt + "/.claude/argus/**)",
	}
	for _, entry := range want {
		if !slices.Contains(settings.Permissions.Deny, entry) {
			t.Errorf("deny list missing %q; got %v", entry, settings.Permissions.Deny)
		}
	}
}

// TestSettingsForDenyListMatchesDenyFloor confirms settingsFor's static
// permissions.deny carries protocol.DenyFloor()'s own commands (git
// commit/push, argus ship/rework/review/supervise), in glob form, as
// defense in depth alongside checkToolHook's live enforcement — the same
// slice AlwaysDeniedCommands/AskGatedCommands feed every brief's NeverRunBrief
// clause, so the deny list and a brief's own wording can never drift apart.
func TestSettingsForDenyListMatchesDenyFloor(t *testing.T) {
	settings := settingsFor("/repo/.claude/worktrees/feat-x", nil, nil, nil)
	for _, cmd := range protocol.DenyFloor() {
		want := "Bash(" + cmd + ":*)"
		if !slices.Contains(settings.Permissions.Deny, want) {
			t.Errorf("deny list missing %q for DenyFloor entry %q; got %v", want, cmd, settings.Permissions.Deny)
		}
	}
}

// TestSettingsForAllowUnionsEveryPhase confirms the static, session-wide
// allow list is the union of every protocol.ConfigurablePhases value's own
// resolved allow — since this file can't itself vary by the worker's
// current phase, a command allowed only in "working" must still show up
// here (the live checkToolHook is what narrows a call back down to the
// worker's *current* phase; see cmd/worker_check_tool.go).
func TestSettingsForAllowUnionsEveryPhase(t *testing.T) {
	project := protocol.PhaseConfig{protocol.PhaseWorking: {Allow: []string{"Bash(go test*)"}}}
	settings := settingsFor("/repo/.claude/worktrees/feat-x", project, nil, nil)
	if !slices.Contains(settings.Permissions.Allow, "Bash(go test*)") {
		t.Errorf("allow list missing phases.working.allow entry; got %v", settings.Permissions.Allow)
	}
}

// TestSettingsForWiresCheckToolHook confirms every freshly spawned worker
// gets the phase-aware PreToolUse hook without any operator config: the
// static allow/deny lists above are only read once at session launch, so
// DeniedInPhase's/ResolvedAllowForPhase's live per-phase enforcement depends
// on this hook actually being present in every rendered settings file.
func TestSettingsForWiresCheckToolHook(t *testing.T) {
	settings := settingsFor("/repo/.claude/worktrees/feat-x", nil, nil, nil)
	if len(settings.Hooks.PreToolUse) != 1 {
		t.Fatalf("PreToolUse hooks = %d, want 1", len(settings.Hooks.PreToolUse))
	}
	matcher := settings.Hooks.PreToolUse[0]
	if matcher.Matcher != "Bash" {
		t.Errorf("matcher = %q, want %q", matcher.Matcher, "Bash")
	}
	if len(matcher.Hooks) != 1 || matcher.Hooks[0].Command != "argus worker check-tool" {
		t.Errorf("hook entries = %+v, want a single \"argus worker check-tool\" command", matcher.Hooks)
	}
}

func TestClaudeCodeAdapterPlanEvidenceDelegates(t *testing.T) {
	home := t.TempDir()
	wt := t.TempDir()

	ok, transcripts, err := (claudeCodeAdapter{}).PlanEvidence(home, wt)
	if err != nil {
		t.Fatalf("PlanEvidence: %v", err)
	}
	if ok {
		t.Error("PlanEvidence should be false when no transcript exists")
	}
	if transcripts != 0 {
		t.Errorf("transcripts checked = %d, want 0 when no transcript directory exists", transcripts)
	}
}
