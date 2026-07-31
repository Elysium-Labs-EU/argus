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

// LifecycleState is a worktree's post-ship lifecycle stage, tracked by argus
// itself rather than self-reported by a worker. It is a separate axis from
// Phase: Phase tracks a worker's progress *inside* an active worktree; State
// tracks the worktree's own life *after* the worker's job is over, gated on
// facts only argus can check (has the PR it opened via ship since merged on
// the forge).
type LifecycleState string

const (
	// LifecycleActive is a worktree with no recorded ship yet, or the implicit
	// state of any worktree argus has never written a lifecycle.json for.
	LifecycleActive LifecycleState = "active"
	// LifecycleShipped is a worktree whose PR argus opened via ship, not yet
	// confirmed merged.
	LifecycleShipped LifecycleState = "shipped"
	// LifecycleMerged is a shipped worktree whose PR the forge reports merged.
	LifecycleMerged LifecycleState = "merged"
	// LifecyclePruned is a merged worktree argus has already cleaned up.
	LifecyclePruned LifecycleState = "pruned"
)

// Lifecycle is the per-worktree record argus writes once ship opens a pull
// request, so a later `argus worktree prune` can identify which forge PR to
// check without re-deriving it from the branch name — and so the decision
// "is this safe to clean up" is deterministic and testable instead of an ad
// hoc git/forge lookup fumbled by hand.
type Lifecycle struct {
	UpdatedAt time.Time      `json:"updated_at"`
	State     LifecycleState `json:"state"`
	Host      string         `json:"host"`
	Owner     string         `json:"owner"`
	Repo      string         `json:"repo"`
	Branch    string         `json:"branch"`
	PRURL     string         `json:"pr_url"`
	PRNumber  int            `json:"pr_number"`
	// JiraNotified marks that the --jira-issue post-ship comment already
	// landed for this worktree's PR. Jira has no FindPR-equivalent lookup a
	// retry could use to detect an already-posted comment, so this is the
	// only signal that tells a ship retry (e.g. after a crash between
	// WriteLifecycle and postShipJira) apart from a first attempt.
	JiraNotified bool `json:"jira_notified,omitempty"`
}

// LifecyclePath is where a worktree's Lifecycle record lives, alongside its
// status.json and verdict.json under the same argus control-plane directory
// (see ship.controlPlanePaths — never staged into a PR).
func LifecyclePath(worktree string) string {
	return filepath.Join(worktree, ".claude", "argus", "lifecycle.json")
}

// WriteLifecycle atomically records l for a worktree, stamping UpdatedAt
// itself the same way protocol.Write does for status — a caller never sets
// its own timestamp.
func WriteLifecycle(worktree string, l *Lifecycle) error {
	path := LifecyclePath(worktree)
	l.UpdatedAt = time.Now()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("creating lifecycle dir: %w", err)
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding lifecycle: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing lifecycle: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("renaming lifecycle into place: %w", err)
	}
	return nil
}

// LoadLifecycle reads a worktree's recorded lifecycle. found is false (with no
// error) when no lifecycle was ever written — a worktree ship never touched,
// or one shipped before this feature existed — which prune must treat as
// "unmanaged: derive PR state from the branch instead."
func LoadLifecycle(worktree string) (l Lifecycle, found bool, err error) {
	data, err := os.ReadFile(LifecyclePath(worktree))
	if errors.Is(err, fs.ErrNotExist) {
		return Lifecycle{}, false, nil
	}
	if err != nil {
		return Lifecycle{}, false, fmt.Errorf("reading lifecycle: %w", err)
	}
	if err := json.Unmarshal(data, &l); err != nil {
		return Lifecycle{}, false, fmt.Errorf("decoding lifecycle: %w", err)
	}
	return l, true, nil
}
