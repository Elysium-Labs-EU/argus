// Package cmd implements the argus CLI: a deterministic supervisor that
// orchestrates herdr worker panes without an LLM in the coordination loop.
package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/buildinfo"
)

var rootCmd = &cobra.Command{
	Use:   "argus",
	Short: "Deterministic supervisor for herdr worker panes",
	Long: fmt.Sprintf(`argus %s

argus runs the mechanical half of multi-pane supervision as plain code
instead of an LLM: it discovers herdr panes, enforces one worktree per
worker, spawns them in auto mode, and tracks each worker's typed status
file rather than scraping terminal scrollback.`, buildinfo.GetVersionOnly()),
	Version: buildinfo.Get(),
	// main.go renders errors itself (with UserError hints where available),
	// and a runtime failure like a missing herdr binary isn't a usage
	// mistake, so don't dump the flag usage block after it.
	SilenceErrors: true,
	SilenceUsage:  true,
}

// Execute runs the argus CLI.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.SetVersionTemplate("{{.Version}}\n")
	rootCmd.PersistentFlags().BoolVar(&debugLog, "debug", false, "tee the run-log events to stderr as they happen (the log is always written under ~/.argus/runs)")
	rootCmd.AddCommand(superviseCmd)
	rootCmd.AddCommand(reviewCmd)
	rootCmd.AddCommand(shipCmd)
	rootCmd.AddCommand(rebaseCmd)
	rootCmd.AddCommand(reworkCmd)
	rootCmd.AddCommand(worktreeCmd)
	rootCmd.AddCommand(statsCmd)
	rootCmd.AddCommand(systemCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(workerCmd)
}
