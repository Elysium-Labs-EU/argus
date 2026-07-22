package cmd

import (
	"fmt"
	"maps"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// This file guards against issue #85: SKILL.md (skills/argus/SKILL.md, and its
// installed mirror at .claude/skills/argus/SKILL.md) has twice drifted out of
// sync with the CLI it documents — once claiming a Codeberg-only story after
// the repo went multi-forge, once citing a flag default that no longer held.
// Neither drift was grep-catchable by hand; these tests are the tripwire.
// They deliberately do NOT attempt full prose/semantic staleness detection
// (e.g. a factually wrong description of *how* a flag behaves) — only the
// mechanical drift a doc/CLI diff can actually catch: flags that no longer
// exist, stated defaults that no longer match, and forge hosts the code
// supports that the doc never mentions.

const (
	skillMDPath    = "../skills/argus/SKILL.md"
	skillMDMirror  = "../.claude/skills/argus/SKILL.md"
	forgeDetectSrc = "../internal/forge/detect.go"
)

// skillDocCommands are the argus subcommands SKILL.md documents by name.
var skillDocCommands = map[string]*cobra.Command{
	"supervise": superviseCmd,
	"ship":      shipCmd,
	"review":    reviewCmd,
	"rebase":    rebaseCmd,
}

// flagSet returns cmd's full flag set exactly as `--help` renders it: its own
// flags plus everything inherited from a parent's persistent flags (--debug).
func flagSet(cmd *cobra.Command) map[string]*pflag.Flag {
	cmd.InitDefaultHelpFlag() // --help is auto-added by cobra on Execute(); we never call it here
	out := map[string]*pflag.Flag{}
	cmd.Flags().VisitAll(func(f *pflag.Flag) { out[f.Name] = f })
	cmd.InheritedFlags().VisitAll(func(f *pflag.Flag) { out[f.Name] = f })
	return out
}

var (
	flagTokenRe  = regexp.MustCompile(`--[a-zA-Z][a-zA-Z0-9-]*`)
	flagValueRe  = regexp.MustCompile(`^(--[a-zA-Z][a-zA-Z0-9-]*)\s+(\S+)$`)
	argusCmdRe   = regexp.MustCompile(`\bargus (supervise|ship|review|rebase)\b`)
	inlineCodeRe = regexp.MustCompile("`([^`\n]+)`")
)

// docSegment is one chunk of SKILL.md as split on ``` fences.
type docSegment struct {
	text   string
	fenced bool
}

// splitFenced splits doc on ``` delimiters, alternating prose and fenced
// code. Flags inside a fenced shell example are checked for existence only
// (they're usage examples with caller-chosen values, not default claims);
// flags inside an inline `--flag value` span are also checked against the
// flag's real --help default.
func splitFenced(doc string) []docSegment {
	parts := strings.Split(doc, "```")
	segments := make([]docSegment, 0, len(parts))
	for i, p := range parts {
		fenced := i%2 == 1
		if fenced {
			if nl := strings.IndexByte(p, '\n'); nl >= 0 {
				p = p[nl+1:] // drop the language tag on the fence's opening line
			}
		}
		segments = append(segments, docSegment{text: p, fenced: fenced})
	}
	return segments
}

func inlineSpans(text string) []string {
	matches := inlineCodeRe.FindAllStringSubmatch(text, -1)
	spans := make([]string, 0, len(matches))
	for _, m := range matches {
		spans = append(spans, m[1])
	}
	return spans
}

// looksLikePlaceholder reports whether a doc-stated flag value is a
// placeholder (`<id>`, `<path>`) rather than a literal default claim.
func looksLikePlaceholder(v string) bool {
	return strings.ContainsAny(v, "<>")
}

// defaultsMatch compares a doc-stated value against a flag's real DefValue,
// normalizing duration flags (SKILL.md says "0", pflag renders "0s").
func defaultsMatch(f *pflag.Flag, docValue string) bool {
	docValue = strings.Trim(docValue, `"'`)
	if f.Value.Type() == "duration" {
		d1, err1 := time.ParseDuration(docValue)
		d2, err2 := time.ParseDuration(f.DefValue)
		if err1 == nil && err2 == nil {
			return d1 == d2
		}
	}
	return docValue == f.DefValue
}

func sortedCmdNames(m map[string]*cobra.Command) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestSkillMDMatchesCLI extracts every --flag SKILL.md mentions in a code
// block or inline code span and checks it against the real flags of the
// argus subcommand the surrounding text is about — plus any default value
// SKILL.md states for it (see issue #85).
func TestSkillMDMatchesCLI(t *testing.T) {
	raw, err := os.ReadFile(skillMDPath)
	if err != nil {
		t.Fatalf("read %s: %v", skillMDPath, err)
	}
	doc := string(raw)

	flags := map[string]map[string]*pflag.Flag{}
	union := map[string]*pflag.Flag{}
	for name, c := range skillDocCommands {
		fs := flagSet(c)
		flags[name] = fs
		maps.Copy(union, fs)
	}

	var problems []string
	currentCmd := ""

	checkToken := func(tok, context string) {
		name := strings.TrimPrefix(tok, "--")
		if _, ok := flags[currentCmd][name]; ok {
			return
		}
		if _, ok := union[name]; ok {
			return
		}
		problems = append(problems, fmt.Sprintf(
			"SKILL.md references %q (%s, read as `argus %s`) but no documented subcommand (%s) defines it — flag renamed or removed?",
			tok, context, currentCmd, strings.Join(sortedCmdNames(skillDocCommands), ", ")))
	}

	checkDefault := func(flagName, docValue string) {
		f, ok := flags[currentCmd][flagName]
		if !ok {
			return // ambiguous context; existence was already checked above
		}
		if !defaultsMatch(f, docValue) {
			problems = append(problems, fmt.Sprintf(
				"SKILL.md claims `--%s` defaults to %q for `argus %s`, but --help reports default %q",
				flagName, docValue, currentCmd, f.DefValue))
		}
	}

	for _, segment := range splitFenced(doc) {
		if m := argusCmdRe.FindStringSubmatch(segment.text); m != nil {
			currentCmd = m[1]
		}
		if segment.fenced {
			for _, tok := range flagTokenRe.FindAllString(segment.text, -1) {
				checkToken(tok, "code example")
			}
			continue
		}
		for _, span := range inlineSpans(segment.text) {
			if fv := flagValueRe.FindStringSubmatch(span); fv != nil {
				checkToken(fv[1], "flag list")
				if !looksLikePlaceholder(fv[2]) {
					checkDefault(strings.TrimPrefix(fv[1], "--"), fv[2])
				}
				continue
			}
			for _, tok := range flagTokenRe.FindAllString(span, -1) {
				checkToken(tok, "inline mention")
			}
		}
	}

	for _, p := range problems {
		t.Error(p)
	}
}

// forgeHostDisplay maps a forge hostname literal to the display name we
// expect SKILL.md to mention somewhere; it mirrors the named hosts in
// internal/forge/detect.go's tokenVarsForHost and credentialHelperToken.
var forgeHostDisplay = map[string]string{
	"github.com":   "GitHub",
	"codeberg.org": "Codeberg",
	"gitlab.com":   "GitLab",
}

var caseHostRe = regexp.MustCompile(`case\s+"([a-zA-Z0-9.-]+)":`)

// funcBody returns the source text of the named top-level function, from its
// `func name(` line up to (not including) the next top-level `func `.
func funcBody(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, "func "+name+"(")
	if start < 0 {
		t.Fatalf("function %s not found in %s — did it get renamed or removed?", name, forgeDetectSrc)
	}
	rest := src[start:]
	if next := strings.Index(rest[1:], "\nfunc "); next >= 0 {
		return rest[:next+1]
	}
	return rest
}

// supportedForgeHosts extracts every forge hostname detect.go's switches
// explicitly handle, so the doc-vs-code cross-check below can't itself drift
// from wherever the real host list lives.
func supportedForgeHosts(t *testing.T, src string) map[string]string {
	t.Helper()
	hosts := map[string]string{}
	for _, fn := range []string{"tokenVarsForHost", "credentialHelperToken"} {
		body := funcBody(t, src, fn)
		for _, m := range caseHostRe.FindAllStringSubmatch(body, -1) {
			if display, known := forgeHostDisplay[m[1]]; known {
				hosts[m[1]] = display
			}
		}
	}
	return hosts
}

// TestSkillMDDescribesAllSupportedForges cross-checks the forge hostnames
// internal/forge/detect.go actually handles against SKILL.md's prose, so the
// doc can't quietly narrow back down to "Codeberg" the way it did before
// argus went multi-forge (issue #85).
func TestSkillMDDescribesAllSupportedForges(t *testing.T) {
	src, err := os.ReadFile(forgeDetectSrc)
	if err != nil {
		t.Fatalf("read %s: %v", forgeDetectSrc, err)
	}
	hosts := supportedForgeHosts(t, string(src))
	if len(hosts) == 0 {
		t.Fatalf("found no known forge hosts in %s — did the switches move or get renamed?", forgeDetectSrc)
	}

	doc, err := os.ReadFile(skillMDPath)
	if err != nil {
		t.Fatalf("read %s: %v", skillMDPath, err)
	}
	lower := strings.ToLower(string(doc))

	for host, display := range hosts {
		if !strings.Contains(lower, strings.ToLower(display)) {
			t.Errorf("SKILL.md never mentions %q (host %q is handled in %s) — doc describes a narrower forge story than the code supports",
				display, host, forgeDetectSrc)
		}
	}
}

// TestSkillMDMirrorInSync guards the second copy of SKILL.md this repo keeps
// checked in at .claude/skills/argus (this project's own local Claude Code
// skill install) against silently drifting from the canonical source under
// skills/argus.
func TestSkillMDMirrorInSync(t *testing.T) {
	canonical, err := os.ReadFile(skillMDPath)
	if err != nil {
		t.Fatalf("read %s: %v", skillMDPath, err)
	}
	mirror, err := os.ReadFile(skillMDMirror)
	if err != nil {
		t.Fatalf("read %s: %v", skillMDMirror, err)
	}
	if string(canonical) != string(mirror) {
		t.Errorf("%s has drifted from %s — copy the canonical file over the mirror", skillMDMirror, skillMDPath)
	}
}
