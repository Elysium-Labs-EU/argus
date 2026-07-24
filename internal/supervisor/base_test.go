package supervisor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
)

// runGitForTest runs a git command against dir, failing the test on error —
// mirrors ship_test.go's inline exec.Command setup pattern.
func runGitForTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// newRepoWithOriginHEAD builds a real git repo whose refs/remotes/origin/HEAD
// points at defaultBranch, by cloning a bare "origin" repo seeded with one
// commit on that branch — the same auto-detection a real `git clone` gives.
func newRepoWithOriginHEAD(t *testing.T, defaultBranch string) string {
	t.Helper()
	origin := t.TempDir()
	runGitForTest(t, origin, "init", "-q", "--initial-branch="+defaultBranch)
	runGitForTest(t, origin, "config", "user.email", "t@t")
	runGitForTest(t, origin, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitForTest(t, origin, "add", "README.md")
	runGitForTest(t, origin, "commit", "-q", "-m", "init")

	repo := t.TempDir()
	runGitForTest(t, filepath.Dir(repo), "clone", "-q", origin, repo)
	return repo
}

func TestDetectDefaultBaseReadsOriginHEAD(t *testing.T) {
	repo := newRepoWithOriginHEAD(t, "trunk")
	got, err := DetectDefaultBase(context.Background(), repo)
	if err != nil {
		t.Fatalf("DetectDefaultBase: %v", err)
	}
	if got != "trunk" {
		t.Errorf("DetectDefaultBase = %q, want %q", got, "trunk")
	}
}

func TestDetectDefaultBaseErrorsWithNoOriginHEAD(t *testing.T) {
	repo := t.TempDir()
	runGitForTest(t, repo, "init", "-q")
	if _, err := DetectDefaultBase(context.Background(), repo); err == nil {
		t.Fatal("want an error when origin/HEAD is unset, got nil")
	}
}

func TestResolveBaseExplicitFlagWinsOutright(t *testing.T) {
	repo := newRepoWithOriginHEAD(t, "trunk")
	if err := protocol.Write(protocol.StatusPath(repo), &protocol.Status{Base: "develop"}); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	got := ResolveBase(context.Background(), repo, "explicit-base", true)
	if got != "explicit-base" {
		t.Errorf("ResolveBase = %q, want the explicit flag value", got)
	}
}

func TestResolveBasePrefersPersistedStatus(t *testing.T) {
	repo := newRepoWithOriginHEAD(t, "trunk")
	if err := protocol.Write(protocol.StatusPath(repo), &protocol.Status{Base: "develop"}); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	if err := repoconfig.Save(repoconfig.Path(repo), repoconfig.Config{BaseBranch: "from-config"}); err != nil {
		t.Fatalf("seeding repo config: %v", err)
	}
	got := ResolveBase(context.Background(), repo, "main", false)
	if got != "develop" {
		t.Errorf("ResolveBase = %q, want the persisted status.Base %q", got, "develop")
	}
}

func TestResolveBaseFallsBackToRepoConfig(t *testing.T) {
	repo := newRepoWithOriginHEAD(t, "trunk")
	if err := repoconfig.Save(repoconfig.Path(repo), repoconfig.Config{BaseBranch: "from-config"}); err != nil {
		t.Fatalf("seeding repo config: %v", err)
	}
	got := ResolveBase(context.Background(), repo, "main", false)
	if got != "from-config" {
		t.Errorf("ResolveBase = %q, want the repo config base_branch %q", got, "from-config")
	}
}

func TestResolveBaseFallsBackToDetectedOriginHEAD(t *testing.T) {
	repo := newRepoWithOriginHEAD(t, "trunk")
	got := ResolveBase(context.Background(), repo, "main", false)
	if got != "trunk" {
		t.Errorf("ResolveBase = %q, want the detected origin/HEAD %q", got, "trunk")
	}
}

func TestResolveBaseFallsBackToLiteralMain(t *testing.T) {
	repo := t.TempDir()
	runGitForTest(t, repo, "init", "-q")
	got := ResolveBase(context.Background(), repo, "main", false)
	if got != "main" {
		t.Errorf("ResolveBase = %q, want the literal fallback %q", got, "main")
	}
}
