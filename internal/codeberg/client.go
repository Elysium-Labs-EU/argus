// Package codeberg is a minimal typed client for the Codeberg (Gitea) REST API,
// scoped to what argus needs to ship a worker's change: opening a pull request.
// It talks HTTPS to codeberg.org only, authenticated with a token, and models
// just the request/response fields used.
package codeberg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const apiBase = "https://codeberg.org/api/v1"

// Client opens pull requests against a Codeberg repository.
type Client struct {
	http  *http.Client
	token string
}

// New returns a Client authenticated with token. A nil-safe default HTTP client
// with a timeout is used.
func New(token string) Client {
	return Client{http: &http.Client{Timeout: 20 * time.Second}, token: token}
}

// NewWithHTTP returns a Client backed by a caller-supplied *http.Client, for
// tests (a fake transport) or custom timeouts.
func NewWithHTTP(token string, hc *http.Client) Client {
	return Client{http: hc, token: token}
}

// PRRequest is the pull request to open. Head and Base are branch names in the
// same repository (argus pushes the worker's branch to origin before opening).
type PRRequest struct {
	Owner string
	Repo  string
	Title string
	Body  string
	Head  string
	Base  string
}

// PR is the subset of a created pull request argus reports back.
type PR struct {
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Number  int    `json:"number"`
}

// OpenPR creates a pull request. It returns a typed error carrying the API's
// message on non-2xx (Codeberg returns 409 when a PR for the branch already
// exists, 422 for validation problems), so callers can surface a useful hint.
func (c Client) OpenPR(ctx context.Context, req *PRRequest) (PR, error) {
	payload, err := json.Marshal(map[string]string{
		"title": req.Title,
		"body":  req.Body,
		"head":  req.Head,
		"base":  req.Base,
	})
	if err != nil {
		return PR{}, fmt.Errorf("encoding pull request: %w", err)
	}

	url := fmt.Sprintf("%s/repos/%s/%s/pulls", apiBase, req.Owner, req.Repo)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return PR{}, fmt.Errorf("building pull request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if c.token != "" {
		httpReq.Header.Set("Authorization", "token "+c.token)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return PR{}, fmt.Errorf("opening pull request: %w", err)
	}
	if resp == nil {
		return PR{}, fmt.Errorf("opening pull request: nil response")
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return PR{}, fmt.Errorf("codeberg returned %s: %s", resp.Status, apiMessage(body))
	}

	var pr PR
	if err := json.Unmarshal(body, &pr); err != nil {
		return PR{}, fmt.Errorf("decoding pull request response: %w", err)
	}
	return pr, nil
}

// apiMessage pulls the human-readable "message" field out of a Gitea error body,
// falling back to the raw (truncated) body when it isn't the expected shape.
func apiMessage(body []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &e); err == nil && e.Message != "" {
		return e.Message
	}
	if len(body) > 300 {
		body = body[:300]
	}
	return string(body)
}
