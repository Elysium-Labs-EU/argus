package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// PaneRegistry maps a worktree's absolute path to the herdr pane id spawned
// for it. It is rooted at the main repository, not any linked worktree, so it
// survives a worktree directory being deleted by hand — the case a
// worktree's own lifecycle.json cannot survive, since that file lives inside
// the very directory that just vanished. Written once at spawn time, read by
// `argus worktree prune` to find (and close) the pane even when the worktree
// it belonged to is already gone.
type PaneRegistry struct {
	Panes map[string]string `json:"panes"`
	// Nested marks a worktree (by the same key as Panes) whose pane was opened
	// as a tab inside a shared parent herdr workspace rather than a fresh
	// top-level workspace argus owns outright (see
	// herdr.WorktreeSpec.Workspace). A worktree missing from this map was not
	// nested. Cleanup must not close a nested worktree's workspace — argus
	// doesn't own it.
	Nested map[string]bool `json:"nested,omitempty"`
}

// PaneRegistryPath is repoRoot's registry file — a single file shared across
// every worktree spawned from that repo, not one per worktree.
func PaneRegistryPath(repoRoot string) string {
	return filepath.Join(repoRoot, ".claude", "argus", "panes.json")
}

// LoadPaneRegistry reads repoRoot's pane registry. A missing file is not an
// error — no worktree has ever recorded one yet — and decodes to an empty,
// ready-to-use map.
func LoadPaneRegistry(repoRoot string) (PaneRegistry, error) {
	data, err := os.ReadFile(PaneRegistryPath(repoRoot))
	if errors.Is(err, fs.ErrNotExist) {
		return PaneRegistry{Panes: map[string]string{}}, nil
	}
	if err != nil {
		return PaneRegistry{}, fmt.Errorf("reading pane registry: %w", err)
	}
	var r PaneRegistry
	if err := json.Unmarshal(data, &r); err != nil {
		return PaneRegistry{}, fmt.Errorf("decoding pane registry: %w", err)
	}
	if r.Panes == nil {
		r.Panes = map[string]string{}
	}
	return r, nil
}

// WritePaneRegistry atomically persists r as repoRoot's registry file. This
// is a plain read-modify-write with no cross-process locking: two `argus
// supervise` invocations spawning workers in the same repo at the same
// instant can race and lose one write. Left unaddressed for now since it
// requires two concurrent invocations against one repo to trigger, and no
// caller in this codebase does that today.
func WritePaneRegistry(repoRoot string, r PaneRegistry) error {
	path := PaneRegistryPath(repoRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("creating pane registry dir: %w", err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding pane registry: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing pane registry: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("renaming pane registry into place: %w", err)
	}
	return nil
}
