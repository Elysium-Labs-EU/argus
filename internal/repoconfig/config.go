// Package repoconfig reads a repo's own optional .argus/config.yml — the
// declarative contract a repo owner can write once instead of fighting
// argus's toolchain-neutral defaults with --allow/--base on every
// invocation (see argus issue #161). argus itself assigns no semantics to
// any value here beyond the three narrow places that read it back
// (agentadapter's base allow-list, supervise/rebase/ship's base-branch
// resolution, and the brief_note appended verbatim to a generated brief) —
// it never runs a build/test/lint command of its own, so there is nothing
// else for this schema to grow into without relocating the same
// toolchain-hardcoding problem from code into config.
package repoconfig

import (
	"fmt"
	"os"
	"path/filepath"
)

// Config is the in-memory shape of .argus/config.yml. All fields are
// optional; a missing file is equivalent to a zero Config.
type Config struct {
	BaseBranch string
	BriefNote  string
	Allow      []string
}

// pathEnvVar overrides the default <repo>/.argus/config.yml location for
// tests, mirroring internal/config's ARGUS_CONFIG_FILE.
const pathEnvVar = "ARGUS_REPO_CONFIG_FILE"

// Path resolves the config file location for repoRoot: $ARGUS_REPO_CONFIG_FILE
// if set, else <repoRoot>/.argus/config.yml.
func Path(repoRoot string) string {
	if p := os.Getenv(pathEnvVar); p != "" {
		return p
	}
	return filepath.Join(repoRoot, ".argus", "config.yml")
}

// Load reads and parses the config file at path. A missing file is not an
// error — it returns a zero Config, the same "nothing configured" state as
// an empty file, since every key is optional.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is derived from a repo root argus already trusts, or an operator-set test env var
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, err
	}
	cfg, err := parseYAML(string(data))
	if err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return cfg, nil
}

// Save writes cfg to path as YAML, creating its parent directory if needed.
func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // .argus/ dir inside a repo checkout, standard perms
		return fmt.Errorf("creating config directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(encodeYAML(cfg)), 0o644); err != nil { //nolint:gosec // repo-tracked config file, meant to be readable
		return fmt.Errorf("writing config file: %w", err)
	}
	return nil
}
