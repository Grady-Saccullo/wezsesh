// Package lualint provides a minimal Lua tokeniser plus a rule-plugging
// API used by the wezsesh CI lint suite (§16.5, §17.4). Concrete rules
// live in their own packages and are passed to Lint as []Rule.
package lualint

import (
	"fmt"
	"strings"
)

// TokenKind enumerates the lexical categories the tokeniser produces.
// The set is deliberately small — this is a lint helper, not a parser —
// but it is rich enough to disambiguate the constructs §16.5 cares about
// (strings, comments, calls, line boundaries).
type TokenKind int

const (
	TokInvalid TokenKind = iota
	TokIdent
	TokNumber
	TokString    // short string: '...' or "..."
	TokLongStr   // long string: [[...]] or [==[...]==]
	TokLineCmt   // -- line comment (without trailing newline)
	TokLongCmt   // --[[ ... ]] long comment
	TokPunct     // any single- or multi-char operator/punctuation
	TokKeyword   // reserved word
	TokNewline   // a single '\n' (and the trailing '\n' of CRLF). '\r' alone is a Newline too.
	TokWhitespace
	TokEOF
)

// String returns a human-readable name for the kind. Used in test output
// and error messages; not load-bearing.
func (k TokenKind) String() string {
	switch k {
	case TokIdent:
		return "ident"
	case TokNumber:
		return "number"
	case TokString:
		return "string"
	case TokLongStr:
		return "longstring"
	case TokLineCmt:
		return "linecomment"
	case TokLongCmt:
		return "longcomment"
	case TokPunct:
		return "punct"
	case TokKeyword:
		return "keyword"
	case TokNewline:
		return "newline"
	case TokWhitespace:
		return "whitespace"
	case TokEOF:
		return "eof"
	}
	return "invalid"
}

// Token is a single lexical unit. Line and Col are 1-based and refer to
// the first byte of Value. EndLine / EndCol point one past the final
// byte of Value (so a one-character token at line=1,col=1 has end at
// line=1,col=2). Multi-line tokens (long strings/comments) update
// EndLine accordingly.
type Token struct {
	Kind    TokenKind
	Value   string
	Line    int
	Col     int
	EndLine int
	EndCol  int
}

// Tokens is the result of tokenising a source file. The Path field is
// preserved for downstream Findings.
type Tokens struct {
	Path string
	All  []Token
}

// luaKeywords is the Lua 5.4 reserved-word set (also valid for 5.1 / LuaJIT
// minus `goto` which Lua 5.1 lacks; lint-time over-classification of an
// identifier as a keyword is harmless because no current rule treats them
// differently).
var luaKeywords = map[string]bool{
	"and": true, "break": true, "do": true, "else": true, "elseif": true,
	"end": true, "false": true, "for": true, "function": true, "goto": true,
	"if": true, "in": true, "local": true, "nil": true, "not": true,
	"or": true, "repeat": true, "return": true, "then": true, "true": true,
	"until": true, "while": true,
}

// Tokenise converts src into a stream of tokens. It never returns an
// error; malformed input (an unterminated string, an unclosed long
// comment) produces a TokInvalid token spanning the offending region so
// downstream rules can still reason positionally. path is recorded on
// the returned Tokens for use in Findings.
func Tokenise(path string, src []byte) *Tokens {
	t := &tokeniser{src: src, line: 1, col: 1}
	out := &Tokens{Path: path}
	for {
		tok, ok := t.next()
		if !ok {
			break
		}
		out.All = append(out.All, tok)
	}
	out.All = append(out.All, Token{
		Kind: TokEOF, Line: t.line, Col: t.col, EndLine: t.line, EndCol: t.col,
	})
	return out
}

type tokeniser struct {
	src       []byte
	pos       int
	line, col int
}

func (t *tokeniser) eof() bool { return t.pos >= len(t.src) }

func (t *tokeniser) peek(off int) byte {
	if t.pos+off >= len(t.src) {
		return 0
	}
	return t.src[t.pos+off]
}

func (t *tokeniser) advance() byte {
	b := t.src[t.pos]
	t.pos++
	if b == '\n' {
		t.line++
		t.col = 1
	} else {
		t.col++
	}
	return b
}

// startTok captures the current position so callers can later assemble a
// Token covering [start..t.pos).
func (t *tokeniser) startTok() (line, col, pos int) {
	return t.line, t.col, t.pos
}

func (t *tokeniser) finishTok(kind TokenKind, line, col, startPos int) Token {
	return Token{
		Kind:    kind,
		Value:   string(t.src[startPos:t.pos]),
		Line:    line,
		Col:     col,
		EndLine: t.line,
		EndCol:  t.col,
	}
}

func (t *tokeniser) next() (Token, bool) {
	if t.eof() {
		return Token{}, false
	}
	b := t.src[t.pos]

	switch {
	case b == '\n' || b == '\r':
		line, col, start := t.startTok()
		// advance() only bumps t.line on '\n', so handle the two CR
		// cases manually: \r\n consumes both bytes; bare \r needs a
		// manual line bump so positions stay correct on classic-Mac
		// line endings.
		if b == '\r' {
			t.advance()
			if !t.eof() && t.src[t.pos] == '\n' {
				t.advance()
			} else {
				t.line++
				t.col = 1
			}
		} else {
			t.advance()
		}
		return t.finishTok(TokNewline, line, col, start), true

	case b == ' ' || b == '\t':
		line, col, start := t.startTok()
		for !t.eof() {
			c := t.src[t.pos]
			if c != ' ' && c != '\t' {
				break
			}
			t.advance()
		}
		return t.finishTok(TokWhitespace, line, col, start), true

	case b == '-' && t.peek(1) == '-':
		return t.lexComment(), true

	case b == '[':
		// Long string?  [=*[ ... ]=*]
		startLine, startCol, startPos := t.startTok()
		if level, ok := t.matchLongOpener(); ok {
			return t.lexLongBracket(level, TokLongStr, startLine, startCol, startPos), true
		}
		// Plain '[': falls through to punctuation.
		return t.lexPunct(), true

	case b == '\'' || b == '"':
		return t.lexShortString(b), true

	case isDigit(b) || (b == '.' && isDigit(t.peek(1))):
		return t.lexNumber(), true

	case isIdentStart(b):
		return t.lexIdent(), true

	default:
		return t.lexPunct(), true
	}
}

// lexComment handles both `-- line` and `--[[ long ]]` forms. The two
// leading dashes are already at t.pos.
func (t *tokeniser) lexComment() Token {
	line, col, start := t.startTok()
	t.advance() // first -
	t.advance() // second -
	// Long comment?  --[==[ ... ]==]
	if !t.eof() && t.src[t.pos] == '[' {
		// Save position; if matchLongOpener succeeds we consume the long
		// body and return a long-comment token.
		savedLine, savedCol, savedPos := t.line, t.col, t.pos
		if level, ok := t.matchLongOpener(); ok {
			body := t.lexLongBracket(level, TokLongCmt, line, col, start)
			return body
		}
		// Not a long comment after all: rewind and fall through.
		t.line, t.col, t.pos = savedLine, savedCol, savedPos
	}
	// Line comment: consume up to (but not including) end-of-line.
	for !t.eof() {
		c := t.src[t.pos]
		if c == '\n' || c == '\r' {
			break
		}
		t.advance()
	}
	return t.finishTok(TokLineCmt, line, col, start)
}

// matchLongOpener tries to consume a long-bracket opener of the form
// `[` `=`* `[` starting at t.pos. On success it returns the equals-count
// (the "level") and advances past the opener. On failure it does not
// advance and returns ok=false.
func (t *tokeniser) matchLongOpener() (int, bool) {
	if t.eof() || t.src[t.pos] != '[' {
		return 0, false
	}
	level := 0
	off := 1
	for t.pos+off < len(t.src) && t.src[t.pos+off] == '=' {
		level++
		off++
	}
	if t.pos+off >= len(t.src) || t.src[t.pos+off] != '[' {
		return 0, false
	}
	// Consume opener.
	for i := 0; i < off+1; i++ {
		t.advance()
	}
	return level, true
}

// lexLongBracket consumes the body of a long string or long comment up
// to the matching closer (`]` `=`{level} `]`). Treats EOF as an
// unterminated body and returns a TokInvalid spanning what was consumed
// so far. The startLine/startCol/startPos arguments point to the very
// first byte of the construct (the leading `[` for long strings, or the
// first `-` of `--[[…]]` for long comments) so the returned token's
// position is caller-friendly.
func (t *tokeniser) lexLongBracket(level int, kind TokenKind, startLine, startCol, startPos int) Token {
	for !t.eof() {
		if t.src[t.pos] == ']' {
			// Possible closer.
			ok := true
			off := 1
			for i := 0; i < level; i++ {
				if t.pos+off >= len(t.src) || t.src[t.pos+off] != '=' {
					ok = false
					break
				}
				off++
			}
			if ok && t.pos+off < len(t.src) && t.src[t.pos+off] == ']' {
				// Consume closer.
				for i := 0; i < off+1; i++ {
					t.advance()
				}
				return Token{
					Kind:    kind,
					Value:   string(t.src[startPos:t.pos]),
					Line:    startLine,
					Col:     startCol,
					EndLine: t.line,
					EndCol:  t.col,
				}
			}
		}
		t.advance()
	}
	// Unterminated.
	return Token{
		Kind:    TokInvalid,
		Value:   string(t.src[startPos:t.pos]),
		Line:    startLine,
		Col:     startCol,
		EndLine: t.line,
		EndCol:  t.col,
	}
}

// lexShortString handles 'foo' / "foo" with backslash escapes. It does
// not interpret escapes — it merely skips the byte after each backslash
// so an embedded quote inside an escape doesn't terminate the literal.
func (t *tokeniser) lexShortString(quote byte) Token {
	line, col, start := t.startTok()
	t.advance() // opening quote
	for !t.eof() {
		c := t.src[t.pos]
		if c == '\\' {
			t.advance()
			if t.eof() {
				return Token{
					Kind:    TokInvalid,
					Value:   string(t.src[start:t.pos]),
					Line:    line,
					Col:     col,
					EndLine: t.line,
					EndCol:  t.col,
				}
			}
			t.advance()
			continue
		}
		if c == quote {
			t.advance()
			return t.finishTok(TokString, line, col, start)
		}
		// Unescaped newline terminates a short string in Lua. Mark as
		// invalid; downstream rules can decide whether to care.
		if c == '\n' || c == '\r' {
			return Token{
				Kind:    TokInvalid,
				Value:   string(t.src[start:t.pos]),
				Line:    line,
				Col:     col,
				EndLine: t.line,
				EndCol:  t.col,
			}
		}
		t.advance()
	}
	return Token{
		Kind:    TokInvalid,
		Value:   string(t.src[start:t.pos]),
		Line:    line,
		Col:     col,
		EndLine: t.line,
		EndCol:  t.col,
	}
}

// lexNumber consumes a numeric literal. Recognises decimal / hex /
// floats / exponent forms. Does not validate; the goal is to produce a
// single Token that downstream rules can ignore.
func (t *tokeniser) lexNumber() Token {
	line, col, start := t.startTok()
	hex := false
	if t.src[t.pos] == '0' && (t.peek(1) == 'x' || t.peek(1) == 'X') {
		hex = true
		t.advance()
		t.advance()
	}
	for !t.eof() {
		c := t.src[t.pos]
		switch {
		case isDigit(c):
			t.advance()
		case hex && isHexDigit(c):
			t.advance()
		case c == '.':
			t.advance()
		case (!hex && (c == 'e' || c == 'E')) || (hex && (c == 'p' || c == 'P')):
			t.advance()
			if !t.eof() && (t.src[t.pos] == '+' || t.src[t.pos] == '-') {
				t.advance()
			}
		default:
			return t.finishTok(TokNumber, line, col, start)
		}
	}
	return t.finishTok(TokNumber, line, col, start)
}

func (t *tokeniser) lexIdent() Token {
	line, col, start := t.startTok()
	for !t.eof() {
		c := t.src[t.pos]
		if !isIdentCont(c) {
			break
		}
		t.advance()
	}
	tok := t.finishTok(TokIdent, line, col, start)
	if luaKeywords[tok.Value] {
		tok.Kind = TokKeyword
	}
	return tok
}

// lexPunct consumes one or more bytes of operator/punctuation. The
// lexer recognises Lua's multi-char operators so a rule walking the
// stream sees `..`, `==`, `::`, `>=`, `<=`, `~=`, `//`, `<<`, `>>` as
// single TokPunct tokens. Everything else is a one-byte TokPunct.
func (t *tokeniser) lexPunct() Token {
	line, col, start := t.startTok()
	b := t.src[t.pos]
	t.advance()
	switch b {
	case '.':
		// `.`, `..`, `...`
		if !t.eof() && t.src[t.pos] == '.' {
			t.advance()
			if !t.eof() && t.src[t.pos] == '.' {
				t.advance()
			}
		}
	case '=', '~', '<', '>':
		if !t.eof() && t.src[t.pos] == '=' {
			t.advance()
		} else if b == '<' && !t.eof() && t.src[t.pos] == '<' {
			t.advance()
		} else if b == '>' && !t.eof() && t.src[t.pos] == '>' {
			t.advance()
		}
	case ':':
		if !t.eof() && t.src[t.pos] == ':' {
			t.advance()
		}
	case '/':
		if !t.eof() && t.src[t.pos] == '/' {
			t.advance()
		}
	}
	return t.finishTok(TokPunct, line, col, start)
}

// --- character classes -----------------------------------------------------

func isDigit(b byte) bool    { return b >= '0' && b <= '9' }
func isHexDigit(b byte) bool { return isDigit(b) || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F') }
func isIdentStart(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
func isIdentCont(b byte) bool { return isIdentStart(b) || isDigit(b) }

// --- helpers exposed to rules ---------------------------------------------

// IsTrivia reports whether a token is whitespace, a newline, or a
// comment — the categories rules typically want to skip when reasoning
// about statement structure.
func (k TokenKind) IsTrivia() bool {
	switch k {
	case TokWhitespace, TokNewline, TokLineCmt, TokLongCmt:
		return true
	}
	return false
}

// PrevNonTrivia returns the index in t.All of the most recent non-trivia
// token strictly before idx, or -1 if none exists.
func (t *Tokens) PrevNonTrivia(idx int) int {
	for i := idx - 1; i >= 0; i-- {
		if !t.All[i].Kind.IsTrivia() {
			return i
		}
	}
	return -1
}

// NextNonTrivia returns the index in t.All of the next non-trivia token
// strictly after idx, or -1 if none exists.
func (t *Tokens) NextNonTrivia(idx int) int {
	for i := idx + 1; i < len(t.All); i++ {
		if !t.All[i].Kind.IsTrivia() {
			return i
		}
	}
	return -1
}

// IsLineLeading reports whether the token at idx is the first
// non-whitespace token on its line. Comments DO NOT count as
// line-leading; an inline `--foo` followed by `(` is still flagged as a
// line-leading `(`.
func (t *Tokens) IsLineLeading(idx int) bool {
	if idx < 0 || idx >= len(t.All) {
		return false
	}
	for i := idx - 1; i >= 0; i-- {
		prev := t.All[i]
		if prev.Kind == TokNewline {
			return true
		}
		if prev.Kind == TokWhitespace {
			continue
		}
		// Comments and any other token mean we are NOT line-leading.
		return false
	}
	// Start of file counts as line-leading.
	return true
}

// IsExpressionEnd reports whether the token at idx could syntactically
// be the final token of a Lua expression. Per §9.0.1.1 the set that
// matters for the line-leading-`(` ambiguity is:
//
//   - `)`     — end of a parenthesised expression or call's arglist
//   - `]`     — end of an indexed expression (and end of a long string)
//   - identifier — bare name reference (callable or value)
//   - string literal — both short and long forms (callable as `s:m()`)
//   - number literal
//   - keywords `nil`, `true`, `false`, `end`  (`end` closes a function
//     literal; the result is callable)
//   - `...`   — varargs
//
// Operators, `then`, `do`, `;`, `,`, opening punctuation, and trivia all
// return false: a `(` immediately after them is unambiguous.
func (t *Tokens) IsExpressionEnd(idx int) bool {
	if idx < 0 || idx >= len(t.All) {
		return false
	}
	tok := t.All[idx]
	switch tok.Kind {
	case TokIdent:
		return true
	case TokString, TokLongStr, TokNumber:
		return true
	case TokKeyword:
		switch tok.Value {
		case "nil", "true", "false", "end":
			return true
		}
		return false
	case TokPunct:
		switch tok.Value {
		case ")", "]", "...":
			return true
		}
		return false
	}
	return false
}

// LineLeadingParenAfterExprEnd reports whether the token at idx is a
// line-leading `(` whose previous non-trivia token is an
// expression-end. This is the §9.0.1.1 ambiguity check: when true, Lua
// will glue the `(` onto the previous statement as a function-call
// continuation.
//
// Callers wanting to suppress the diagnostic on lines explicitly
// disambiguated with a leading `;` get that for free: if the user wrote
// `; (foo)`, the previous non-trivia token is `;` (TokPunct, not in the
// IsExpressionEnd set) and the function returns false.
func (t *Tokens) LineLeadingParenAfterExprEnd(idx int) bool {
	if idx < 0 || idx >= len(t.All) {
		return false
	}
	tok := t.All[idx]
	if tok.Kind != TokPunct || tok.Value != "(" {
		return false
	}
	if !t.IsLineLeading(idx) {
		return false
	}
	prev := t.PrevNonTrivia(idx)
	if prev < 0 {
		return false
	}
	return t.IsExpressionEnd(prev)
}

// String returns a debug dump of the token stream — one token per line
// with positions. Used by tests; not load-bearing.
func (t *Tokens) String() string {
	var b strings.Builder
	for i, tok := range t.All {
		fmt.Fprintf(&b, "%d\t%s\t%d:%d\t%q\n", i, tok.Kind, tok.Line, tok.Col, tok.Value)
	}
	return b.String()
}
