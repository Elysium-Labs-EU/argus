package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Approval is argus's recorded disposition of a worker's change, written by
// supervise and read by ship. It is the typed handoff that makes a verdict
// enforceable rather than advisory: ship refuses to open a PR for a worktree
// whose Approval is missing or not Approved (absent --force). Source is "gate"
// when the deterministic gate cleared it, or "review" when the LLM reviewer did.
type Approval struct {
	UpdatedAt time.Time `json:"updated_at"`
	Source    string    `json:"source"`
	Summary   string    `json:"summary"`
	Reasons   []string  `json:"reasons,omitempty"`
	Approved  bool      `json:"approved"`
}

// VerdictPath is where a worker's Approval lives inside its worktree. It sits
// under .claude/argus so ship (and CommitAll's excludes) can find it, and so it
// never lands in the PR.
func VerdictPath(worktree string) string {
	return filepath.Join(worktree, ".claude", "argus", "verdict.json")
}

// WriteApproval atomically records a's disposition for a worktree.
func WriteApproval(worktree string, a *Approval) error {
	path := VerdictPath(worktree)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("creating verdict dir: %w", err)
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding verdict: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing verdict: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("renaming verdict into place: %w", err)
	}
	return nil
}

// LoadApproval reads a worktree's recorded verdict. found is false (with no
// error) when no verdict was written — the "supervise never cleared this" case
// ship must treat as "not approved".
func LoadApproval(worktree string) (approval Approval, found bool, err error) {
	data, err := os.ReadFile(VerdictPath(worktree))
	if errors.Is(err, fs.ErrNotExist) {
		return Approval{}, false, nil
	}
	if err != nil {
		return Approval{}, false, fmt.Errorf("reading verdict: %w", err)
	}
	if err := json.Unmarshal(data, &approval); err != nil {
		return Approval{}, false, fmt.Errorf("decoding verdict: %w", err)
	}
	return approval, true, nil
}
