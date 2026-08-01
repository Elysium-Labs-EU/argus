package ui

import (
	"testing"
	"time"
)

func TestWithSpinnerSkipsRenderWhenNotInteractive(t *testing.T) {
	orig := isStderrInteractive
	isStderrInteractive = func() bool { return false }
	defer func() { isStderrInteractive = orig }()

	ran := false
	err := WithSpinner("working", func() error { ran = true; return nil })
	if err != nil || !ran {
		t.Errorf("expected fn to run directly, ran=%v err=%v", ran, err)
	}
}

func TestWithSpinnerDelegatesToRenderWhenInteractive(t *testing.T) {
	orig := isStderrInteractive
	isStderrInteractive = func() bool { return true }
	defer func() { isStderrInteractive = orig }()

	ran := false
	err := WithSpinner("working", func() error { ran = true; return nil })
	if err != nil || !ran {
		t.Errorf("expected fn to run via renderSpinner, ran=%v err=%v", ran, err)
	}
}

func TestRenderSpinnerRedrawsOnTick(t *testing.T) {
	// Ticker fires every 100ms; fn must outlive that for the redraw
	// branch (as opposed to the immediate-done branch) to run at all.
	err := renderSpinner("working", func() error {
		time.Sleep(250 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Errorf("renderSpinner returned %v, want nil", err)
	}
}
