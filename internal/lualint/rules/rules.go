// Package rules implements the concrete CI lints declared in §16.5
// (custom CI lints) and §17.4 (CI lint suite). The rules split into
// two surfaces:
//
//   - GoRule: lints that operate on Go source files (raw bytes plus an
//     optional *ast.File). The cmd/lualint driver discovers .go files
//     under "restricted packages" (see §16.5) and runs each GoRule on
//     them.
//   - lualint.Rule: existing T-003 contract, lints that operate on a
//     tokenised Lua file.
//
// In addition to per-file rules there are repo-shape rules that read
// specific files at known paths (e.g. the verb_args_shape parity check
// against canonical_json.lua + ops.lua, and the package.loaded bust-loop
// presence check against init.lua). These run once per cmd/lualint
// invocation rather than per-file.
//
// All rule files in this package are deliberately small and self-
// describing: each rule's invariant is keyed to a §-citation in design.md,
// and the test suite (rules/<name>_test.go) carries a positive and
// negative inline fixture.
package rules

import (
	"go/ast"
	"go/token"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// GoRule is the per-file Go contract. ID is a stable identifier reused
// in Findings and the --rule= filter. Check is invoked once per .go
// file with the file's path (relative to repo root), the raw source
// bytes, and an optional parsed *ast.File. A rule that needs the AST
// will receive it; a rule that only needs grep semantics can ignore it.
//
// f and fset may be nil if parsing failed; rules MUST gracefully
// handle that. Returning a nil/empty slice means "this file is clean
// for this rule".
type GoRule interface {
	ID() string
	Check(path string, src []byte, fset *token.FileSet, f *ast.File) []lualint.Finding
}

// goRuleFunc adapts a function to the GoRule interface. Used by tests
// and to keep rule files compact (one rule per file with a small
// constructor instead of a struct + method).
type goRuleFunc struct {
	id string
	fn func(path string, src []byte, fset *token.FileSet, f *ast.File) []lualint.Finding
}

// ID returns the rule identifier.
func (r goRuleFunc) ID() string { return r.id }

// Check delegates to the wrapped function.
func (r goRuleFunc) Check(path string, src []byte, fset *token.FileSet, f *ast.File) []lualint.Finding {
	if r.fn == nil {
		return nil
	}
	return r.fn(path, src, fset, f)
}

// AllGoRules returns the curated set of Go-targeting rules, in the
// order they appear in §16.5. The ordering is informational only —
// findings from different rules may interleave at the driver level.
func AllGoRules() []GoRule {
	return []GoRule{
		FOFDSetlkRule(),
		WriteFileBanRule(),
		WeztermCLIRule(),
		StartListenerRule(),
		TeaAfterRule(),
		DeferRecoverInGoroutineRule(),
		LoggerBanRule(),
	}
}

// AllLuaRules returns the curated set of Lua-targeting rules. The
// repo-shape rules (verb_args_shape parity, init.lua bust-loop /
// resurrect_error.register presence) are returned separately by
// AllRepoRules — they don't fit the per-file iteration model.
func AllLuaRules() []lualint.Rule {
	return []lualint.Rule{
		GWeztermBan(),
		DebugBan(),
		DofileBan(),
		LineLeadingParenRule(),
		WeztermGlobalNestedTableRule(),
		ResurrectErrorOutsideOwner(),
		RestoreFinishedBan(),
		BarePcallSaveStateRule(),
		BarePcallLoadStateRule(),
		BackgroundChildProcessPcallRule(),
	}
}

// RepoShapeRule is the contract for rules that look at the whole repo
// at once rather than at one file at a time. They're invoked once per
// cmd/lualint run with the repo root path and emit Findings against
// whichever specific files they care about.
type RepoShapeRule interface {
	ID() string
	Check(repoRoot string) []lualint.Finding
}

// AllRepoRules returns the repo-shape rules: verb_args_shape parity
// and the init.lua presence checks (bust loop + resurrect_error.register).
func AllRepoRules() []RepoShapeRule {
	return []RepoShapeRule{
		VerbShapeParityRule(),
		PackageLoadedBustLoopRule(),
		ResurrectErrorRegisterPresenceRule(),
	}
}
