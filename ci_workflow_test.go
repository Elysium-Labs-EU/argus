package main

import (
	"os"
	"regexp"
	"testing"
)

// GitHub's merge queue creates a synthetic merge_group ref and requires the
// "CI" status check to report against it before a PR can leave
// AWAITING_CHECKS. A workflow that only triggers on push/pull_request never
// fires for that ref, so the required check never reports and every queued
// PR hangs until it times out. This test guards the ci.yml on: block against
// losing the merge_group trigger again.
func TestCIWorkflowTriggersOnMergeGroup(t *testing.T) {
	raw, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("reading ci.yml: %v", err)
	}

	onBlock := regexp.MustCompile(`(?ms)^on:\n(.*?)^\S`).FindSubmatch(raw)
	if onBlock == nil {
		t.Fatal("could not locate on: trigger block in ci.yml")
	}

	if !regexp.MustCompile(`(?m)^\s*merge_group:`).Match(onBlock[1]) {
		t.Fatalf("ci.yml on: block is missing merge_group trigger, required for the merge queue's CI check to report:\n%s", onBlock[1])
	}
}
