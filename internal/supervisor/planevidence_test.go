package supervisor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

// planEvidenceTestWorktree is the fixed worktree path every HasPlanEvidence
// test below encodes into a transcript directory name.
const planEvidenceTestWorktree = "/repo/.claude/worktrees/fix-issue-103"

// writePlanTranscript creates
// home/.claude/projects/<encoded planEvidenceTestWorktree>/<name>.jsonl with
// the given content, mirroring the directory layout Claude Code itself writes
// session transcripts under.
func writePlanTranscript(t *testing.T, home, name, content string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", projectPathReplacer.Replace(planEvidenceTestWorktree))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".jsonl"), []byte(content), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
}

func TestHasPlanEvidenceFindsTodoWriteToolCall(t *testing.T) {
	home := t.TempDir()
	writePlanTranscript(t, home, "session-1",
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"TodoWrite","input":{}}]}}`+"\n")

	ok, err := HasPlanEvidence(home, planEvidenceTestWorktree)
	if err != nil {
		t.Fatalf("HasPlanEvidence: %v", err)
	}
	if !ok {
		t.Error("HasPlanEvidence = false, want true for a transcript with a real TodoWrite tool call")
	}
}

func TestHasPlanEvidenceFindsTaskCreateToolCall(t *testing.T) {
	home := t.TempDir()
	writePlanTranscript(t, home, "session-1",
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"TaskCreate","input":{}}]}}`+"\n")

	ok, err := HasPlanEvidence(home, planEvidenceTestWorktree)
	if err != nil {
		t.Fatalf("HasPlanEvidence: %v", err)
	}
	if !ok {
		t.Error("HasPlanEvidence = false, want true for a transcript with a real TaskCreate tool call")
	}
}

func TestHasPlanEvidenceFalseWhenNoMatchingToolCall(t *testing.T) {
	home := t.TempDir()
	// A transcript exists but never calls a todo/task tool — the exact issue
	// #103 case: a worker's brief said to write a todo list, and it didn't.
	writePlanTranscript(t, home, "session-1",
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{}}]}}`+"\n")

	ok, err := HasPlanEvidence(home, planEvidenceTestWorktree)
	if err != nil {
		t.Fatalf("HasPlanEvidence: %v", err)
	}
	if ok {
		t.Error("HasPlanEvidence = true, want false when no transcript line mentions TodoWrite/TaskCreate")
	}
}

func TestHasPlanEvidenceFalseWhenNoTranscriptDirectory(t *testing.T) {
	home := t.TempDir()
	ok, err := HasPlanEvidence(home, "/repo/.claude/worktrees/never-ran")
	if err != nil {
		t.Fatalf("HasPlanEvidence: %v", err)
	}
	if ok {
		t.Error("HasPlanEvidence = true, want false when the project's transcript directory doesn't exist")
	}
}

func TestHasPlanEvidenceChecksEveryTranscriptForTheWorktree(t *testing.T) {
	// A worktree can span more than one worker session (e.g. an initial
	// implementation run and a later review-feedback follow-up) — the earlier
	// session carries the evidence, a later one doesn't, and either must count.
	home := t.TempDir()
	writePlanTranscript(t, home, "session-1",
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{}}]}}`+"\n")
	writePlanTranscript(t, home, "session-2",
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"TodoWrite","input":{}}]}}`+"\n")

	ok, err := HasPlanEvidence(home, planEvidenceTestWorktree)
	if err != nil {
		t.Fatalf("HasPlanEvidence: %v", err)
	}
	if !ok {
		t.Error("HasPlanEvidence = false, want true when any transcript for the worktree has the evidence")
	}
}

func TestGateEscalatesWhenPlanEvidenceMissing(t *testing.T) {
	st := &workerState{
		hasFile:         true,
		measuredOK:      true,
		measured:        protocol.DiffStat{Files: 1, Insertions: 3},
		measuredFiles:   []string{"cmd/root.go"},
		planEvidenceOK:  true,
		hasPlanEvidence: false,
		plan:            &WorkerPlan{Worker: Worker{Task: "no-todo-list"}},
		status: protocol.Status{
			Phase: protocol.PhaseAwaitingReview,
			Plan:  []string{"claims a plan"}, // self-reported, but no matching tool call
			Tests: []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultPass}},
		},
	}
	v := gateVerdict(st, nil)
	if v.AutoApprove {
		t.Fatal("gate must not auto-approve a worker with no plan/todo evidence in its transcript")
	}
	if !hasReasonContaining(v.Reasons, "no TodoWrite/TaskCreate tool call") {
		t.Errorf("expected a missing-plan-evidence reason, got %v", v.Reasons)
	}
}

func TestGateEscalatesWhenPlanEvidenceUnverifiable(t *testing.T) {
	st := &workerState{
		hasFile:         true,
		measuredOK:      true,
		measured:        protocol.DiffStat{Files: 1, Insertions: 3},
		measuredFiles:   []string{"cmd/root.go"},
		planEvidenceErr: os.ErrPermission,
		plan:            &WorkerPlan{Worker: Worker{Task: "unreadable-transcript"}},
		status: protocol.Status{
			Phase: protocol.PhaseAwaitingReview,
			Tests: []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultPass}},
		},
	}
	v := gateVerdict(st, nil)
	if v.AutoApprove {
		t.Fatal("gate must not auto-approve when it could not verify plan evidence")
	}
	if !hasReasonContaining(v.Reasons, "could not verify plan evidence") {
		t.Errorf("expected an unverifiable reason, got %v", v.Reasons)
	}
}

func TestGateDoesNotRequirePlanEvidenceCheckForNonTerminalPhase(t *testing.T) {
	// planEvidenceOK/hasPlanEvidence are both zero-valued (unchecked) here —
	// mirroring how the diff checks stay silent until measuredOK is set, the
	// plan-evidence check must not fire for a worker still mid-flight.
	st := &workerState{
		hasFile: true,
		plan:    &WorkerPlan{Worker: Worker{Task: "still-working"}},
		status:  protocol.Status{Phase: protocol.PhaseWorking},
	}
	v := gateVerdict(st, nil)
	if hasReasonContaining(v.Reasons, "plan evidence") || hasReasonContaining(v.Reasons, "TodoWrite") {
		t.Errorf("plan-evidence check must not run for a non-terminal phase, got %v", v.Reasons)
	}
}
