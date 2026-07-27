// Package forge is argus's abstraction over the git host it ships to. argus was
// born Codeberg-only; this package lets it open pull requests and read issues on
// GitHub and GitLab too, selected by the host in the worktree's git remote for
// the three hosted forges (github.com, gitlab.com, codeberg.org) and by an
// explicit Kind override for anything self-hosted, where hostname alone can't
// say which REST shape it speaks. Gitea/Forgejo (Codeberg) and GitHub differ
// only in a few details (base URL, auth scheme, Accept header) over an
// otherwise identical REST surface, so one parameterized client serves both.
// GitLab's shape (PRIVATE-TOKEN auth, merge requests instead of pulls) differs
// enough to warrant its own client.
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

// PR is the subset of a pull/merge request argus reports back or reads to
// check merge state. MergedAt is nil for an open or closed-without-merging
// PR; GitHub, Gitea, and GitLab all expose this field under the same name, so
// a caller never has to special-case which forge answered.
type PR struct {
	MergedAt *time.Time `json:"merged_at"`
	HTMLURL  string     `json:"html_url"`
	State    string     `json:"state"`
	Number   int        `json:"number"`
}

// Merged reports whether the PR has been merged. It is the single place that
// knows MergedAt is the ground truth (not State, which is "closed" for both a
// merge and an abandoned close).
func (p PR) Merged() bool {
	return p.MergedAt != nil
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
	// FindPR looks up the most recent PR (any state) whose head is branch, for
	// callers that know a worktree's branch but not its PR number — notably
	// `argus worktree prune` clearing a worktree ship opened before it, or one
	// from before this lookup existed. found is false (with no error) when no
	// PR was ever opened for branch.
	FindPR(ctx context.Context, owner, repo, branch string) (pr PR, found bool, err error)
	Host() string
}

// Kind explicitly selects a forge's API shape for a host outside New's
// hosted-forge allowlist: a self-hosted GitLab instance and a self-hosted
// Gitea/Forgejo instance are indistinguishable by host name alone, but their
// REST surfaces (merge_requests vs pulls, PRIVATE-TOKEN vs token auth) are
// not interchangeable. KindAuto defers to New's allowlist-based detection.
type Kind string

const (
	KindAuto   Kind = ""
	KindGitLab Kind = "gitlab"
	KindGitea  Kind = "gitea"
)

// New returns a Forge for host, authenticated with token. hc may be nil for a
// default client with a timeout.
//
// With kind == KindAuto, only the three hosted forges argus knows the exact
// API shape of without being told — github.com, gitlab.com, codeberg.org —
// auto-route. Every other host, self-hosted or not, is refused: a host name
// is not a reliable signal for which REST shape (Gitea/Forgejo's or GitLab's)
// a self-hosted instance actually speaks — "git.company.com" and
// "code.company.com" are exactly as likely to be either as something with
// "gitlab" in the name is — and guessing wrong sends the wrong shaped API
// request instead of a clean error. Pass kind == KindGitLab or KindGitea to
// say which shape a non-hosted host actually is and bypass that refusal.
func New(host, token string, hc *http.Client, kind Kind) (Forge, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	switch kind {
	case KindGitLab:
		return &gitlab{host: host, base: "https://" + host + "/api/v4", http: hc, token: token}, nil
	case KindGitea:
		return newGiteaShaped(host, hc, token), nil
	case KindAuto:
		// fall through to the allowlist below
	default:
		return nil, fmt.Errorf("unknown forge kind %q: want %q or %q", kind, KindGitLab, KindGitea)
	}
	switch host {
	case "github.com":
		return &rest{
			host: host, base: "https://api.github.com",
			authScheme: "Bearer", accept: "application/vnd.github+json",
			http: hc, token: token,
		}, nil
	case "gitlab.com":
		return &gitlab{host: host, base: "https://gitlab.com/api/v4", http: hc, token: token}, nil
	case "codeberg.org":
		return newGiteaShaped(host, hc, token), nil
	default:
		return nil, fmt.Errorf(
			"host %q is not one of the auto-detected forges (github.com, gitlab.com, codeberg.org); "+
				"argus cannot tell a self-hosted GitLab and a self-hosted Gitea/Forgejo apart by hostname "+
				"alone, and guessing wrong sends the wrong shaped API request instead of a clean error — "+
				"pass --forge gitlab or --forge gitea to say which this host is", host)
	}
}

// newGiteaShaped builds the shared REST client for Gitea/Forgejo, whose API
// lives at https://<host>/api/v1.
func newGiteaShaped(host string, hc *http.Client, token string) Forge {
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

// FindPR lists PRs against branch's head and returns the most recent one.
// GitHub's list endpoint supports filtering server-side via head=owner:branch;
// Gitea's does not, so for that host the (typically short) list is filtered
// client-side instead.
func (r *rest) FindPR(ctx context.Context, owner, repo, branch string) (PR, bool, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls?state=all", r.base, owner, repo)
	if r.host == "github.com" {
		url += "&head=" + owner + ":" + branch
	}
	body, err := r.do(ctx, http.MethodGet, url, nil)
	if err != nil {
		return PR{}, false, err
	}
	var prs []struct {
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
		PR
	}
	if err := json.Unmarshal(body, &prs); err != nil {
		return PR{}, false, fmt.Errorf("decoding pull request list: %w", err)
	}
	for _, p := range prs {
		if r.host == "github.com" || p.Head.Ref == branch {
			return p.PR, true, nil
		}
	}
	return PR{}, false, nil
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
