package credential

import (
	"reflect"
	"testing"
)

func TestEnvVarsPrependsOverride(t *testing.T) {
	got := EnvVars("github.com", map[string]string{"github.com": "MY_GH_TOKEN"}, []string{"GITHUB_TOKEN", "GH_TOKEN"})
	want := []string{"MY_GH_TOKEN", "GITHUB_TOKEN", "GH_TOKEN"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("EnvVars = %v, want %v", got, want)
	}
}

func TestEnvVarsNoOverride(t *testing.T) {
	got := EnvVars("github.com", nil, []string{"GITHUB_TOKEN", "GH_TOKEN"})
	want := []string{"GITHUB_TOKEN", "GH_TOKEN"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("EnvVars = %v, want %v", got, want)
	}
}

func TestLookupPrefersEarlierVar(t *testing.T) {
	t.Setenv("ARGUS_TEST_A", "")
	t.Setenv("ARGUS_TEST_B", "b-value")
	if got := Lookup([]string{"ARGUS_TEST_A", "ARGUS_TEST_B"}); got != "b-value" {
		t.Errorf("Lookup = %q, want b-value", got)
	}
}

func TestLookupNoneSet(t *testing.T) {
	if got := Lookup([]string{"ARGUS_TEST_UNSET_A", "ARGUS_TEST_UNSET_B"}); got != "" {
		t.Errorf("Lookup = %q, want empty", got)
	}
}

func TestMergeCLIWinsOverPersisted(t *testing.T) {
	cli := map[string]string{"anthropic": "CLI_VAR"}
	persisted := map[string]string{"anthropic": "CONFIG_VAR", "github.com": "MY_GH_TOKEN"}
	got := Merge(cli, persisted)
	want := map[string]string{"anthropic": "CLI_VAR", "github.com": "MY_GH_TOKEN"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Merge = %v, want %v", got, want)
	}
}

func TestMergeNilArgs(t *testing.T) {
	if got := Merge(nil, nil); len(got) != 0 {
		t.Errorf("Merge(nil, nil) = %v, want empty", got)
	}
}

func TestScrubVarsDeduplicates(t *testing.T) {
	overrides := map[string]string{
		"anthropic":  "MY_CLAUDE_KEY",
		"github.com": "MY_GH_TOKEN",
		"other":      "MY_CLAUDE_KEY",
		"empty":      "",
	}
	got := ScrubVars(overrides)
	seen := map[string]int{}
	for _, v := range got {
		seen[v]++
	}
	if seen["MY_CLAUDE_KEY"] != 1 {
		t.Errorf("MY_CLAUDE_KEY appeared %d times, want 1", seen["MY_CLAUDE_KEY"])
	}
	if seen["MY_GH_TOKEN"] != 1 {
		t.Errorf("MY_GH_TOKEN appeared %d times, want 1", seen["MY_GH_TOKEN"])
	}
	if len(got) != 2 {
		t.Errorf("ScrubVars = %v, want 2 unique entries", got)
	}
}
