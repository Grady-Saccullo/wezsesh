package rules

import (
	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// BackgroundChildProcessPcallRule flags any direct call to
// `wezterm.background_child_process(…)` that is NOT immediately
// preceded by `pcall(`. Per §16.5 / CLAUDE.md (Lua handler synchrony):
// `wezterm.background_child_process` is permitted in step (i) of the
// user-var-changed handler, but ONLY when pcall-wrapped — an unwrapped
// raise inside the spawn would propagate into the wezterm event loop.
//
// Two acceptable shapes:
//
//	pcall(wezterm.background_child_process, argv, env)
//	local ok = pcall(wezterm.background_child_process, …)
//
// Any other shape — bare `wezterm.background_child_process(...)` or
// passing the function as a value into something that doesn't pcall —
// fires.
//
// We approximate via grep semantics: the rule fires if the dotted
// chain `wezterm.background_child_process` appears AND the preceding
// non-trivia token is NOT `,` (the `pcall(fn,` pattern) AND NOT `(`
// (the start of the pcall arg list itself). Bare callsites
// (`wezterm.background_child_process(...)`) are flagged because the
// preceding non-trivia token is whatever statement led into them —
// neither `,` nor `(` from a pcall ledger.
func BackgroundChildProcessPcallRule() lualint.Rule {
	return lualint.RuleFunc{
		RuleID: "lua-bg-child-process-pcall",
		Fn: func(t *lualint.Tokens) []lualint.Finding {
			var out []lualint.Finding
			for i := 0; i < len(t.All); i++ {
				tk := t.All[i]
				if tk.Kind != lualint.TokIdent || tk.Value != "wezterm" {
					continue
				}
				dotIdx := t.NextNonTrivia(i)
				if dotIdx < 0 || t.All[dotIdx].Value != "." {
					continue
				}
				memberIdx := t.NextNonTrivia(dotIdx)
				if memberIdx < 0 ||
					t.All[memberIdx].Kind != lualint.TokIdent ||
					t.All[memberIdx].Value != "background_child_process" {
					continue
				}
				if isInsidePcallArglist(t, i) {
					continue
				}
				out = append(out, lualint.Finding{
					Path:     t.Path,
					Line:     tk.Line,
					Col:      tk.Col,
					RuleID:   "lua-bg-child-process-pcall",
					Severity: lualint.SevError,
					Message:  "wezterm.background_child_process MUST be invoked via pcall(…) — an unwrapped raise wedges the wezterm event loop (§16.5)",
				})
			}
			return out
		},
	}
}

// isInsidePcallArglist reports whether the token at idx is the first
// non-trivia element of the argument list of an enclosing `pcall(…)`
// call. We accept the `pcall(wezterm.background_child_process, …)`
// shape (function passed as value, separated by `,`).
func isInsidePcallArglist(t *lualint.Tokens, idx int) bool {
	// Walk backwards through the immediately preceding non-trivia
	// token. If it is `(` AND the token before it is `pcall`, we are
	// the first arg of a pcall call.
	prev := t.PrevNonTrivia(idx)
	if prev < 0 {
		return false
	}
	if t.All[prev].Kind != lualint.TokPunct || t.All[prev].Value != "(" {
		return false
	}
	pcallIdx := t.PrevNonTrivia(prev)
	if pcallIdx < 0 {
		return false
	}
	pt := t.All[pcallIdx]
	return pt.Kind == lualint.TokIdent && pt.Value == "pcall"
}
