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
	"path/filepath"
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

// configPathEnvVar overrides the default config-file location NewFromEnv
// falls back to when the JIRA_* env vars aren't all set. It exists mainly so
// tests can point at a throwaway file instead of the developer's real
// ~/.argus.
const configPathEnvVar = "JIRA_CONFIG_FILE"

// Config is the on-disk shape of the JSON file NewFromEnv falls back to when
// JIRA_BASE_URL, JIRA_EMAIL, and JIRA_API_TOKEN aren't all set in the env.
// It exists so credentials already provisioned for another tool — e.g. an
// Atlassian MCP server a session already has Jira access through — can be
// written to one file once instead of duplicated as env vars for argus too.
type Config struct {
	BaseURL  string `json:"base_url"`
	Email    string `json:"email"`
	APIToken string `json:"api_token"`
}

// NewFromEnv builds a Client from JIRA_BASE_URL, JIRA_EMAIL, and
// JIRA_API_TOKEN if all three are set, following the same env-var-configured
// pattern as the other forges (see forge.TokenForHost). Otherwise it falls
// back to a JSON config file — $JIRA_CONFIG_FILE if set, else
// ~/.argus/jira.json — so credentials provisioned once for another tool
// don't need to be duplicated as env vars (see Config). hc may be nil for a
// default client with a timeout.
func NewFromEnv(hc *http.Client) (*Client, error) {
	baseURL := os.Getenv("JIRA_BASE_URL")
	email := os.Getenv("JIRA_EMAIL")
	token := os.Getenv("JIRA_API_TOKEN")
	if baseURL != "" && email != "" && token != "" {
		return New(baseURL, email, token, hc), nil
	}

	path, pathErr := configFilePath()
	if pathErr == nil {
		cfg, cfgErr := readConfigFile(path)
		switch {
		case cfgErr == nil:
			return New(cfg.BaseURL, cfg.Email, cfg.APIToken, hc), nil
		case !os.IsNotExist(cfgErr):
			return nil, fmt.Errorf("jira: reading config file %s: %w", path, cfgErr)
		}
	}

	return nil, fmt.Errorf("jira: set JIRA_BASE_URL, JIRA_EMAIL, and JIRA_API_TOKEN, or write them as JSON to %s (see jira.Config)", path)
}

// configFilePath resolves the JSON config file NewFromEnv reads when the
// JIRA_* env vars aren't all set: $JIRA_CONFIG_FILE if set, else
// ~/.argus/jira.json, matching the ~/.argus directory the rest of argus
// already uses for its own state (see cmd.argusDataDir).
func configFilePath() (string, error) {
	if p := os.Getenv(configPathEnvVar); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".argus", "jira.json"), nil
}

// readConfigFile reads and validates a Jira Config file. A missing file
// surfaces as an os.IsNotExist error so NewFromEnv can tell "no config file"
// apart from "config file is broken" and give a more specific message for
// the latter.
func readConfigFile(path string) (Config, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is an operator-set env var or our own fixed ~/.argus/jira.json, not attacker input
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing JSON: %w", err)
	}
	if cfg.BaseURL == "" || cfg.Email == "" || cfg.APIToken == "" {
		return Config{}, fmt.Errorf("base_url, email, and api_token must all be set")
	}
	return cfg, nil
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
