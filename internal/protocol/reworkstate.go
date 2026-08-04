package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// ReworkState tracks how many rework rounds a worktree has been dispatched
// for across every separate `argus rework` invocation in its lifetime —
// unlike status.json and verdict.json, InvalidateStatus never removes this
// file, so the count survives a supervisor re-invoking rework after one
// invocation's own --max-rounds already gave up. This is what lets a
// restart budget (see supervisor.DefaultMaxReworkBudget) span invocations
// instead of resetting to zero every time.
type ReworkState struct {
	UpdatedAt       time.Time `json:"updated_at"`
	RoundsAttempted int       `json:"rounds_attempted"`
}

// ReworkStatePath is where a worktree's cumulative rework count lives,
// alongside status.json and verdict.json under the same control-plane
// directory ship's CommitAll always excludes.
func ReworkStatePath(worktree string) string {
	return filepath.Join(worktree, ".claude", "argus", "rework_state.json")
}

// LoadReworkState reads a worktree's rework count. A missing file (no rework
// has ever run against this worktree) returns a zero ReworkState, not an
// error.
func LoadReworkState(worktree string) (ReworkState, error) {
	data, err := os.ReadFile(ReworkStatePath(worktree))
	if errors.Is(err, fs.ErrNotExist) {
		return ReworkState{}, nil
	}
	if err != nil {
		return ReworkState{}, fmt.Errorf("reading rework state: %w", err)
	}
	// json.Unmarshal of a literal `null` into a struct pointer is a documented
	// no-op — it leaves the zero value with a nil error, which would let a
	// worktree that already burned its rework rounds look un-exhausted again.
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return ReworkState{}, fmt.Errorf("decoding rework state: file contains null, not a valid state")
	}
	var s ReworkState
	if err := json.Unmarshal(data, &s); err != nil {
		return ReworkState{}, fmt.Errorf("decoding rework state: %w", err)
	}
	return s, nil
}

// WriteReworkState atomically records s for worktree. s is taken by pointer
// solely to avoid copying the struct at the call site; WriteReworkState does
// not mutate it.
func WriteReworkState(worktree string, s *ReworkState) error {
	path := ReworkStatePath(worktree)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("creating rework state dir: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding rework state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing rework state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("renaming rework state into place: %w", err)
	}
	return nil
}
