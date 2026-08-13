package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
)

func fixedNow(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// gitLinkedWorktree makes a temp main repo with one commit and a branch, then
// `git worktree add`s a second directory linked to it — the shape supervise
// itself creates for a worker (see internal/repoconfig), and the only shape
// VerifyLinkedWorktree accepts.
func gitLinkedWorktree(t *testing.T) string {
	t.Helper()
	main := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run(main, "init", "-q", "-b", "main")
	run(main, "config", "user.email", "t@t")
	run(main, "config", "user.name", "t")
	run(main, "commit", "-q", "--allow-empty", "-m", "seed")
	run(main, "branch", "feat-x")

	linked := filepath.Join(t.TempDir(), "feat-x")
	run(main, "worktree", "add", "-q", linked, "feat-x")
	return linked
}

// gitPlainRepo makes a temp directory that is a git repository but not a
// linked worktree — the main checkout itself, which VerifyLinkedWorktree
// must also reject.
func gitPlainRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	return dir
}

func TestRunWorkerReportRejectsIllegalTransition(t *testing.T) {
	wt := t.TempDir()
	// No prior status.json: cur resolves as planning (see runWorkerReport's
	// os.ErrNotExist branch), so only planning/working are legal.
	err := runWorkerReport(wt, protocol.PhaseAwaitingReview, &protocol.Status{}, fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error for an illegal first transition, got nil")
	}
	if !strings.Contains(err.Error(), "illegal status transition") {
		t.Errorf("error = %q, want it to mention the illegal transition", err.Error())
	}
	if _, loadErr := protocol.Load(protocol.StatusPath(wt)); loadErr == nil {
		t.Error("status.json should not have been written for a rejected transition")
	}
}

// TestRunWorkerReportMissingStatusResolvesAsPlanning is the acceptance test
// for eliminating Phase(""): a worker's very first report, against a
// worktree with no status.json at all, is legal exactly when it would be
// legal from an explicit planning report — the self-loop planning ->
// planning, or (once plan evidence exists) planning -> working — and illegal
// otherwise, the same as any other phase's own transition rules.
func TestRunWorkerReportMissingStatusResolvesAsPlanning(t *testing.T) {
	wt := t.TempDir()
	// planning -> planning (self-loop) is legal from a resolved-as-planning
	// missing file, same as from a real planning report.
	if err := runWorkerReport(wt, protocol.PhasePlanning, &protocol.Status{Task: "t"}, fixedNow(time.Now())); err != nil {
		t.Fatalf("first planning report against a missing status.json rejected: %v", err)
	}

	wt2 := t.TempDir()
	// planning -> working is illegal without plan evidence — a missing file
	// carries no Plan, so RequiresPlanEvidence must still fire exactly as it
	// would for an explicit empty planning report.
	err := runWorkerReport(wt2, protocol.PhaseWorking, &protocol.Status{Task: "t"}, fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error moving straight to working from a missing status.json with no plan evidence, got nil")
	}
	if !strings.Contains(err.Error(), "no plan/todo evidence") {
		t.Errorf("error = %q, want it to mention missing plan evidence", err.Error())
	}
}

func TestRunWorkerReportRejectsCorruptStatus(t *testing.T) {
	wt := t.TempDir()
	path := protocol.StatusPath(wt)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("{bad json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// A worker actually in awaiting_review reports planning; a corrupt
	// status.json must not be silently treated as "hasn't reported yet"
	// (cur.Phase == "") and let this through as a legal "" -> planning move.
	err := runWorkerReport(wt, protocol.PhasePlanning, &protocol.Status{Task: "t"}, fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error for a corrupt status.json, got nil")
	}

	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile: %v", readErr)
	}
	if string(got) != "{bad json" {
		t.Errorf("status.json was overwritten despite the load error: %q", got)
	}
}

func TestRunWorkerReportRejectsDone(t *testing.T) {
	wt := t.TempDir()
	seed := protocol.Status{Phase: protocol.PhaseAwaitingReview}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	if err := runWorkerReport(wt, protocol.PhaseDone, &protocol.Status{}, fixedNow(time.Now())); err == nil {
		t.Fatal("want an error reporting done, got nil")
	}
}

func TestRunWorkerReportStampsArgusClockNotCallers(t *testing.T) {
	wt := t.TempDir()
	callerClaimed := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	argusNow := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

	body := protocol.Status{
		UpdatedAt: callerClaimed,
		Task:      "argus#92",
		Branch:    "fix-issue-92",
	}
	if err := runWorkerReport(wt, protocol.PhasePlanning, &body, fixedNow(argusNow)); err != nil {
		t.Fatalf("legal transition rejected: %v", err)
	}
	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.UpdatedAt.Equal(argusNow) {
		t.Errorf("UpdatedAt = %v, want argus's clock %v (caller-claimed %v must be ignored)", got.UpdatedAt, argusNow, callerClaimed)
	}
	if got.Phase != protocol.PhasePlanning {
		t.Errorf("Phase = %q, want %q", got.Phase, protocol.PhasePlanning)
	}
	if got.Task != "argus#92" || got.Branch != "fix-issue-92" {
		t.Errorf("rest of the body was not persisted: %+v", got)
	}
}

func TestRunWorkerReportRejectsWorkingWithoutPlanEvidence(t *testing.T) {
	wt := t.TempDir()
	// Planning report on file has no plan array — the real-world case of a
	// worker that reported the planning phase but never actually wrote a
	// todo list.
	seed := protocol.Status{Phase: protocol.PhasePlanning}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	err := runWorkerReport(wt, protocol.PhaseWorking, &protocol.Status{}, fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error moving planning -> working with no plan evidence, got nil")
	}
	if !strings.Contains(err.Error(), "no plan/todo evidence") {
		t.Errorf("error = %q, want it to mention missing plan evidence", err.Error())
	}
	got, loadErr := protocol.Load(protocol.StatusPath(wt))
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if got.Phase != protocol.PhasePlanning {
		t.Errorf("status.json Phase = %q, want unchanged planning (rejected transition must not persist)", got.Phase)
	}
}

func TestRunWorkerReportAcceptsWorkingWithPlanEvidence(t *testing.T) {
	wt := t.TempDir()
	seed := protocol.Status{Phase: protocol.PhasePlanning, Plan: []string{"read the brief", "write the fix"}}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	if err := runWorkerReport(wt, protocol.PhaseWorking, &protocol.Status{}, fixedNow(time.Now())); err != nil {
		t.Fatalf("legal transition with plan evidence rejected: %v", err)
	}
	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Phase != protocol.PhaseWorking {
		t.Errorf("Phase = %q, want working", got.Phase)
	}
}

// TestRunWorkerReportRecoversFromEmptyPlanningViaSelfLoop pins the documented
// recovery flow from an empty first planning report: working is rejected for
// lack of plan evidence, and planning -> planning must stay legal so a worker
// has a way to refile the same phase with a filled-in plan instead of being
// stuck with no legal move at all.
func TestRunWorkerReportRecoversFromEmptyPlanningViaSelfLoop(t *testing.T) {
	wt := t.TempDir()
	if err := runWorkerReport(wt, protocol.PhasePlanning, &protocol.Status{Task: "t", Branch: "b"}, fixedNow(time.Now())); err != nil {
		t.Fatalf("first planning report rejected: %v", err)
	}
	if err := runWorkerReport(wt, protocol.PhaseWorking, &protocol.Status{Task: "t", Branch: "b"}, fixedNow(time.Now())); err == nil {
		t.Fatal("want working rejected with no plan evidence, got nil")
	}
	if err := runWorkerReport(wt, protocol.PhasePlanning, &protocol.Status{Task: "t", Branch: "b", Plan: []string{"x"}}, fixedNow(time.Now())); err != nil {
		t.Fatalf("planning self-loop with a filled-in plan rejected: %v", err)
	}
	if err := runWorkerReport(wt, protocol.PhaseWorking, &protocol.Status{Task: "t", Branch: "b"}, fixedNow(time.Now())); err != nil {
		t.Fatalf("working rejected even after plan evidence was refiled: %v", err)
	}
	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Phase != protocol.PhaseWorking {
		t.Errorf("final phase = %q, want working", got.Phase)
	}
}

func TestRunWorkerReportFullLegalSequence(t *testing.T) {
	wt := t.TempDir()
	now := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	seq := []protocol.Phase{
		protocol.PhasePlanning,
		protocol.PhaseWorking,
		protocol.PhaseSelfTest,
		protocol.PhaseAwaitingReview,
	}
	for _, phase := range seq {
		now = now.Add(time.Minute)
		body := &protocol.Status{Task: "t", Branch: "b"}
		if phase == protocol.PhasePlanning {
			body.Plan = []string{"read the brief", "write the fix"}
		}
		if err := runWorkerReport(wt, phase, body, fixedNow(now)); err != nil {
			t.Fatalf("transition to %q rejected: %v", phase, err)
		}
	}
	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Phase != protocol.PhaseAwaitingReview {
		t.Errorf("final phase = %q, want awaiting_review", got.Phase)
	}
}

// TestRunWorkerReportPreservesBaseAcrossReports pins the carry-forward fix in
// runWorkerReport: Base is set once by supervise (never by the worker), and a
// worker's own report body has no "base" key at all, so every subsequent
// report must not clobber it back to empty.
func TestRunWorkerReportPreservesBaseAcrossReports(t *testing.T) {
	wt := t.TempDir()
	// Base-only, Phase: planning mirrors the real pre-dispatch bookkeeping
	// write provisionWorktree makes before a worker's first report (see
	// internal/supervisor/loop.go).
	seed := protocol.Status{Base: "develop", Phase: protocol.PhasePlanning}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	body := &protocol.Status{Task: "t", Branch: "b", Plan: []string{"do the thing"}}
	if err := runWorkerReport(wt, protocol.PhasePlanning, body, fixedNow(time.Now())); err != nil {
		t.Fatalf("planning report rejected: %v", err)
	}
	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Base != "develop" {
		t.Errorf("Base = %q after planning report, want it preserved as %q", got.Base, "develop")
	}

	body2 := &protocol.Status{Task: "t", Branch: "b"}
	if err = runWorkerReport(wt, protocol.PhaseWorking, body2, fixedNow(time.Now())); err != nil {
		t.Fatalf("working report rejected: %v", err)
	}
	got2, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got2.Base != "develop" {
		t.Errorf("Base = %q after working report, want it still preserved as %q", got2.Base, "develop")
	}
}

// TestRunWorkerReportPreservesTitleWhenReportOmitsIt pins the fix for a
// rework round's report (which reasonably describes only that round's fix)
// silently clobbering an earlier, more accurate title: a report with no
// title of its own must not wipe out one already on file.
func TestRunWorkerReportPreservesTitleWhenReportOmitsIt(t *testing.T) {
	wt := t.TempDir()
	seed := protocol.Status{Title: "feat: interactive shell-completion installer for argus completion", Phase: protocol.PhasePlanning}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	body := &protocol.Status{Task: "t", Branch: "b"}
	if err := runWorkerReport(wt, protocol.PhasePlanning, body, fixedNow(time.Now())); err != nil {
		t.Fatalf("planning report rejected: %v", err)
	}
	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Title != seed.Title {
		t.Errorf("Title = %q after title-less report, want it preserved as %q", got.Title, seed.Title)
	}
}

// TestRunWorkerReportOverwritesTitleWhenReportSetsIt pins the other half: a
// report that does name a new title is a deliberate retitle and must win.
func TestRunWorkerReportOverwritesTitleWhenReportSetsIt(t *testing.T) {
	wt := t.TempDir()
	seed := protocol.Status{Title: "feat: old title", Phase: protocol.PhasePlanning}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	body := &protocol.Status{Task: "t", Branch: "b", Title: "feat: new title"}
	if err := runWorkerReport(wt, protocol.PhasePlanning, body, fixedNow(time.Now())); err != nil {
		t.Fatalf("planning report rejected: %v", err)
	}
	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Title != "feat: new title" {
		t.Errorf("Title = %q, want the report's new title %q", got.Title, "feat: new title")
	}
}

// TestRunWorkerReportPreservesAnswerAcrossReports pins the carry-forward
// behavior for the answer trace: a supervisor's `argus worker answer` writes
// Question/Answer directly to status.json, outside runWorkerReport, so the
// worker's own next report body (which never sends either field) must not
// silently clobber that record back to nil.
func TestRunWorkerReportPreservesAnswerAcrossReports(t *testing.T) {
	wt := t.TempDir()
	seed := protocol.Status{
		Phase:         protocol.PhaseBlocked,
		BlockedReason: "which base?",
		Question: &protocol.Question{
			Text:    "wait and rebase, or cherry-pick now?",
			Options: []string{"wait and rebase", "cherry-pick now"},
		},
		Answer: &protocol.Answer{Text: "cherry-pick now", Option: 2},
	}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	body := &protocol.Status{Task: "t", Branch: "b"}
	if err := runWorkerReport(wt, protocol.PhaseWorking, body, fixedNow(time.Now())); err != nil {
		t.Fatalf("working report rejected: %v", err)
	}
	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Question == nil || got.Question.Text != seed.Question.Text {
		t.Errorf("Question = %+v, want it preserved as %+v", got.Question, seed.Question)
	}
	if got.Answer == nil || got.Answer.Text != "cherry-pick now" {
		t.Errorf("Answer = %+v, want it preserved as %+v", got.Answer, seed.Answer)
	}
}

// TestRunWorkerReportFreshQuestionResetsAnswer pins the other half: a worker
// that reports blocked again with a brand-new Question means a new blocked
// cycle started, so any earlier Answer no longer applies to it and must not
// leak forward as if it resolved the new question too.
func TestRunWorkerReportFreshQuestionResetsAnswer(t *testing.T) {
	wt := t.TempDir()
	seed := protocol.Status{
		Phase:  protocol.PhaseWorking,
		Answer: &protocol.Answer{Text: "cherry-pick now", Option: 2},
	}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	body := &protocol.Status{
		Task:          "t",
		Branch:        "b",
		BlockedReason: "now which forge?",
		Question:      &protocol.Question{Text: "github or gitlab?"},
	}
	if err := runWorkerReport(wt, protocol.PhaseBlocked, body, fixedNow(time.Now())); err != nil {
		t.Fatalf("blocked report rejected: %v", err)
	}
	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Question == nil || got.Question.Text != "github or gitlab?" {
		t.Errorf("Question = %+v, want the fresh one", got.Question)
	}
	if got.Answer != nil {
		t.Errorf("Answer = %+v, want it reset to nil for the new question", got.Answer)
	}
}

// TestRunWorkerReportPreservesSteersAcrossReports pins the carry-forward fix:
// a supervisor's `argus worker steer` appends onto status.json directly, not
// through a worker's own report body (which never carries a "steers" key at
// all), so a worker's next phase report must not silently erase that durable
// audit trace the way it used to — restoring the field's own documented
// "durable trace" contract (see protocol.Status.Steers).
func TestRunWorkerReportPreservesSteersAcrossReports(t *testing.T) {
	wt := t.TempDir()
	seed := protocol.Status{
		Phase:  protocol.PhaseWorking,
		Steers: []protocol.Steer{{Text: "note 1"}, {Text: "note 2"}},
	}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	body := &protocol.Status{Task: "t", Branch: "b"}
	if err := runWorkerReport(wt, protocol.PhaseSelfTest, body, fixedNow(time.Now())); err != nil {
		t.Fatalf("self_test report rejected: %v", err)
	}
	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Steers) != 2 || got.Steers[0].Text != "note 1" || got.Steers[1].Text != "note 2" {
		t.Errorf("Steers = %+v after the worker's own report, want the prior trace preserved unchanged", got.Steers)
	}
}

// TestRunWorkerReportNormalReportStillUpdatesPhasePlanAndDiffStat pins that
// the Steers carry-forward fix doesn't regress the rest of a normal report:
// Phase, Plan, and DiffStat must still come from the reported body even when
// a preserved Steers trace is also being carried forward on the same write.
func TestRunWorkerReportNormalReportStillUpdatesPhasePlanAndDiffStat(t *testing.T) {
	wt := t.TempDir()
	seed := protocol.Status{
		Phase:  protocol.PhaseWorking,
		Steers: []protocol.Steer{{Text: "note 1", Delivered: true}},
	}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	body := &protocol.Status{
		Task:     "t",
		Branch:   "b",
		Plan:     []string{"run the tests", "report self_test"},
		DiffStat: protocol.DiffStat{Files: 3, Insertions: 42, Deletions: 7},
	}
	if err := runWorkerReport(wt, protocol.PhaseSelfTest, body, fixedNow(time.Now())); err != nil {
		t.Fatalf("self_test report rejected: %v", err)
	}
	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Phase != protocol.PhaseSelfTest {
		t.Errorf("Phase = %q, want self_test", got.Phase)
	}
	if len(got.Plan) != 2 || got.Plan[0] != "run the tests" {
		t.Errorf("Plan = %+v, want the reported plan", got.Plan)
	}
	if got.DiffStat != (protocol.DiffStat{Files: 3, Insertions: 42, Deletions: 7}) {
		t.Errorf("DiffStat = %+v, want the reported diff stat", got.DiffStat)
	}
	if len(got.Steers) != 1 || got.Steers[0].Text != "note 1" {
		t.Errorf("Steers = %+v, want the prior trace still preserved alongside the normal update", got.Steers)
	}
}

func TestParseReportablePhaseRejectsUnknown(t *testing.T) {
	for _, s := range []string{"done", "bogus", ""} {
		if _, err := parseReportablePhase(s); err == nil {
			t.Errorf("parseReportablePhase(%q) = nil error, want rejection", s)
		}
	}
	for _, s := range []string{"planning", "working", "self_test", "awaiting_review", "blocked"} {
		if p, err := parseReportablePhase(s); err != nil || string(p) != s {
			t.Errorf("parseReportablePhase(%q) = %q, %v, want %q, nil", s, p, err, s)
		}
	}
}

// TestReadReportBodyRejectsEmptyFile matches the stdin branch's existing
// "empty status body" guard onto the --file branch, so an empty --file fails
// with the same clear hint instead of surfacing json.Unmarshal's opaque
// "unexpected end of JSON input" from the caller downstream.
func TestReadReportBodyRejectsEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := readReportBody(&cobra.Command{}, path)
	if err == nil {
		t.Fatal("want error for an empty --file, got nil")
	}
	if !strings.Contains(err.Error(), "empty status body") {
		t.Errorf("error = %q, want it to mention \"empty status body\"", err.Error())
	}
}

// TestWorkerReportCmdStdinBriefCompliance is the brief-compliance smoke test:
// it drives the actual cobra command (as a worker following the brief text in
// protocol.WriterBrief would) with a JSON body piped on stdin, and checks the
// worker's own claimed updated_at never makes it into the persisted file.
func TestWorkerReportCmdStdinBriefCompliance(t *testing.T) {
	wt := gitLinkedWorktree(t)

	body, err := json.Marshal(protocol.Status{
		UpdatedAt:      time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC), // a lying worker clock
		Task:           "argus#92",
		Branch:         "fix-issue-92",
		RealWorldProof: "go build ./... && go test ./...",
		FilesTouched:   []string{"cmd/worker_report.go"},
		Tests: []protocol.TestRun{
			{Cmd: "make test", Target: "./...", Result: protocol.ResultPass},
		},
		DiffStat: protocol.DiffStat{Files: 1, Insertions: 10, Deletions: 0},
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	cmd := newWorkerReportCmd()
	cmd.SetArgs([]string{"planning", "--worktree", wt})
	cmd.SetIn(bytes.NewReader(body))
	var out bytes.Buffer
	cmd.SetOut(&out)

	if execErr := cmd.Execute(); execErr != nil {
		t.Fatalf("cmd.Execute: %v", execErr)
	}

	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Phase != protocol.PhasePlanning {
		t.Errorf("Phase = %q, want planning", got.Phase)
	}
	if got.UpdatedAt.Year() == 1999 {
		t.Errorf("UpdatedAt = %v, worker-claimed timestamp leaked through", got.UpdatedAt)
	}
	if got.Task != "argus#92" {
		t.Errorf("Task = %q, want argus#92", got.Task)
	}
}

func TestWorkerReportCmdRejectsUnknownPhase(t *testing.T) {
	wt := t.TempDir()
	cmd := newWorkerReportCmd()
	cmd.SetArgs([]string{"done", "--worktree", wt})
	cmd.SetIn(bytes.NewReader([]byte(`{}`)))
	if err := cmd.Execute(); err == nil {
		t.Fatal("want an error reporting done via the CLI, got nil")
	}
}

// TestWorkerReportCmdRejectsNonGitDirectory pins the regression this fixes: a
// plain, non-git directory (e.g. a worker pane that cd'd to the wrong place)
// must be refused before anything is written, not silently accepted as if it
// were the worker's own worktree.
func TestWorkerReportCmdRejectsNonGitDirectory(t *testing.T) {
	wt := t.TempDir()
	cmd := newWorkerReportCmd()
	cmd.SetArgs([]string{"planning", "--worktree", wt})
	cmd.SetIn(bytes.NewReader([]byte(`{"task":"t","branch":"b","plan":["x"]}`)))
	if err := cmd.Execute(); err == nil {
		t.Fatal("want an error reporting from a non-git directory, got nil")
	}
	if _, err := protocol.Load(protocol.StatusPath(wt)); err == nil {
		t.Error("status.json should not have been created in a non-git directory")
	}
}

// TestWorkerReportCmdRejectsMainRepoCheckout pins the other half: a git repo
// that is a real repository but not a linked worktree (e.g. the main
// checkout itself) must also be refused — supervise only ever gives a worker
// a `git worktree add`-created directory, never the main checkout.
func TestWorkerReportCmdRejectsMainRepoCheckout(t *testing.T) {
	dir := gitPlainRepo(t)
	cmd := newWorkerReportCmd()
	cmd.SetArgs([]string{"planning", "--worktree", dir})
	cmd.SetIn(bytes.NewReader([]byte(`{"task":"t","branch":"b","plan":["x"]}`)))
	if err := cmd.Execute(); err == nil {
		t.Fatal("want an error reporting from the main repo checkout, got nil")
	}
	if _, err := protocol.Load(protocol.StatusPath(dir)); err == nil {
		t.Error("status.json should not have been created in the main repo checkout")
	}
}

// TestReadReportBodyFileSuccess pins the --file success path: the file's
// exact bytes are returned unchanged, with no stdin fallback.
func TestReadReportBodyFileSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	want := `{"task":"t"}`
	if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := readReportBody(&cobra.Command{}, path)
	if err != nil {
		t.Fatalf("readReportBody: %v", err)
	}
	if string(got) != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestReadReportBodyFileReadError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	_, err := readReportBody(&cobra.Command{}, path)
	if err == nil {
		t.Fatal("want error for a nonexistent --file, got nil")
	}
	if !strings.Contains(err.Error(), "reading status body") {
		t.Errorf("error = %q, want it to mention \"reading status body\"", err.Error())
	}
}

// TestReadReportBodyRejectsEmptyStdin matches TestReadReportBodyRejectsEmptyFile
// onto the stdin branch: piping nothing must fail with the same clear hint
// instead of surfacing json.Unmarshal's opaque error downstream.
func TestReadReportBodyRejectsEmptyStdin(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetIn(bytes.NewReader(nil))
	_, err := readReportBody(cmd, "")
	if err == nil {
		t.Fatal("want error for empty stdin, got nil")
	}
	if !strings.Contains(err.Error(), "empty status body") {
		t.Errorf("error = %q, want it to mention \"empty status body\"", err.Error())
	}
}

// TestWorkerReportCmdRejectsMalformedJSON pins the decode-error branch in
// RunE: a body that reads fine but doesn't unmarshal as protocol.Status must
// fail clearly and must not write status.json.
func TestWorkerReportCmdRejectsMalformedJSON(t *testing.T) {
	wt := gitLinkedWorktree(t)
	cmd := newWorkerReportCmd()
	cmd.SetArgs([]string{"planning", "--worktree", wt})
	cmd.SetIn(bytes.NewReader([]byte("{not valid json")))
	err := cmd.Execute()
	if err == nil {
		t.Fatal("want error for malformed JSON body, got nil")
	}
	if !strings.Contains(err.Error(), "decoding status body") {
		t.Errorf("error = %q, want it to mention \"decoding status body\"", err.Error())
	}
	if _, loadErr := protocol.Load(protocol.StatusPath(wt)); loadErr == nil {
		t.Error("status.json should not have been created for a malformed body")
	}
}

// TestWorkerReportCmdPropagatesFileReadError pins RunE's error path when
// readReportBody itself fails, distinct from the unit test of readReportBody
// alone: the CLI must surface the same wrapped error, not swallow it.
func TestWorkerReportCmdPropagatesFileReadError(t *testing.T) {
	wt := gitLinkedWorktree(t)
	cmd := newWorkerReportCmd()
	cmd.SetArgs([]string{"planning", "--worktree", wt, "--file", filepath.Join(t.TempDir(), "missing.json")})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("want error for a nonexistent --file, got nil")
	}
	if !strings.Contains(err.Error(), "reading status body") {
		t.Errorf("error = %q, want it to mention \"reading status body\"", err.Error())
	}
}

// TestWorkerReportCmdPropagatesTransitionError pins RunE's error path when
// runWorkerReport itself rejects the transition, distinct from the direct
// unit tests of runWorkerReport: the CLI must surface the same error.
func TestWorkerReportCmdPropagatesTransitionError(t *testing.T) {
	wt := gitLinkedWorktree(t)
	cmd := newWorkerReportCmd()
	cmd.SetArgs([]string{"awaiting_review", "--worktree", wt})
	cmd.SetIn(bytes.NewReader([]byte(`{"task":"t","branch":"b"}`)))
	err := cmd.Execute()
	if err == nil {
		t.Fatal("want error for an illegal first transition via the CLI, got nil")
	}
	if !strings.Contains(err.Error(), "illegal status transition") {
		t.Errorf("error = %q, want it to mention the illegal transition", err.Error())
	}
}

// TestWorkerReportCmdUsesGetwdWhenWorktreeFlagEmpty exercises the actual
// os.Getwd fallback (not just the flag wiring TestWorkerReportCmdWorktreeFlagDefaultsEmpty
// pins): no test in this package uses t.Parallel, so a chdir for the
// duration of this test cannot race another test's cwd-relative work.
func TestWorkerReportCmdUsesGetwdWhenWorktreeFlagEmpty(t *testing.T) {
	wt := gitLinkedWorktree(t)
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if chdirErr := os.Chdir(wt); chdirErr != nil {
		t.Fatalf("Chdir: %v", chdirErr)
	}
	defer func() {
		if chdirErr := os.Chdir(origWd); chdirErr != nil {
			t.Fatalf("Chdir back to %s: %v", origWd, chdirErr)
		}
	}()

	cmd := newWorkerReportCmd()
	cmd.SetArgs([]string{"planning"})
	cmd.SetIn(bytes.NewReader([]byte(`{"task":"t","branch":"b","plan":["x"]}`)))
	if execErr := cmd.Execute(); execErr != nil {
		t.Fatalf("cmd.Execute: %v", execErr)
	}

	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Phase != protocol.PhasePlanning {
		t.Errorf("Phase = %q, want planning", got.Phase)
	}
}

// TestRunWorkerReportRejectsGatedEdgeWithLogButNoFreshRecord is the hooked-run
// hard-reject case: a plan-log.jsonl exists (argus's hooks are wired) but no
// record has landed since the checkpoint a prior gated transition already
// consumed — the actual cheat this issue closes, distinct from the fail-open
// (no log at all) case below.
func TestRunWorkerReportRejectsGatedEdgeWithLogButNoFreshRecord(t *testing.T) {
	wt := t.TempDir()
	// Plan is non-empty here specifically to prove the self-reported field is
	// NOT what's being checked once a plan-log exists — only the live record.
	seed := protocol.Status{Phase: protocol.PhasePlanning, Plan: []string{"x"}}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := supervisor.AppendPlanLog(wt, "TodoWrite", old); err != nil {
		t.Fatalf("AppendPlanLog: %v", err)
	}
	if err := supervisor.AdvancePlanCheckpoint(wt, old.Add(time.Minute)); err != nil {
		t.Fatalf("AdvancePlanCheckpoint: %v", err)
	}

	err := runWorkerReport(wt, protocol.PhaseWorking, &protocol.Status{}, fixedNow(old.Add(time.Hour)))
	if err == nil {
		t.Fatal("want an error moving planning -> working with a plan-log present but no fresh record since checkpoint, got nil")
	}
	if !strings.Contains(err.Error(), "no fresh TodoWrite/TaskCreate/TaskUpdate activity") {
		t.Errorf("error = %q, want it to mention no fresh activity", err.Error())
	}
	got, loadErr := protocol.Load(protocol.StatusPath(wt))
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if got.Phase != protocol.PhasePlanning {
		t.Errorf("status.json Phase = %q, want unchanged planning (rejected transition must not persist)", got.Phase)
	}
}

// TestRunWorkerReportAcceptsGatedEdgeWithFreshLogRecordAndAdvancesCheckpoint
// pins the accept path and its side effect: a fresh record lets the
// transition through, and the checkpoint advances so the same (now-stale)
// record can't satisfy the next gated edge too.
func TestRunWorkerReportAcceptsGatedEdgeWithFreshLogRecordAndAdvancesCheckpoint(t *testing.T) {
	wt := t.TempDir()
	seed := protocol.Status{Phase: protocol.PhasePlanning}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	if err := supervisor.AppendPlanLog(wt, "TodoWrite", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("AppendPlanLog: %v", err)
	}
	stamp := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := runWorkerReport(wt, protocol.PhaseWorking, &protocol.Status{}, fixedNow(stamp)); err != nil {
		t.Fatalf("gated transition with a fresh (pre-checkpoint) plan-log record rejected: %v", err)
	}
	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Phase != protocol.PhaseWorking {
		t.Errorf("Phase = %q, want working", got.Phase)
	}

	fresh, logExists, err := supervisor.HasFreshPlanEvidence(wt)
	if err != nil {
		t.Fatalf("HasFreshPlanEvidence: %v", err)
	}
	if !logExists {
		t.Fatal("logExists = false, want true")
	}
	if fresh {
		t.Error("fresh = true after the accepted transition, want false — the checkpoint must have advanced past the record this transition just spent")
	}
}

// TestRunWorkerReportRetriesSameEdgeAfterFreshEvidenceRecorded pins the
// documented no-self-loop retry design at checkPlanEvidence's call site: a
// rejected gated report leaves the phase unchanged, so the worker's retry is
// to record fresh evidence and re-send the exact same forward edge — which
// must then succeed, with no new self-loop transition ever required.
func TestRunWorkerReportRetriesSameEdgeAfterFreshEvidenceRecorded(t *testing.T) {
	wt := t.TempDir()
	seed := protocol.Status{Phase: protocol.PhaseWorking}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := supervisor.AppendPlanLog(wt, "TodoWrite", old); err != nil {
		t.Fatalf("AppendPlanLog: %v", err)
	}
	if err := supervisor.AdvancePlanCheckpoint(wt, old.Add(time.Minute)); err != nil {
		t.Fatalf("AdvancePlanCheckpoint: %v", err)
	}

	if err := runWorkerReport(wt, protocol.PhaseSelfTest, &protocol.Status{}, fixedNow(old.Add(2*time.Minute))); err == nil {
		t.Fatal("want the first working -> self_test attempt rejected for stale evidence, got nil")
	}

	fresh := old.Add(3 * time.Minute)
	if err := supervisor.AppendPlanLog(wt, "TodoWrite", fresh); err != nil {
		t.Fatalf("AppendPlanLog (retry): %v", err)
	}

	if err := runWorkerReport(wt, protocol.PhaseSelfTest, &protocol.Status{}, fixedNow(fresh.Add(time.Minute))); err != nil {
		t.Fatalf("retry of the same edge after fresh plan-log evidence rejected: %v", err)
	}
	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Phase != protocol.PhaseSelfTest {
		t.Errorf("Phase = %q, want self_test", got.Phase)
	}
}

// TestRunWorkerReportEnforcesFreshEvidencePerGatedEdge drives all three gated
// edges through a hooked run's real plan-log.jsonl, proving each one needs
// its own fresh record — the windowed half of "record live, enforce per
// phase": the record that satisfied planning -> working must not still
// satisfy working -> self_test, and so on.
func TestRunWorkerReportEnforcesFreshEvidencePerGatedEdge(t *testing.T) {
	wt := t.TempDir()
	base := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	tick := 0
	next := func() time.Time {
		tick++
		return base.Add(time.Duration(tick) * time.Minute)
	}
	record := func() {
		if err := supervisor.AppendPlanLog(wt, "TodoWrite", next()); err != nil {
			t.Fatalf("AppendPlanLog: %v", err)
		}
	}
	report := func(phase protocol.Phase) error {
		return runWorkerReport(wt, phase, &protocol.Status{}, fixedNow(next()))
	}

	if err := report(protocol.PhasePlanning); err != nil {
		t.Fatalf("planning report rejected: %v", err)
	}

	record()
	if err := report(protocol.PhaseWorking); err != nil {
		t.Fatalf("planning -> working rejected despite a fresh record: %v", err)
	}

	if err := report(protocol.PhaseSelfTest); err == nil {
		t.Fatal("want working -> self_test rejected — no new plan-log activity since planning -> working's own checkpoint advance")
	}
	record()
	if err := report(protocol.PhaseSelfTest); err != nil {
		t.Fatalf("working -> self_test rejected despite a fresh record: %v", err)
	}

	if err := report(protocol.PhaseAwaitingReview); err == nil {
		t.Fatal("want self_test -> awaiting_review rejected — no new plan-log activity since self_test's own checkpoint advance")
	}
	record()
	if err := report(protocol.PhaseAwaitingReview); err != nil {
		t.Fatalf("self_test -> awaiting_review rejected despite a fresh record: %v", err)
	}

	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Phase != protocol.PhaseAwaitingReview {
		t.Errorf("final phase = %q, want awaiting_review", got.Phase)
	}
}

// TestRunWorkerReportFailsOpenForNewlyGatedEdgesWithNoPlanLog is the
// fail-open acceptance test for the two edges RequiresPlanEvidence newly
// gates (working -> self_test, self_test -> awaiting_review): with no
// plan-log.jsonl at all (a foreign/headless spawn argus never hooked), they
// have no legacy self-reported signal to fall back on, so they must stay
// allowed exactly as they were before this issue widened RequiresPlanEvidence
// to cover them.
func TestRunWorkerReportFailsOpenForNewlyGatedEdgesWithNoPlanLog(t *testing.T) {
	wt := t.TempDir()
	seed := protocol.Status{Phase: protocol.PhaseWorking}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	if err := runWorkerReport(wt, protocol.PhaseSelfTest, &protocol.Status{}, fixedNow(time.Now())); err != nil {
		t.Fatalf("working -> self_test rejected with no plan-log at all: %v", err)
	}

	wt2 := t.TempDir()
	seed2 := protocol.Status{Phase: protocol.PhaseSelfTest}
	if err := protocol.Write(protocol.StatusPath(wt2), &seed2); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	if err := runWorkerReport(wt2, protocol.PhaseAwaitingReview, &protocol.Status{}, fixedNow(time.Now())); err != nil {
		t.Fatalf("self_test -> awaiting_review rejected with no plan-log at all: %v", err)
	}
}

// TestRunWorkerReportPropagatesPlanEvidenceCheckError exercises
// checkPlanEvidence's own error branch: a corrupt plan-checkpoint.json makes
// HasFreshPlanEvidence itself fail, which must surface as a UserError rather
// than being silently swallowed.
func TestRunWorkerReportPropagatesPlanEvidenceCheckError(t *testing.T) {
	wt := t.TempDir()
	seed := protocol.Status{Phase: protocol.PhasePlanning}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	if err := supervisor.AppendPlanLog(wt, "TodoWrite", time.Now()); err != nil {
		t.Fatalf("AppendPlanLog: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".claude", "argus", "plan-checkpoint.json"), []byte("{bad json"), 0o600); err != nil {
		t.Fatalf("seeding corrupt checkpoint: %v", err)
	}

	err := runWorkerReport(wt, protocol.PhaseWorking, &protocol.Status{}, fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error when plan evidence can't be checked, got nil")
	}
	if !strings.Contains(err.Error(), "checking plan evidence") {
		t.Errorf("error = %q, want it to mention checking plan evidence", err.Error())
	}
}

// TestRunWorkerReportPropagatesCheckpointAdvanceError exercises
// checkPlanEvidence's other error branch: a fresh record clears
// HasFreshPlanEvidence, but AdvancePlanCheckpoint itself then fails to
// persist — that must also surface as a UserError, not a silent pass.
func TestRunWorkerReportPropagatesCheckpointAdvanceError(t *testing.T) {
	wt := t.TempDir()
	seed := protocol.Status{Phase: protocol.PhasePlanning}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	if err := supervisor.AppendPlanLog(wt, "TodoWrite", time.Now()); err != nil {
		t.Fatalf("AppendPlanLog: %v", err)
	}
	dir := filepath.Join(wt, ".claude", "argus")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := runWorkerReport(wt, protocol.PhaseWorking, &protocol.Status{}, fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error when the checkpoint can't be persisted, got nil")
	}
	if !strings.Contains(err.Error(), "advancing plan checkpoint") {
		t.Errorf("error = %q, want it to mention advancing plan checkpoint", err.Error())
	}
}

func TestWorkerReportCmdWorktreeFlagDefaultsEmpty(t *testing.T) {
	// An empty default means RunE falls back to os.Getwd() (see
	// newWorkerReportCmd) — a worker running `argus worker report <phase>`
	// from inside its own worktree (SpawnCommand cd's it there) needs no
	// --worktree flag at all. Exercising the actual getwd fallback would
	// require a chdir, which races other parallel tests, so this only pins
	// the flag wiring.
	cmd := newWorkerReportCmd()
	f := cmd.Flags().Lookup("worktree")
	if f == nil || f.DefValue != "" {
		t.Fatalf("worktree flag = %+v, want a registered flag defaulting to empty", f)
	}
}
