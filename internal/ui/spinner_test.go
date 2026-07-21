package ui

import "testing"

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
