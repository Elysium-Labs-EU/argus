package cmd

import (
	"fmt"
	"io"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
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

// resolveVerifyCommand applies an explicit --verify-cmd flag over this repo's
// .argus/config.yml verify_command over "" (no command configured — the
// gate's prior behavior), the same explicit-flag-wins precedence
// resolveGatePolicy and resolveSuperviseBase apply for their own sources.
// It is not folded into gateFlags/resolveGatePolicy: unlike ReviewPolicy's
// fields, VerifyCommand is not consumed by the pure Assess/gateVerdict
// policy check, it is a shell command supervisor.Config threads to
// RunVerifyCommand. explicit is cmd.Flags().Changed("verify-cmd"). rc is a
// pointer solely to avoid copying the struct at the call site.
func resolveVerifyCommand(explicit bool, flagValue string, rc *repoconfig.Config) string {
	if explicit {
		return flagValue
	}
	if rc.VerifyCommand != "" {
		return rc.VerifyCommand
	}
	return flagValue
}

// resolveWorktreeSetupCmd applies an explicit --worktree-setup-cmd flag over
// this repo's .argus/config.yml worktree_bootstrap_command over "" (no command
// configured — a bare `git worktree add` with no bootstrap step, the prior
// behavior), the same explicit-flag-wins precedence resolveVerifyCommand
// applies for its own source. explicit is
// cmd.Flags().Changed("worktree-setup-cmd"). rc is a pointer solely to avoid
// copying the struct at the call site.
func resolveWorktreeSetupCmd(explicit bool, flagValue string, rc *repoconfig.Config) string {
	if explicit {
		return flagValue
	}
	if rc.WorktreeSetupCmd != "" {
		return rc.WorktreeSetupCmd
	}
	return flagValue
}

// resolveOwnerStaleAfter applies an explicit --owner-stale-after flag over
// this repo's .argus/config.yml owner_stale_after over flagValue (already
// ownership.DefaultStaleAfter when nothing was passed — see newRebaseCmd et
// al.'s own flag registration), the same explicit-flag-wins precedence
// resolveVerifyCommand/resolveWorktreeSetupCmd apply for their own sources.
// Unlike those two (plain shell-command strings), owner_stale_after is a Go
// duration string that can be malformed, so this is the one place in the
// config-key chain that can fail — a bad config value surfaces as a
// *ui.UserError naming the repo config path, not a panic or a silently
// ignored key. explicit is cmd.Flags().Changed("owner-stale-after"). rc is a
// pointer solely to avoid copying the struct at the call site.
func resolveOwnerStaleAfter(explicit bool, flagValue time.Duration, rc *repoconfig.Config, configPath string) (time.Duration, error) {
	if explicit || rc.OwnerStaleAfter == "" {
		return flagValue, nil
	}
	d, err := time.ParseDuration(rc.OwnerStaleAfter)
	if err != nil {
		return 0, &ui.UserError{Err: fmt.Errorf("%s: owner_stale_after: %w", configPath, err)}
	}
	return d, nil
}

// resolveWorktreeDir applies an explicit --worktree-dir flag over this
// repo's .argus/config.yml worktree_dir over "" (the flag's own default —
// argus's own <repo>/.claude/worktrees/<branch> convention, see
// supervisor.WorktreePath), the same explicit-flag-wins precedence
// resolveWorktreeSetupCmd applies for its own source. explicit is
// cmd.Flags().Changed("worktree-dir"). rc is a pointer solely to avoid
// copying the struct at the call site.
func resolveWorktreeDir(explicit bool, flagValue string, rc *repoconfig.Config) string {
	if explicit {
		return flagValue
	}
	if rc.WorktreeDir != "" {
		return rc.WorktreeDir
	}
	return flagValue
}

// warnDeprecatedConfigKeys prints one line per deprecated .argus/config.yml
// key Load found, so a repo migrates opportunistically instead of needing a
// dedicated "check config" pass. Called at every command that loads repo
// config directly with access to command output; supervisor.ResolveBase's
// own internal Load stays silent — it has no output channel and is already
// documented as best-effort.
func warnDeprecatedConfigKeys(out io.Writer, rc *repoconfig.Config) {
	for _, d := range rc.Deprecated {
		_, _ = fmt.Fprintf(out, "warning: .argus/config.yml key %q is deprecated, use %q instead (both still work)\n", d.Old, d.New)
	}
}
