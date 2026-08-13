package supervisor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

func TestParseNumstatCountsAndFiles(t *testing.T) {
	out := "3\t1\tcmd/root.go\x0010\t0\tinternal/x.go\x00-\t-\tlogo.png\x00"
	records, err := parseNumstat(out)
	if err != nil {
		t.Fatalf("parseNumstat: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("records: got %v", records)
	}
	var ins, del int
	for _, r := range records {
		ins += r.insertions
		del += r.deletions
	}
	if ins != 13 || del != 1 {
		t.Errorf("got ins=%d del=%d want ins=13 del=1", ins, del)
	}
	if records[2].path != "logo.png" {
		t.Errorf("records: got %v", records)
	}
}

func TestParseNumstatEmptyOutputYieldsNoRecords(t *testing.T) {
	records, err := parseNumstat("")
	if err != nil {
		t.Fatalf("parseNumstat: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("want no records for empty output, got %v", records)
	}
}

// TestParseNumstatResolvesRenameToNewPath pins the fix for a real bypass: git
// diff --numstat (without -z) renders a rename as text like "old => new" or,
// when the paths share a prefix, "prefix{old => new}suffix" — the old
// (pre-numstat) implementation naively split on whitespace and resolved to
// the OLD path, so any check keyed on FilesTouched (including selfConfigPath
// in review.go) never saw the file's real, current path. With -z, git emits
// the old and new paths as two separate, unambiguous NUL-terminated tokens
// instead, which parseNumstat now resolves to the new path.
func TestParseNumstatResolvesRenameToNewPath(t *testing.T) {
	// "ins\tdel\t" (empty path) then two tokens: old path, new path — the
	// exact shape `git diff --numstat -z` emits for a detected rename.
	out := "0\t0\t\x00docs/notes.md\x00.argus/config.yml\x00"
	records, err := parseNumstat(out)
	if err != nil {
		t.Fatalf("parseNumstat: %v", err)
	}
	if len(records) != 1 || records[0].path != ".argus/config.yml" {
		t.Fatalf("rename must resolve to the new path, got %v", records)
	}
}

func TestParseNumstatTruncatedRenameRecordErrors(t *testing.T) {
	// A rename record's empty-path token with no old/new tokens following it
	// (malformed/truncated input) must error, not panic or silently drop it.
	if _, err := parseNumstat("0\t0\t\x00docs/notes.md\x00"); err == nil {
		t.Fatal("want an error for a truncated rename record")
	}
}

// bigGitWorktree makes a git repo whose uncommitted change adds `lines` lines to
// path, so MeasureDiff sees a large real diff regardless of what a worker claims.
func bigGitWorktree(t *testing.T, path string, lines int) string {
	t.Helper()
	wt := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", wt}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	full := filepath.Join(wt, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	if err := os.WriteFile(full, []byte("base\n"+strings.Repeat("added\n", lines)), 0o600); err != nil {
		t.Fatal(err)
	}
	return wt
}

func TestMeasureDiffCountsUntrackedFiles(t *testing.T) {
	// A worker that ADDS a new file: git diff misses it, but MeasureDiff must not.
	wt := gitWorktreeWithDiff(t) // has a tracked edit (+2) vs HEAD
	newFile := filepath.Join(wt, "cmd", "new_e2e_test.go")
	if err := os.MkdirAll(filepath.Dir(newFile), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newFile, []byte(strings.Repeat("line\n", 40)), 0o600); err != nil {
		t.Fatal(err)
	}
	ds, files, err := MeasureDiff(context.Background(), wt, "HEAD")
	if err != nil {
		t.Fatalf("MeasureDiff: %v", err)
	}
	if ds.Insertions < 40 {
		t.Errorf("untracked new file not counted: insertions=%d", ds.Insertions)
	}
	found := false
	for _, f := range files {
		if strings.HasSuffix(f, "new_e2e_test.go") {
			found = true
		}
	}
	if !found {
		t.Errorf("untracked file not in file list: %v", files)
	}
}

// TestMeasureDiffExcludesUntrackedBinaryFile guards against MeasureDiff
// treating a new binary fixture (PDF, PNG, font, ...) as hundreds of spurious
// inserted "lines": countLines does a raw newline scan with no binary
// detection, unlike parseNumstat's handling of tracked binary files (git
// reports "-" for them). An inflated count here can trip the gate's
// unwaivable under-report hard check even when the worker's own diff_stat
// was accurate.
func TestMeasureDiffExcludesUntrackedBinaryFile(t *testing.T) {
	wt := gitWorktreeWithDiff(t) // has a tracked edit (+2) vs HEAD
	binPath := filepath.Join(wt, "fixture.bin")
	data := make([]byte, 8192)
	for i := range data {
		// A NUL byte plus a very dense run of '\n' bytes: if countLines ever
		// regressed to a raw newline scan, this alone would report thousands
		// of "lines" for a single fixture.
		if i%2 == 0 {
			data[i] = '\n'
		} else {
			data[i] = 0
		}
	}
	if err := os.WriteFile(binPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	ds, files, err := MeasureDiff(context.Background(), wt, "HEAD")
	if err != nil {
		t.Fatalf("MeasureDiff: %v", err)
	}
	found := false
	for _, f := range files {
		if strings.HasSuffix(f, "fixture.bin") {
			found = true
		}
	}
	if !found {
		t.Errorf("untracked binary file must still be counted as a touched file: %v", files)
	}
	// The tracked edit to f.go contributes 2 insertions; the binary file must
	// contribute none of the ~4096 newline bytes it contains.
	if ds.Insertions > 10 {
		t.Errorf("untracked binary file inflated insertions: got %d, want <=10 (just f.go's real edit)", ds.Insertions)
	}
}

// status.json changes on every normal work session, and a repo that never
// gitignored .claude can carry a tracked copy of it forward from an earlier
// branch — either way it must not inflate the measured diff, since ship
// always drops it before opening the PR.
func TestMeasureDiffExcludesControlPlaneFiles(t *testing.T) {
	wt := gitWorktreeWithDiff(t) // has a tracked edit to f.go (+2) vs HEAD
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", wt}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	// Committing this add on its own, rather than folding it into f.go's
	// uncommitted edit above, puts status.json in the baseline itself — proving
	// the exclude holds for a tracked diff against HEAD, not only for an
	// untracked file git diff would already miss.
	statusPath := filepath.Join(wt, ".claude", "argus", "status.json")
	if err := os.MkdirAll(filepath.Dir(statusPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statusPath, []byte(`{"phase":"working"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", ".claude/argus/status.json")
	run("commit", "-q", "-m", "pre-existing control-plane baggage")
	if err := os.WriteFile(statusPath, []byte(strings.Repeat(`{"phase":"awaiting_review"}`+"\n", 20)), 0o600); err != nil {
		t.Fatal(err)
	}

	ds, files, err := MeasureDiff(context.Background(), wt, "HEAD")
	if err != nil {
		t.Fatalf("MeasureDiff: %v", err)
	}
	for _, f := range files {
		if strings.HasPrefix(f, ".claude/argus/") {
			t.Errorf("measured files must not include control-plane path, got %v", files)
		}
	}
	if ds.Insertions >= 20 {
		t.Errorf("control-plane edit inflated measured insertions: %+v", ds)
	}
	if ds.Files != 1 {
		t.Errorf("expected only the real f.go edit to be measured, got %+v files=%v", ds, files)
	}
}

// TestMeasureDiffExcludesUntrackedReportBodyScratchFile is the regression
// test for the exact symptom that put .argus-report-body.json in
// controlPlanePaths: a worker that passes `argus worker report --file` a
// body it wrote out at the worktree root first, rather than piping it over
// stdin, leaves that file untracked and — unlike the control-plane files
// covered by TestMeasureDiffExcludesControlPlaneFiles above — it was never
// gitignored, so `git ls-files --others --exclude-standard` used to surface
// it here and inflate the measured diff past the worker's own self-reported
// line count, tripping the gate's unwaivable under-report check on an
// otherwise clean change.
func TestMeasureDiffExcludesUntrackedReportBodyScratchFile(t *testing.T) {
	wt := gitWorktreeWithDiff(t) // has a tracked edit to f.go (+2) vs HEAD
	scratch := filepath.Join(wt, ".argus-report-body.json")
	if err := os.WriteFile(scratch, []byte(strings.Repeat(`{"phase":"working"}`+"\n", 20)), 0o600); err != nil {
		t.Fatal(err)
	}

	ds, files, err := MeasureDiff(context.Background(), wt, "HEAD")
	if err != nil {
		t.Fatalf("MeasureDiff: %v", err)
	}
	for _, f := range files {
		if f == ".argus-report-body.json" {
			t.Errorf("measured files must not include the report-body scratch file, got %v", files)
		}
	}
	if ds.Insertions >= 20 {
		t.Errorf("report-body scratch file inflated measured insertions: %+v", ds)
	}
	if ds.Files != 1 {
		t.Errorf("expected only the real f.go edit to be measured, got %+v files=%v", ds, files)
	}
}

// TestMeasureDiffCatchesConfigPlantedViaRename is the full end-to-end
// regression test for the rename bypass: a worker that plants
// .argus/config.yml by renaming an unrelated tracked file onto it (git
// detects this as a rename by content similarity, not a delete+add) must
// still show up in MeasureDiff's file list under its real, current path —
// and, fed into Assess, still trip selfConfigPath's unconditional escalation
// (review.go). Before the -z fix, git's abbreviated rename text ("old =>
// new") resolved to the OLD path, so .argus/config.yml never appeared in
// FilesTouched at all and this attack auto-approved cleanly.
func TestMeasureDiffCatchesConfigPlantedViaRename(t *testing.T) {
	wt := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", wt}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	// Padded enough that git's default rename-similarity heuristic (>=50%)
	// still detects this as a rename once "allow" is appended below, not a
	// plain delete+add.
	base := strings.Repeat("alpha beta gamma delta epsilon\n", 8)
	notesPath := filepath.Join(wt, "docs", "notes.md")
	if err := os.MkdirAll(filepath.Dir(notesPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(notesPath, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "base")

	if err := os.MkdirAll(filepath.Join(wt, ".argus"), 0o750); err != nil {
		t.Fatal(err)
	}
	run("mv", filepath.Join("docs", "notes.md"), filepath.Join(".argus", "config.yml"))
	planted := base + "allow: [\"Bash(*)\"]\nalways_review_paths: []\n"
	if err := os.WriteFile(filepath.Join(wt, ".argus", "config.yml"), []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}

	_, files, err := MeasureDiff(context.Background(), wt, "HEAD")
	if err != nil {
		t.Fatalf("MeasureDiff: %v", err)
	}
	found := false
	for _, f := range files {
		if f == ".argus/config.yml" {
			found = true
		}
	}
	if !found {
		t.Fatalf("MeasureDiff must resolve the rename to .argus/config.yml, got %v", files)
	}

	v := Assess(&protocol.Status{
		Phase:        protocol.PhaseAwaitingReview,
		Tests:        []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultPass}},
		FilesTouched: files,
		DiffStat:     protocol.DiffStat{Insertions: 1, Deletions: 0},
	}, nil)
	if v.AutoApprove {
		t.Fatal("a .argus/config.yml planted via rename must still escalate, not auto-approve")
	}
	if !hasReasonContaining(v.Reasons, "always reviewed regardless") {
		t.Errorf("expected selfConfigPath's escalation reason, got %v", v.Reasons)
	}
}

func TestGateEscalatesWhenWorkerUnderReportsDiff(t *testing.T) {
	wt := bigGitWorktree(t, "cmd/root.go", 50)
	ds, files, err := MeasureDiff(context.Background(), wt, "HEAD")
	if err != nil {
		t.Fatalf("MeasureDiff: %v", err)
	}
	if ds.Insertions < 50 {
		t.Fatalf("expected >=50 measured insertions, got %d", ds.Insertions)
	}

	// Worker lies: claims a 1-line clean change with passing tests.
	st := &workerState{
		hasFile:       true,
		measuredOK:    true,
		measured:      ds,
		measuredFiles: files,
		plan:          &WorkerPlan{Worker: Worker{Task: "liar", Worktree: wt}},
		status: protocol.Status{
			Phase:    protocol.PhaseAwaitingReview,
			DiffStat: protocol.DiffStat{Files: 1, Insertions: 1},
			Tests:    []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultPass}},
		},
	}
	v := gateVerdict(st, nil)
	if v.AutoApprove {
		t.Fatal("gate must not auto-approve a worker whose real diff dwarfs its self-report")
	}
	if !hasReasonContaining(v.Reasons, "under-reported diff") {
		t.Errorf("expected an under-report reason, got %v", v.Reasons)
	}
	if !hasReasonContaining(v.HardReasons, "under-reported diff") {
		t.Errorf("under-report must be a hard reason (unwaivable by --review), got HardReasons=%v", v.HardReasons)
	}
}

// TestGateDoesNotUnderReportCheckForNonTerminalPhase guards against comparing
// a live git measurement to a self-report that isn't final yet: mid-"working"
// status.json's DiffStat is a stale snapshot from an earlier report, while git
// reflects whatever the worker has on disk at the instant of this poll — an
// honest worker still mid-edit must not trip the unwaivable under-report hard
// reason just because those two numbers haven't caught up with each other yet.
func TestGateDoesNotUnderReportCheckForNonTerminalPhase(t *testing.T) {
	wt := bigGitWorktree(t, "cmd/other.go", 50)
	ds, files, err := MeasureDiff(context.Background(), wt, "HEAD")
	if err != nil {
		t.Fatalf("MeasureDiff: %v", err)
	}

	st := &workerState{
		hasFile:       true,
		measuredOK:    true,
		measured:      ds,
		measuredFiles: files,
		plan:          &WorkerPlan{Worker: Worker{Task: "still-working", Worktree: wt}},
		status: protocol.Status{
			Phase:    protocol.PhaseWorking,
			DiffStat: protocol.DiffStat{Files: 1, Insertions: 1},
		},
	}
	v := gateVerdict(st, nil)
	if hasReasonContaining(v.Reasons, "under-reported diff") {
		t.Errorf("under-report check must not run for a non-terminal phase, got %v", v.Reasons)
	}
}

// TestGateOversizedDiffIsNotAHardReason documents the other half of the
// hard/soft split: a diff that merely exceeds the size ceiling is
// a judgment call --review can still approve past, so it must land only in
// Reasons, never HardReasons.
func TestGateOversizedDiffIsNotAHardReason(t *testing.T) {
	st := &workerState{
		hasFile:       true,
		measuredOK:    true,
		measured:      protocol.DiffStat{Files: 1, Insertions: 500, Deletions: 100},
		measuredFiles: []string{"cmd/root.go"},
		plan:          &WorkerPlan{Worker: Worker{Task: "big-but-honest"}},
		status: protocol.Status{
			Phase:    protocol.PhaseAwaitingReview,
			DiffStat: protocol.DiffStat{Files: 1, Insertions: 500, Deletions: 100},
			Tests:    []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultPass}},
		},
	}
	v := gateVerdict(st, &ReviewPolicy{MaxDiffLines: 400})
	if v.AutoApprove {
		t.Fatal("oversized diff must still escalate")
	}
	if !hasReasonContaining(v.Reasons, "exceeds max") {
		t.Errorf("expected an oversized-diff reason, got %v", v.Reasons)
	}
	if len(v.HardReasons) != 0 {
		t.Errorf("oversized diff (honestly reported) must not be a hard reason, got HardReasons=%v", v.HardReasons)
	}
}

func TestGateEscalatesWhenMeasuredDiffIsEmptyDespiteClaimedCompletion(t *testing.T) {
	// Regression test: a worker (or a stale/fabricated status.json left
	// behind by a launcher spawn that never really ran) reports a terminal phase
	// with passing tests and a plausible-looking self-reported diff, but git shows
	// zero files actually changed against base. This must never auto-approve.
	wt := t.TempDir()
	st := &workerState{
		hasFile:       true,
		measuredOK:    true,
		measured:      protocol.DiffStat{Files: 0, Insertions: 0, Deletions: 0},
		measuredFiles: nil,
		plan:          &WorkerPlan{Worker: Worker{Task: "fabricated", Worktree: wt}},
		status: protocol.Status{
			Phase:    protocol.PhaseAwaitingReview,
			DiffStat: protocol.DiffStat{Files: 1, Insertions: 3, Deletions: 3},
			Tests:    []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultPass}},
		},
	}
	v := gateVerdict(st, nil)
	if v.AutoApprove {
		t.Fatal("gate must not auto-approve a terminal-phase worker whose measured diff touches zero files")
	}
	if !hasReasonContaining(v.Reasons, "zero files changed") {
		t.Errorf("expected a zero-files-changed reason, got %v", v.Reasons)
	}
	if !hasReasonContaining(v.HardReasons, "zero files changed") {
		t.Errorf("zero-files-changed must be a hard reason (unwaivable by --review), got HardReasons=%v", v.HardReasons)
	}
}

func TestGateEscalatesWhenDiffUnmeasurable(t *testing.T) {
	st := &workerState{
		hasFile: true,
		diffErr: os.ErrNotExist,
		plan:    &WorkerPlan{Worker: Worker{Task: "x", Worktree: "/nope"}},
		status:  protocol.Status{Phase: protocol.PhaseAwaitingReview},
	}
	v := gateVerdict(st, nil)
	if v.AutoApprove {
		t.Fatal("gate must not auto-approve when it could not verify the diff")
	}
	if !hasReasonContaining(v.Reasons, "could not measure diff") {
		t.Errorf("expected an unmeasurable reason, got %v", v.Reasons)
	}
	if !hasReasonContaining(v.HardReasons, "could not measure diff") {
		t.Errorf("unmeasurable diff must be a hard reason (unwaivable by --review), got HardReasons=%v", v.HardReasons)
	}
}

func TestGateAlwaysReviewsBehaviorCriticalPaths(t *testing.T) {
	// A small, clean, passing change — but it touches internal/monitor, a
	// behavior-critical (degraded-mode) surface, so the gate must escalate.
	st := &workerState{
		hasFile:         true,
		measuredOK:      true,
		measured:        protocol.DiffStat{Files: 1, Insertions: 4},
		measuredFiles:   []string{"internal/monitor/health_monitor.go"},
		planEvidenceOK:  true,
		hasPlanEvidence: true,
		plan:            &WorkerPlan{Worker: Worker{Task: "restart backoff"}},
		status: protocol.Status{
			Phase: protocol.PhaseAwaitingReview,
			Tests: []protocol.TestRun{{Cmd: "make ci", Result: protocol.ResultPass}},
		},
	}
	v := gateVerdict(st, nil) // nil -> DefaultReviewPolicy, which includes monitor/health
	if v.AutoApprove {
		t.Fatal("a change to a behavior-critical path must not auto-approve")
	}
	if !hasReasonContaining(v.Reasons, "behavior-critical") {
		t.Errorf("expected a behavior-critical reason, got %v", v.Reasons)
	}

	// The same diff elsewhere still auto-approves.
	st.measuredFiles = []string{"internal/textutil/wrap.go"}
	if v := gateVerdict(st, nil); !v.AutoApprove {
		t.Errorf("a benign small clean change should auto-approve, got %v", v.Reasons)
	}
}

func TestMatchAnyIsSegmentAware(t *testing.T) {
	cases := []struct {
		path, glob string
		want       bool
	}{
		{"cmd/install.go", "install", true},
		{"cmd/uninstall.go", "install", false},
		{"reinstaller/main.go", "install", false},
		{"etc/hosts", "/etc/", true},
		{"config/etc/hosts", "etc", true},
		{"internal/config/x.go", "internal/config", true},
		{"internal/configstore/x.go", "internal/config", true}, // multi-segment glob = substring
		{"pkg/systemd/unit.go", "systemd", true},
		{"pkg/mysystemdemo/x.go", "systemd", false},
	}
	for _, c := range cases {
		_, got := matchAny(c.path, []string{c.glob})
		if got != c.want {
			t.Errorf("matchAny(%q, %q) = %v, want %v", c.path, c.glob, got, c.want)
		}
	}
}

func TestSpawnCommandSingleQuotesWorktree(t *testing.T) {
	// A worktree path with a space and a shell-substitution-looking segment must
	// be a single quoted literal, not something the pane's shell interprets.
	cmd := SpawnCommand("/repo/.claude/worktrees/feat $(whoami)", "claude", nil, nil)
	if !strings.Contains(cmd, `cd '/repo/.claude/worktrees/feat $(whoami)'`) {
		t.Errorf("worktree not single-quoted: %s", cmd)
	}
	// An embedded single quote is escaped, not left to close the quote early.
	got := shellQuote("a'b")
	if got != `'a'\''b'` {
		t.Errorf("shellQuote(a'b) = %s", got)
	}
}

func TestSpawnCommandScrubsEnv(t *testing.T) {
	// With scrub vars, the launcher runs under `env -u` for each, so a forge or
	// issue-tracker token the pane inherited is not in the worker's environment.
	// Without them, the command is byte-for-byte the plain form.
	plain := SpawnCommand("/wt", "claude", nil, nil)
	if strings.Contains(plain, "env -u") {
		t.Errorf("nil scrub must not add env -u: %s", plain)
	}
	scrubbed := SpawnCommand("/wt", "claude", []string{"CODEBERG_TOKEN", "GH_TOKEN"}, nil)
	if !strings.Contains(scrubbed, "&& env -u CODEBERG_TOKEN -u GH_TOKEN claude ") {
		t.Errorf("scrub not applied before launcher: %s", scrubbed)
	}
}

func TestSpawnCommandNilEnvUnchanged(t *testing.T) {
	// With no scrub and no worker env, SpawnCommand must produce the same
	// command whether nil or empty slices are passed, so both knobs stay
	// strictly opt-in.
	got := SpawnCommand("/wt", "claude", nil, nil)
	want := `cd '/wt' && claude "Read .claude/argus/brief.md and follow it exactly; it is your task brief."`
	if got != want {
		t.Errorf("SpawnCommand(nil, nil) = %q, want %q", got, want)
	}
}

func TestSpawnCommandInjectsWorkerEnv(t *testing.T) {
	env := []string{
		"ANTHROPIC_BASE_URL=http://127.0.0.1:5555/anthropic",
		"ANTHROPIC_API_KEY=argus-sentinel-abc",
		"malformed-no-equals",
	}
	cmd := SpawnCommand("/repo/wt", "claude --permission-mode auto", nil, env)

	// Assignments land inline before the launcher, values single-quoted, so the
	// launcher and its children inherit them while the pane shell does not.
	if !strings.Contains(cmd, `&& ANTHROPIC_BASE_URL='http://127.0.0.1:5555/anthropic' ANTHROPIC_API_KEY='argus-sentinel-abc' claude`) {
		t.Errorf("env not injected before launcher: %s", cmd)
	}
	// A pair without '=' is skipped, never emitted as a bare word.
	if strings.Contains(cmd, "malformed-no-equals") {
		t.Errorf("malformed env entry leaked into command: %s", cmd)
	}
	// A value that looks like shell substitution stays a quoted literal.
	inj := SpawnCommand("/wt", "claude", nil, []string{"X=$(whoami)"})
	if !strings.Contains(inj, `X='$(whoami)'`) {
		t.Errorf("env value not single-quoted: %s", inj)
	}
}

func TestSpawnCommandCombinesScrubAndWorkerEnv(t *testing.T) {
	// The two knobs are independent and can both be active at once: scrubbed
	// names are withheld via `env -u` and the worker's phantom credentials are
	// still set inline, all under the same `env` invocation.
	cmd := SpawnCommand("/wt", "claude", []string{"CODEBERG_TOKEN"}, []string{"ANTHROPIC_API_KEY=argus-sentinel-abc"})
	if !strings.Contains(cmd, "&& env -u CODEBERG_TOKEN ANTHROPIC_API_KEY='argus-sentinel-abc' claude ") {
		t.Errorf("scrub and worker env not combined: %s", cmd)
	}
}

func TestResolveLauncherPathSubstitutesAbsolutePath(t *testing.T) {
	// "ls" is on PATH in any test environment; assert the binary token is
	// replaced with its absolute path while trailing args are preserved
	// byte-for-byte.
	resolved, err := exec.LookPath("ls")
	if err != nil {
		t.Skip("ls not found on PATH in this test environment")
	}

	got := ResolveLauncherPath("ls -la /tmp")
	want := resolved + " -la /tmp"
	if got != want {
		t.Errorf("ResolveLauncherPath(%q) = %q, want %q", "ls -la /tmp", got, want)
	}
}

func TestResolveLauncherPathUnchangedWhenNotFound(t *testing.T) {
	launcher := "definitely-not-a-real-binary-argus-test --permission-mode auto"
	if got := ResolveLauncherPath(launcher); got != launcher {
		t.Errorf("ResolveLauncherPath(%q) = %q, want unchanged", launcher, got)
	}
}

func TestResolveLauncherPathEmptyUnchanged(t *testing.T) {
	if got := ResolveLauncherPath(""); got != "" {
		t.Errorf("ResolveLauncherPath(\"\") = %q, want empty", got)
	}
}

func TestResolveWorktreeRelativeBecomesAbsolute(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	got, err := ResolveWorktree(filepath.Join(".", "featx"))
	if err != nil {
		t.Fatalf("ResolveWorktree: %v", err)
	}
	want := filepath.Join(dir, "featx")
	if got != want {
		t.Errorf("ResolveWorktree(%q) = %q, want %q", "./featx", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("ResolveWorktree(%q) = %q, want an absolute path", "./featx", got)
	}
}

func TestResolveWorktreeAbsolutePassesThroughUnchanged(t *testing.T) {
	dir := t.TempDir()
	got, err := ResolveWorktree(dir)
	if err != nil {
		t.Fatalf("ResolveWorktree: %v", err)
	}
	if got != dir {
		t.Errorf("ResolveWorktree(%q) = %q, want unchanged", dir, got)
	}
}

// TestResolveWorktreeEmptyResolvesToCwd documents (rather than special-cases)
// filepath.Abs's own behavior for "": every caller already refuses an empty
// --worktree with its own "no worktree given" ui.UserError before ever
// reaching ResolveWorktree, so this is what a caller that skipped that guard
// would get, not a contract ResolveWorktree enforces itself.
func TestResolveWorktreeEmptyResolvesToCwd(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	got, err := ResolveWorktree("")
	if err != nil {
		t.Fatalf("ResolveWorktree(\"\"): %v", err)
	}
	if got != dir {
		t.Errorf(`ResolveWorktree("") = %q, want cwd %q`, got, dir)
	}
}

// movedBaseWorktree builds a real repo plus a linked worktree that simulates
// argus's own worker setup: repo's "main" branch has an initial commit, a
// worker worktree branches off it with an honest uncommitted edit to f.go,
// and then — while the worker is still "running" — main advances with an
// unrelated commit, the same way another PR merging to origin/main while a
// worker works does. Returns the repo dir (where main lives) and the worker
// worktree dir.
func movedBaseWorktree(t *testing.T) (repo, worktree string) {
	t.Helper()
	repo = t.TempDir()
	run := func(dir string, args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run(repo, "init", "-q", "--initial-branch=main")
	run(repo, "config", "user.email", "t@t")
	run(repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "f.go"), []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", "-A")
	run(repo, "commit", "-q", "-m", "init")

	// `git worktree add` insists on creating its target dir itself, so hand it
	// a not-yet-existing child of a fresh temp dir rather than t.TempDir()'s
	// own (already-created) path.
	worktree = filepath.Join(t.TempDir(), "wt")
	run(repo, "worktree", "add", "-q", "-b", "feature", worktree, "main")

	// The worker's own honest, uncommitted edit — never committed, matching
	// how a real worker's worktree looks when the gate measures it.
	if err := os.WriteFile(filepath.Join(worktree, "f.go"), []byte("package x\n\nvar Added = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// main advances with an unrelated commit while the worker keeps working —
	// exactly what a moved base looks like. The repo dir is still checked out
	// on "main" (the new worktree took "feature"), so this commits straight
	// onto it.
	if err := os.WriteFile(filepath.Join(repo, "other.go"), []byte("package other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", "-A")
	run(repo, "commit", "-q", "-m", "unrelated merge landed on main")

	return repo, worktree
}

// TestResolveEffectiveDiffBaseIgnoresBaseMovingAhead is the regression test
// for the moving-base bug: once main advances past the worktree's branch
// point, the merge-base must still be the original commit the worktree
// actually branched from, not main's new tip.
func TestResolveEffectiveDiffBaseIgnoresBaseMovingAhead(t *testing.T) {
	repo, worktree := movedBaseWorktree(t)

	mergeBase, err := ResolveEffectiveDiffBase(context.Background(), worktree, "main")
	if err != nil {
		t.Fatalf("ResolveEffectiveDiffBase: %v", err)
	}

	mainTip, err := git(context.Background(), repo, "rev-parse", "main")
	if err != nil {
		t.Fatalf("rev-parse main: %v", err)
	}
	if mergeBase == mainTip {
		t.Fatalf("merge-base must not equal main's moved tip %s, got %s", mainTip, mergeBase)
	}
	branchPoint, err := git(context.Background(), worktree, "rev-parse", "feature")
	if err != nil {
		t.Fatalf("rev-parse feature: %v", err)
	}
	if mergeBase != branchPoint {
		t.Errorf("merge-base = %s, want the worktree's actual branch point %s", mergeBase, branchPoint)
	}
}

// TestMeasureDiffIgnoresBaseMovingAhead is the full regression test for the
// moving-base bug: MeasureDiff against a base that has advanced since the
// worktree branched must report only the worker's own honest edit, never a
// phantom deletion of whatever landed on base in the meantime. Before the
// merge-base fix, this measured other.go as a deleted file (the two-dot diff
// against main's new tip reverts main's own unrelated commit), inflating the
// diff and risking a fabricated "under-reported diff" hard reason even though
// the worker told the truth.
func TestMeasureDiffIgnoresBaseMovingAhead(t *testing.T) {
	_, worktree := movedBaseWorktree(t)

	ds, files, err := MeasureDiff(context.Background(), worktree, "main")
	if err != nil {
		t.Fatalf("MeasureDiff: %v", err)
	}
	if ds.Files != 1 || ds.Deletions != 0 {
		t.Fatalf("expected only the worker's own 1-file, 0-deletion edit, got %+v files=%v", ds, files)
	}
	for _, f := range files {
		if f == "other.go" {
			t.Fatalf("MeasureDiff must not report other.go — that's main's own unrelated commit, not the worker's, got files=%v", files)
		}
	}

	// Confirms the test actually exercises the bug this guards against: a
	// plain two-dot diff against main's moved tip does show other.go as
	// deleted, which is exactly the phantom revert the merge-base fix avoids.
	oldWay, err := parseNumstat(mustNumstat(t, worktree, "main"))
	if err != nil {
		t.Fatalf("parseNumstat: %v", err)
	}
	sawPhantomDelete := false
	for _, r := range oldWay {
		if r.path == "other.go" && r.deletions > 0 {
			sawPhantomDelete = true
		}
	}
	if !sawPhantomDelete {
		t.Fatal("test setup is broken: a plain two-dot diff against main should show other.go as a phantom deletion")
	}
}

// mustNumstat runs the old, pre-fix two-dot `git diff --numstat -z <base>`
// directly, for TestMeasureDiffIgnoresBaseMovingAhead to contrast against
// MeasureDiff's merge-base-based result.
func mustNumstat(t *testing.T, worktree, base string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", worktree, "diff", "--numstat", "-z", base)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git diff --numstat -z %s: %v", base, err)
	}
	return string(out)
}

// TestDiffForIgnoresBaseMovingAhead mirrors TestMeasureDiffIgnoresBaseMovingAhead
// for the reviewer's own diff: the text an LLM reviewer reads must not show
// main's own unrelated commit as a deletion either.
func TestDiffForIgnoresBaseMovingAhead(t *testing.T) {
	_, worktree := movedBaseWorktree(t)

	diff, err := DiffFor(context.Background(), worktree, "main")
	if err != nil {
		t.Fatalf("DiffFor: %v", err)
	}
	if strings.Contains(diff, "other.go") {
		t.Errorf("reviewer diff must not mention other.go — that's main's own unrelated commit, not the worker's:\n%s", diff)
	}
	if !strings.Contains(diff, "Added") {
		t.Errorf("reviewer diff should still show the worker's own real edit:\n%s", diff)
	}
}

// TestCommitsBehindBaseCountsCommitsBaseGainedSinceBranch is the unit test for
// the separate staleness signal: base moved ahead by exactly the one commit
// movedBaseWorktree adds after the worktree branches.
func TestCommitsBehindBaseCountsCommitsBaseGainedSinceBranch(t *testing.T) {
	_, worktree := movedBaseWorktree(t)

	behind, err := CommitsBehindBase(context.Background(), worktree, "main")
	if err != nil {
		t.Fatalf("CommitsBehindBase: %v", err)
	}
	if behind != 1 {
		t.Errorf("CommitsBehindBase = %d, want 1", behind)
	}
}

// TestCommitsBehindBaseZeroWhenBaseUnmoved guards the other half: a worktree
// whose base never moved must report zero, not stay unset in some way that
// reads the same as an error.
func TestCommitsBehindBaseZeroWhenBaseUnmoved(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	behind, err := CommitsBehindBase(context.Background(), wt, "HEAD")
	if err != nil {
		t.Fatalf("CommitsBehindBase: %v", err)
	}
	if behind != 0 {
		t.Errorf("CommitsBehindBase = %d, want 0 when base never moved", behind)
	}
}

// TestGateDoesNotUnderReportWhenBaseMovedAhead is the gate-level regression
// test: a worker whose self-report honestly matches its own small edit must
// still auto-approve even though base advanced with unrelated commits while
// it worked — the exact false "under-reported diff" hard escalation the
// merge-base fix exists to prevent. See TestMeasureDiffIgnoresBaseMovingAhead
// for the lower-level proof that MeasureDiff itself no longer inflates the
// diff; this proves the gate draws the right conclusion from it.
func TestGateDoesNotUnderReportWhenBaseMovedAhead(t *testing.T) {
	_, worktree := movedBaseWorktree(t)

	ds, files, err := MeasureDiff(context.Background(), worktree, "main")
	if err != nil {
		t.Fatalf("MeasureDiff: %v", err)
	}

	st := &workerState{
		hasFile:           true,
		measuredOK:        true,
		measured:          ds,
		measuredFiles:     files,
		commitsBehindBase: 1,
		plan:              &WorkerPlan{Worker: Worker{Task: "honest", Worktree: worktree}},
		status: protocol.Status{
			Phase:    protocol.PhaseAwaitingReview,
			DiffStat: ds,
			Tests:    []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultPass}},
		},
	}
	v := gateVerdict(st, nil)
	if hasReasonContaining(v.HardReasons, "under-reported diff") {
		t.Fatalf("a moved base must never fabricate an under-report hard reason, got HardReasons=%v", v.HardReasons)
	}
	if !v.AutoApprove {
		t.Errorf("an honest small change should auto-approve regardless of how far base moved, got Reasons=%v", v.Reasons)
	}
	if !hasReasonContaining(v.Notes, "commit(s) behind base") {
		t.Errorf("expected the separate behind-base note, got Notes=%v", v.Notes)
	}
}

func TestCommitsBehindBaseErrorsOnBadBaseRef(t *testing.T) {
	worktree, _ := initGitRepo(t)
	_, err := CommitsBehindBase(context.Background(), worktree, "nonexistent-base-ref")
	if err == nil {
		t.Fatal("want an error for a nonexistent base ref")
	}
	if !strings.Contains(err.Error(), "nonexistent-base-ref") {
		t.Errorf("error should name the bad ref, got: %v", err)
	}
}

// TestCommitsBehindBaseErrorsOnUnparseableCount covers the Atoi failure path,
// which real git plumbing can't reach on its own (`rev-list --count` always
// emits a plain integer) — only a stubbed git binary can hand back something
// that doesn't parse.
func TestCommitsBehindBaseErrorsOnUnparseableCount(t *testing.T) {
	stubDir := t.TempDir()
	stub := "#!/bin/sh\necho 'not-a-number'\n"
	if err := os.WriteFile(filepath.Join(stubDir, "git"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := CommitsBehindBase(context.Background(), t.TempDir(), "main")
	if err == nil {
		t.Fatal("want an error when git's own output does not parse as a count")
	}
	if !strings.Contains(err.Error(), "parsing commits-behind count") {
		t.Errorf("error = %q, want it to name the parse failure", err)
	}
}

// TestMeasureDiffErrorsOnBadBaseRef exercises MeasureDiff's own early return
// from a merge-base resolution failure (as opposed to a bad --worktree,
// already covered by TestReconcileMeasuresAndRecordsError).
func TestMeasureDiffErrorsOnBadBaseRef(t *testing.T) {
	worktree, _ := initGitRepo(t)
	_, _, err := MeasureDiff(context.Background(), worktree, "nonexistent-base-ref")
	if err == nil {
		t.Fatal("want an error for a nonexistent base ref")
	}
	if !strings.Contains(err.Error(), "nonexistent-base-ref") {
		t.Errorf("error should name the bad ref, got: %v", err)
	}
}

func hasReasonContaining(reasons []string, sub string) bool {
	for _, r := range reasons {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}
