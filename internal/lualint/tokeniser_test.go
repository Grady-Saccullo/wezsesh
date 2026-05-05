package lualint

import (
	"os"
	"path/filepath"
	"testing"
)

func mustReadFixture(t *testing.T, name string) []byte {
	t.Helper()
	p := filepath.Join("testdata", name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read fixture %s: %v", p, err)
	}
	return b
}

// TestShortStringEscapes verifies that escaped quotes inside short
// strings do not terminate the literal early. A regex tokeniser would
// trip on \" / \'.
func TestShortStringEscapes(t *testing.T) {
	src := mustReadFixture(t, "short_string_escapes.lua")
	toks := Tokenise("short_string_escapes.lua", src)

	want := []string{
		`"hello\"world"`,
		`'it\'s a test'`,
		`"with backslash: \\"`,
		`"tab\there"`,
	}
	got := stringTokens(toks)
	if len(got) != len(want) {
		t.Fatalf("string-token count: got %d want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("string %d: got %q want %q", i, got[i], want[i])
		}
	}

	for _, tk := range toks.All {
		if tk.Kind == TokInvalid {
			t.Fatalf("unexpected TokInvalid: %+v", tk)
		}
	}
}

// TestLongStringLevels verifies that a level-N long string is not
// terminated by a level-M closer when N != M.
func TestLongStringLevels(t *testing.T) {
	src := mustReadFixture(t, "long_string_levels.lua")
	toks := Tokenise("long_string_levels.lua", src)

	var longs []Token
	for _, tk := range toks.All {
		if tk.Kind == TokLongStr {
			longs = append(longs, tk)
		}
		if tk.Kind == TokInvalid {
			t.Fatalf("unexpected TokInvalid: %+v", tk)
		}
	}
	if len(longs) != 1 {
		t.Fatalf("long-string count: got %d want 1", len(longs))
	}
	body := longs[0].Value
	if len(body) < len("[==[]==]") {
		t.Fatalf("long string too short: %q", body)
	}
	if body[:4] != "[==[" {
		t.Errorf("long string opener: got %q want [==[", body[:4])
	}
	if body[len(body)-4:] != "]==]" {
		t.Errorf("long string closer: got %q want ]==]", body[len(body)-4:])
	}
}

// TestLongCommentPunct verifies that punctuation inside a long comment
// does not leak out as separate tokens.
func TestLongCommentPunct(t *testing.T) {
	src := mustReadFixture(t, "long_comment_punct.lua")
	toks := Tokenise("long_comment_punct.lua", src)

	var longCmts []Token
	for _, tk := range toks.All {
		if tk.Kind == TokLongCmt {
			longCmts = append(longCmts, tk)
		}
		if tk.Kind == TokInvalid {
			t.Fatalf("unexpected TokInvalid: %+v", tk)
		}
	}
	if len(longCmts) != 1 {
		t.Fatalf("long-comment count: got %d want 1", len(longCmts))
	}
	if longCmts[0].Value[:4] != "--[[" {
		t.Errorf("long comment opener: got %q want --[[", longCmts[0].Value[:4])
	}
}

// TestLineLeadingParenAmbiguous covers the §9.0.1.1 case: a line that
// starts with `(` immediately after an expression-end statement.
func TestLineLeadingParenAmbiguous(t *testing.T) {
	src := mustReadFixture(t, "lineleading_paren_ambiguous.lua")
	toks := Tokenise("lineleading_paren_ambiguous.lua", src)

	idx := findLineLeadingParen(toks)
	if idx < 0 {
		t.Fatalf("did not find a line-leading `(` token; tokens:\n%s", toks)
	}
	if !toks.IsLineLeading(idx) {
		t.Errorf("expected token %d to be line-leading", idx)
	}
	if !toks.LineLeadingParenAfterExprEnd(idx) {
		t.Errorf("expected ambiguity flag at token %d; prev=%+v", idx, toks.All[toks.PrevNonTrivia(idx)])
	}
}

// TestLineLeadingParenDisambiguated covers the negative case: a leading
// `;` separates the `(` from the previous expression and the lookback
// MUST return false.
func TestLineLeadingParenDisambiguated(t *testing.T) {
	src := mustReadFixture(t, "lineleading_paren_disambiguated.lua")
	toks := Tokenise("lineleading_paren_disambiguated.lua", src)

	idx := findLineLeadingParen(toks)
	if idx < 0 {
		t.Fatalf("did not find a line-leading `(` token; tokens:\n%s", toks)
	}
	if !toks.IsLineLeading(idx) {
		t.Errorf("expected token %d to be line-leading", idx)
	}
	if toks.LineLeadingParenAfterExprEnd(idx) {
		t.Errorf("did NOT expect ambiguity flag at token %d (leading `;` should disambiguate)", idx)
	}
}

// TestLineLeadingParenNestedCall covers the §16.5 motivation case:
// `f(g(x))\n(y)`. A regex linter would miss the ambiguity because the
// nested `)` closes the inner call rather than the outer one; the
// tokeniser sees it as a TokPunct ")" which counts as expression-end.
func TestLineLeadingParenNestedCall(t *testing.T) {
	src := mustReadFixture(t, "lineleading_paren_nested_call.lua")
	toks := Tokenise("lineleading_paren_nested_call.lua", src)

	idx := findLineLeadingParen(toks)
	if idx < 0 {
		t.Fatalf("did not find a line-leading `(` token; tokens:\n%s", toks)
	}
	if !toks.LineLeadingParenAfterExprEnd(idx) {
		t.Errorf("expected ambiguity flag at token %d; prev=%+v", idx, toks.All[toks.PrevNonTrivia(idx)])
	}
}

// TestLineLeadingParenStringTarget covers the second §16.5 motivation
// case: a string literal as the previous expression's terminator,
// e.g. `("foo"):upper()\n("bar")`. TokString must count as
// expression-end for the lookback to fire.
func TestLineLeadingParenStringTarget(t *testing.T) {
	src := mustReadFixture(t, "lineleading_paren_string_target.lua")
	toks := Tokenise("lineleading_paren_string_target.lua", src)

	idx := findLineLeadingParen(toks)
	if idx < 0 {
		t.Fatalf("did not find a line-leading `(` token; tokens:\n%s", toks)
	}
	if !toks.LineLeadingParenAfterExprEnd(idx) {
		t.Errorf("expected ambiguity flag at token %d; prev=%+v", idx, toks.All[toks.PrevNonTrivia(idx)])
	}
}

// TestUnterminatedShortStringAtEOF locks in the documented TokInvalid
// contract for an unterminated short string. Future rules that walk
// the stream rely on a TokInvalid sentinel rather than a missing token.
func TestUnterminatedShortStringAtEOF(t *testing.T) {
	toks := Tokenise("bad.lua", []byte(`local x = "no closing quote`))
	var invalids []Token
	for _, tk := range toks.All {
		if tk.Kind == TokInvalid {
			invalids = append(invalids, tk)
		}
	}
	if len(invalids) != 1 {
		t.Fatalf("invalid-token count: got %d want 1; toks:\n%s", len(invalids), toks)
	}
	if invalids[0].Value[0] != '"' {
		t.Errorf("invalid token should start at the opening quote; got %q", invalids[0].Value)
	}
}

// TestUnterminatedLongStringAtEOF locks in the contract for a long
// string whose closer is missing.
func TestUnterminatedLongStringAtEOF(t *testing.T) {
	toks := Tokenise("bad.lua", []byte(`local x = [==[no closer here`))
	var invalids []Token
	for _, tk := range toks.All {
		if tk.Kind == TokInvalid {
			invalids = append(invalids, tk)
		}
	}
	if len(invalids) != 1 {
		t.Fatalf("invalid-token count: got %d want 1; toks:\n%s", len(invalids), toks)
	}
	if invalids[0].Value[:4] != "[==[" {
		t.Errorf("invalid token should span the opener; got %q", invalids[0].Value[:4])
	}
}

// TestUnterminatedLongCommentAtEOF locks in the contract for a long
// comment whose closer is missing.
func TestUnterminatedLongCommentAtEOF(t *testing.T) {
	toks := Tokenise("bad.lua", []byte(`--[==[no closer here`))
	var invalids []Token
	for _, tk := range toks.All {
		if tk.Kind == TokInvalid {
			invalids = append(invalids, tk)
		}
	}
	if len(invalids) != 1 {
		t.Fatalf("invalid-token count: got %d want 1; toks:\n%s", len(invalids), toks)
	}
	if invalids[0].Value[:6] != "--[==[" {
		t.Errorf("invalid token should span the comment opener; got %q", invalids[0].Value[:6])
	}
}

// TestExpressionEndClassification spot-checks the IsExpressionEnd set:
// each kind of expression terminator returns true; common non-enders
// return false.
func TestExpressionEndClassification(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{"foo", true},        // ident
		{"\"s\"", true},      // short string
		{"123", true},        // number
		{"nil", true},        // keyword nil
		{"true", true},       // keyword true
		{"false", true},      // keyword false
		{"function() end", true}, // keyword end (via expression)
		{"...", true},        // varargs (parsed as a single TokPunct)
		{"+", false},         // operator
		{"then", false},      // non-expression keyword
		{",", false},         // separator
	}
	for _, tc := range cases {
		toks := Tokenise("expr.lua", []byte(tc.src))
		// Last non-trivia, non-EOF token.
		lastIdx := -1
		for i := len(toks.All) - 1; i >= 0; i-- {
			if toks.All[i].Kind == TokEOF {
				continue
			}
			if toks.All[i].Kind.IsTrivia() {
				continue
			}
			lastIdx = i
			break
		}
		if lastIdx < 0 {
			t.Errorf("%q: no non-trivia token found", tc.src)
			continue
		}
		got := toks.IsExpressionEnd(lastIdx)
		if got != tc.want {
			t.Errorf("IsExpressionEnd(%q -> %+v) = %v want %v",
				tc.src, toks.All[lastIdx], got, tc.want)
		}
	}
}

// TestEOFAlwaysPresent guarantees the tokeniser appends a TokEOF
// sentinel even on empty input.
func TestEOFAlwaysPresent(t *testing.T) {
	toks := Tokenise("empty.lua", nil)
	if len(toks.All) != 1 {
		t.Fatalf("empty: token count %d want 1; got %v", len(toks.All), toks.All)
	}
	if toks.All[0].Kind != TokEOF {
		t.Errorf("empty: kind %v want EOF", toks.All[0].Kind)
	}
}

// stringTokens returns the raw values of every TokString token.
func stringTokens(t *Tokens) []string {
	var out []string
	for _, tk := range t.All {
		if tk.Kind == TokString {
			out = append(out, tk.Value)
		}
	}
	return out
}

// findLineLeadingParen scans the token stream for the first `(` that
// IsLineLeading reports true for. Returns -1 if none.
func findLineLeadingParen(toks *Tokens) int {
	for i, tk := range toks.All {
		if tk.Kind == TokPunct && tk.Value == "(" && toks.IsLineLeading(i) {
			return i
		}
	}
	return -1
}
