package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

// initGitDirWithDiff is initGitDir plus an untracked file, so MeasureDiff
// reports a non-empty, non-zero-files diff against the branch's own tip —
// required for a round to actually reach Approved=true: the gate's hard,
// unwaivable "zero files changed despite a claimed terminal phase" check
// would otherwise override even a reviewer "approve" back to not-approved,
// regardless of what the reviewer says.
func initGitDirWithDiff(t *testing.T) string {
	t.Helper()
	dir := initGitDir(t)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatalf("seeding untracked diff file: %v", err)
	}
	return dir
}

// sequenceReviewer returns each result in turn on successive Review calls,
// repeating the last for any calls beyond the list, and counts how many times
// it ran — the cmd-package analog of internal/supervisor's own test-only
// sequenceReviewRunner, needed here because rework's loop can call the
// reviewer once per round.
type sequenceReviewer struct {
	results []supervisor.ReviewResult
	calls   int
	mu      sync.Mutex
}

func (s *sequenceReviewer) Review(_ context.Context, _ *supervisor.ReviewRequest) (supervisor.ReviewResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	if i >= len(s.results) {
		i = len(s.results) - 1
	}
	s.calls++
	return s.results[i], nil
}

func (s *sequenceReviewer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// reworkTestPaneID is the pane every fakeReworkClient below answers with —
// every test dispatches into the same single worktree/pane pair.
const reworkTestPaneID = "w1:p1"

// fakeReworkClient models a worktree whose pane starts bare and comes up live
// after the first spawn (mirroring fakeRebaseClient in rebase_test.go): every
// dispatch — whether the initial PaneRun spawn or a later AgentPrompt reuse —
// writes status shortly after, as a real worker eventually would, and mutates
// the worktree with round-distinct content first. A real rework worker always
// changes something (that is the whole point of the round), so the gate's
// zero-delta check would otherwise flag every round here as a no-op and block
// even a reviewer "approve"; the per-dispatch content keeps each round a
// genuine delta from the state before it.
func fakeReworkClient(worktree string, status *protocol.Status) herdr.Client {
	var mu sync.Mutex
	var spawned bool
	var dispatchN int
	writeStatusSoon := func() {
		mu.Lock()
		dispatchN++
		n := dispatchN
		mu.Unlock()
		go func() {
			time.Sleep(20 * time.Millisecond)
			_ = os.WriteFile(filepath.Join(worktree, "reworked.txt"), fmt.Appendf(nil, "rework round %d\n", n), 0o644)
			_ = protocol.Write(protocol.StatusPath(worktree), status)
		}()
	}
	return herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "worktree":
			return fmt.Appendf(nil, `{"result":{"root_pane":{"pane_id":%q}}}`, reworkTestPaneID), nil
		case len(args) > 1 && args[0] == "agent" && args[1] == "get":
			mu.Lock()
			live := spawned
			mu.Unlock()
			if !live {
				return nil, fmt.Errorf("herdr agent get: %w", herdr.ErrAgentNotFound)
			}
			return fmt.Appendf(nil, `{"result":{"agent":{"pane_id":%q,"agent":"claude","agent_status":"done"}}}`, reworkTestPaneID), nil
		case len(args) > 1 && args[0] == "pane" && args[1] == "run":
			mu.Lock()
			spawned = true
			mu.Unlock()
			writeStatusSoon()
			return []byte(`{"result":{}}`), nil
		case len(args) > 1 && args[0] == "agent" && args[1] == "prompt":
			writeStatusSoon()
			return []byte(`{"result":{}}`), nil
		default:
			return []byte(`{"result":{}}`), nil
		}
	})
}

// fakeReworkClientRounds is fakeReworkClient generalized to report a
// different status on each successive dispatch, keyed by a 1-based round
// counter — needed to grow the worktree's real diff between rounds and
// self-report only that round's own delta, the shape that exercises
// gateVerdict's priorMeasured subtraction (see
// TestRunReworkSubtractsPriorMeasuredAcrossRounds).
func fakeReworkClientRounds(worktree string, statusFor func(round int) *protocol.Status) herdr.Client {
	var mu sync.Mutex
	var spawned bool
	var round int
	dispatch := func() {
		mu.Lock()
		round++
		r := round
		mu.Unlock()
		go func() {
			time.Sleep(20 * time.Millisecond)
			_ = protocol.Write(protocol.StatusPath(worktree), statusFor(r))
		}()
	}
	return herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "worktree":
			return fmt.Appendf(nil, `{"result":{"root_pane":{"pane_id":%q}}}`, reworkTestPaneID), nil
		case len(args) > 1 && args[0] == "agent" && args[1] == "get":
			mu.Lock()
			live := spawned
			mu.Unlock()
			if !live {
				return nil, fmt.Errorf("herdr agent get: %w", herdr.ErrAgentNotFound)
			}
			return fmt.Appendf(nil, `{"result":{"agent":{"pane_id":%q,"agent":"claude","agent_status":"done"}}}`, reworkTestPaneID), nil
		case len(args) > 1 && args[0] == "pane" && args[1] == "run":
			mu.Lock()
			spawned = true
			mu.Unlock()
			dispatch()
			return []byte(`{"result":{}}`), nil
		case len(args) > 1 && args[0] == "agent" && args[1] == "prompt":
			dispatch()
			return []byte(`{"result":{}}`), nil
		default:
			return []byte(`{"result":{}}`), nil
		}
	})
}

// writeLinesFile overwrites path with n newline-terminated lines, so
// MeasureDiff's untracked-file line count (see countLines in measure.go)
// reports exactly n insertions for it.
func writeLinesFile(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Repeat("line\n", n)), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// reworkStatus is the failing-test status fakeReworkClient's worker "reports"
// after every dispatch in the tests below: it forces the gate to escalate
// every round, so the reviewer's own decision (not diff/plan-evidence
// bookkeeping) drives the loop.
func reworkStatus() *protocol.Status {
	return &protocol.Status{
		Phase: protocol.PhaseAwaitingReview,
		Tests: []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultFail}},
	}
}

func reworkLogger() *eventlog.Logger {
	return eventlog.New(nil, "rework", "test-run", nil)
}

func TestRunReworkEmptyWorktree(t *testing.T) {
	cmd, _ := testCmd()
	err := runRework(cmd, herdr.New(), &fakeReviewer{}, reworkLogger(), &reworkOpts{})
	if _, ok := errors.AsType[*ui.UserError](err); !ok {
		t.Fatalf("want a *ui.UserError for an empty worktree, got %v", err)
	}
}

// updateRefOriginBranch fakes a remote-tracking ref (refs/remotes/origin/<branch>)
// pointing at HEAD, the shape a real clone off a non-main-default repo would
// have, without needing a real remote to pull from — mirrors the pattern
// ship_test.go's TestWritePRChangeSectionHappyPath already uses.
func updateRefOriginBranch(t *testing.T, dir, branch string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", dir, "update-ref", "refs/remotes/origin/"+branch, "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("git update-ref refs/remotes/origin/%s: %v\n%s", branch, err, out)
	}
}

// writeReworkRepoConfig writes a minimal .argus/config.yml under dir with the
// given raw YAML body — unlike doctor_test.go's writeRepoConfig, the body
// isn't hardcoded to base_branch: main, since these tests need other values.
func writeReworkRepoConfig(t *testing.T, dir, yaml string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".argus"), 0o755); err != nil {
		t.Fatalf("mkdir .argus: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".argus", "config.yml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("writing config.yml: %v", err)
	}
}

// TestBuildReworkConfigResolvesBaseFromRepoConfig is the regression test for
// the reported bug: buildReworkConfig loaded repoconfig but set Config.Base
// straight from the flag, never consulting base_branch, so rework always
// diffed against origin/main even on a repo whose trunk was "develop". Proof
// here is indirect but exact: the fixture's only real remote-tracking ref is
// origin/develop — if buildReworkConfig fell back to the unresolved
// "origin/main" flag value (the pre-fix behavior), VerifyBaseRef would reject
// it and this call would error instead of returning origin/develop.
func TestBuildReworkConfigResolvesBaseFromRepoConfig(t *testing.T) {
	dir := initGitDirWithDiff(t)
	writeReworkRepoConfig(t, dir, "base_branch: \"develop\"\n")
	updateRefOriginBranch(t, dir, "develop")

	cfg, _, _, err := buildReworkConfig(context.Background(), io.Discard, &reworkOpts{worktree: dir, base: "origin/main", maxRounds: supervisor.DefaultMaxReworkRounds}, &fakeReviewer{}, reworkLogger())
	if err != nil {
		t.Fatalf("buildReworkConfig: %v", err)
	}
	if cfg.Base != "origin/develop" {
		t.Errorf("Config.Base = %q, want origin/develop from .argus/config.yml base_branch, not the unresolved origin/main flag default", cfg.Base)
	}
	if cfg.BaseSource != supervisor.BaseSourceConfig {
		t.Errorf("Config.BaseSource = %q, want %q", cfg.BaseSource, supervisor.BaseSourceConfig)
	}
}

// TestBuildReworkConfigPropagatesRepoRootError proves buildReworkConfig
// wraps and returns a supervisor.RepoRoot failure instead of proceeding with
// an unresolved repo root.
func TestBuildReworkConfigPropagatesRepoRootError(t *testing.T) {
	notARepo := t.TempDir()
	_, _, _, err := buildReworkConfig(context.Background(), io.Discard, &reworkOpts{worktree: notARepo}, &fakeReviewer{}, reworkLogger())
	if err == nil {
		t.Fatal("want an error when the worktree isn't a git repo, got nil")
	}
}

// TestBuildReworkConfigPropagatesRepoConfigLoadError proves a malformed
// .argus/config.yml fails buildReworkConfig instead of silently falling
// through with a zero Config.
func TestBuildReworkConfigPropagatesRepoConfigLoadError(t *testing.T) {
	dir := initGitDir(t)
	writeReworkRepoConfig(t, dir, "base_branch: [unterminated\n")
	_, _, _, err := buildReworkConfig(context.Background(), io.Discard, &reworkOpts{worktree: dir}, &fakeReviewer{}, reworkLogger())
	if err == nil {
		t.Fatal("want an error for a malformed .argus/config.yml, got nil")
	}
}

// TestBuildReworkConfigPropagatesHomeDirError proves buildReworkConfig
// surfaces an os.UserHomeDir failure rather than proceeding with an empty
// Home.
func TestBuildReworkConfigPropagatesHomeDirError(t *testing.T) {
	t.Setenv("HOME", "")
	dir := initGitDir(t)
	_, _, _, err := buildReworkConfig(context.Background(), io.Discard, &reworkOpts{worktree: dir}, &fakeReviewer{}, reworkLogger())
	if err == nil {
		t.Fatal("want an error when $HOME is unset, got nil")
	}
}

// TestBuildReworkConfigPropagatesGatePolicyError proves an invalid
// --max-diff-lines fails buildReworkConfig instead of silently accepting a
// nonsensical negative diff ceiling.
func TestBuildReworkConfigPropagatesGatePolicyError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	_, _, _, err := buildReworkConfig(context.Background(), io.Discard, &reworkOpts{
		worktree: dir,
		gate:     gateFlags{maxDiffLines: -1, maxDiffLinesExplicit: true},
	}, &fakeReviewer{}, reworkLogger())
	if err == nil {
		t.Fatal("want an error for a negative --max-diff-lines, got nil")
	}
}

// TestBuildReworkConfigResolvesMaxRoundsFromRepoConfig proves rework.max_rounds
// overrides opts.maxRounds when --max-rounds wasn't explicitly passed — the
// per-invocation loop ceiling, distinct from ReworkBudget's own cross-invocation
// restart budget.
func TestBuildReworkConfigResolvesMaxRoundsFromRepoConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	writeReworkRepoConfig(t, dir, "rework:\n  max_rounds: 7\n")

	opts := &reworkOpts{worktree: dir, maxRounds: supervisor.DefaultMaxReworkRounds}
	_, _, _, err := buildReworkConfig(context.Background(), io.Discard, opts, &fakeReviewer{}, reworkLogger())
	if err != nil {
		t.Fatalf("buildReworkConfig: %v", err)
	}
	if opts.maxRounds != 7 {
		t.Errorf("opts.maxRounds = %d, want 7 from .argus/config.yml rework.max_rounds", opts.maxRounds)
	}
}

// TestBuildReworkConfigExplicitMaxRoundsFlagWinsOverRepoConfig proves an
// explicit --max-rounds flag is not overridden by rework.max_rounds, the same
// explicit-flag-wins precedence every other resolver in cmd/gatepolicy.go
// gives its own source.
func TestBuildReworkConfigExplicitMaxRoundsFlagWinsOverRepoConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	writeReworkRepoConfig(t, dir, "rework:\n  max_rounds: 7\n")

	opts := &reworkOpts{worktree: dir, maxRounds: 2, maxRoundsExplicit: true}
	_, _, _, err := buildReworkConfig(context.Background(), io.Discard, opts, &fakeReviewer{}, reworkLogger())
	if err != nil {
		t.Fatalf("buildReworkConfig: %v", err)
	}
	if opts.maxRounds != 2 {
		t.Errorf("opts.maxRounds = %d, want the explicit --max-rounds value 2, not repo config's 7", opts.maxRounds)
	}
}

// TestBuildReworkConfigRejectsNonPositiveResolvedMaxRounds proves a
// rework.max_rounds of 0 (or negative) is a hard error naming the bad value —
// unlike ReworkBudget, 0 has no "disabled" meaning for max_rounds, since a
// loop that runs zero rounds can never dispatch a worker at all.
func TestBuildReworkConfigRejectsNonPositiveResolvedMaxRounds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	writeReworkRepoConfig(t, dir, "rework:\n  max_rounds: 0\n")

	opts := &reworkOpts{worktree: dir, maxRounds: supervisor.DefaultMaxReworkRounds}
	_, _, _, err := buildReworkConfig(context.Background(), io.Discard, opts, &fakeReviewer{}, reworkLogger())
	if err == nil {
		t.Fatal("want an error for a resolved max_rounds of 0, got nil")
	}
	if !strings.Contains(err.Error(), "max_rounds") {
		t.Errorf("error = %q, want it to mention max_rounds", err.Error())
	}
}

// TestRunReworkDryRunShowsConfigResolvedBase pins the review fix: --dry-run
// must preview the same base ref a real run would actually diff against —
// resolved via ResolveGateBase from .argus/config.yml's base_branch — not
// the raw, unresolved --base flag default. Before this fix, --dry-run never
// called buildReworkConfig at all, so it always previewed the literal flag
// value regardless of repo config, breaking parity with supervise's own
// --dry-run (which does resolve through config).
func TestRunReworkDryRunShowsConfigResolvedBase(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDirWithDiff(t)
	writeReworkRepoConfig(t, dir, "base_branch: \"develop\"\n")
	updateRefOriginBranch(t, dir, "develop")
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	cmd, buf := testCmd()
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		t.Fatalf("unexpected herdr call during --dry-run: %v", args)
		return nil, nil
	})

	err := runRework(cmd, client, &fakeReviewer{}, reworkLogger(), &reworkOpts{
		worktree: dir, base: "origin/main", maxRounds: 3, dryRun: true,
	})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "base:       origin/develop") {
		t.Errorf("dry-run plan should show the config-resolved base origin/develop, not the unresolved origin/main flag default:\n%s", out)
	}
	if strings.Contains(out, "origin/main") {
		t.Errorf("dry-run plan must not show the unresolved flag default origin/main:\n%s", out)
	}
}

// TestReworkAndSuperviseAgreeOnGateBase pins that rework's real
// buildReworkConfig code path and supervise's own base-resolution call shape
// (cmd/supervise.go's newSuperviseCmd RunE) land on the identical ref and
// source for the same repo/config — both now funnel through the one shared
// supervisor.ResolveGateBase helper, so they can't independently drift apart
// again the way they did before this fix.
func TestReworkAndSuperviseAgreeOnGateBase(t *testing.T) {
	dir := initGitDirWithDiff(t)
	writeReworkRepoConfig(t, dir, "base_branch: \"develop\"\n")
	updateRefOriginBranch(t, dir, "develop")

	reworkCfg, repoRoot, _, err := buildReworkConfig(context.Background(), io.Discard, &reworkOpts{worktree: dir, base: "origin/main", maxRounds: supervisor.DefaultMaxReworkRounds}, &fakeReviewer{}, reworkLogger())
	if err != nil {
		t.Fatalf("buildReworkConfig: %v", err)
	}

	rc, err := repoconfig.Load(repoconfig.Path(repoRoot))
	if err != nil {
		t.Fatalf("repoconfig.Load: %v", err)
	}
	superviseBase := supervisor.ResolveGateBase(context.Background(), false, "origin/main", repoRoot, &rc)

	if reworkCfg.Base != superviseBase.Ref || reworkCfg.BaseSource != superviseBase.Source {
		t.Errorf("rework resolved base=%q source=%q, supervise resolved base=%q source=%q — must agree",
			reworkCfg.Base, reworkCfg.BaseSource, superviseBase.Ref, superviseBase.Source)
	}
}

// TestRunReworkFailsFastOnUnresolvableBaseRef pins the fail-fast fix: an
// unresolvable --base must error before any round dispatches (no herdr call
// at all) with a message naming the bad ref and its source, rather than
// dispatching a round that would later surface as an opaque per-worker
// measure_diff failure indistinguishable from a real review escalation.
func TestRunReworkFailsFastOnUnresolvableBaseRef(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	cmd, _ := testCmd()
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		t.Fatalf("unexpected herdr call: an unresolvable base ref must fail before any dispatch: %v", args)
		return nil, nil
	})

	err := runRework(cmd, client, &fakeReviewer{}, reworkLogger(), &reworkOpts{
		worktree: dir, base: "origin/does-not-exist", baseExplicit: true, maxRounds: 3,
	})
	userErr, ok := errors.AsType[*ui.UserError](err)
	if !ok {
		t.Fatalf("want a *ui.UserError for an unresolvable base ref, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Error(), `"origin/does-not-exist" does not exist`) {
		t.Errorf("error = %q, want it to name the unresolvable ref", userErr.Error())
	}
	if !strings.Contains(userErr.Error(), "resolved from: flag") {
		t.Errorf("error = %q, want it to name the source (flag)", userErr.Error())
	}
	// A fail-fast base-ref error must never reach the gate and overwrite the
	// seeded verdict with a gate escalation.
	if a, found, aerr := protocol.LoadApproval(dir); aerr != nil || !found || a.Source != "review" {
		t.Errorf("verdict should be unchanged from the seeded one, got found=%v approval=%+v err=%v", found, a, aerr)
	}
}

func TestRunReworkNoVerdictErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	cmd, _ := testCmd()

	err := runRework(cmd, herdr.New(), &fakeReviewer{}, reworkLogger(), &reworkOpts{worktree: dir, base: "feat-x", maxRounds: 3})
	if err == nil || !strings.Contains(err.Error(), "no argus verdict") {
		t.Fatalf("want a no-verdict error, got %v", err)
	}
}

func TestRunReworkAlreadyApprovedIsNoop(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: true, Source: "review", Summary: "ok"}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	cmd, buf := testCmd()

	// A client that errors on any call proves runRework never dispatches once
	// it sees an already-approving verdict.
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		t.Fatalf("unexpected herdr call for an already-approved worktree: %v", args)
		return nil, nil
	})

	err := runRework(cmd, client, &fakeReviewer{}, reworkLogger(), &reworkOpts{worktree: dir, base: "feat-x", maxRounds: 3})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	if !strings.Contains(buf.String(), "nothing to rework") {
		t.Errorf("expected a nothing-to-rework message:\n%s", buf.String())
	}
}

func TestRunReworkDryRunPrintsPlanWithoutDispatching(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	cmd, buf := testCmd()

	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		t.Fatalf("unexpected herdr call during --dry-run: %v", args)
		return nil, nil
	})

	err := runRework(cmd, client, &fakeReviewer{}, reworkLogger(), &reworkOpts{worktree: dir, base: "feat-x", maxRounds: 3, dryRun: true})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	if !strings.Contains(buf.String(), "rework plan (dry run)") || !strings.Contains(buf.String(), "missing nil check") {
		t.Errorf("expected a dry-run plan with the recorded finding:\n%s", buf.String())
	}
}

func TestRunReworkMaxRoundsZeroOrNegativeErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}

	for _, n := range []int{0, -1} {
		cmd, _ := testCmd()
		client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
			t.Fatalf("unexpected herdr call for an invalid --max-rounds: %v", args)
			return nil, nil
		})
		err := runRework(cmd, client, &fakeReviewer{}, reworkLogger(), &reworkOpts{
			worktree: dir, base: "feat-x", maxRounds: n, maxRoundsExplicit: true,
		})
		if _, ok := errors.AsType[*ui.UserError](err); !ok {
			t.Fatalf("--max-rounds %d: want a *ui.UserError, got %v", n, err)
		}
	}
}

func TestRunReworkMaxRoundsDefaultsWhenOmitted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	cmd, buf := testCmd()

	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		t.Fatalf("unexpected herdr call during --dry-run: %v", args)
		return nil, nil
	})

	// maxRounds left at its zero value with maxRoundsExplicit false, mirroring
	// what happens when the flag is never passed on the command line.
	err := runRework(cmd, client, &fakeReviewer{}, reworkLogger(), &reworkOpts{worktree: dir, base: "feat-x", dryRun: true})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	if !strings.Contains(buf.String(), fmt.Sprintf("max-rounds: %d", supervisor.DefaultMaxReworkRounds)) {
		t.Errorf("expected the default max-rounds in the plan:\n%s", buf.String())
	}
}

// TestPreRoundContentHashFallsBackWithoutPriorMeasuredFiles covers the first
// round (or a legacy verdict recorded before MeasuredFiles existed): with no
// recorded set to read, preRoundContentHash must behave exactly as before —
// a fresh MeasureDiff against base, hashed.
func TestPreRoundContentHashFallsBackWithoutPriorMeasuredFiles(t *testing.T) {
	dir := initGitDirWithDiff(t)

	got := preRoundContentHash(context.Background(), "feat-x", dir, nil)

	_, files, err := supervisor.MeasureDiff(context.Background(), dir, "feat-x")
	if err != nil {
		t.Fatalf("MeasureDiff: %v", err)
	}
	want, err := supervisor.ContentHash(dir, files)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if got == "" || got != want {
		t.Errorf("preRoundContentHash = %q, want fresh-measure hash %q", got, want)
	}
}

// TestPreRoundContentHashUsesPriorMeasuredFilesSet pins the rework half of the
// ship fix: preRoundContentHash must hash the prior verdict's own recorded
// MeasuredFiles set, not a fresh re-measure — otherwise an artifact a prior
// round's gate-verify command left behind (not part of that verdict's own
// set) would make this round's zero-delta comparison see a different,
// spuriously larger file set than the one the prior verdict was bound to.
func TestPreRoundContentHashUsesPriorMeasuredFilesSet(t *testing.T) {
	dir := initGitDirWithDiff(t)
	_, files, err := supervisor.MeasureDiff(context.Background(), dir, "feat-x")
	if err != nil {
		t.Fatalf("MeasureDiff: %v", err)
	}
	prior := &protocol.Approval{MeasuredFiles: files}

	// Simulate an artifact left by the prior round's own gate-verify command,
	// not part of the verdict's recorded set.
	if werr := os.WriteFile(filepath.Join(dir, "artifact.out"), []byte("x\n"), 0o600); werr != nil {
		t.Fatal(werr)
	}

	got := preRoundContentHash(context.Background(), "feat-x", dir, prior)
	want, err := supervisor.ContentHash(dir, files)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if got != want {
		t.Errorf("preRoundContentHash = %q, want the recorded-set hash %q (ignoring the new artifact)", got, want)
	}
}

func TestRunReworkApprovesFirstRound(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDirWithDiff(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	cmd, buf := testCmd()
	client := fakeReworkClient(dir, reworkStatus())
	reviewer := &sequenceReviewer{results: []supervisor.ReviewResult{{Decision: "approve", Summary: "fixed"}}}

	err := runRework(cmd, client, reviewer, reworkLogger(), &reworkOpts{
		worktree: dir, base: "feat-x", maxRounds: 3, interval: 5 * time.Millisecond,
		gate: gateFlags{},
	})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	if reviewer.callCount() != 1 {
		t.Errorf("want exactly 1 review call, got %d", reviewer.callCount())
	}
	out := buf.String()
	if !strings.Contains(out, "round 1/3") || !strings.Contains(out, "approve") {
		t.Errorf("expected a round-1 approve report:\n%s", out)
	}
	if strings.Contains(out, "escalating") {
		t.Errorf("an approved round must not escalate:\n%s", out)
	}
	approval, found, aerr := protocol.LoadApproval(dir)
	if aerr != nil || !found || !approval.Approved {
		t.Errorf("want a persisted approved verdict, found=%v approval=%+v err=%v", found, approval, aerr)
	}
}

// TestRunReworkBuildsReviewerFromRepoConfigWhenNil covers rework's lazy
// reviewer construction: RunE passes a nil reviewer (it can't resolve
// review_effort's flag/config precedence before .argus/config.yml is loaded
// inside runRework), so runRework must build one itself once rc is read, and
// resolveReviewEffort's own precedence (explicit flag > config > default)
// must still hold at that later construction point.
func TestRunReworkBuildsReviewerFromRepoConfigWhenNil(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDirWithDiff(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".argus"), 0o755); err != nil {
		t.Fatalf("mkdir .argus: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".argus", "config.yml"), []byte("review_effort: \"high\"\n"), 0o644); err != nil {
		t.Fatalf("writing config.yml: %v", err)
	}

	type captured struct{ model, effort string }
	var got captured
	original := newReviewer
	newReviewer = func(model, effort string, _ *eventlog.Logger) supervisor.Reviewer {
		got = captured{model: model, effort: effort}
		return &fakeReviewer{res: supervisor.ReviewResult{Decision: "approve", Summary: "ok"}}
	}
	t.Cleanup(func() { newReviewer = original })

	cmd, _ := testCmd()
	client := fakeReworkClient(dir, reworkStatus())

	err := runRework(cmd, client, nil, reworkLogger(), &reworkOpts{
		worktree: dir, base: "feat-x", maxRounds: 3, interval: 5 * time.Millisecond,
		reviewModel: "sonnet", gate: gateFlags{},
	})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	if got.model != "sonnet" || got.effort != "high" {
		t.Errorf("newReviewer got model=%q effort=%q, want model=sonnet effort=high (from .argus/config.yml)", got.model, got.effort)
	}
}

// TestRunReworkExplicitEffortFlagWinsOverRepoConfig is the same setup as
// TestRunReworkBuildsReviewerFromRepoConfigWhenNil but with --review-effort
// passed explicitly, which must win over the repo's config value.
func TestRunReworkExplicitEffortFlagWinsOverRepoConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDirWithDiff(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".argus"), 0o755); err != nil {
		t.Fatalf("mkdir .argus: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".argus", "config.yml"), []byte("review_effort: \"high\"\n"), 0o644); err != nil {
		t.Fatalf("writing config.yml: %v", err)
	}

	var gotEffort string
	original := newReviewer
	newReviewer = func(_, effort string, _ *eventlog.Logger) supervisor.Reviewer {
		gotEffort = effort
		return &fakeReviewer{res: supervisor.ReviewResult{Decision: "approve", Summary: "ok"}}
	}
	t.Cleanup(func() { newReviewer = original })

	cmd, _ := testCmd()
	client := fakeReworkClient(dir, reworkStatus())

	err := runRework(cmd, client, nil, reworkLogger(), &reworkOpts{
		worktree: dir, base: "feat-x", maxRounds: 3, interval: 5 * time.Millisecond,
		reviewEffort: "low", reviewEffortExplicit: true, gate: gateFlags{},
	})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	if gotEffort != "low" {
		t.Errorf("newReviewer got effort=%q, want the explicit flag value \"low\" over the repo config's \"high\"", gotEffort)
	}
}

func TestRunReworkLoopsOnRequestChangesThenApproves(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDirWithDiff(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	cmd, buf := testCmd()
	client := fakeReworkClient(dir, reworkStatus())
	reviewer := &sequenceReviewer{results: []supervisor.ReviewResult{
		{Decision: "request-changes", Summary: "still wrong", Findings: []string{"nil check in foo.go"}},
		{Decision: "approve", Summary: "fixed now"},
	}}

	err := runRework(cmd, client, reviewer, reworkLogger(), &reworkOpts{
		worktree: dir, base: "feat-x", maxRounds: 3, interval: 5 * time.Millisecond,
		gate: gateFlags{},
	})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	if reviewer.callCount() != 2 {
		t.Fatalf("want exactly 2 review calls (one retry), got %d", reviewer.callCount())
	}
	out := buf.String()
	if !strings.Contains(out, "round 1/3") || !strings.Contains(out, "round 2/3") {
		t.Errorf("expected both rounds reported:\n%s", out)
	}
	approval, found, aerr := protocol.LoadApproval(dir)
	if aerr != nil || !found || !approval.Approved {
		t.Errorf("want a persisted approved verdict after round 2, found=%v approval=%+v err=%v", found, approval, aerr)
	}
}

// TestRunReworkSubtractsPriorMeasuredAcrossRounds is the regression test for
// InvalidateStatus deleting verdict.json before every rework round, which
// permanently defeated gateVerdict's under-report subtraction (see
// priorMeasured in loop.go) from round 2 onward: the worktree's real diff
// grows from 20 to 55 lines between round 1 and round 2, and round 2's
// self-report (35) is exactly that round's own delta since round 1's
// verdict, not the 55-line cumulative total — the same shape as the false
// under-report this bug produced in production. Without the fix, round 2's
// gate always sees priorMeasuredOK=false, compares the self-report against
// the full 55-line cumulative diff instead, and flags an unwaivable
// "under-reported diff" that keeps the final verdict not-approved even
// though the reviewer approves.
func TestRunReworkSubtractsPriorMeasuredAcrossRounds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	f := filepath.Join(dir, "f.txt")

	statusFor := func(round int) *protocol.Status {
		switch round {
		case 1:
			writeLinesFile(t, f, 20)
			return &protocol.Status{
				Phase:    protocol.PhaseAwaitingReview,
				Tests:    []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultFail}},
				DiffStat: protocol.DiffStat{Files: 1, Insertions: 20},
			}
		default:
			writeLinesFile(t, f, 55)
			return &protocol.Status{
				Phase:    protocol.PhaseAwaitingReview,
				Tests:    []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultFail}},
				DiffStat: protocol.DiffStat{Files: 1, Insertions: 35},
			}
		}
	}

	cmd, buf := testCmd()
	client := fakeReworkClientRounds(dir, statusFor)
	reviewer := &sequenceReviewer{results: []supervisor.ReviewResult{
		{Decision: "request-changes", Summary: "still missing something", Findings: []string{"finding"}},
		{Decision: "approve", Summary: "delta looks right"},
	}}

	err := runRework(cmd, client, reviewer, reworkLogger(), &reworkOpts{
		worktree: dir, base: "feat-x", maxRounds: 3, interval: 5 * time.Millisecond,
		gate: gateFlags{},
	})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	if reviewer.callCount() != 2 {
		t.Fatalf("want exactly 2 review calls, got %d", reviewer.callCount())
	}
	out := buf.String()
	if strings.Contains(out, "under-reported") {
		t.Errorf("round 2's self-report matches its own delta since round 1's verdict — must not be flagged as an under-report:\n%s", out)
	}
	approval, found, aerr := protocol.LoadApproval(dir)
	if aerr != nil || !found || !approval.Approved {
		t.Errorf("want a persisted approved verdict once round 2's delta-only self-report clears the gate, found=%v approval=%+v err=%v", found, approval, aerr)
	}
}

func TestRunReworkExhaustsRoundsAndEscalates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	cmd, buf := testCmd()
	client := fakeReworkClient(dir, reworkStatus())
	reviewer := &sequenceReviewer{results: []supervisor.ReviewResult{
		{Decision: "request-changes", Summary: "still wrong", Findings: []string{"finding"}},
	}} // repeats for every round

	err := runRework(cmd, client, reviewer, reworkLogger(), &reworkOpts{
		worktree: dir, base: "feat-x", maxRounds: 2, interval: 5 * time.Millisecond,
		gate: gateFlags{},
	})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	if reviewer.callCount() != 2 {
		t.Fatalf("want exactly 2 review calls (max-rounds), got %d", reviewer.callCount())
	}
	out := buf.String()
	if !strings.Contains(out, "rework rounds exhausted (2/2)") {
		t.Errorf("expected an exhausted-rounds escalation message:\n%s", out)
	}
	approval, found, aerr := protocol.LoadApproval(dir)
	if aerr != nil || !found || approval.Approved {
		t.Errorf("want a persisted not-approved verdict after exhausting rounds, found=%v approval=%+v err=%v", found, approval, aerr)
	}
}

// TestRunReworkBudgetTripsMidInvocation covers a --max-rework-budget smaller
// than --max-rounds: the budget must cut the loop short before max-rounds
// would, escalating with a distinct rework-budget-exceeded verdict instead of
// dispatching a round the budget has already exhausted.
func TestRunReworkBudgetTripsMidInvocation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	cmd, buf := testCmd()
	client := fakeReworkClient(dir, reworkStatus())
	reviewer := &sequenceReviewer{results: []supervisor.ReviewResult{
		{Decision: "request-changes", Summary: "still wrong", Findings: []string{"finding"}},
	}} // repeats for every round

	err := runRework(cmd, client, reviewer, reworkLogger(), &reworkOpts{
		worktree: dir, base: "feat-x", maxRounds: 3, maxReworkBudget: 1, maxReworkBudgetExplicit: true,
		interval: 5 * time.Millisecond, gate: gateFlags{},
	})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	if reviewer.callCount() != 1 {
		t.Fatalf("want exactly 1 review call (budget of 1), got %d", reviewer.callCount())
	}
	out := buf.String()
	if !strings.Contains(out, "rework budget exceeded") {
		t.Errorf("expected a budget-exceeded escalation message:\n%s", out)
	}
	approval, found, aerr := protocol.LoadApproval(dir)
	if aerr != nil || !found || approval.Approved {
		t.Errorf("want a persisted not-approved verdict, found=%v approval=%+v err=%v", found, approval, aerr)
	}
	if approval.Provenance() != protocol.ProvenanceReworkBudgetExceeded {
		t.Errorf("Provenance() = %q, want %q", approval.Provenance(), protocol.ProvenanceReworkBudgetExceeded)
	}
}

// TestRunReworkBudgetPersistsAcrossInvocations covers the actual gap this
// budget closes: a supervisor that keeps re-invoking `argus rework` against
// the same worktree after each invocation's own --max-rounds gives up must
// eventually be refused, not get a fresh --max-rounds allowance every time.
// The first call exhausts a 1-round budget; the second call — a brand new
// runRework invocation, as a supervisor's repeated CLI call would be — must
// dispatch zero further rounds and escalate immediately.
func TestRunReworkBudgetPersistsAcrossInvocations(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	reviewer := &sequenceReviewer{results: []supervisor.ReviewResult{
		{Decision: "request-changes", Summary: "still wrong", Findings: []string{"finding"}},
	}} // repeats for every round

	opts := func() *reworkOpts {
		return &reworkOpts{
			worktree: dir, base: "feat-x", maxRounds: 1, maxReworkBudget: 1, maxReworkBudgetExplicit: true,
			interval: 5 * time.Millisecond, gate: gateFlags{},
		}
	}

	cmd1, _ := testCmd()
	if err := runRework(cmd1, fakeReworkClient(dir, reworkStatus()), reviewer, reworkLogger(), opts()); err != nil {
		t.Fatalf("first runRework: %v", err)
	}
	if reviewer.callCount() != 1 {
		t.Fatalf("first invocation: want exactly 1 review call, got %d", reviewer.callCount())
	}

	cmd2, buf2 := testCmd()
	// A client whose PaneRun/AgentPrompt would fail the test if called — the
	// second invocation must never dispatch a round at all.
	client2 := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "worktree" {
			return fmt.Appendf(nil, `{"result":{"root_pane":{"pane_id":%q}}}`, reworkTestPaneID), nil
		}
		t.Fatalf("second invocation dispatched a round via %v; the exhausted budget should have refused before any dispatch", args)
		return nil, errors.New("unreachable")
	})
	if err := runRework(cmd2, client2, reviewer, reworkLogger(), opts()); err != nil {
		t.Fatalf("second runRework: %v", err)
	}
	if reviewer.callCount() != 1 {
		t.Errorf("second invocation: want no further review calls, got %d total", reviewer.callCount())
	}
	if !strings.Contains(buf2.String(), "rework budget exceeded") {
		t.Errorf("expected a budget-exceeded escalation message on the second invocation:\n%s", buf2.String())
	}
	approval, found, aerr := protocol.LoadApproval(dir)
	if aerr != nil || !found || approval.Approved {
		t.Errorf("want a persisted not-approved verdict, found=%v approval=%+v err=%v", found, approval, aerr)
	}
}

// TestRunReworkBudgetExceededPreservesApprovingVerdict is the regression test
// for the reported bug: an explicit --findings call for an unrelated
// follow-up bypasses prepareReworkRun's "already approved, nothing to
// rework" short-circuit, and can land on an already-exhausted cumulative
// budget before dispatching a single round. Refusing must not overwrite the
// worktree's existing approving verdict — ship must still see it as
// approved, since nothing about the approved change was re-examined.
func TestRunReworkBudgetExceededPreservesApprovingVerdict(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: true, Source: "review", Summary: "looks good"}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	if err := protocol.WriteReworkState(dir, &protocol.ReworkState{RoundsAttempted: 3}); err != nil {
		t.Fatalf("seeding rework state: %v", err)
	}
	cmd, buf := testCmd()
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		t.Fatalf("unexpected herdr call: an already-exhausted budget must refuse before any dispatch: %v", args)
		return nil, nil
	})

	err := runRework(cmd, client, &fakeReviewer{}, reworkLogger(), &reworkOpts{
		worktree: dir, base: "feat-x", maxRounds: 3, maxReworkBudget: 3, maxReworkBudgetExplicit: true,
		findings: []string{"unrelated small follow-up"},
	})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	if !strings.Contains(buf.String(), "rework budget exceeded") {
		t.Errorf("expected a budget-exceeded refusal message:\n%s", buf.String())
	}
	approval, found, aerr := protocol.LoadApproval(dir)
	if aerr != nil || !found || !approval.Approved || approval.Summary != "looks good" {
		t.Errorf("want the prior approving verdict preserved untouched, found=%v approval=%+v err=%v", found, approval, aerr)
	}
}

func TestRunReworkNeedsHumanEscalatesImmediately(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	cmd, buf := testCmd()
	client := fakeReworkClient(dir, reworkStatus())
	reviewer := &sequenceReviewer{results: []supervisor.ReviewResult{
		{Decision: "needs-human", Summary: "can't tell"},
		{Decision: "request-changes", Summary: "should never be reached"},
	}}

	err := runRework(cmd, client, reviewer, reworkLogger(), &reworkOpts{
		worktree: dir, base: "feat-x", maxRounds: 3, interval: 5 * time.Millisecond,
		gate: gateFlags{},
	})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	if reviewer.callCount() != 1 {
		t.Errorf("want rework to stop after round 1 on needs-human, got %d review calls", reviewer.callCount())
	}
	if !strings.Contains(buf.String(), "needs-human") {
		t.Errorf("expected a needs-human escalation message:\n%s", buf.String())
	}
}

func TestRunReworkStopsImmediatelyWhenWorkerReportsBlocked(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	cmd, buf := testCmd()
	blocked := &protocol.Status{Phase: protocol.PhaseBlocked, BlockedReason: "need a design decision"}
	client := fakeReworkClient(dir, blocked)
	reviewer := &sequenceReviewer{results: []supervisor.ReviewResult{{Decision: "approve", Summary: "should never be reached"}}}

	err := runRework(cmd, client, reviewer, reworkLogger(), &reworkOpts{
		worktree: dir, base: "feat-x", maxRounds: 3, interval: 5 * time.Millisecond,
		gate: gateFlags{},
	})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	if reviewer.callCount() != 0 {
		t.Errorf("a blocked worker must not reach the reviewer, got %d calls", reviewer.callCount())
	}
	if !strings.Contains(buf.String(), "need a design decision") {
		t.Errorf("expected the blocked reason surfaced:\n%s", buf.String())
	}
}

// fakeReworkClientStuckPane is fakeReworkClient's spawn-a-fresh-worker path
// (the worktree starts with no live pane) plus a "pane list" that always
// reports agentStatus for reworkTestPaneID — used to drive WaitForStatus's
// paneStuckTracker into escalating instead of the dispatched worker ever
// writing status.json, mirroring TestDispatchRebaseWorkerHerdrStuckFailsFast.
func fakeReworkClientStuckPane(agentStatus string) herdr.Client {
	var mu sync.Mutex
	var spawned bool
	return herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "worktree":
			return fmt.Appendf(nil, `{"result":{"root_pane":{"pane_id":%q}}}`, reworkTestPaneID), nil
		case len(args) > 1 && args[0] == "agent" && args[1] == "get":
			mu.Lock()
			live := spawned
			mu.Unlock()
			if !live {
				return nil, fmt.Errorf("herdr agent get: %w", herdr.ErrAgentNotFound)
			}
			return fmt.Appendf(nil, `{"result":{"agent":{"pane_id":%q,"agent":"claude","agent_status":%q}}}`, reworkTestPaneID, agentStatus), nil
		case len(args) > 1 && args[0] == "pane" && args[1] == "run":
			mu.Lock()
			spawned = true
			mu.Unlock()
			return []byte(`{"result":{}}`), nil
		case len(args) > 1 && args[0] == "pane" && args[1] == "list":
			return fmt.Appendf(nil, `{"result":{"panes":[{"pane_id":%q,"agent_status":%q}]}}`, reworkTestPaneID, agentStatus), nil
		default:
			return []byte(`{"result":{}}`), nil
		}
	})
}

// TestRunReworkHerdrStuckFailsFast is the regression test for argus issue
// #383: a worktree with no live pane falls back to spawning a fresh worker
// (see dispatchIntoPane), and a launcher that wedges on an interactive
// prompt herdr reports as "blocked" must fail fast with a clear error
// instead of runRework hanging on WaitForStatus forever. A 3-minute interval
// — comfortably over internal/supervisor's own herdrStuckThreshold (2
// minutes) — makes WaitForStatus's very first tick (fired immediately, see
// time.NewTimer(0)) already cross the threshold, so this resolves in
// milliseconds of real time rather than actually waiting out the threshold.
func TestRunReworkHerdrStuckFailsFast(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	cmd, _ := testCmd()
	client := fakeReworkClientStuckPane("blocked")

	err := runRework(cmd, client, &fakeReviewer{}, reworkLogger(), &reworkOpts{
		worktree: dir, base: "feat-x", maxRounds: 3, interval: 3 * time.Minute,
		gate: gateFlags{},
	})
	if err == nil || !strings.Contains(err.Error(), "agent_status") {
		t.Fatalf("want a herdr-stuck escalation error, got %v", err)
	}
}

// TestRestoreTitleAcrossRound covers restoreTitleAcrossRound directly: it
// must persist the restored title to status.json on disk, not just correct
// the in-memory struct, since ship reads the title back off disk in a wholly
// separate process invocation with no access to this round's state.
func TestRestoreTitleAcrossRound(t *testing.T) {
	cases := []struct {
		name        string
		prior       string
		statusTitle string
		wantFinal   string
		wantWrite   bool
	}{
		{"unchanged", "feat: thing", "feat: thing", "feat: thing", false},
		{"no prior title", "", "feat: thing", "feat: thing", false},
		{"round left it empty", "feat: original feature", "", "feat: original feature", true},
		{"round narrowed it", "feat: original feature", "fix: narrow round nit", "feat: original feature", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wt := t.TempDir()
			status := &protocol.Status{Title: tc.statusTitle}
			var buf bytes.Buffer
			if err := restoreTitleAcrossRound(&buf, wt, 2, 3, tc.prior, status); err != nil {
				t.Fatalf("restoreTitleAcrossRound: %v", err)
			}
			if status.Title != tc.wantFinal {
				t.Errorf("status.Title = %q, want %q", status.Title, tc.wantFinal)
			}
			onDisk, err := protocol.Load(protocol.StatusPath(wt))
			if tc.wantWrite {
				if err != nil {
					t.Fatalf("expected status.json to be written, Load failed: %v", err)
				}
				if onDisk.Title != tc.wantFinal {
					t.Errorf("on-disk Title = %q, want %q", onDisk.Title, tc.wantFinal)
				}
				if !strings.Contains(buf.String(), "keeping original title") {
					t.Errorf("expected a note about keeping the original title, got:\n%s", buf.String())
				}
			} else if err == nil {
				t.Errorf("expected no status.json write, but found one with Title %q", onDisk.Title)
			}
		})
	}
}

// TestRunReworkRestoresOriginalTitleAcrossRealInvalidateStatus is the
// end-to-end pin for issue #282: it runs the real runRework -> runReworkRound
// -> dispatchReworkRound path (only the herdr client is faked), so it
// actually exercises supervisor.InvalidateStatus deleting status.json before
// the round's dispatch — the exact deletion that made runWorkerReport's
// cur.Title carry-forward alone insufficient, since cur is loaded after the
// file is already gone. A rework round whose own report names a narrower
// title (describing only that round's fix) must not leave that narrower
// title as what's on disk once the round completes.
func TestRunReworkRestoresOriginalTitleAcrossRealInvalidateStatus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDirWithDiff(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	originalTitle := "feat: interactive shell-completion installer for argus completion"
	seed := &protocol.Status{Title: originalTitle}
	if err := protocol.Write(protocol.StatusPath(dir), seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	cmd, buf := testCmd()
	retitled := reworkStatus()
	retitled.Title = "fix: isolate HOME in runUpdate completion-refresh test"
	client := fakeReworkClient(dir, retitled)
	reviewer := &sequenceReviewer{results: []supervisor.ReviewResult{{Decision: "approve", Summary: "fixed"}}}

	err := runRework(cmd, client, reviewer, reworkLogger(), &reworkOpts{
		worktree: dir, base: "feat-x", maxRounds: 3, interval: 5 * time.Millisecond,
		gate: gateFlags{},
	})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	got, err := protocol.Load(protocol.StatusPath(dir))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Title != originalTitle {
		t.Errorf("status.json Title = %q after rework round, want the original PR title %q preserved (this is exactly what ship reads to title the PR/commit)", got.Title, originalTitle)
	}
	if !strings.Contains(buf.String(), "keeping original title") {
		t.Errorf("expected rework's own output to note the restore:\n%s", buf.String())
	}
}

func TestReworkOptsDispatchTargetCopiesFields(t *testing.T) {
	o := &reworkOpts{
		worktree: "/wt", launcher: "claude", workerRuntime: "podman", noCredProxy: true,
		credentialEnv:   map[string]string{"github.com": "MY_TOKEN"},
		livenessTimeout: time.Second, livenessInterval: time.Millisecond,
	}
	cfg := &supervisor.Config{
		RepoPhases: protocol.PhaseConfig{protocol.PhaseWorking: protocol.PhasePolicy{Allow: []string{"Bash(make test*)"}}},
		RepoAllow:  []string{"Bash(make build*)"}, ExperimentalSandbox: true, SandboxAllowWrite: []string{"/tmp/cache"},
	}
	rebaseAllow := []string{"Bash(git fetch origin main)"}
	target := o.dispatchTarget("AP-1207: fix DELETE endpoint", cfg, rebaseAllow)
	if target.worktree != o.worktree || target.launcher != o.launcher || target.workerRuntime != o.workerRuntime ||
		target.noCredProxy != o.noCredProxy || target.livenessTimeout != o.livenessTimeout || target.livenessInterval != o.livenessInterval {
		t.Errorf("dispatchTarget() = %+v, want it to mirror reworkOpts %+v", target, o)
	}
	if target.credentialEnv["github.com"] != "MY_TOKEN" {
		t.Errorf("dispatchTarget() dropped credentialEnv: %+v", target)
	}
	if target.label != "AP-1207" {
		t.Errorf("dispatchTarget() label = %q, want the ticket key extracted from task", target.label)
	}
	if !slices.Equal(target.repoAllow, cfg.RepoAllow) || !slices.Equal(target.sandboxAllowWrite, cfg.SandboxAllowWrite) ||
		target.experimentalSandbox != cfg.ExperimentalSandbox || !slices.Equal(target.rebaseAllow, rebaseAllow) {
		t.Errorf("dispatchTarget() = %+v, want it to carry cfg's settings-render fields and the caller's own rebaseAllow", target)
	}
	if len(target.repoPhases[protocol.PhaseWorking].Allow) != 1 || target.repoPhases[protocol.PhaseWorking].Allow[0] != "Bash(make test*)" {
		t.Errorf("dispatchTarget() repoPhases = %+v, want cfg.RepoPhases carried through", target.repoPhases)
	}
}

// TestReworkOptsDispatchTargetLabelEmptyWithoutTicketKey covers a task with
// no ticket-key prefix: dispatchTarget's label must stay "" so
// relabelFreshPane leaves herdr's own default label alone, rather than
// re-deriving a branch-slug fallback here.
func TestReworkOptsDispatchTargetLabelEmptyWithoutTicketKey(t *testing.T) {
	o := &reworkOpts{worktree: "/wt"}
	target := o.dispatchTarget("fix the DELETE endpoint, no ticket prefix", &supervisor.Config{}, nil)
	if target.label != "" {
		t.Errorf("dispatchTarget() label = %q, want \"\" for a task with no ticket-key prefix", target.label)
	}
}

// TestRespawnRebaseAllowEmptyBaseReturnsNil covers a worktree with no
// recorded spawn base (no status.json yet, or one predating this field) —
// nothing to preserve, so no grant is computed.
func TestRespawnRebaseAllowEmptyBaseReturnsNil(t *testing.T) {
	if got := respawnRebaseAllow("", &supervisor.Config{GateVerifyCommand: "make verify"}); got != nil {
		t.Errorf("respawnRebaseAllow(\"\", ...) = %v, want nil", got)
	}
}

// TestRespawnRebaseAllowMirrorsProvisionWorktree confirms respawnRebaseAllow
// computes the identical grant provisionWorktree bakes in at spawn time
// (RebasePhaseAllow(baseBranch, "", cfg.GateVerifyCommand) — see
// internal/supervisor/loop.go) for the worktree's own recorded base, not
// opts.base.
func TestRespawnRebaseAllowMirrorsProvisionWorktree(t *testing.T) {
	got := respawnRebaseAllow("main", &supervisor.Config{GateVerifyCommand: "make verify"})
	want := supervisor.RebasePhaseAllow("main", "", "make verify")
	if !slices.Equal(got, want) {
		t.Errorf("respawnRebaseAllow() = %v, want %v", got, want)
	}
}

// TestRunReworkRespawnPreservesRebasePhaseGitGrant is the regression test for
// rework's own respawn re-render: without threading the worktree's persisted
// spawn base through dispatchReworkRound (see respawnRebaseAllow),
// dispatchIntoPane's own WriteSettings call would silently drop the
// rebase-phase git fetch/merge grant provisionWorktree originally baked into
// settings.local.json — a worktree respawned via rework, not rebase, must
// still keep it, since it's the same worktree provisionWorktree provisioned.
func TestRunReworkRespawnPreservesRebasePhaseGitGrant(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDirWithDiff(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	if err := protocol.Write(protocol.StatusPath(dir), &protocol.Status{Base: "feat-x"}); err != nil {
		t.Fatalf("seeding status.json with a spawn base: %v", err)
	}
	rebaseGrant := supervisor.RebasePhaseAllow("feat-x", "", "")
	if err := supervisor.WriteSettings(dir, nil, nil, nil, rebaseGrant, false, nil); err != nil {
		t.Fatalf("seeding settings.local.json as provisionWorktree would at spawn: %v", err)
	}

	cmd, _ := testCmd()
	client := fakeReworkClient(dir, reworkStatus())
	reviewer := &sequenceReviewer{results: []supervisor.ReviewResult{{Decision: "approve", Summary: "fixed"}}}

	err := runRework(cmd, client, reviewer, reworkLogger(), &reworkOpts{
		worktree: dir, base: "feat-x", maxRounds: 3, interval: 5 * time.Millisecond,
		gate: gateFlags{},
	})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatalf("reading settings.local.json: %v", err)
	}
	for _, want := range rebaseGrant {
		if !strings.Contains(string(data), want) {
			t.Errorf("settings.local.json should still carry the rebase-phase grant %q after rework's own respawn re-render, missing it:\n%s", want, data)
		}
	}
}

// TestReworkFindingsFlagIsVerbatimAndRepeatable is the regression for the
// reported bug: --findings used to be CSV-parsed, so a single finding's own
// commas split it into fragments and an embedded double quote failed the parse
// outright. As a repeatable non-CSV flag each value must reach the plan whole.
func TestReworkFindingsFlagIsVerbatimAndRepeatable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	const withCommas = `deletes status.json entirely before the round's worker report runs, breaking the carry-forward`
	const withQuote = `the snippet t.Setenv("HOME", ...) reproduces it`

	cmd := newReworkCmd()
	var buf bytes.Buffer
	cmd.SetContext(context.Background())
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--worktree", dir, "--base", "HEAD", "--dry-run", "--findings", withCommas, "--findings", withQuote})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("rework --dry-run: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "    - "+withCommas+"\n") {
		t.Errorf("comma-containing finding was split or altered:\n%s", out)
	}
	if !strings.Contains(out, "    - "+withQuote+"\n") {
		t.Errorf("quote-containing finding was dropped or altered:\n%s", out)
	}
}

// TestReworkFindingsFileAppends covers --findings-file: its lines are appended
// after --findings, each taken verbatim (newline-split only, never CSV), so a
// line's own commas stay intact.
func TestReworkFindingsFileAppends(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	file := filepath.Join(t.TempDir(), "findings.txt")
	if err := os.WriteFile(file, []byte("root cause, spanning a clause\n\nsecond finding\n"), 0o644); err != nil {
		t.Fatalf("writing findings file: %v", err)
	}

	cmd := newReworkCmd()
	var buf bytes.Buffer
	cmd.SetContext(context.Background())
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--worktree", dir, "--base", "HEAD", "--dry-run", "--findings", "flag finding", "--findings-file", file})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("rework --dry-run: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"    - flag finding\n", "    - root cause, spanning a clause\n", "    - second finding\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing finding %q in plan:\n%s", want, out)
		}
	}
	// A blank line in the file must not become an empty finding.
	if strings.Contains(out, "    - \n") {
		t.Errorf("blank line leaked as an empty finding:\n%s", out)
	}
}

func TestAppendFindingsFile(t *testing.T) {
	base := []string{"flag one"}

	got, err := appendFindingsFile(base, "")
	if err != nil || len(got) != 1 || got[0] != "flag one" {
		t.Errorf("empty path must be a no-op, got %v err=%v", got, err)
	}

	dir := t.TempDir()
	full := filepath.Join(dir, "ok.txt")
	if werr := os.WriteFile(full, []byte("a, with comma\r\nb\n"), 0o644); werr != nil {
		t.Fatalf("writing file: %v", werr)
	}
	got, err = appendFindingsFile(base, full)
	if err != nil {
		t.Fatalf("appendFindingsFile: %v", err)
	}
	if want := []string{"flag one", "a, with comma", "b"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	empty := filepath.Join(dir, "empty.txt")
	if werr := os.WriteFile(empty, []byte("\n\n"), 0o644); werr != nil {
		t.Fatalf("writing empty file: %v", werr)
	}
	if _, err := appendFindingsFile(base, empty); err == nil {
		t.Error("want an error for a file with no non-empty lines")
	}

	if _, err := appendFindingsFile(base, filepath.Join(dir, "missing.txt")); err == nil {
		t.Error("want an error for a missing file")
	}
}

// TestReworkVerifyCmdFlagDeprecatedAliasStillWorks pins the flag rename's
// backward-compat contract: --verify-cmd was renamed to
// --gate-verify-command, but the old flag name must still parse (bound to
// the same variable) and print a deprecation warning rather than either
// silently doing nothing or hard-erroring as an unknown flag.
func TestReworkVerifyCmdFlagDeprecatedAliasStillWorks(t *testing.T) {
	cmd := newReworkCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.ParseFlags([]string{"--verify-cmd", "make lint"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if !cmd.Flags().Changed("verify-cmd") {
		t.Error("Changed(\"verify-cmd\") = false, want true")
	}
	f := cmd.Flags().Lookup("gate-verify-command")
	if f == nil {
		t.Fatal("expected --gate-verify-command flag to be registered")
	}
	if got := f.Value.String(); got != "make lint" {
		t.Errorf("--gate-verify-command's bound value = %q, want %q (shared with --verify-cmd)", got, "make lint")
	}
	if !strings.Contains(buf.String(), "deprecated") || !strings.Contains(buf.String(), "gate-verify-command") {
		t.Errorf("output = %q, want a deprecation warning pointing at --gate-verify-command", buf.String())
	}
}

// TestReworkGateVerifyCommandFlagNoDeprecationWarning is the other half:
// the new flag name prints no warning and needs no old-name involvement.
func TestReworkGateVerifyCommandFlagNoDeprecationWarning(t *testing.T) {
	cmd := newReworkCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.ParseFlags([]string{"--gate-verify-command", "make lint"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cmd.Flags().Changed("verify-cmd") {
		t.Error("Changed(\"verify-cmd\") = true, want false — only the new flag was passed")
	}
	if buf.Len() != 0 {
		t.Errorf("output = %q, want no deprecation warning for the new flag name", buf.String())
	}
}
