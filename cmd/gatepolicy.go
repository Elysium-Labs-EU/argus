package cmd

import (
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
)

// gateFlags carries supervise/rework's --max-diff-lines/--proof-required-path/
// --always-review-path values together with whether each was actually passed
// on the command line (cmd.Flags().Changed), mirroring resolveSuperviseBase's
// own explicit/flagValue split for --base.
type gateFlags struct {
	proofRequiredPaths    []string
	alwaysReviewPaths     []string
	maxDiffLines          int
	maxDiffLinesExplicit  bool
	proofRequiredExplicit bool
	alwaysReviewExplicit  bool
}

// resolveGatePolicy applies an explicit flag over this repo's .argus/config.yml
// gate keys over the flag's own default (already the value in f when nothing
// was passed), the same three-source precedence resolveSuperviseBase applies
// for --base. rc is a pointer solely to avoid copying the struct at the call
// site; rc.MaxDiffLines is itself a pointer because 0 is a legal, meaningful
// value (disables the diff ceiling) that must stay distinguishable from "key
// not present in config.yml".
func resolveGatePolicy(f gateFlags, rc *repoconfig.Config) *supervisor.ReviewPolicy {
	p := &supervisor.ReviewPolicy{
		MaxDiffLines:       f.maxDiffLines,
		ProofRequiredPaths: f.proofRequiredPaths,
		AlwaysReviewPaths:  f.alwaysReviewPaths,
	}
	if !f.maxDiffLinesExplicit && rc.MaxDiffLines != nil {
		p.MaxDiffLines = *rc.MaxDiffLines
	}
	if !f.proofRequiredExplicit && len(rc.ProofRequiredPaths) > 0 {
		p.ProofRequiredPaths = rc.ProofRequiredPaths
	}
	if !f.alwaysReviewExplicit && len(rc.AlwaysReviewPaths) > 0 {
		p.AlwaysReviewPaths = rc.AlwaysReviewPaths
	}
	return p
}
