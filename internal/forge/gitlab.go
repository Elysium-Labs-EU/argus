package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// gitlab implements Forge for GitLab.com's REST v4 API. GitLab differs from the
// Gitea/GitHub shape handled by rest enough (PRIVATE-TOKEN auth, merge_requests
// instead of pulls, source_branch/target_branch/description field names, iid
// instead of number) to warrant its own small client rather than bending rest to
// fit a third shape.
type gitlab struct {
	http  *http.Client
	host  string
	base  string
	token string
}

func (g *gitlab) Host() string { return g.host }

func (g *gitlab) OpenPR(ctx context.Context, req *PRRequest) (PR, error) {
	payload, err := json.Marshal(map[string]string{
		"source_branch": req.Head, "target_branch": req.Base, "title": req.Title, "description": req.Body,
	})
	if err != nil {
		return PR{}, fmt.Errorf("encoding merge request: %w", err)
	}
	url := fmt.Sprintf("%s/projects/%s/merge_requests", g.base, projectID(req.Owner, req.Repo))
	body, err := g.do(ctx, http.MethodPost, url, payload)
	if err != nil {
		return PR{}, err
	}
	var mr struct {
		WebURL string `json:"web_url"`
		State  string `json:"state"`
		IID    int    `json:"iid"`
	}
	if err := json.Unmarshal(body, &mr); err != nil {
		return PR{}, fmt.Errorf("decoding merge request response: %w", err)
	}
	return PR{HTMLURL: mr.WebURL, State: mr.State, Number: mr.IID}, nil
}

func (g *gitlab) FetchIssue(ctx context.Context, owner, repo string, number int) (Issue, error) {
	url := fmt.Sprintf("%s/projects/%s/issues/%d", g.base, projectID(owner, repo), number)
	body, err := g.do(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Issue{}, err
	}
	var issue struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		IID         int    `json:"iid"`
	}
	if err := json.Unmarshal(body, &issue); err != nil {
		return Issue{}, fmt.Errorf("decoding issue response: %w", err)
	}
	return Issue{Title: issue.Title, Body: issue.Description, Number: issue.IID}, nil
}

// projectID URL-encodes owner/repo into GitLab's namespaced-path project id form,
// e.g. "acme/widget" -> "acme%2Fwidget".
func projectID(owner, repo string) string {
	return url.QueryEscape(owner + "/" + repo)
}

// do performs one request and returns the response body, turning a non-2xx into a
// typed error carrying GitLab's message field.
func (g *gitlab) do(ctx context.Context, method, reqURL string, payload []byte) ([]byte, error) {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, reqURL, reader)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	if payload != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if g.token != "" {
		httpReq.Header.Set("PRIVATE-TOKEN", g.token)
	}

	resp, err := g.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, reqURL, err)
	}
	if resp == nil {
		return nil, fmt.Errorf("%s %s: nil response", method, reqURL)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s returned %s: %s", g.host, resp.Status, apiMessage(body))
	}
	return body, nil
}
