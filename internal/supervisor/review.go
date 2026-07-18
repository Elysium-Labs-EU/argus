package supervisor

import (
	"fmt"
	"strings"

	"codeberg.org/Elysium_Labs/argus/internal/protocol"
)

// ReviewPolicy is the deterministic gate that decides whether a worker's change
// can skip human/LLM review. It is the cheap-path half of Adam Jacob's tactic 6
// (slide review intensity to the risk): the low-risk majority auto-approves on
// plain facts from the status file, so the expensive review is spent only where
// it earns its keep. Glob fields are matched as path substrings — predictable and
// enough here (e.g. "internal/config", "systemd", "/etc/").
type ReviewPolicy struct {
	SharedGlobs  []string // paths that always require review (shared/prod surface)
	OSPathGlobs  []string // paths whose change needs real-world proof
	MaxDiffLines int      // insertions+deletions above this escalate; 0 = no limit
}

// DefaultReviewPolicy is a conservative starting gate: modest diff ceiling and
// the OS-integration surfaces the supervise-agents skill calls out as needing
// real-world testing, with no shared-path restrictions until the caller sets them.
func DefaultReviewPolicy() ReviewPolicy {
	return ReviewPolicy{
		MaxDiffLines: 400,
		OSPathGlobs:  []string{"systemd", "openrc", "launchd", "install", "/etc/"},
	}
}

// Verdict is the gate's decision for one worker. When AutoApprove is false,
// Reasons lists every trigger that forced escalation, in evaluation order.
type Verdict struct {
	Reasons     []string
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
	}

	if s.RealWorldProof == "" {
		if f, g, ok := firstOSPath(s.FilesTouched, p.OSPathGlobs); ok {
			reasons = append(reasons, fmt.Sprintf("OS-integration change %q (matched %q) has no real-world proof", f, g))
		}
	}

	return Verdict{AutoApprove: len(reasons) == 0, Reasons: reasons}
}

func blockedText(s *protocol.Status) string {
	if s.BlockedReason != "" {
		return s.BlockedReason
	}
	return "no reason given"
}

func matchAny(path string, globs []string) (string, bool) {
	for _, g := range globs {
		if g != "" && strings.Contains(path, g) {
			return g, true
		}
	}
	return "", false
}

func firstOSPath(files, globs []string) (file, glob string, ok bool) {
	for _, f := range files {
		if g, matched := matchAny(f, globs); matched {
			return f, g, true
		}
	}
	return "", "", false
}
