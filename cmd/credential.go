package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/config"
	"github.com/Elysium-Labs-EU/argus/internal/credential"
	"github.com/Elysium-Labs-EU/argus/internal/credproxy"
	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
)

// credentialEnvFlagHelp is shared by every command that registers
// --credential-env, so the flag documents itself identically everywhere it
// appears.
// The value name/type placeholder pflag renders in --help comes from the
// first backtick-quoted span it finds, so this string deliberately contains
// none — a literal 'name=ENVVAR' in backticks here would replace the
// "stringToString" placeholder pflag would otherwise show.
const credentialEnvFlagHelp = "override which env var carries a credential, name=ENVVAR (e.g. anthropic=MY_CLAUDE_KEY, github.com=MY_GH_TOKEN); repeatable. Takes priority over both argus's built-in defaults and 'argus config set credential.<name>'."

// resolveCredentialOverrides merges cli (this invocation's --credential-env
// flags) over the persisted ~/.argus/config.toml credential map (see
// internal/config and `argus config set`), per the CLI-beats-config priority
// order. cli may be nil.
func resolveCredentialOverrides(cli map[string]string) (map[string]string, error) {
	path, err := config.Path()
	if err != nil {
		return nil, fmt.Errorf("resolving argus config path: %w", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, fmt.Errorf("loading %s: %w", path, err)
	}
	return credential.Merge(cli, cfg.Credential), nil
}

// startCredentialProxy fronts every credproxy.Registry() agent-key shape
// whose credential currently resolves to a value (checking overrides before
// argus's built-in env var names, via internal/credential) — the
// generalization of the old Anthropic-only wiring: which agent(s) get
// fronted now follows from which keys are actually present, not from one
// hardcoded branch. It returns a nil proxy, a no-op cleanup, and no error
// when no known agent key resolves, matching the existing "off when there's
// nothing to front" behavior. extraScrub lists the override env var names
// that supplied a fronted upstream's real key — those are argus's own lookup
// convenience, never a name the worker's own launcher reads, and must be
// added to the worker's env scrub list so the real secret isn't inherited
// alongside the sentinel (see credential.ScrubVars).
func startCredentialProxy(logger *eventlog.Logger, overrides map[string]string) (proxy *credproxy.Proxy, extraScrub []string, cleanup func(), err error) {
	cleanup = func() {}

	var ups []*credproxy.Upstream
	for _, spec := range credproxy.Registry() {
		vars := credential.EnvVars(spec.Name, overrides, []string{spec.KeyVar})
		key := credential.Lookup(vars)
		if key == "" {
			continue
		}
		ups = append(ups, credproxy.FromSpec(spec, key))
		if v := overrides[spec.Name]; v != "" {
			extraScrub = append(extraScrub, v)
		}
	}
	if len(ups) == 0 {
		return nil, nil, cleanup, nil
	}

	proxy = credproxy.New(func(agent, method, path string) {
		logger.Action("credproxy", agent, method, path)
	}, ups...)
	if serr := proxy.Start(); serr != nil {
		return nil, nil, cleanup, fmt.Errorf("starting credential proxy: %w", serr)
	}
	cleanup = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = proxy.Shutdown(ctx)
	}
	return proxy, extraScrub, cleanup, nil
}
