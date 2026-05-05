package rules

import (
	"go/ast"
	"go/token"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// DeferRecoverInGoroutineRule flags `go func() { ... }()` bodies in
// restricted packages whose first non-trivia statement is NOT a
// `defer` containing a recover() call. Per §16.5 every goroutine in
// the restricted set MUST surface its top-level panic via recover so
// a single misbehaving handler cannot crash the whole binary.
//
// The rule is AST-based; raw grep would over-match (any `go ` token
// followed by a function literal somewhere in the same file).
//
// Exemptions:
//   - test files
//   - internal/safefs/netfs.go (its goroutines are bounded to a single
//     syscall + channel send; see exemptions.go for the rationale)
//   - lualint tooling
//   - codegen tools
func DeferRecoverInGoroutineRule() GoRule {
	const id = "go-goroutine-recover"
	return goRuleFunc{
		id: id,
		fn: func(path string, src []byte, fset *token.FileSet, f *ast.File) []lualint.Finding {
			if !isRestrictedPkg(path) {
				return nil
			}
			if isTestFile(path) || isSafefsNetfs(path) ||
				isLuaLintTooling(path) || isCodegenTool(path) {
				return nil
			}
			if f == nil {
				return nil
			}
			var out []lualint.Finding
			ast.Inspect(f, func(n ast.Node) bool {
				gs, ok := n.(*ast.GoStmt)
				if !ok {
					return true
				}
				// We only audit `go func(){ ... }()` literal bodies.
				// `go someFn(...)` punts to the named function which is
				// audited at its own definition (and the recover, if
				// any, lives there).
				call, ok := gs.Call.Fun.(*ast.FuncLit)
				if !ok {
					return true
				}
				if hasTopLevelRecover(call.Body) {
					return true
				}
				pos := f.Package
				if call.Body != nil {
					pos = call.Body.Lbrace
				}
				line, col := resolvePos(fset, src, pos)
				out = append(out, lualint.Finding{
					Path:     path,
					Line:     line,
					Col:      col,
					RuleID:   id,
					Severity: lualint.SevError,
					Message: "goroutine in restricted package MUST start with " +
						"`defer func() { if r := recover(); r != nil { ... } }()` " +
						"(§16.5)",
				})
				return true
			})
			return out
		},
	}
}

// hasTopLevelRecover reports whether body contains, as one of its
// first few statements, a `defer` whose function literal calls
// recover(). The check is conservative — we accept any defer with a
// recover() call somewhere inside its body, anchored to the first
// three statements of the goroutine to keep the gate strict (a
// recover() buried after twenty lines of work doesn't catch panics
// from those twenty lines).
func hasTopLevelRecover(body *ast.BlockStmt) bool {
	if body == nil {
		return false
	}
	limit := len(body.List)
	if limit > 3 {
		limit = 3
	}
	for i := 0; i < limit; i++ {
		ds, ok := body.List[i].(*ast.DeferStmt)
		if !ok {
			continue
		}
		if callsRecover(ds) {
			return true
		}
	}
	return false
}

// callsRecover reports whether a defer statement's function expression
// invokes recover() somewhere in its body or directly defers it.
func callsRecover(ds *ast.DeferStmt) bool {
	// `defer recover()` — the call expression is recover() itself.
	if isRecoverCall(ds.Call) {
		return true
	}
	// `defer func() { ... recover() ... }()` — walk the literal body.
	if fn, ok := ds.Call.Fun.(*ast.FuncLit); ok && fn.Body != nil {
		found := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if found {
				return false
			}
			if c, ok := n.(*ast.CallExpr); ok && isRecoverCall(c) {
				found = true
				return false
			}
			return true
		})
		return found
	}
	return false
}

// isRecoverCall reports whether c is a call to the builtin recover().
// Imported packages cannot shadow recover within a file (it's a builtin
// identifier), so a bare ident "recover" is sufficient.
func isRecoverCall(c *ast.CallExpr) bool {
	if c == nil {
		return false
	}
	id, ok := c.Fun.(*ast.Ident)
	if !ok {
		return false
	}
	return id.Name == "recover"
}

// resolvePos resolves a token.Pos against the FileSet provided by the
// caller. If the FileSet is nil (test path that constructs an *ast.File
// without one) we fall back to recomputing line/col from raw source
// bytes via posLineCol, treating Pos as a 1-based byte offset.
func resolvePos(fset *token.FileSet, src []byte, pos token.Pos) (int, int) {
	if fset != nil && pos.IsValid() {
		p := fset.Position(pos)
		return p.Line, p.Column
	}
	off := int(pos) - 1
	if off < 0 {
		off = 0
	}
	return posLineCol(src, off)
}
