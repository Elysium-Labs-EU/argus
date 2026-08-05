package supervisor

import (
	"slices"
	"strings"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

// structuralFloorAllow are the Claude Code permission-allow entries
// guaranteed granted in every phase, regardless of repo config: read-only
// git (enough to inspect the working tree) and a worker's own status
// self-calls. Nothing here is ever written into .argus/config.yml as data —
// ResolvedAllowForPhase unions it in from code every time, so a hand-edited
// or missing config file can never narrow a worker's actual floor. init
// documents it as a comment instead (see repoconfig's encodeYAML), for
// visibility without a second, config-file copy that resolution would then
// have to reconcile against this one.
//
// git ls-files is here for the same reason git status/diff/log are: every
// worker brief's shared status-reporting block (protocol.WriterBrief) tells
// the worker to run `git ls-files --others --exclude-standard` to compute
// diff_stat's untracked-file count — an argus-authored instruction handed to
// every worker, in every phase, not something scoped to one dispatch path
// the way RebaseExtraAllow is. Denying it by default would repeat the same
// brief-instructs-a-command-nothing-grants gap the rebase dispatch hit, just
// for every worker's routine status report instead of one operation.
var structuralFloorAllow = []string{
	"Bash(git status*)",
	"Bash(git diff*)",
	"Bash(git log*)",
	"Bash(git ls-files*)",
	"Bash(argus worker report*)",
	"Bash(argus worker answer*)",
	"Bash(argus worker steer*)",
}

// ResolvedAllowForPhase computes the Claude Code permission-allow entries a
// worker reporting phase gets: the structural floor, union baseAllow (a
// repo's phase-independent .argus/config.yml allow list), union project's
// own materialized allow for phase, union extraAllow (operator --allow
// flags, flat across every phase), minus any entry that could authorize a
// DenyFloor command — subtracted last, so nothing upstream of this call can
// re-grant ship/rework/review/supervise or git commit/push by putting a
// wide-enough Bash pattern under any phase's allow list. Order is
// first-seen-wins deduped, not sorted, so a caller's own ordering intent
// (floor first, most-specific config last) survives into the rendered
// settings file.
func ResolvedAllowForPhase(phase protocol.Phase, project protocol.PhaseConfig, baseAllow, extraAllow []string) []string {
	allow := make([]string, 0, len(structuralFloorAllow)+len(baseAllow)+len(extraAllow))
	allow = append(allow, structuralFloorAllow...)
	allow = append(allow, baseAllow...)
	if policy, ok := project[phase]; ok {
		allow = append(allow, policy.Allow...)
	}
	allow = append(allow, extraAllow...)
	return stripDenyFloor(dedupeStrings(allow))
}

// dedupeStrings returns items with duplicates removed, keeping each value's
// first occurrence and original relative order.
func dedupeStrings(items []string) []string {
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))
	for _, it := range items {
		if seen[it] {
			continue
		}
		seen[it] = true
		out = append(out, it)
	}
	return out
}

// stripDenyFloor drops every allow entry that could ever authorize a
// protocol.DenyFloor() command, so a repo config (or --allow flag) putting
// e.g. "Bash(git push*)" under any phase's allow can't re-grant what the
// deny floor is supposed to make unremovable. Entries that aren't
// Bash(...)-shaped (e.g. "Edit(<worktree>/**)") pass through untouched —
// only Bash entries can ever cover a DenyFloor command.
func stripDenyFloor(allow []string) []string {
	floor := protocol.DenyFloor()
	kept := make([]string, 0, len(allow))
	for _, entry := range allow {
		be, ok := parseBashEntry(entry)
		if !ok {
			kept = append(kept, entry)
			continue
		}
		if !be.overlapsAnyFamily(floor) {
			kept = append(kept, entry)
		}
	}
	return kept
}

// bashEntry is one parsed "Bash(pattern)" Claude Code permission entry: its
// literal command prefix with any trailing wildcard stripped, and whether
// that wildcard was present. A bare (unwildcarded) entry matches only that
// exact command line — the same rule Claude Code itself applies — which
// matchesCommand relies on.
type bashEntry struct {
	prefix     string
	wildcarded bool
}

// parseBashEntry parses entry into its bashEntry form, or ok=false if entry
// isn't shaped like "Bash(...)" at all (e.g. an Edit/Write glob entry).
func parseBashEntry(entry string) (be bashEntry, ok bool) {
	inner, ok := strings.CutPrefix(entry, "Bash(")
	if !ok {
		return bashEntry{}, false
	}
	inner, ok = strings.CutSuffix(inner, ")")
	if !ok {
		return bashEntry{}, false
	}
	trimmed := strings.TrimSuffix(inner, ":*")
	if trimmed == inner {
		trimmed = strings.TrimSuffix(inner, "*")
	}
	wildcarded := trimmed != inner
	return bashEntry{prefix: strings.TrimRight(trimmed, " "), wildcarded: wildcarded}, true
}

// matchesCommand reports whether be covers the literal Bash command line
// cmd: an exact match, or — only if be was itself wildcarded — cmd starting
// with be's prefix at a word boundary, so "Bash(go test*)" matches "go test
// ./..." but "Bash(go)" (bare) matches only the literal command "go".
func (be bashEntry) matchesCommand(cmd string) bool {
	if be.prefix == cmd {
		return true
	}
	return be.wildcarded && strings.HasPrefix(cmd, be.prefix+" ")
}

// overlapsFamily reports whether be could ever authorize a command in the
// same family as target (e.g. target "git commit" against be parsed from
// "Bash(git commit -m*)" or "Bash(git *)"). Deliberately checked in both
// directions and regardless of wildcard-ness: be might be broader than
// target ("git" covers "git commit"), or a more specific command still
// squarely inside target's own family ("git commit -m" is still a commit).
// Either way be can produce a real invocation of target, so it must be
// treated as covering it — a false-positive strip here only ever removes an
// allow entry that could never have mattered; a false negative would let a
// worker commit or push.
func (be bashEntry) overlapsFamily(target string) bool {
	if be.prefix == target {
		return true
	}
	return strings.HasPrefix(be.prefix, target+" ") || strings.HasPrefix(target, be.prefix+" ")
}

// overlapsAnyFamily reports whether be overlaps any of targets (see
// overlapsFamily).
func (be bashEntry) overlapsAnyFamily(targets []string) bool {
	return slices.ContainsFunc(targets, be.overlapsFamily)
}

// bashGlobEntries renders commands (literal Bash prefixes, e.g.
// protocol.DenyFloor()'s output) as wildcarded "Bash(cmd:*)" permission
// entries — the glob form Claude Code's settings.json allow/deny lists
// expect.
func bashGlobEntries(commands []string) []string {
	entries := make([]string, len(commands))
	for i, c := range commands {
		entries[i] = "Bash(" + c + ":*)"
	}
	return entries
}

// AllowCoversCommand reports whether cmd (a literal Bash command line) is
// covered by any entry in allow — the positive-match counterpart to
// stripDenyFloor's negative one, used by the check-tool hook to decide
// whether a command falls inside the current phase's resolved allow set.
// Non-Bash-shaped entries (e.g. Edit/Write globs) never match, since they
// can't describe a Bash command line to begin with.
func AllowCoversCommand(allow []string, cmd string) bool {
	for _, entry := range allow {
		if be, ok := parseBashEntry(entry); ok && be.matchesCommand(cmd) {
			return true
		}
	}
	return false
}
