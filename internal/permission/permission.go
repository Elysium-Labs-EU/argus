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
	"strings"
)

// DefaultAllowEntry is the broadest entry that satisfies the check: every
// argus subcommand. A caller wanting tighter, per-subcommand scoping (e.g.
// "Bash(argus ship *)") can pass that instead — Check and Ensure both accept
// any entry shaped like coverageRe.
const DefaultAllowEntry = "Bash(argus *)"

// denyTargets are the raw herdr subcommands that bypass argus's own
// delivery-confirmed dispatch (deliverPaneMessage, behind `argus worker
// steer`/`answer`): herdr accepts the text and returns immediately whether or
// not a live agent turn ever picks it up, so a stalled prompt and a
// delivered one look identical from the caller's side. Read-only pane
// commands (list/read/get) are how a supervising session checks on things,
// not a coordination risk, and are deliberately excluded.
var denyTargets = []string{"pane send-text", "pane send-keys", "pane run"}

// DefaultDenyEntries is the permissions.deny entry CheckDeny/EnsureDeny add
// for each of denyTargets, derived from the single denyTargets list so the
// two can't silently drift apart.
func DefaultDenyEntries() []string {
	entries := make([]string, len(denyTargets))
	for i, t := range denyTargets {
		entries[i] = "Bash(herdr " + t + ":*)"
	}
	return entries
}

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

// shipForceTarget is the literal command CoversShipForce checks entry
// against — an entry covers it either by exact match or, if wildcarded, by
// prefix-matching it at a word boundary.
const shipForceTarget = "argus ship --force"

// CoversShipForce reports whether entry authorizes `argus ship --force` to
// run without the calling agent's own approval prompt: an exact-literal
// match, the blanket wildcard, or one scoped to "ship" (with a trailing
// wildcard/colon-glob, spaced or not before the "*"). An entry with no
// wildcard that isn't the exact literal command only ever matches that
// exact, argument-less command line, so it can never reach a call carrying
// "--force" — Bash allow-glob syntax has no way to match "argus ship
// <safe-flags>" while excluding one specific flag, so any wildcard scoped to
// ship (or broader) is unavoidably ship --force too.
func CoversShipForce(entry string) bool {
	prefix, wildcarded, ok := entryPrefix(entry)
	if !ok {
		return false
	}
	if prefix == shipForceTarget {
		return true
	}
	return wildcarded && strings.HasPrefix(shipForceTarget, prefix+" ")
}

// entryPrefix returns the literal command prefix a Bash(...) permission
// entry matches against, stripping the trailing wildcard ("*" or ":*") the
// glob syntax allows, and whether that wildcard was actually present.
// wildcarded matters to callers: Claude Code matches a wildcard-less
// "Bash(x)" rule only against the exact argument-less command "x", never
// against "x <anything>" — so a bare entry is not a license to treat it as
// covering every longer command sharing its prefix. ok is false if entry
// isn't Bash(...)-shaped at all.
func entryPrefix(entry string) (prefix string, wildcarded bool, ok bool) {
	inner, ok := strings.CutPrefix(entry, "Bash(")
	if !ok {
		return "", false, false
	}
	inner, ok = strings.CutSuffix(inner, ")")
	if !ok {
		return "", false, false
	}
	trimmed := strings.TrimSuffix(inner, ":*")
	if trimmed == inner {
		trimmed = strings.TrimSuffix(inner, "*")
	}
	wildcarded = trimmed != inner
	return strings.TrimRight(trimmed, " "), wildcarded, true
}

// denyEntryCovers reports whether entry (one string from permissions.deny)
// already denies the raw herdr invocation "herdr <target>" — either that
// exact subcommand (with its own wildcard, or bare), or a strictly broader
// prefix of it, e.g. "Bash(herdr pane *)" or "Bash(herdr *)" both already
// deny "herdr pane send-text". A strictly broader prefix only covers when it
// was wildcarded — a bare "Bash(herdr)" matches solely the exact command
// "herdr", not "herdr pane run". Word-boundary aware, same as coverageRe, so
// "herdr pane send" doesn't false-positive against "herdr pane send-text".
func denyEntryCovers(entry, target string) bool {
	prefix, wildcarded, ok := entryPrefix(entry)
	if !ok {
		return false
	}
	want := "herdr " + target
	if want == prefix {
		return true
	}
	return wildcarded && strings.HasPrefix(want, prefix+" ")
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

// permList returns the string list at permissions.<key> (e.g. "allow" or
// "deny"), along with the raw permissions block so a caller can rewrite it
// without dropping sibling keys. A missing block or key returns a nil list,
// not an empty one, so callers can't assume either is already initialized.
func permList(raw rawSettings, key string) ([]string, map[string]json.RawMessage, error) {
	permsRaw, ok := raw["permissions"]
	if !ok {
		return nil, map[string]json.RawMessage{}, nil
	}
	var perms map[string]json.RawMessage
	if err := json.Unmarshal(permsRaw, &perms); err != nil {
		return nil, nil, fmt.Errorf("parsing permissions block: %w", err)
	}
	listRaw, ok := perms[key]
	if !ok {
		return nil, perms, nil
	}
	var list []string
	if err := json.Unmarshal(listRaw, &list); err != nil {
		return nil, nil, fmt.Errorf("parsing permissions.%s: %w", key, err)
	}
	return list, perms, nil
}

func allowList(raw rawSettings) ([]string, map[string]json.RawMessage, error) {
	return permList(raw, "allow")
}

func denyList(raw rawSettings) ([]string, map[string]json.RawMessage, error) {
	return permList(raw, "deny")
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

	raw, err = withEntries(raw, perms, "allow", allow, entry)
	if err != nil {
		return false, err
	}
	if err := writeSettingsFile(path, raw); err != nil {
		return false, err
	}
	return true, nil
}

// CheckDeny reports, for each of DefaultDenyEntries's targets, whether the
// settings file at path already denies it (exactly or via a broader deny
// entry), mirroring Check's allow-side report. missing lists the raw herdr
// pane-mutation entries (in DefaultDenyEntries's own form) not yet covered —
// empty when every target is already denied. A missing settings file is not
// an error — nothing in it is denied yet.
func CheckDeny(path string) (missing []string, err error) {
	raw, err := load(path)
	if err != nil {
		return nil, err
	}
	deny, _, err := denyList(raw)
	if err != nil {
		return nil, err
	}
	for i, target := range denyTargets {
		if !slices.ContainsFunc(deny, func(entry string) bool { return denyEntryCovers(entry, target) }) {
			missing = append(missing, DefaultDenyEntries()[i])
		}
	}
	return missing, nil
}

// EnsureDeny adds whichever of DefaultDenyEntries aren't yet covered (exactly
// or by a broader existing deny entry) to permissions.deny at path, creating
// the file/directory if necessary and leaving every other key untouched.
// added lists exactly the entries this call wrote — empty when every target
// was already covered, matching Ensure's idempotency contract.
func EnsureDeny(path string) (added []string, err error) {
	raw, err := load(path)
	if err != nil {
		return nil, err
	}
	deny, perms, err := denyList(raw)
	if err != nil {
		return nil, err
	}

	for i, target := range denyTargets {
		if !slices.ContainsFunc(deny, func(entry string) bool { return denyEntryCovers(entry, target) }) {
			added = append(added, DefaultDenyEntries()[i])
		}
	}
	if len(added) == 0 {
		return nil, nil
	}

	raw, err = withEntries(raw, perms, "deny", deny, added...)
	if err != nil {
		return nil, err
	}
	if err := writeSettingsFile(path, raw); err != nil {
		return nil, err
	}
	return added, nil
}

// withEntries returns raw with entries appended to permissions.<key>,
// filling in the permissions block (and raw itself) when either was absent —
// load/permList return nil maps for a missing file or missing block rather
// than empty ones, so callers can't assume they're already initialized.
func withEntries(raw rawSettings, perms map[string]json.RawMessage, key string, list []string, entries ...string) (rawSettings, error) {
	list = append(list, entries...)
	listData, err := json.Marshal(list)
	if err != nil {
		return nil, fmt.Errorf("encoding %s list: %w", key, err)
	}
	if perms == nil {
		perms = map[string]json.RawMessage{}
	}
	perms[key] = listData
	permsData, err := json.Marshal(perms)
	if err != nil {
		return nil, fmt.Errorf("encoding permissions block: %w", err)
	}
	if raw == nil {
		raw = rawSettings{}
	}
	raw["permissions"] = permsData
	return raw, nil
}

// writeSettingsFile marshals raw and swaps it into place via a same-directory
// rename, so a reader never observes a partially written settings.json.
func writeSettingsFile(path string, raw rawSettings) error {
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding settings: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // project-local .claude dir, standard perms
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil { //nolint:gosec // local settings file, not a secret
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("renaming settings into place: %w", err)
	}
	return nil
}
