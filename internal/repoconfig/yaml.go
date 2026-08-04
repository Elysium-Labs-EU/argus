package repoconfig

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

// configSchemaHeader points editors with the yaml-language-server extension
// at schemas/config.schema.json, the same trick eos's initSchemaHeader uses
// for service.yaml — inline validation/autocomplete for free, no custom LSP.
const configSchemaHeader = "# yaml-language-server: $schema=https://raw.githubusercontent.com/Elysium-Labs-EU/argus/main/schemas/config.schema.json\n"

// deprecatedKeyAliases maps a superseded .argus/config.yml key to its
// current name. parseYAML still accepts every key here, assigning to the
// same field the current name would, and records the mapping on
// Config.Deprecated so a caller can warn an operator to migrate — argus is
// young enough that names are still being corrected, and support for an old
// name is expected to be temporary, not permanent API surface.
var deprecatedKeyAliases = map[string]string{
	"ship_lint":              "ship_verify_command",
	"verify_command":         "gate_verify_command",
	"worktree_setup_cmd":     "worktree_bootstrap_command",
	"worktree_setup_command": "worktree_bootstrap_command",
}

// encodeYAML renders cfg as the minimal YAML document parseYAML can read
// back: a leading comment, then any of the keys that are actually set, in
// field order. Like internal/config's TOML encoder, this is deliberately not
// a general-purpose YAML encoder — the schema is exactly the optional keys
// listed in Config's doc comment.
func encodeYAML(cfg *Config) string {
	var b strings.Builder
	b.WriteString(configSchemaHeader)
	b.WriteString("# .argus/config.yml — all keys are optional; see `argus init`.\n")
	if cfg.BaseBranch != "" {
		fmt.Fprintf(&b, "base_branch: %s\n", quoteYAML(cfg.BaseBranch))
	}
	if cfg.WorkerPlacement != "" {
		fmt.Fprintf(&b, "worker_placement: %s\n", quoteYAML(cfg.WorkerPlacement))
	}
	if cfg.Launcher != "" {
		fmt.Fprintf(&b, "launcher: %s\n", quoteYAML(cfg.Launcher))
	}
	if cfg.Forge != "" {
		fmt.Fprintf(&b, "forge: %s\n", quoteYAML(cfg.Forge))
	}
	if cfg.StatusPage != "" {
		fmt.Fprintf(&b, "status_page: %s\n", quoteYAML(cfg.StatusPage))
	}
	if cfg.WorktreeDir != "" {
		fmt.Fprintf(&b, "worktree_dir: %s\n", quoteYAML(cfg.WorktreeDir))
	}
	if cfg.ReviewEffort != "" {
		fmt.Fprintf(&b, "review_effort: %s\n", quoteYAML(cfg.ReviewEffort))
	}
	if len(cfg.Allow) > 0 {
		b.WriteString("allow:\n")
		for _, a := range cfg.Allow {
			fmt.Fprintf(&b, "  - %s\n", quoteYAML(a))
		}
	}
	if cfg.BriefNote != "" {
		fmt.Fprintf(&b, "brief_note: %s\n", quoteYAML(cfg.BriefNote))
	}
	if cfg.ReviewNote != "" {
		fmt.Fprintf(&b, "review_note: %s\n", quoteYAML(cfg.ReviewNote))
	}
	if cfg.ShipVerifyCommand != "" {
		fmt.Fprintf(&b, "ship_verify_command: %s\n", quoteYAML(cfg.ShipVerifyCommand))
	}
	if cfg.GateVerifyCommand != "" {
		fmt.Fprintf(&b, "gate_verify_command: %s\n", quoteYAML(cfg.GateVerifyCommand))
	}
	if cfg.WorktreeBootstrapCommand != "" {
		fmt.Fprintf(&b, "worktree_bootstrap_command: %s\n", quoteYAML(cfg.WorktreeBootstrapCommand))
	}
	if cfg.TitlePrefixTemplate != "" {
		fmt.Fprintf(&b, "title_prefix_template: %s\n", quoteYAML(cfg.TitlePrefixTemplate))
	}
	if cfg.OwnerStaleAfter != "" {
		fmt.Fprintf(&b, "owner_stale_after: %s\n", quoteYAML(cfg.OwnerStaleAfter))
	}
	if cfg.MaxDiffLines != nil {
		fmt.Fprintf(&b, "max_diff_lines: %d\n", *cfg.MaxDiffLines)
	}
	if cfg.ReworkBudget != nil {
		fmt.Fprintf(&b, "rework_budget: %d\n", *cfg.ReworkBudget)
	}
	writeYAMLList(&b, "proof_required_paths", cfg.ProofRequiredPaths)
	writeYAMLList(&b, "always_review_paths", cfg.AlwaysReviewPaths)
	writePhaseKeys(&b, cfg.Phases)
	return b.String()
}

// writePhaseKeys writes phase.<name>.skip/.deny for every configured phase,
// in protocol.ConfigurablePhases order — split out of encodeYAML to keep
// that function's own complexity down, not for reuse.
func writePhaseKeys(b *strings.Builder, phases protocol.PhaseConfig) {
	for _, p := range protocol.ConfigurablePhases {
		policy, ok := phases[p]
		if !ok {
			continue
		}
		if policy.Skip {
			fmt.Fprintf(b, "phase.%s.skip: true\n", p)
		}
		writeYAMLList(b, "phase."+string(p)+".deny", policy.Deny)
	}
}

// writeYAMLList writes a key's indented "- value" list block, or nothing if
// items is empty, matching the "allow:" block's own shape above.
func writeYAMLList(b *strings.Builder, key string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "%s:\n", key)
	for _, item := range items {
		fmt.Fprintf(b, "  - %s\n", quoteYAML(item))
	}
}

// quoteYAML double-quotes s using Go's own quoting rules. Go's backslash/
// quote escaping is a compatible subset of YAML's double-quoted scalar
// syntax, the same trick internal/config's TOML encoder relies on.
func quoteYAML(s string) string {
	return strconv.Quote(s)
}

// unquoteYAML strips and unescapes a double-quoted scalar, or returns s
// unchanged if it is a bare (unquoted) token — parseYAML's encodeYAML-written
// input is always quoted, but a hand-edited file may leave a value bare.
func unquoteYAML(s string) (string, error) {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strconv.Unquote(s)
	}
	return s, nil
}

// phaseKeyPrefix precedes every per-phase policy key: phase.<name>.<subkey>.
const phaseKeyPrefix = "phase."

// parsePhaseKey splits a phase.<name>.<subkey> config key into its phase and
// subkey parts. ok is false for any key not shaped like phase.<name>.<subkey>
// — such a key falls through to listFieldFor/assignScalarField as usual.
func parsePhaseKey(key string) (phase protocol.Phase, subkey string, ok bool) {
	rest, found := strings.CutPrefix(key, phaseKeyPrefix)
	if !found {
		return "", "", false
	}
	name, subkey, found := strings.Cut(rest, ".")
	if !found {
		return "", "", false
	}
	return protocol.Phase(name), subkey, true
}

// assignPhaseKey sets cfg.Phases[phase]'s Skip or Deny field for one
// phase.<name>.<subkey> config key. Both an unrecognized phase name and an
// unrecognized subkey are hard errors — unlike a wholly unrelated unknown
// top-level key, anything under the phase.* namespace belongs to this
// schema, so a typo here (phase.plannning.deny, phase.planning.frobnicate)
// should fail loudly rather than silently do nothing. consumed is how many
// extra lines a list-shaped subkey (deny) read past line, for the caller to
// skip past, mirroring listFieldFor's own list-consuming callers.
func assignPhaseKey(cfg *Config, phase protocol.Phase, subkey, value string, lines []string, next, line int) (consumed int, err error) {
	if !slices.Contains(protocol.ConfigurablePhases, phase) {
		return 0, fmt.Errorf("config: line %d: unrecognized phase %q", line, phase)
	}
	if cfg.Phases == nil {
		cfg.Phases = protocol.PhaseConfig{}
	}
	policy := cfg.Phases[phase]
	switch subkey {
	case "skip":
		b, perr := strconv.ParseBool(value)
		if perr != nil {
			return 0, fmt.Errorf("config: line %d: phase.%s.skip: %w", line, phase, perr)
		}
		policy.Skip = b
	case "deny":
		if value != "" {
			return 0, fmt.Errorf("config: line %d: phase.%s.deny expects a list on following indented lines, not an inline value", line, phase)
		}
		items, listConsumed, lerr := parseYAMLList(lines, next)
		if lerr != nil {
			return 0, lerr
		}
		policy.Deny = items
		consumed = listConsumed
	default:
		return 0, fmt.Errorf("config: line %d: unrecognized phase policy key %q", line, subkey)
	}
	cfg.Phases[phase] = policy
	return consumed, nil
}

// listFieldFor returns a pointer to cfg's field for key, for the keys whose
// value is a list block (`allow`, `proof_required_paths`,
// `always_review_paths`), or nil if key names a scalar or unknown key.
func listFieldFor(cfg *Config, key string) *[]string {
	switch key {
	case "allow":
		return &cfg.Allow
	case "proof_required_paths":
		return &cfg.ProofRequiredPaths
	case "always_review_paths":
		return &cfg.AlwaysReviewPaths
	default:
		return nil
	}
}

// parseYAML parses the minimal subset of YAML encodeYAML produces: comments
// (# to end of line, outside quotes), blank lines, top-level `key: value`
// scalars (base_branch, worker_placement, launcher, forge, status_page,
// worktree_dir, brief_note, review_note, ship_verify_command,
// gate_verify_command, worktree_bootstrap_command, title_prefix_template,
// owner_stale_after, review_effort, max_diff_lines, rework_budget; value
// optionally quoted, plus
// deprecatedKeyAliases' old names), a top-level list key (`allow`, `proof_required_paths`,
// `always_review_paths`) followed by indented `- value` list items, and a
// dotted `phase.<name>.skip`/`phase.<name>.deny` key per protocol.
// ConfigurablePhases (see parsePhaseKey/assignPhaseKey) — an unrecognized
// phase name or subkey under that namespace is a hard error, unlike an
// unrelated unknown top-level key, which is ignored (along with any
// indented block under it),
// so a future config key this version doesn't know about doesn't break
// parsing — the same forward-compatibility internal/config's TOML parser
// gives unknown sections.
func parseYAML(data string) (Config, error) {
	var cfg Config
	lines := strings.Split(data, "\n")
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(stripYAMLComment(lines[i]))
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "- ") || trimmed == "-" {
			return Config{}, fmt.Errorf("config: line %d: list item %q outside of a recognized key", i+1, trimmed)
		}
		key, rest, ok := strings.Cut(trimmed, ":")
		if !ok {
			return Config{}, fmt.Errorf("config: line %d: expected key: value, got %q", i+1, trimmed)
		}
		key = strings.TrimSpace(key)
		rest = strings.TrimSpace(rest)

		if newKey, ok := deprecatedKeyAliases[key]; ok {
			cfg.Deprecated = append(cfg.Deprecated, DeprecatedKeyUse{Old: key, New: newKey})
			key = newKey
		}

		if dst := listFieldFor(&cfg, key); dst != nil {
			if rest != "" {
				return Config{}, fmt.Errorf("config: line %d: %q expects a list on following indented lines, not an inline value", i+1, key)
			}
			items, consumed, err := parseYAMLList(lines, i+1)
			if err != nil {
				return Config{}, err
			}
			*dst = items
			i += consumed
			continue
		}

		value, err := unquoteYAML(rest)
		if err != nil {
			return Config{}, fmt.Errorf("config: line %d: bad value %q: %w", i+1, rest, err)
		}

		if phase, subkey, ok := parsePhaseKey(key); ok {
			consumed, perr := assignPhaseKey(&cfg, phase, subkey, value, lines, i+1, i+1)
			if perr != nil {
				return Config{}, perr
			}
			i += consumed
			continue
		}

		handled, err := assignScalarField(&cfg, key, value, i+1)
		if err != nil {
			return Config{}, err
		}
		if !handled && rest == "" {
			// Unknown key: if it introduces its own indented list block, skip
			// past it too so the next iteration doesn't trip the "list item
			// outside of a recognized key" check above.
			_, consumed, _ := parseYAMLList(lines, i+1)
			i += consumed
		}
	}
	return cfg, nil
}

// assignScalarField sets cfg's field for one of parseYAML's scalar keys
// (base_branch, worker_placement, launcher, forge, status_page, worktree_dir,
// brief_note, review_note, ship_verify_command, gate_verify_command,
// worktree_bootstrap_command, title_prefix_template, owner_stale_after,
// review_effort, max_diff_lines, rework_budget — key is already the canonical name by the
// time it reaches here, deprecatedKeyAliases having been applied by the
// caller), reporting whether key was recognized so parseYAML can still skip
// an unrecognized key's indented block. line is the 1-based source line,
// for error messages.
func assignScalarField(cfg *Config, key, value string, line int) (bool, error) {
	switch key {
	case "base_branch":
		cfg.BaseBranch = value
	case "worker_placement":
		cfg.WorkerPlacement = value
	case "launcher":
		cfg.Launcher = value
	case "forge":
		cfg.Forge = value
	case "status_page":
		cfg.StatusPage = value
	case "worktree_dir":
		cfg.WorktreeDir = value
	case "brief_note":
		cfg.BriefNote = value
	case "review_note":
		cfg.ReviewNote = value
	case "ship_verify_command":
		cfg.ShipVerifyCommand = value
	case "gate_verify_command":
		cfg.GateVerifyCommand = value
	case "worktree_bootstrap_command":
		cfg.WorktreeBootstrapCommand = value
	case "title_prefix_template":
		cfg.TitlePrefixTemplate = value
	case "owner_stale_after":
		cfg.OwnerStaleAfter = value
	case "review_effort":
		cfg.ReviewEffort = value
	case "max_diff_lines":
		n, err := strconv.Atoi(value)
		if err != nil {
			return true, fmt.Errorf("config: line %d: max_diff_lines: %w", line, err)
		}
		cfg.MaxDiffLines = &n
	case "rework_budget":
		n, err := strconv.Atoi(value)
		if err != nil {
			return true, fmt.Errorf("config: line %d: rework_budget: %w", line, err)
		}
		cfg.ReworkBudget = &n
	default:
		return false, nil
	}
	return true, nil
}

// parseYAMLList reads consecutive indented "- value" list items starting at
// lines[start], stopping at the first blank/comment-only line, non-list-item
// line, or end of input. It returns the parsed items and how many lines were
// consumed (so the caller can advance its own index past them).
func parseYAMLList(lines []string, start int) (items []string, consumed int, err error) {
	i := start
	for ; i < len(lines); i++ {
		trimmed := strings.TrimSpace(stripYAMLComment(lines[i]))
		if trimmed == "" {
			consumed = i - start + 1
			continue
		}
		if !strings.HasPrefix(trimmed, "- ") {
			break
		}
		value, uerr := unquoteYAML(strings.TrimSpace(trimmed[2:]))
		if uerr != nil {
			return nil, 0, fmt.Errorf("config: line %d: bad list item %q: %w", i+1, trimmed, uerr)
		}
		items = append(items, value)
		consumed = i - start + 1
	}
	return items, consumed, nil
}

// stripYAMLComment removes a trailing "# ..." comment, honoring
// double-quoted strings so a '#' inside a value is not mistaken for the
// start of one — the same logic internal/config's TOML parser uses.
func stripYAMLComment(line string) string {
	inQuote := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			if !quoteEscaped(line, i) {
				inQuote = !inQuote
			}
		case '#':
			if !inQuote {
				return line[:i]
			}
		}
	}
	return line
}

// quoteEscaped reports whether the '"' at i is escaped, i.e. preceded by an
// odd run of backslashes — an even run (e.g. a value ending in "\\") is
// itself escaped backslashes and leaves the quote a real delimiter.
func quoteEscaped(line string, i int) bool {
	n := 0
	for j := i - 1; j >= 0 && line[j] == '\\'; j-- {
		n++
	}
	return n%2 == 1
}
