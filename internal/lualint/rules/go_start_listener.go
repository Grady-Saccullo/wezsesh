package rules

import (
	"go/ast"
	"go/token"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// StartListenerRule flags `ipcsock.StartListener` callsites outside
// internal/ipcdispatcher. The concrete Dispatcher MUST live in that
// package only (CLAUDE.md "Concrete Dispatcher" invariant); other
// callers go through the dispatcher.Dispatch / dispatcher interface.
func StartListenerRule() GoRule {
	const id = "go-start-listener-boundary"
	return goRuleFunc{
		id: id,
		fn: func(path string, src []byte, _ *token.FileSet, _ *ast.File) []lualint.Finding {
			if isIpcdispatcherPackage(path) {
				return nil
			}
			if isE2EHarness(path) {
				return nil
			}
			// Test files outside ipcdispatcher / e2e would still be a
			// boundary violation: §16.5 makes no exemption for unit
			// tests here, and a unit test that wires its own listener
			// leaks the concrete socket lifecycle into a non-dispatcher
			// caller. Production code in ipcsock itself names the
			// function in docstrings only; goSpans strips comments
			// before matching.
			var out []lualint.Finding
			for _, off := range indexAllInCode(src, "ipcsock.StartListener") {
				line, col := posLineCol(src, off)
				out = append(out, lualint.Finding{
					Path:     path,
					Line:     line,
					Col:      col,
					RuleID:   id,
					Severity: lualint.SevError,
					Message: "ipcsock.StartListener callers MUST live in internal/ipcdispatcher " +
						"(§16.5 concrete-Dispatcher rule)",
				})
			}
			return out
		},
	}
}
