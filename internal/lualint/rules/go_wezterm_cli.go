package rules

import (
	"go/ast"
	"go/token"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// WeztermCLIRule flags `exec.Command(..., "wezterm", ...)` /
// `exec.CommandContext(..., "wezterm", ...)` outside internal/wezcli.
// Per §16.5 every wezterm-CLI invocation MUST flow through that
// package so the project-wide CI lint can guard the boundary at one
// place.
//
// Implementation notes:
//   - The exec.Command first arg is positional; CommandContext takes
//     a context arg first. We accept either shape and look for a
//     literal "wezterm" string anywhere in the call's textual extent.
//   - Pure grep would over-match (e.g. comment text "exec.Command for
//     wezterm"); we strip comments / strings via goSpans first, then
//     pair an `exec.Command` token with a "wezterm" literal on the
//     same line (or two-line span for line-broken arg lists).
func WeztermCLIRule() GoRule {
	const id = "go-wezterm-cli-boundary"
	return goRuleFunc{
		id: id,
		fn: func(path string, src []byte, _ *token.FileSet, _ *ast.File) []lualint.Finding {
			if isWezcliPackage(path) {
				return nil
			}
			if isTestFile(path) {
				return nil
			}
			// Look for `exec.Command` or `exec.CommandContext` and
			// require a literal "wezterm" inside the call's argument
			// list. We bound the search to the next line break-counted
			// span (200 bytes) which covers single-line and the rare
			// multi-line argv assembly.
			var out []lualint.Finding
			needles := []string{"exec.Command(", "exec.CommandContext("}
			for _, needle := range needles {
				for _, off := range indexAllInCode(src, needle) {
					end := off + 200
					if end > len(src) {
						end = len(src)
					}
					window := src[off:end]
					if !containsLiteralWezterm(window) {
						continue
					}
					line, col := posLineCol(src, off)
					out = append(out, lualint.Finding{
						Path:     path,
						Line:     line,
						Col:      col,
						RuleID:   id,
						Severity: lualint.SevError,
						Message: "wezterm-CLI invocation MUST live inside internal/wezcli; " +
							"call wezcli.Client methods instead (§16.5)",
					})
				}
			}
			return out
		},
	}
}

// containsLiteralWezterm reports whether window contains a Go string
// literal whose contents are exactly "wezterm". The window is raw
// source bytes; we scan for the byte sequence "\"wezterm\"" or
// `\x60wezterm\x60` (back-quoted raw string) so a method named
// WeztermCLI doesn't flag.
func containsLiteralWezterm(window []byte) bool {
	patterns := [][]byte{
		[]byte(`"wezterm"`),
		[]byte("`wezterm`"),
	}
	for _, pat := range patterns {
		if indexBytes(window, pat) >= 0 {
			return true
		}
	}
	return false
}

// indexBytes is bytes.Index without the import. Tiny helper kept here
// to avoid dragging the bytes package into a tight grep loop.
func indexBytes(haystack, needle []byte) int {
	if len(needle) == 0 {
		return 0
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
