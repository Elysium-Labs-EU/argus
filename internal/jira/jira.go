// Package jira fetches issues from Jira Cloud and renders them into the same
// forge.Issue shape argus's --issues pipeline already builds worker briefs
// from. Jira has no pull-request concept, so unlike internal/forge this is
// not a Forge implementation (no OpenPR) — just a FetchIssue-shaped reader
// that can feed the same downstream code.
package jira

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/forge"
)

// Client fetches issues from one Jira Cloud site.
type Client struct {
	http    *http.Client
	baseURL string // e.g. https://acme.atlassian.net
	email   string
	token   string
}

// NewFromEnv builds a Client from JIRA_BASE_URL, JIRA_EMAIL, and
// JIRA_API_TOKEN, following the same env-var-configured pattern as the other
// forges (see forge.TokenForHost). hc may be nil for a default client with a
// timeout.
func NewFromEnv(hc *http.Client) (*Client, error) {
	baseURL := os.Getenv("JIRA_BASE_URL")
	email := os.Getenv("JIRA_EMAIL")
	token := os.Getenv("JIRA_API_TOKEN")
	if baseURL == "" || email == "" || token == "" {
		return nil, fmt.Errorf("jira: JIRA_BASE_URL, JIRA_EMAIL, and JIRA_API_TOKEN must all be set")
	}
	return New(baseURL, email, token, hc), nil
}

// New builds a Client for baseURL (e.g. https://acme.atlassian.net),
// authenticated as email with an API token. hc may be nil for a default
// client with a timeout.
func New(baseURL, email, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	return &Client{
		http:    hc,
		baseURL: strings.TrimSuffix(baseURL, "/"),
		email:   email,
		token:   token,
	}
}

// issueResponse is the subset of a Jira Cloud v3 issue payload we read.
type issueResponse struct {
	Key    string `json:"key"`
	Fields struct {
		Summary     string  `json:"summary"`
		Description adfNode `json:"description"`
	} `json:"fields"`
}

// FetchIssue fetches one issue by key (e.g. "PROJ-123") and renders it into
// a forge.Issue: fields.summary becomes Title, and fields.description — an
// Atlassian Document Format tree, not plain text — is flattened to plain
// text for Body via flattenADF. Number is the numeric suffix of the key when
// there is one, else 0.
func (c *Client) FetchIssue(ctx context.Context, key string) (forge.Issue, error) {
	url := fmt.Sprintf("%s/rest/api/3/issue/%s", c.baseURL, key)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return forge.Issue{}, fmt.Errorf("building request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Basic "+basicAuth(c.email, c.token))

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return forge.Issue{}, fmt.Errorf("GET %s: %w", url, err)
	}
	if resp == nil {
		return forge.Issue{}, fmt.Errorf("GET %s: nil response", url)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return forge.Issue{}, fmt.Errorf("jira returned %s: %s", resp.Status, apiMessage(body))
	}

	var iss issueResponse
	if err := json.Unmarshal(body, &iss); err != nil {
		return forge.Issue{}, fmt.Errorf("decoding issue response: %w", err)
	}

	return forge.Issue{
		Title:  iss.Fields.Summary,
		Body:   flattenADF(iss.Fields.Description),
		Number: numberFromKey(iss.Key),
	}, nil
}

// basicAuth returns the base64(email:token) credential for a Basic
// Authorization header, per Jira Cloud's API token auth scheme.
func basicAuth(email, token string) string {
	return base64.StdEncoding.EncodeToString([]byte(email + ":" + token))
}

// numberFromKey extracts the numeric suffix of a Jira key (PROJ-123 -> 123)
// so it can populate forge.Issue.Number; keys with no numeric suffix yield 0.
func numberFromKey(key string) int {
	i := strings.LastIndexByte(key, '-')
	if i < 0 {
		return 0
	}
	n, err := strconv.Atoi(key[i+1:])
	if err != nil {
		return 0
	}
	return n
}

// apiMessage pulls Jira's error-message shape out of a non-2xx body, falling
// back to the raw (truncated) body when it isn't that shape. Jira errors
// look like {"errorMessages":["..."],"errors":{}}.
func apiMessage(body []byte) string {
	var e struct {
		ErrorMessages []string `json:"errorMessages"`
	}
	if err := json.Unmarshal(body, &e); err == nil && len(e.ErrorMessages) > 0 {
		return strings.Join(e.ErrorMessages, "; ")
	}
	if len(body) > 300 {
		body = body[:300]
	}
	return string(body)
}
