package rules

import (
	"go/ast"
	"go/token"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// LoggerBanRule flags `log.Println` and `fmt.Fprintln(os.Stderr, …)`
// in restricted packages. All structured logging in the runtime goes
// through internal/logger; ad-hoc stderr writes split the log stream
// and lose the §17.3 sync-flush guarantee on Warn/Error.
//
// Exemptions:
//   - internal/logger (the implementation IS the logger)
//   - internal/lualint, cmd/lualint (lint tooling needs stderr)
//   - codegen tools (CLI utilities)
//   - test files (tests print diagnostics freely)
func LoggerBanRule() GoRule {
	const id = "go-logger-ban"
	return goRuleFunc{
		id: id,
		fn: func(path string, src []byte, _ *token.FileSet, _ *ast.File) []lualint.Finding {
			if !isRestrictedPkg(path) {
				return nil
			}
			if isTestFile(path) || isLoggerPackage(path) ||
				isLuaLintTooling(path) || isCodegenTool(path) {
				return nil
			}
			var out []lualint.Finding
			for _, off := range indexAllInCode(src, "log.Println") {
				line, col := posLineCol(src, off)
				out = append(out, lualint.Finding{
					Path:     path,
					Line:     line,
					Col:      col,
					RuleID:   id,
					Severity: lualint.SevError,
					Message:  "log.Println is banned in restricted packages; use internal/logger (§16.5)",
				})
			}
			for _, off := range indexAllInCode(src, "fmt.Fprintln(os.Stderr") {
				line, col := posLineCol(src, off)
				out = append(out, lualint.Finding{
					Path:     path,
					Line:     line,
					Col:      col,
					RuleID:   id,
					Severity: lualint.SevError,
					Message:  "fmt.Fprintln(os.Stderr,…) is banned in restricted packages; use internal/logger (§16.5)",
				})
			}
			return out
		},
	}
}
