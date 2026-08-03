package supervisor

import "testing"

func TestSplitShellWordsHonorsQuotesAndEscapes(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"go test -run TestFoo|TestBar ./...", []string{"go", "test", "-run", "TestFoo|TestBar", "./..."}},
		{`echo "a b" c`, []string{"echo", "a b", "c"}},
		{`echo 'a b' c`, []string{"echo", "a b", "c"}},
		{`echo foo\ bar`, []string{"echo", "foo bar"}},
		{"  make   check  ", []string{"make", "check"}},
		{`echo "say \"hi\""`, []string{"echo", `say "hi"`}},
		{`echo ''`, []string{"echo", ""}},
		{"", nil},
	}
	for _, tc := range cases {
		got, ok := splitShellWords(tc.in)
		if !ok {
			t.Errorf("splitShellWords(%q) reported unterminated quote, want ok", tc.in)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("splitShellWords(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitShellWords(%q) = %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}
}

func TestSplitShellWordsRejectsUnterminatedQuote(t *testing.T) {
	if _, ok := splitShellWords(`echo "unterminated`); ok {
		t.Error("splitShellWords with an unterminated quote should report ok=false")
	}
}

func TestTokenNeedsShell(t *testing.T) {
	cases := []struct {
		tok  string
		want bool
	}{
		{"TestFoo|TestBar", false}, // regex alternation embedded mid-word: not shell syntax
		{"TestFoo$", false},        // regex end anchor, not a shell var reference
		{"./...", false},
		{"-run", false},
		{"|", true},                 // bare pipe operator
		{"&&", true},                // bare logical-and operator
		{"boom-output-here;", true}, // trailing operator, e.g. `echo boom;`
		{"f()", true},               // trailing paren: function definition
		{"{", true},
		{"}", true},
		{"$HOME", true},        // genuine variable reference
		{"$(pwd)", true},       // command substitution
		{"${HOME}", true},      // parameter expansion
		{"`pwd`", true},        // backtick substitution
		{"*.go", true},         // leading glob: `golangci-lint fmt *.go`
		{"file*.txt", true},    // glob embedded mid-word, still needs real expansion
		{"test[0-9].go", true}, // bracket glob
		{"a?.go", true},        // single-char glob
	}
	for _, tc := range cases {
		if got := tokenNeedsShell(tc.tok); got != tc.want {
			t.Errorf("tokenNeedsShell(%q) = %v, want %v", tc.tok, got, tc.want)
		}
	}
}
