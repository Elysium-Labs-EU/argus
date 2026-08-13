// Package config manages argus's small persisted operator config file,
// ~/.argus/config.toml. Today it holds exactly one thing: credential-name ->
// env-var-name overrides (see internal/credential), written only by `argus
// config set credential.<name> <env-var>`, never hand-authored — the
// discoverability problem a hand-edited schema would raise ("how would the
// operator know the shape?") goes away when the only writer is a command that
// validates as it goes.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// pathEnvVar overrides the default ~/.argus/config.toml location. It exists so
// tests can point at a throwaway file instead of the developer's real
// ~/.argus, matching the pattern internal/jira's configPathEnvVar already
// uses for the same reason.
const pathEnvVar = "ARGUS_CONFIG_FILE"

// Config is the in-memory shape of ~/.argus/config.toml.
type Config struct {
	// Credential maps a credential name (a forge host like "github.com", or an
	// agent-key name like "anthropic") to the environment variable argus should
	// read it from instead of its own built-in default.
	Credential map[string]string
}

// Path resolves the config file location: $ARGUS_CONFIG_FILE if set, else
// ~/.argus/config.toml.
func Path() (string, error) {
	if p := os.Getenv(pathEnvVar); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".argus", "config.toml"), nil
}

// Load reads and parses the config file at path. A missing file is not an
// error — it returns a zero Config, the same "nothing configured yet" state
// as an empty file — since the config is entirely optional: argus's built-in
// env-var default names (see internal/credential) already cover the case
// where an operator has set nothing.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is an operator-set env var or our own fixed ~/.argus/config.toml, not attacker input
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, err
	}
	return parseTOML(string(data))
}

// Save writes cfg to path as TOML, creating its parent directory if needed.
func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(encodeTOML(cfg)), 0o600); err != nil {
		return fmt.Errorf("writing config file: %w", err)
	}
	return nil
}

// Set applies a `argus config set <key> <value>` command to cfg in place. The
// only supported key namespace today is credential.<name>; other keys are
// rejected so a typo surfaces immediately instead of being silently ignored.
func (c *Config) Set(key, value string) error {
	name, ok := strings.CutPrefix(key, "credential.")
	if !ok || name == "" {
		return fmt.Errorf("unsupported config key %q (supported: credential.<name>, e.g. credential.github.com)", key)
	}
	if c.Credential == nil {
		c.Credential = make(map[string]string)
	}
	c.Credential[name] = value
	return nil
}

// Get resolves a `argus config get <key>` command against cfg. It parses key
// the same way Set does, so get/set stay symmetric over the same
// credential.<name> namespace. found is false both for an unsupported key
// namespace and for a supported key with no persisted value.
func (c *Config) Get(key string) (value string, found bool, err error) {
	name, ok := strings.CutPrefix(key, "credential.")
	if !ok || name == "" {
		return "", false, fmt.Errorf("unsupported config key %q (supported: credential.<name>, e.g. credential.github.com)", key)
	}
	value, found = c.Credential[name]
	return value, found, nil
}
