package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

// setBinaryLookPath swaps the package-level exec.LookPath seam for the duration
// of a test, restoring it afterward — the command-level analog of how doctor's
// tests inject a lookPath stub through doctorArgs. found/notFound are reused
// from doctor_test.go.
func setBinaryLookPath(t *testing.T, f func(string) (string, error)) {
	t.Helper()
	orig := binaryLookPath
	binaryLookPath = f
	t.Cleanup(func() { binaryLookPath = orig })
}

func TestRequireBinariesAllPresent(t *testing.T) {
	if err := requireBinaries(found, binHerdr, binClaude); err != nil {
		t.Fatalf("all binaries present must return nil, got %v", err)
	}
	if err := requireBinaries(found); err != nil {
		t.Fatalf("no binaries requested must return nil, got %v", err)
	}
}

func TestRequireBinariesReportsFirstMissingWithHint(t *testing.T) {
	// herdr listed first: with everything missing, its error and hint win.
	err := requireBinaries(notFound, binHerdr, binClaude)
	if err == nil {
		t.Fatal("want an error when a required binary is missing")
	}
	if !strings.Contains(err.Error(), "herdr not found on PATH") {
		t.Errorf("want the herdr-not-found message, got %q", err.Error())
	}
	var uerr *ui.UserError
	if !errors.As(err, &uerr) {
		t.Fatalf("want a *ui.UserError carrying a hint, got %T", err)
	}
	if uerr.Hint != installHints[binHerdr] {
		t.Errorf("want the centralized herdr install hint, got %q", uerr.Hint)
	}
}

func TestRequireBinariesClaudeMissingHint(t *testing.T) {
	// herdr present, claude missing: the claude hint must be the one surfaced,
	// proving the error is keyed to the actual missing binary, not the first.
	lookPath := func(name string) (string, error) {
		if name == binHerdr {
			return "/usr/local/bin/herdr", nil
		}
		return notFound(name)
	}
	err := requireBinaries(lookPath, binHerdr, binClaude)
	if err == nil || !strings.Contains(err.Error(), "claude not found on PATH") {
		t.Fatalf("want a claude-not-found error, got %v", err)
	}
	var uerr *ui.UserError
	if !errors.As(err, &uerr) {
		t.Fatalf("want a *ui.UserError, got %T", err)
	}
	if uerr.Hint != installHints[binClaude] {
		t.Errorf("want the centralized claude install hint, got %q", uerr.Hint)
	}
}

func TestRequireBinariesNilLookPathUsesRealPath(t *testing.T) {
	// nil lookPath falls back to the real exec.LookPath; a name that cannot
	// exist proves the fallback ran and reported it missing.
	if err := requireBinaries(nil, "argus-prereq-definitely-not-a-real-binary"); err == nil {
		t.Fatal("want the real PATH lookup to report a nonexistent binary missing")
	}
}

func TestSuperviseRequiredBinaries(t *testing.T) {
	cases := []struct {
		name           string
		want           []string
		dryRun, attach bool
		review         bool
	}{
		{name: "dry run needs nothing", dryRun: true, want: nil},
		{name: "spawn needs herdr and claude", want: []string{binHerdr, binClaude}},
		{name: "attach needs only herdr", attach: true, want: []string{binHerdr}},
		{name: "attach with review also needs claude", attach: true, review: true, want: []string{binHerdr, binClaude}},
		{name: "spawn with review still both", review: true, want: []string{binHerdr, binClaude}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := superviseRequiredBinaries(tc.dryRun, tc.attach, tc.review)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("superviseRequiredBinaries(%v,%v,%v) = %v, want %v", tc.dryRun, tc.attach, tc.review, got, tc.want)
			}
		})
	}
}

// --- per-command wiring: present proceeds past the check, missing fails fast ---

func TestReviewMissingClaudeFailsFast(t *testing.T) {
	setBinaryLookPath(t, notFound)
	cmd := newReviewCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--worktree", t.TempDir()})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "claude not found on PATH") {
		t.Fatalf("want a claude-not-found error before any diffing, got %v", err)
	}
}

func TestReviewPresentClaudeProceedsPastCheck(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setBinaryLookPath(t, found)
	cmd := newReviewCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	// No --worktree: the claude check passes, so control reaches runReview,
	// which then reports the missing worktree — proving the check let it past.
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "no worktree given") {
		t.Fatalf("want the check to pass and runReview to report the missing worktree, got %v", err)
	}
}

func TestReworkMissingBinaryFailsFast(t *testing.T) {
	setBinaryLookPath(t, notFound)
	cmd := newReworkCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--worktree", t.TempDir()})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "herdr not found on PATH") {
		t.Fatalf("want a herdr-not-found error before dispatch, got %v", err)
	}
}

func TestReworkPresentBinaryProceedsPastCheck(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setBinaryLookPath(t, found)
	cmd := newReworkCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "no worktree given") {
		t.Fatalf("want the check to pass and runRework to report the missing worktree, got %v", err)
	}
}

func TestReworkDryRunSkipsBinaryCheck(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setBinaryLookPath(t, notFound)
	cmd := newReworkCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	// A dry run dispatches nothing, so a missing binary must not block it: the
	// run gets past the check and instead fails for the real reason (no verdict
	// in this bare temp dir).
	cmd.SetArgs([]string{"--worktree", t.TempDir(), "--dry-run"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("want an error for a dir with no verdict")
	}
	if strings.Contains(err.Error(), "not found on PATH") {
		t.Fatalf("a dry run must skip the binary check, got %v", err)
	}
}

func TestSuperviseMissingBinaryFailsFast(t *testing.T) {
	setBinaryLookPath(t, notFound)
	cmd := newSuperviseCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--tasks", "x", "--branches", "feat-x", "--repo", t.TempDir()})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "herdr not found on PATH") {
		t.Fatalf("want a herdr-not-found error before any worktree is created, got %v", err)
	}
}

func TestSupervisePresentBinaryProceedsPastCheck(t *testing.T) {
	setBinaryLookPath(t, found)
	cmd := newSuperviseCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	// No workers: the check passes, so control reaches spawnWorkers, which
	// reports the missing worker source — proving the check let it past.
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "no workers given") {
		t.Fatalf("want the check to pass and supervise to report no workers, got %v", err)
	}
}

func TestSuperviseDryRunSkipsBinaryCheck(t *testing.T) {
	setBinaryLookPath(t, notFound)
	cmd := newSuperviseCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	// A dry run needs no binary; with none present it must still get past the
	// check and fail only for the real reason (no workers here).
	cmd.SetArgs([]string{"--dry-run"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("want an error for a dry run with no workers")
	}
	if strings.Contains(err.Error(), "not found on PATH") {
		t.Fatalf("a dry run must skip the binary check, got %v", err)
	}
}

func TestRunRebaseMissingBinaryFailsBeforeDispatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repoDir := filepath.Join(t.TempDir(), "featx")
	initGitDirAt(t, repoDir)
	setBinaryLookPath(t, notFound)

	cmd := newRebaseCmd()
	cmd.SetContext(context.Background())
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	// A client that fails on any call proves the check fires before the worker
	// is ever dispatched into a pane.
	client := herdr.NewWithRunner(func(context.Context, ...string) ([]byte, error) {
		return nil, fmt.Errorf("herdr must not be reached before the prerequisite check")
	})
	err := runRebase(cmd, client, &rebaseOpts{worktree: repoDir, base: "main", force: true, interval: 10 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "herdr not found on PATH") {
		t.Fatalf("want a herdr-not-found error before dispatch, got %v", err)
	}
}

// TestRunRebaseNoWorkerPathSkipsBinaryCheck proves the check does not sit up
// front: the no-conflict, origin-behind fast path force-pushes directly with no
// worker, so it must succeed even with neither binary on PATH.
func TestRunRebaseNoWorkerPathSkipsBinaryCheck(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	wt := setupNoConflictOriginBehind(t)
	setBinaryLookPath(t, notFound)

	cmd := newRebaseCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--worktree", wt, "--base", "main"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("the no-worker direct-push path must not require any binary, got %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "pushed to origin") {
		t.Errorf("expected the direct-push success message, got:\n%s", buf.String())
	}
}
