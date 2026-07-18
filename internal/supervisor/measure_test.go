package supervisor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"codeberg.org/Elysium_Labs/argus/internal/protocol"
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
	cmd := SpawnCommand("/repo/.claude/worktrees/feat $(whoami)", "claude")
	if !strings.Contains(cmd, `cd '/repo/.claude/worktrees/feat $(whoami)'`) {
		t.Errorf("worktree not single-quoted: %s", cmd)
	}
	// An embedded single quote is escaped, not left to close the quote early.
	got := shellQuote("a'b")
	if got != `'a'\''b'` {
		t.Errorf("shellQuote(a'b) = %s", got)
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
