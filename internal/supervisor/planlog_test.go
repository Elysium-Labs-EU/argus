package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHasFreshPlanEvidenceNoLogFileDoesNotExist(t *testing.T) {
	wt := t.TempDir()
	fresh, logExists, err := HasFreshPlanEvidence(wt)
	if err != nil {
		t.Fatalf("HasFreshPlanEvidence: %v", err)
	}
	if logExists {
		t.Error("logExists = true, want false when plan-log.jsonl was never written")
	}
	if fresh {
		t.Error("fresh = true, want false when the log doesn't exist")
	}
}

func TestHasFreshPlanEvidenceFreshBeforeAnyCheckpoint(t *testing.T) {
	wt := t.TempDir()
	if err := AppendPlanLog(wt, "TodoWrite", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("AppendPlanLog: %v", err)
	}
	fresh, logExists, err := HasFreshPlanEvidence(wt)
	if err != nil {
		t.Fatalf("HasFreshPlanEvidence: %v", err)
	}
	if !logExists {
		t.Error("logExists = false, want true once AppendPlanLog wrote a record")
	}
	if !fresh {
		t.Error("fresh = false, want true — no checkpoint set yet, so any record counts as fresh")
	}
}

func TestHasFreshPlanEvidenceStaleAfterCheckpointAdvances(t *testing.T) {
	wt := t.TempDir()
	recordedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := AppendPlanLog(wt, "TodoWrite", recordedAt); err != nil {
		t.Fatalf("AppendPlanLog: %v", err)
	}
	// Advance the checkpoint to after the only record written so far — the
	// windowed-enforcement case: a gated transition already consumed this
	// evidence, so a later gated transition must not reuse it.
	if err := AdvancePlanCheckpoint(wt, recordedAt.Add(time.Minute)); err != nil {
		t.Fatalf("AdvancePlanCheckpoint: %v", err)
	}
	fresh, logExists, err := HasFreshPlanEvidence(wt)
	if err != nil {
		t.Fatalf("HasFreshPlanEvidence: %v", err)
	}
	if !logExists {
		t.Error("logExists = false, want true")
	}
	if fresh {
		t.Error("fresh = true, want false — the only record predates the checkpoint")
	}
}

func TestHasFreshPlanEvidenceFreshAfterNewRecordPastCheckpoint(t *testing.T) {
	wt := t.TempDir()
	first := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := AppendPlanLog(wt, "TodoWrite", first); err != nil {
		t.Fatalf("AppendPlanLog: %v", err)
	}
	if err := AdvancePlanCheckpoint(wt, first.Add(time.Minute)); err != nil {
		t.Fatalf("AdvancePlanCheckpoint: %v", err)
	}
	second := first.Add(2 * time.Minute)
	if err := AppendPlanLog(wt, "TaskUpdate", second); err != nil {
		t.Fatalf("AppendPlanLog: %v", err)
	}
	fresh, logExists, err := HasFreshPlanEvidence(wt)
	if err != nil {
		t.Fatalf("HasFreshPlanEvidence: %v", err)
	}
	if !logExists {
		t.Error("logExists = false, want true")
	}
	if !fresh {
		t.Error("fresh = false, want true — a new record was written after the checkpoint advanced")
	}
}

func TestAppendPlanLogAppendsMultipleRecords(t *testing.T) {
	wt := t.TempDir()
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	if err := AppendPlanLog(wt, "TodoWrite", t1); err != nil {
		t.Fatalf("AppendPlanLog 1: %v", err)
	}
	if err := AppendPlanLog(wt, "TaskCreate", t2); err != nil {
		t.Fatalf("AppendPlanLog 2: %v", err)
	}
	data, err := os.ReadFile(planLogPath(wt))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := 0
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	if lines != 2 {
		t.Errorf("plan-log.jsonl has %d lines, want 2", lines)
	}
}

func TestAppendPlanLogOpenErrorWhenPathIsADirectory(t *testing.T) {
	wt := t.TempDir()
	if err := os.MkdirAll(planLogPath(wt), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	err := AppendPlanLog(wt, "TodoWrite", time.Now())
	if err == nil {
		t.Fatal("AppendPlanLog err = nil, want error when plan-log.jsonl path is a directory")
	}
}

func TestHasFreshPlanEvidenceSkipsMalformedLines(t *testing.T) {
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(planLogPath(wt)), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "not json\n" + `{"timestamp":"2026-01-01T00:00:00Z","tool_name":"TodoWrite"}` + "\n"
	if err := os.WriteFile(planLogPath(wt), []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fresh, logExists, err := HasFreshPlanEvidence(wt)
	if err != nil {
		t.Fatalf("HasFreshPlanEvidence: %v", err)
	}
	if !logExists || !fresh {
		t.Errorf("fresh=%v logExists=%v, want true,true — the malformed line must be skipped, not fail the scan", fresh, logExists)
	}
}

func TestHasFreshPlanEvidenceCorruptCheckpointReturnsError(t *testing.T) {
	wt := t.TempDir()
	if err := AppendPlanLog(wt, "TodoWrite", time.Now()); err != nil {
		t.Fatalf("AppendPlanLog: %v", err)
	}
	if err := os.WriteFile(planCheckpointPath(wt), []byte("{bad json"), 0o600); err != nil {
		t.Fatalf("WriteFile checkpoint: %v", err)
	}
	_, logExists, err := HasFreshPlanEvidence(wt)
	if err == nil {
		t.Fatal("HasFreshPlanEvidence err = nil, want error for a corrupt plan-checkpoint.json")
	}
	if !logExists {
		t.Error("logExists = false, want true — the log itself opened fine, only the checkpoint is corrupt")
	}
}

func TestAdvancePlanCheckpointPersistsAndIsReloaded(t *testing.T) {
	wt := t.TempDir()
	stamp := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if err := AdvancePlanCheckpoint(wt, stamp); err != nil {
		t.Fatalf("AdvancePlanCheckpoint: %v", err)
	}
	got, err := loadPlanCheckpoint(wt)
	if err != nil {
		t.Fatalf("loadPlanCheckpoint: %v", err)
	}
	if !got.Equal(stamp) {
		t.Errorf("loadPlanCheckpoint = %v, want %v", got, stamp)
	}
}

func TestLoadPlanCheckpointDefaultsToZeroTimeWhenMissing(t *testing.T) {
	wt := t.TempDir()
	got, err := loadPlanCheckpoint(wt)
	if err != nil {
		t.Fatalf("loadPlanCheckpoint: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("loadPlanCheckpoint = %v, want the zero time when no checkpoint has been recorded", got)
	}
}

func TestAppendPlanLogMkdirFailsUnderReadOnlyWorktree(t *testing.T) {
	wt := t.TempDir()
	if err := os.Chmod(wt, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(wt, 0o700) })

	err := AppendPlanLog(wt, "TodoWrite", time.Now())
	if err == nil {
		t.Fatal("AppendPlanLog err = nil, want error creating .claude/argus under a read-only worktree")
	}
}

func TestAdvancePlanCheckpointMkdirFailsUnderReadOnlyWorktree(t *testing.T) {
	wt := t.TempDir()
	if err := os.Chmod(wt, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(wt, 0o700) })

	err := AdvancePlanCheckpoint(wt, time.Now())
	if err == nil {
		t.Fatal("AdvancePlanCheckpoint err = nil, want error creating .claude/argus under a read-only worktree")
	}
}

func TestAdvancePlanCheckpointWriteFailsUnderReadOnlyDir(t *testing.T) {
	wt := t.TempDir()
	dir := filepath.Dir(planCheckpointPath(wt))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := AdvancePlanCheckpoint(wt, time.Now())
	if err == nil {
		t.Fatal("AdvancePlanCheckpoint err = nil, want error writing plan-checkpoint.json into a read-only dir")
	}
}

// TestAppendPlanLogEncodeFails exercises the json.Marshal error path via
// time.Time's own MarshalJSON, which errors for years outside [0,9999] — the
// same fault-injection-free trick internal/protocol's TestWriteEncodeFails
// uses for the analogous branch in Write.
func TestAppendPlanLogEncodeFails(t *testing.T) {
	wt := t.TempDir()
	err := AppendPlanLog(wt, "TodoWrite", time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("want error encoding a record with an out-of-range year, got nil")
	}
	if !strings.Contains(err.Error(), "encoding plan log record") {
		t.Errorf("error = %q, want it to mention encoding plan log record", err)
	}
}

// TestAdvancePlanCheckpointEncodeFails is TestAppendPlanLogEncodeFails' analog
// for the checkpoint's own encoding branch.
func TestAdvancePlanCheckpointEncodeFails(t *testing.T) {
	wt := t.TempDir()
	err := AdvancePlanCheckpoint(wt, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("want error encoding a checkpoint with an out-of-range year, got nil")
	}
	if !strings.Contains(err.Error(), "encoding plan checkpoint") {
		t.Errorf("error = %q, want it to mention encoding plan checkpoint", err)
	}
}

// TestHasFreshPlanEvidenceOpenErrorWhenLogIsUnreadable forces os.Open's
// generic (non-IsNotExist) error branch: a file that exists but that this
// process has no read permission on, distinct from
// TestHasFreshPlanEvidenceNoLogFileDoesNotExist's IsNotExist branch.
func TestHasFreshPlanEvidenceOpenErrorWhenLogIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permission bits, this test can't force a permission error")
	}
	wt := t.TempDir()
	if err := AppendPlanLog(wt, "TodoWrite", time.Now()); err != nil {
		t.Fatalf("AppendPlanLog: %v", err)
	}
	if err := os.Chmod(planLogPath(wt), 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(planLogPath(wt), 0o600) })

	_, logExists, err := HasFreshPlanEvidence(wt)
	if err == nil {
		t.Fatal("HasFreshPlanEvidence err = nil, want a permission error opening an unreadable plan-log.jsonl")
	}
	if logExists {
		t.Error("logExists = true, want false alongside a real open error")
	}
	if !strings.Contains(err.Error(), "opening plan log") {
		t.Errorf("error = %q, want it to mention opening plan log", err)
	}
}

// TestLoadPlanCheckpointReadErrorWhenUnreadable is
// TestHasFreshPlanEvidenceOpenErrorWhenLogIsUnreadable's analog for the
// checkpoint file's own non-IsNotExist read-error branch.
func TestLoadPlanCheckpointReadErrorWhenUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permission bits, this test can't force a permission error")
	}
	wt := t.TempDir()
	if err := AdvancePlanCheckpoint(wt, time.Now()); err != nil {
		t.Fatalf("AdvancePlanCheckpoint: %v", err)
	}
	if err := os.Chmod(planCheckpointPath(wt), 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(planCheckpointPath(wt), 0o600) })

	_, err := loadPlanCheckpoint(wt)
	if err == nil {
		t.Fatal("loadPlanCheckpoint err = nil, want a permission error reading an unreadable plan-checkpoint.json")
	}
	if !strings.Contains(err.Error(), "reading plan checkpoint") {
		t.Errorf("error = %q, want it to mention reading plan checkpoint", err)
	}
}

// TestHasFreshPlanEvidenceScanErrorWhenLogIsADirectory forces the scan-error
// branch rather than the open-error one: os.Open succeeds on a directory (the
// error only surfaces once bufio.Scanner tries to read from it), so
// logExists is true right alongside the error, distinguishing this from the
// os.IsNotExist branch above.
func TestHasFreshPlanEvidenceScanErrorWhenLogIsADirectory(t *testing.T) {
	wt := t.TempDir()
	if err := os.MkdirAll(planLogPath(wt), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, logExists, err := HasFreshPlanEvidence(wt)
	if err == nil {
		t.Fatal("HasFreshPlanEvidence err = nil, want error when plan-log.jsonl path is a directory")
	}
	if !logExists {
		t.Error("logExists = false, want true — os.Open succeeds on a directory, only the scan fails")
	}
}
