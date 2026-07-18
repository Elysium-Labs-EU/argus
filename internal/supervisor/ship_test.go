package supervisor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseOwnerRepo(t *testing.T) {
	cases := []struct {
		url, owner, repo string
	}{
		{"git@codeberg.org:Elysium_Labs/eos.git", "Elysium_Labs", "eos"},
		{"git@codeberg.org:Elysium_Labs/eos", "Elysium_Labs", "eos"},
		{"https://codeberg.org/Elysium_Labs/eos.git", "Elysium_Labs", "eos"},
		{"https://codeberg.org/Elysium_Labs/eos", "Elysium_Labs", "eos"},
		{"ssh://git@codeberg.org/Elysium_Labs/eos.git", "Elysium_Labs", "eos"},
	}
	for _, tc := range cases {
		owner, repo, err := parseOwnerRepo(tc.url)
		if err != nil {
			t.Errorf("%s: %v", tc.url, err)
			continue
		}
		if owner != tc.owner || repo != tc.repo {
			t.Errorf("%s: got %s/%s want %s/%s", tc.url, owner, repo, tc.owner, tc.repo)
		}
	}
}

func TestParseOwnerRepoRejectsGarbage(t *testing.T) {
	if _, _, err := parseOwnerRepo("not-a-url"); err == nil {
		t.Fatal("want error for unparseable remote")
	}
}

func TestCommitAllExcludesControlPlane(t *testing.T) {
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

	// A real code change plus the argus control-plane files a worker's worktree
	// carries. Only the code change may be committed.
	write := func(rel, content string) {
		full := filepath.Join(wt, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go", "package main\n")
	write(".claude/argus/status.json", `{"phase":"awaiting_review"}`)
	write(".claude/argus/brief.md", "do the thing")
	write(".claude/settings.local.json", "{}")

	if err := CommitAll(context.Background(), wt, "feat: real change"); err != nil {
		t.Fatalf("CommitAll: %v", err)
	}

	files, err := git(context.Background(), wt, "show", "--name-only", "--pretty=format:", "HEAD")
	if err != nil {
		t.Fatalf("listing committed files: %v", err)
	}
	if !strings.Contains(files, "main.go") {
		t.Errorf("real change not committed; files=%q", files)
	}
	if strings.Contains(files, ".claude") {
		t.Errorf("control-plane files leaked into the commit; files=%q", files)
	}
}

func TestCommitAllControlPlaneOnlyIsNothingToCommit(t *testing.T) {
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
	if err := os.MkdirAll(filepath.Join(wt, ".claude", "argus"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".claude", "argus", "status.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The only change is argus's own control plane → nothing worth shipping.
	if err := CommitAll(context.Background(), wt, "m"); !errors.Is(err, ErrNothingToCommit) {
		t.Fatalf("want ErrNothingToCommit, got %v", err)
	}
}
