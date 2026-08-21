// Package credproxy keeps the real API key out of a worker agent's own hands.
// Instead of handing each worker the real ANTHROPIC_API_KEY, argus runs a
// loopback reverse proxy that holds the secret and hands each worker a unique
// throwaway sentinel. The worker points its API base URL at the proxy and
// authenticates with the sentinel; the proxy validates the sentinel, swaps in
// the real credential on the way out, and forwards to the real upstream over
// TLS. The launcher process argus starts, and every child it spawns, is given
// the sentinel in place of the key.
//
// What this buys, enforced by construction rather than by asking the agent to
// behave:
//
//   - No accidental key handling: the real key is never placed in the worker
//     process's environment, argv, or config, so it cannot end up in the
//     agent's logs, its transcript, a child process it spawns, or an
//     exfiltration of "my environment". The worker authenticates with a value
//     that is worthless anywhere else.
//   - Credential scope: the only real credential that exists is for the
//     registered upstream(s), and every forwarded request is host-pinned to
//     that upstream. A worker cannot turn the key against any other host.
//   - Attribution and revocation: each worker gets a distinct, unguessable
//     sentinel, so every call is attributable to one agent and that agent's
//     access can be revoked (Revoke) without disturbing the others.
//
// What this is NOT. This is a credential boundary, not a sandbox. The worker
// still runs as the same OS user, on the same filesystem, with unrestricted
// outbound network. A hostile agent that goes looking can still obtain the real
// key by other means — reading an ancestor process's environment (e.g.
// /proc/<ppid>/environ on Linux), the user's shell rc files, a project .env, or
// ~/.claude credentials — and can reach any host with its own network access.
// Closing those requires real isolation (a container or namespace with a
// scrubbed environment and no read access to the host's secrets); this proxy is
// the credential half of that story, not a substitute for it.
//
// The proxy is a reverse proxy per upstream (worker speaks plain HTTP to
// localhost, the proxy speaks TLS to the real host), deliberately not a
// forwarding MITM proxy: there is no certificate injection and nothing decrypts
// the worker's own TLS.
package credproxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

// LogFunc records one proxied call for the run log. It is handed the worker's
// agent label (never the sentinel or the real key), the HTTP method, and the
// request path, so the operator gets a tamper-evident trail of what each agent
// reached for without any secret touching the log.
type LogFunc func(agent, method, path string)

// Upstream is one real service the proxy fronts. Name is the first path segment
// workers address it by (e.g. "anthropic" → the proxy serves "/anthropic/").
// Target is the real host. inject rewrites an outbound request to carry the real
// credential in place of the sentinel. env reports the environment variables a
// worker must set to route this upstream's traffic through the proxy.
type Upstream struct {
	inject func(r *http.Request)
	env    func(sentinel, baseURL string) []string
	Target *url.URL
	Name   string
}

// KeySpec describes one known agent-API-key shape credproxy can front: which
// upstream it targets, which env vars a worker/launcher expects the key and
// base URL under, and how the real key is injected into an outbound request.
// Registry lists the shapes credproxy knows out of the box; a caller picks
// which env var actually carries a given spec's key via the same resolution
// mechanism forge token lookup uses (see internal/credential and cmd
// --credential-env), not by editing this list — the hardcoding this
// generalizes was ever having exactly one shape (Anthropic) wired into the
// call site at all.
type KeySpec struct {
	Inject     func(r *http.Request, realKey string)
	Name       string
	Target     string
	KeyVar     string
	BaseURLVar string
}

// Registry lists the agent-key shapes credproxy fronts out of the box. Order
// is fixed (not map iteration) so callers that front every resolvable spec
// get deterministic behavior across runs.
func Registry() []KeySpec {
	return []KeySpec{
		{
			Name:       "anthropic",
			Target:     "https://api.anthropic.com",
			KeyVar:     "ANTHROPIC_API_KEY",
			BaseURLVar: "ANTHROPIC_BASE_URL",
			Inject: func(r *http.Request, realKey string) {
				r.Header.Del("Authorization")
				r.Header.Set("x-api-key", realKey)
				if r.Header.Get("anthropic-version") == "" {
					r.Header.Set("anthropic-version", "2023-06-01")
				}
			},
		},
		{
			Name:       "openai",
			Target:     "https://api.openai.com",
			KeyVar:     "OPENAI_API_KEY",
			BaseURLVar: "OPENAI_BASE_URL",
			Inject: func(r *http.Request, realKey string) {
				r.Header.Set("Authorization", "Bearer "+realKey)
			},
		},
	}
}

// FromSpec builds an Upstream fronting spec's target with realKey: workers get
// the sentinel under spec.KeyVar and their base URL (spec.BaseURLVar) pointed
// at the proxy; spec.Inject swaps the sentinel for realKey on the way out.
func FromSpec(spec KeySpec, realKey string) *Upstream {
	target, _ := url.Parse(spec.Target)
	return &Upstream{
		Name:   spec.Name,
		Target: target,
		inject: func(r *http.Request) { spec.Inject(r, realKey) },
		env: func(sentinel, baseURL string) []string {
			return []string{
				spec.BaseURLVar + "=" + baseURL + "/" + spec.Name,
				spec.KeyVar + "=" + sentinel,
			}
		},
	}
}

// Anthropic fronts the Anthropic API with realKey. Workers get the sentinel in
// ANTHROPIC_API_KEY and their base URL pointed at the proxy; the proxy replaces
// the sentinel with realKey in the x-api-key header (and supplies the required
// anthropic-version when the caller omitted it) before forwarding. It is the
// Registry's "anthropic" spec applied via FromSpec, kept as a named
// constructor since it predates Registry and existing callers/tests still
// build a proxy directly from it.
func Anthropic(realKey string) *Upstream {
	for _, spec := range Registry() {
		if spec.Name == "anthropic" {
			return FromSpec(spec, realKey)
		}
	}
	panic("credproxy: anthropic spec missing from Registry")
}

// session is the worker behind one sentinel: agent is its log label, branch is
// the worktree branch it owns (reserved for per-agent scope enforcement on
// write-capable upstreams such as a forge).
type session struct {
	agent  string
	branch string
}

// Proxy is the running credential broker: a set of fronted upstreams, a registry
// mapping each live sentinel to its worker, and the HTTP server that serves them
// on loopback.
type Proxy struct {
	log       LogFunc
	srv       *http.Server
	upstreams map[string]*Upstream
	sessions  map[string]session
	baseURL   string
	mu        sync.RWMutex
}

// New builds a proxy fronting ups. log may be nil (calls go unlogged). The proxy
// is not listening until Start is called.
func New(log LogFunc, ups ...*Upstream) *Proxy {
	m := make(map[string]*Upstream, len(ups))
	for _, u := range ups {
		m[u.Name] = u
	}
	return &Proxy{
		log:       log,
		upstreams: m,
		sessions:  make(map[string]session),
	}
}

// Start binds a loopback listener on an OS-chosen port and serves the fronted
// upstreams in the background. Binding to 127.0.0.1 (never 0.0.0.0) keeps the
// broker off the network: only processes on this host can reach it, and even
// they must present a registered sentinel. Start returns once the socket is open
// so callers can spawn workers immediately.
func (p *Proxy) Start() error {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("credproxy: binding loopback listener: %w", err)
	}
	p.baseURL = "http://" + ln.Addr().String()

	mux := http.NewServeMux()
	for name, u := range p.upstreams {
		mux.Handle("/"+name+"/", p.handlerFor(u))
	}
	p.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = p.srv.Serve(ln) }()
	return nil
}

// Shutdown stops serving and drains in-flight requests. Sentinels do not survive
// it: a stopped proxy authorizes nothing.
func (p *Proxy) Shutdown(ctx context.Context) error {
	if p.srv == nil {
		return nil
	}
	return p.srv.Shutdown(ctx)
}

// WorkerEnv registers a fresh sentinel for the worker identified by agent on
// branch and returns the environment assignments that route the worker's API
// traffic through the proxy. Each call mints a distinct sentinel, so no two
// workers share credentials and any one can be revoked alone. It must be called
// after Start (the returned env embeds the proxy's chosen address).
func (p *Proxy) WorkerEnv(agent, branch string) []string {
	s := newSentinel()
	p.mu.Lock()
	p.sessions[s] = session{agent: agent, branch: branch}
	p.mu.Unlock()

	var env []string
	for _, u := range p.upstreams {
		env = append(env, u.env(s, p.baseURL)...)
	}
	return env
}

// Revoke invalidates every sentinel issued to agent, so its later calls are
// refused with 403 while all other workers keep working. It returns how many
// sentinels it dropped. This is the per-worker cutoff the proxy promises: a
// supervisor can call it the moment a worker reaches a terminal phase (or
// misbehaves) instead of waiting to tear the whole proxy down. Revoking an
// unknown agent is a no-op.
func (p *Proxy) Revoke(agent string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for s, sess := range p.sessions {
		if sess.agent == agent {
			delete(p.sessions, s)
			n++
		}
	}
	return n
}

// handlerFor gates an upstream behind sentinel validation, then reverse-proxies
// cleared requests to the real host with the real credential injected.
func (p *Proxy) handlerFor(u *Upstream) http.Handler {
	prefix := "/" + u.Name
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = u.Target.Scheme
			pr.Out.URL.Host = u.Target.Host
			pr.Out.Host = u.Target.Host
			// path.Clean collapses any ".." the caller's decoded path carries (e.g.
			// GET /anthropic/%2e%2e/secret) before it is forwarded, so a traversal
			// segment can never survive into the request sent upstream.
			pr.Out.URL.Path = path.Clean(singleJoiningSlash(u.Target.Path, strings.TrimPrefix(pr.In.URL.Path, prefix)))
			u.inject(pr.Out)
		},
	}
	return p.gate(u, rp)
}

// gate is the trust boundary. It reads the sentinel the worker presented, refuses
// the call outright if it is not a live sentinel (so a leaked or guessed value is
// worthless), logs the attributable call, and only then lets the reverse proxy
// swap in the real credential and forward. The sentinel never reaches the
// upstream: inject overwrites the auth header on the way out.
func (p *Proxy) gate(u *Upstream, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sentinel := presentedSentinel(r)
		p.mu.RLock()
		sess, ok := p.sessions[sentinel]
		p.mu.RUnlock()
		if !ok {
			http.Error(w, "credproxy: unrecognized credential", http.StatusForbidden)
			if p.log != nil {
				p.log("unknown", r.Method, u.Name+" "+r.URL.Path)
			}
			return
		}
		if p.log != nil {
			p.log(sess.agent, r.Method, u.Name+" "+r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

// presentedSentinel pulls the caller-supplied token from wherever the client SDK
// put it — Anthropic uses x-api-key; a bearer scheme uses Authorization. The real
// key is never any of these values; a sentinel is.
func presentedSentinel(r *http.Request) string {
	if v := r.Header.Get("x-api-key"); v != "" {
		return v
	}
	if v := r.Header.Get("Authorization"); v != "" {
		return strings.TrimPrefix(v, "Bearer ")
	}
	return ""
}

// newSentinel returns an unguessable throwaway token. It carries no real secret;
// its only job is to be unique per worker and infeasible to forge.
func newSentinel() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return "argus-sentinel-" + hex.EncodeToString(b[:])
}

// singleJoiningSlash joins two URL path segments with exactly one slash between
// them, matching the standard library's reverse-proxy path handling.
func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash && b != "":
		return a + "/" + b
	}
	return a + b
}
