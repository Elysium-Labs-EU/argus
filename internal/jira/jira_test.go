package jira

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const fetchFixtureBody = `{
	"key": "PROJ-42",
	"fields": {
		"summary": "Login button does nothing",
		"description": {
			"type": "doc",
			"version": 1,
			"content": [
				{"type": "paragraph", "content": [
					{"type": "text", "text": "Clicking "},
					{"type": "text", "text": "Login", "marks": [{"type": "strong"}]},
					{"type": "text", "text": " on the "},
					{"type": "text", "text": "home page", "marks": [{"type": "em"}]},
					{"type": "text", "text": " is a no-op."}
				]},
				{"type": "paragraph", "content": [
					{"type": "text", "text": "Seen on Chrome and Firefox."}
				]}
			]
		}
	}
}`

func TestFetchIssue(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fetchFixtureBody))
	}))
	defer srv.Close()

	c := New(srv.URL, "dev@example.com", "secret-token", nil)
	iss, err := c.FetchIssue(context.Background(), "PROJ-42")
	if err != nil {
		t.Fatalf("FetchIssue: %v", err)
	}

	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("dev@example.com:secret-token"))
	if gotAuth != wantAuth {
		t.Errorf("Authorization header = %q, want %q", gotAuth, wantAuth)
	}
	if gotPath != "/rest/api/3/issue/PROJ-42" {
		t.Errorf("request path = %q, want /rest/api/3/issue/PROJ-42", gotPath)
	}

	if iss.Title != "Login button does nothing" {
		t.Errorf("Title = %q", iss.Title)
	}
	wantBody := "Clicking Login on the home page is a no-op.\nSeen on Chrome and Firefox."
	if iss.Body != wantBody {
		t.Errorf("Body = %q, want %q", iss.Body, wantBody)
	}
	if iss.Number != 42 {
		t.Errorf("Number = %d, want 42", iss.Number)
	}
}

func TestFetchIssueSurfacesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errorMessages":["Issue does not exist"],"errors":{}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "dev@example.com", "secret-token", nil)
	_, err := c.FetchIssue(context.Background(), "PROJ-999")
	if err == nil || !strings.Contains(err.Error(), "Issue does not exist") {
		t.Errorf("want surfaced API message, got %v", err)
	}
}

func TestNewFromEnv(t *testing.T) {
	t.Run("missing vars", func(t *testing.T) {
		t.Setenv("JIRA_BASE_URL", "")
		t.Setenv("JIRA_EMAIL", "")
		t.Setenv("JIRA_API_TOKEN", "")
		if _, err := NewFromEnv(nil); err == nil {
			t.Error("want error when env vars are unset")
		}
	})

	t.Run("all set", func(t *testing.T) {
		t.Setenv("JIRA_BASE_URL", "https://acme.atlassian.net")
		t.Setenv("JIRA_EMAIL", "dev@example.com")
		t.Setenv("JIRA_API_TOKEN", "secret-token")
		c, err := NewFromEnv(nil)
		if err != nil {
			t.Fatalf("NewFromEnv: %v", err)
		}
		if c.baseURL != "https://acme.atlassian.net" {
			t.Errorf("baseURL = %q", c.baseURL)
		}
	})
}
