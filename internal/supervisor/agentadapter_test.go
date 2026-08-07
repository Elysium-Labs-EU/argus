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
	path, content, err := (claudeCodeAdapter{}).RenderSettings(wt, nil, []string{"Bash(pnpm *)"}, []string{"Bash(task *)"}, nil, false, nil)
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
	settings := settingsFor(wt, nil, nil, nil, nil, false, nil)

	want := []string{
		"Edit(" + absPathPattern(wt+"/.claude/settings.local.json") + ")",
		"Write(" + absPathPattern(wt+"/.claude/settings.local.json") + ")",
		"Edit(" + absPathPattern(wt+"/.claude/settings.json") + ")",
		"Write(" + absPathPattern(wt+"/.claude/settings.json") + ")",
	}
	for _, entry := range want {
		if !slices.Contains(settings.Permissions.Deny, entry) {
			t.Errorf("deny list missing %q; got %v", entry, settings.Permissions.Deny)
		}
	}
	// The allow glob this carve-out protects against is rendered
	// filesystem-absolute (see TestStructuralFloorAllow_EditWriteUseDoubleSlash);
	// a single-slash deny here would match nothing and silently reopen these
	// files to Edit/Write.
	bad := []string{
		"Edit(" + wt + "/.claude/settings.local.json)",
		"Write(" + wt + "/.claude/settings.local.json)",
	}
	for _, entry := range bad {
		if slices.Contains(settings.Permissions.Deny, entry) {
			t.Errorf("deny list contains single-slash %q, which matches nothing under dontAsk; got %v", entry, settings.Permissions.Deny)
		}
	}
}

// TestSettingsForEnablesAllProjectMcpServers guards against a headless
// worker hanging forever on Claude Code's one-time MCP-server consent gate:
// with no human present to answer it, every project MCP server must already
// be pre-approved before the worker's launcher ever starts.
func TestSettingsForEnablesAllProjectMcpServers(t *testing.T) {
	settings := settingsFor("/repo/.claude/worktrees/feat-x", nil, nil, nil, nil, false, nil)
	if !settings.EnableAllProjectMcpServers {
		t.Error("expected EnableAllProjectMcpServers to be true so the first-run MCP consent gate never blocks a headless worker")
	}
}

func TestSettingsForDeniesEditOfControlPlaneFiles(t *testing.T) {
	wt := "/repo/.claude/worktrees/feat-x"
	settings := settingsFor(wt, nil, nil, nil, nil, false, nil)

	want := []string{
		"Edit(" + absPathPattern(wt+"/.claude/argus/**") + ")",
		"Write(" + absPathPattern(wt+"/.claude/argus/**") + ")",
	}
	for _, entry := range want {
		if !slices.Contains(settings.Permissions.Deny, entry) {
			t.Errorf("deny list missing %q; got %v", entry, settings.Permissions.Deny)
		}
	}
	bad := []string{
		"Edit(" + wt + "/.claude/argus/**)",
		"Write(" + wt + "/.claude/argus/**)",
	}
	for _, entry := range bad {
		if slices.Contains(settings.Permissions.Deny, entry) {
			t.Errorf("deny list contains single-slash %q, which matches nothing under dontAsk; got %v", entry, settings.Permissions.Deny)
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
	settings := settingsFor("/repo/.claude/worktrees/feat-x", nil, nil, nil, nil, false, nil)
	for _, cmd := range protocol.DenyFloor() {
		want := "Bash(" + cmd + ":*)"
		if !slices.Contains(settings.Permissions.Deny, want) {
			t.Errorf("deny list missing %q for DenyFloor entry %q; got %v", want, cmd, settings.Permissions.Deny)
		}
	}
}

// TestSettingsForDeniesCredentialFiles confirms settingsFor carries every
// protocol.CredentialDenyFloor() entry into the rendered Permissions.Deny,
// alongside (not instead of) the pre-existing control-plane/self-settings
// deny entries — a worker's Read tool bypasses the OS sandbox entirely, so
// this static list is the only thing standing between it and the
// orchestrator's own ~/.ssh, ~/.aws, and similar credential files.
func TestSettingsForDeniesCredentialFiles(t *testing.T) {
	wt := "/repo/.claude/worktrees/feat-x"
	settings := settingsFor(wt, nil, nil, nil, nil, false, nil)

	for _, entry := range protocol.CredentialDenyFloor() {
		if !slices.Contains(settings.Permissions.Deny, entry) {
			t.Errorf("deny list missing credential-floor entry %q; got %v", entry, settings.Permissions.Deny)
		}
	}

	preExisting := []string{
		"Bash(rm -rf *)",
		"Edit(" + absPathPattern(wt+"/.claude/settings.local.json") + ")",
		"Edit(" + absPathPattern(wt+"/.claude/argus/**") + ")",
	}
	for _, entry := range preExisting {
		if !slices.Contains(settings.Permissions.Deny, entry) {
			t.Errorf("deny list lost pre-existing entry %q after adding credential floor; got %v", entry, settings.Permissions.Deny)
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
	settings := settingsFor("/repo/.claude/worktrees/feat-x", project, nil, nil, nil, false, nil)
	if !slices.Contains(settings.Permissions.Allow, "Bash(go test*)") {
		t.Errorf("allow list missing phases.working.allow entry; got %v", settings.Permissions.Allow)
	}
}

// TestSettingsForWiresCheckToolHook confirms every freshly spawned worker
// gets the phase-aware PreToolUse hook, on all three tools whose phase
// scoping is only ever enforced live (Bash commands and, since Edit/Write
// can't be phase-scoped in the static Allow list — see PhaseAllowsMutation
// — Edit/Write calls too), without any operator config: the static
// allow/deny lists above are only read once at session launch, so
// DeniedInPhase's/ResolvedAllowForPhase's live per-phase enforcement depends
// on this hook actually being present in every rendered settings file.
func TestSettingsForWiresCheckToolHook(t *testing.T) {
	settings := settingsFor("/repo/.claude/worktrees/feat-x", nil, nil, nil, nil, false, nil)
	if len(settings.Hooks.PreToolUse) != 3 {
		t.Fatalf("PreToolUse hooks = %d, want 3", len(settings.Hooks.PreToolUse))
	}
	wantMatchers := map[string]bool{"Bash": false, "Edit": false, "Write": false}
	for _, matcher := range settings.Hooks.PreToolUse {
		if _, ok := wantMatchers[matcher.Matcher]; !ok {
			t.Errorf("unexpected matcher %q", matcher.Matcher)
			continue
		}
		wantMatchers[matcher.Matcher] = true
		if len(matcher.Hooks) != 1 || matcher.Hooks[0].Command != "argus worker check-tool" {
			t.Errorf("matcher %q hook entries = %+v, want a single \"argus worker check-tool\" command", matcher.Matcher, matcher.Hooks)
		}
	}
	for m, seen := range wantMatchers {
		if !seen {
			t.Errorf("missing PreToolUse hook for matcher %q", m)
		}
	}
}

// TestRecordPlanHooksShape pins recordPlanHooks' own matcher/command shape,
// independent of settingsFor's wiring — the two TodoWrite/TaskCreate|
// TaskUpdate matchers, each pointing at the single hidden `argus worker
// record-plan` command.
func TestRecordPlanHooksShape(t *testing.T) {
	hooks := recordPlanHooks()
	if len(hooks) != 2 {
		t.Fatalf("recordPlanHooks() = %d matchers, want 2", len(hooks))
	}
	wantMatchers := map[string]bool{"TodoWrite": false, "TaskCreate|TaskUpdate": false}
	for _, matcher := range hooks {
		if _, ok := wantMatchers[matcher.Matcher]; !ok {
			t.Errorf("unexpected matcher %q", matcher.Matcher)
			continue
		}
		wantMatchers[matcher.Matcher] = true
		if len(matcher.Hooks) != 1 || matcher.Hooks[0].Command != "argus worker record-plan" {
			t.Errorf("matcher %q hook entries = %+v, want a single \"argus worker record-plan\" command", matcher.Matcher, matcher.Hooks)
		}
	}
	for m, seen := range wantMatchers {
		if !seen {
			t.Errorf("missing matcher %q", m)
		}
	}
}

// TestSettingsForWiresRecordPlanHook is TestSettingsForWiresCheckToolHook's
// PostToolUse counterpart: every freshly spawned worker must get the live
// plan-evidence recorder, not just the PreToolUse gate, or
// HasFreshPlanEvidence would have nothing to check against for a normal
// argus-spawned worker.
func TestSettingsForWiresRecordPlanHook(t *testing.T) {
	settings := settingsFor("/repo/.claude/worktrees/feat-x", nil, nil, nil, nil, false, nil)
	if len(settings.Hooks.PostToolUse) != 2 {
		t.Fatalf("PostToolUse hooks = %d, want 2", len(settings.Hooks.PostToolUse))
	}
	wantMatchers := map[string]bool{"TodoWrite": false, "TaskCreate|TaskUpdate": false}
	for _, matcher := range settings.Hooks.PostToolUse {
		if _, ok := wantMatchers[matcher.Matcher]; !ok {
			t.Errorf("unexpected matcher %q", matcher.Matcher)
			continue
		}
		wantMatchers[matcher.Matcher] = true
		if len(matcher.Hooks) != 1 || matcher.Hooks[0].Command != "argus worker record-plan" {
			t.Errorf("matcher %q hook entries = %+v, want a single \"argus worker record-plan\" command", matcher.Matcher, matcher.Hooks)
		}
	}
	for m, seen := range wantMatchers {
		if !seen {
			t.Errorf("missing PostToolUse hook for matcher %q", m)
		}
	}
}

// TestSettingsForSandboxDisabledByDefault confirms settingsFor renders no
// "sandbox" key at all when the toggle is off — the default, so an
// unconfigured or non-opted-in repo's worker is unaffected by this feature
// existing.
func TestSettingsForSandboxDisabledByDefault(t *testing.T) {
	settings := settingsFor("/repo/.claude/worktrees/feat-x", nil, nil, nil, nil, false, nil)
	if settings.Sandbox != nil {
		t.Errorf("Sandbox = %+v, want nil (no sandbox key rendered) when the toggle is off", settings.Sandbox)
	}
}

// TestSettingsForSandboxEnabledRendersFullBlock confirms the enabled sandbox
// block carries every credential deny path, all four boolean flags, and
// filesystem.allowWrite populated from sandbox_allow_write.
func TestSettingsForSandboxEnabledRendersFullBlock(t *testing.T) {
	allowWrite := []string{"/home/me/go/pkg/mod", "/home/me/.cache/go-build"}
	settings := settingsFor("/repo/.claude/worktrees/feat-x", nil, nil, nil, nil, true, allowWrite)
	if settings.Sandbox == nil {
		t.Fatal("Sandbox = nil, want a rendered block when the toggle is on")
	}
	s := settings.Sandbox
	if !s.Enabled {
		t.Error("Sandbox.Enabled = false, want true")
	}
	if !s.AutoAllowBashIfSandboxed {
		t.Error("Sandbox.AutoAllowBashIfSandboxed = false, want true")
	}
	if s.AllowUnsandboxedCommands {
		t.Error("Sandbox.AllowUnsandboxedCommands = true, want false")
	}
	if !s.FailIfUnavailable {
		t.Error("Sandbox.FailIfUnavailable = false, want true")
	}
	wantPaths := []string{
		"~/.ssh", "~/.aws", "~/.azure", "~/.config/gh", "~/.git-credentials",
		"~/.gnupg", "~/.docker/config.json", "~/.kube", "~/.npmrc", "~/.pypirc", "~/.gem/credentials",
	}
	if len(s.Credentials.Files) != len(wantPaths) {
		t.Fatalf("Sandbox.Credentials.Files = %d entries, want %d", len(s.Credentials.Files), len(wantPaths))
	}
	for _, want := range wantPaths {
		found := false
		for _, f := range s.Credentials.Files {
			if f.Path == want {
				found = true
				if f.Mode != "deny" {
					t.Errorf("credentials.files[%q].Mode = %q, want %q", want, f.Mode, "deny")
				}
				break
			}
		}
		if !found {
			t.Errorf("credentials.files missing deny entry for %q; got %+v", want, s.Credentials.Files)
		}
	}
	if s.Filesystem == nil {
		t.Fatal("Sandbox.Filesystem = nil, want a populated allowWrite block")
	}
	if !slices.Equal(s.Filesystem.AllowWrite, allowWrite) {
		t.Errorf("Sandbox.Filesystem.AllowWrite = %v, want %v", s.Filesystem.AllowWrite, allowWrite)
	}
}

// TestSettingsForSandboxEnabledOmitsFilesystemWhenAllowWriteEmpty confirms
// the filesystem sub-block is left out entirely (not rendered empty) when
// sandbox_allow_write has no entries — see sandboxSettings' own doc comment
// on why no whole-home whitelist or denyRead is ever rendered either.
func TestSettingsForSandboxEnabledOmitsFilesystemWhenAllowWriteEmpty(t *testing.T) {
	settings := settingsFor("/repo/.claude/worktrees/feat-x", nil, nil, nil, nil, true, nil)
	if settings.Sandbox == nil {
		t.Fatal("Sandbox = nil, want a rendered block when the toggle is on")
	}
	if settings.Sandbox.Filesystem != nil {
		t.Errorf("Sandbox.Filesystem = %+v, want nil when sandbox_allow_write is empty", settings.Sandbox.Filesystem)
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
