package protocol

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestExtraAllowPath(t *testing.T) {
	want := filepath.Join("/repo/wt", ".claude", "argus", "extra_allow.json")
	if got := ExtraAllowPath("/repo/wt"); got != want {
		t.Errorf("ExtraAllowPath = %q, want %q", got, want)
	}
}

func TestSaveLoadExtraAllowRoundTrip(t *testing.T) {
	wt := t.TempDir()
	want := []string{"Bash(task *)", "Bash(npm *)"}
	if err := SaveExtraAllow(wt, want); err != nil {
		t.Fatalf("SaveExtraAllow: %v", err)
	}
	got, err := LoadExtraAllow(wt)
	if err != nil {
		t.Fatalf("LoadExtraAllow: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("LoadExtraAllow = %v, want %v", got, want)
	}
}

func TestLoadExtraAllowMissingFileReturnsNilNoError(t *testing.T) {
	got, err := LoadExtraAllow(t.TempDir())
	if err != nil {
		t.Fatalf("LoadExtraAllow: %v", err)
	}
	if got != nil {
		t.Errorf("LoadExtraAllow with no file = %v, want nil", got)
	}
}

func TestSaveExtraAllowEmptyStillWritesFile(t *testing.T) {
	wt := t.TempDir()
	if err := SaveExtraAllow(wt, nil); err != nil {
		t.Fatalf("SaveExtraAllow: %v", err)
	}
	got, err := LoadExtraAllow(wt)
	if err != nil {
		t.Fatalf("LoadExtraAllow: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("LoadExtraAllow after saving nil = %v, want empty", got)
	}
}

func TestSaveExtraAllowWrapsMkdirAllError(t *testing.T) {
	wt := t.TempDir()
	// A regular file at .claude blocks MkdirAll from creating .claude/argus.
	if err := os.WriteFile(filepath.Join(wt, ".claude"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seeding blocking file: %v", err)
	}
	err := SaveExtraAllow(wt, []string{"x"})
	if err == nil {
		t.Fatal("want error when extra-allow dir can't be created, got nil")
	}
	if !strings.Contains(err.Error(), "creating extra-allow dir") {
		t.Errorf("error should be wrapped with context, got: %v", err)
	}
}

func TestSaveExtraAllowWrapsWriteFileError(t *testing.T) {
	wt := t.TempDir()
	// A directory at the target path itself blocks WriteFile, while MkdirAll
	// of its already-existing parent still succeeds.
	if err := os.MkdirAll(ExtraAllowPath(wt), 0o750); err != nil {
		t.Fatalf("seeding blocking dir: %v", err)
	}
	err := SaveExtraAllow(wt, []string{"x"})
	if err == nil {
		t.Fatal("want error when extra_allow.json can't be written, got nil")
	}
	if !strings.Contains(err.Error(), "writing extra allow") {
		t.Errorf("error should be wrapped with context, got: %v", err)
	}
}

func TestLoadExtraAllowReadErrorOtherThanNotExist(t *testing.T) {
	wt := t.TempDir()
	// A directory at the target path makes os.ReadFile fail with something
	// other than fs.ErrNotExist ("is a directory"), the wrapped-error branch
	// distinct from the missing-file fail-open path.
	if err := os.MkdirAll(ExtraAllowPath(wt), 0o750); err != nil {
		t.Fatalf("seeding blocking dir: %v", err)
	}
	if _, err := LoadExtraAllow(wt); err == nil {
		t.Error("want an error when the path is a directory, not a missing-file fail-open")
	}
}

func TestLoadExtraAllowMalformedFile(t *testing.T) {
	wt := t.TempDir()
	if err := SaveExtraAllow(wt, []string{"x"}); err != nil {
		t.Fatalf("SaveExtraAllow: %v", err)
	}
	if err := os.WriteFile(ExtraAllowPath(wt), []byte("not json"), 0o600); err != nil {
		t.Fatalf("seeding malformed file: %v", err)
	}
	if _, err := LoadExtraAllow(wt); err == nil {
		t.Error("LoadExtraAllow with malformed JSON should error")
	}
}
