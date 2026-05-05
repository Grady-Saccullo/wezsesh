// Package nameval is the single source of truth for §15 validation
// rules. Workspace-name and tag validators (§15.1, §15.2) live here, as
// does the §15.4 render-time sanitizer and the §15.5 middle-truncate
// algorithm. Every disk-sourced string MUST pass through
// SanitizeForDisplay before being handed to lipgloss / fmt / log /
// toast / doctor output (CLAUDE.md TUI discipline).
package nameval

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
	"golang.org/x/text/unicode/norm"
)

// CodeIllegalName is the error code emitted on every §15.1 / §15.2
// failure. It rides on top of the IPC reply path's `details.code`.
const CodeIllegalName = "ILLEGAL_NAME"

// ValidationError is returned by ValidateWorkspaceName, ValidateTag,
// and ValidateTags. Field is "name" for workspace-name failures and
// "tags[i]" for the i-th tag (zero-indexed).
type ValidationError struct {
	Reason string
	Field  string
	Code   string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s (field=%s)", e.Code, e.Reason, e.Field)
}

func newErr(field, reason string) *ValidationError {
	return &ValidationError{Reason: reason, Field: field, Code: CodeIllegalName}
}

const (
	maxWorkspaceNameBytes = 200
	minTagBytes           = 1
	maxTagBytes           = 50
	minTagCount           = 1
	maxTagCount           = 10
)

// ValidateWorkspaceName runs §15.1 rules on a workspace name. The name
// is checked as raw bytes; it is the caller's responsibility to NFC-
// normalise before comparing with stored names (callers should pipe
// through NormalizeNFC at ingestion).
func ValidateWorkspaceName(name string) error {
	return validateName("name", name, maxWorkspaceNameBytes, true)
}

// ValidateTag runs §15.2 rules on a single tag. Field is reported as
// "tags[?]" — callers that know the index should use ValidateTags or
// wrap the returned error to set the index.
func ValidateTag(tag string) error {
	return validateName("tags[?]", tag, maxTagBytes, false)
}

// ValidateTags checks count (§15.2: 1..10) and each tag in turn.
// The first failure wins; the field is set to "tags[i]" with i being
// the zero-indexed offset of the offending tag.
func ValidateTags(tags []string) error {
	if len(tags) < minTagCount {
		return newErr("tags", fmt.Sprintf("at least %d tag required", minTagCount))
	}
	if len(tags) > maxTagCount {
		return newErr("tags", fmt.Sprintf("at most %d tags allowed", maxTagCount))
	}
	for i, t := range tags {
		field := fmt.Sprintf("tags[%d]", i)
		if err := validateName(field, t, maxTagBytes, false); err != nil {
			return err
		}
	}
	return nil
}

// validateName implements the byte-level checks shared by §15.1 and
// §15.2. allowDot rejects "." / ".." (workspace-name only).
func validateName(field, s string, maxBytes int, allowDot bool) error {
	if len(s) == 0 {
		return newErr(field, "must not be empty")
	}
	if len(s) > maxBytes {
		return newErr(field, fmt.Sprintf("must be %d bytes or fewer", maxBytes))
	}
	if !utf8.ValidString(s) {
		return newErr(field, "must be valid UTF-8")
	}
	if allowDot {
		if s == "." || s == ".." {
			return newErr(field, fmt.Sprintf("must not be %q", s))
		}
	}
	// Leading/trailing whitespace and all-whitespace check. unicode.IsSpace
	// covers ASCII space + \t\n\v\f\r + U+00A0 + U+1680 + the U+2000-200A
	// run + U+2028 U+2029 U+202F U+205F U+3000.
	if strings.TrimSpace(s) == "" {
		return newErr(field, "must not be all-whitespace")
	}
	first, _ := utf8.DecodeRuneInString(s)
	last, _ := utf8.DecodeLastRuneInString(s)
	if unicode.IsSpace(first) {
		return newErr(field, "must not start with whitespace")
	}
	if unicode.IsSpace(last) {
		return newErr(field, "must not end with whitespace")
	}
	for i, r := range s {
		// Byte-level: NUL, C0 (0x01-0x1F including \t\n\r), DEL.
		// §15.1/§15.2 forbid \t in names (byte rules apply).
		if r == 0x00 {
			return newErr(field, fmt.Sprintf("contains NUL byte at offset %d", i))
		}
		if r >= 0x01 && r <= 0x1F {
			return newErr(field, fmt.Sprintf("contains C0 control byte 0x%02X at offset %d", r, i))
		}
		if r == 0x7F {
			return newErr(field, fmt.Sprintf("contains DEL (0x7F) at offset %d", i))
		}
		if r >= 0x80 && r <= 0x9F {
			return newErr(field, fmt.Sprintf("contains C1 control U+%04X at offset %d", r, i))
		}
		if r == ' ' || r == ' ' {
			return newErr(field, fmt.Sprintf("contains line separator U+%04X at offset %d", r, i))
		}
		if allowDot && r == '\\' {
			return newErr(field, fmt.Sprintf("contains backslash at offset %d", i))
		}
	}
	return nil
}

// NormalizeNFC normalizes s via NFC. Apply at every name ingestion site
// (TUI input, IPC request decode, sidecar load).
func NormalizeNFC(s string) string {
	return norm.NFC.String(s)
}

// HasPlusWarning reports whether s contains a literal '+' character;
// the TUI surfaces a non-fatal warning on save/rename (§15.1 last row).
func HasPlusWarning(s string) bool {
	return strings.ContainsRune(s, '+')
}

// TruncateMiddle implements §15.5 with mode "middle". Cell width is
// measured by go-runewidth; runes (not grapheme clusters) are the
// truncation unit. If cellWidth(s) <= width the input is returned
// verbatim. If width is too small to fit the ellipsis, the result is
// truncated to a prefix that fits in `width` cells without an ellipsis.
func TruncateMiddle(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= width {
		return s
	}
	const ellipsis = "…" // U+2026 HORIZONTAL ELLIPSIS, 1 cell.
	ellW := runewidth.StringWidth(ellipsis)
	if width <= ellW {
		// No room for ellipsis — return a prefix that fits.
		return runesFromLeft(s, width)
	}
	budget := width - ellW
	prefixCells := budget / 2
	suffixCells := budget - prefixCells
	return runesFromLeft(s, prefixCells) + ellipsis + runesFromRight(s, suffixCells)
}

// runesFromLeft returns the longest prefix of s whose cell width is
// <= cells. Zero-width runes attach to the prefix as long as a non-zero
// rune has already been emitted (so combining marks aren't orphaned).
func runesFromLeft(s string, cells int) string {
	if cells <= 0 {
		return ""
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		w := runewidth.RuneWidth(r)
		if used+w > cells {
			break
		}
		b.WriteRune(r)
		used += w
	}
	return b.String()
}

// runesFromRight returns the longest suffix of s whose cell width is
// <= cells.
func runesFromRight(s string, cells int) string {
	if cells <= 0 {
		return ""
	}
	runes := []rune(s)
	used := 0
	cut := len(runes)
	for i := len(runes) - 1; i >= 0; i-- {
		w := runewidth.RuneWidth(runes[i])
		if used+w > cells {
			break
		}
		used += w
		cut = i
	}
	return string(runes[cut:])
}
