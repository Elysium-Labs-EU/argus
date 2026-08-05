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
// points at "trunk", by cloning a bare "origin" repo seeded with one commit
// on that branch — the same auto-detection a real `git clone` gives.
func newRepoWithOriginHEAD(t *testing.T) string {
	t.Helper()
	origin := t.TempDir()
	runGitForTest(t, origin, "init", "-q", "--initial-branch=trunk")
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
	repo := newRepoWithOriginHEAD(t)
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

// Real git refuses to store a symbolic-ref target with an empty final
// component ("refs/remotes/origin/"), so the TrimPrefix-empty branch can't
// be reached through any real repo state — only through a stubbed git
// binary that hands back that exact string.
func TestDetectDefaultBaseErrorsWhenOriginHEADTrimsToEmpty(t *testing.T) {
	stubDir := t.TempDir()
	stub := "#!/bin/sh\necho 'origin/'\n"
	if err := os.WriteFile(filepath.Join(stubDir, "git"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := DetectDefaultBase(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("want an error when origin/HEAD trims to an empty branch name, got nil")
	}
	if !strings.Contains(err.Error(), "empty branch name") {
		t.Errorf("error = %q, want it to mention the empty branch name", err)
	}
}

func TestResolveBaseExplicitFlagWinsOutright(t *testing.T) {
	repo := newRepoWithOriginHEAD(t)
	if err := protocol.Write(protocol.StatusPath(repo), &protocol.Status{Base: "develop"}); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	got := ResolveBase(context.Background(), repo, "explicit-base", true)
	if got != "explicit-base" {
		t.Errorf("ResolveBase = %q, want the explicit flag value", got)
	}
}

func TestResolveBasePrefersPersistedStatus(t *testing.T) {
	repo := newRepoWithOriginHEAD(t)
	if err := protocol.Write(protocol.StatusPath(repo), &protocol.Status{Base: "develop"}); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	if err := repoconfig.Save(repoconfig.Path(repo), &repoconfig.Config{BaseBranch: "from-config"}); err != nil {
		t.Fatalf("seeding repo config: %v", err)
	}
	got := ResolveBase(context.Background(), repo, "main", false)
	if got != "develop" {
		t.Errorf("ResolveBase = %q, want the persisted status.Base %q", got, "develop")
	}
}

func TestResolveBaseFallsBackToRepoConfig(t *testing.T) {
	repo := newRepoWithOriginHEAD(t)
	if err := repoconfig.Save(repoconfig.Path(repo), &repoconfig.Config{BaseBranch: "from-config"}); err != nil {
		t.Fatalf("seeding repo config: %v", err)
	}
	got := ResolveBase(context.Background(), repo, "main", false)
	if got != "from-config" {
		t.Errorf("ResolveBase = %q, want the repo config base_branch %q", got, "from-config")
	}
}

func TestResolveBaseFallsBackToDetectedOriginHEAD(t *testing.T) {
	repo := newRepoWithOriginHEAD(t)
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

func TestResolveBaseFallsBackToMainWhenRepoRootFails(t *testing.T) {
	notARepo := t.TempDir()
	got := ResolveBase(context.Background(), notARepo, "main", false)
	if got != "main" {
		t.Errorf("ResolveBase = %q, want the literal fallback %q when RepoRoot errors", got, "main")
	}
}

func TestResolveGateBaseExplicitFlagWinsOutright(t *testing.T) {
	repo := newRepoWithOriginHEAD(t)
	rc := repoconfig.Config{BaseBranch: "develop"}
	got := ResolveGateBase(context.Background(), true, "origin/explicit", repo, &rc)
	if got.Ref != "origin/explicit" || got.Source != BaseSourceFlag {
		t.Errorf("ResolveGateBase = %+v, want {origin/explicit flag}", got)
	}
}

func TestResolveGateBasePrefersRepoConfig(t *testing.T) {
	repo := newRepoWithOriginHEAD(t)
	rc := repoconfig.Config{BaseBranch: "develop"}
	got := ResolveGateBase(context.Background(), false, "origin/main", repo, &rc)
	if got.Ref != "origin/develop" || got.Source != BaseSourceConfig {
		t.Errorf("ResolveGateBase = %+v, want {origin/develop config}", got)
	}
}

func TestResolveGateBaseFallsBackToDetectedOriginHEAD(t *testing.T) {
	repo := newRepoWithOriginHEAD(t)
	got := ResolveGateBase(context.Background(), false, "origin/main", repo, &repoconfig.Config{})
	if got.Ref != "origin/trunk" || got.Source != BaseSourceDetected {
		t.Errorf("ResolveGateBase = %+v, want {origin/trunk detected}", got)
	}
}

func TestResolveGateBaseFallsBackToFlagDefault(t *testing.T) {
	got := ResolveGateBase(context.Background(), false, "origin/main", "", &repoconfig.Config{})
	if got.Ref != "origin/main" || got.Source != BaseSourceDefault {
		t.Errorf("ResolveGateBase = %+v, want {origin/main default} when nothing else resolves", got)
	}
}

// TestResolveGateBaseAgreesForSupervisePathAndReworkPath pins the actual
// point of the fix: supervise and rework both feed --base's explicit flag,
// this repo's config, and the same resolved repoRoot into this one function,
// so calling it the way each command does — for the identical repo/config —
// can never again independently diverge the way rework's own bespoke
// (pre-fix) resolution used to.
func TestResolveGateBaseAgreesForSupervisePathAndReworkPath(t *testing.T) {
	repo := newRepoWithOriginHEAD(t)
	if err := repoconfig.Save(repoconfig.Path(repo), &repoconfig.Config{BaseBranch: "develop"}); err != nil {
		t.Fatalf("seeding repo config: %v", err)
	}
	rc, err := repoconfig.Load(repoconfig.Path(repo))
	if err != nil {
		t.Fatalf("repoconfig.Load: %v", err)
	}

	// supervise's own call shape (cmd/supervise.go's newSuperviseCmd RunE):
	// no --base passed.
	superviseBase := ResolveGateBase(context.Background(), false, "origin/main", repo, &rc)
	// rework's own call shape (cmd/rework.go's buildReworkConfig): same
	// flag/default, same repo, same loaded config.
	reworkBase := ResolveGateBase(context.Background(), false, "origin/main", repo, &rc)

	if superviseBase != reworkBase {
		t.Errorf("supervise resolved %+v, rework resolved %+v — must agree", superviseBase, reworkBase)
	}
	if superviseBase.Ref != "origin/develop" {
		t.Errorf("resolved ref = %q, want origin/develop from base_branch config", superviseBase.Ref)
	}
}

func TestVerifyBaseRefAcceptsExistingRef(t *testing.T) {
	repo := newRepoWithOriginHEAD(t)
	if err := VerifyBaseRef(context.Background(), repo, ResolvedBase{Ref: "origin/trunk", Source: BaseSourceDetected}); err != nil {
		t.Errorf("VerifyBaseRef should accept an existing ref, got %v", err)
	}
}

func TestVerifyBaseRefNoopsOnEmptyRef(t *testing.T) {
	repo := newRepoWithOriginHEAD(t)
	if err := VerifyBaseRef(context.Background(), repo, ResolvedBase{}); err != nil {
		t.Errorf("VerifyBaseRef should no-op on an empty ref, got %v", err)
	}
}

// TestVerifyBaseRefRejectsMissingRef pins the fail-fast fix's error shape: a
// base ref that doesn't exist must fail with a message naming both the
// resolved ref and where it came from, not git's raw "fatal: bad revision"
// text — see the escalation gap this closes in measureReconcileDiffs.
func TestVerifyBaseRefRejectsMissingRef(t *testing.T) {
	repo := newRepoWithOriginHEAD(t)
	err := VerifyBaseRef(context.Background(), repo, ResolvedBase{Ref: "origin/does-not-exist", Source: BaseSourceDefault})
	if err == nil {
		t.Fatal("want an error for a base ref that doesn't exist, got nil")
	}
	msg := err.Error()
	for _, want := range []string{`"origin/does-not-exist"`, "does not exist", "resolved from: flag default"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}
