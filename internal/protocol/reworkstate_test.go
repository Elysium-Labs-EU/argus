package protocol

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReworkStateRoundTrips(t *testing.T) {
	wt := t.TempDir()
	in := &ReworkState{
		RoundsAttempted: 2,
		UpdatedAt:       time.Now().Truncate(time.Second),
	}
	if err := WriteReworkState(wt, in); err != nil {
		t.Fatalf("WriteReworkState: %v", err)
	}
	got, err := LoadReworkState(wt)
	if err != nil {
		t.Fatalf("LoadReworkState: %v", err)
	}
	if got.RoundsAttempted != 2 {
		t.Errorf("RoundsAttempted = %d, want 2", got.RoundsAttempted)
	}
}

func TestLoadReworkStateMissingIsZeroValue(t *testing.T) {
	got, err := LoadReworkState(t.TempDir())
	if err != nil {
		t.Fatalf("missing rework state should not error: %v", err)
	}
	if got.RoundsAttempted != 0 {
		t.Errorf("RoundsAttempted = %d, want 0 for a worktree that never reworked", got.RoundsAttempted)
	}
}

func TestLoadReworkStateNullIsError(t *testing.T) {
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(ReworkStatePath(wt)), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(ReworkStatePath(wt), []byte("null"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := LoadReworkState(wt); err == nil {
		t.Fatal("LoadReworkState with a null-content file should error, not return a fresh zero budget")
	}
}

func TestReworkStateSurvivesStatusAndVerdictRemoval(t *testing.T) {
	// supervisor.InvalidateStatus removes status.json and verdict.json before
	// every rework round; a persisted budget count that lived in either file
	// would reset to zero every round, defeating the whole point of a
	// cross-invocation budget. This test replicates that removal directly
	// (protocol cannot import supervisor) to pin that ReworkState lives
	// elsewhere and survives it.
	wt := t.TempDir()
	if err := WriteReworkState(wt, &ReworkState{RoundsAttempted: 3}); err != nil {
		t.Fatalf("WriteReworkState: %v", err)
	}
	if err := Write(StatusPath(wt), &Status{}); err != nil {
		t.Fatalf("Write status: %v", err)
	}
	if err := WriteApproval(wt, &Approval{}); err != nil {
		t.Fatalf("WriteApproval: %v", err)
	}
	if err := os.Remove(StatusPath(wt)); err != nil {
		t.Fatalf("removing status.json: %v", err)
	}
	if err := os.Remove(VerdictPath(wt)); err != nil {
		t.Fatalf("removing verdict.json: %v", err)
	}
	got, err := LoadReworkState(wt)
	if err != nil {
		t.Fatalf("LoadReworkState: %v", err)
	}
	if got.RoundsAttempted != 3 {
		t.Errorf("RoundsAttempted = %d after status/verdict removal, want 3 (rework state must survive it)", got.RoundsAttempted)
	}
}

func TestLoadReworkStateMalformedJSON(t *testing.T) {
	wt := t.TempDir()
	path := ReworkStatePath(wt)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not valid"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := LoadReworkState(wt)
	if err == nil {
		t.Fatal("want error decoding malformed rework state file, got nil")
	}
	if !strings.Contains(err.Error(), "decoding rework state") {
		t.Errorf("err = %q, want it to mention %q", err.Error(), "decoding rework state")
	}
}

func TestLoadReworkStateUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission-denial tests can't force a read failure")
	}
	wt := t.TempDir()
	if err := WriteReworkState(wt, &ReworkState{RoundsAttempted: 1}); err != nil {
		t.Fatalf("setup WriteReworkState: %v", err)
	}
	path := ReworkStatePath(wt)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	_, err := LoadReworkState(wt)
	if err == nil {
		t.Fatal("want error reading a permission-denied rework state file, got nil")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want a wrapped read error, not ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), "reading rework state") {
		t.Errorf("err = %q, want it to mention %q", err.Error(), "reading rework state")
	}
}

func TestWriteReworkStateUnwritableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission-denial tests can't force a mkdir failure")
	}
	wt := t.TempDir()
	if err := os.Chmod(wt, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(wt, 0o700) }) // let t.TempDir's own cleanup remove it

	err := WriteReworkState(wt, &ReworkState{RoundsAttempted: 1})
	if err == nil {
		t.Fatal("want error writing rework state under a read-only worktree, got nil")
	}
	if !strings.Contains(err.Error(), "creating rework state dir") {
		t.Errorf("err = %q, want it to mention %q", err.Error(), "creating rework state dir")
	}
}

func TestWriteReworkStateUnwritableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission-denial tests can't force a write failure")
	}
	wt := t.TempDir()
	// MkdirAll succeeds against an already-existing dir regardless of its
	// permission bits, so this exercises WriteFile's own failure path rather
	// than MkdirAll's.
	dir := filepath.Dir(ReworkStatePath(wt))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("setup MkdirAll: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })

	err := WriteReworkState(wt, &ReworkState{RoundsAttempted: 1})
	if err == nil {
		t.Fatal("want error writing rework state file into a read-only dir, got nil")
	}
	if !strings.Contains(err.Error(), "writing rework state") {
		t.Errorf("err = %q, want it to mention %q", err.Error(), "writing rework state")
	}
}

func TestWriteReworkStateRenameOverDir(t *testing.T) {
	wt := t.TempDir()
	path := ReworkStatePath(wt)
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatalf("setup MkdirAll: %v", err)
	}

	err := WriteReworkState(wt, &ReworkState{RoundsAttempted: 1})
	if err == nil {
		t.Fatal("want error renaming rework state over an existing directory, got nil")
	}
	if !strings.Contains(err.Error(), "renaming rework state into place") {
		t.Errorf("err = %q, want it to mention %q", err.Error(), "renaming rework state into place")
	}
}

func TestWriteReworkStateParentIsFile(t *testing.T) {
	wt := t.TempDir()
	// ReworkStatePath descends through .claude/argus; blocking that
	// intermediate path with a plain file forces MkdirAll itself to fail,
	// distinct from the permission-denied case above.
	if err := os.WriteFile(filepath.Join(wt, ".claude"), []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("setup WriteFile: %v", err)
	}
	err := WriteReworkState(wt, &ReworkState{RoundsAttempted: 1})
	if err == nil {
		t.Fatal("want error when the rework state dir path is blocked by a file, got nil")
	}
	if !strings.Contains(err.Error(), "creating rework state dir") {
		t.Errorf("err = %q, want it to mention %q", err.Error(), "creating rework state dir")
	}
}
