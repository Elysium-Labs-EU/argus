package protocol

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLifecycleRoundTrips(t *testing.T) {
	wt := t.TempDir()
	in := &Lifecycle{
		State: LifecycleShipped, Host: "codeberg.org", Owner: "o", Repo: "r",
		Branch: "feat-x", PRURL: "https://codeberg.org/o/r/pulls/7", PRNumber: 7,
	}
	if err := WriteLifecycle(wt, in); err != nil {
		t.Fatalf("WriteLifecycle: %v", err)
	}
	got, found, err := LoadLifecycle(wt)
	if err != nil || !found {
		t.Fatalf("LoadLifecycle: found=%v err=%v", found, err)
	}
	if got.State != LifecycleShipped || got.PRNumber != 7 || got.PRURL != in.PRURL {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("WriteLifecycle should stamp UpdatedAt")
	}
}

func TestLoadLifecycleMissingIsNotFound(t *testing.T) {
	_, found, err := LoadLifecycle(t.TempDir())
	if err != nil {
		t.Fatalf("missing lifecycle should not error: %v", err)
	}
	if found {
		t.Error("found should be false when no lifecycle was written")
	}
}

func TestWriteLifecycleUnwritableDir(t *testing.T) {
	wt := t.TempDir()
	if err := os.Chmod(wt, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(wt, 0o700) }) // let t.TempDir's own cleanup remove it

	if err := WriteLifecycle(wt, &Lifecycle{State: LifecycleShipped}); err == nil {
		t.Fatal("want error writing lifecycle under a read-only worktree, got nil")
	}
}

func TestLoadLifecycleMalformedJSON(t *testing.T) {
	wt := t.TempDir()
	path := LifecyclePath(wt)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, _, err := LoadLifecycle(wt); err == nil {
		t.Fatal("want error decoding malformed lifecycle, got nil")
	}
}

func TestLoadLifecycleUnreadablePath(t *testing.T) {
	wt := t.TempDir()
	path := LifecyclePath(wt)
	// A directory at the lifecycle path makes os.ReadFile fail with an
	// error distinct from fs.ErrNotExist, exercising the "reading
	// lifecycle" wrap branch rather than the not-found branch.
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	l, found, err := LoadLifecycle(wt)
	if err == nil {
		t.Fatal("want error reading an unreadable lifecycle path, got nil")
	}
	if found {
		t.Error("found should be false on a read error")
	}
	if l != (Lifecycle{}) {
		t.Errorf("want zero Lifecycle on error, got %+v", l)
	}
}

func TestLoadLifecycleLiteralNull(t *testing.T) {
	wt := t.TempDir()
	path := LifecyclePath(wt)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("null"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	l, found, err := LoadLifecycle(wt)
	if err != nil {
		t.Fatalf("literal null should decode without error: %v", err)
	}
	if !found {
		t.Error("found should be true for an existing, decodable literal null")
	}
	if l != (Lifecycle{}) {
		t.Errorf("want zero Lifecycle for literal null, got %+v", l)
	}
}

func TestWriteLifecycleWriteFileFails(t *testing.T) {
	wt := t.TempDir()
	dir := filepath.Dir(LifecyclePath(wt))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Pre-create the dir read+exec only, so the later MkdirAll in
	// WriteLifecycle is a no-op (dir already exists) but the WriteFile
	// into it fails, isolating the "writing lifecycle" branch from the
	// "creating lifecycle dir" one.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })

	err := WriteLifecycle(wt, &Lifecycle{State: LifecycleShipped})
	if err == nil {
		t.Fatal("want error writing lifecycle into a read-only dir, got nil")
	}
}

func TestWriteLifecycleRenameFails(t *testing.T) {
	wt := t.TempDir()
	path := LifecyclePath(wt)
	// A pre-existing directory at the destination path lets MkdirAll and
	// WriteFile(tmp) both succeed, so only the final os.Rename(tmp, path)
	// fails — isolating the "renaming lifecycle into place" branch.
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	err := WriteLifecycle(wt, &Lifecycle{State: LifecycleShipped})
	if err == nil {
		t.Fatal("want error renaming lifecycle over an existing directory, got nil")
	}
}

func TestWriteLifecycleOverwritesCallerUpdatedAt(t *testing.T) {
	wt := t.TempDir()
	stale := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	in := &Lifecycle{State: LifecycleShipped, UpdatedAt: stale}

	if err := WriteLifecycle(wt, in); err != nil {
		t.Fatalf("WriteLifecycle: %v", err)
	}
	if in.UpdatedAt.Equal(stale) {
		t.Error("WriteLifecycle should overwrite a caller-set UpdatedAt in place")
	}

	got, found, err := LoadLifecycle(wt)
	if err != nil || !found {
		t.Fatalf("LoadLifecycle: found=%v err=%v", found, err)
	}
	if got.UpdatedAt.Equal(stale) {
		t.Error("persisted UpdatedAt should not be the caller-set stale value")
	}
}
