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

// legacyKey names how a superseded flat top-level .argus/config.yml key
// still parses. assignAs is the key name assignScalarField/listFieldFor
// dispatch on internally — identical to the map key itself unless the key
// was also renamed, not just relocated (ship_lint/verify_command, whose
// current names are ship_verify_command/gate_verify_command). newLoc is the
// dotted path shown in the deprecation warning: the key's actual location
// under the new ship:/rework:/review:/phases: nesting.
type legacyKey struct {
	assignAs string
	newLoc   string
}

// legacyFlatKeys maps a superseded top-level .argus/config.yml key to how it
// still parses. parseYAML accepts every key here, assigning to the same
// Config field its current nested location would, and records the mapping
// on Config.Deprecated so a caller can warn an operator to migrate — argus
// is young enough that key names and locations are still being corrected,
// and support for an old shape is expected to be temporary, not permanent
// API surface. worktree_setup_cmd/worktree_setup_command are the one entry
// pair that only renamed, never relocated: worktree_bootstrap_command was
// always a top-level (region 1) key, so their newLoc is unchanged from
// their assignAs.
var legacyFlatKeys = map[string]legacyKey{
	"ship_lint":              {"ship_verify_command", "ship.verify_command"},
	"ship_verify_command":    {"ship_verify_command", "ship.verify_command"},
	"verify_command":         {"gate_verify_command", "review.gate_verify_command"},
	"gate_verify_command":    {"gate_verify_command", "review.gate_verify_command"},
	"worktree_setup_cmd":     {"worktree_bootstrap_command", "worktree_bootstrap_command"},
	"worktree_setup_command": {"worktree_bootstrap_command", "worktree_bootstrap_command"},
	"title_prefix_template":  {"title_prefix_template", "ship.title_prefix_template"},
	"max_diff_lines":         {"max_diff_lines", "review.max_diff_lines"},
	"proof_required_paths":   {"proof_required_paths", "review.proof_required_paths"},
	"always_review_paths":    {"always_review_paths", "review.always_review_paths"},
	"review_note":            {"review_note", "review.review_note"},
	"review_effort":          {"review_effort", "review.review_effort"},
	"rework_budget":          {"rework_budget", "rework.budget"},
}

// awaitingReviewOnlyKeys are the gate/review cluster's deprecated subkeys
// under phases.awaiting_review (canonical location: review.<key>, see
// parseReviewBlock) — kept parsing for back-compat only. They fire once, on
// entering the terminal phase, right before a verdict is recorded — unlike
// allow/deny/skip, which are live on every configurable phase and stay
// canonical there.
var awaitingReviewOnlyKeys = []string{
	"gate_verify_command", "max_diff_lines", "proof_required_paths",
	"always_review_paths", "review_note", "review_effort",
}

// encodeYAML renders cfg as the minimal YAML document parseYAML can read
// back: a leading comment, then region 1's top-level scalars/lists actually
// set, then a `ship:`, `rework:`, `review:`, and `phases:` block for whatever's
// configured there. Like internal/config's TOML encoder, this is deliberately
// not a general-purpose YAML encoder. It never emits a deprecated flat/dotted
// key name — only the current nested shape, regardless of which shape the
// in-memory Config was originally loaded from.
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
	if cfg.WorktreeBootstrapCommand != "" {
		fmt.Fprintf(&b, "worktree_bootstrap_command: %s\n", quoteYAML(cfg.WorktreeBootstrapCommand))
	}
	if cfg.OwnerStaleAfter != "" {
		fmt.Fprintf(&b, "owner_stale_after: %s\n", quoteYAML(cfg.OwnerStaleAfter))
	}
	if cfg.ExperimentalWorkerSandbox {
		fmt.Fprintf(&b, "experimental_worker_sandbox: %t\n", cfg.ExperimentalWorkerSandbox)
	}
	writeYAMLList(&b, "sandbox_allow_write", cfg.SandboxAllowWrite)
	writeYAMLList(&b, "allow", cfg.Allow)
	if cfg.BriefNote != "" {
		fmt.Fprintf(&b, "brief_note: %s\n", quoteYAML(cfg.BriefNote))
	}
	writeShipBlock(&b, cfg)
	writeReworkBlock(&b, cfg)
	writeReviewBlock(&b, cfg)
	writePhasesBlock(&b, cfg)
	return b.String()
}

// writeShipBlock appends the `ship:` block — argus-side actions that run
// after a worker is done, initiated by the operator, so they get their own
// region instead of living under phases:. Writes nothing if neither field is
// set.
func writeShipBlock(b *strings.Builder, cfg *Config) {
	if cfg.ShipVerifyCommand == "" && cfg.TitlePrefixTemplate == "" {
		return
	}
	b.WriteString("\nship:\n")
	if cfg.ShipVerifyCommand != "" {
		fmt.Fprintf(b, "  verify_command: %s\n", quoteYAML(cfg.ShipVerifyCommand))
	}
	if cfg.TitlePrefixTemplate != "" {
		fmt.Fprintf(b, "  title_prefix_template: %s\n", quoteYAML(cfg.TitlePrefixTemplate))
	}
}

// writeReworkBlock appends the `rework:` block — argus's own rework-loop
// operation config (the cumulative restart budget and one invocation's own
// round ceiling), sibling to `ship:` rather than a top-level static fact.
// Writes nothing if neither field is set.
func writeReworkBlock(b *strings.Builder, cfg *Config) {
	if cfg.ReworkBudget == nil && cfg.MaxReworkRounds == nil {
		return
	}
	b.WriteString("\nrework:\n")
	if cfg.ReworkBudget != nil {
		fmt.Fprintf(b, "  budget: %d\n", *cfg.ReworkBudget)
	}
	if cfg.MaxReworkRounds != nil {
		fmt.Fprintf(b, "  max_rounds: %d\n", *cfg.MaxReworkRounds)
	}
}

// writeReviewBlock appends the `review:` block — the gate/review cluster,
// sibling to `ship:`/`rework:` rather than smeared onto the worker-permission
// phases.awaiting_review block it used to live under. Writes nothing if
// reviewBlockLines finds nothing configured.
func writeReviewBlock(b *strings.Builder, cfg *Config) {
	lines := reviewBlockLines(cfg)
	if len(lines) == 0 {
		return
	}
	b.WriteString("\nreview:\n")
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\n")
	}
}

// reviewBlockLines renders the gate/review cluster's configured fields as
// already-indented (2-space, region-2 depth) YAML lines, shared by
// writeReviewBlock.
func reviewBlockLines(cfg *Config) []string {
	const indent = "  "
	var lines []string
	if cfg.GateVerifyCommand != "" {
		lines = append(lines, fmt.Sprintf("%sgate_verify_command: %s", indent, quoteYAML(cfg.GateVerifyCommand)))
	}
	if cfg.MaxDiffLines != nil {
		lines = append(lines, fmt.Sprintf("%smax_diff_lines: %d", indent, *cfg.MaxDiffLines))
	}
	lines = append(lines, indentedYAMLList(indent, "proof_required_paths", cfg.ProofRequiredPaths)...)
	lines = append(lines, indentedYAMLList(indent, "always_review_paths", cfg.AlwaysReviewPaths)...)
	if cfg.ReviewNote != "" {
		lines = append(lines, fmt.Sprintf("%sreview_note: %s", indent, quoteYAML(cfg.ReviewNote)))
	}
	if cfg.ReviewEffort != "" {
		lines = append(lines, fmt.Sprintf("%sreview_effort: %s", indent, quoteYAML(cfg.ReviewEffort)))
	}
	return lines
}

// writePhasesBlock appends the `phases:` block: one nested entry per
// protocol.ConfigurablePhases value that has its own live allow/deny/skip
// policy configured — purely worker-permission contexts now, no operation
// config (the gate/review cluster moved to its own `review:` region, see
// writeReviewBlock). A phase with nothing configured is omitted entirely,
// mirroring every other optional key here.
func writePhasesBlock(b *strings.Builder, cfg *Config) {
	bodies := make(map[protocol.Phase][]string, len(protocol.ConfigurablePhases))
	for _, p := range protocol.ConfigurablePhases {
		policy, ok := cfg.Phases[p]
		if !ok {
			continue
		}
		var lines []string
		if policy.Skip {
			lines = append(lines, "    skip: true")
		}
		lines = append(lines, indentedYAMLList("    ", "allow", policy.Allow)...)
		lines = append(lines, indentedYAMLList("    ", "deny", policy.Deny)...)
		if len(lines) > 0 {
			bodies[p] = lines
		}
	}
	if len(bodies) == 0 {
		return
	}
	b.WriteString("\nphases:\n")
	for _, p := range protocol.ConfigurablePhases {
		lines, ok := bodies[p]
		if !ok {
			continue
		}
		fmt.Fprintf(b, "  %s:\n", p)
		for _, l := range lines {
			b.WriteString(l)
			b.WriteString("\n")
		}
	}
}

// indentedYAMLList renders a key's "- value" list block at the given indent
// (4 spaces for phases.<name>.<subkey>, 2 spaces for review.<subkey> — see
// callers), or nothing if items is empty — the nested-block equivalent of
// writeYAMLList, which only ever writes at zero indentation.
func indentedYAMLList(indent, key string, items []string) []string {
	if len(items) == 0 {
		return nil
	}
	lines := make([]string, 0, len(items)+1)
	lines = append(lines, indent+key+":")
	for _, item := range items {
		lines = append(lines, indent+"  - "+quoteYAML(item))
	}
	return lines
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

// phaseKeyPrefix precedes the deprecated dotted per-phase policy key:
// phase.<name>.<subkey>, superseded by the nested phases.<name>.<subkey>
// shape (see parsePhasesBlock).
const phaseKeyPrefix = "phase."

// parsePhaseKey splits a deprecated dotted phase.<name>.<subkey> config key
// into its phase and subkey parts. ok is false for any key not shaped like
// phase.<name>.<subkey> — such a key falls through to legacyFlatKeys/
// listFieldFor/assignScalarField as usual.
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
// deprecated dotted phase.<name>.<subkey> config key — the current,
// non-deprecated shape nests the same policy under phases.<name>.<subkey>
// instead (see parsePhaseSubBlock). Both an unrecognized phase name and an
// unrecognized subkey are hard errors — unlike a wholly unrelated unknown
// top-level key, anything under the phase.* namespace belongs to this
// schema, so a typo here (phase.plannning.deny, phase.planning.frobnicate)
// should fail loudly rather than silently do nothing. consumed is how many
// extra lines a list-shaped subkey (deny) read past line, for the caller to
// skip past, mirroring listFieldFor's own list-consuming callers. allow has
// no dotted form: it's new in the nested shape, with nothing to deprecate.
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
	cfg.Deprecated = append(cfg.Deprecated, DeprecatedKeyUse{
		Old: fmt.Sprintf("phase.%s.%s", phase, subkey),
		New: fmt.Sprintf("phases.%s.%s", phase, subkey),
	})
	return consumed, nil
}

// listFieldFor returns a pointer to cfg's field for key, for the keys whose
// value is a list block (`allow`, `proof_required_paths`,
// `always_review_paths`), or nil if key names a scalar or unknown key. It is
// used both for these keys' current top-level home (proof_required_paths/
// always_review_paths are deprecated there, allow is not) and, via
// assignAwaitingReviewKey, for proof_required_paths/always_review_paths'
// current nested home under phases.awaiting_review.
func listFieldFor(cfg *Config, key string) *[]string {
	switch key {
	case "allow":
		return &cfg.Allow
	case "proof_required_paths":
		return &cfg.ProofRequiredPaths
	case "always_review_paths":
		return &cfg.AlwaysReviewPaths
	case "sandbox_allow_write":
		return &cfg.SandboxAllowWrite
	default:
		return nil
	}
}

// cutKeyValue splits a trimmed "key: value" line into its space-trimmed
// parts. ok is false when there is no ':' at all — trimmed isn't shaped like
// a key line (blank and list-item lines are filtered by callers before this
// runs).
func cutKeyValue(trimmed string) (key, value string, ok bool) {
	k, v, found := strings.Cut(trimmed, ":")
	if !found {
		return "", "", false
	}
	return strings.TrimSpace(k), strings.TrimSpace(v), true
}

// indentOf counts line's leading space characters — parseYAML's nested
// blocks (ship:, phases:, and phases:'s own per-phase sub-blocks) are
// indentation-scoped: a block's content is every contiguous line indented
// past its header, blank/comment lines skipped over without ending it.
// Tabs aren't recognized as indentation; encodeYAML never emits them.
func indentOf(line string) int {
	n := 0
	for _, r := range line {
		if r != ' ' {
			break
		}
		n++
	}
	return n
}

// parseYAML parses the minimal subset of YAML encodeYAML produces: comments
// (# to end of line, outside quotes), blank lines, region 1's top-level
// `key: value` scalars (base_branch, worker_placement, launcher, forge,
// status_page, worktree_dir, brief_note, worktree_bootstrap_command,
// owner_stale_after, rework_budget; value optionally quoted) and its one
// top-level list key (`allow`); a top-level `ship:` block (see
// parseShipBlock) and a top-level `phases:` block (see parsePhasesBlock,
// parsePhaseSubBlock); legacyFlatKeys' deprecated flat top-level keys and
// the deprecated dotted `phase.<name>.skip`/`phase.<name>.deny` key (see
// parsePhaseKey/assignPhaseKey) — both still parse into their current
// field/location, recording the mapping on Config.Deprecated. An unknown or
// malformed key at any level is a hard error naming a line number, never
// silently skipped — a config key argus doesn't recognize is far more often
// a typo or a stale name than a forward-compatible key a future version will
// understand, and a silent skip gives an operator no signal either way. The
// actual per-key dispatch lives in assignTopLevelKey — split out so this
// loop stays a small, easily-read driver instead of one large per-line
// switch.
func parseYAML(data string) (Config, error) {
	var cfg Config
	lines := strings.Split(data, "\n")
	for i := 0; i < len(lines); i++ {
		raw := lines[i]
		trimmed := strings.TrimSpace(stripYAMLComment(raw))
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "- ") || trimmed == "-" {
			return Config{}, fmt.Errorf("config: line %d: list item %q outside of a recognized key", i+1, trimmed)
		}
		if indentOf(raw) != 0 {
			return Config{}, fmt.Errorf("config: line %d: unexpected indentation", i+1)
		}
		key, rest, ok := cutKeyValue(trimmed)
		if !ok {
			return Config{}, fmt.Errorf("config: line %d: expected key: value, got %q", i+1, trimmed)
		}
		consumed, err := assignTopLevelKey(&cfg, lines, i, key, rest)
		if err != nil {
			return Config{}, err
		}
		i += consumed
	}
	return cfg, nil
}

// assignTopLevelKey dispatches one top-level "key: value"/"key:" line
// (already split into key/rest by parseYAML) to whichever region it names: a
// ship:/rework:/review:/phases: block header (parseRegionBlock), a
// deprecated dotted phase.<name>.<subkey> key (parseDottedPhaseKey), or an
// ordinary flat scalar/list key, current or deprecated (assignFlatKey).
// Splitting these three families into their own functions — rather than one
// large switch — keeps each one's own cyclomatic complexity (and so its CRAP
// score) low individually, even though the total behavior is unchanged. i is
// the line's zero-based index in lines, needed by the block-header/dotted-key
// cases to locate their own following content; line is i+1, the 1-based
// line number every error message here uses.
func assignTopLevelKey(cfg *Config, lines []string, i int, key, rest string) (consumed int, err error) {
	line := i + 1
	switch key {
	case "ship", "rework", "review", "phases":
		return parseRegionBlock(cfg, lines, i, key, rest, line)
	}
	if phase, subkey, ok := parsePhaseKey(key); ok {
		return parseDottedPhaseKey(cfg, lines, i, phase, subkey, rest, line)
	}
	return assignFlatKey(cfg, lines, i+1, key, rest, line)
}

// parseRegionBlock handles a top-level "ship:"/"rework:"/"review:"/"phases:"
// block-header line: it must carry no inline value (the block's content is
// its indented body — see parseShipBlock/parseReworkBlock/parseReviewBlock/
// parsePhasesBlock), and dispatches to the matching block parser.
func parseRegionBlock(cfg *Config, lines []string, i int, key, rest string, line int) (consumed int, err error) {
	if rest != "" {
		return 0, fmt.Errorf("config: line %d: %q expects a nested block, not an inline value", line, key)
	}
	switch key {
	case "ship":
		return parseShipBlock(cfg, lines, i+1, 0)
	case "rework":
		return parseReworkBlock(cfg, lines, i+1, 0)
	case "review":
		return parseReviewBlock(cfg, lines, i+1, 0)
	default:
		return parsePhasesBlock(cfg, lines, i+1, 0)
	}
}

// parseDottedPhaseKey handles a deprecated dotted phase.<name>.<subkey>
// top-level key (see parsePhaseKey/assignPhaseKey) — split out of
// assignTopLevelKey purely to keep that dispatcher's own branching minimal.
func parseDottedPhaseKey(cfg *Config, lines []string, i int, phase protocol.Phase, subkey, rest string, line int) (consumed int, err error) {
	value, uerr := unquoteYAML(rest)
	if uerr != nil {
		return 0, fmt.Errorf("config: line %d: bad value %q: %w", line, rest, uerr)
	}
	return assignPhaseKey(cfg, phase, subkey, value, lines, i+1, line)
}

// assignFlatKey handles a top-level key that is neither a ship:/phases:
// block header nor a dotted phase.<name>.<subkey> key: a region 1 scalar/
// list key, or one of legacyFlatKeys' deprecated flat forms (which still
// assigns the same field, recording a Deprecated entry first). An
// unrecognized key is a hard error naming a line number, never silently
// skipped. next is lines' index right after this key's own line, for the
// list-shaped keys (allow, proof_required_paths, always_review_paths) that
// read their items from following indented lines.
func assignFlatKey(cfg *Config, lines []string, next int, key, rest string, line int) (consumed int, err error) {
	if lk, ok := legacyFlatKeys[key]; ok {
		cfg.Deprecated = append(cfg.Deprecated, DeprecatedKeyUse{Old: key, New: lk.newLoc})
		key = lk.assignAs
	}

	if dst := listFieldFor(cfg, key); dst != nil {
		if rest != "" {
			return 0, fmt.Errorf("config: line %d: %q expects a list on following indented lines, not an inline value", line, key)
		}
		items, listConsumed, lerr := parseYAMLList(lines, next)
		if lerr != nil {
			return 0, lerr
		}
		*dst = items
		return listConsumed, nil
	}

	value, verr := unquoteYAML(rest)
	if verr != nil {
		return 0, fmt.Errorf("config: line %d: bad value %q: %w", line, rest, verr)
	}
	handled, herr := assignScalarField(cfg, key, value, line)
	if herr != nil {
		return 0, herr
	}
	if !handled {
		return 0, fmt.Errorf("config: line %d: unrecognized key %q", line, key)
	}
	return 0, nil
}

// parseShipBlock parses the indented body of a top-level `ship:` key — the
// argus-side actions that run after a worker is done, initiated by the
// operator, so they get their own region instead of living under phases:.
// start is the line after "ship:"; headerIndent is ship:'s own indentation
// (always 0 — parseYAML only ever calls this for a top-level ship: line).
// An unrecognized subkey is a hard error, the same treatment every other
// namespace here gives a typo. Returns how many lines belong to the block,
// for the caller to skip past.
func parseShipBlock(cfg *Config, lines []string, start, headerIndent int) (consumed int, err error) {
	i := start
	for ; i < len(lines); i++ {
		raw := lines[i]
		trimmed := strings.TrimSpace(stripYAMLComment(raw))
		if trimmed == "" {
			continue
		}
		if indentOf(raw) <= headerIndent {
			break
		}
		key, rest, ok := cutKeyValue(trimmed)
		if !ok {
			return 0, fmt.Errorf("config: line %d: expected key: value, got %q", i+1, trimmed)
		}
		value, uerr := unquoteYAML(rest)
		if uerr != nil {
			return 0, fmt.Errorf("config: line %d: bad value %q: %w", i+1, rest, uerr)
		}
		switch key {
		case "verify_command":
			cfg.ShipVerifyCommand = value
		case "title_prefix_template":
			cfg.TitlePrefixTemplate = value
		default:
			return 0, fmt.Errorf("config: line %d: unrecognized ship key %q", i+1, key)
		}
	}
	return i - start, nil
}

// parseReworkBlock parses the indented body of a top-level `rework:` key —
// argus's own rework-loop operation config, sibling to `ship:` rather than a
// top-level static fact. start is the line after "rework:"; headerIndent is
// rework:'s own indentation (always 0). An unrecognized subkey is a hard
// error, the same treatment every other namespace here gives a typo. Returns
// how many lines belong to the block, for the caller to skip past.
func parseReworkBlock(cfg *Config, lines []string, start, headerIndent int) (consumed int, err error) {
	i := start
	for ; i < len(lines); i++ {
		raw := lines[i]
		trimmed := strings.TrimSpace(stripYAMLComment(raw))
		if trimmed == "" {
			continue
		}
		if indentOf(raw) <= headerIndent {
			break
		}
		key, rest, ok := cutKeyValue(trimmed)
		if !ok {
			return 0, fmt.Errorf("config: line %d: expected key: value, got %q", i+1, trimmed)
		}
		line := i + 1
		switch key {
		case "budget":
			n, aerr := strconv.Atoi(rest)
			if aerr != nil {
				return 0, fmt.Errorf("config: line %d: rework.budget: %w", line, aerr)
			}
			cfg.ReworkBudget = &n
		case "max_rounds":
			n, aerr := strconv.Atoi(rest)
			if aerr != nil {
				return 0, fmt.Errorf("config: line %d: rework.max_rounds: %w", line, aerr)
			}
			cfg.MaxReworkRounds = &n
		default:
			return 0, fmt.Errorf("config: line %d: unrecognized rework key %q", line, key)
		}
	}
	return i - start, nil
}

// reviewBlockListFieldFor returns a pointer to cfg's field for one of
// review:'s two list-shaped keys, or nil for any other key — deliberately
// narrower than listFieldFor (which also matches "allow", not a review: key)
// so a stray "allow:" under review: is the unrecognized-key error it should
// be, not a silent write to cfg.Allow.
func reviewBlockListFieldFor(cfg *Config, key string) *[]string {
	switch key {
	case "proof_required_paths":
		return &cfg.ProofRequiredPaths
	case "always_review_paths":
		return &cfg.AlwaysReviewPaths
	default:
		return nil
	}
}

// parseReviewBlock parses the indented body of a top-level `review:` key —
// the gate/review cluster's canonical home, moved off phases.awaiting_review
// (still accepted there as a deprecated form, see assignAwaitingReviewKey) so
// an argus operation's config no longer lives on a worker permission phase.
// start is the line after "review:"; headerIndent is review:'s own
// indentation (always 0). An unrecognized subkey is a hard error, the same
// treatment every other namespace here gives a typo. Returns how many lines
// belong to the block, for the caller to skip past.
func parseReviewBlock(cfg *Config, lines []string, start, headerIndent int) (consumed int, err error) {
	i := start
	for ; i < len(lines); i++ {
		raw := lines[i]
		trimmed := strings.TrimSpace(stripYAMLComment(raw))
		if trimmed == "" {
			continue
		}
		if indentOf(raw) <= headerIndent {
			break
		}
		key, rest, ok := cutKeyValue(trimmed)
		if !ok {
			return 0, fmt.Errorf("config: line %d: expected key: value, got %q", i+1, trimmed)
		}
		line := i + 1
		if dst := reviewBlockListFieldFor(cfg, key); dst != nil {
			if rest != "" {
				return 0, fmt.Errorf("config: line %d: %q expects a list on following indented lines, not an inline value", line, key)
			}
			items, lc, lerr := parseYAMLList(lines, i+1)
			if lerr != nil {
				return 0, lerr
			}
			*dst = items
			i += lc
			continue
		}
		value, uerr := unquoteYAML(rest)
		if uerr != nil {
			return 0, fmt.Errorf("config: line %d: bad value %q: %w", line, rest, uerr)
		}
		switch key {
		case "gate_verify_command":
			cfg.GateVerifyCommand = value
		case "review_note":
			cfg.ReviewNote = value
		case "review_effort":
			cfg.ReviewEffort = value
		case "max_diff_lines":
			n, aerr := strconv.Atoi(value)
			if aerr != nil {
				return 0, fmt.Errorf("config: line %d: max_diff_lines: %w", line, aerr)
			}
			cfg.MaxDiffLines = &n
		default:
			return 0, fmt.Errorf("config: line %d: unrecognized review key %q", line, key)
		}
	}
	return i - start, nil
}

// parsePhasesBlock parses the indented body of a top-level `phases:` key:
// one nested block per worker-lifecycle phase name (protocol.
// ConfigurablePhases), each holding that phase's own live allow/deny/skip
// rules plus, only for the terminal awaiting_review phase, the gate/review
// cluster (see parsePhaseSubBlock). start is the line after "phases:";
// headerIndent is phases:'s own indentation. An unrecognized phase name is a
// hard error, the same treatment the deprecated dotted form
// (assignPhaseKey) already gives it.
func parsePhasesBlock(cfg *Config, lines []string, start, headerIndent int) (consumed int, err error) {
	i := start
	for ; i < len(lines); i++ {
		raw := lines[i]
		trimmed := strings.TrimSpace(stripYAMLComment(raw))
		if trimmed == "" {
			continue
		}
		phaseIndent := indentOf(raw)
		if phaseIndent <= headerIndent {
			break
		}
		key, rest, ok := cutKeyValue(trimmed)
		if !ok {
			return 0, fmt.Errorf("config: line %d: expected key: value, got %q", i+1, trimmed)
		}
		if rest != "" {
			return 0, fmt.Errorf("config: line %d: phases.%s expects a nested block, not an inline value", i+1, key)
		}
		phase := protocol.Phase(key)
		if !slices.Contains(protocol.ConfigurablePhases, phase) {
			return 0, fmt.Errorf("config: line %d: unrecognized phase %q", i+1, phase)
		}
		sub, serr := parsePhaseSubBlock(cfg, phase, lines, i+1, phaseIndent)
		if serr != nil {
			return 0, serr
		}
		i += sub
	}
	return i - start, nil
}

// parsePhaseSubBlock parses one phase's nested body inside `phases:`:
// allow/deny/skip, live on every configurable phase, plus (only under
// awaiting_review) the gate/review cluster named in awaitingReviewOnlyKeys —
// a gate/review key under any other phase is a hard error naming the phase
// it actually belongs to, so the config can never misrepresent when
// something fires. start is the line after "<phase>:"; headerIndent is the
// phase key's own indentation. cfg.Phases only gains an entry for phase if
// allow/deny/skip actually appeared — a phase configured with only gate/
// review keys leaves the phase policy map untouched, since those keys are
// plain Config fields, not part of a phase's policy.
func parsePhaseSubBlock(cfg *Config, phase protocol.Phase, lines []string, start, headerIndent int) (consumed int, err error) {
	policy := cfg.Phases[phase]
	touchedPolicy := false
	i := start
	for ; i < len(lines); i++ {
		raw := lines[i]
		trimmed := strings.TrimSpace(stripYAMLComment(raw))
		if trimmed == "" {
			continue
		}
		if indentOf(raw) <= headerIndent {
			break
		}
		key, rest, ok := cutKeyValue(trimmed)
		if !ok {
			return 0, fmt.Errorf("config: line %d: expected key: value, got %q", i+1, trimmed)
		}
		line := i + 1
		switch key {
		case "skip":
			b, perr := strconv.ParseBool(rest)
			if perr != nil {
				return 0, fmt.Errorf("config: line %d: phases.%s.skip: %w", line, phase, perr)
			}
			policy.Skip = b
			touchedPolicy = true
		case "deny", "allow":
			if rest != "" {
				return 0, fmt.Errorf("config: line %d: phases.%s.%s expects a list on following indented lines, not an inline value", line, phase, key)
			}
			items, lc, lerr := parseYAMLList(lines, i+1)
			if lerr != nil {
				return 0, lerr
			}
			if key == "deny" {
				policy.Deny = items
			} else {
				policy.Allow = items
			}
			touchedPolicy = true
			i += lc
		default:
			if !slices.Contains(awaitingReviewOnlyKeys, key) {
				return 0, fmt.Errorf("config: line %d: unrecognized phase key %q", line, key)
			}
			if phase != protocol.PhaseAwaitingReview {
				return 0, fmt.Errorf("config: line %d: %q is only valid under phases.awaiting_review (the gate/review cluster fires once, entering the terminal phase), not phases.%s", line, key, phase)
			}
			extra, aerr := assignAwaitingReviewKey(cfg, key, rest, lines, i+1, line)
			if aerr != nil {
				return 0, aerr
			}
			i += extra
		}
	}
	if touchedPolicy {
		if cfg.Phases == nil {
			cfg.Phases = protocol.PhaseConfig{}
		}
		cfg.Phases[phase] = policy
	}
	return i - start, nil
}

// assignAwaitingReviewKey sets one gate/review cluster field
// (awaitingReviewOnlyKeys) from inside phases.awaiting_review — a deprecated
// location now that review: is the canonical one (see parseReviewBlock) —
// reusing listFieldFor/assignScalarField's own field dispatch, the same
// functions that handle these keys' deprecated flat top-level form, so there
// is exactly one place that knows how to parse each value, however it
// arrives. Records the phases.awaiting_review.<key> -> review.<key>
// deprecation on every call, since every awaitingReviewOnlyKeys entry is
// deprecated at this location, unconditionally. consumed is how many extra
// lines a list-shaped key (proof_required_paths/always_review_paths) read
// past line, for the caller to skip past.
func assignAwaitingReviewKey(cfg *Config, key, value string, lines []string, next, line int) (consumed int, err error) {
	cfg.Deprecated = append(cfg.Deprecated, DeprecatedKeyUse{
		Old: fmt.Sprintf("phases.awaiting_review.%s", key),
		New: fmt.Sprintf("review.%s", key),
	})
	if dst := listFieldFor(cfg, key); dst != nil {
		if value != "" {
			return 0, fmt.Errorf("config: line %d: %q expects a list on following indented lines, not an inline value", line, key)
		}
		items, lc, lerr := parseYAMLList(lines, next)
		if lerr != nil {
			return 0, lerr
		}
		*dst = items
		return lc, nil
	}
	unquoted, uerr := unquoteYAML(value)
	if uerr != nil {
		return 0, fmt.Errorf("config: line %d: bad value %q: %w", line, value, uerr)
	}
	if _, serr := assignScalarField(cfg, key, unquoted, line); serr != nil {
		return 0, serr
	}
	return 0, nil
}

// assignScalarField sets cfg's field for one of parseYAML's scalar keys
// (base_branch, worker_placement, launcher, forge, status_page, worktree_dir,
// brief_note, review_note, ship_verify_command, gate_verify_command,
// worktree_bootstrap_command, title_prefix_template, owner_stale_after,
// review_effort, max_diff_lines, rework_budget — key is already the
// canonical field name by the time it reaches here, legacyFlatKeys having
// been applied by the caller for a deprecated flat key, and this same switch
// being reused directly by assignAwaitingReviewKey for these keys' current
// nested location), reporting whether key was recognized so parseYAML can
// error on an unrecognized top-level key instead of silently ignoring it.
// line is the 1-based source line, for error messages.
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
	case "experimental_worker_sandbox":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return true, fmt.Errorf("config: line %d: experimental_worker_sandbox: %w", line, err)
		}
		cfg.ExperimentalWorkerSandbox = b
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
