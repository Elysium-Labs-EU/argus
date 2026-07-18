package buildinfo

import (
	"strings"
	"testing"
)

func TestGetIncludesVersionCommitDate(t *testing.T) {
	got := Get()
	for _, want := range []string{Version, GitCommit, BuildDate} {
		if !strings.Contains(got, want) {
			t.Errorf("Get() = %q, missing %q", got, want)
		}
	}
}

func TestGetVersionOnly(t *testing.T) {
	if GetVersionOnly() != Version {
		t.Errorf("GetVersionOnly() = %q want %q", GetVersionOnly(), Version)
	}
}
