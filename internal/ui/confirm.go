package ui

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Confirm prompts on out and reads a y/n answer from in, returning defaultYes
// when the answer is empty (just Enter).
func Confirm(in io.Reader, out io.Writer, prompt string, defaultYes bool) bool {
	suffix := "[y/N]"
	if defaultYes {
		suffix = "[Y/n]"
	}
	_, _ = fmt.Fprintf(out, "%s %s %s ", LabelWarning.Render("?"), prompt, suffix)

	line, _ := bufio.NewReader(in).ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	if line == "" {
		return defaultYes
	}
	return line == "y" || line == "yes"
}
