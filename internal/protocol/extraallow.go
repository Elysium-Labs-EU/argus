package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ExtraAllowPath is where a worktree's operator-supplied --allow flags
// (captured once at spawn time by WriteSettings) are persisted, so a live
// PreToolUse hook can fold them into its own resolved-allow check on every
// Bash call. Unlike a repo's .argus/config.yml (read fresh, each call, from
// the trusted main checkout), extraAllow has no repo-side home — it is
// purely a property of this one invocation — so it has to live somewhere
// the worktree itself can hand back to the hook that runs inside it.
func ExtraAllowPath(worktree string) string {
	return filepath.Join(worktree, ".claude", "argus", "extra_allow.json")
}

// extraAllowFile is ExtraAllowPath's on-disk shape — just the list, no
// envelope needed since nothing else is ever recorded alongside it.
type extraAllowFile struct {
	Allow []string `json:"allow"`
}

// SaveExtraAllow writes allow to worktree's ExtraAllowPath, creating its
// parent directory if needed. A nil/empty allow still writes the file (an
// explicit empty list), so a stale extra_allow.json from a worktree directory
// reused across runs can never be mistaken for this run's own (also empty)
// flags.
func SaveExtraAllow(worktree string, allow []string) error {
	path := ExtraAllowPath(worktree)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("creating extra-allow dir: %w", err)
	}
	data, err := json.MarshalIndent(extraAllowFile{Allow: allow}, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding extra allow: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing extra allow: %w", err)
	}
	return nil
}

// LoadExtraAllow reads worktree's persisted --allow flags. A missing file
// (a worktree WriteSettings never provisioned, or an --attach target argus
// didn't spawn) returns nil with no error — the live hook fails open to "no
// extra flags" rather than failing the whole permission check.
func LoadExtraAllow(worktree string) ([]string, error) {
	data, err := os.ReadFile(ExtraAllowPath(worktree))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading extra allow: %w", err)
	}
	var f extraAllowFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parsing extra allow: %w", err)
	}
	return f.Allow, nil
}
