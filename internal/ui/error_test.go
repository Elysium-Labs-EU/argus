package ui

import (
	"errors"
	"strings"
	"testing"
)

func TestUserErrorErrorAndUnwrap(t *testing.T) {
	base := errors.New("boom")
	ue := &UserError{Err: base, Hint: "do the thing"}
	if ue.Error() != "boom" {
		t.Errorf("Error(): got %q", ue.Error())
	}
	if !errors.Is(ue, base) {
		t.Error("Unwrap should expose the wrapped error")
	}
}

func TestUserErrorRender(t *testing.T) {
	withHint := (&UserError{Err: errors.New("bad flag"), Hint: "argus review --worktree x"}).Render()
	if !strings.Contains(withHint, "bad flag") || !strings.Contains(withHint, "argus review --worktree x") {
		t.Errorf("render should include message and hint: %q", withHint)
	}
	noHint := (&UserError{Err: errors.New("bad flag")}).Render()
	if strings.Contains(noHint, "run:") {
		t.Errorf("render without a hint should not print a run: line: %q", noHint)
	}
}

func TestWithSpinnerRunsFnWhenNotTTY(t *testing.T) {
	// In tests stderr is not a terminal, so WithSpinner just runs fn.
	ran := false
	err := WithSpinner("working", func() error { ran = true; return nil })
	if err != nil || !ran {
		t.Errorf("WithSpinner should run fn and return its error: ran=%v err=%v", ran, err)
	}
}
