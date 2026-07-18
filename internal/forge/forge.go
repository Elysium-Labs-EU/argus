// Package forge is argus's abstraction over the git host it ships to. argus was
// born Codeberg-only; this package lets it open pull requests and read issues on
// GitHub too, selected by the host in the worktree's git remote. Gitea/Forgejo
// (Codeberg) and GitHub differ only in a few details (base URL, auth scheme,
// Accept header) over an otherwise identical REST surface, so one parameterized
// client serves both.
package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// PRRequest is the pull request to open. Head and Base are branch names in the
// same repository (argus pushes the worker's branch before opening).
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

// Issue is the subset of an issue argus reads to build a worker brief.
type Issue struct {
	Title  string `json:"title"`
	Body   string `json:"body"`
	Number int    `json:"number"`
}

// Forge is a git host argus can ship to and read issues from.
type Forge interface {
	OpenPR(ctx context.Context, req *PRRequest) (PR, error)
	FetchIssue(ctx context.Context, owner, repo string, number int) (Issue, error)
	Host() string
}

// New returns a Forge for host, authenticated with token. github.com maps to the
// GitHub API; every other host is treated as Gitea/Forgejo (Codeberg and any
// self-hosted instance), whose API lives at https://<host>/api/v1. hc may be nil
// for a default client with a timeout.
func New(host, token string, hc *http.Client) Forge {
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	if host == "github.com" {
		return &rest{
			host: host, base: "https://api.github.com",
			authScheme: "Bearer", accept: "application/vnd.github+json",
			http: hc, token: token,
		}
	}
	return &rest{
		host: host, base: "https://" + host + "/api/v1",
		authScheme: "token", accept: "application/json",
		http: hc, token: token,
	}
}

// rest is the shared REST implementation for Gitea/Forgejo and GitHub.
type rest struct {
	http       *http.Client
	host       string
	base       string
	authScheme string
	accept     string
	token      string
}

func (r *rest) Host() string { return r.host }

func (r *rest) OpenPR(ctx context.Context, req *PRRequest) (PR, error) {
	payload, err := json.Marshal(map[string]string{
		"title": req.Title, "body": req.Body, "head": req.Head, "base": req.Base,
	})
	if err != nil {
		return PR{}, fmt.Errorf("encoding pull request: %w", err)
	}
	url := fmt.Sprintf("%s/repos/%s/%s/pulls", r.base, req.Owner, req.Repo)
	body, err := r.do(ctx, http.MethodPost, url, payload)
	if err != nil {
		return PR{}, err
	}
	var pr PR
	if err := json.Unmarshal(body, &pr); err != nil {
		return PR{}, fmt.Errorf("decoding pull request response: %w", err)
	}
	return pr, nil
}

func (r *rest) FetchIssue(ctx context.Context, owner, repo string, number int) (Issue, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d", r.base, owner, repo, number)
	body, err := r.do(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Issue{}, err
	}
	var issue Issue
	if err := json.Unmarshal(body, &issue); err != nil {
		return Issue{}, fmt.Errorf("decoding issue response: %w", err)
	}
	return issue, nil
}

// do performs one request and returns the response body, turning a non-2xx into a
// typed error carrying the host's message field.
func (r *rest) do(ctx context.Context, method, url string, payload []byte) ([]byte, error) {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	if payload != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	httpReq.Header.Set("Accept", r.accept)
	if r.token != "" {
		httpReq.Header.Set("Authorization", r.authScheme+" "+r.token)
	}

	resp, err := r.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	if resp == nil {
		return nil, fmt.Errorf("%s %s: nil response", method, url)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s returned %s: %s", r.host, resp.Status, apiMessage(body))
	}
	return body, nil
}

// apiMessage pulls the human-readable "message" field out of a Gitea/GitHub error
// body, falling back to the raw (truncated) body when it isn't that shape.
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
