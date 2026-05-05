package rules

import (
	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// BarePcallSaveStateRule flags `pcall(<...>state_manager.save_state, …)`
// callsites that are NOT lexically enclosed by a
// `resurrect_error.with_capture(...)` call. Per spike #2 / §17.4 the
// bare pcall form silently misses the I/O failure path because
// resurrect.state_manager.save_state swallows I/O / encryption errors
// into a `wezterm.emit("resurrect.error", string)` rather than raising
// — `with_capture` is the side-channel buffer that observes them.
func BarePcallSaveStateRule() lualint.Rule {
	return lualint.RuleFunc{
		RuleID: "lua-bare-pcall-save-state",
		Fn: func(t *lualint.Tokens) []lualint.Finding {
			return findBarePcall(t, "save_state",
				"lua-bare-pcall-save-state",
				"pcall(<...>state_manager.save_state, …) silently misses the I/O failure path; wrap in resurrect_error.with_capture (spike-#2 / §17.4)")
		},
	}
}

// BarePcallLoadStateRule is the load_state mirror of
// BarePcallSaveStateRule. Same shape: silent decrypt failures escape a
// bare pcall, only the with_capture buffer observes them.
func BarePcallLoadStateRule() lualint.Rule {
	return lualint.RuleFunc{
		RuleID: "lua-bare-pcall-load-state",
		Fn: func(t *lualint.Tokens) []lualint.Finding {
			return findBarePcall(t, "load_state",
				"lua-bare-pcall-load-state",
				"pcall(<...>state_manager.load_state, …) silently misses the decrypt failure path; wrap in resurrect_error.with_capture (spike-#2 / §17.4)")
		},
	}
}

// findBarePcall walks the token stream for `pcall(<chain>.<member>(…)`
// or `pcall(<chain>.<member>, …)` shapes where <member> is the given
// memberName (typically "save_state" or "load_state") AND the
// preceding chain ends in `state_manager`. A finding is emitted iff
// the call is NOT lexically inside a
// `resurrect_error.with_capture(...)` enclosing call (walked back via
// the token-paren-depth ledger).
//
// The walk is intentionally lightweight — we don't reconstruct a
// proper Lua AST. The relevant shape is `pcall(...state_manager.M…`
// which is unambiguous in the grep table of §17.4 once we know we're
// on a TokIdent("pcall") followed by `(` and an arg-list whose first
// token chain matches.
func findBarePcall(t *lualint.Tokens, memberName, ruleID, message string) []lualint.Finding {
	var out []lualint.Finding
	enclosing := captureEnclosingMap(t)

	for i := 0; i < len(t.All); i++ {
		tk := t.All[i]
		if tk.Kind != lualint.TokIdent || tk.Value != "pcall" {
			continue
		}
		paren := t.NextNonTrivia(i)
		if paren < 0 || t.All[paren].Kind != lualint.TokPunct || t.All[paren].Value != "(" {
			continue
		}
		// Walk the dotted chain that follows. Accept `<id>(.<id>)*`
		// then require the last identifier == memberName and the
		// preceding identifier == "state_manager".
		j := t.NextNonTrivia(paren)
		var chain []string
		var firstIdx = -1
		for j >= 0 {
			tj := t.All[j]
			if tj.Kind != lualint.TokIdent {
				break
			}
			chain = append(chain, tj.Value)
			if firstIdx < 0 {
				firstIdx = j
			}
			next := t.NextNonTrivia(j)
			if next < 0 || t.All[next].Kind != lualint.TokPunct || t.All[next].Value != "." {
				break
			}
			j = t.NextNonTrivia(next)
		}
		if len(chain) < 2 {
			continue
		}
		if chain[len(chain)-1] != memberName {
			continue
		}
		if chain[len(chain)-2] != "state_manager" {
			continue
		}
		// Found the bare pcall shape. Skip if it is enclosed by a
		// `resurrect_error.with_capture(...)` call — but in practice
		// pcall(state_manager.save_state, …) inside with_capture
		// would be doubly-redundant, so this enclosure check exists
		// mainly for the negative test fixture and for symmetry with
		// the §17.4 wording.
		if enclosing[i] {
			continue
		}
		out = append(out, lualint.Finding{
			Path:     t.Path,
			Line:     tk.Line,
			Col:      tk.Col,
			RuleID:   ruleID,
			Severity: lualint.SevError,
			Message:  message,
		})
	}
	return out
}

// captureEnclosingMap returns a map keyed by token index whose value
// is true iff the token at that index is lexically inside a
// `resurrect_error.with_capture(...)` call's argument list. The
// implementation tracks open-paren positions on a stack; on each `(`
// we record whether the call site (the two tokens immediately
// preceding) is the with_capture target. On each `)` we pop. This
// gives us O(n) lookup per token.
func captureEnclosingMap(t *lualint.Tokens) map[int]bool {
	out := make(map[int]bool)
	type frame struct {
		isCapture bool
	}
	var stack []frame
	for i := 0; i < len(t.All); i++ {
		tk := t.All[i]
		if tk.Kind != lualint.TokPunct {
			if len(stack) > 0 && stack[len(stack)-1].isCapture {
				out[i] = true
			}
			continue
		}
		switch tk.Value {
		case "(":
			isCap := callSiteIsWithCapture(t, i)
			stack = append(stack, frame{isCapture: isCap})
		case ")":
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		default:
			if len(stack) > 0 && stack[len(stack)-1].isCapture {
				out[i] = true
			}
		}
	}
	return out
}

// callSiteIsWithCapture reports whether the `(` at index parenIdx is
// the open-paren of a `resurrect_error.with_capture` call. We accept
// any ident chain whose last component is `with_capture` (the helper
// is sometimes aliased as `local with_capture = mod.with_capture`).
// A bare `with_capture(…)` call (no dot prefix) is also accepted —
// that's the shape ops.lua uses after `resolve_with_capture()` is
// called.
func callSiteIsWithCapture(t *lualint.Tokens, parenIdx int) bool {
	prev := t.PrevNonTrivia(parenIdx)
	if prev < 0 {
		return false
	}
	tp := t.All[prev]
	if tp.Kind != lualint.TokIdent {
		return false
	}
	return tp.Value == "with_capture"
}
