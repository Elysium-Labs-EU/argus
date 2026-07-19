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
