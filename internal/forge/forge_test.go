package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fakeHTTP(t *testing.T, wantURLContains, wantAuth, reply string, code int) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if wantURLContains != "" && !strings.Contains(r.URL.String(), wantURLContains) {
			t.Errorf("url = %s, want contains %q", r.URL.String(), wantURLContains)
		}
		if wantAuth != "" && r.Header.Get("Authorization") != wantAuth {
			t.Errorf("auth = %q, want %q", r.Header.Get("Authorization"), wantAuth)
		}
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(reply)), Header: make(http.Header)}, nil
	})}
}

func TestGiteaOpenPR(t *testing.T) {
	hc := fakeHTTP(t, "https://codeberg.org/api/v1/repos/o/r/pulls", "token secret",
		`{"number":7,"html_url":"https://codeberg.org/o/r/pulls/7","state":"open"}`, 201)
	f, err := New("codeberg.org", "secret", hc, KindAuto)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pr, err := f.OpenPR(context.Background(), &PRRequest{Owner: "o", Repo: "r", Title: "t", Head: "b", Base: "main"})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if pr.Number != 7 || pr.State != "open" {
		t.Errorf("unexpected pr: %+v", pr)
	}
}

func TestGitHubOpenPRUsesBearerAndGitHubAPI(t *testing.T) {
	hc := fakeHTTP(t, "https://api.github.com/repos/o/r/pulls", "Bearer ght",
		`{"number":3,"html_url":"https://github.com/o/r/pull/3","state":"open"}`, 201)
	f, err := New("github.com", "ght", hc, KindAuto)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if f.Host() != "github.com" {
		t.Errorf("host: got %q", f.Host())
	}
	pr, err := f.OpenPR(context.Background(), &PRRequest{Owner: "o", Repo: "r", Title: "t", Head: "b", Base: "main"})
	if err != nil || pr.Number != 3 {
		t.Fatalf("OpenPR: %+v err=%v", pr, err)
	}
}

func TestFetchIssue(t *testing.T) {
	hc := fakeHTTP(t, "/repos/o/r/issues/42", "", `{"number":42,"title":"Bug","body":"it breaks"}`, 200)
	f, err := New("codeberg.org", "", hc, KindAuto)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	iss, err := f.FetchIssue(context.Background(), "o", "r", 42)
	if err != nil || iss.Title != "Bug" || iss.Body != "it breaks" {
		t.Fatalf("FetchIssue: %+v err=%v", iss, err)
	}
}

func TestPRMerged(t *testing.T) {
	merged := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if (PR{MergedAt: &merged}).Merged() != true {
		t.Error("PR with a MergedAt should report Merged() true")
	}
	if (PR{}).Merged() != false {
		t.Error("PR with no MergedAt should report Merged() false")
	}
}

func TestGiteaFindPRFiltersClientSideByHeadRef(t *testing.T) {
	hc := fakeHTTP(t, "https://codeberg.org/api/v1/repos/o/r/pulls?state=all", "token secret",
		`[{"number":5,"html_url":"https://codeberg.org/o/r/pulls/5","state":"closed","head":{"ref":"other"}},
		  {"number":7,"html_url":"https://codeberg.org/o/r/pulls/7","state":"closed","merged_at":"2026-01-01T00:00:00Z","head":{"ref":"feat-x"}}]`, 200)
	f, err := New("codeberg.org", "secret", hc, KindAuto)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pr, found, err := f.FindPR(context.Background(), "o", "r", "feat-x")
	if err != nil {
		t.Fatalf("FindPR: %v", err)
	}
	if !found || pr.Number != 7 || !pr.Merged() {
		t.Errorf("unexpected pr: %+v found=%v", pr, found)
	}
}

func TestGiteaFindPRNotFound(t *testing.T) {
	hc := fakeHTTP(t, "", "", `[]`, 200)
	f, err := New("codeberg.org", "secret", hc, KindAuto)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, found, err := f.FindPR(context.Background(), "o", "r", "feat-x")
	if err != nil {
		t.Fatalf("FindPR: %v", err)
	}
	if found {
		t.Error("want found=false for a branch with no PR")
	}
}

func TestGitHubFindPRUsesHeadFilterAndTakesFirstResult(t *testing.T) {
	hc := fakeHTTP(t, "head=o:feat-x", "Bearer ght",
		`[{"number":3,"html_url":"https://github.com/o/r/pull/3","state":"open"}]`, 200)
	f, err := New("github.com", "ght", hc, KindAuto)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pr, found, err := f.FindPR(context.Background(), "o", "r", "feat-x")
	if err != nil || !found || pr.Number != 3 {
		t.Fatalf("FindPR: %+v found=%v err=%v", pr, found, err)
	}
	if pr.Merged() {
		t.Error("open PR should not report Merged()")
	}
}

func TestOpenPRSurfacesAPIMessage(t *testing.T) {
	hc := fakeHTTP(t, "", "", `{"message":"branch already exists"}`, 409)
	f, err := New("codeberg.org", "t", hc, KindAuto)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = f.OpenPR(context.Background(), &PRRequest{Owner: "o", Repo: "r"})
	if err == nil || !strings.Contains(err.Error(), "branch already exists") {
		t.Errorf("want surfaced API message, got %v", err)
	}
}

// sequencedHTTP replies with one canned response per call, in order, letting
// a test drive a client that issues more than one request per method call
// (PRChecks fetches the PR for its head sha, then the check-runs for that
// sha). A call past the end of replies repeats the last one.
func sequencedHTTP(t *testing.T, replies []string, code int) *http.Client {
	t.Helper()
	call := 0
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		i := call
		if i >= len(replies) {
			i = len(replies) - 1
		}
		call++
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(replies[i])), Header: make(http.Header)}, nil
	})}
}

func TestGitHubPRChecksMergeReadyWhenAllSucceed(t *testing.T) {
	hc := sequencedHTTP(t, []string{
		`{"head":{"sha":"deadbeef"}}`,
		`{"check_runs":[
			{"name":"build","status":"completed","conclusion":"success","html_url":"https://github.com/o/r/runs/1"},
			{"name":"lint","status":"completed","conclusion":"neutral"}
		]}`,
	}, 200)
	f, err := New("github.com", "ght", hc, KindAuto)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	checks, err := f.PRChecks(context.Background(), "o", "r", 7)
	if err != nil {
		t.Fatalf("PRChecks: %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("checks = %+v, want 2", checks)
	}
	for _, c := range checks {
		if !c.Terminal() || c.Failed() {
			t.Errorf("check %+v: want Terminal()=true Failed()=false", c)
		}
	}
	if checks[0].LogURL != "https://github.com/o/r/runs/1" {
		t.Errorf("LogURL = %q, want the html_url", checks[0].LogURL)
	}
}

func TestGitHubPRChecksNamesFailingCheck(t *testing.T) {
	hc := sequencedHTTP(t, []string{
		`{"head":{"sha":"deadbeef"}}`,
		`{"check_runs":[
			{"name":"build","status":"completed","conclusion":"success"},
			{"name":"test","status":"completed","conclusion":"failure","details_url":"https://github.com/o/r/runs/2"}
		]}`,
	}, 200)
	f, err := New("github.com", "ght", hc, KindAuto)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	checks, err := f.PRChecks(context.Background(), "o", "r", 7)
	if err != nil {
		t.Fatalf("PRChecks: %v", err)
	}
	var failing []Check
	for _, c := range checks {
		if c.Failed() {
			failing = append(failing, c)
		}
	}
	if len(failing) != 1 || failing[0].Name != "test" {
		t.Fatalf("failing checks = %+v, want just %q", failing, "test")
	}
	if failing[0].LogURL != "https://github.com/o/r/runs/2" {
		t.Errorf("LogURL = %q, want the details_url fallback", failing[0].LogURL)
	}
}

func TestGitHubPRChecksInFlightIsNotTerminal(t *testing.T) {
	hc := sequencedHTTP(t, []string{
		`{"head":{"sha":"deadbeef"}}`,
		`{"check_runs":[{"name":"build","status":"in_progress"}]}`,
	}, 200)
	f, err := New("github.com", "ght", hc, KindAuto)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	checks, err := f.PRChecks(context.Background(), "o", "r", 7)
	if err != nil {
		t.Fatalf("PRChecks: %v", err)
	}
	if len(checks) != 1 || checks[0].Terminal() {
		t.Errorf("checks = %+v, want one non-terminal check", checks)
	}
}

// TestGitHubPRChecksPaginatesBeyondFirstPage pins a rework-round fix: a page
// exactly as long as checksPerPage must not be mistaken for the last page —
// evaluateChecks (cmd/tend.go) would otherwise call a PR merge-ready while a
// failing check sitting on page 2 (e.g. a matrix build with >100 jobs) was
// never fetched at all.
func TestGitHubPRChecksPaginatesBeyondFirstPage(t *testing.T) {
	page1Runs := make([]map[string]string, checksPerPage)
	for i := range page1Runs {
		page1Runs[i] = map[string]string{"name": fmt.Sprintf("job-%d", i), "status": "completed", "conclusion": "success"}
	}
	page1Body, err := json.Marshal(map[string]any{"check_runs": page1Runs})
	if err != nil {
		t.Fatalf("marshaling page 1 fixture: %v", err)
	}
	page2Body, err := json.Marshal(map[string]any{"check_runs": []map[string]string{
		{"name": "job-page-2-failing", "status": "completed", "conclusion": "failure"},
	}})
	if err != nil {
		t.Fatalf("marshaling page 2 fixture: %v", err)
	}

	replies := []string{`{"head":{"sha":"deadbeef"}}`, string(page1Body), string(page2Body)}
	var gotURLs []string
	call := 0
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotURLs = append(gotURLs, r.URL.String())
		i := call
		if i >= len(replies) {
			i = len(replies) - 1
		}
		call++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(replies[i])), Header: make(http.Header)}, nil
	})}

	f, err := New("github.com", "ght", hc, KindAuto)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	checks, err := f.PRChecks(context.Background(), "o", "r", 7)
	if err != nil {
		t.Fatalf("PRChecks: %v", err)
	}
	if len(checks) != checksPerPage+1 {
		t.Fatalf("checks = %d, want %d (a full first page plus one on page 2)", len(checks), checksPerPage+1)
	}
	var failing *Check
	for i := range checks {
		if checks[i].Name == "job-page-2-failing" {
			failing = &checks[i]
		}
	}
	if failing == nil {
		t.Fatal("want the page-2-only check present in the result")
	}
	if !failing.Failed() {
		t.Errorf("job-page-2-failing: Failed() = false, want true")
	}

	if len(gotURLs) != 3 {
		t.Fatalf("requests made = %d, want 3 (PR lookup + 2 check-run pages), got %v", len(gotURLs), gotURLs)
	}
	if !strings.Contains(gotURLs[1], "page=1") || !strings.Contains(gotURLs[2], "page=2") {
		t.Errorf("want the page param to increment across requests, got %v", gotURLs)
	}
}

// TestGiteaPRChecksRefusesUnimplementedHost guards the deliberate MVP scope
// cut: PRChecks is GitHub-only for now, and a Gitea/Forgejo-shaped host
// (sharing the rest type) must refuse clearly rather than send a request
// shaped for an endpoint it doesn't have.
func TestGiteaPRChecksRefusesUnimplementedHost(t *testing.T) {
	f, err := New("codeberg.org", "secret", nil, KindAuto)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = f.PRChecks(context.Background(), "o", "r", 7)
	if err == nil {
		t.Fatal("want an error for a Gitea/Forgejo-shaped host, got nil")
	}
}

// TestNewRefusesAnyHostOutsideTheAutoAllowlist pins the fix for a rework
// round: a substring check on "gitlab" missed the common self-hosted
// pattern of a host with no such substring at all (git.company.com,
// scm.company.io, ...) that is just as likely to be a self-hosted GitLab as a
// self-hosted Gitea/Forgejo. New's KindAuto path now refuses every host
// outside its three-host allowlist, named-like-GitLab or not.
func TestNewRefusesAnyHostOutsideTheAutoAllowlist(t *testing.T) {
	for _, host := range []string{"gitlab.corp.example.com", "git.company.com", "scm.company.io", "gitea.example.com"} {
		_, err := New(host, "secret", nil, KindAuto)
		if err == nil {
			t.Errorf("New(%q, KindAuto) = nil error, want a refusal (not on the allowlist)", host)
			continue
		}
		if !strings.Contains(err.Error(), "--forge") {
			t.Errorf("New(%q): error should point at --forge, got %q", host, err.Error())
		}
	}
}

func TestNewAllowlistedHostsAutoRoute(t *testing.T) {
	for _, host := range []string{"github.com", "gitlab.com", "codeberg.org"} {
		f, err := New(host, "secret", nil, KindAuto)
		if err != nil {
			t.Errorf("New(%q, KindAuto): %v", host, err)
			continue
		}
		if f.Host() != host {
			t.Errorf("New(%q).Host() = %q", host, f.Host())
		}
	}
}

func TestNewKindOverrideBypassesAllowlistRefusal(t *testing.T) {
	for _, host := range []string{"gitlab.corp.example.com", "git.company.com"} {
		f, err := New(host, "secret", nil, KindGitea)
		if err != nil {
			t.Errorf("New(%q, KindGitea): %v", host, err)
			continue
		}
		if f.Host() != host {
			t.Errorf("New(%q, KindGitea).Host() = %q", host, f.Host())
		}
	}
}

func TestNewUnknownKindErrors(t *testing.T) {
	if _, err := New("codeberg.org", "secret", nil, Kind("bogus")); err == nil {
		t.Error("want an error for an unrecognized forge kind")
	}
}

func TestDetect(t *testing.T) {
	cases := []struct{ url, host, owner, repo string }{
		{"git@github.com:acme/widget.git", "github.com", "acme", "widget"},
		{"https://github.com/acme/widget", "github.com", "acme", "widget"},
		{"git@codeberg.org:Elysium_Labs/argus.git", "codeberg.org", "Elysium_Labs", "argus"},
		{"ssh://git@codeberg.org/Elysium_Labs/argus.git", "codeberg.org", "Elysium_Labs", "argus"},
		{"https://gitea.example.com:3000/grp/sub/proj.git", "gitea.example.com", "sub", "proj"},
	}
	for _, c := range cases {
		host, owner, repo, err := Detect(c.url)
		if err != nil || host != c.host || owner != c.owner || repo != c.repo {
			t.Errorf("Detect(%q) = %s %s/%s err=%v; want %s %s/%s", c.url, host, owner, repo, err, c.host, c.owner, c.repo)
		}
	}
	if _, _, _, err := Detect("not a url"); err == nil {
		t.Error("want error for an unparseable remote")
	}
}

func TestTokenForHost(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "gh")
	t.Setenv("CODEBERG_TOKEN", "cb")
	t.Setenv("GITEA_EXAMPLE_COM_TOKEN", "gt")
	if got := TokenForHost("github.com", nil); got != "gh" {
		t.Errorf("github token: %q", got)
	}
	if got := TokenForHost("codeberg.org", nil); got != "cb" {
		t.Errorf("codeberg token: %q", got)
	}
	if got := TokenForHost("gitea.example.com", nil); got != "gt" {
		t.Errorf("self-hosted token: %q", got)
	}
	if got := TokenForHost("unknown.host", nil); got != "" {
		t.Errorf("unknown host should have no token, got %q", got)
	}
}

func TestTokenForHostFallsBackToCredentialHelperWhenEnvUnset(t *testing.T) {
	t.Setenv("CODEBERG_TOKEN", "")

	orig := tokenFromHelper
	defer func() { tokenFromHelper = orig }()

	var gotHost string
	tokenFromHelper = func(host string) string {
		gotHost = host
		return "helper-token"
	}

	if got := TokenForHost("codeberg.org", nil); got != "helper-token" {
		t.Errorf("want fallback token from helper, got %q", got)
	}
	if gotHost != "codeberg.org" {
		t.Errorf("helper called with host %q, want codeberg.org", gotHost)
	}
}

func TestTokenForHostPrefersEnvOverCredentialHelper(t *testing.T) {
	t.Setenv("CODEBERG_TOKEN", "env-token")

	orig := tokenFromHelper
	defer func() { tokenFromHelper = orig }()
	tokenFromHelper = func(string) string {
		t.Fatal("credential helper should not run when the env var is set")
		return ""
	}

	if got := TokenForHost("codeberg.org", nil); got != "env-token" {
		t.Errorf("want env token, got %q", got)
	}
}

func TestTokenForHostPrefersOverrideOverBuiltinEnvVar(t *testing.T) {
	t.Setenv("CODEBERG_TOKEN", "builtin-token")
	t.Setenv("MY_CODEBERG_TOKEN", "override-token")

	overrides := map[string]string{"codeberg.org": "MY_CODEBERG_TOKEN"}
	if got := TokenForHost("codeberg.org", overrides); got != "override-token" {
		t.Errorf("want override token, got %q", got)
	}
}

func TestTokenForHostOverrideFallsBackToBuiltinWhenUnset(t *testing.T) {
	t.Setenv("CODEBERG_TOKEN", "builtin-token")
	t.Setenv("MY_CODEBERG_TOKEN", "")

	overrides := map[string]string{"codeberg.org": "MY_CODEBERG_TOKEN"}
	if got := TokenForHost("codeberg.org", overrides); got != "builtin-token" {
		t.Errorf("want fallback to builtin token when override var is unset, got %q", got)
	}
}

func TestParseCredentialPassword(t *testing.T) {
	out := "protocol=https\nhost=codeberg.org\nusername=x\npassword=s3cr3t\n"
	if got := parseCredentialPassword(out); got != "s3cr3t" {
		t.Errorf("parseCredentialPassword: got %q", got)
	}
	if got := parseCredentialPassword("protocol=https\nhost=codeberg.org\n"); got != "" {
		t.Errorf("parseCredentialPassword with no password line: got %q, want empty", got)
	}
}

func TestCredentialHelperTokenNeverPromptsForUnknownHost(t *testing.T) {
	// A host with no configured credential helper must return empty quickly,
	// not hang waiting on a terminal prompt (GIT_TERMINAL_PROMPT=0 disables that).
	if got := credentialHelperToken("argus-fix-issue-58-test.invalid"); got != "" {
		t.Errorf("want empty token for a host with no configured credentials, got %q", got)
	}
}
