package jira

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// fakeCloudID is the cloudId every mock /_edge/tenant_info handler in this
// file returns, so tests can assert the resolved request path/host without
// caring about the exact value.
const fakeCloudID = "11111111-2222-3333-4444-555555555555"

// withMockAtlassianAPI points the package-level apiAtlassianPrefix var (see
// jira.go) at a local httptest.Server for the duration of one test, so
// resolved cloudId requests hit the mock server instead of the real
// api.atlassian.com host. It restores the original value on cleanup.
func withMockAtlassianAPI(t *testing.T, srvURL string) {
	t.Helper()
	original := apiAtlassianPrefix
	apiAtlassianPrefix = srvURL + "/ex/jira/"
	t.Cleanup(func() { apiAtlassianPrefix = original })
}

// tenantInfoHandler returns an http.HandlerFunc serving /_edge/tenant_info
// with fakeCloudID, and incrementing *hits on every call, so tests can
// assert resolution happened (or only happened once).
func tenantInfoHandler(hits *int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cloudId":"` + fakeCloudID + `"}`))
	}
}

func TestFetchIssue(t *testing.T) {
	var gotAuth, gotPath string
	var tenantHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/_edge/tenant_info", tenantInfoHandler(&tenantHits))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fetchFixtureBody))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withMockAtlassianAPI(t, srv.URL)

	c := New(srv.URL, "dev@example.com", "secret-token", nil)
	iss, err := c.FetchIssue(context.Background(), "PROJ-42")
	if err != nil {
		t.Fatalf("FetchIssue: %v", err)
	}

	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("dev@example.com:secret-token"))
	if gotAuth != wantAuth {
		t.Errorf("Authorization header = %q, want %q", gotAuth, wantAuth)
	}
	wantPath := "/ex/jira/" + fakeCloudID + "/rest/api/3/issue/PROJ-42"
	if gotPath != wantPath {
		t.Errorf("request path = %q, want %q", gotPath, wantPath)
	}
	if tenantHits != 1 {
		t.Errorf("tenant_info hits = %d, want 1", tenantHits)
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
	var tenantHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/_edge/tenant_info", tenantInfoHandler(&tenantHits))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errorMessages":["Issue does not exist"],"errors":{}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withMockAtlassianAPI(t, srv.URL)

	c := New(srv.URL, "dev@example.com", "secret-token", nil)
	_, err := c.FetchIssue(context.Background(), "PROJ-999")
	if err == nil || !strings.Contains(err.Error(), "Issue does not exist") {
		t.Errorf("want surfaced API message, got %v", err)
	}
}

// TestFetchIssueResolvesCloudID asserts the cloudId resolution round trip:
// the first FetchIssue call hits /_edge/tenant_info on the configured site
// domain, then issues its actual request against the resolved
// api.atlassian.com/ex/jira/{cloudId} base rather than the raw site URL —
// and a second call reuses the cached resolution instead of resolving again
// (see resolveOnce in jira.go).
func TestFetchIssueResolvesCloudID(t *testing.T) {
	var tenantHits int
	var gotHosts []string
	mux := http.NewServeMux()
	mux.HandleFunc("/_edge/tenant_info", tenantInfoHandler(&tenantHits))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		gotHosts = append(gotHosts, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fetchFixtureBody))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withMockAtlassianAPI(t, srv.URL)

	c := New(srv.URL, "dev@example.com", "secret-token", nil)

	if _, err := c.FetchIssue(context.Background(), "PROJ-1"); err != nil {
		t.Fatalf("first FetchIssue: %v", err)
	}
	if _, err := c.FetchIssue(context.Background(), "PROJ-2"); err != nil {
		t.Fatalf("second FetchIssue: %v", err)
	}

	if tenantHits != 1 {
		t.Errorf("tenant_info hits = %d, want 1 (resolution should be cached)", tenantHits)
	}
	wantPrefix := "/ex/jira/" + fakeCloudID + "/rest/api/3/issue/"
	for _, p := range gotHosts {
		if !strings.HasPrefix(p, wantPrefix) {
			t.Errorf("request path = %q, want prefix %q", p, wantPrefix)
		}
	}
}

// TestFetchIssueSkipsResolutionForAlreadyResolvedBaseURL covers the escape
// hatch: if base_url is already configured as an api.atlassian.com/ex/jira/
// URL — someone worked around the granular-token bug manually before this
// fix existed — FetchIssue must not call /_edge/tenant_info at all, and
// must use the configured URL as-is.
func TestFetchIssueSkipsResolutionForAlreadyResolvedBaseURL(t *testing.T) {
	var tenantHits int
	var gotPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/_edge/tenant_info", tenantInfoHandler(&tenantHits))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fetchFixtureBody))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withMockAtlassianAPI(t, srv.URL)

	preResolvedBase := srv.URL + "/ex/jira/" + fakeCloudID
	c := New(preResolvedBase, "dev@example.com", "secret-token", nil)

	if _, err := c.FetchIssue(context.Background(), "PROJ-42"); err != nil {
		t.Fatalf("FetchIssue: %v", err)
	}

	if tenantHits != 0 {
		t.Errorf("tenant_info hits = %d, want 0 (already-resolved base_url should skip resolution)", tenantHits)
	}
	wantPath := "/ex/jira/" + fakeCloudID + "/rest/api/3/issue/PROJ-42"
	if gotPath != wantPath {
		t.Errorf("request path = %q, want %q", gotPath, wantPath)
	}
}

// TestFetchIssueSurfacesCloudIDResolutionError covers a broken
// /_edge/tenant_info response (non-2xx, or a 200 with no usable cloudId)
// surfacing as a clear FetchIssue error instead of silently falling through
// to a nonsensical request.
func TestFetchIssueSurfacesCloudIDResolutionError(t *testing.T) {
	t.Run("non-2xx response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("internal error"))
		}))
		defer srv.Close()

		c := New(srv.URL, "dev@example.com", "secret-token", nil)
		_, err := c.FetchIssue(context.Background(), "PROJ-1")
		if err == nil || !strings.Contains(err.Error(), "resolving cloud id") {
			t.Errorf("want resolution error, got %v", err)
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("not json"))
		}))
		defer srv.Close()

		c := New(srv.URL, "dev@example.com", "secret-token", nil)
		_, err := c.FetchIssue(context.Background(), "PROJ-1")
		if err == nil || !strings.Contains(err.Error(), "resolving cloud id") {
			t.Errorf("want resolution error, got %v", err)
		}
	})

	t.Run("valid JSON with no cloudId", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"someOtherField":"value"}`))
		}))
		defer srv.Close()

		c := New(srv.URL, "dev@example.com", "secret-token", nil)
		_, err := c.FetchIssue(context.Background(), "PROJ-1")
		if err == nil || !strings.Contains(err.Error(), "resolving cloud id") {
			t.Errorf("want resolution error, got %v", err)
		}
	})
}

// noConfigFile points JIRA_CONFIG_FILE at a path that does not exist, so
// tests exercising the missing-env-vars path don't accidentally pick up a
// real ~/.argus/jira.json on the machine running the test.
func noConfigFile(t *testing.T) {
	t.Helper()
	t.Setenv(configPathEnvVar, filepath.Join(t.TempDir(), "does-not-exist.json"))
}

func writeConfigFile(t *testing.T, cfg Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jira.json")
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshaling config: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing config file: %v", err)
	}
	return path
}

func TestNewFromEnv(t *testing.T) {
	t.Run("missing vars and no config file", func(t *testing.T) {
		t.Setenv("JIRA_BASE_URL", "")
		t.Setenv("JIRA_EMAIL", "")
		t.Setenv("JIRA_API_TOKEN", "")
		noConfigFile(t)
		if _, err := NewFromEnv(nil); err == nil {
			t.Error("want error when env vars are unset and no config file exists")
		}
	})

	t.Run("all set", func(t *testing.T) {
		t.Setenv("JIRA_BASE_URL", "https://acme.atlassian.net")
		t.Setenv("JIRA_EMAIL", "dev@example.com")
		t.Setenv("JIRA_API_TOKEN", "secret-token")
		noConfigFile(t)
		c, err := NewFromEnv(nil)
		if err != nil {
			t.Fatalf("NewFromEnv: %v", err)
		}
		if c.baseURL != "https://acme.atlassian.net" {
			t.Errorf("baseURL = %q", c.baseURL)
		}
	})

	t.Run("falls back to config file when env vars unset", func(t *testing.T) {
		t.Setenv("JIRA_BASE_URL", "")
		t.Setenv("JIRA_EMAIL", "")
		t.Setenv("JIRA_API_TOKEN", "")
		path := writeConfigFile(t, Config{
			BaseURL:  "https://acme.atlassian.net",
			Email:    "dev@example.com",
			APIToken: "secret-token",
		})
		t.Setenv(configPathEnvVar, path)

		c, err := NewFromEnv(nil)
		if err != nil {
			t.Fatalf("NewFromEnv: %v", err)
		}
		if c.baseURL != "https://acme.atlassian.net" || c.email != "dev@example.com" || c.token != "secret-token" {
			t.Errorf("client = %+v, want fields from config file", c)
		}
	})

	t.Run("env vars take precedence over config file", func(t *testing.T) {
		t.Setenv("JIRA_BASE_URL", "https://env.atlassian.net")
		t.Setenv("JIRA_EMAIL", "env@example.com")
		t.Setenv("JIRA_API_TOKEN", "env-token")
		path := writeConfigFile(t, Config{
			BaseURL:  "https://config.atlassian.net",
			Email:    "config@example.com",
			APIToken: "config-token",
		})
		t.Setenv(configPathEnvVar, path)

		c, err := NewFromEnv(nil)
		if err != nil {
			t.Fatalf("NewFromEnv: %v", err)
		}
		if c.baseURL != "https://env.atlassian.net" {
			t.Errorf("baseURL = %q, want env value to win", c.baseURL)
		}
	})

	t.Run("incomplete config file surfaces an error", func(t *testing.T) {
		t.Setenv("JIRA_BASE_URL", "")
		t.Setenv("JIRA_EMAIL", "")
		t.Setenv("JIRA_API_TOKEN", "")
		path := writeConfigFile(t, Config{BaseURL: "https://acme.atlassian.net"})
		t.Setenv(configPathEnvVar, path)

		if _, err := NewFromEnv(nil); err == nil {
			t.Error("want error when config file is missing required fields")
		}
	})

	t.Run("malformed config file surfaces an error", func(t *testing.T) {
		t.Setenv("JIRA_BASE_URL", "")
		t.Setenv("JIRA_EMAIL", "")
		t.Setenv("JIRA_API_TOKEN", "")
		path := filepath.Join(t.TempDir(), "jira.json")
		if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
			t.Fatalf("writing config file: %v", err)
		}
		t.Setenv(configPathEnvVar, path)

		if _, err := NewFromEnv(nil); err == nil {
			t.Error("want error when config file is not valid JSON")
		}
	})
}
