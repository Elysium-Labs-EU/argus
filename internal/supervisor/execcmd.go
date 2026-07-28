package supervisor

import (
	"context"
	"os/exec"
	"slices"
	"strings"
)

// shellOperatorChars are the characters that only mean something as shell
// syntax (chaining, redirection, grouping) — never as plain argument text.
const shellOperatorChars = "|&;<>(){}"

// execArgvOrShell builds the *exec.Cmd to replay cmdStr with, preferring a
// direct argv-style exec — bypassing /bin/sh entirely — over `sh -c cmdStr`
// whenever cmdStr has no genuine shell-feature dependency (chaining,
// redirection, grouping, substitution). sh -c has no way to tell a real
// shell operator apart from a worker's own metacharacter reported as plain
// argument text — a Go test -run regex alternation like `TestFoo|TestBar`
// is the observed case: unquoted, sh -c misreads it as a pipeline and splits
// it into two bogus commands (`go test -run TestFoo` piped into a
// nonexistent `TestBar ./...`), even though the worker never ran it through
// a shell at all. Argv-style exec sidesteps the ambiguity entirely by never
// asking anything to reinterpret cmdStr as shell syntax.
//
// Falls back to `sh -c cmdStr` when cmdStr can't be word-split cleanly (an
// unterminated quote), when any word carries a genuine shell feature, or
// when the resulting argv's command name isn't a real executable on PATH
// (e.g. a shell builtin like `exit`, or a multi-statement `f() { ...; }; f`
// reported as a single Cmd).
func execArgvOrShell(ctx context.Context, cmdStr string) *exec.Cmd {
	if argv, ok := splitShellWords(cmdStr); ok && len(argv) > 0 && !slices.ContainsFunc(argv, tokenNeedsShell) {
		if _, err := exec.LookPath(argv[0]); err == nil {
			return exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // re-running the worker's own reported command, argv-style — no shell involved
		}
	}
	return exec.CommandContext(ctx, "sh", "-c", cmdStr) //nolint:gosec // cmdStr needs real shell semantics, or its command name isn't a standalone executable
}

// splitShellWords tokenizes cmdStr the way a POSIX shell would split it into
// words — honoring single/double quotes and backslash escapes — without
// treating any operator character as special. ok is false when cmdStr can't
// be split cleanly (an unterminated quote), so the caller falls back to a
// real shell rather than mis-split it.
func splitShellWords(cmdStr string) (words []string, ok bool) {
	var cur strings.Builder
	var quote byte
	has := false
	for i := 0; i < len(cmdStr); i++ {
		c := cmdStr[i]
		switch {
		case quote != 0:
			switch {
			case c == quote:
				quote = 0
			case quote == '"' && c == '\\' && i+1 < len(cmdStr) && (cmdStr[i+1] == '"' || cmdStr[i+1] == '\\'):
				i++
				cur.WriteByte(cmdStr[i])
			default:
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote = c
			has = true
		case c == '\\' && i+1 < len(cmdStr):
			i++
			cur.WriteByte(cmdStr[i])
			has = true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if has {
				words = append(words, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteByte(c)
			has = true
		}
	}
	if quote != 0 {
		return nil, false
	}
	if has {
		words = append(words, cur.String())
	}
	return words, true
}

// tokenNeedsShell reports whether tok, one already-whitespace-split word
// from a worker's reported command, requires a real shell to run correctly.
// An operator character is only trusted as an operator when it borders the
// edge of its word — the way `cmd1 && cmd2` or `echo hi;` are actually
// written — so a metacharacter embedded strictly inside a word (the
// `TestFoo|TestBar` case: no shell in sight when the worker actually ran it)
// is left as ordinary argument text instead of being mistaken for shell
// syntax the way a bare `sh -c` replay always did.
func tokenNeedsShell(tok string) bool {
	if tok == "" {
		return false
	}
	if strings.ContainsRune(tok, '`') || strings.Contains(tok, "$(") || strings.Contains(tok, "${") || hasShellVarRef(tok) {
		return true
	}
	first := strings.IndexAny(tok, shellOperatorChars)
	if first < 0 {
		return false
	}
	if first == 0 {
		return true
	}
	return strings.LastIndexAny(tok, shellOperatorChars) == len(tok)-1
}

// hasShellVarRef reports whether tok references a shell variable by name
// (`$FOO`, `$_foo`) — deliberately excluding a bare trailing `$`, which is
// far more likely to be a regex end-of-string anchor (`TestFoo$`) than an
// intentional, truncated variable reference.
func hasShellVarRef(tok string) bool {
	for i := 0; i < len(tok)-1; i++ {
		if tok[i] != '$' {
			continue
		}
		c := tok[i+1]
		if c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') {
			return true
		}
	}
	return false
}
