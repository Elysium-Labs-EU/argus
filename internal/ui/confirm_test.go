package ui

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestConfirmAcceptsYAndYes(t *testing.T) {
	for _, input := range []string{"y\n", "Y\n", "yes\n", "YES\n"} {
		out := &bytes.Buffer{}
		in := bufio.NewReader(strings.NewReader(input))
		if got := Confirm(in, out, "proceed?", false); !got {
			t.Errorf("Confirm(%q) = false, want true", input)
		}
	}
}

func TestConfirmDeclinesOtherInput(t *testing.T) {
	for _, input := range []string{"n\n", "no\n", "nope\n"} {
		out := &bytes.Buffer{}
		in := bufio.NewReader(strings.NewReader(input))
		if got := Confirm(in, out, "proceed?", true); got {
			t.Errorf("Confirm(%q) = true, want false", input)
		}
	}
}

func TestConfirmEmptyLineReturnsDefault(t *testing.T) {
	for _, defaultYes := range []bool{true, false} {
		out := &bytes.Buffer{}
		in := bufio.NewReader(strings.NewReader("\n"))
		if got := Confirm(in, out, "proceed?", defaultYes); got != defaultYes {
			t.Errorf("Confirm empty input with defaultYes=%v = %v, want %v", defaultYes, got, defaultYes)
		}
	}
}

func TestConfirmEOFReturnsDefault(t *testing.T) {
	// No trailing newline: ReadString hits io.EOF, but the accumulated line
	// is still empty, so this must fall back to defaultYes rather than hang
	// or panic on the error return.
	out := &bytes.Buffer{}
	in := bufio.NewReader(strings.NewReader(""))
	if got := Confirm(in, out, "proceed?", true); !got {
		t.Errorf("Confirm on EOF with defaultYes=true = false, want true")
	}
}

func TestConfirmWritesPromptWithSuffix(t *testing.T) {
	out := &bytes.Buffer{}
	in := bufio.NewReader(strings.NewReader("y\n"))
	Confirm(in, out, "delete file?", false)
	if !strings.Contains(out.String(), "delete file?") || !strings.Contains(out.String(), "[y/N]") {
		t.Errorf("prompt output = %q, want it to contain prompt text and [y/N] suffix", out.String())
	}
}
