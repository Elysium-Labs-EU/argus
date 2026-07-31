package cmd

import "github.com/spf13/cobra"

// bindWorktreeFlag registers the --worktree flag shared by every
// worker-targeting command. help is command-specific (what the worktree is
// used for differs per command) but the flag name/type/default is one thing
// to keep in sync, not five copy-pasted StringVar calls.
func bindWorktreeFlag(cmd *cobra.Command, dst *string, help string) {
	cmd.Flags().StringVar(dst, "worktree", "", help)
}

// bindBaseFlag registers the --base flag. defaultBase is passed in, not
// hardcoded, because supervise/review/rework diff against a fetched ref
// ("origin/main") while rebase/ship name a bare branch ("main") — see
// resolveSuperviseBase and shipArgs.baseIsDefault for why that split is
// deliberate rather than accidental drift.
func bindBaseFlag(cmd *cobra.Command, dst *string, defaultBase, help string) {
	cmd.Flags().StringVar(dst, "base", defaultBase, help)
}

// bindRepoFlag registers the --repo flag. help is command-specific: e.g.
// supervise's --repo is a filesystem root, ship's is an owner/name override.
func bindRepoFlag(cmd *cobra.Command, dst *string, help string) {
	cmd.Flags().StringVar(dst, "repo", "", help)
}

// bindVerifyCmdFlag registers the --verify-cmd flag shared by supervise and
// rework.
func bindVerifyCmdFlag(cmd *cobra.Command, dst *string, help string) {
	cmd.Flags().StringVar(dst, "verify-cmd", "", help)
}
