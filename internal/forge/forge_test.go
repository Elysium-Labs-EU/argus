package forge

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
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
	f := New("codeberg.org", "secret", hc)
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
	f := New("github.com", "ght", hc)
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
	f := New("codeberg.org", "", hc)
	iss, err := f.FetchIssue(context.Background(), "o", "r", 42)
	if err != nil || iss.Title != "Bug" || iss.Body != "it breaks" {
		t.Fatalf("FetchIssue: %+v err=%v", iss, err)
	}
}

func TestOpenPRSurfacesAPIMessage(t *testing.T) {
	hc := fakeHTTP(t, "", "", `{"message":"branch already exists"}`, 409)
	f := New("codeberg.org", "t", hc)
	_, err := f.OpenPR(context.Background(), &PRRequest{Owner: "o", Repo: "r"})
	if err == nil || !strings.Contains(err.Error(), "branch already exists") {
		t.Errorf("want surfaced API message, got %v", err)
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
	if got := TokenForHost("github.com"); got != "gh" {
		t.Errorf("github token: %q", got)
	}
	if got := TokenForHost("codeberg.org"); got != "cb" {
		t.Errorf("codeberg token: %q", got)
	}
	if got := TokenForHost("gitea.example.com"); got != "gt" {
		t.Errorf("self-hosted token: %q", got)
	}
	if got := TokenForHost("unknown.host"); got != "" {
		t.Errorf("unknown host should have no token, got %q", got)
	}
}
