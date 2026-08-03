package cmd

import (
	"fmt"
	"os/exec"

	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

// Hard-required external binaries argus shells out to, named once so doctor's
// checklist and the commands' own upfront presence checks agree on both the
// name and the install hint.
const (
	binHerdr  = "herdr"
	binClaude = "claude"
)

// installHints is the single source of truth for each hard-required binary and
// the actionable install hint shown when it is missing from PATH. doctor's
// checklist (checkBinary) and every command's upfront presence check
// (requireBinaries) both read it, so a hint string is written exactly once —
// duplicating one into a second place is a defect.
var installHints = map[string]string{
	binHerdr:  "install herdr and put it on your PATH — it hosts the worker panes argus drives",
	binClaude: "install the Claude CLI and put it on your PATH — it is the default worker launcher",
}

// binaryLookPath is the exec.LookPath seam the command entrypoints resolve
// required binaries through — a package-level var, like newReviewer, purely so
// a test can force a binary present or missing without one on the real PATH.
// doctor injects its own lookPath through doctorArgs instead; this serves the
// commands, whose run functions tests drive directly.
var binaryLookPath = exec.LookPath

// requireBinaries fails fast with a ui.UserError carrying the centralized
// install hint for the first hard-required binary missing from PATH, so a
// command that cannot run without herdr or claude says so up front instead of
// failing deep at spawn/launch time. Only the binaries a command actually
// launches should be passed — a command that never runs claude must not
// require it. A nil lookPath resolves against the real PATH.
func requireBinaries(lookPath func(string) (string, error), names ...string) error {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	for _, name := range names {
		if _, err := lookPath(name); err != nil {
			return &ui.UserError{
				Err:  fmt.Errorf("%s not found on PATH: %w", name, err),
				Hint: installHints[name],
			}
		}
	}
	return nil
}

// superviseRequiredBinaries returns the hard-required binaries for a supervise
// invocation. A dry run creates and launches nothing, so it needs none. Every
// real run talks to herdr; claude is additionally needed whenever a worker or
// reviewer will actually launch — a spawn (not --attach), or --review.
func superviseRequiredBinaries(dryRun, attach, review bool) []string {
	if dryRun {
		return nil
	}
	required := []string{binHerdr}
	if !attach || review {
		required = append(required, binClaude)
	}
	return required
}
