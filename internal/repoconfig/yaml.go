package repoconfig

import (
	"fmt"
	"strconv"
	"strings"
)

// configSchemaHeader points editors with the yaml-language-server extension
// at schemas/config.schema.json, the same trick eos's initSchemaHeader uses
// for service.yaml — inline validation/autocomplete for free, no custom LSP.
const configSchemaHeader = "# yaml-language-server: $schema=https://raw.githubusercontent.com/Elysium-Labs-EU/argus/main/schemas/config.schema.json\n"

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
	if cfg.Forge != "" {
		fmt.Fprintf(&b, "forge: %s\n", quoteYAML(cfg.Forge))
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
	if cfg.ShipLint != "" {
		fmt.Fprintf(&b, "ship_lint: %s\n", quoteYAML(cfg.ShipLint))
	}
	if cfg.VerifyCommand != "" {
		fmt.Fprintf(&b, "verify_command: %s\n", quoteYAML(cfg.VerifyCommand))
	}
	if cfg.MaxDiffLines != nil {
		fmt.Fprintf(&b, "max_diff_lines: %d\n", *cfg.MaxDiffLines)
	}
	writeYAMLList(&b, "proof_required_paths", cfg.ProofRequiredPaths)
	writeYAMLList(&b, "always_review_paths", cfg.AlwaysReviewPaths)
	return b.String()
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
// scalars (base_branch, worker_placement, forge, worktree_dir, brief_note,
// review_note, ship_lint, verify_command, review_effort, max_diff_lines;
// value optionally quoted), and a top-level list key (`allow`, `proof_required_paths`,
// `always_review_paths`) followed by indented `- value` list items. Any
// other top-level key is ignored (along with any indented block under it),
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
		switch key {
		case "base_branch":
			cfg.BaseBranch = value
		case "worker_placement":
			cfg.WorkerPlacement = value
		case "forge":
			cfg.Forge = value
		case "worktree_dir":
			cfg.WorktreeDir = value
		case "brief_note":
			cfg.BriefNote = value
		case "review_note":
			cfg.ReviewNote = value
		case "ship_lint":
			cfg.ShipLint = value
		case "verify_command":
			cfg.VerifyCommand = value
		case "review_effort":
			cfg.ReviewEffort = value
		case "max_diff_lines":
			n, perr := strconv.Atoi(value)
			if perr != nil {
				return Config{}, fmt.Errorf("config: line %d: max_diff_lines: %w", i+1, perr)
			}
			cfg.MaxDiffLines = &n
		default:
			// Unknown key: if it introduces its own indented list block, skip
			// past it too so the next iteration doesn't trip the "list item
			// outside of a recognized key" check above.
			if rest == "" {
				_, consumed, _ := parseYAMLList(lines, i+1)
				i += consumed
			}
		}
	}
	return cfg, nil
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
			if i == 0 || line[i-1] != '\\' {
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
