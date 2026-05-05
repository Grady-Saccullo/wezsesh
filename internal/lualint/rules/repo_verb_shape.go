package rules

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// VerbShapeParityRule asserts that the keyset of
// `M.verb_args_shape` in plugin/wezsesh/canonical_json.lua equals the
// keyset of `M.dispatch_table` in plugin/wezsesh/ops.lua. Any verb
// declared in one but missing from the other is a release-process bug
// per §17.4 (verb / shape parity).
//
// We don't try to type-check the shape declarations — only the
// presence of each verb's key. Both files are pure Lua data tables; we
// extract identifier keys at depth 1 of the constructor.
func VerbShapeParityRule() RepoShapeRule {
	return repoRuleFunc{
		id: "lua-verb-shape-parity",
		fn: func(repoRoot string) []lualint.Finding {
			canonPath := filepath.Join(repoRoot, "plugin", "wezsesh", "canonical_json.lua")
			opsPath := filepath.Join(repoRoot, "plugin", "wezsesh", "ops.lua")
			canonKeys, canonErr := extractTableKeys(canonPath, "verb_args_shape")
			opsKeys, opsErr := extractTableKeys(opsPath, "dispatch_table")
			// A missing file in a fresh tree is not a violation per se
			// — it's a build-state issue surfaced elsewhere. Emit an
			// informational warning so the check is visible without
			// breaking the build for repos that haven't reached this
			// task yet.
			if canonErr != nil {
				return []lualint.Finding{warning(canonPath,
					"lua-verb-shape-parity",
					fmt.Sprintf("could not read verb_args_shape: %v", canonErr))}
			}
			if opsErr != nil {
				return []lualint.Finding{warning(opsPath,
					"lua-verb-shape-parity",
					fmt.Sprintf("could not read dispatch_table: %v", opsErr))}
			}

			missingFromOps := sortedDiff(canonKeys, opsKeys)
			missingFromCanon := sortedDiff(opsKeys, canonKeys)
			if len(missingFromOps) == 0 && len(missingFromCanon) == 0 {
				return nil
			}
			var out []lualint.Finding
			for _, k := range missingFromOps {
				out = append(out, lualint.Finding{
					Path:     opsPath,
					Line:     1,
					Col:      1,
					RuleID:   "lua-verb-shape-parity",
					Severity: lualint.SevError,
					Message: fmt.Sprintf(
						"verb %q is declared in canonical_json.verb_args_shape but missing from ops.dispatch_table (§17.4)",
						k),
				})
			}
			for _, k := range missingFromCanon {
				out = append(out, lualint.Finding{
					Path:     canonPath,
					Line:     1,
					Col:      1,
					RuleID:   "lua-verb-shape-parity",
					Severity: lualint.SevError,
					Message: fmt.Sprintf(
						"verb %q is dispatched in ops.dispatch_table but has no canonical_json.verb_args_shape entry (§17.4)",
						k),
				})
			}
			return out
		},
	}
}

// repoRuleFunc adapts a function to the RepoShapeRule interface.
type repoRuleFunc struct {
	id string
	fn func(repoRoot string) []lualint.Finding
}

// ID returns the rule identifier.
func (r repoRuleFunc) ID() string { return r.id }

// Check delegates to the wrapped function.
func (r repoRuleFunc) Check(repoRoot string) []lualint.Finding {
	if r.fn == nil {
		return nil
	}
	return r.fn(repoRoot)
}

// warning is a tiny constructor for an informational finding pinned
// at line 1; used when the check cannot fully run (e.g., missing file
// in a partial tree).
func warning(path, ruleID, msg string) lualint.Finding {
	return lualint.Finding{
		Path: path, Line: 1, Col: 1,
		RuleID: ruleID, Severity: lualint.SevWarning,
		Message: msg,
	}
}

// extractTableKeys reads a Lua source file at path, finds the first
// `<owner>.<tableName> = { … }` (or `M.<tableName> = { … }`)
// assignment, and returns the depth-1 identifier keys of the
// constructor body.
//
// A "key" here is any token of the form `<ident> =` whose `=` sits at
// depth 1 (the rule's invariant only cares about the top-level verb
// names; nested `_shape = "object"` declarations live at depth 2 and
// are skipped).
func extractTableKeys(path, tableName string) ([]string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	toks := lualint.Tokenise(path, src)
	all := toks.All
	for i := 0; i < len(all); i++ {
		tk := all[i]
		if tk.Kind != lualint.TokIdent || tk.Value != tableName {
			continue
		}
		// Expect `= {` next (we accept either `<owner>.<tableName>` or
		// bare `<tableName>` at the LHS).
		eqIdx := toks.NextNonTrivia(i)
		if eqIdx < 0 || all[eqIdx].Kind != lualint.TokPunct || all[eqIdx].Value != "=" {
			continue
		}
		braceIdx := toks.NextNonTrivia(eqIdx)
		if braceIdx < 0 || all[braceIdx].Kind != lualint.TokPunct || all[braceIdx].Value != "{" {
			continue
		}
		return collectDepth1Keys(toks, braceIdx), nil
	}
	return nil, fmt.Errorf("table %q not found in %s", tableName, path)
}

// collectDepth1Keys walks toks starting at the `{` token at idx and
// collects every TokIdent followed by `=` at depth 1. Depth is
// tracked across nested `{ }` pairs so an inner `{ name = "string" }`
// does not contribute its `name` key.
func collectDepth1Keys(toks *lualint.Tokens, openBraceIdx int) []string {
	all := toks.All
	depth := 1
	var keys []string
	for j := openBraceIdx + 1; j < len(all); j++ {
		tk := all[j]
		if tk.Kind == lualint.TokPunct {
			switch tk.Value {
			case "{":
				depth++
				continue
			case "}":
				depth--
				if depth == 0 {
					return keys
				}
				continue
			}
		}
		if depth != 1 {
			continue
		}
		if tk.Kind != lualint.TokIdent {
			continue
		}
		next := toks.NextNonTrivia(j)
		if next < 0 {
			continue
		}
		nt := all[next]
		if nt.Kind == lualint.TokPunct && nt.Value == "=" {
			keys = append(keys, tk.Value)
		}
	}
	return keys
}

// sortedDiff returns the elements present in a but not in b, sorted
// by string order. Used to surface missing-from-ops and
// missing-from-canon disjoint sets in stable form so test output
// stays deterministic.
func sortedDiff(a, b []string) []string {
	bset := make(map[string]bool, len(b))
	for _, x := range b {
		bset[x] = true
	}
	var out []string
	for _, x := range a {
		if !bset[x] {
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}
