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
	out := "3\t1\tcmd/root.go\n10\t0\tinternal/x.go\n-\t-\tlogo.png\n"
	stat, files, err := parseNumstat(out)
	if err != nil {
		t.Fatalf("parseNumstat: %v", err)
	}
	if stat.Files != 3 || stat.Insertions != 13 || stat.Deletions != 1 {
		t.Errorf("stat: got %+v want files=3 ins=13 del=1", stat)
	}
	if len(files) != 3 || files[2] != "logo.png" {
		t.Errorf("files: got %v", files)
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
}

func TestGateAlwaysReviewsBehaviorCriticalPaths(t *testing.T) {
	// A small, clean, passing change — but it touches internal/monitor, a
	// behavior-critical (degraded-mode) surface, so the gate must escalate.
	st := &workerState{
		hasFile:       true,
		measuredOK:    true,
		measured:      protocol.DiffStat{Files: 1, Insertions: 4},
		measuredFiles: []string{"internal/monitor/health_monitor.go"},
		plan:          &WorkerPlan{Worker: Worker{Task: "restart backoff"}},
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

func hasReasonContaining(reasons []string, sub string) bool {
	for _, r := range reasons {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}
