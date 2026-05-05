package rules

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// verbsDir is the directory housing per-verb modules. Files whose
// basenames begin with `_` (verbs/_deps.lua, verbs/_restore.lua) and
// the registry entry-point (verbs/init.lua) are not verbs and are
// excluded from the contract check; specs (`*_spec.lua`) too.
var verbsDir = filepath.Join("plugin", "wezsesh", "verbs")

// VerbContractRule asserts that every per-verb module under
// `plugin/wezsesh/verbs/` exports both `args_shape` (a Lua table) and
// `dispatch` (a function). The two-field contract is what allows
// `verbs/init.lua` to register modules generically: a verb missing
// either field would dispatch correctly until the canonical-JSON
// re-encode tries to look up a missing shape, at which point the
// failure surfaces as wire-silent `IPC_TIMEOUT` on the binary side.
//
// Adding a sixth verb is therefore a single-file change: drop a
// `verbs/X.lua` exposing both fields, add `M.register("X", …)` in
// `verbs/init.lua`. This rule catches the half-finished case where
// a module declares one but not the other.
//
// Replaces the previous lua-verb-shape-parity rule, which compared
// canonical_json's literal verb_args_shape table to ops.dispatch_table.
// After the refactor the source of truth is per-verb args_shape; the
// table in canonical_json is populated dynamically and a literal-table
// comparison no longer makes sense.
func VerbContractRule() RepoShapeRule {
	return repoRuleFunc{
		id: "lua-verb-contract",
		fn: func(repoRoot string) []lualint.Finding {
			dir := filepath.Join(repoRoot, verbsDir)
			entries, err := os.ReadDir(dir)
			if err != nil {
				return []lualint.Finding{warning(filepath.Join(dir),
					"lua-verb-contract",
					fmt.Sprintf("could not read verbs directory: %v", err))}
			}
			var paths []string
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() || !strings.HasSuffix(name, ".lua") {
					continue
				}
				if name == "init.lua" || strings.HasPrefix(name, "_") {
					continue
				}
				if strings.HasSuffix(name, "_spec.lua") {
					continue
				}
				paths = append(paths, filepath.Join(dir, name))
			}
			sort.Strings(paths)

			var out []lualint.Finding
			for _, p := range paths {
				out = append(out, checkVerbContract(p)...)
			}
			return out
		},
	}
}

// checkVerbContract reads one verb module and emits a finding for each
// missing exported field. The check looks for `M.args_shape = ...` and
// `M.dispatch = ...` / `function M.dispatch(...)` at module scope.
func checkVerbContract(path string) []lualint.Finding {
	src, err := os.ReadFile(path)
	if err != nil {
		return []lualint.Finding{warning(path, "lua-verb-contract",
			fmt.Sprintf("could not read verb module: %v", err))}
	}
	toks := lualint.Tokenise(path, src)
	hasShape := tableField(toks, "M", "args_shape")
	hasDispatch := tableField(toks, "M", "dispatch") ||
		moduleFunction(toks, "M", "dispatch")
	var out []lualint.Finding
	if !hasShape {
		out = append(out, lualint.Finding{
			Path: path, Line: 1, Col: 1,
			RuleID:   "lua-verb-contract",
			Severity: lualint.SevError,
			Message: "verb module MUST export `M.args_shape = { _shape = \"object\", … }`; " +
				"absent fields make the wire-protocol re-encode fail at HMAC verify time",
		})
	}
	if !hasDispatch {
		out = append(out, lualint.Finding{
			Path: path, Line: 1, Col: 1,
			RuleID:   "lua-verb-contract",
			Severity: lualint.SevError,
			Message: "verb module MUST export `M.dispatch = function(payload, window, pane) … end`",
		})
	}
	return out
}

// tableField detects `<obj>.<field> =` at module scope. Used for the
// `M.args_shape = ...` and `M.dispatch = ...` field-assignment shapes.
func tableField(toks *lualint.Tokens, obj, field string) bool {
	all := toks.All
	for i := 0; i < len(all); i++ {
		tk := all[i]
		if tk.Kind != lualint.TokIdent || tk.Value != obj {
			continue
		}
		dotIdx := toks.NextNonTrivia(i)
		if dotIdx < 0 || all[dotIdx].Kind != lualint.TokPunct || all[dotIdx].Value != "." {
			continue
		}
		fieldIdx := toks.NextNonTrivia(dotIdx)
		if fieldIdx < 0 || all[fieldIdx].Kind != lualint.TokIdent || all[fieldIdx].Value != field {
			continue
		}
		eqIdx := toks.NextNonTrivia(fieldIdx)
		if eqIdx < 0 || all[eqIdx].Kind != lualint.TokPunct || all[eqIdx].Value != "=" {
			continue
		}
		return true
	}
	return false
}

// moduleFunction detects `function <obj>.<name>(` declarations at
// module scope. Used for the `function M.dispatch(...)` shape, the
// most common way to declare a method-style function on a module
// table.
func moduleFunction(toks *lualint.Tokens, obj, name string) bool {
	all := toks.All
	for i := 0; i < len(all); i++ {
		tk := all[i]
		// `function` is classified as TokKeyword by the tokeniser
		// (reserved word), not TokIdent. Match on Value alone.
		if tk.Value != "function" {
			continue
		}
		objIdx := toks.NextNonTrivia(i)
		if objIdx < 0 || all[objIdx].Kind != lualint.TokIdent || all[objIdx].Value != obj {
			continue
		}
		dotIdx := toks.NextNonTrivia(objIdx)
		if dotIdx < 0 || all[dotIdx].Kind != lualint.TokPunct || all[dotIdx].Value != "." {
			continue
		}
		nameIdx := toks.NextNonTrivia(dotIdx)
		if nameIdx < 0 || all[nameIdx].Kind != lualint.TokIdent || all[nameIdx].Value != name {
			continue
		}
		parenIdx := toks.NextNonTrivia(nameIdx)
		if parenIdx < 0 || all[parenIdx].Kind != lualint.TokPunct || all[parenIdx].Value != "(" {
			continue
		}
		return true
	}
	return false
}
