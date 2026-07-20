package credproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeUpstream stands in for a real API. It records the last request's auth
// headers so tests can prove the sentinel was swapped for the real key, and
// echoes a fixed body so a proxied round-trip is observable end to end.
type fakeUpstream struct {
	server   *httptest.Server
	gotKey   string
	gotAuth  string
	gotPath  string
	gotVers  string
	requests int
}

func newFakeUpstream() *fakeUpstream {
	f := &fakeUpstream{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests++
		f.gotKey = r.Header.Get("x-api-key")
		f.gotAuth = r.Header.Get("Authorization")
		f.gotVers = r.Header.Get("anthropic-version")
		f.gotPath = r.URL.Path
		_, _ = io.WriteString(w, "ok")
	}))
	return f
}

// upstreamFor builds an Anthropic-shaped upstream pointed at the fake server
// instead of the real api.anthropic.com.
func upstreamFor(f *fakeUpstream, realKey string) *Upstream {
	u := Anthropic(realKey)
	u.Target, _ = url.Parse(f.server.URL)
	return u
}

// startProxy spins up a proxy fronting one fake Anthropic upstream and returns it
// wired for teardown.
func startProxy(t *testing.T, realKey string, log LogFunc) (*Proxy, *fakeUpstream) {
	t.Helper()
	f := newFakeUpstream()
	t.Cleanup(f.server.Close)

	p := New(log, upstreamFor(f, realKey))
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})
	return p, f
}

// sentinelFrom extracts the ANTHROPIC_API_KEY value the proxy issued to a worker.
func sentinelFrom(t *testing.T, env []string) string {
	t.Helper()
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "ANTHROPIC_API_KEY" {
			return v
		}
	}
	t.Fatalf("no ANTHROPIC_API_KEY in %v", env)
	return ""
}

// baseFrom extracts the ANTHROPIC_BASE_URL the proxy issued to a worker.
func baseFrom(t *testing.T, env []string) string {
	t.Helper()
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "ANTHROPIC_BASE_URL" {
			return v
		}
	}
	t.Fatalf("no ANTHROPIC_BASE_URL in %v", env)
	return ""
}

func TestValidSentinelSwapsInRealKey(t *testing.T) {
	const realKey = "sk-real-do-not-leak"
	p, f := startProxy(t, realKey, nil)

	env := p.WorkerEnv("agent-1", "feat/x")
	sentinel := sentinelFrom(t, env)
	base := baseFrom(t, env)

	if sentinel == realKey {
		t.Fatal("sentinel must never equal the real key")
	}

	resp := do(t, http.MethodPost, base+"/v1/messages", sentinel)
	if resp != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp)
	}
	if f.gotKey != realKey {
		t.Fatalf("upstream saw x-api-key %q, want the real key", f.gotKey)
	}
	if strings.Contains(f.gotAuth, sentinel) || f.gotKey == sentinel {
		t.Fatal("sentinel leaked to the upstream")
	}
	if f.gotVers == "" {
		t.Fatal("proxy did not supply the required anthropic-version header")
	}
	if f.gotPath != "/v1/messages" {
		t.Fatalf("upstream path = %q, want /v1/messages", f.gotPath)
	}
}

func TestUnknownSentinelRejected(t *testing.T) {
	p, f := startProxy(t, "sk-real", nil)
	base := baseFrom(t, p.WorkerEnv("agent-1", "feat/x"))

	// A plausible-looking but never-registered token must not forward.
	resp := do(t, http.MethodPost, base+"/v1/messages", "argus-sentinel-deadbeef")
	if resp != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp)
	}
	if f.requests != 0 {
		t.Fatalf("upstream received %d requests, want 0 — a bad sentinel reached the real API", f.requests)
	}
}

func TestMissingCredentialRejected(t *testing.T) {
	p, f := startProxy(t, "sk-real", nil)
	base := baseFrom(t, p.WorkerEnv("agent-1", "feat/x"))

	resp := do(t, http.MethodGet, base+"/v1/models", "")
	if resp != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp)
	}
	if f.requests != 0 {
		t.Fatal("a request with no credential reached the upstream")
	}
}

func TestUnregisteredRouteHasNoUpstream(t *testing.T) {
	p, _ := startProxy(t, "sk-real", nil)
	env := p.WorkerEnv("agent-1", "feat/x")
	base := baseFrom(t, env)
	// The proxy address with a route that was never registered: no credential
	// exists for it and no handler serves it, so egress is impossible.
	root := strings.TrimSuffix(base, "/anthropic")

	resp := do(t, http.MethodGet, root+"/codeberg/api/v1/user", sentinelFrom(t, env))
	if resp == http.StatusOK {
		t.Fatal("an unregistered route was reachable")
	}
}

func TestEachWorkerGetsDistinctSentinel(t *testing.T) {
	p, _ := startProxy(t, "sk-real", nil)
	a := sentinelFrom(t, p.WorkerEnv("agent-1", "feat/a"))
	b := sentinelFrom(t, p.WorkerEnv("agent-2", "feat/b"))
	if a == b {
		t.Fatal("two workers were issued the same sentinel; one cannot be revoked alone")
	}
}

func TestLogRecordsAgentNotSecret(t *testing.T) {
	const realKey = "sk-real-do-not-leak"
	var lines []string
	log := func(agent, method, path string) {
		lines = append(lines, agent+" "+method+" "+path)
	}
	p, _ := startProxy(t, realKey, log)
	env := p.WorkerEnv("agent-7", "feat/x")
	sentinel := sentinelFrom(t, env)
	do(t, http.MethodPost, baseFrom(t, env)+"/v1/messages", sentinel)

	if len(lines) == 0 {
		t.Fatal("proxied call was not logged")
	}
	got := strings.Join(lines, "\n")
	if !strings.Contains(got, "agent-7") {
		t.Fatalf("log %q missing agent label", got)
	}
	if strings.Contains(got, realKey) || strings.Contains(got, sentinel) {
		t.Fatalf("log leaked a credential: %q", got)
	}
}

func TestRevokeCutsOffOneWorkerOnly(t *testing.T) {
	p, f := startProxy(t, "sk-real", nil)
	victim := p.WorkerEnv("agent-doomed", "feat/a")
	keep := p.WorkerEnv("agent-safe", "feat/b")

	// Both work before revocation.
	if got := do(t, http.MethodGet, baseFrom(t, victim)+"/v1/models", sentinelFrom(t, victim)); got != http.StatusOK {
		t.Fatalf("victim pre-revoke status = %d, want 200", got)
	}

	if n := p.Revoke("agent-doomed"); n != 1 {
		t.Fatalf("Revoke dropped %d sentinels, want 1", n)
	}
	before := f.requests

	// The revoked worker is now refused and never reaches the upstream...
	if got := do(t, http.MethodGet, baseFrom(t, victim)+"/v1/models", sentinelFrom(t, victim)); got != http.StatusForbidden {
		t.Fatalf("revoked worker status = %d, want 403", got)
	}
	if f.requests != before {
		t.Fatal("revoked worker's call still reached the upstream")
	}
	// ...while the untouched worker keeps working.
	if got := do(t, http.MethodGet, baseFrom(t, keep)+"/v1/models", sentinelFrom(t, keep)); got != http.StatusOK {
		t.Fatalf("bystander worker status = %d after revoking a different worker, want 200", got)
	}
	// Revoking an unknown agent changes nothing.
	if n := p.Revoke("never-registered"); n != 0 {
		t.Fatalf("Revoke of unknown agent dropped %d, want 0", n)
	}
}

// TestConcurrentRegisterAndGate exercises WorkerEnv (write) against live gate
// reads so `go test -race` has something real to inspect on the sessions map,
// rather than the strictly-sequential access the other tests make.
func TestConcurrentRegisterAndGate(t *testing.T) {
	p, _ := startProxy(t, "sk-real", nil)
	base := baseFrom(t, p.WorkerEnv("agent-0", "feat/0"))

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(2)
		go func(i int) { defer wg.Done(); p.WorkerEnv(fmt.Sprintf("agent-%d", i), "feat/x") }(i)
		go func() { defer wg.Done(); do(t, http.MethodGet, base+"/v1/models", "argus-sentinel-nope") }()
	}
	wg.Wait()
}

// do issues a request carrying token as x-api-key and returns the status code.
func do(t *testing.T, method, rawurl, token string) int {
	t.Helper()
	req, err := http.NewRequest(method, rawurl, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("x-api-key", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp == nil {
		t.Fatal("Do returned a nil response")
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
