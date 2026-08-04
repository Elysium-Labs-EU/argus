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
}

func TestFleetCmdJSONEmitsStructuredRows(t *testing.T) {
	_, worktree := repoWithWorktree(t, "feat-json")
	if err := protocol.Write(protocol.StatusPath(worktree), &protocol.Status{Phase: protocol.PhaseAwaitingReview}); err != nil {
		t.Fatal(err)
	}

	cmd := newFleetCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--repo", worktree, "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("fleet --json: %v", err)
	}

	var rows []supervisor.FleetRow
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("decoding fleet --json output: %v\n%s", err, buf.String())
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].Branch != "feat-json" {
		t.Errorf("branch = %q, want feat-json", rows[0].Branch)
	}
	if rows[0].StatusFile != supervisor.FileOK || rows[0].Status.Phase != protocol.PhaseAwaitingReview {
		t.Errorf("status = %+v", rows[0])
	}
	if rows[0].VerdictFile != supervisor.FileAbsent {
		t.Errorf("verdict file = %q, want absent (no verdict.json written)", rows[0].VerdictFile)
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
	noPR := &supervisor.FleetRow{LifecycleFile: supervisor.FileOK, Lifecycle: protocol.Lifecycle{PRNumber: 0}}
	if got := prCell(noPR); got != "-" {
		t.Errorf("prCell(no PR) = %q, want -", got)
	}
	unreadableLifecycle := &supervisor.FleetRow{LifecycleFile: supervisor.FileUnreadable}
	if got := prCell(unreadableLifecycle); got != "unreadable" {
		t.Errorf("prCell(unreadable) = %q, want unreadable", got)
	}

	unreadableOwner := &supervisor.FleetRow{OwnerFile: supervisor.FileUnreadable}
	if got := heartbeatCell(unreadableOwner); got != "unreadable" {
		t.Errorf("heartbeatCell(unreadable) = %q, want unreadable", got)
	}
	absentOwner := &supervisor.FleetRow{OwnerFile: supervisor.FileAbsent}
	if got := heartbeatCell(absentOwner); got != "-" {
		t.Errorf("heartbeatCell(absent) = %q, want -", got)
	}
	okOwner := &supervisor.FleetRow{OwnerFile: supervisor.FileOK, HeartbeatAge: 90 * time.Second}
	if got := heartbeatCell(okOwner); !strings.HasSuffix(got, " ago") {
		t.Errorf("heartbeatCell(ok) = %q, want a duration suffixed with \" ago\"", got)
	}
}

// failingWriter always errors, so it exercises runFleet's --json path when
// the encoder itself can't write — otherwise unreachable through a normal
// bytes.Buffer sink.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write refused") }

func TestRunFleetPropagatesJSONEncodeError(t *testing.T) {
	repo := gitRepo(t)
	cmd := &cobra.Command{}
	cmd.SetOut(failingWriter{})
	cmd.SetContext(context.Background())
	if err := runFleet(cmd, repo, true); err == nil {
		t.Fatal("want an error when the json encoder can't write its output")
	}
}
