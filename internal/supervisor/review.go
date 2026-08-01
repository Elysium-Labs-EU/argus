package supervisor

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

// ReviewPolicy is the deterministic gate that decides whether a worker's change
// can skip human/LLM review. It is the cheap-path half of Adam Jacob's tactic 6
// (slide review intensity to the risk): the low-risk majority auto-approves on
// plain facts from the status file, so the expensive review is spent only where
// it earns its keep. Path fields match a whole path segment/word, or — if the
// value contains "/" — a path substring; these are not shell wildcards, "*" and
// "?" have no special meaning (see matchAny).
//
// This used to be three path fields (SharedGlobs, OSPathGlobs, AlwaysReviewGlobs).
// SharedGlobs is not merely renamed here — it is gone, deliberately consolidated
// into AlwaysReviewPaths: both escalated unconditionally on match and differed
// only in their reason string ("shared path" vs "behavior-critical path"), so
// keeping two lists for one behavior was duplication, not a real distinction.
// A caller still passing the old --shared-glob flag now fails with an
// unknown-flag error and must switch to --always-review-path.
type ReviewPolicy struct {
	ProofRequiredPaths []string // paths whose change needs real-world proof
	AlwaysReviewPaths  []string // behavior-critical paths that always escalate, even for a small clean diff — also covers what was once the separate SharedGlobs (shared/prod-path) field, see the consolidation note above
	MaxDiffLines       int      // insertions+deletions above this escalate; 0 = no limit
}

// DefaultReviewPolicy is a conservative starting gate: modest diff ceiling, the
// OS-integration surfaces the supervise-agents skill calls out as needing
// real-world testing, and the behavior-critical (degraded-mode) surfaces that
// must never auto-approve on diff size alone. See selfConfigPath's own comment
// for why .argus/config.yml is checked unconditionally in Assess instead of
// living in this droppable list. No default shared/prod-path restrictions
// until the caller sets them via AlwaysReviewPaths — this default set never
// populated the old SharedGlobs either, so the consolidation above changes no
// default behavior.
func DefaultReviewPolicy() ReviewPolicy {
	return ReviewPolicy{
		MaxDiffLines:       400,
		ProofRequiredPaths: []string{"systemd", "openrc", "launchd", "install", "/etc/"},
		AlwaysReviewPaths:  []string{"monitor", "daemon", "restart", "health", "liveness"},
	}
}

// selfConfigPath is checked unconditionally by Assess, independent of
// AlwaysReviewPaths, because it is a true security invariant rather than a
// matter of taste: a worker can't widen its own RepoAllow this run (base.go
// bakes it in before the worker touches anything), but an undetected change
// here merges straight into next run's config, so the gate must flag it even
// at a one-line diff — including for a repo whose own always_review_paths
// replaces the default list and never re-lists it.
const selfConfigPath = ".argus/config.yml"

// Verdict is the gate's decision for one worker. When AutoApprove is false,
// Reasons lists every trigger that forced escalation, in evaluation order.
// HardReasons is the subset of Reasons that no reviewer verdict can waive: the
// diff-vs-git checks in gateVerdict below, where status.json's own claim
// diverged from what argus measured. A non-empty HardReasons means the final
// approval must stay false even if --review comes back "approve" — see
// reviewEscalations in loop.go. Notes carries informational call-outs that
// never affect AutoApprove — e.g. a tests[] entry marked as an intentional,
// expected failure — so a reviewer still sees it happened without it reading
// as a regression to chase down.
type Verdict struct {
	Reasons     []string
	HardReasons []string
	Notes       []string
	AutoApprove bool
}

// Assess applies the policy to a worker's reported status. It auto-approves only
// when the worker is actually ready for review (awaiting_review or done), every
// test passed, the diff is within the ceiling, no always-review path was touched,
// and any proof-required-path change carries real-world proof. Anything else
// escalates with a reason. A nil policy uses DefaultReviewPolicy.
func Assess(s *protocol.Status, policy *ReviewPolicy) Verdict {
	p := DefaultReviewPolicy()
	if policy != nil {
		p = *policy
	}

	var reasons, notes []string

	switch s.Phase {
	case protocol.PhaseAwaitingReview, protocol.PhaseDone:
		// ready to judge
	case protocol.PhaseBlocked:
		reasons = append(reasons, "worker blocked: "+blockedText(s))
	case protocol.PhasePlanning, protocol.PhaseWorking, protocol.PhaseSelfTest:
		reasons = append(reasons, "worker not ready for review (phase "+string(s.Phase)+")")
	default:
		reasons = append(reasons, "unknown phase "+string(s.Phase))
	}

	var hasExpectedFailure, hasCleanPass bool
	for i := range s.Tests {
		t := s.Tests[i]
		switch {
		case t.Result == protocol.ResultPass:
			hasCleanPass = true
		case t.Result == protocol.ResultFail && t.ExpectedResult == protocol.ResultFail:
			hasExpectedFailure = true
			notes = append(notes, "intentional failure (verification proof), not escalated: "+t.Cmd)
		case t.Result == protocol.ResultFail:
			reasons = append(reasons, "test failed: "+t.Cmd)
		}
	}
	// An intentional break-then-revert proof is only proof if the revert is
	// also shown: without this, marking every failing run "expected" would
	// let a worker skip ever demonstrating the clean state it claims to have
	// restored, and the gate would have nothing left to catch that.
	if hasExpectedFailure && !hasCleanPass {
		reasons = append(reasons, "intentional failure(s) reported with no clean-state passing test to confirm the revert")
	}

	if lines := s.DiffStat.Insertions + s.DiffStat.Deletions; p.MaxDiffLines > 0 && lines > p.MaxDiffLines {
		reasons = append(reasons, fmt.Sprintf("diff %d lines exceeds max %d", lines, p.MaxDiffLines))
	}

	for _, f := range s.FilesTouched {
		if filepath.ToSlash(f) == selfConfigPath {
			reasons = append(reasons, fmt.Sprintf("touches %s — always reviewed regardless of always_review_paths (an unreviewed change here silently governs next run's own gate policy)", selfConfigPath))
			continue
		}
		if g, ok := matchAny(f, p.AlwaysReviewPaths); ok {
			reasons = append(reasons, fmt.Sprintf("touches behavior-critical path %q (matched %q) — always reviewed", f, g))
		}
	}

	if s.RealWorldProof == "" {
		if f, g, ok := firstProofRequiredPath(s.FilesTouched, p.ProofRequiredPaths); ok {
			reasons = append(reasons, fmt.Sprintf("OS-integration change %q (matched %q) has no real-world proof", f, g))
		}
	}

	return Verdict{AutoApprove: len(reasons) == 0, Reasons: reasons, Notes: notes}
}

// diffMismatchTolerance is how many more lines the real diff may exceed the
// worker's self-reported count before the gate treats it as an under-report and
// escalates. A small slack absorbs benign accounting differences (trailing
// newline, generated file) without letting a materially larger change auto-approve.
const diffMismatchTolerance = 10

// underReportReason compares a terminal-phase worker's self-reported diff
// against argus's own git measurement, returning a non-empty reason when the
// real diff materially exceeds what was claimed. Only meaningful once the
// worker has reached a terminal phase — see gateVerdict's phase gate on the
// caller.
func underReportReason(st *workerState) string {
	reported := st.status.DiffStat.Insertions + st.status.DiffStat.Deletions
	measured := st.measured.Insertions + st.measured.Deletions
	// A worker's self-report only ever covers this round's own delta, never
	// the cumulative diff since base — so once a prior round's change already
	// cleared a verdict, that verdict's measurement is subtracted here, or
	// every further round on an already-large change would fail this check
	// regardless of how correct it is.
	if st.priorMeasuredOK {
		if prior := st.priorMeasured.Insertions + st.priorMeasured.Deletions; prior < measured {
			measured -= prior
		} else {
			measured = 0
		}
	}
	if measured-reported > diffMismatchTolerance {
		return fmt.Sprintf("worker under-reported diff: claimed %d lines, git measured %d", reported, measured)
	}
	return ""
}

// gateVerdict is Assess applied to a worker's *measured* state plus checks the
// pure gate can't make: it escalates when argus could not measure the diff (so
// the self-report is unverifiable), when the real diff materially exceeds what
// the worker claimed (a buggy or dishonest status.json), when a claimed test
// pass does not reproduce (st.testMismatches, from VerifyTests — a claimed
// pass that could not even be re-run, e.g. a Cmd that isn't literal shell,
// escalates too via st.testUnverifiable, but only as a waivable reason: it
// is not proof the claim is false, just that this gate couldn't confirm it),
// when this repo's own configured verify command fails to reproduce clean
// (st.verifyMismatch, from RunGateVerifyCommand — the same bar `argus ship`'s
// `git commit` enforces via the repo's own hooks), and when herdr's own
// agent_status — not status.json — is the only evidence of the worker's real
// state (checkHerdrStuck in loop.go), which forces escalation even if
// status.json was never written at all. This is where "trust typed
// self-report" becomes "trust it only where it matches ground truth."
func gateVerdict(st *workerState, policy *ReviewPolicy) Verdict {
	eff := st.effective()
	v := Assess(&eff, policy)
	if st.herdrEscalation != "" {
		v.AutoApprove = false
		v.Reasons = append(v.Reasons, st.herdrEscalation)
		v.HardReasons = append(v.HardReasons, st.herdrEscalation)
	}
	if st.diffErr != nil {
		v.AutoApprove = false
		reason := "could not measure diff to verify worker report: " + st.diffErr.Error()
		v.Reasons = append(v.Reasons, reason)
		v.HardReasons = append(v.HardReasons, reason)
		return v
	}
	if st.measuredOK {
		applyMeasuredChecks(&v, st, &eff)
	}
	if eff.Phase == protocol.PhaseAwaitingReview || eff.Phase == protocol.PhaseDone {
		applyTerminalTestChecks(&v, st)
		applyPlanEvidenceCheck(&v, st)
	}
	return v
}

// applyMeasuredChecks folds gateVerdict's git-measured-diff checks (self-report
// vs git ground truth) into v: under-reported diff size, a terminal phase with
// zero files changed, and a rework round that changed nothing since the state
// already found wanting. Split out of gateVerdict purely to keep that
// function's own cyclomatic complexity down — go-crap gates on a function's
// CRAP score, and folding every check gateVerdict makes into one body pushed
// it over threshold even at full test coverage. Behavior is unchanged; every
// existing gateVerdict test still exercises this through the same call.
// eff is a pointer (rather than the value gateVerdict holds) purely to avoid
// copying protocol.Status on every call — applyMeasuredChecks never mutates it.
func applyMeasuredChecks(v *Verdict, st *workerState, eff *protocol.Status) {
	// A worker's self-reported DiffStat is only meaningful once it claims to
	// be done — mid-"working" it's a snapshot from an earlier report, while
	// git measures whatever's on disk *right now* as the worker keeps
	// editing. Comparing those before the worker reaches a terminal phase
	// pits a live, still-changing number against a stale one and can flag a
	// perfectly honest in-progress worker as having "under-reported" — this
	// hard reason is unwaivable, so it must never fire on a mid-flight
	// comparison that isn't apples-to-apples yet.
	terminal := eff.Phase == protocol.PhaseAwaitingReview || eff.Phase == protocol.PhaseDone
	if terminal {
		if reason := underReportReason(st); reason != "" {
			v.AutoApprove = false
			v.Reasons = append(v.Reasons, reason)
			v.HardReasons = append(v.HardReasons, reason)
		}
	}
	// A worker that reaches a terminal phase claiming completed, verified work
	// but touched zero files per git is exactly the failure mode that let a
	// launcher spawn silently fail while its stale/fabricated status.json still
	// sailed through the gate: no code change means nothing was actually done
	// or reviewable, so this must never auto-approve even though an empty diff
	// looks "clean" by every other check above.
	if len(st.measuredFiles) == 0 && terminal {
		v.AutoApprove = false
		reason := fmt.Sprintf("worker reports phase %q but git shows zero files changed against base — status may be stale or unverified", eff.Phase)
		v.Reasons = append(v.Reasons, reason)
		v.HardReasons = append(v.HardReasons, reason)
	}
	// A rework round only exists because a prior verdict was NOT approved, so
	// something is expected to change. Reaching a terminal phase byte-identical
	// to the state already found wanting (same touched files, same content)
	// means it addressed none of its findings — exactly the self-report/reality
	// mismatch the zero-files check catches for a fresh dispatch, and just as
	// unwaivable. priorContentHash is set only on a rework round (see JudgeOne),
	// so this never fires in the main supervise loop where files legitimately
	// stay unchanged between polls of a single worker.
	//
	// Byte-identical content is not proof of a no-op round on its own: a worker
	// can commit content that was already sitting in the worktree uncommitted
	// before the round even started (e.g. a fix applied but never landed), which
	// leaves ContentHash unchanged while a genuinely new HEAD commit ships it —
	// git rev-parse HEAD moving to a distinct SHA is real, verifiable progress
	// no content hash can see. Nor is it proof when the prior finding was about
	// the worker's own report rather than the source tree (e.g. an over-claimed
	// test run): st.priorStatus lets SelfReportEqual catch that the report
	// itself changed even though nothing else did. All three signals — content,
	// HEAD, and self-report — must agree nothing happened before this
	// hard-escalates.
	if terminal && st.priorContentHash != "" && st.contentHash == st.priorContentHash && st.headSHA == st.priorHeadSHA &&
		(st.priorStatus == nil || protocol.SelfReportEqual(st.priorStatus, eff)) {
		v.AutoApprove = false
		reason := fmt.Sprintf("rework round reports phase %q but changed nothing since the state already found wanting — findings not addressed", eff.Phase)
		v.Reasons = append(v.Reasons, reason)
		v.HardReasons = append(v.HardReasons, reason)
	}
}

// applyTerminalTestChecks folds VerifyTests' and RunVerifyCommand's
// reproduction results into v once a worker reaches a terminal phase — see
// gateVerdict's own doc comment for why a reproduced mismatch is unwaivable
// while an unverifiable claim (st.testUnverifiable) is only a waivable
// reason. Split out for the same CRAP-score reason as applyMeasuredChecks.
func applyTerminalTestChecks(v *Verdict, st *workerState) {
	// Same unfakeable treatment for a claimed test pass: VerifyTests
	// already reproduced (or failed to reproduce) each one against the
	// real worktree, so a mismatch here means status.json's pass claim
	// doesn't hold up — no reviewer verdict should be able to waive it,
	// the same as an under-reported diff.
	for _, m := range st.testMismatches {
		v.AutoApprove = false
		v.Reasons = append(v.Reasons, m)
		v.HardReasons = append(v.HardReasons, m)
	}
	// A claimed pass this gate could never actually re-run (cmdStr didn't
	// parse as shell syntax) still needs a human/reviewer look — the claim
	// is unconfirmed, not disproven — but a reviewer verdict can waive it,
	// unlike a reproduced mismatch above.
	for _, m := range st.testUnverifiable {
		v.AutoApprove = false
		v.Reasons = append(v.Reasons, m)
	}
	// Same unwaivable treatment for this repo's own configured verify
	// command (lint/build/pre-commit): a failure here means ship's own
	// `git commit` would hit the same failure via the repo's pre-commit
	// hook, so no reviewer verdict should be able to approve past it.
	if st.verifyMismatch != "" {
		v.AutoApprove = false
		v.Reasons = append(v.Reasons, st.verifyMismatch)
		v.HardReasons = append(v.HardReasons, st.verifyMismatch)
	}
}

// applyPlanEvidenceCheck is the unfakeable backstop for the planning phase's
// self-reported Plan field: a status.json claiming a plan/todo list, with no
// matching TodoWrite/TaskCreate tool call anywhere in the worker's own
// transcript, escalates exactly like a diff under-report does — only checked
// once the worker is asking to be judged, mirroring the diff checks' own
// gating on a terminal phase. Split out for the same CRAP-score reason as
// applyMeasuredChecks.
func applyPlanEvidenceCheck(v *Verdict, st *workerState) {
	switch {
	case st.planEvidenceErr != nil:
		v.AutoApprove = false
		v.Reasons = append(v.Reasons, "could not verify plan evidence in worker transcript: "+st.planEvidenceErr.Error())
	case st.planEvidenceOK && !st.hasPlanEvidence:
		v.AutoApprove = false
		v.Reasons = append(v.Reasons,
			"no TodoWrite/TaskCreate tool call found in worker's session transcript — planning claim unverified")
	}
}

func blockedText(s *protocol.Status) string {
	if s.BlockedReason != "" {
		return s.BlockedReason
	}
	return "no reason given"
}

// matchAny reports whether path matches any entry on a word boundary, so "install"
// matches cmd/install.go and install/main.go but not uninstall.go or reinstaller/,
// and "systemd" matches pkg/systemd/unit.go but not mysystemdemo/. Each path
// segment is tokenized into alphanumeric words (splitting on ., -, _, /), and a
// single-word entry matches when it equals one of those words. A multi-segment
// entry (one containing a separator, e.g. "internal/config") is matched as a path
// substring instead, since it is already a path fragment. These are not shell
// wildcards: "*" and "?" have no special meaning.
func matchAny(path string, paths []string) (string, bool) {
	slash := filepath.ToSlash(path)
	words := tokenizePath(slash)
	for _, p := range paths {
		if p == "" {
			continue
		}
		needle := strings.Trim(p, "/")
		if strings.Contains(needle, "/") {
			if strings.Contains(slash, needle) {
				return p, true
			}
			continue
		}
		if _, ok := words[needle]; ok {
			return p, true
		}
	}
	return "", false
}

// tokenizePath returns the set of alphanumeric words in a path, breaking on any
// non-alphanumeric byte (/, ., -, _). "cmd/install.go" -> {cmd, install, go}.
func tokenizePath(path string) map[string]struct{} {
	words := map[string]struct{}{}
	cur := strings.Builder{}
	flush := func() {
		if cur.Len() > 0 {
			words[cur.String()] = struct{}{}
			cur.Reset()
		}
	}
	for _, r := range path {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return words
}

func firstProofRequiredPath(files, paths []string) (file, matched string, ok bool) {
	for _, f := range files {
		if m, hit := matchAny(f, paths); hit {
			return f, m, true
		}
	}
	return "", "", false
}
