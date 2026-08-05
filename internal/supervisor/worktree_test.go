package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
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
	s := settingsFor(wt, nil, nil, nil)

	wantAllow := "Edit(" + wt + "/**)"
	if !slices.Contains(s.Permissions.Allow, wantAllow) {
		t.Errorf("allow missing %q; got %v", wantAllow, s.Permissions.Allow)
	}
	if !slices.Contains(s.Permissions.Deny, "Bash(git commit:*)") {
		t.Errorf("commit should be denied outright (deny floor, dontAsk mode); got %v", s.Permissions.Deny)
	}
	if !slices.Contains(s.Permissions.Deny, "Bash(git push:*)") {
		t.Errorf("push should be denied outright (deny floor, dontAsk mode); got %v", s.Permissions.Deny)
	}
	for _, want := range []string{"Bash(sudo *)", "Bash(rm -rf *)", "Bash(git reset --hard*)"} {
		if !slices.Contains(s.Permissions.Deny, want) {
			t.Errorf("deny missing %q; got %v", want, s.Permissions.Deny)
		}
	}
}

// TestSettingsForNoRepoAllowIsToolchainNeutral pins the toolchain-neutrality
// fix: with no
// repo config, argus assumes no build/test toolchain for anyone (not just
// non-Go repos) — the old hardcoded "Bash(go build *)"/"Bash(make *)"
// defaults must not resurface. git add is deliberately absent too: a worker
// never stages anything — it edits files and reports, and the gate measures
// the uncommitted working tree, so only read-only git plumbing is floor.
func TestSettingsForNoRepoAllowIsToolchainNeutral(t *testing.T) {
	wt := "/repo/.claude/worktrees/feat-x"
	s := settingsFor(wt, nil, nil, nil)

	for _, unwanted := range []string{"Bash(go build *)", "Bash(go test *)", "Bash(go vet *)", "Bash(go get *)", "Bash(make *)", "Bash(git add*)"} {
		if slices.Contains(s.Permissions.Allow, unwanted) {
			t.Errorf("allow should not assume a toolchain (or grant git add) by default; unexpectedly found %q in %v", unwanted, s.Permissions.Allow)
		}
	}
	for _, want := range []string{"Bash(git status*)", "Bash(git diff*)", "Bash(git log*)"} {
		if !slices.Contains(s.Permissions.Allow, want) {
			t.Errorf("allow missing structural-floor read-only git %q; got %v", want, s.Permissions.Allow)
		}
	}
}

func TestSettingsForAppendsRepoAndExtraAllow(t *testing.T) {
	wt := "/repo/.claude/worktrees/feat-x"
	s := settingsFor(wt, nil, []string{"Bash(task *)"}, []string{"Bash(npm *)"})

	for _, want := range []string{"Bash(task *)", "Bash(npm *)"} {
		if !slices.Contains(s.Permissions.Allow, want) {
			t.Errorf("allow missing %q; got %v", want, s.Permissions.Allow)
		}
	}
}

func TestRunWorktreeBootstrapCommandEmptyIsNoop(t *testing.T) {
	if err := RunWorktreeBootstrapCommand(context.Background(), t.TempDir(), ""); err != nil {
		t.Fatalf("empty worktree_setup_cmd should be a no-op, got %v", err)
	}
}

func TestRunWorktreeBootstrapCommandRunsInWorktree(t *testing.T) {
	wt := t.TempDir()
	if err := RunWorktreeBootstrapCommand(context.Background(), wt, "pwd > marker.txt"); err != nil {
		t.Fatalf("RunWorktreeBootstrapCommand: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(wt, "marker.txt"))
	if err != nil {
		t.Fatalf("reading marker.txt: %v", err)
	}
	if strings.TrimSpace(string(got)) != wt {
		t.Errorf("cmd ran with cwd %q, want %q", strings.TrimSpace(string(got)), wt)
	}
}

func TestRunWorktreeBootstrapCommandFailureCarriesOutput(t *testing.T) {
	err := RunWorktreeBootstrapCommand(context.Background(), t.TempDir(), "echo boom >&2; exit 1")
	if err == nil {
		t.Fatal("non-zero exit should return an error, got nil")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should carry the command's captured output, got: %v", err)
	}
}

func TestWriteSettingsWritesConfinedFile(t *testing.T) {
	wt := t.TempDir()
	if err := WriteSettings(wt, nil, nil, []string{"Bash(task *)"}); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}
	path := filepath.Join(wt, ".claude", "settings.local.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading settings: %v", err)
	}
	var round permissionSettings
	if err = json.Unmarshal(data, &round); err != nil {
		t.Fatalf("settings not valid json: %v", err)
	}
	if !strings.Contains(string(data), wt+"/**") {
		t.Errorf("settings should scope edits to the worktree path")
	}

	// WriteSettings also persists extraAllow so the live check-tool hook can
	// read it back — it has no other access to this invocation's own flags.
	extraAllow, err := protocol.LoadExtraAllow(wt)
	if err != nil {
		t.Fatalf("LoadExtraAllow: %v", err)
	}
	if !slices.Contains(extraAllow, "Bash(task *)") {
		t.Errorf("LoadExtraAllow = %v, want it to contain the extraAllow WriteSettings was given", extraAllow)
	}
}

func TestWriteSettingsWrapsMkdirAllError(t *testing.T) {
	wt := t.TempDir()
	// A regular file at .claude blocks MkdirAll from creating it as a dir.
	if err := os.WriteFile(filepath.Join(wt, ".claude"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seeding blocking file: %v", err)
	}

	err := WriteSettings(wt, nil, nil, nil)
	if err == nil {
		t.Fatal("want error when settings dir can't be created, got nil")
	}
	if !strings.Contains(err.Error(), "creating settings dir") {
		t.Errorf("error should be wrapped with context, got: %v", err)
	}
}

func TestWriteSettingsWrapsWriteFileError(t *testing.T) {
	wt := t.TempDir()
	// A directory at the settings path itself blocks WriteFile, while MkdirAll
	// of its already-existing parent still succeeds.
	settingsPath := filepath.Join(wt, ".claude", "settings.local.json")
	if err := os.MkdirAll(settingsPath, 0o755); err != nil {
		t.Fatalf("seeding blocking dir: %v", err)
	}

	err := WriteSettings(wt, nil, nil, nil)
	if err == nil {
		t.Fatal("want error when settings file can't be written, got nil")
	}
	if !strings.Contains(err.Error(), "writing settings file") {
		t.Errorf("error should be wrapped with context, got: %v", err)
	}
}

// TestWriteSettingsWrapsSaveExtraAllowError forces WriteSettings' last
// branch: settings.local.json itself writes fine, but persisting extraAllow
// (protocol.SaveExtraAllow) fails because a regular file already sits where
// .claude/argus/ needs to be a directory.
func TestWriteSettingsWrapsSaveExtraAllowError(t *testing.T) {
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".claude", "argus"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seeding blocking file: %v", err)
	}

	err := WriteSettings(wt, nil, nil, []string{"Bash(task *)"})
	if err == nil {
		t.Fatal("want error when extra_allow.json's parent dir can't be created, got nil")
	}
	if !strings.Contains(err.Error(), "persisting extra allow flags") {
		t.Errorf("error should be wrapped with context, got: %v", err)
	}
}

// fakeFailingAgent is a minimal AgentAdapter whose RenderSettings always
// errors, used to force WriteSettings' one remaining branch (a
// RenderSettings failure) without any adapter that actually fails.
type fakeFailingAgent struct{}

func (fakeFailingAgent) DefaultLauncher() string { return "" }

func (fakeFailingAgent) RenderSettings(string, protocol.PhaseConfig, []string, []string) (string, []byte, error) {
	return "", nil, errors.New("boom")
}

func (fakeFailingAgent) PlanEvidence(string, string) (bool, int, error) { return false, 0, nil }

func TestWriteSettingsWrapsRenderSettingsError(t *testing.T) {
	orig := defaultAgent
	defer func() { defaultAgent = orig }()
	defaultAgent = fakeFailingAgent{}

	err := WriteSettings(t.TempDir(), nil, nil, nil)
	if err == nil {
		t.Fatal("want error when RenderSettings fails, got nil")
	}
	if !strings.Contains(err.Error(), "rendering settings") {
		t.Errorf("error should be wrapped with context, got: %v", err)
	}
}

func TestWriteBriefWritesTrailingNewline(t *testing.T) {
	wt := t.TempDir()
	if err := WriteBrief(wt, "do the thing"); err != nil {
		t.Fatalf("WriteBrief: %v", err)
	}
	got, err := os.ReadFile(protocol.BriefPath(wt))
	if err != nil {
		t.Fatalf("reading brief.md: %v", err)
	}
	if string(got) != "do the thing\n" {
		t.Errorf("brief content = %q, want %q", got, "do the thing\n")
	}
}

func TestWriteBriefWrapsMkdirAllError(t *testing.T) {
	wt := t.TempDir()
	// A regular file at .claude blocks MkdirAll from creating .claude/argus.
	if err := os.WriteFile(filepath.Join(wt, ".claude"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seeding blocking file: %v", err)
	}

	err := WriteBrief(wt, "brief")
	if err == nil {
		t.Fatal("want error when argus dir can't be created, got nil")
	}
	if !strings.Contains(err.Error(), "creating argus dir") {
		t.Errorf("error should be wrapped with context, got: %v", err)
	}
}

func TestWriteBriefWrapsWriteFileError(t *testing.T) {
	wt := t.TempDir()
	// A directory at brief.md's own path blocks WriteFile, while MkdirAll of
	// its already-existing parent still succeeds.
	if err := os.MkdirAll(protocol.BriefPath(wt), 0o755); err != nil {
		t.Fatalf("seeding blocking dir: %v", err)
	}

	err := WriteBrief(wt, "brief")
	if err == nil {
		t.Fatal("want error when brief.md can't be written, got nil")
	}
	if !strings.Contains(err.Error(), "writing brief.md") {
		t.Errorf("error should be wrapped with context, got: %v", err)
	}
}

func TestRunWorktreeBootstrapCommandTimeoutIsKilled(t *testing.T) {
	// Passing an already-short-deadlined parent ctx makes WithTimeout's
	// effective deadline the earlier one, exercising the 5-minute timeout
	// path without waiting anywhere near 5 minutes.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := RunWorktreeBootstrapCommand(ctx, t.TempDir(), "sleep 5")
	if err == nil {
		t.Fatal("want error when bootstrap command exceeds its deadline, got nil")
	}
	if !strings.Contains(err.Error(), "exceeded") || !strings.Contains(err.Error(), "was killed") {
		t.Errorf("error should report the timeout, got: %v", err)
	}
}
