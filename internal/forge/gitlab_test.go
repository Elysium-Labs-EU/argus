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
	f := New("gitlab.com", "secret", hc)
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
	f := New("gitlab.com", "glpat-secret", hc)
	if _, err := f.OpenPR(context.Background(), &PRRequest{Owner: "o", Repo: "r", Title: "t", Head: "b", Base: "main"}); err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if gotHeader != "glpat-secret" {
		t.Errorf("PRIVATE-TOKEN = %q, want %q", gotHeader, "glpat-secret")
	}
}

func TestGitLabFetchIssue(t *testing.T) {
	hc := fakeHTTP(t, "/projects/o%2Fr/issues/42", "", `{"iid":42,"title":"Bug","description":"it breaks"}`, 200)
	f := New("gitlab.com", "", hc)
	iss, err := f.FetchIssue(context.Background(), "o", "r", 42)
	if err != nil || iss.Title != "Bug" || iss.Body != "it breaks" || iss.Number != 42 {
		t.Fatalf("FetchIssue: %+v err=%v", iss, err)
	}
}

func TestGitLabFindPRUsesSourceBranchFilter(t *testing.T) {
	hc := fakeHTTP(t, "source_branch=feat-x", "",
		`[{"iid":9,"web_url":"https://gitlab.com/o/r/-/merge_requests/9","state":"merged","merged_at":"2026-01-01T00:00:00Z"}]`, 200)
	f := New("gitlab.com", "secret", hc)
	pr, found, err := f.FindPR(context.Background(), "o", "r", "feat-x")
	if err != nil || !found || pr.Number != 9 || !pr.Merged() {
		t.Fatalf("FindPR: %+v found=%v err=%v", pr, found, err)
	}
}

func TestGitLabFindPRNotFound(t *testing.T) {
	hc := fakeHTTP(t, "", "", `[]`, 200)
	f := New("gitlab.com", "secret", hc)
	_, found, err := f.FindPR(context.Background(), "o", "r", "feat-x")
	if err != nil {
		t.Fatalf("FindPR: %v", err)
	}
	if found {
		t.Error("want found=false for a branch with no merge request")
	}
}

func TestGitLabOpenPRSurfacesAPIMessage(t *testing.T) {
	hc := fakeHTTP(t, "", "", `{"message":"409 Branch already exists"}`, 409)
	f := New("gitlab.com", "t", hc)
	_, err := f.OpenPR(context.Background(), &PRRequest{Owner: "o", Repo: "r"})
	if err == nil || !strings.Contains(err.Error(), "409 Branch already exists") {
		t.Errorf("want surfaced API message, got %v", err)
	}
}
