package rules

import (
	"go/ast"
	"go/token"
	"path/filepath"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// FOFDSetlkRule flags any reference to `unix.F_OFD_SETLK` outside the
// single Linux build-tag file (internal/safefs/lock_linux.go) where
// the symbol is legitimately used. Per §16.5 / §17.4 this is a "build
// error" — the symbol is Linux-only and any inadvertent reference
// from cross-platform code would fail to compile on darwin.
//
// The rule is grep-shaped (the design.md row is "grep + AST walk")
// and operates on raw source bytes after stripping comments and string
// literals so that documentation prose and test descriptions don't
// trigger the gate.
func FOFDSetlkRule() GoRule {
	return goRuleFunc{
		id: "go-fofd-setlk-build-tag",
		fn: func(path string, src []byte, _ *token.FileSet, _ *ast.File) []lualint.Finding {
			if filepath.ToSlash(path) == safefsLockLinux {
				return nil
			}
			hits := indexAllInCode(src, "unix.F_OFD_SETLK")
			if len(hits) == 0 {
				return nil
			}
			var out []lualint.Finding
			for _, off := range hits {
				line, col := posLineCol(src, off)
				out = append(out, lualint.Finding{
					Path:     path,
					Line:     line,
					Col:      col,
					RuleID:   "go-fofd-setlk-build-tag",
					Severity: lualint.SevError,
					Message: "unix.F_OFD_SETLK is Linux-only and MUST live only in " +
						safefsLockLinux + " (§16.5 build-tag rule)",
				})
			}
			return out
		},
	}
}
