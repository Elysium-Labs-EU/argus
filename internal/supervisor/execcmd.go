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

// shellGlobChars are pathname-expansion metacharacters. Unlike
// shellOperatorChars they matter no matter where in the word they sit
// (`file*.txt` expands the same as `*.go`), so tokenNeedsShell checks for
// them anywhere in tok rather than only at a word edge. Falling back to sh -c
// whenever one is present is still safe for a token that was never meant as
// a glob: POSIX leaves a pattern with no filesystem match untouched, so the
// shell reproduces a direct exec's literal argument byte-for-byte in that
// case and only diverges (correctly) when the worker's own shell would also
// have expanded it.
const shellGlobChars = "*?[]"

// execArgvOrShell builds the *exec.Cmd to replay cmdStr with, preferring a
// direct argv-style exec — bypassing /bin/sh entirely — over `sh -c cmdStr`
// whenever cmdStr has no genuine shell-feature dependency (chaining,
// redirection, grouping, substitution, pathname expansion). sh -c has no way
// to tell a real shell operator apart from a worker's own metacharacter
// reported as plain argument text — a Go test -run regex alternation like
// `TestFoo|TestBar`
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
	if argv, ok := directArgv(cmdStr); ok {
		return exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // re-running the worker's own reported command, argv-style — no shell involved
	}
	return exec.CommandContext(ctx, "sh", "-c", cmdStr) //nolint:gosec // cmdStr needs real shell semantics, or its command name isn't a standalone executable
}

// directArgv returns cmdStr's word-split argv when it can run directly
// (bypassing sh -c) — see execArgvOrShell. ok is false whenever
// execArgvOrShell would fall back to a real shell instead, which
// VerifyTests uses to tell a genuine `sh -c` parse failure apart from a
// directly-executed command that happens to exit 2 on its own (see
// isShellSyntaxError).
func directArgv(cmdStr string) (argv []string, ok bool) {
	argv, split := splitShellWords(cmdStr)
	if !split || len(argv) == 0 || slices.ContainsFunc(argv, tokenNeedsShell) {
		return nil, false
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		return nil, false
	}
	return argv, true
}

// wordScanner accumulates splitShellWords' current word: the builder holding
// its bytes so far, and whether any byte (even a quoted empty string) has
// been assigned to it yet — has is what tells an empty quoted string apart
// from no word at all.
type wordScanner struct {
	cur strings.Builder
	has bool
}

// flush appends the current word to words if one is pending, and resets the
// scanner for the next word.
func (w *wordScanner) flush(words *[]string) {
	if !w.has {
		return
	}
	*words = append(*words, w.cur.String())
	w.cur.Reset()
	w.has = false
}

// consumeQuoted processes cmdStr[i], one byte inside an active quote, into
// w.cur. It returns the still-active quote byte (0 once the quote closes)
// and how many extra bytes beyond cmdStr[i] were consumed (1 for a
// recognized double-quote escape, else 0), so the caller's loop index can
// skip over them.
func (w *wordScanner) consumeQuoted(cmdStr string, i int, quote byte) (nextQuote byte, extra int) {
	c := cmdStr[i]
	if c == quote {
		return 0, 0
	}
	if quote == '"' && c == '\\' && i+1 < len(cmdStr) && (cmdStr[i+1] == '"' || cmdStr[i+1] == '\\') {
		w.cur.WriteByte(cmdStr[i+1])
		return quote, 1
	}
	w.cur.WriteByte(c)
	return quote, 0
}

func isShellQuote(c byte) bool { return c == '\'' || c == '"' }

func isShellSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// splitShellWords tokenizes cmdStr the way a POSIX shell would split it into
// words — honoring single/double quotes and backslash escapes — without
// treating any operator character as special. ok is false when cmdStr can't
// be split cleanly (an unterminated quote), so the caller falls back to a
// real shell rather than mis-split it.
func splitShellWords(cmdStr string) (words []string, ok bool) {
	var w wordScanner
	var quote byte
	for i := 0; i < len(cmdStr); i++ {
		c := cmdStr[i]
		switch {
		case quote != 0:
			var extra int
			quote, extra = w.consumeQuoted(cmdStr, i, quote)
			i += extra
		case isShellQuote(c):
			quote = c
			w.has = true
		case c == '\\' && i+1 < len(cmdStr):
			w.cur.WriteByte(cmdStr[i+1])
			w.has = true
			i++
		case isShellSpace(c):
			w.flush(&words)
		default:
			w.cur.WriteByte(c)
			w.has = true
		}
	}
	if quote != 0 {
		return nil, false
	}
	w.flush(&words)
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
	if strings.ContainsAny(tok, shellGlobChars) {
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
