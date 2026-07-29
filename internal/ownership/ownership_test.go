package ownership

import (
	"errors"
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
