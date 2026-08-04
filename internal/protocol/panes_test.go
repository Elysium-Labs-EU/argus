package protocol

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestWritePaneRegistryUnwritableDir(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.Chmod(repoRoot, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(repoRoot, 0o700) }) // let t.TempDir's own cleanup remove it

	if err := WritePaneRegistry(repoRoot, PaneRegistry{Panes: map[string]string{"k": "v"}}); err == nil {
		t.Fatal("want error writing registry under a read-only repo root, got nil")
	}
}

func TestLoadPaneRegistryNoPanesKeyReinitsEmptyMap(t *testing.T) {
	repoRoot := t.TempDir()
	path := PaneRegistryPath(repoRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Valid JSON, but no "panes" key at all — Panes decodes nil, not empty,
	// so the reinit branch is the only thing standing between callers and a
	// nil-map write panic.
	if err := os.WriteFile(path, []byte(`{"nested":{"x":true}}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := LoadPaneRegistry(repoRoot)
	if err != nil {
		t.Fatalf("LoadPaneRegistry: %v", err)
	}
	if got.Panes == nil || len(got.Panes) != 0 {
		t.Errorf("want a non-nil empty Panes map, got %+v", got.Panes)
	}
}

func TestLoadPaneRegistryUnreadablePath(t *testing.T) {
	repoRoot := t.TempDir()
	path := PaneRegistryPath(repoRoot)
	// A directory at the registry path fails os.ReadFile with EISDIR, not
	// ErrNotExist — this exercises the "reading pane registry" branch
	// distinct from the missing-file case.
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	_, err := LoadPaneRegistry(repoRoot)
	if err == nil {
		t.Fatal("want error reading a registry path that is a directory, got nil")
	}
	if got := err.Error(); !strings.Contains(got, "reading pane registry") {
		t.Errorf("error %q does not mention reading pane registry", got)
	}
}

func TestLoadPaneRegistryMalformedJSON(t *testing.T) {
	repoRoot := t.TempDir()
	path := PaneRegistryPath(repoRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := LoadPaneRegistry(repoRoot); err == nil {
		t.Fatal("want error decoding malformed pane registry, got nil")
	}
}

func TestWritePaneRegistryRenameFails(t *testing.T) {
	repoRoot := t.TempDir()
	path := PaneRegistryPath(repoRoot)
	// A directory already sitting at the target path makes the final
	// os.Rename fail after the tmp file was written successfully —
	// distinct from a MkdirAll or WriteFile failure.
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	err := WritePaneRegistry(repoRoot, PaneRegistry{Panes: map[string]string{"k": "v"}})
	if err == nil {
		t.Fatal("want error renaming into a path occupied by a directory, got nil")
	}
	if got := err.Error(); !strings.Contains(got, "renaming pane registry into place") {
		t.Errorf("error %q does not mention renaming pane registry into place", got)
	}
}

func TestWritePaneRegistryWriteFailsAfterMkdirAll(t *testing.T) {
	repoRoot := t.TempDir()
	dir := filepath.Dir(PaneRegistryPath(repoRoot))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// The dir already exists, so WritePaneRegistry's own MkdirAll is a
	// no-op that succeeds regardless of permissions — only the WriteFile
	// into it fails here.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) }) // let t.TempDir's own cleanup remove it

	err := WritePaneRegistry(repoRoot, PaneRegistry{Panes: map[string]string{"k": "v"}})
	if err == nil {
		t.Fatal("want error writing into a read-only registry dir, got nil")
	}
	if got := err.Error(); !strings.Contains(got, "writing pane registry") {
		t.Errorf("error %q does not mention writing pane registry", got)
	}
}
