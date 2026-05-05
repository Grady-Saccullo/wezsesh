package rules

import (
	"strings"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// globalsOwnerSuffixes are the path suffixes (forward-slash form) of
// the two Lua files that legitimately read or write
// `wezterm.GLOBAL.wezsesh_*` keys. Every other access — anywhere in
// `plugin/` — must route through `runtime/globals.lua` (scalar values)
// or `runtime/state.lua` (per-pane buckets).
var globalsOwnerSuffixes = []string{
	"plugin/wezsesh/runtime/globals.lua",
	"plugin/wezsesh/runtime/state.lua",
}

// GlobalsOnly flags any `wezterm.GLOBAL.wezsesh_<name>` or
// `wezterm.GLOBAL["wezsesh_<name>"]` access outside the two owner
// files. The match is structural (token-driven) so prose mentions
// inside comments / strings don't trigger.
//
// Why centralise the GLOBAL surface: the GLOBAL bucket has shape rules
// (no nested-table values, snapshot-on-read), and a wipe-on-version-
// mismatch loop runs at apply_to_config time. Keeping every access
// behind a typed accessor in `runtime/` makes the contract reviewable
// in one place and lets a future schema change land in one file
// instead of grep-and-replace across the plugin.
func GlobalsOnly() lualint.Rule {
	return lualint.RuleFunc{
		RuleID: "lua-globals-only",
		Fn: func(t *lualint.Tokens) []lualint.Finding {
			path := t.Path
			for _, suffix := range globalsOwnerSuffixes {
				if strings.HasSuffix(path, suffix) {
					return nil
				}
			}
			var out []lualint.Finding
			for i := 0; i < len(t.All); i++ {
				if hit, ok := matchGlobalsAccess(t, i); ok {
					tk := t.All[hit]
					out = append(out, lualint.Finding{
						Path:     t.Path,
						Line:     tk.Line,
						Col:      tk.Col,
						RuleID:   "lua-globals-only",
						Severity: lualint.SevError,
						Message: "wezterm.GLOBAL.wezsesh_* access MUST go through " +
							"plugin/wezsesh/runtime/{globals,state}.lua",
					})
				}
			}
			return out
		},
	}
}

// matchGlobalsAccess detects either `wezterm.GLOBAL.wezsesh_<name>` or
// `wezterm.GLOBAL["wezsesh_<name>"]` starting at token index i. Returns
// the index of the leading `wezterm` token on a hit so the finding
// points at the start of the chain.
func matchGlobalsAccess(t *lualint.Tokens, i int) (int, bool) {
	tk := t.All[i]
	if tk.Kind != lualint.TokIdent || tk.Value != "wezterm" {
		return 0, false
	}
	dotIdx := t.NextNonTrivia(i)
	if dotIdx < 0 || t.All[dotIdx].Kind != lualint.TokPunct || t.All[dotIdx].Value != "." {
		return 0, false
	}
	gIdx := t.NextNonTrivia(dotIdx)
	if gIdx < 0 || t.All[gIdx].Kind != lualint.TokIdent || t.All[gIdx].Value != "GLOBAL" {
		return 0, false
	}
	// `wezterm.GLOBAL` followed by `.wezsesh_<name>`?
	nextIdx := t.NextNonTrivia(gIdx)
	if nextIdx < 0 {
		return 0, false
	}
	nv := t.All[nextIdx]
	if nv.Kind == lualint.TokPunct && nv.Value == "." {
		keyIdx := t.NextNonTrivia(nextIdx)
		if keyIdx < 0 || t.All[keyIdx].Kind != lualint.TokIdent {
			return 0, false
		}
		if !strings.HasPrefix(t.All[keyIdx].Value, "wezsesh_") {
			return 0, false
		}
		return i, true
	}
	// `wezterm.GLOBAL["wezsesh_<name>"]`?
	if nv.Kind == lualint.TokPunct && nv.Value == "[" {
		keyIdx := t.NextNonTrivia(nextIdx)
		if keyIdx < 0 || t.All[keyIdx].Kind != lualint.TokString {
			return 0, false
		}
		literal := t.All[keyIdx].Value
		if len(literal) < 2 {
			return 0, false
		}
		first, last := literal[0], literal[len(literal)-1]
		if (first != '"' && first != '\'') || first != last {
			return 0, false
		}
		inner := literal[1 : len(literal)-1]
		if !strings.HasPrefix(inner, "wezsesh_") {
			return 0, false
		}
		return i, true
	}
	return 0, false
}
