package nameval

import (
	"strings"
	"unicode/utf8"
)

// SanitizeForDisplay implements §15.4. Every byte in 0x00-0x1F (except
// \t = 0x09) and 0x7F is replaced with U+FFFD. Valid-UTF-8
// representations of the C1 controls U+0080-U+009F (encoded as the
// two-byte sequences 0xC2 0x80 ... 0xC2 0x9F) are also replaced. So
// are U+2028 LINE SEPARATOR and U+2029 PARAGRAPH SEPARATOR — both
// terminate logical lines on common renderers and would be confusing
// inside a TUI row. Every render of a disk-sourced string MUST go
// through this function (CLAUDE.md).
//
// Invalid UTF-8 byte sequences are also replaced with U+FFFD on a
// per-byte basis, so the function is total: it returns valid UTF-8
// for any input.
func SanitizeForDisplay(s string) string {
	// Fast path: scan for any byte that needs replacing. If none, the
	// input is reusable verbatim.
	if !needsSanitize(s) {
		return s
	}
	const repl = "�"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		// ASCII control range — single-byte fast path.
		if c < 0x80 {
			if c == '\t' {
				b.WriteByte(c)
				i++
				continue
			}
			if c < 0x20 || c == 0x7F {
				b.WriteString(repl)
				i++
				continue
			}
			b.WriteByte(c)
			i++
			continue
		}
		// Non-ASCII: decode one rune and decide.
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			// Invalid UTF-8 byte; replace.
			b.WriteString(repl)
			i++
			continue
		}
		if (r >= 0x80 && r <= 0x9F) || r == 0x2028 || r == 0x2029 {
			b.WriteString(repl)
			i += size
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String()
}

// needsSanitize is a byte scan that bails out on the first character
// that SanitizeForDisplay would replace. Most disk-sourced strings are
// already clean; this avoids the per-rune decode + Builder allocation
// for the common case.
func needsSanitize(s string) bool {
	for i := 0; i < len(s); {
		c := s[i]
		if c < 0x80 {
			if c == '\t' {
				i++
				continue
			}
			if c < 0x20 || c == 0x7F {
				return true
			}
			i++
			continue
		}
		// 0xC2 0x80..0x9F = U+0080..U+009F (C1 controls).
		if c == 0xC2 && i+1 < len(s) {
			n := s[i+1]
			if n >= 0x80 && n <= 0x9F {
				return true
			}
		}
		// 0xE2 0x80 0xA8 = U+2028; 0xE2 0x80 0xA9 = U+2029.
		if c == 0xE2 && i+2 < len(s) && s[i+1] == 0x80 {
			n := s[i+2]
			if n == 0xA8 || n == 0xA9 {
				return true
			}
		}
		// Detect invalid UTF-8 — DecodeRuneInString returns size==1 on bad
		// bytes. Skip the cheap detector and fall through to advance.
		_, size := utf8.DecodeRuneInString(s[i:])
		if size == 1 && c >= 0x80 {
			return true
		}
		i += size
	}
	return false
}
