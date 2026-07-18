package codeberg

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// roundTripFunc adapts a function to an http.RoundTripper so tests can stand in
// for the real transport without a live server.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestOpenPRSendsCorrectRequest(t *testing.T) {
	var gotURL, gotAuth, gotMethod string
	var gotBody map[string]string

	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		return jsonResp(201, `{"number":7,"html_url":"https://codeberg.org/Elysium_Labs/eos/pulls/7","state":"open"}`), nil
	})}

	c := NewWithHTTP("secret-token", hc)
	pr, err := c.OpenPR(context.Background(), &PRRequest{
		Owner: "Elysium_Labs", Repo: "eos",
		Title: "fix: #144", Body: "Closes #144",
		Head: "fix-x", Base: "main",
	})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if pr.Number != 7 || !strings.HasSuffix(pr.HTMLURL, "/pulls/7") {
		t.Errorf("unexpected PR: %+v", pr)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: got %s", gotMethod)
	}
	if gotURL != "https://codeberg.org/api/v1/repos/Elysium_Labs/eos/pulls" {
		t.Errorf("url: got %s", gotURL)
	}
	if gotAuth != "token secret-token" {
		t.Errorf("auth: got %q", gotAuth)
	}
	if gotBody["head"] != "fix-x" || gotBody["base"] != "main" || gotBody["title"] != "fix: #144" {
		t.Errorf("body: got %+v", gotBody)
	}
}

func TestOpenPRSurfacesAPIError(t *testing.T) {
	hc := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return jsonResp(409, `{"message":"pull request already exists for these targets"}`), nil
	})}
	c := NewWithHTTP("t", hc)
	_, err := c.OpenPR(context.Background(), &PRRequest{Owner: "o", Repo: "r", Head: "h", Base: "b"})
	if err == nil {
		t.Fatal("want error on 409")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error should carry the API message, got %v", err)
	}
}
