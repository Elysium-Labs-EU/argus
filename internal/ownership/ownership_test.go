package ownership

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

func TestWriteLoadRoundTrips(t *testing.T) {
	wt := t.TempDir()
	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	in := &Owner{OwnerID: "sess-1", OwnerLabel: "host-a (pid 123)", SpawnedAt: now, HeartbeatAt: now}
	if err := Write(wt, in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, found, err := Load(wt)
	if err != nil || !found {
		t.Fatalf("Load: found=%v err=%v", found, err)
	}
	if got.OwnerID != in.OwnerID || got.OwnerLabel != in.OwnerLabel || !got.SpawnedAt.Equal(now) || !got.HeartbeatAt.Equal(now) {
		t.Errorf("round-trip mismatch: %+v, want %+v", got, in)
	}
}

func TestLoadMissingIsNotFound(t *testing.T) {
	o, found, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("missing owner.json should not error: %v", err)
	}
	if found {
		t.Error("found should be false when no owner.json was written")
	}
	if o != (Owner{}) {
		t.Errorf("o = %+v, want zero value when not found", o)
	}
}

func TestSpawnSetsBothTimestamps(t *testing.T) {
	wt := t.TempDir()
	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	if err := Spawn(wt, "sess-1", "host-a (pid 1)", now); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	got, found, err := Load(wt)
	if err != nil || !found {
		t.Fatalf("Load: found=%v err=%v", found, err)
	}
	if !got.SpawnedAt.Equal(now) || !got.HeartbeatAt.Equal(now) {
		t.Errorf("Spawn timestamps = %+v, want both %v", got, now)
	}
}

func TestHeartbeatAdvancesHeartbeatButNotSpawnedAt(t *testing.T) {
	wt := t.TempDir()
	spawned := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	if err := Spawn(wt, "sess-1", "host-a (pid 1)", spawned); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	later := spawned.Add(15 * time.Second)
	if err := Heartbeat(wt, later); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	got, found, err := Load(wt)
	if err != nil || !found {
		t.Fatalf("Load: found=%v err=%v", found, err)
	}
	if !got.SpawnedAt.Equal(spawned) {
		t.Errorf("SpawnedAt = %v, want it unchanged at %v", got.SpawnedAt, spawned)
	}
	if !got.HeartbeatAt.Equal(later) {
		t.Errorf("HeartbeatAt = %v, want advanced to %v", got.HeartbeatAt, later)
	}
}

func TestHeartbeatNoOpsWhenMissing(t *testing.T) {
	wt := t.TempDir()
	if err := Heartbeat(wt, time.Now()); err != nil {
		t.Fatalf("Heartbeat on a worktree with no lease should not error, got: %v", err)
	}
	if _, found, _ := Load(wt); found {
		t.Error("Heartbeat should not create a lease where none existed")
	}
}

func TestIsOwner(t *testing.T) {
	o := Owner{OwnerID: "sess-1"}
	if !IsOwner(&o, "sess-1") {
		t.Error("IsOwner should match the same owner_id")
	}
	if IsOwner(&o, "sess-2") {
		t.Error("IsOwner should not match a different owner_id")
	}
}

func TestStale(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	o := Owner{HeartbeatAt: now.Add(-31 * time.Minute)}
	if !Stale(&o, now, 30*time.Minute) {
		t.Error("want a 31-minute-old heartbeat stale against a 30-minute threshold")
	}
	fresh := Owner{HeartbeatAt: now.Add(-29 * time.Minute)}
	if Stale(&fresh, now, 30*time.Minute) {
		t.Error("want a 29-minute-old heartbeat not yet stale against a 30-minute threshold")
	}
}

func TestResolveOwnerIDPrecedence(t *testing.T) {
	if got := ResolveOwnerID("explicit"); got != "explicit" {
		t.Errorf("ResolveOwnerID with a flag value = %q, want it to win outright", got)
	}

	t.Setenv("ARGUS_OWNER_ID", "from-env")
	t.Setenv("HERDR_WORKSPACE_ID", "from-workspace")
	if got := ResolveOwnerID(""); got != "from-env" {
		t.Errorf("ResolveOwnerID with no flag = %q, want $ARGUS_OWNER_ID to win over $HERDR_WORKSPACE_ID", got)
	}

	t.Setenv("ARGUS_OWNER_ID", "")
	if got := ResolveOwnerID(""); got != "from-workspace" {
		t.Errorf("ResolveOwnerID with no flag/$ARGUS_OWNER_ID = %q, want $HERDR_WORKSPACE_ID", got)
	}

	t.Setenv("HERDR_WORKSPACE_ID", "")
	got1 := ResolveOwnerID("")
	got2 := ResolveOwnerID("")
	if got1 == "" || got2 == "" {
		t.Fatalf("ResolveOwnerID with nothing set should generate a non-empty id, got %q and %q", got1, got2)
	}
	if got1 == got2 {
		t.Error("ResolveOwnerID with nothing set should generate a fresh id each call, got the same value twice")
	}
}

func TestEnforceSameOwnerProceeds(t *testing.T) {
	wt := t.TempDir()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	if err := Spawn(wt, "sess-1", "host-a (pid 1)", now); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	notice, err := Enforce(wt, "sess-1", now, DefaultStaleAfter, false)
	if err != nil {
		t.Fatalf("Enforce for the owning caller should not refuse, got: %v", err)
	}
	if notice != "" {
		t.Errorf("notice = %q, want empty for the owning caller", notice)
	}
}

func TestEnforceMismatchRefuses(t *testing.T) {
	wt := t.TempDir()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	if err := Spawn(wt, "sess-1", "host-a (pid 1)", now); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	notice, err := Enforce(wt, "sess-2", now, DefaultStaleAfter, false)
	if err == nil {
		t.Fatal("want a mismatched, still-fresh lease to refuse, got nil error")
	}
	uerr, ok := errors.AsType[*ui.UserError](err)
	if !ok {
		t.Errorf("err = %v (%T), want a *ui.UserError", err, err)
	} else if got := uerr.Error(); got == "" {
		t.Error("want a non-empty refusal message")
	}
	if notice != "" {
		t.Errorf("notice = %q, want empty on refusal", notice)
	}
}

func TestEnforceMismatchWithForceProceeds(t *testing.T) {
	wt := t.TempDir()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	if err := Spawn(wt, "sess-1", "host-a (pid 1)", now); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	notice, err := Enforce(wt, "sess-2", now, DefaultStaleAfter, true)
	if err != nil {
		t.Fatalf("--force-foreign-owner should override a mismatch, got: %v", err)
	}
	if notice != "" {
		t.Errorf("notice = %q, want empty when force overrides", notice)
	}
}

func TestEnforceStaleLeaseDoesNotRefuse(t *testing.T) {
	wt := t.TempDir()
	spawned := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	if err := Spawn(wt, "sess-1", "host-a (pid 1)", spawned); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	now := spawned.Add(31 * time.Minute)
	notice, err := Enforce(wt, "sess-2", now, 30*time.Minute, false)
	if err != nil {
		t.Fatalf("a stale lease should not refuse a mismatched caller, got: %v", err)
	}
	if notice == "" {
		t.Error("want a non-empty notice for a mismatched but stale lease")
	}
}

func TestEnforceMissingOwnerFileTreatedAsUnowned(t *testing.T) {
	wt := t.TempDir()
	notice, err := Enforce(wt, "anyone", time.Now(), DefaultStaleAfter, false)
	if err != nil {
		t.Fatalf("a worktree with no owner.json should never refuse, got: %v", err)
	}
	if notice != "" {
		t.Errorf("notice = %q, want empty for a worktree with no recorded lease", notice)
	}
}

func TestEnforcePropagatesLoadError(t *testing.T) {
	wt := t.TempDir()
	writeMalformedOwnerFile(t, wt)
	notice, err := Enforce(wt, "sess-1", time.Now(), DefaultStaleAfter, false)
	if err == nil {
		t.Fatal("want Enforce to propagate a Load decode error")
	}
	if notice != "" {
		t.Errorf("notice = %q, want empty when Load fails", notice)
	}
}

func TestHeartbeatPropagatesLoadError(t *testing.T) {
	wt := t.TempDir()
	writeMalformedOwnerFile(t, wt)
	if err := Heartbeat(wt, time.Now()); err == nil {
		t.Fatal("want Heartbeat to propagate a Load decode error")
	}
}

func TestDefaultOwnerLabelFormat(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	want := fmt.Sprintf("%s (pid %d)", host, os.Getpid())
	if got := DefaultOwnerLabel(); got != want {
		t.Errorf("DefaultOwnerLabel() = %q, want %q", got, want)
	}
}

func TestWriteMkdirAllError(t *testing.T) {
	wt := t.TempDir()
	// A regular file at .claude blocks MkdirAll from creating .claude/argus beneath it.
	if err := os.WriteFile(filepath.Join(wt, ".claude"), []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := Write(wt, &Owner{OwnerID: "sess-1"}); err == nil {
		t.Fatal("want an error when .claude exists as a file blocking the owner dir")
	}
}

func TestWriteUnwritableDirError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission checks")
	}
	wt := t.TempDir()
	dir := filepath.Dir(Path(wt))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })
	if err := Write(wt, &Owner{OwnerID: "sess-1"}); err == nil {
		t.Fatal("want an error writing into a read-only owner lease dir")
	}
}

func TestWriteRenameOntoDirectoryError(t *testing.T) {
	wt := t.TempDir()
	// Rename onto an existing directory fails, unlike renaming onto an existing file.
	if err := os.MkdirAll(Path(wt), 0o750); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := Write(wt, &Owner{OwnerID: "sess-1"}); err == nil {
		t.Fatal("want an error renaming the tmp file onto an existing directory")
	}
}

func TestLoadUnreadableFileError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permission checks")
	}
	wt := t.TempDir()
	if err := Spawn(wt, "sess-1", "host-a (pid 1)", time.Now()); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.Chmod(Path(wt), 0o000); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(Path(wt), 0o600) })
	if _, _, err := Load(wt); err == nil {
		t.Fatal("want an error reading a permission-denied owner.json")
	}
}

func TestLoadMalformedJSONError(t *testing.T) {
	wt := t.TempDir()
	writeMalformedOwnerFile(t, wt)
	_, found, err := Load(wt)
	if err == nil {
		t.Fatal("want a decode error for malformed owner.json")
	}
	if found {
		t.Error("found should be false when decode fails")
	}
}

func writeMalformedOwnerFile(t *testing.T, worktree string) {
	t.Helper()
	path := Path(worktree)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
}
