package rules

import (
	"strings"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// resurrectRefOwnerFile is the only Lua file that legitimately reads or
// writes the `_G.resurrect` / `_G.wezsesh_resurrect` globals.
// `runtime/resurrect_ref.lua` IS the resolution rule; every other call
// site duplicates it.
const resurrectRefOwnerFile = "plugin/wezsesh/runtime/resurrect_ref.lua"

// resurrectRefKeys is the set of `_G` keys that route through the
// resurrect_ref module. Both names are gated: `wezsesh_resurrect` is
// the explicit stash init.lua writes; `resurrect` is the legacy global
// some users wire up themselves.
var resurrectRefKeys = []string{"resurrect", "wezsesh_resurrect"}

// ResurrectRefOnly flags any reach into `_G.resurrect` /
// `_G.wezsesh_resurrect` outside `runtime/resurrect_ref.lua`. Patterns
// caught: `rawget(_G, "<key>")`, `rawset(_G, "<key>", …)`,
// `_G.<key>` (read or write), and `_G["<key>"]` (read or write).
//
// Why centralise the lookup: the resolution rule is a two-source check
// (explicit `set` fed by init.lua, legacy `_G.resurrect` fallback).
// Duplicating it in each consumer led to a real bug where ops.lua and
// on_pane_restore.lua had to be fixed independently when the rule
// changed. This lint catches the regression.
func ResurrectRefOnly() lualint.Rule {
	return lualint.RuleFunc{
		RuleID: "lua-resurrect-ref-only",
		Fn: func(t *lualint.Tokens) []lualint.Finding {
			if strings.HasSuffix(t.Path, resurrectRefOwnerFile) {
				return nil
			}
			var out []lualint.Finding
			for i := 0; i < len(t.All); i++ {
				if hit, ok := matchRawAccess(t, i); ok {
					out = append(out, finding(t, hit))
					continue
				}
				if hit, ok := matchDotAccess(t, i); ok {
					out = append(out, finding(t, hit))
					continue
				}
				if hit, ok := matchBracketAccess(t, i); ok {
					out = append(out, finding(t, hit))
					continue
				}
			}
			return out
		},
	}
}

// matchRawAccess catches `rawget(_G, "<key>")` and `rawset(_G, "<key>", …)`.
// Returns the token index of the function-name identifier on a hit.
func matchRawAccess(t *lualint.Tokens, i int) (int, bool) {
	tk := t.All[i]
	if tk.Kind != lualint.TokIdent {
		return 0, false
	}
	if tk.Value != "rawget" && tk.Value != "rawset" {
		return 0, false
	}
	parenIdx := t.NextNonTrivia(i)
	if parenIdx < 0 || t.All[parenIdx].Kind != lualint.TokPunct || t.All[parenIdx].Value != "(" {
		return 0, false
	}
	gIdx := t.NextNonTrivia(parenIdx)
	if gIdx < 0 || t.All[gIdx].Kind != lualint.TokIdent || t.All[gIdx].Value != "_G" {
		return 0, false
	}
	commaIdx := t.NextNonTrivia(gIdx)
	if commaIdx < 0 || t.All[commaIdx].Kind != lualint.TokPunct || t.All[commaIdx].Value != "," {
		return 0, false
	}
	keyIdx := t.NextNonTrivia(commaIdx)
	if keyIdx < 0 || t.All[keyIdx].Kind != lualint.TokString {
		return 0, false
	}
	if !stringTokenMatches(t.All[keyIdx].Value, resurrectRefKeys) {
		return 0, false
	}
	return i, true
}

// matchDotAccess catches `_G.<key>` (read or write).
func matchDotAccess(t *lualint.Tokens, i int) (int, bool) {
	tk := t.All[i]
	if tk.Kind != lualint.TokIdent || tk.Value != "_G" {
		return 0, false
	}
	dotIdx := t.NextNonTrivia(i)
	if dotIdx < 0 || t.All[dotIdx].Kind != lualint.TokPunct || t.All[dotIdx].Value != "." {
		return 0, false
	}
	keyIdx := t.NextNonTrivia(dotIdx)
	if keyIdx < 0 || t.All[keyIdx].Kind != lualint.TokIdent {
		return 0, false
	}
	if !identMatches(t.All[keyIdx].Value, resurrectRefKeys) {
		return 0, false
	}
	return i, true
}

// matchBracketAccess catches `_G["<key>"]` (read or write).
func matchBracketAccess(t *lualint.Tokens, i int) (int, bool) {
	tk := t.All[i]
	if tk.Kind != lualint.TokIdent || tk.Value != "_G" {
		return 0, false
	}
	openIdx := t.NextNonTrivia(i)
	if openIdx < 0 || t.All[openIdx].Kind != lualint.TokPunct || t.All[openIdx].Value != "[" {
		return 0, false
	}
	keyIdx := t.NextNonTrivia(openIdx)
	if keyIdx < 0 || t.All[keyIdx].Kind != lualint.TokString {
		return 0, false
	}
	if !stringTokenMatches(t.All[keyIdx].Value, resurrectRefKeys) {
		return 0, false
	}
	return i, true
}

// stringTokenMatches reports whether a Lua string literal token (with
// surrounding quotes preserved) matches any of the bare key names.
// Long-string forms (`[[…]]`) are not expected for these names and are
// not matched.
func stringTokenMatches(literal string, keys []string) bool {
	if len(literal) < 2 {
		return false
	}
	first, last := literal[0], literal[len(literal)-1]
	if (first != '"' && first != '\'') || first != last {
		return false
	}
	inner := literal[1 : len(literal)-1]
	for _, k := range keys {
		if inner == k {
			return true
		}
	}
	return false
}

// identMatches reports whether ident is one of keys.
func identMatches(ident string, keys []string) bool {
	for _, k := range keys {
		if ident == k {
			return true
		}
	}
	return false
}

// finding builds the per-hit Finding payload. The token at idx is the
// identifier that triggered the match (`rawget`/`rawset`/`_G`).
func finding(t *lualint.Tokens, idx int) lualint.Finding {
	tk := t.All[idx]
	return lualint.Finding{
		Path:     t.Path,
		Line:     tk.Line,
		Col:      tk.Col,
		RuleID:   "lua-resurrect-ref-only",
		Severity: lualint.SevError,
		Message: "_G.resurrect / _G.wezsesh_resurrect access MUST go through " +
			"plugin/wezsesh/runtime/resurrect_ref.lua (resurrect_ref.get / set)",
	}
}
