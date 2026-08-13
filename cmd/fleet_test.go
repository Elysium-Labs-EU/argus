package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/ownership"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
)

func TestFleetCmdEmptyRepoPrintsNoWorktrees(t *testing.T) {
	repo := gitRepo(t)
	cmd := newFleetCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--repo", repo})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("fleet: %v", err)
	}
	if !strings.Contains(buf.String(), "no worktrees linked") {
		t.Errorf("empty fleet should say so:\n%s", buf.String())
	}
}

func TestFleetCmdTablePrintsPhaseAndBranch(t *testing.T) {
	_, worktree := repoWithWorktree(t, "feat-fleet")
	if err := protocol.Write(protocol.StatusPath(worktree), &protocol.Status{Phase: protocol.PhaseWorking, Title: "add widget"}); err != nil {
		t.Fatal(err)
	}

	cmd := newFleetCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--repo", worktree})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("fleet: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "feat-fleet") || !strings.Contains(out, "working") || !strings.Contains(out, "add widget") {
		t.Errorf("fleet table missing expected columns:\n%s", out)
	}
	if !strings.Contains(out, "AGE") {
		t.Errorf("fleet table missing AGE column header:\n%s", out)
	}
}

func TestFleetCmdJSONEmitsEnvelope(t *testing.T) {
	_, worktree := repoWithWorktree(t, "feat-json")
	if err := protocol.Write(protocol.StatusPath(worktree), &protocol.Status{Phase: protocol.PhaseAwaitingReview}); err != nil {
		t.Fatal(err)
	}

	cmd := newFleetCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--repo", worktree, "--json", "--owner", "test-owner"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("fleet --json: %v", err)
	}

	var envelope fleetEnvelope
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding fleet --json output: %v\n%s", err, buf.String())
	}
	if envelope.Scope != "mine" {
		t.Errorf("scope = %q, want mine", envelope.Scope)
	}
	if envelope.ControllerID != "test-owner" {
		t.Errorf("controller_id = %q, want test-owner", envelope.ControllerID)
	}
	if envelope.GeneratedAt.IsZero() {
		t.Error("generated_at should be set")
	}
	if envelope.Count != 1 {
		t.Fatalf("count = %d, want 1", envelope.Count)
	}
	if len(envelope.Worktrees) != 1 {
		t.Fatalf("want 1 row, got %d", len(envelope.Worktrees))
	}
	if envelope.Worktrees[0].Branch != "feat-json" {
		t.Errorf("branch = %q, want feat-json", envelope.Worktrees[0].Branch)
	}
	if envelope.Worktrees[0].StatusFile != supervisor.FileOK || envelope.Worktrees[0].Status.Phase != protocol.PhaseAwaitingReview {
		t.Errorf("status = %+v", envelope.Worktrees[0])
	}
	if envelope.Worktrees[0].VerdictFile != supervisor.FileAbsent {
		t.Errorf("verdict file = %q, want absent (no verdict.json written)", envelope.Worktrees[0].VerdictFile)
	}
}

// TestFleetCmdScopesToOwnerByDefault proves the default view excludes a
// foreign-owned worktree and counts it, while --all restores it.
func TestFleetCmdScopesToOwnerByDefault(t *testing.T) {
	repoRoot, mine := repoWithWorktree(t, "feat-mine")
	if err := protocol.Write(protocol.StatusPath(mine), &protocol.Status{Phase: protocol.PhaseWorking}); err != nil {
		t.Fatal(err)
	}
	if err := ownership.Spawn(mine, "test-owner", "me", time.Now()); err != nil {
		t.Fatal(err)
	}

	foreign := repoWithWorktreeIn(t, repoRoot, "feat-foreign")
	if err := protocol.Write(protocol.StatusPath(foreign), &protocol.Status{Phase: protocol.PhaseWorking}); err != nil {
		t.Fatal(err)
	}
	if err := ownership.Spawn(foreign, "someone-else", "them", time.Now()); err != nil {
		t.Fatal(err)
	}

	cmd := newFleetCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--repo", repoRoot, "--json", "--owner", "test-owner"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("fleet --json: %v", err)
	}
	var envelope fleetEnvelope
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding: %v\n%s", err, buf.String())
	}
	if envelope.Count != 1 || envelope.Worktrees[0].Branch != "feat-mine" {
		t.Errorf("default scope should keep only feat-mine, got %+v", envelope.Worktrees)
	}
	if envelope.ExcludedForeignCount != 1 {
		t.Errorf("excluded_foreign_count = %d, want 1", envelope.ExcludedForeignCount)
	}

	cmd = newFleetCmd()
	buf.Reset()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--repo", repoRoot, "--json", "--owner", "test-owner", "--all"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("fleet --json --all: %v", err)
	}
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding: %v\n%s", err, buf.String())
	}
	if envelope.Scope != "all" || envelope.Count != 2 || envelope.ExcludedForeignCount != 0 {
		t.Errorf("--all should restore both worktrees with no exclusions: %+v", envelope)
	}
}

// TestFleetCmdHidesIdleByDefault proves an idle (no status.json) worktree is
// hidden by default and counted, and restored by --include-idle.
func TestFleetCmdHidesIdleByDefault(t *testing.T) {
	repoRoot, touched := repoWithWorktree(t, "feat-touched")
	if err := protocol.Write(protocol.StatusPath(touched), &protocol.Status{Phase: protocol.PhaseWorking}); err != nil {
		t.Fatal(err)
	}
	repoWithWorktreeIn(t, repoRoot, "feat-idle") // never reports, stays idle

	cmd := newFleetCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--repo", repoRoot, "--json", "--all"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("fleet --json --all: %v", err)
	}
	var envelope fleetEnvelope
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding: %v\n%s", err, buf.String())
	}
	if envelope.Count != 1 || envelope.Worktrees[0].Branch != "feat-touched" {
		t.Errorf("default scope should hide the idle worktree, got %+v", envelope.Worktrees)
	}
	if envelope.ExcludedIdleCount != 1 {
		t.Errorf("excluded_idle_count = %d, want 1", envelope.ExcludedIdleCount)
	}

	cmd = newFleetCmd()
	buf.Reset()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--repo", repoRoot, "--json", "--all", "--include-idle"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("fleet --json --all --include-idle: %v", err)
	}
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding: %v\n%s", err, buf.String())
	}
	if envelope.Count != 2 || envelope.ExcludedIdleCount != 0 {
		t.Errorf("--include-idle should restore the idle worktree: %+v", envelope)
	}
}

// TestFleetCmdTitleFallsBackToLifecycle proves a shipped worktree whose
// worker never self-titled still shows a non-empty title, taken from
// lifecycle.json (see cmd/ship.go's shipChange).
func TestFleetCmdTitleFallsBackToLifecycle(t *testing.T) {
	_, worktree := repoWithWorktree(t, "feat-shipped")
	if err := protocol.Write(protocol.StatusPath(worktree), &protocol.Status{Phase: protocol.PhaseDone}); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteLifecycle(worktree, &protocol.Lifecycle{State: protocol.LifecycleShipped, Title: "feat: resolved PR title"}); err != nil {
		t.Fatal(err)
	}

	cmd := newFleetCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--repo", worktree, "--all", "--include-idle"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("fleet: %v", err)
	}
	if !strings.Contains(buf.String(), "feat: resolved PR title") {
		t.Errorf("fleet table should fall back to the lifecycle title:\n%s", buf.String())
	}
}

func TestFleetCmdRejectsNonGitRepo(t *testing.T) {
	cmd := newFleetCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--repo", t.TempDir()})
	if err := cmd.Execute(); err == nil {
		t.Fatal("fleet against a non-git directory should error")
	}
}

func TestFleetCmdRejectsMissingRepo(t *testing.T) {
	cmd := newFleetCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--repo", "/nonexistent/path/for/fleet"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("fleet against a nonexistent --repo should error")
	}
}

func TestFleetFieldAndCellHelpers(t *testing.T) {
	if got := orDash(""); got != "-" {
		t.Errorf("orDash(\"\") = %q, want -", got)
	}
	if got := orDash("x"); got != "x" {
		t.Errorf("orDash(x) = %q, want x", got)
	}
	if got := fleetField(supervisor.FileUnreadable, "anything"); got != "unreadable" {
		t.Errorf("fleetField(unreadable) = %q, want unreadable", got)
	}
	if got := fleetField(supervisor.FileAbsent, ""); got != "-" {
		t.Errorf("fleetField(absent, \"\") = %q, want -", got)
	}
	if got := fleetField(supervisor.FileOK, "planning"); got != "planning" {
		t.Errorf("fleetField(ok, planning) = %q, want planning", got)
	}

	approved := &supervisor.FleetRow{VerdictFile: supervisor.FileOK, Verdict: protocol.Approval{Approved: true}}
	if got := approvedCell(approved); got != "y" {
		t.Errorf("approvedCell(approved) = %q, want y", got)
	}
	rejected := &supervisor.FleetRow{VerdictFile: supervisor.FileOK, Verdict: protocol.Approval{Approved: false}}
	if got := approvedCell(rejected); got != "n" {
		t.Errorf("approvedCell(rejected) = %q, want n", got)
	}
	unreadableVerdict := &supervisor.FleetRow{VerdictFile: supervisor.FileUnreadable}
	if got := approvedCell(unreadableVerdict); got != "unreadable" {
		t.Errorf("approvedCell(unreadable) = %q, want unreadable", got)
	}
	absentVerdict := &supervisor.FleetRow{VerdictFile: supervisor.FileAbsent}
	if got := approvedCell(absentVerdict); got != "-" {
		t.Errorf("approvedCell(absent) = %q, want -", got)
	}

	withPR := &supervisor.FleetRow{LifecycleFile: supervisor.FileOK, Lifecycle: protocol.Lifecycle{PRNumber: 7}}
	if got := prCell(withPR); got != "#7" {
		t.Errorf("prCell(with PR) = %q, want #7", got)
	}
	withPRURL := &supervisor.FleetRow{LifecycleFile: supervisor.FileOK, Lifecycle: protocol.Lifecycle{PRNumber: 7, PRURL: "https://example.com/pr/7"}}
	if got := prCell(withPRURL); got != "https://example.com/pr/7" {
		t.Errorf("prCell(with PR URL) = %q, want the full URL", got)
	}
	noPR := &supervisor.FleetRow{LifecycleFile: supervisor.FileOK, Lifecycle: protocol.Lifecycle{PRNumber: 0}}
	if got := prCell(noPR); got != "-" {
		t.Errorf("prCell(no PR) = %q, want -", got)
	}
	unreadableLifecycle := &supervisor.FleetRow{LifecycleFile: supervisor.FileUnreadable}
	if got := prCell(unreadableLifecycle); got != "unreadable" {
		t.Errorf("prCell(unreadable) = %q, want unreadable", got)
	}

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	unreadableOwner := &supervisor.FleetRow{OwnerFile: supervisor.FileUnreadable}
	if got := heartbeatCell(unreadableOwner); got != "unreadable" {
		t.Errorf("heartbeatCell(unreadable) = %q, want unreadable", got)
	}
	absentOwner := &supervisor.FleetRow{OwnerFile: supervisor.FileAbsent}
	if got := heartbeatCell(absentOwner); got != "-" {
		t.Errorf("heartbeatCell(absent) = %q, want -", got)
	}
	okOwner := &supervisor.FleetRow{OwnerFile: supervisor.FileOK, Owner: ownership.Owner{HeartbeatAt: now.Add(-90 * time.Second)}, HeartbeatAge: 90 * time.Second}
	if got := heartbeatCell(okOwner); !strings.HasSuffix(got, " ago") {
		t.Errorf("heartbeatCell(ok) = %q, want a duration suffixed with \" ago\"", got)
	}
	zeroHeartbeatOwner := &supervisor.FleetRow{OwnerFile: supervisor.FileOK, HeartbeatAge: 50 * 365 * 24 * time.Hour}
	if got := heartbeatCell(zeroHeartbeatOwner); got != "-" {
		t.Errorf("heartbeatCell(zero heartbeat_at) = %q, want -", got)
	}

	unreadableStatusForAge := &supervisor.FleetRow{StatusFile: supervisor.FileUnreadable}
	if got := phaseAgeCell(unreadableStatusForAge, time.Now()); got != "unreadable" {
		t.Errorf("phaseAgeCell(unreadable) = %q, want unreadable", got)
	}
	absentStatusForAge := &supervisor.FleetRow{StatusFile: supervisor.FileAbsent}
	if got := phaseAgeCell(absentStatusForAge, time.Now()); got != "-" {
		t.Errorf("phaseAgeCell(absent) = %q, want -", got)
	}
	okStatusForAge := &supervisor.FleetRow{StatusFile: supervisor.FileOK, Status: protocol.Status{UpdatedAt: now.Add(-90 * time.Second)}}
	if got := phaseAgeCell(okStatusForAge, now); got != "1m30s ago" {
		t.Errorf("phaseAgeCell(ok) = %q, want 1m30s ago", got)
	}
	zeroUpdatedAtStatus := &supervisor.FleetRow{StatusFile: supervisor.FileOK}
	if got := phaseAgeCell(zeroUpdatedAtStatus, now); got != "-" {
		t.Errorf("phaseAgeCell(zero updated_at) = %q, want -", got)
	}

	unreadableStatusForTitle := &supervisor.FleetRow{StatusFile: supervisor.FileUnreadable}
	if got := titleCell(unreadableStatusForTitle); got != "unreadable" {
		t.Errorf("titleCell(unreadable status) = %q, want unreadable", got)
	}
	statusTitleWins := &supervisor.FleetRow{StatusFile: supervisor.FileOK, Status: protocol.Status{Title: "worker title"}, LifecycleFile: supervisor.FileOK, Lifecycle: protocol.Lifecycle{Title: "lifecycle title"}}
	if got := titleCell(statusTitleWins); got != "worker title" {
		t.Errorf("titleCell should prefer status.Title = %q, want worker title", got)
	}
	fallsBackToLifecycle := &supervisor.FleetRow{StatusFile: supervisor.FileOK, LifecycleFile: supervisor.FileOK, Lifecycle: protocol.Lifecycle{Title: "lifecycle title"}}
	if got := titleCell(fallsBackToLifecycle); got != "lifecycle title" {
		t.Errorf("titleCell should fall back to lifecycle.Title = %q, want lifecycle title", got)
	}
	neitherTitle := &supervisor.FleetRow{StatusFile: supervisor.FileOK}
	if got := titleCell(neitherTitle); got != "-" {
		t.Errorf("titleCell(neither) = %q, want -", got)
	}
}

// failingWriter always errors, so it exercises runFleet's --json path when
// the encoder itself can't write — otherwise unreachable through a normal
// bytes.Buffer sink.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write refused") }

// TestRunFleetPropagatesBuildFleetError exercises runFleet's own error
// branch after ResolveWorktree/RepoRoot both already succeeded — not
// reachable through a real repo, since a RepoRoot that resolves at all
// means `git worktree list` will too.
func TestRunFleetPropagatesBuildFleetError(t *testing.T) {
	repo := gitRepo(t)
	original := buildFleet
	buildFleet = func(context.Context, string, time.Time) ([]supervisor.FleetRow, error) {
		return nil, errors.New("boom")
	}
	t.Cleanup(func() { buildFleet = original })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runFleet(cmd, &fleetArgs{repo: repo}); err == nil {
		t.Fatal("want BuildFleet's error to propagate")
	}
}

func TestRunFleetPropagatesJSONEncodeError(t *testing.T) {
	repo := gitRepo(t)
	cmd := &cobra.Command{}
	cmd.SetOut(failingWriter{})
	cmd.SetContext(context.Background())
	if err := runFleet(cmd, &fleetArgs{repo: repo, jsonOut: true}); err == nil {
		t.Fatal("want an error when the json encoder can't write its output")
	}
}
