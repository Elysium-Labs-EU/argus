package forge

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestGitLabPRChecksMergeReadyWhenAllSucceed(t *testing.T) {
	hc := sequencedHTTP(t, []string{
		`{"head_pipeline":{"id":55}}`,
		`[
			{"name":"build","status":"success","web_url":"https://gitlab.com/o/r/-/jobs/1"},
			{"name":"lint","status":"skipped"}
		]`,
	})
	f, err := New("gitlab.com", "t", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	checks, err := f.PRChecks(context.Background(), "o", "r", 9)
	if err != nil {
		t.Fatalf("PRChecks: %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("checks = %+v, want 2", checks)
	}
	for _, c := range checks {
		if !c.Terminal() || c.Failed() {
			t.Errorf("check %+v: want Terminal()=true Failed()=false", c)
		}
	}
	if checks[0].LogURL != "https://gitlab.com/o/r/-/jobs/1" {
		t.Errorf("LogURL = %q, want the job's web_url", checks[0].LogURL)
	}
}

func TestGitLabPRChecksNamesFailingCheck(t *testing.T) {
	hc := sequencedHTTP(t, []string{
		`{"head_pipeline":{"id":55}}`,
		`[
			{"name":"build","status":"success"},
			{"name":"test","status":"failed","web_url":"https://gitlab.com/o/r/-/jobs/2"}
		]`,
	})
	f, err := New("gitlab.com", "t", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	checks, err := f.PRChecks(context.Background(), "o", "r", 9)
	if err != nil {
		t.Fatalf("PRChecks: %v", err)
	}
	var failing []Check
	for _, c := range checks {
		if c.Failed() {
			failing = append(failing, c)
		}
	}
	if len(failing) != 1 || failing[0].Name != "test" {
		t.Fatalf("failing checks = %+v, want just %q", failing, "test")
	}
	if failing[0].Conclusion != "failure" {
		t.Errorf("Conclusion = %q, want %q", failing[0].Conclusion, "failure")
	}
}

func TestGitLabPRChecksCanceledMapsToCancelledConclusion(t *testing.T) {
	hc := sequencedHTTP(t, []string{
		`{"head_pipeline":{"id":55}}`,
		`[{"name":"deploy","status":"canceled"}]`,
	})
	f, err := New("gitlab.com", "t", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	checks, err := f.PRChecks(context.Background(), "o", "r", 9)
	if err != nil {
		t.Fatalf("PRChecks: %v", err)
	}
	if len(checks) != 1 || checks[0].Conclusion != "cancelled" || !checks[0].Terminal() || !checks[0].Failed() {
		t.Fatalf("checks = %+v, want one terminal, failed cancelled check", checks)
	}
}

// TestGitLabPRChecksMixedRunningAndTerminalIsNotAllDone pins tend's own
// done-detection: a pipeline with even one still-running job must not read
// as merge-ready just because the rest already finished.
func TestGitLabPRChecksMixedRunningAndTerminalIsNotAllDone(t *testing.T) {
	hc := sequencedHTTP(t, []string{
		`{"head_pipeline":{"id":55}}`,
		`[
			{"name":"build","status":"success"},
			{"name":"test","status":"running"}
		]`,
	})
	f, err := New("gitlab.com", "t", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	checks, err := f.PRChecks(context.Background(), "o", "r", 9)
	if err != nil {
		t.Fatalf("PRChecks: %v", err)
	}
	allTerminal := true
	for _, c := range checks {
		if !c.Terminal() {
			allTerminal = false
		}
	}
	if allTerminal {
		t.Error("want at least one non-terminal check while a job is still running")
	}
}

// TestGitLabPRChecksPendingAndCreatedAreNotTerminal covers the remaining
// non-terminal statuses the design lists alongside "running".
func TestGitLabPRChecksPendingAndCreatedAreNotTerminal(t *testing.T) {
	hc := sequencedHTTP(t, []string{
		`{"head_pipeline":{"id":55}}`,
		`[
			{"name":"queued","status":"pending"},
			{"name":"not-started","status":"created"}
		]`,
	})
	f, err := New("gitlab.com", "t", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	checks, err := f.PRChecks(context.Background(), "o", "r", 9)
	if err != nil {
		t.Fatalf("PRChecks: %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("checks = %+v, want 2", checks)
	}
	for _, c := range checks {
		if c.Terminal() {
			t.Errorf("check %+v: want Terminal()=false", c)
		}
	}
}

// TestGitLabPRChecksManualJobMapsToSkippedNonBlocking pins the documented
// design decision: a manual job GitLab never auto-triggers must not leave
// tend blocking forever, so it maps to a terminal, non-blocking "skipped"
// conclusion — the same one GitHub gives an intentionally-not-run check.
func TestGitLabPRChecksManualJobMapsToSkippedNonBlocking(t *testing.T) {
	hc := sequencedHTTP(t, []string{
		`{"head_pipeline":{"id":55}}`,
		`[
			{"name":"build","status":"success"},
			{"name":"deploy-to-prod","status":"manual"}
		]`,
	})
	f, err := New("gitlab.com", "t", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	checks, err := f.PRChecks(context.Background(), "o", "r", 9)
	if err != nil {
		t.Fatalf("PRChecks: %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("checks = %+v, want 2", checks)
	}
	for _, c := range checks {
		if !c.Terminal() || c.Failed() {
			t.Errorf("check %+v: want Terminal()=true Failed()=false (manual job must not hang tend)", c)
		}
	}
}

// TestGitLabPRChecksFallsBackToLegacyPipelineField covers an MR response
// whose head_pipeline is absent (older GitLab versions only ever populate
// the legacy "pipeline" field) so PRChecks still finds a pipeline to poll.
func TestGitLabPRChecksFallsBackToLegacyPipelineField(t *testing.T) {
	hc := sequencedHTTP(t, []string{
		`{"pipeline":{"id":77}}`,
		`[{"name":"build","status":"success"}]`,
	})
	f, err := New("gitlab.com", "t", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	checks, err := f.PRChecks(context.Background(), "o", "r", 9)
	if err != nil {
		t.Fatalf("PRChecks: %v", err)
	}
	if len(checks) != 1 || checks[0].Name != "build" {
		t.Fatalf("checks = %+v, want the single job from the legacy pipeline", checks)
	}
}

// TestGitLabPRChecksNoPipelineYetReturnsEmptyChecks covers an MR that has no
// pipeline at all yet (CI hasn't picked it up): PRChecks must report zero
// checks with no error, the same "not started" signal tend's evaluateChecks
// already reads for a GitHub PR before any check has posted, rather than
// erroring on a merge request that just hasn't run CI yet.
func TestGitLabPRChecksNoPipelineYetReturnsEmptyChecks(t *testing.T) {
	hc := fakeHTTP(t, "", "", `{}`, 200)
	f, err := New("gitlab.com", "t", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	checks, err := f.PRChecks(context.Background(), "o", "r", 9)
	if err != nil {
		t.Fatalf("PRChecks: %v", err)
	}
	if len(checks) != 0 {
		t.Errorf("checks = %+v, want none", checks)
	}
}

// TestGitLabPRChecksPaginatesBeyondFirstPage pins the same page-boundary
// hazard as the GitHub client: a page exactly as long as gitlabJobsPerPage
// must not be mistaken for the last one, or a failing job sitting on page 2
// would never be fetched at all.
func TestGitLabPRChecksPaginatesBeyondFirstPage(t *testing.T) {
	page1Jobs := make([]map[string]string, gitlabJobsPerPage)
	for i := range page1Jobs {
		page1Jobs[i] = map[string]string{"name": fmt.Sprintf("job-%d", i), "status": "success"}
	}
	page1Body, err := json.Marshal(page1Jobs)
	if err != nil {
		t.Fatalf("marshaling page 1 fixture: %v", err)
	}
	page2Body, err := json.Marshal([]map[string]string{
		{"name": "job-page-2-failing", "status": "failed"},
	})
	if err != nil {
		t.Fatalf("marshaling page 2 fixture: %v", err)
	}

	replies := []string{`{"head_pipeline":{"id":55}}`, string(page1Body), string(page2Body)}
	var gotURLs []string
	call := 0
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotURLs = append(gotURLs, r.URL.String())
		i := call
		if i >= len(replies) {
			i = len(replies) - 1
		}
		call++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(replies[i])), Header: make(http.Header)}, nil
	})}

	f, err := New("gitlab.com", "t", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	checks, err := f.PRChecks(context.Background(), "o", "r", 9)
	if err != nil {
		t.Fatalf("PRChecks: %v", err)
	}
	if len(checks) != gitlabJobsPerPage+1 {
		t.Fatalf("checks = %d, want %d (a full first page plus one on page 2)", len(checks), gitlabJobsPerPage+1)
	}
	var failing *Check
	for i := range checks {
		if checks[i].Name == "job-page-2-failing" {
			failing = &checks[i]
		}
	}
	if failing == nil {
		t.Fatal("want the page-2-only check present in the result")
	}
	if !failing.Failed() {
		t.Errorf("job-page-2-failing: Failed() = false, want true")
	}
	if len(gotURLs) != 3 {
		t.Fatalf("requests made = %d, want 3 (MR lookup + 2 job pages), got %v", len(gotURLs), gotURLs)
	}
	if !strings.Contains(gotURLs[1], "page=1") || !strings.Contains(gotURLs[2], "page=2") {
		t.Errorf("want the page param to increment across requests, got %v", gotURLs)
	}
}

// TestGitLabFetchIssueCommentsFiltersSystemNotes pins the reason
// FetchIssueComments can't just return every note verbatim: GitLab mixes a
// human's actual comments into the same list as its own system-generated
// activity (e.g. "changed the description"), and a system note carries no
// context a worker needs.
func TestGitLabFetchIssueCommentsFiltersSystemNotes(t *testing.T) {
	hc := fakeHTTP(t, "/projects/o%2Fr/issues/42/notes", "", `[
		{"author":{"username":"alice"},"body":"please add a test","system":false},
		{"author":{"username":"argus-bot"},"body":"changed the description","system":true},
		{"author":{"username":"bob"},"body":"done","system":false}
	]`, 200)
	f, err := New("gitlab.com", "t", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	comments, err := f.FetchIssueComments(context.Background(), "o", "r", 42)
	if err != nil {
		t.Fatalf("FetchIssueComments: %v", err)
	}
	want := []Comment{{Author: "alice", Body: "please add a test"}, {Author: "bob", Body: "done"}}
	if len(comments) != len(want) || comments[0] != want[0] || comments[1] != want[1] {
		t.Errorf("comments = %+v, want %+v (system note filtered out)", comments, want)
	}
}

func TestGitLabFetchIssueCommentsPaginatesBeyondFirstPage(t *testing.T) {
	page1Notes := make([]map[string]any, gitlabIssueNotesPerPage)
	for i := range page1Notes {
		page1Notes[i] = map[string]any{"author": map[string]string{"username": "u"}, "body": fmt.Sprintf("c%d", i), "system": false}
	}
	page1Body, err := json.Marshal(page1Notes)
	if err != nil {
		t.Fatalf("marshaling page 1 fixture: %v", err)
	}
	page2Body, err := json.Marshal([]map[string]any{
		{"author": map[string]string{"username": "last"}, "body": "final", "system": false},
	})
	if err != nil {
		t.Fatalf("marshaling page 2 fixture: %v", err)
	}
	hc := sequencedHTTP(t, []string{string(page1Body), string(page2Body)})
	f, err := New("gitlab.com", "t", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	comments, err := f.FetchIssueComments(context.Background(), "o", "r", 42)
	if err != nil {
		t.Fatalf("FetchIssueComments: %v", err)
	}
	if len(comments) != gitlabIssueNotesPerPage+1 {
		t.Fatalf("got %d comments, want %d", len(comments), gitlabIssueNotesPerPage+1)
	}
	if comments[len(comments)-1].Author != "last" {
		t.Errorf("last comment author = %q, want last", comments[len(comments)-1].Author)
	}
}

func TestGitLabFetchIssueCommentsDecodeError(t *testing.T) {
	hc := fakeHTTP(t, "", "", `not json`, 200)
	f, err := New("gitlab.com", "t", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = f.FetchIssueComments(context.Background(), "o", "r", 42)
	if err == nil || !strings.Contains(err.Error(), "decoding issue notes response") {
		t.Errorf("FetchIssueComments err = %v, want it to mention decoding issue notes response", err)
	}
}

func TestGitLabFetchIssueCommentsSurfacesDoError(t *testing.T) {
	hc := fakeHTTP(t, "", "", `{"message":"issue not found"}`, 404)
	f, err := New("gitlab.com", "t", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = f.FetchIssueComments(context.Background(), "o", "r", 42)
	if err == nil || !strings.Contains(err.Error(), "issue not found") {
		t.Errorf("FetchIssueComments err = %v, want it to surface the 404 API message", err)
	}
}

func TestGitLabPRChecksMRLookupDecodeError(t *testing.T) {
	hc := fakeHTTP(t, "", "", `not json`, 200)
	f, err := New("gitlab.com", "t", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = f.PRChecks(context.Background(), "o", "r", 9)
	if err == nil || !strings.Contains(err.Error(), "decoding merge request response") {
		t.Errorf("PRChecks error = %v, want it to mention decoding merge request response", err)
	}
}

func TestGitLabPRChecksJobsDecodeError(t *testing.T) {
	hc := sequencedHTTP(t, []string{
		`{"head_pipeline":{"id":55}}`,
		`not json`,
	})
	f, err := New("gitlab.com", "t", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = f.PRChecks(context.Background(), "o", "r", 9)
	if err == nil || !strings.Contains(err.Error(), "decoding pipeline jobs response") {
		t.Errorf("PRChecks error = %v, want it to mention decoding pipeline jobs response", err)
	}
}

func TestGitLabPRChecksMRLookupSurfacesDoError(t *testing.T) {
	hc := fakeHTTP(t, "", "", `{"message":"merge request not found"}`, 404)
	f, err := New("gitlab.com", "t", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = f.PRChecks(context.Background(), "o", "r", 9)
	if err == nil || !strings.Contains(err.Error(), "merge request not found") {
		t.Errorf("PRChecks error = %v, want it to surface the 404 API message", err)
	}
}

func TestGitLabPRChecksJobsLookupSurfacesDoError(t *testing.T) {
	call := 0
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		call++
		if call == 1 {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"head_pipeline":{"id":55}}`)), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{"message":"pipeline not found"}`)), Header: make(http.Header)}, nil
	})}
	f, err := New("gitlab.com", "t", hc, KindAuto, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = f.PRChecks(context.Background(), "o", "r", 9)
	if err == nil || !strings.Contains(err.Error(), "pipeline not found") {
		t.Errorf("PRChecks error = %v, want it to surface the 404 API message", err)
	}
}
