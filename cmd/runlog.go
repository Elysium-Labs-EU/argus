package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
)

// debugLog, when set via --debug, tees every run-log event to stderr as it happens.
var debugLog bool

// addDebugFlag registers --debug on cmd. Only commands that call openRunLog
// get it — a shared root-level persistent flag would show identical help
// text on every subcommand, including ones like config/init that never
// write a run log, making --debug look silently ignored there.
func addDebugFlag(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&debugLog, "debug", false, "tee this command's run-log events to stderr as they happen (the log is always written under ~/.argus/runs)")
}

// openRunLog opens the per-run JSONL log for a command and returns the logger plus
// a closer to defer. Logging must never break a run: on any failure it returns a
// nil logger (a valid no-op) so the command proceeds unlogged. Under --debug the
// log path and each event are also written to stderr.
func openRunLog(cmd *cobra.Command, command string) (*eventlog.Logger, func()) {
	var debug io.Writer
	if debugLog {
		debug = cmd.ErrOrStderr()
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, func() {}
	}
	logger, path, closer, err := eventlog.Open(home, command, debug)
	if err != nil {
		return nil, func() {}
	}
	if debugLog {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "run log: %s\n", path)
	}
	return logger, func() { _ = closer() }
}
