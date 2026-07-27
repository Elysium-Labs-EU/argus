// Package credential is the shared resolution mechanism forge token lookup
// (internal/forge) and the credential proxy's agent-key wiring (internal/credproxy,
// cmd/supervise.go) both build on: given a credential name — a forge host like
// "github.com", or an agent-key name like "anthropic" — resolve which
// environment variable actually carries it, checking an operator override
// before argus's own built-in default names. argus's job is the resolution
// mechanism, not an opinion about which env var name an operator happens to
// use for a given service.
package credential

import "os"

// EnvVars returns the ordered candidate environment variable names to check
// for credential name: overrides[name] first, if the operator named one via
// --credential-env or a persisted `argus config set credential.<name>` (see
// Merge) — the explicit, highest-priority source — followed by defaults,
// argus's built-in list of env vars for name (e.g. GITHUB_TOKEN, GH_TOKEN).
func EnvVars(name string, overrides map[string]string, defaults []string) []string {
	var out []string
	if v := overrides[name]; v != "" {
		out = append(out, v)
	}
	return append(out, defaults...)
}

// Lookup returns the value of the first set environment variable in vars, in
// order, or "" if none of them are set.
func Lookup(vars []string) string {
	for _, v := range vars {
		if val := os.Getenv(v); val != "" {
			return val
		}
	}
	return ""
}

// Merge combines two credential-name -> env-var-name override maps, with cli
// taking precedence over persisted: the CLI flag is the more explicit,
// one-off source, the persisted config file the operator's standing default.
// Either argument may be nil.
func Merge(cli, persisted map[string]string) map[string]string {
	out := make(map[string]string, len(cli)+len(persisted))
	for k, v := range persisted {
		if v != "" {
			out[k] = v
		}
	}
	for k, v := range cli {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// ScrubVars returns the deduplicated set of env var names named as overrides.
// An override value is, by construction, a var name argus itself resolves for
// its own use (seeding a credential proxy, or authenticating to a forge on the
// operator's behalf) — never one a worker's own launcher tool reads under that
// custom name, since the tool still expects the standard var name (e.g.
// ANTHROPIC_API_KEY). It therefore belongs in a worker's env scrub list
// alongside argus's built-in credential var names (see forge.StandardTokenVars),
// so the real secret an override points at is never handed to the worker.
func ScrubVars(overrides map[string]string) []string {
	seen := make(map[string]bool, len(overrides))
	var out []string
	for _, v := range overrides {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
