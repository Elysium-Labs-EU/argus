package protocol

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestApprovalRoundTrips(t *testing.T) {
	wt := t.TempDir()
	in := &Approval{
		Approved:  true,
		Source:    "review",
		Summary:   "looks correct",
		Reasons:   []string{"diff verified"},
		UpdatedAt: time.Now().Truncate(time.Second),
	}
	if err := WriteApproval(wt, in); err != nil {
		t.Fatalf("WriteApproval: %v", err)
	}
	got, found, err := LoadApproval(wt)
	if err != nil || !found {
		t.Fatalf("LoadApproval: found=%v err=%v", found, err)
	}
	if !got.Approved || got.Source != "review" || got.Summary != "looks correct" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestLoadApprovalMissingIsNotFound(t *testing.T) {
	_, found, err := LoadApproval(t.TempDir())
	if err != nil {
		t.Fatalf("missing verdict should not error: %v", err)
	}
	if found {
		t.Error("found should be false when no verdict was written")
	}
}

func TestWriteApprovalUnwritableDir(t *testing.T) {
	wt := t.TempDir()
	if err := os.Chmod(wt, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(wt, 0o700) }) // let t.TempDir's own cleanup remove it

	if err := WriteApproval(wt, &Approval{Approved: true}); err == nil {
		t.Fatal("want error writing verdict under a read-only worktree, got nil")
	}
}

// TestLoadApprovalReadErrorNotNotExist covers the read-failure branch that
// isn't fs.ErrNotExist (e.g. permission denied, or the path itself being a
// directory) — LoadApproval must still fail closed (found=false) but surface
// the error instead of swallowing it like the missing-file case does.
func TestLoadApprovalReadErrorNotNotExist(t *testing.T) {
	wt := t.TempDir()
	path := VerdictPath(wt)
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	approval, found, err := LoadApproval(wt)
	if err == nil {
		t.Fatal("want error reading a verdict path that is a directory, got nil")
	}
	if !strings.Contains(err.Error(), "reading verdict") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "reading verdict")
	}
	if found {
		t.Error("found should be false when the read fails")
	}
	if !reflect.DeepEqual(approval, Approval{}) {
		t.Errorf("approval = %+v, want zero value on read error", approval)
	}
}

// TestLoadApprovalNullContentFailsClosed pins the safe counterpart to
// rework's budget-exceeded state: a verdict.json literally containing "null"
// unmarshals into a zero Approval (Approved=false), so a corrupted-to-null
// verdict still reads as found (ship won't treat it as "never written") yet
// unapproved (ship still refuses to ship it).
func TestLoadApprovalNullContentFailsClosed(t *testing.T) {
	wt := t.TempDir()
	path := VerdictPath(wt)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("null"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	approval, found, err := LoadApproval(wt)
	if err != nil {
		t.Fatalf("LoadApproval: %v", err)
	}
	if !found {
		t.Error("found should be true — the verdict file exists and decoded")
	}
	if !reflect.DeepEqual(approval, Approval{}) {
		t.Errorf("approval = %+v, want zero value for null content", approval)
	}
	if approval.Approved {
		t.Error("Approved should be false for a null verdict — fail closed")
	}
}

// TestWriteApprovalWriteFileFails covers the WriteFile branch specifically:
// MkdirAll succeeds (the dir already exists) but the write to the .tmp path
// fails because a directory is already sitting there.
func TestWriteApprovalWriteFileFails(t *testing.T) {
	wt := t.TempDir()
	path := VerdictPath(wt)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.MkdirAll(path+".tmp", 0o750); err != nil {
		t.Fatalf("MkdirAll tmp: %v", err)
	}

	err := WriteApproval(wt, &Approval{Approved: true})
	if err == nil {
		t.Fatal("want error writing verdict when the .tmp path is a directory, got nil")
	}
	if !strings.Contains(err.Error(), "writing verdict") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "writing verdict")
	}
}

// TestWriteApprovalRenameFails covers the final os.Rename branch: MkdirAll and
// WriteFile both succeed, but the rename into place fails because the
// destination is already occupied by a directory (rename refuses to replace a
// directory with a regular file).
func TestWriteApprovalRenameFails(t *testing.T) {
	wt := t.TempDir()
	path := VerdictPath(wt)
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	err := WriteApproval(wt, &Approval{Approved: true})
	if err == nil {
		t.Fatal("want error renaming verdict when the destination is a directory, got nil")
	}
	if !strings.Contains(err.Error(), "renaming verdict into place") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "renaming verdict into place")
	}
}

func TestLoadApprovalMalformedJSON(t *testing.T) {
	wt := t.TempDir()
	path := VerdictPath(wt)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, _, err := LoadApproval(wt); err == nil {
		t.Fatal("want error decoding malformed verdict, got nil")
	}
}

// TestProvenanceClassifiesApproval pins the derivation both the supervise report
// and run_summary rely on: a rejected verdict always needs a human regardless of
// which source rejected it, a reviewer approve and a gate approve are told apart
// by Source, and only the awaiting-human case reports NeedsHumanRead. This is the
// signal that lets an operator skip re-reading an already-cleared diff.
func TestProvenanceClassifiesApproval(t *testing.T) {
	cases := []struct {
		name      string
		source    string
		want      Provenance
		approved  bool
		wantHuman bool
	}{
		{"rejected by reviewer", "review", ProvenanceAwaitingHuman, false, true},
		{"rejected/escalated by gate", "gate", ProvenanceAwaitingHuman, false, true},
		{"rejected with no source", "", ProvenanceAwaitingHuman, false, true},
		{"reviewer approved", "review", ProvenanceReviewerApproved, true, false},
		{"gate auto-approved", "gate", ProvenanceGateApproved, true, false},
		{"approved by any non-review source", "other", ProvenanceGateApproved, true, false},
		{"rework budget exceeded", SourceReworkBudget, ProvenanceReworkBudgetExceeded, false, true},
		{"approved despite rework-budget source", SourceReworkBudget, ProvenanceGateApproved, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := Approval{Approved: tc.approved, Source: tc.source}
			got := a.Provenance()
			if got != tc.want {
				t.Errorf("Provenance() = %q, want %q", got, tc.want)
			}
			if got.NeedsHumanRead() != tc.wantHuman {
				t.Errorf("NeedsHumanRead() = %v, want %v", got.NeedsHumanRead(), tc.wantHuman)
			}
		})
	}
}
