package rules

import (
	"strings"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// GWeztermBan flags any `_G.wezterm` reference in a Lua file. mlua's
// sandbox does not expose wezterm globally; every submodule MUST
// acquire it via `local wezterm = require("wezterm")`. Reading
// _G.wezterm silently resolves to nil and silently puts every
// "if _G.wezterm then runtime else test-mode" branch into test-mode
// (spike #1).
func GWeztermBan() lualint.Rule {
	return lualint.RuleFunc{
		RuleID: "lua-g-wezterm",
		Fn: func(t *lualint.Tokens) []lualint.Finding {
			var out []lualint.Finding
			// Walk identifiers; flag a "wezterm" preceded by a `.` whose
			// preceding token is the `_G` identifier. The tokeniser
			// produces TokIdent for both names, so a structural walk is
			// cleaner than a regex (which would over-match in comments).
			for i := 0; i < len(t.All); i++ {
				tk := t.All[i]
				if tk.Kind != lualint.TokIdent || tk.Value != "wezterm" {
					continue
				}
				// Must be preceded by `_G.`: the previous non-trivia
				// tokens are an identifier "_G" and a "." punct.
				dotIdx := i - 1
				if dotIdx < 0 {
					continue
				}
				dot := t.All[dotIdx]
				if dot.Kind != lualint.TokPunct || dot.Value != "." {
					continue
				}
				gIdx := dotIdx - 1
				if gIdx < 0 {
					continue
				}
				g := t.All[gIdx]
				if g.Kind != lualint.TokIdent || g.Value != "_G" {
					continue
				}
				out = append(out, lualint.Finding{
					Path:     t.Path,
					Line:     g.Line,
					Col:      g.Col,
					RuleID:   "lua-g-wezterm",
					Severity: lualint.SevError,
					Message:  "_G.wezterm is unavailable in mlua sandbox; use `local wezterm = require(\"wezterm\")` (§9.0.1)",
				})
			}
			return out
		},
	}
}

// DebugBan flags any `debug.<member>` reference. The mlua sandbox
// strips the entire debug library (§9.0.1). The grep variant in §17.4
// is `\bdebug%.`; here we match TokIdent("debug") followed by a `.`,
// which the tokeniser already disambiguates from string literals and
// comments.
func DebugBan() lualint.Rule {
	return lualint.RuleFunc{
		RuleID: "lua-debug-ban",
		Fn: func(t *lualint.Tokens) []lualint.Finding {
			var out []lualint.Finding
			for i := 0; i < len(t.All); i++ {
				tk := t.All[i]
				if tk.Kind != lualint.TokIdent || tk.Value != "debug" {
					continue
				}
				next := t.NextNonTrivia(i)
				if next < 0 {
					continue
				}
				nt := t.All[next]
				if nt.Kind != lualint.TokPunct || nt.Value != "." {
					continue
				}
				out = append(out, lualint.Finding{
					Path:     t.Path,
					Line:     tk.Line,
					Col:      tk.Col,
					RuleID:   "lua-debug-ban",
					Severity: lualint.SevError,
					Message:  "debug.* is unavailable in mlua sandbox (§9.0.1)",
				})
			}
			return out
		},
	}
}

// DofileBan flags any bare `dofile(` call. The mlua sandbox strips
// dofile from apply_to_config eval (§9.0.1); plugin code uses
// `loadfile(path)()` instead.
func DofileBan() lualint.Rule {
	return lualint.RuleFunc{
		RuleID: "lua-dofile-ban",
		Fn: func(t *lualint.Tokens) []lualint.Finding {
			var out []lualint.Finding
			for i := 0; i < len(t.All); i++ {
				tk := t.All[i]
				if tk.Kind != lualint.TokIdent || tk.Value != "dofile" {
					continue
				}
				next := t.NextNonTrivia(i)
				if next < 0 {
					continue
				}
				nt := t.All[next]
				if nt.Kind != lualint.TokPunct || nt.Value != "(" {
					continue
				}
				out = append(out, lualint.Finding{
					Path:     t.Path,
					Line:     tk.Line,
					Col:      tk.Col,
					RuleID:   "lua-dofile-ban",
					Severity: lualint.SevError,
					Message:  "dofile() is unavailable in mlua sandbox; use loadfile(path)() (§9.0.1)",
				})
			}
			return out
		},
	}
}

// LineLeadingParenRule flags any line whose first non-whitespace token
// is `(` AND whose previous non-trivia token is an expression-end —
// the §9.0.1.1 ambiguity. We delegate to the existing helper on
// *Tokens which already implements the structural check.
func LineLeadingParenRule() lualint.Rule {
	return lualint.RuleFunc{
		RuleID: "lua-line-leading-paren",
		Fn: func(t *lualint.Tokens) []lualint.Finding {
			var out []lualint.Finding
			for i := 0; i < len(t.All); i++ {
				if !t.LineLeadingParenAfterExprEnd(i) {
					continue
				}
				tk := t.All[i]
				out = append(out, lualint.Finding{
					Path:     t.Path,
					Line:     tk.Line,
					Col:      tk.Col,
					RuleID:   "lua-line-leading-paren",
					Severity: lualint.SevError,
					Message:  "line-leading `(` after an expression-call statement is parsed as a chained call; capture the chunk into a local first or prefix with `;` (§9.0.1.1)",
				})
			}
			return out
		},
	}
}

// canonicalJSONFile is the path suffix the wezterm.GLOBAL nested-table
// rule exempts. Per §16.5 nested-table writes are forbidden EXCEPT in
// canonical-JSON helpers, which legitimately construct in-memory tagged
// tables before encoding them to scalar strings for storage.
const canonicalJSONFile = "plugin/wezsesh/canonical_json.lua"

// WeztermGlobalNestedTableRule flags `wezterm.GLOBAL.<path> = {` and
// `wezterm.GLOBAL[<key>] = {` patterns where the RHS is a literal
// table constructor. Per §16.5 the GLOBAL bucket only ever stores flat
// scalar values (or JSON-encoded strings); a nested-table value
// breaks the read-back path because mlua snapshots GLOBAL tables on
// every read and partial mutation is silently discarded.
//
// The §16.5 grep is `wezterm%.GLOBAL[%w_.]*%s*=%s*{`, but this misses
// bracket-indexed shapes (`wezterm.GLOBAL["foo"] = {…}`); we accept
// both to match the intent of the rule.
func WeztermGlobalNestedTableRule() lualint.Rule {
	return lualint.RuleFunc{
		RuleID: "lua-wezterm-global-nested-table",
		Fn: func(t *lualint.Tokens) []lualint.Finding {
			if strings.HasSuffix(t.Path, canonicalJSONFile) {
				return nil
			}
			var out []lualint.Finding
			for i := 0; i < len(t.All); i++ {
				tk := t.All[i]
				if tk.Kind != lualint.TokIdent || tk.Value != "wezterm" {
					continue
				}
				// Expect `.GLOBAL` next.
				dotIdx := t.NextNonTrivia(i)
				if dotIdx < 0 || t.All[dotIdx].Value != "." {
					continue
				}
				gidx := t.NextNonTrivia(dotIdx)
				if gidx < 0 || t.All[gidx].Kind != lualint.TokIdent || t.All[gidx].Value != "GLOBAL" {
					continue
				}
				// Walk past any `.<ident>` / `[<expr>]` index chain.
				cur := gidx
				for {
					nxt := t.NextNonTrivia(cur)
					if nxt < 0 {
						break
					}
					nv := t.All[nxt]
					if nv.Kind == lualint.TokPunct && nv.Value == "." {
						after := t.NextNonTrivia(nxt)
						if after < 0 || t.All[after].Kind != lualint.TokIdent {
							break
						}
						cur = after
						continue
					}
					if nv.Kind == lualint.TokPunct && nv.Value == "[" {
						// Skip until matching `]` at depth 0.
						depth := 1
						j := nxt + 1
						for ; j < len(t.All); j++ {
							v := t.All[j]
							if v.Kind != lualint.TokPunct {
								continue
							}
							if v.Value == "[" {
								depth++
							} else if v.Value == "]" {
								depth--
								if depth == 0 {
									break
								}
							}
						}
						if j >= len(t.All) {
							break
						}
						cur = j
						continue
					}
					break
				}
				// `cur` is the last token of the LHS chain. Expect `=` then `{`.
				eqIdx := t.NextNonTrivia(cur)
				if eqIdx < 0 || t.All[eqIdx].Kind != lualint.TokPunct || t.All[eqIdx].Value != "=" {
					continue
				}
				braceIdx := t.NextNonTrivia(eqIdx)
				if braceIdx < 0 || t.All[braceIdx].Kind != lualint.TokPunct || t.All[braceIdx].Value != "{" {
					continue
				}
				out = append(out, lualint.Finding{
					Path:     t.Path,
					Line:     tk.Line,
					Col:      tk.Col,
					RuleID:   "lua-wezterm-global-nested-table",
					Severity: lualint.SevError,
					Message:  "wezterm.GLOBAL stores scalars only; encode nested tables to JSON strings (§5.4 / §16.5)",
				})
			}
			return out
		},
	}
}
