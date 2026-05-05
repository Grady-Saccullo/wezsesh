package rules

import (
	"go/ast"
	"go/token"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// TeaAfterRule flags any reference to `tea.After`. The bubbletea API
// has no After function — only tea.Tick — and a tea.After callsite is
// either a typo or a ported pattern from a non-bubbletea library.
// Per §16.5 / §17.4 this is a "build error".
func TeaAfterRule() GoRule {
	const id = "go-tea-after"
	return goRuleFunc{
		id: id,
		fn: func(path string, src []byte, _ *token.FileSet, _ *ast.File) []lualint.Finding {
			if isTestFile(path) {
				return nil
			}
			var out []lualint.Finding
			for _, off := range indexAllInCode(src, "tea.After") {
				line, col := posLineCol(src, off)
				out = append(out, lualint.Finding{
					Path:     path,
					Line:     line,
					Col:      col,
					RuleID:   id,
					Severity: lualint.SevError,
					Message:  "tea.After does not exist in any released bubbletea version; use tea.Tick (CLAUDE.md TUI discipline)",
				})
			}
			return out
		},
	}
}
