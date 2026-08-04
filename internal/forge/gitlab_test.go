package forge

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestGitLabOpenPR(t *testing.T) {
	hc := fakeHTTP(t, "https://gitlab.com/api/v4/projects/o%2Fr/merge_requests", "",
		`{"iid":9,"web_url":"https://gitlab.com/o/r/-/merge_requests/9","state":"opened"}`, 201)
	f, err := New("gitlab.com", "secret", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if f.Host() != "gitlab.com" {
		t.Errorf("host: got %q", f.Host())
	}
	pr, err := f.OpenPR(context.Background(), &PRRequest{Owner: "o", Repo: "r", Title: "t", Body: "b", Head: "feature", Base: "main"})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if pr.Number != 9 || pr.State != "opened" || pr.HTMLURL != "https://gitlab.com/o/r/-/merge_requests/9" {
		t.Errorf("unexpected pr: %+v", pr)
	}
}

func TestGitLabOpenPRUsesPrivateTokenHeader(t *testing.T) {
	var gotHeader string
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotHeader = r.Header.Get("PRIVATE-TOKEN")
		if r.Header.Get("Authorization") != "" {
			t.Errorf("Authorization header should not be set for GitLab, got %q", r.Header.Get("Authorization"))
		}
		reply := `{"iid":1,"web_url":"https://gitlab.com/o/r/-/merge_requests/1","state":"opened"}`
		return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader(reply)), Header: make(http.Header)}, nil
	})}
	f, err := New("gitlab.com", "glpat-secret", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := f.OpenPR(context.Background(), &PRRequest{Owner: "o", Repo: "r", Title: "t", Head: "b", Base: "main"}); err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if gotHeader != "glpat-secret" {
		t.Errorf("PRIVATE-TOKEN = %q, want %q", gotHeader, "glpat-secret")
	}
}

func TestGitLabFetchIssue(t *testing.T) {
	hc := fakeHTTP(t, "/projects/o%2Fr/issues/42", "", `{"iid":42,"title":"Bug","description":"it breaks"}`, 200)
	f, err := New("gitlab.com", "", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	iss, err := f.FetchIssue(context.Background(), "o", "r", 42)
	if err != nil || iss.Title != "Bug" || iss.Body != "it breaks" || iss.Number != 42 {
		t.Fatalf("FetchIssue: %+v err=%v", iss, err)
	}
}

func TestGitLabFindPRUsesSourceBranchFilter(t *testing.T) {
	hc := fakeHTTP(t, "source_branch=feat-x", "",
		`[{"iid":9,"web_url":"https://gitlab.com/o/r/-/merge_requests/9","state":"merged","merged_at":"2026-01-01T00:00:00Z"}]`, 200)
	f, err := New("gitlab.com", "secret", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pr, found, err := f.FindPR(context.Background(), "o", "r", "feat-x")
	if err != nil || !found || pr.Number != 9 || !pr.Merged() {
		t.Fatalf("FindPR: %+v found=%v err=%v", pr, found, err)
	}
}

func TestGitLabFindPRNotFound(t *testing.T) {
	hc := fakeHTTP(t, "", "", `[]`, 200)
	f, err := New("gitlab.com", "secret", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, found, err := f.FindPR(context.Background(), "o", "r", "feat-x")
	if err != nil {
		t.Fatalf("FindPR: %v", err)
	}
	if found {
		t.Error("want found=false for a branch with no merge request")
	}
}

// TestNewKindGitLabBuildsSelfHostedBase pins the escape hatch: a self-hosted
// GitLab host passed with an explicit KindGitLab gets a GitLab client whose
// API base is built from that host, not hardcoded to gitlab.com.
func TestNewKindGitLabBuildsSelfHostedBase(t *testing.T) {
	hc := fakeHTTP(t, "https://gitlab.corp.example.com/api/v4/projects/o%2Fr/merge_requests", "",
		`{"iid":1,"web_url":"https://gitlab.corp.example.com/o/r/-/merge_requests/1","state":"opened"}`, 201)
	f, err := New("gitlab.corp.example.com", "secret", hc, KindGitLab, "")
	if err != nil {
		t.Fatalf("New with KindGitLab: %v", err)
	}
	if f.Host() != "gitlab.corp.example.com" {
		t.Errorf("Host() = %q", f.Host())
	}
	if _, err := f.OpenPR(context.Background(), &PRRequest{Owner: "o", Repo: "r", Title: "t", Head: "b", Base: "main"}); err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
}

// TestGitLabDoAppendsConfiguredStatusPageForSelfHostedHost pins issue #300
// for the GitLab-shaped client: a self-hosted GitLab host has no entry in
// svcstatus's built-in map, so a 5xx needs New's statusPageURL override to
// get any hint at all.
func TestGitLabDoAppendsConfiguredStatusPageForSelfHostedHost(t *testing.T) {
	hc := fakeHTTP(t, "", "", `{"message":"internal error"}`, 502)
	f, err := New("gitlab.corp.example.com", "secret", hc, KindGitLab, "https://status.corp.example.com")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = f.OpenPR(context.Background(), &PRRequest{Owner: "o", Repo: "r", Title: "t", Head: "b", Base: "main"})
	if err == nil {
		t.Fatal("want an error for a 502 response")
	}
	if !strings.Contains(err.Error(), "https://status.corp.example.com") {
		t.Errorf("OpenPR error = %q, want it to mention the configured status page", err.Error())
	}
}

func TestGitLabOpenPRSurfacesAPIMessage(t *testing.T) {
	hc := fakeHTTP(t, "", "", `{"message":"409 Branch already exists"}`, 409)
	f, err := New("gitlab.com", "t", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = f.OpenPR(context.Background(), &PRRequest{Owner: "o", Repo: "r"})
	if err == nil || !strings.Contains(err.Error(), "409 Branch already exists") {
		t.Errorf("want surfaced API message, got %v", err)
	}
}

func TestGitLabOpenPRMalformedResponse(t *testing.T) {
	hc := fakeHTTP(t, "", "", `not json`, 201)
	f, err := New("gitlab.com", "t", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = f.OpenPR(context.Background(), &PRRequest{Owner: "o", Repo: "r"})
	if err == nil || !strings.Contains(err.Error(), "decoding merge request response") {
		t.Errorf("OpenPR err = %v, want it to mention decoding merge request response", err)
	}
}

func TestGitLabFindPRMalformedResponse(t *testing.T) {
	hc := fakeHTTP(t, "", "", `{"not":"an array"}`, 200)
	f, err := New("gitlab.com", "t", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, err = f.FindPR(context.Background(), "o", "r", "feat-x")
	if err == nil || !strings.Contains(err.Error(), "decoding merge request list") {
		t.Errorf("FindPR err = %v, want it to mention decoding merge request list", err)
	}
}

func TestGitLabFindPRSurfacesDoError(t *testing.T) {
	hc := fakeHTTP(t, "", "", `{"message":"project not found"}`, 404)
	f, err := New("gitlab.com", "t", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, found, err := f.FindPR(context.Background(), "o", "r", "feat-x")
	if err == nil || !strings.Contains(err.Error(), "project not found") {
		t.Errorf("FindPR err = %v, want it to surface the 404 API message", err)
	}
	if found {
		t.Error("found should be false on error")
	}
}

func TestGitLabFetchIssueMalformedResponse(t *testing.T) {
	hc := fakeHTTP(t, "", "", `not json`, 200)
	f, err := New("gitlab.com", "t", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = f.FetchIssue(context.Background(), "o", "r", 42)
	if err == nil || !strings.Contains(err.Error(), "decoding issue response") {
		t.Errorf("FetchIssue err = %v, want it to mention decoding issue response", err)
	}
}

func TestGitLabFetchIssueSurfacesDoError(t *testing.T) {
	hc := fakeHTTP(t, "", "", `{"message":"issue not found"}`, 404)
	f, err := New("gitlab.com", "t", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = f.FetchIssue(context.Background(), "o", "r", 42)
	if err == nil || !strings.Contains(err.Error(), "issue not found") {
		t.Errorf("FetchIssue err = %v, want it to surface the 404 API message", err)
	}
}

// TestGitLabDoRejectsInvalidMethod pins do's own request-construction failure
// path: http.NewRequestWithContext rejects a method containing whitespace,
// and that must surface wrapped rather than panic or silently succeed.
func TestGitLabDoRejectsInvalidMethod(t *testing.T) {
	g := &gitlab{http: &http.Client{}, host: "gitlab.com", base: "https://gitlab.com/api/v4", token: "t"}
	_, err := g.do(context.Background(), "BAD METHOD", "https://gitlab.com/api/v4/projects/o%2Fr", nil)
	if err == nil || !strings.Contains(err.Error(), "building request") {
		t.Errorf("do err = %v, want it to mention building request", err)
	}
}

// TestGitLabDoOmitsStatusNoteForClientError pins svcstatus.WorthMentioning's
// caller-vs-host split at the do level: a 4xx is the caller's problem, so no
// status-page note should be appended even when one is configured.
func TestGitLabDoOmitsStatusNoteForClientError(t *testing.T) {
	hc := fakeHTTP(t, "", "", `{"message":"bad request"}`, 400)
	f, err := New("gitlab.corp.example.com", "t", hc, KindGitLab, "https://status.corp.example.com")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = f.OpenPR(context.Background(), &PRRequest{Owner: "o", Repo: "r"})
	if err == nil || !strings.Contains(err.Error(), "bad request") {
		t.Fatalf("OpenPR err = %v, want it to surface the 400 API message", err)
	}
	if strings.Contains(err.Error(), "status.corp.example.com") {
		t.Errorf("OpenPR err = %q, want no status-page note for a 4xx", err.Error())
	}
}

// TestGitLabPRChecksRefusesUnimplemented guards the deliberate MVP scope cut:
// PRChecks is GitHub-only for now, and GitLab must refuse clearly rather than
// silently return no checks (which a poller would misread as "not started
// yet" forever).
func TestGitLabPRChecksRefusesUnimplemented(t *testing.T) {
	f, err := New("gitlab.com", "t", nil, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := f.PRChecks(context.Background(), "o", "r", 1); err == nil {
		t.Fatal("want an error for GitLab, got nil")
	}
}
