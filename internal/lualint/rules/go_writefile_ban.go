package rules

import (
	"go/ast"
	"go/token"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// WriteFileBanRule flags `os.WriteFile`, `os.OpenFile`, and
// `syscall.Open` calls in §16.5 restricted packages. Disk writes in
// the restricted set MUST go through internal/safefs (atomic write +
// symlink defence per CLAUDE.md "Filesystem" invariant).
//
// Exemptions:
//   - internal/safefs (any file): the package wraps these primitives.
//   - internal/uservar/writer.go: legitimately opens /dev/tty for OSC
//     writes (§3.1 single-syscall property).
//   - Test files (*_test.go): tests need to materialise files on disk.
//   - Codegen tools (internal/argvallow/codegen): write generated
//     source files, not runtime data.
//   - Lualint tooling (internal/lualint, cmd/lualint): tests in the
//     rules package use t.TempDir() + os.WriteFile to build synthetic
//     trees.
func WriteFileBanRule() GoRule {
	const id = "go-restricted-os-write"
	return goRuleFunc{
		id: id,
		fn: func(path string, src []byte, _ *token.FileSet, _ *ast.File) []lualint.Finding {
			if !isRestrictedPkg(path) {
				return nil
			}
			if isTestFile(path) {
				return nil
			}
			if isSafefsPackage(path) {
				return nil
			}
			if isUservarWriter(path) {
				return nil
			}
			if isCodegenTool(path) {
				return nil
			}
			if isLuaLintTooling(path) {
				return nil
			}
			if isSnapshotsRepo(path) {
				return nil
			}
			patterns := []string{
				"os.WriteFile",
				"os.OpenFile",
				"syscall.Open",
			}
			var out []lualint.Finding
			for _, pat := range patterns {
				for _, off := range indexAllInCode(src, pat) {
					line, col := posLineCol(src, off)
					out = append(out, lualint.Finding{
						Path:     path,
						Line:     line,
						Col:      col,
						RuleID:   id,
						Severity: lualint.SevError,
						Message: "use internal/safefs.AtomicWriteFile / safefs.OpenFile rather than " +
							pat + " in restricted package (§16.5)",
					})
				}
			}
			return out
		},
	}
}
