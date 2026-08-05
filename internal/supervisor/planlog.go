package supervisor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// planLogPath is where the PostToolUse recorder (`argus worker record-plan`)
// appends one line per real TodoWrite/TaskCreate/TaskUpdate call — under
// .claude/argus/, the same directory settingsFor already denies to a
// worker's own Edit/Write tool and DenyFloor denies to `argus worker
// record-plan` invoked directly from a worker's own Bash turn, so the log is
// as unfakeable as status.json.
func planLogPath(worktree string) string {
	return filepath.Join(worktree, ".claude", "argus", "plan-log.jsonl")
}

// planCheckpointPath is where AdvancePlanCheckpoint persists the point in
// time a gated transition last consumed plan-log evidence up to — under the
// same worker-denied directory as planLogPath.
func planCheckpointPath(worktree string) string {
	return filepath.Join(worktree, ".claude", "argus", "plan-checkpoint.json")
}

// planLogRecord is one line of plan-log.jsonl: a timestamped marker that a
// plan-related tool actually fired, independent of a worker's own
// self-reported Plan field on status.json.
type planLogRecord struct {
	Timestamp time.Time `json:"timestamp"`
	ToolName  string    `json:"tool_name"`
}

// AppendPlanLog appends one record for a TodoWrite/TaskCreate/TaskUpdate call
// against worktree at ts — called by `argus worker record-plan` on every
// matching PostToolUse hook invocation. ts is the caller's own clock
// (time.Now in production), never read here, so every timestamp in the log
// traces back to the one call site that reads the wall clock.
func AppendPlanLog(worktree, toolName string, ts time.Time) error {
	dir := filepath.Dir(planLogPath(worktree))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating plan log dir: %w", err)
	}
	f, err := os.OpenFile(planLogPath(worktree), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("opening plan log: %w", err)
	}
	defer func() { _ = f.Close() }()

	line, err := json.Marshal(planLogRecord{Timestamp: ts, ToolName: toolName})
	if err != nil {
		return fmt.Errorf("encoding plan log record: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("writing plan log record: %w", err)
	}
	return nil
}

// HasFreshPlanEvidence reports whether worktree's plan-log.jsonl carries any
// record after the worktree's current plan checkpoint (see
// AdvancePlanCheckpoint), and whether the log exists at all. logExists=false
// means no PostToolUse recorder ever wrote to this worktree — a worker
// spawned without argus's hooks (a foreign or headless run) — which callers
// must treat as "nothing to enforce" rather than "cheated," falling back to
// the worker's self-reported Plan field and the transcript-grep backstop
// instead (see runWorkerReport). Log present but fresh=false means a hooked
// run that has not called a plan tool since the last checkpoint was
// consumed — the actual cheat this closes.
func HasFreshPlanEvidence(worktree string) (fresh bool, logExists bool, err error) {
	f, err := os.Open(planLogPath(worktree))
	if err != nil {
		if os.IsNotExist(err) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("opening plan log: %w", err)
	}
	defer func() { _ = f.Close() }()

	checkpoint, err := loadPlanCheckpoint(worktree)
	if err != nil {
		return false, true, err
	}

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec planLogRecord
		if jsonErr := json.Unmarshal(sc.Bytes(), &rec); jsonErr != nil {
			continue // skip malformed lines rather than fail the whole scan
		}
		if rec.Timestamp.After(checkpoint) {
			return true, true, nil
		}
	}
	if err := sc.Err(); err != nil {
		return false, true, fmt.Errorf("scanning plan log: %w", err)
	}
	return false, true, nil
}

// planCheckpoint is planCheckpointPath's on-disk shape.
type planCheckpoint struct {
	AdvancedAt time.Time `json:"advanced_at"`
}

// loadPlanCheckpoint reads worktree's plan checkpoint, defaulting to the zero
// time when none has been recorded yet — so a worktree's very first gated
// transition treats every plan-log record written so far (e.g. planning's own
// TodoWrite call) as fresh.
func loadPlanCheckpoint(worktree string) (time.Time, error) {
	data, err := os.ReadFile(planCheckpointPath(worktree))
	if err != nil {
		if os.IsNotExist(err) {
			return time.Time{}, nil
		}
		return time.Time{}, fmt.Errorf("reading plan checkpoint: %w", err)
	}
	var cp planCheckpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return time.Time{}, fmt.Errorf("decoding plan checkpoint: %w", err)
	}
	return cp.AdvancedAt, nil
}

// AdvancePlanCheckpoint bumps worktree's plan checkpoint to now, so a later
// gated transition needs fresh plan-log activity recorded after this point,
// not evidence this transition already spent — the windowed half of "record
// live, enforce per phase."
func AdvancePlanCheckpoint(worktree string, now time.Time) error {
	dir := filepath.Dir(planCheckpointPath(worktree))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating plan checkpoint dir: %w", err)
	}
	data, err := json.Marshal(planCheckpoint{AdvancedAt: now})
	if err != nil {
		return fmt.Errorf("encoding plan checkpoint: %w", err)
	}
	if err := os.WriteFile(planCheckpointPath(worktree), data, 0o600); err != nil {
		return fmt.Errorf("writing plan checkpoint: %w", err)
	}
	return nil
}
