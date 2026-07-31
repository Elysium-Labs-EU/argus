package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

// FetchBase updates the worktree's view of the base branch from origin, so a
// conflict check and rebase see the just-merged sibling changes.
func FetchBase(ctx context.Context, worktree, base string) error {
	_, err := git(ctx, worktree, "fetch", "origin", base)
	return err
}

// ConflictsWith reports whether the worktree's HEAD would conflict when rebased
// onto origin/<base>. git's own merge-tree is trusted for textual conflicts, but
// not as a proxy for "semantically safe to combine": two branches can each edit
// the same function without their edits textually overlapping (one inserts a
// guard clause immediately above a line the other renames, say), and git's
// context-based 3-way merge picks a side for that line without ever surfacing a
// conflict — silently dropping the other branch's edit. So a textually clean
// merge gets a second, cheaper check: if both branches' diffs against the same
// merge-base touch the same function in the same file, treat it as a conflict
// too. False positives here just cost an unnecessary worker dispatch; the false
// negative this replaces costs silently losing a branch's change.
func ConflictsWith(ctx context.Context, worktree, base string) (bool, error) {
	textConflict, err := gitMergeConflicts(ctx, worktree, base)
	if err != nil {
		return false, err
	}
	if textConflict {
		return true, nil
	}
	return sameFunctionTouchedByBoth(ctx, worktree, base)
}

// gitMergeConflicts reports whether HEAD would textually conflict when merged
// with origin/<base>. It uses `git merge-tree --write-tree`, which computes the
// merge without touching the working tree and exits non-zero (code 1) when the
// merge has conflicts.
func gitMergeConflicts(ctx context.Context, worktree, base string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", worktree, "merge-tree", "--write-tree", "origin/"+base, "HEAD") //nolint:gosec // fixed git binary; worktree/base argus-derived
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return true, nil // conflicts
		}
		return false, fmt.Errorf("checking merge conflicts: %w", err)
	}
	return false, nil
}

// sameFunctionTouchedByBoth reports whether HEAD and origin/<base> each carry a
// change (vs their shared merge-base) that touches the same function in the
// same file. It never inspects the merge result itself — only the two sides'
// independent diffs — so it catches the loss before it happens rather than
// after.
func sameFunctionTouchedByBoth(ctx context.Context, worktree, base string) (bool, error) {
	mergeBase, err := git(ctx, worktree, "merge-base", "HEAD", "origin/"+base)
	if err != nil {
		return false, fmt.Errorf("resolving merge base with origin/%s: %w", base, err)
	}
	ours, err := touchedFunctions(ctx, worktree, mergeBase, "HEAD")
	if err != nil {
		return false, err
	}
	theirs, err := touchedFunctions(ctx, worktree, mergeBase, "origin/"+base)
	if err != nil {
		return false, err
	}
	for file, funcs := range ours {
		theirFuncs, ok := theirs[file]
		if !ok {
			continue
		}
		for fn := range funcs {
			if theirFuncs[fn] {
				return true, nil
			}
		}
	}
	return false, nil
}

// touchedFunctions maps each file changed between from and to to the set of
// enclosing function (or other top-level declaration) names git's diff hunk
// headers report as touched. It relies on git's own funcname context — the
// text after the second "@@" in a unified diff hunk header, which by default
// is the nearest preceding line that looks like the start of a top-level
// declaration — rather than parsing the language itself, so it works across
// whatever languages a repo mixes in without argus needing a per-language
// parser.
func touchedFunctions(ctx context.Context, worktree, from, to string) (map[string]map[string]bool, error) {
	out, err := git(ctx, worktree, "diff", "--unified=0", from, to)
	if err != nil {
		return nil, fmt.Errorf("diffing %s..%s: %w", from, to, err)
	}
	return parseTouchedFunctions(out), nil
}

var (
	hunkHeaderRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+\d+(?:,\d+)? @@ ?(.*)$`)
	funcNameRe   = regexp.MustCompile(`(\w+)\s*\(`)
)

// parseTouchedFunctions extracts the per-file, per-function touch set out of a
// `git diff --unified=0` transcript (see touchedFunctions).
func parseTouchedFunctions(diff string) map[string]map[string]bool {
	touched := map[string]map[string]bool{}
	var file string
	for line := range strings.SplitSeq(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ "):
			file = strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "b/")
		case strings.HasPrefix(line, "@@"):
			m := hunkHeaderRe.FindStringSubmatch(line)
			if m == nil || file == "" {
				continue
			}
			fn := funcNameInContext(m[1])
			if fn == "" {
				continue
			}
			if touched[file] == nil {
				touched[file] = map[string]bool{}
			}
			touched[file][fn] = true
		}
	}
	return touched
}

// funcNameInContext pulls the likely declaration name out of a hunk header's
// context text (e.g. "func (s *Supervisor) reconcile(cfg *Config) error {" ->
// "reconcile"): the last identifier immediately followed by "(", which for a
// receiver method skips the receiver's own parenthesized group.
func funcNameInContext(hunkContext string) string {
	matches := funcNameRe.FindAllStringSubmatch(hunkContext, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1][1]
}

// HeadSHA resolves worktree's current HEAD commit.
func HeadSHA(ctx context.Context, worktree string) (string, error) {
	return git(ctx, worktree, "rev-parse", "HEAD")
}

// RemoteBranchSHA returns the commit SHA origin/<branch> currently points to,
// queried directly from the remote via `git ls-remote` rather than a local
// remote-tracking ref that only a fetch would refresh. Returns "" if the
// branch doesn't exist on origin.
func RemoteBranchSHA(ctx context.Context, worktree, branch string) (string, error) {
	out, err := git(ctx, worktree, "ls-remote", "origin", "refs/heads/"+branch)
	if err != nil {
		return "", fmt.Errorf("querying origin for %s: %w", branch, err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", nil
	}
	return fields[0], nil
}

// CommitsAheadOfBase counts worktree's local HEAD commits not reachable from
// the local origin/<base> remote-tracking ref. Zero means a force-push has
// nothing to publish: a rebase round that fast-forwards HEAD exactly onto
// base (a sibling PR already carried an identical change), or a worktree
// whose worker never made a commit of its own to begin with, both land here
// — distinct from a branch with real history that genuinely needs to reach
// origin. Deliberately uses the local ref rather than a fresh `ls-remote`
// (mirroring hasUnpushedCommits' @{u}..HEAD in prune.go): a live SHA queried
// mid-dispatch can advance past what FetchBase and the worker's own `git
// fetch origin <base>` step (see RebaseBrief) already pulled into the local
// object DB, and rev-list on an object git has never fetched fails outright
// — aborting a dispatch whose push actually landed fine.
func CommitsAheadOfBase(ctx context.Context, worktree, base string) (int, error) {
	out, err := git(ctx, worktree, "rev-list", "--count", "origin/"+base+"..HEAD")
	if err != nil {
		return 0, fmt.Errorf("counting commits ahead of origin/%s: %w", base, err)
	}
	n, err := strconv.Atoi(out)
	if err != nil {
		return 0, fmt.Errorf("parsing commit count %q: %w", out, err)
	}
	return n, nil
}

// ForcePushBranch force-pushes worktree's HEAD to origin/<branch>, using
// --force-with-lease so a remote change that landed after our last view of it
// is refused rather than silently clobbered.
func ForcePushBranch(ctx context.Context, worktree, branch string) error {
	_, err := git(ctx, worktree, "push", "--force-with-lease", "origin", "HEAD:refs/heads/"+branch)
	return err
}

// VerifyPushLanded confirms origin/<branch> equals worktree's local HEAD,
// querying the remote directly rather than trusting a caller's belief that a
// preceding push succeeded: a rebase worker can report awaiting_review after
// rebasing locally without its own `git push --force-with-lease` having
// actually reached origin (a pre-push hook rejection it never checked the
// exit code of, or a run killed mid-push), and nothing about a terminal
// status.json distinguishes that from a real success.
func VerifyPushLanded(ctx context.Context, worktree, branch string) error {
	local, err := HeadSHA(ctx, worktree)
	if err != nil {
		return fmt.Errorf("resolving local HEAD: %w", err)
	}
	remote, err := RemoteBranchSHA(ctx, worktree, branch)
	if err != nil {
		return err
	}
	if remote != local {
		return fmt.Errorf("origin/%s is at %s, not local HEAD %s — the push did not land", branch, remote, local)
	}
	return nil
}

// RebaseBrief is the task brief argus injects when dispatching a worker to
// resolve a post-merge conflict. The deterministic work (detecting the conflict,
// spawning the worker, verifying the result) is argus's; the conflict resolution
// itself needs the worker's judgment.
func RebaseBrief(branch, base string) string {
	return fmt.Sprintf(`Task: resolve a post-merge conflict on branch %s

A sibling change merged into %s first, so your branch now conflicts with it.
This is expected. Update your branch in place (do NOT open a new PR):

  git fetch origin %s
  git rebase origin/%s
  # resolve conflicts so BOTH your change and the merged change coexist
  # re-run the repo's checks (make ci, or make test + make lint)
  git push --force-with-lease

Confirm the checks pass and the branch is mergeable, then set your status phase to
"awaiting_review". Use "blocked" if the resolution needs a decision only the
supervisor can make.

%s`, branch, base, base, base, protocol.WriterBrief("origin/"+base))
}

// InvalidateStatus removes a worktree's status and verdict files, if present,
// before a rebase worker is dispatched into it. Without this, a worker
// re-dispatched into a worktree that already carries a terminal status.json
// from an earlier, unrelated task (the normal supervise flow's own
// awaiting_review, written before this rebase was ever requested) leaves that
// leftover file for WaitForStatus's poller to read — and since the poller's
// first tick is immediate, it can report success before the newly spawned
// worker has done anything at all. A missing file is not an
// error.
func InvalidateStatus(worktree string) error {
	for _, path := range []string{protocol.StatusPath(worktree), protocol.VerdictPath(worktree)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing %s: %w", path, err)
		}
	}
	return nil
}

// staleTolerance absorbs the gap between since (an in-process time.Now(),
// nanosecond precision) and a file's mtime as reported back by the OS, which
// on some filesystems/runners rounds to coarser resolution. A genuine
// post-dispatch write can therefore read back a few milliseconds to low
// single-digit seconds "before" since even though the write happened after
// it (this previously flaked make ci: the same commit passed and failed
// WaitForStatus's mtime check on identical CI hardware minutes apart).
// Widening the boundary by this much is still safe
// against what isStale actually guards against — a leftover file from before
// this dispatch — because InvalidateStatus removes that file immediately
// before dispatch; a leftover would need a genuinely concurrent write, not
// mere clock/mtime skew, to slip through.
const staleTolerance = 2 * time.Second

// isStale reports whether the status file at path was last written more than
// staleTolerance before since, using the file's own mtime rather than any
// timestamp the worker self-reported inside it. InvalidateStatus os.Removes
// status.json before a worker is dispatched, so any file present afterward
// was necessarily (re)written by this dispatch — its mtime is ground truth,
// immune to a worker writing a wrong clock value into the JSON body (the
// worker-reported UpdatedAt was trusted for this decision
// before, letting a garbage timestamp make a real, current status look
// pre-dispatch forever). The comparison gives mtime a grace window below
// since (see staleTolerance) rather than an exact boundary, because some
// filesystems record mtime at coarser resolution than time.Now(), and a real
// post-dispatch write can round down to just under since.
// A file that can't be stat'd is treated as not stale; the caller's own
// handling of the read error decides that case.
func isStale(path string, since time.Time) bool {
	if since.IsZero() {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.ModTime().Before(since.Add(-staleTolerance))
}

// WaitForStatus polls a worktree's status file until it reports a phase
// written at or after since, reaches a terminal phase, or ctx is canceled,
// returning the last such status read and whether one was seen. A status.json
// whose file mtime is more than staleTolerance before since is a stale
// leftover — from before this dispatch, or from a race with
// InvalidateStatus — and is treated the same as no file at all, so a caller
// can never mistake it for this dispatch's outcome. since should be no later
// than the moment the worker was dispatched. It is the single-worker analog
// of the supervise watch loop, for commands like rebase that dispatch one
// worker.
//
// A status.json with an empty Phase is also treated as not yet seen: a real
// worker report always sets Phase, so an empty one can only be the
// dispatcher's own pre-dispatch bookkeeping write (recording Base right
// after InvalidateStatus so a worker's later report can carry it forward)
// landing inside staleTolerance of since and so not caught by the mtime
// check above.
//
// paneID, when non-empty, is cross-checked against herdr's own agent_status on
// every tick the same way the full supervise loop's checkHerdrStuck does: a
// pane herdr reports "blocked" or "done" can never itself write a fresh
// status.json, so polling status.json alone leaves a caller silent for
// however long it takes a human to notice and clear an unanswered prompt — a
// repo `Ask` permission rule overriding a worker's auto mode for one command
// (e.g. `Bash(git push:*)`) parks it there with no other signal short of
// reading the pane by hand. out, if non-nil, gets one line the moment that
// condition is first observed and again once it clears, rather than once per
// tick, so a stuck worker is reported once, not spammed every interval.
// client/paneID left zero disables the check — there's no pane to ask about
// (unit tests, or an --attach --worktrees caller with no resolvable pane).
func WaitForStatus(ctx context.Context, client herdr.Client, paneID, worktree string, interval time.Duration, since time.Time, out io.Writer) (protocol.Status, bool) {
	path := protocol.StatusPath(worktree)
	timer := time.NewTimer(0)
	defer timer.Stop()
	var last protocol.Status
	var hasFile bool
	var stuck bool
	for {
		select {
		case <-ctx.Done():
			return last, hasFile
		case <-timer.C:
			if s, err := protocol.Load(path); err == nil && s.Phase != "" && !isStale(path, since) {
				last = s
				hasFile = true
				if protocol.IsTerminal(s.Phase) {
					return last, hasFile
				}
			}
			stuck = reportPaneStuck(ctx, client, paneID, out, stuck)
			timer.Reset(interval)
		}
	}
}

// reportPaneStuck cross-checks herdr's live agent_status for paneID and, on
// the edge where herdrStuck's verdict changes from wasStuck, writes a one-line
// notice to out naming why status.json isn't moving. It returns the new
// stuck verdict for the caller to carry into its next tick. paneID == "" (no
// pane to ask about) and a herdr transport error (which says nothing about
// the worker's real state — surfacing it here would bury the one signal this
// exists to raise) both leave wasStuck unchanged.
func reportPaneStuck(ctx context.Context, client herdr.Client, paneID string, out io.Writer, wasStuck bool) bool {
	if paneID == "" {
		return false
	}
	panes, err := client.PaneList(ctx)
	if err != nil {
		return wasStuck
	}
	pane, found := findPane(panes, paneID)
	stuck := found && herdrStuck(pane.AgentStatus)
	if out != nil && stuck != wasStuck {
		if stuck {
			_, _ = fmt.Fprintf(out, "%s %s\n", ui.LabelWarning.Render("○"), herdrBlockedMessage(pane.AgentStatus, paneID))
		} else {
			_, _ = fmt.Fprintf(out, "%s pane %s resumed\n", ui.LabelInfo.Render("i"), paneID)
		}
	}
	return stuck
}

// herdrBlockedMessage renders herdr's externally-observed pane state into a
// message distinguishing why the worker can't write status.json itself: still
// running but parked on an unanswered prompt (most commonly a permission-rule
// override interrupting auto mode) versus its process having already ended.
func herdrBlockedMessage(agentStatus, paneID string) string {
	switch agentStatus {
	case "blocked":
		return fmt.Sprintf("blocked: awaiting permission approval in pane %s", paneID)
	default: // "done"
		return fmt.Sprintf("blocked: pane %s ended without writing a terminal status", paneID)
	}
}
