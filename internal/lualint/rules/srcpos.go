package rules

// posLineCol returns the 1-based line and column of byte offset off in
// src. An offset off ≥ len(src) clamps to the line/col one past the
// final byte (matching the convention used elsewhere in lualint).
func posLineCol(src []byte, off int) (int, int) {
	if off < 0 {
		off = 0
	}
	if off > len(src) {
		off = len(src)
	}
	line := 1
	col := 1
	for i := 0; i < off; i++ {
		if src[i] == '\n' {
			line++
			col = 1
			continue
		}
		col++
	}
	return line, col
}

// goLineKind is the lexical state used by stripGoTrivia to decide
// whether bytes at a given position are "live code" or sit inside a
// comment / string literal. The grep-style Go rules only care about
// matches outside of comments and strings; inside, the symbols are
// either documentation or test fixtures and don't represent actual
// program behaviour.
type goLineKind int

const (
	goCode goLineKind = iota
	goLineComment
	goBlockComment
	goString    // "..."
	goRawString // `...`
	goChar      // '...'
)

// goSpans walks src once and produces a slice the same length as src
// where each index records whether that byte is part of code (true)
// or part of a comment / string (false). Used by the grep-style rules
// to suppress false positives in doc comments and example strings.
func goSpans(src []byte) []bool {
	out := make([]bool, len(src))
	state := goCode
	for i := 0; i < len(src); i++ {
		switch state {
		case goCode:
			if i+1 < len(src) && src[i] == '/' && src[i+1] == '/' {
				state = goLineComment
				i++ // skip the second '/'
				continue
			}
			if i+1 < len(src) && src[i] == '/' && src[i+1] == '*' {
				state = goBlockComment
				i++ // skip the '*'
				continue
			}
			if src[i] == '"' {
				out[i] = true
				state = goString
				continue
			}
			if src[i] == '`' {
				out[i] = true
				state = goRawString
				continue
			}
			if src[i] == '\'' {
				out[i] = true
				state = goChar
				continue
			}
			out[i] = true
		case goLineComment:
			if src[i] == '\n' {
				state = goCode
				out[i] = true // newline counts as code-territory for line accounting
			}
		case goBlockComment:
			if i+1 < len(src) && src[i] == '*' && src[i+1] == '/' {
				i++
				state = goCode
			}
		case goString:
			if src[i] == '\\' && i+1 < len(src) {
				i++ // skip escaped byte
				continue
			}
			if src[i] == '"' || src[i] == '\n' {
				state = goCode
			}
		case goRawString:
			if src[i] == '`' {
				state = goCode
			}
		case goChar:
			if src[i] == '\\' && i+1 < len(src) {
				i++
				continue
			}
			if src[i] == '\'' {
				state = goCode
			}
		}
	}
	return out
}

// indexAllInCode finds every byte offset in src where pattern occurs
// AND that offset is classified as code (not a comment, not inside a
// string literal). Returns offsets in source order.
func indexAllInCode(src []byte, pattern string) []int {
	if len(pattern) == 0 || len(src) == 0 {
		return nil
	}
	spans := goSpans(src)
	var hits []int
	pb := []byte(pattern)
	for i := 0; i+len(pb) <= len(src); i++ {
		if !spans[i] {
			continue
		}
		match := true
		for j := 0; j < len(pb); j++ {
			if src[i+j] != pb[j] {
				match = false
				break
			}
		}
		if match {
			hits = append(hits, i)
			i += len(pb) - 1
		}
	}
	return hits
}
