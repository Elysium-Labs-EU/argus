package repoconfig

import (
	"fmt"
	"strconv"
	"strings"
)

// encodeYAML renders cfg as the minimal YAML document parseYAML can read
// back: a leading comment, then any of the three keys that are actually set,
// in field order. Like internal/config's TOML encoder, this is deliberately
// not a general-purpose YAML encoder — the schema is exactly three optional
// keys (see Config's doc comment).
func encodeYAML(cfg Config) string {
	var b strings.Builder
	b.WriteString("# .argus/config.yml — all keys are optional; see `argus init`.\n")
	if cfg.BaseBranch != "" {
		fmt.Fprintf(&b, "base_branch: %s\n", quoteYAML(cfg.BaseBranch))
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
	return b.String()
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

// parseYAML parses the minimal subset of YAML encodeYAML produces: comments
// (# to end of line, outside quotes), blank lines, top-level `key: value`
// scalars (base_branch, brief_note; value optionally quoted), and a top-level
// `allow:` key followed by indented `- value` list items. Any other
// top-level key is ignored (along with any indented block under it), so a
// future config key this version doesn't know about doesn't break parsing —
// the same forward-compatibility internal/config's TOML parser gives unknown
// sections.
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

		if key == "allow" {
			if rest != "" {
				return Config{}, fmt.Errorf("config: line %d: %q expects a list on following indented lines, not an inline value", i+1, key)
			}
			items, consumed, err := parseYAMLList(lines, i+1)
			if err != nil {
				return Config{}, err
			}
			cfg.Allow = items
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
		case "brief_note":
			cfg.BriefNote = value
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
