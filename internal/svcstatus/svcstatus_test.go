package svcstatus

import (
	"strings"
	"testing"
)

func TestWorthMentioning(t *testing.T) {
	cases := []struct {
		code int
		want bool
	}{
		{500, true},
		{502, true},
		{503, true},
		{504, true},
		{400, false},
		{401, false},
		{403, false},
		{404, false},
		{422, false},
		{429, false},
	}
	for _, c := range cases {
		if got := WorthMentioning(c.code); got != c.want {
			t.Errorf("WorthMentioning(%d) = %v, want %v", c.code, got, c.want)
		}
	}
}

func TestNote(t *testing.T) {
	if got := Note("git.example.com", ""); got != "" {
		t.Errorf("unknown host Note = %q, want empty", got)
	}
	for _, host := range []string{"github.com", "gitlab.com", "codeberg.org"} {
		got := Note(host, "")
		if !strings.Contains(got, pageURL(host, "")) {
			t.Errorf("Note(%q) = %q, want it to contain %q", host, got, pageURL(host, ""))
		}
	}
	if got := Note("ci.codeberg.org", ""); !strings.Contains(got, "status.codeberg.org") {
		t.Errorf("Note(subdomain) = %q, want it to resolve to codeberg.org's page", got)
	}
}

func TestNoteOverride(t *testing.T) {
	const custom = "https://status.example.com"
	if got := Note("git.example.com", custom); !strings.Contains(got, custom) {
		t.Errorf("Note(unknown host, override) = %q, want it to contain %q", got, custom)
	}
	if got := Note("github.com", custom); !strings.Contains(got, custom) || strings.Contains(got, "githubstatus.com") {
		t.Errorf("Note(known host, override) = %q, want override %q to win over the built-in page", got, custom)
	}
}
