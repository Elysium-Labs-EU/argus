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
// it earns its keep. Glob fields are matched as path substrings — predictable and
// enough here (e.g. "internal/config", "systemd", "/etc/").
type ReviewPolicy struct {
	SharedGlobs       []string // paths that always require review (shared/prod surface)
	OSPathGlobs       []string // paths whose change needs real-world proof
	AlwaysReviewGlobs []string // behavior-critical paths that always escalate, even for a small clean diff
	MaxDiffLines      int      // insertions+deletions above this escalate; 0 = no limit
}

// DefaultReviewPolicy is a conservative starting gate: modest diff ceiling, the
// OS-integration surfaces the supervise-agents skill calls out as needing
// real-world testing, and the behavior-critical (degraded-mode) surfaces that
// must never auto-approve on diff size alone. .argus/config.yml is in that last
// set too: a worker can't widen its own RepoAllow this run (base.go bakes it in
// before the worker touches anything), but an undetected change merges straight
// into next run's config, so the gate must flag it even at a one-line diff. No
// shared-path restrictions until the caller sets them.
func DefaultReviewPolicy() ReviewPolicy {
	return ReviewPolicy{
		MaxDiffLines:      400,
		OSPathGlobs:       []string{"systemd", "openrc", "launchd", "install", "/etc/"},
		AlwaysReviewGlobs: []string{"monitor", "daemon", "restart", "health", "liveness", ".argus/config.yml"},
	}
}

// Verdict is the gate's decision for one worker. When AutoApprove is false,
// Reasons lists every trigger that forced escalation, in evaluation order.
// HardReasons is the subset of Reasons that no reviewer verdict can waive: the
// diff-vs-git checks in gateVerdict below, where status.json's own claim
// diverged from what argus measured. A non-empty HardReasons means the final
// approval must stay false even if --review comes back "approve" — see
// reviewEscalations in loop.go.
type Verdict struct {
	Reasons     []string
	HardReasons []string
	AutoApprove bool
}

// Assess applies the policy to a worker's reported status. It auto-approves only
// when the worker is actually ready for review (awaiting_review or done), every
// test passed, the diff is within the ceiling, no shared path was touched, and any
// OS-integration change carries real-world proof. Anything else escalates with a
// reason. A nil policy uses DefaultReviewPolicy.
func Assess(s *protocol.Status, policy *ReviewPolicy) Verdict {
	p := DefaultReviewPolicy()
	if policy != nil {
		p = *policy
	}

	var reasons []string

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

	for i := range s.Tests {
		if s.Tests[i].Result == protocol.ResultFail {
			reasons = append(reasons, "test failed: "+s.Tests[i].Cmd)
		}
	}

	if lines := s.DiffStat.Insertions + s.DiffStat.Deletions; p.MaxDiffLines > 0 && lines > p.MaxDiffLines {
		reasons = append(reasons, fmt.Sprintf("diff %d lines exceeds max %d", lines, p.MaxDiffLines))
	}

	for _, f := range s.FilesTouched {
		if g, ok := matchAny(f, p.SharedGlobs); ok {
			reasons = append(reasons, fmt.Sprintf("touches shared path %q (matched %q)", f, g))
		}
		if g, ok := matchAny(f, p.AlwaysReviewGlobs); ok {
			reasons = append(reasons, fmt.Sprintf("touches behavior-critical path %q (matched %q) — always reviewed", f, g))
		}
	}

	if s.RealWorldProof == "" {
		if f, g, ok := firstOSPath(s.FilesTouched, p.OSPathGlobs); ok {
			reasons = append(reasons, fmt.Sprintf("OS-integration change %q (matched %q) has no real-world proof", f, g))
		}
	}

	return Verdict{AutoApprove: len(reasons) == 0, Reasons: reasons}
}

// diffMismatchTolerance is how many more lines the real diff may exceed the
// worker's self-reported count before the gate treats it as an under-report and
// escalates. A small slack absorbs benign accounting differences (trailing
// newline, generated file) without letting a materially larger change auto-approve.
const diffMismatchTolerance = 10

// gateVerdict is Assess applied to a worker's *measured* state plus checks the
// pure gate can't make: it escalates when argus could not measure the diff (so
// the self-report is unverifiable), when the real diff materially exceeds what
// the worker claimed (a buggy or dishonest status.json), and when herdr's own
// agent_status — not status.json — is the only evidence of the worker's real
// state (checkHerdrStuck in loop.go), which forces escalation even if status.json
// was never written at all. This is where "trust typed self-report" becomes
// "trust it only where it matches ground truth."
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
		reported := st.status.DiffStat.Insertions + st.status.DiffStat.Deletions
		measured := st.measured.Insertions + st.measured.Deletions
		// A worker's self-report only ever covers this round's own delta,
		// never the cumulative diff since base — so once a prior round's
		// change already cleared a verdict, that verdict's measurement is
		// subtracted here, or every further round on an already-large change
		// would fail this check regardless of how correct it is.
		if st.priorMeasuredOK {
			if prior := st.priorMeasured.Insertions + st.priorMeasured.Deletions; prior < measured {
				measured -= prior
			} else {
				measured = 0
			}
		}
		if measured-reported > diffMismatchTolerance {
			v.AutoApprove = false
			reason := fmt.Sprintf("worker under-reported diff: claimed %d lines, git measured %d", reported, measured)
			v.Reasons = append(v.Reasons, reason)
			v.HardReasons = append(v.HardReasons, reason)
		}
		// A worker that reaches a terminal phase claiming completed, verified work
		// but touched zero files per git is exactly the failure mode that let a
		// launcher spawn silently fail while its stale/fabricated status.json still
		// sailed through the gate: no code change means nothing was actually done
		// or reviewable, so this must never auto-approve even though an empty diff
		// looks "clean" by every other check above.
		if len(st.measuredFiles) == 0 && (eff.Phase == protocol.PhaseAwaitingReview || eff.Phase == protocol.PhaseDone) {
			v.AutoApprove = false
			reason := fmt.Sprintf("worker reports phase %q but git shows zero files changed against base — status may be stale or unverified", eff.Phase)
			v.Reasons = append(v.Reasons, reason)
			v.HardReasons = append(v.HardReasons, reason)
		}
	}

	// The unfakeable backstop for the planning phase's self-reported Plan
	// field: a status.json claiming a plan/todo list, with no matching
	// TodoWrite/TaskCreate tool call anywhere in the worker's own transcript,
	// escalates exactly like a diff under-report does above — only checked
	// once the worker is asking to be judged, mirroring the diff checks' own
	// gating on a terminal phase.
	if eff.Phase == protocol.PhaseAwaitingReview || eff.Phase == protocol.PhaseDone {
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
	return v
}

func blockedText(s *protocol.Status) string {
	if s.BlockedReason != "" {
		return s.BlockedReason
	}
	return "no reason given"
}

// matchAny reports whether path matches any glob on a word boundary, so "install"
// matches cmd/install.go and install/main.go but not uninstall.go or reinstaller/,
// and "systemd" matches pkg/systemd/unit.go but not mysystemdemo/. Each path
// segment is tokenized into alphanumeric words (splitting on ., -, _, /), and a
// single-word glob matches when it equals one of those words. A multi-segment glob
// (one containing a separator, e.g. "internal/config") is matched as a path
// substring instead, since it is already a path fragment.
func matchAny(path string, globs []string) (string, bool) {
	slash := filepath.ToSlash(path)
	words := tokenizePath(slash)
	for _, g := range globs {
		if g == "" {
			continue
		}
		needle := strings.Trim(g, "/")
		if strings.Contains(needle, "/") {
			if strings.Contains(slash, needle) {
				return g, true
			}
			continue
		}
		if _, ok := words[needle]; ok {
			return g, true
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

func firstOSPath(files, globs []string) (file, glob string, ok bool) {
	for _, f := range files {
		if g, matched := matchAny(f, globs); matched {
			return f, g, true
		}
	}
	return "", "", false
}
