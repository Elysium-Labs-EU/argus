package forge

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// withFakeBin puts a directory of fake executables at the front of PATH for
// the duration of the test, so credentialHelperToken/gitCredentialFill can be
// exercised without depending on gh/glab/git actually being installed and
// configured on the machine running the test.
func withFakeBin(t *testing.T, scripts map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatalf("writing fake %s: %v", name, err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestDetectHTTPSWithNoPathErrors(t *testing.T) {
	_, _, _, err := Detect("https://github.com")
	if err == nil || !strings.Contains(err.Error(), "cannot parse host/path from remote") {
		t.Errorf("Detect(%q) err = %v, want a host/path parse error", "https://github.com", err)
	}
}

func TestDetectHTTPSOwnerOnlyPropagatesSplitError(t *testing.T) {
	_, _, _, err := Detect("https://github.com/onlyowner")
	if err == nil || !strings.Contains(err.Error(), "cannot parse owner/repo from") {
		t.Errorf("Detect(%q) err = %v, want owner/repo parse error propagated", "https://github.com/onlyowner", err)
	}
}

func TestDetectSCPWithEmptyHostErrors(t *testing.T) {
	_, _, _, err := Detect(":owner/repo")
	if err == nil || !strings.Contains(err.Error(), "no host in remote") {
		t.Errorf("Detect(%q) err = %v, want a no-host error", ":owner/repo", err)
	}
}

func TestDetectSCPSingleSegmentPropagatesSplitError(t *testing.T) {
	_, _, _, err := Detect("git@github.com:widget")
	if err == nil || !strings.Contains(err.Error(), "cannot parse owner/repo from") {
		t.Errorf("Detect(%q) err = %v, want owner/repo parse error propagated", "git@github.com:widget", err)
	}
}

func TestSplitOwnerRepoErrors(t *testing.T) {
	cases := []string{"repo", "", "o/", "/r", "o//r"}
	for _, path := range cases {
		owner, repo, err := splitOwnerRepo(path)
		if err == nil {
			t.Errorf("splitOwnerRepo(%q) = %q/%q, want error", path, owner, repo)
			continue
		}
		if !strings.Contains(err.Error(), "cannot parse owner/repo from") {
			t.Errorf("splitOwnerRepo(%q) err = %v, want owner/repo parse error", path, err)
		}
	}
}

func TestCredentialHelperTokenGitHubUsesGhAuthToken(t *testing.T) {
	withFakeBin(t, map[string]string{
		"gh": "#!/bin/sh\nif [ \"$1\" = auth ] && [ \"$2\" = token ]; then printf 'ghp_stubbed\\n'; exit 0; fi\nexit 1\n",
	})
	if got := credentialHelperToken("github.com"); got != "ghp_stubbed" {
		t.Errorf("credentialHelperToken(github.com) = %q, want the stubbed gh auth token", got)
	}
}

func TestCredentialHelperTokenNonGitHubUsesGlabConfigGetToken(t *testing.T) {
	withFakeBin(t, map[string]string{
		"glab": "#!/bin/sh\nif [ \"$1\" = config ] && [ \"$2\" = get ] && [ \"$3\" = token ]; then printf 'glab_stubbed\\n'; exit 0; fi\nexit 1\n",
	})
	if got := credentialHelperToken("gitlab.example.com"); got != "glab_stubbed" {
		t.Errorf("credentialHelperToken(gitlab.example.com) = %q, want the stubbed glab token", got)
	}
}

func TestRunTrimmedTrimsTrailingWhitespace(t *testing.T) {
	got := runTrimmed(context.Background(), "sh", "-c", "printf '  value  \\n'")
	if got != "value" {
		t.Errorf("runTrimmed = %q, want trimmed %q", got, "value")
	}
}

func TestGitCredentialFillParsesPassword(t *testing.T) {
	withFakeBin(t, map[string]string{
		"git": "#!/bin/sh\nif [ \"$1\" = credential ] && [ \"$2\" = fill ]; then cat > /dev/null; printf 'protocol=https\\nhost=example.com\\npassword=filled_pw\\n'; exit 0; fi\nexit 1\n",
	})
	got := gitCredentialFill(context.Background(), "example.com")
	if got != "filled_pw" {
		t.Errorf("gitCredentialFill = %q, want %q", got, "filled_pw")
	}
}

func TestTokenVarsForHostGitLab(t *testing.T) {
	got := tokenVarsForHost("gitlab.com")
	want := []string{"GITLAB_TOKEN"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tokenVarsForHost(gitlab.com) = %v, want %v", got, want)
	}
}

func TestStandardTokenVarsOrder(t *testing.T) {
	got := StandardTokenVars()
	want := []string{"CODEBERG_TOKEN", "GITHUB_TOKEN", "GH_TOKEN", "GITLAB_TOKEN", "FORGE_TOKEN", "JIRA_API_TOKEN"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("StandardTokenVars() = %v, want %v", got, want)
	}
}
