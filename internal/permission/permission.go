// Package permission checks and repairs the one thing the argus skill
// (skills/argus/SKILL.md) cannot enforce itself: whether the calling agent's
// own Bash-tool permission settings allowlist argus invocations. The gate,
// verdict-required ship, etc. are argus's own safety layer; the outer
// harness's per-command approval prompt is a separate layer argus can't
// reach into. Without an allow entry in the adopting repo's
// .claude/settings.json, every `argus supervise`/`ship`/`review`/... call
// prompts for manual approval, defeating the point of using argus for the
// mechanical half of supervision.
package permission

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
)

// DefaultAllowEntry is the broadest entry that satisfies the check: every
// argus subcommand. A caller wanting tighter, per-subcommand scoping (e.g.
// "Bash(argus ship *)") can pass that instead — Check and Ensure both accept
// any entry shaped like coverageRe.
const DefaultAllowEntry = "Bash(argus *)"

// coverageRe matches a Bash allow entry that covers argus invocations: the
// bare command ("Bash(argus)"), the wildcard ("Bash(argus *)"), or a
// subcommand-scoped entry ("Bash(argus ship *)"). It deliberately requires a
// word boundary after "argus" (a space or the closing paren) so a
// same-prefixed binary name like "argustest" doesn't false-positive.
var coverageRe = regexp.MustCompile(`^Bash\(argus(\s.*)?\)$`)

// Covers reports whether entry (one string from permissions.allow) already
// authorizes argus Bash invocations.
func Covers(entry string) bool {
	return coverageRe.MatchString(entry)
}

// shipForceRe matches an allow entry broad enough to also authorize `argus
// ship --force` without a prompt: the blanket wildcard, or one scoped to
// "ship" with a trailing wildcard/colon-glob. An entry with no wildcard
// ("Bash(argus)", "Bash(argus ship)") only ever matches that exact,
// argument-less command line, so it can never reach a call carrying
// "--force" — Bash allow-glob syntax has no way to match "argus ship
// <safe-flags>" while excluding one specific flag, so any wildcard scoped to
// ship (or broader) is unavoidably ship --force too.
var shipForceRe = regexp.MustCompile(`^Bash\(argus\s+(\*|ship(:\*|\s+.*\*))\)$`)

// CoversShipForce reports whether entry authorizes `argus ship --force` to
// run without the calling agent's own approval prompt.
func CoversShipForce(entry string) bool {
	return shipForceRe.MatchString(entry)
}

// SettingsPath is where Claude Code reads a project's committed permission
// settings, relative to repo.
func SettingsPath(repo string) string {
	return filepath.Join(repo, ".claude", "settings.json")
}

// rawSettings preserves every top-level key of settings.json we don't
// otherwise understand, so a rewrite never drops fields like "hooks" or
// "model" that argus has no business touching.
type rawSettings map[string]json.RawMessage

func load(path string) (rawSettings, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is an operator-given --repo/settings path, not attacker input
	if errors.Is(err, fs.ErrNotExist) {
		return rawSettings{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var raw rawSettings
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return raw, nil
}

func allowList(raw rawSettings) ([]string, map[string]json.RawMessage, error) {
	permsRaw, ok := raw["permissions"]
	if !ok {
		return nil, map[string]json.RawMessage{}, nil
	}
	var perms map[string]json.RawMessage
	if err := json.Unmarshal(permsRaw, &perms); err != nil {
		return nil, nil, fmt.Errorf("parsing permissions block: %w", err)
	}
	allowRaw, ok := perms["allow"]
	if !ok {
		return nil, perms, nil
	}
	var allow []string
	if err := json.Unmarshal(allowRaw, &allow); err != nil {
		return nil, nil, fmt.Errorf("parsing permissions.allow: %w", err)
	}
	return allow, perms, nil
}

// Check reports whether the settings file at path already has a
// permissions.allow entry covering argus, and returns every matching entry
// found (so a caller can tell the operator exactly what already covers it).
// A missing settings file is not an error — it just isn't covered yet.
func Check(path string) (covered bool, matches []string, err error) {
	raw, err := load(path)
	if err != nil {
		return false, nil, err
	}
	allow, _, err := allowList(raw)
	if err != nil {
		return false, nil, err
	}
	for _, entry := range allow {
		if Covers(entry) {
			matches = append(matches, entry)
		}
	}
	return len(matches) > 0, matches, nil
}

// Ensure adds entry to permissions.allow at path if no existing entry
// already covers argus, creating the file (and its .claude directory) if
// necessary. It preserves every other key in the file untouched. added is
// false when an existing entry (the requested one or a broader one) already
// covers argus — Ensure is idempotent and never writes in that case.
func Ensure(path, entry string) (added bool, err error) {
	raw, err := load(path)
	if err != nil {
		return false, err
	}
	allow, perms, err := allowList(raw)
	if err != nil {
		return false, err
	}
	if slices.ContainsFunc(allow, Covers) {
		return false, nil
	}

	allow = append(allow, entry)
	allowData, err := json.Marshal(allow)
	if err != nil {
		return false, fmt.Errorf("encoding allow list: %w", err)
	}
	if perms == nil {
		perms = map[string]json.RawMessage{}
	}
	perms["allow"] = allowData
	permsData, err := json.Marshal(perms)
	if err != nil {
		return false, fmt.Errorf("encoding permissions block: %w", err)
	}
	if raw == nil {
		raw = rawSettings{}
	}
	raw["permissions"] = permsData

	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return false, fmt.Errorf("encoding settings: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // project-local .claude dir, standard perms
		return false, fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil { //nolint:gosec // local settings file, not a secret
		return false, fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return false, fmt.Errorf("renaming settings into place: %w", err)
	}
	return true, nil
}
