package ui

import (
	"fmt"
	"os"
	"time"

	"github.com/mattn/go-isatty"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// isStderrInteractive reports whether stderr is an interactive terminal,
// i.e. whether the carriage-return redraw in renderSpinner makes sense
// there. It's a var, not a plain call, so tests can force either outcome
// without needing a real terminal (go-isatty always reports false under
// `go test`, which would otherwise leave the interactive path untested).
var isStderrInteractive = func() bool {
	return isatty.IsTerminal(os.Stderr.Fd())
}

// WithSpinner runs fn, showing an indeterminate spinner with an elapsed
// timer on stderr for the duration. It is suppressed when stderr isn't a
// terminal (piped output, CI logs), since the carriage-return redraw only
// makes sense on an interactive terminal.
func WithSpinner(message string, fn func() error) error {
	if !isStderrInteractive() {
		return fn()
	}
	return renderSpinner(message, fn)
}

// renderSpinner is the terminal-writing half of WithSpinner. It assumes
// stderr is already known to be interactive, so it's a thin wrapper left
// untested by design; the decision of whether to call it lives in
// WithSpinner/isStderrInteractive instead, where it can be unit tested.
func renderSpinner(message string, fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()

	start := time.Now()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	frame := 0
	for {
		select {
		case err := <-done:
			fmt.Fprint(os.Stderr, "\r\033[K")
			return err
		case <-ticker.C:
			elapsed := time.Since(start).Round(time.Second)
			fmt.Fprintf(os.Stderr, "\r\033[K%s %s %s",
				LabelInfo.Render(spinnerFrames[frame%len(spinnerFrames)]),
				message,
				TextMuted.Render(elapsed.String()))
			frame++
		}
	}
}
