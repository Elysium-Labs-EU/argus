package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// encodeTOML renders cfg as a minimal TOML document: a single [credential]
// table of quoted-string key/value pairs. Keys are quoted unconditionally
// (valid TOML in every case, and required whenever a credential name — e.g. a
// forge host like "github.com" — contains a dot, which bare TOML keys treat
// as a nested table). Keys are sorted so repeated writes of an unchanged
// Config produce an identical file.
func encodeTOML(cfg Config) string {
	if len(cfg.Credential) == 0 {
		return ""
	}
	names := make([]string, 0, len(cfg.Credential))
	for name := range cfg.Credential {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("[credential]\n")
	for _, name := range names {
		fmt.Fprintf(&b, "%s = %s\n", strconv.Quote(name), strconv.Quote(cfg.Credential[name]))
	}
	return b.String()
}

// parseTOML parses the minimal subset of TOML encodeTOML produces: comments
// (# to end of line), blank lines, a [section] header, and key = "value"
// lines whose key may be bare or a quoted string. It is deliberately not a
// general-purpose TOML parser — the file is only ever written by `argus
// config set` (see Config's doc comment) — but parses quoted keys/values with
// Go's own quoting rules (strconv.Unquote) so a value containing a quote or
// backslash round-trips correctly. Only the [credential] section is
// recognized; any other section's keys are ignored, so a future config
// section this version doesn't know about doesn't break parsing.
func parseTOML(data string) (Config, error) {
	var cfg Config
	section := ""
	for lineNum, raw := range strings.Split(data, "\n") {
		line := stripComment(raw)
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return Config{}, fmt.Errorf("config: line %d: expected key = value, got %q", lineNum+1, raw)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		unquotedKey, err := unquoteTOML(key)
		if err != nil {
			return Config{}, fmt.Errorf("config: line %d: bad key %q: %w", lineNum+1, key, err)
		}
		unquotedValue, err := unquoteTOML(value)
		if err != nil {
			return Config{}, fmt.Errorf("config: line %d: bad value %q: %w", lineNum+1, value, err)
		}

		if section == "credential" {
			if cfg.Credential == nil {
				cfg.Credential = make(map[string]string)
			}
			cfg.Credential[unquotedKey] = unquotedValue
		}
	}
	return cfg, nil
}

// stripComment removes a trailing "# ..." comment, honoring double-quoted
// strings so a '#' inside a value is not mistaken for the start of one.
func stripComment(line string) string {
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

// unquoteTOML strips and unescapes a double-quoted string using Go's own
// quoting rules (a compatible subset of TOML's own escape sequences), or
// returns s unchanged if it is a bare (unquoted) token.
func unquoteTOML(s string) (string, error) {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strconv.Unquote(s)
	}
	return s, nil
}
