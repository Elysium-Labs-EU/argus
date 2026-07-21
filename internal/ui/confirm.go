package ui

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Confirm prompts on out and reads a y/n answer from in, returning defaultYes
// when the answer is empty (just Enter). in must be a *bufio.Reader shared
// across every Confirm call against the same underlying stream in one
// command invocation: bufio.Reader reads ahead past the first "\n", so
// wrapping the raw stream fresh on each call (as this used to) would strand
// an already-buffered second answer inside a bufio.Reader that then gets
// discarded, and the next Confirm call sees only EOF.
func Confirm(in *bufio.Reader, out io.Writer, prompt string, defaultYes bool) bool {
	suffix := "[y/N]"
	if defaultYes {
		suffix = "[Y/n]"
	}
	_, _ = fmt.Fprintf(out, "%s %s %s ", LabelWarning.Render("?"), prompt, suffix)

	line, _ := in.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	if line == "" {
		return defaultYes
	}
	return line == "y" || line == "yes"
}
