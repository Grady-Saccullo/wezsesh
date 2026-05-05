package rules

import (
	"strings"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// resurrectErrorOwnerFile is the single Lua file that legitimately
// owns the `wezterm.on("resurrect.error", …)` registration. Every
// other file that calls wezterm.on with that event name is a spike-#2
// double-register bug waiting to happen.
const resurrectErrorOwnerFile = "plugin/wezsesh/resurrect_error.lua"

// ResurrectErrorOutsideOwner flags any `wezterm.on("resurrect.error",
// …)` registration outside resurrect_error.lua. The owning module
// keeps the listener idempotent via a `_G` install gate; a duplicate
// elsewhere fans out N copies on every resurrect.error emission.
func ResurrectErrorOutsideOwner() lualint.Rule {
	return lualint.RuleFunc{
		RuleID: "lua-resurrect-error-owner",
		Fn: func(t *lualint.Tokens) []lualint.Finding {
			if strings.HasSuffix(t.Path, resurrectErrorOwnerFile) {
				return nil
			}
			var out []lualint.Finding
			for _, hit := range findWeztermOnEvent(t, "resurrect.error") {
				out = append(out, lualint.Finding{
					Path:     t.Path,
					Line:     hit.Line,
					Col:      hit.Col,
					RuleID:   "lua-resurrect-error-owner",
					Severity: lualint.SevError,
					Message:  "wezterm.on(\"resurrect.error\", …) MUST live only in resurrect_error.lua (spike-#2 / §17.4)",
				})
			}
			return out
		},
	}
}

// restoreFinishedEvents is the set of resurrect events that fire only
// on the success path and are never a completion signal. Per §17.4
// any subscription to one of these is a spike-#2 misclassification
// trap regardless of where it appears.
var restoreFinishedEvents = []string{
	"resurrect.workspace_state.restore_workspace.finished",
	"resurrect.window_state.restore_window.finished",
	"resurrect.tab_state.restore_tab.finished",
}

// RestoreFinishedBan flags any `wezterm.on(<one of the three success-
// only events>, …)` registration anywhere. The events fire only on the
// success path (resurrect does not pcall its restore loops) so a
// handler subscribed to them never observes failure — wezsesh wraps
// restore calls in `with_capture` instead.
func RestoreFinishedBan() lualint.Rule {
	return lualint.RuleFunc{
		RuleID: "lua-restore-finished-ban",
		Fn: func(t *lualint.Tokens) []lualint.Finding {
			var out []lualint.Finding
			for _, ev := range restoreFinishedEvents {
				for _, hit := range findWeztermOnEvent(t, ev) {
					out = append(out, lualint.Finding{
						Path:     t.Path,
						Line:     hit.Line,
						Col:      hit.Col,
						RuleID:   "lua-restore-finished-ban",
						Severity: lualint.SevError,
						Message:  "wezterm.on(\"" + ev + "\", …) is a spike-#2 misclassification trap; the event fires only on success — use resurrect_error.with_capture (§17.4)",
					})
				}
			}
			return out
		},
	}
}

// hit is a tiny tuple for findWeztermOnEvent; it's local to this file
// because the rules package doesn't otherwise need a position type.
type hit struct {
	Line int
	Col  int
}

// findWeztermOnEvent returns the position of every
// `wezterm.on("<event>", ...)` callsite in t. The match is structural
// against the token stream so prose mentions like `wezterm.on(...)`
// inside `--[[ ]]` comments don't trigger.
func findWeztermOnEvent(t *lualint.Tokens, event string) []hit {
	var out []hit
	for i := 0; i < len(t.All); i++ {
		tk := t.All[i]
		if tk.Kind != lualint.TokIdent || tk.Value != "wezterm" {
			continue
		}
		dotIdx := t.NextNonTrivia(i)
		if dotIdx < 0 || t.All[dotIdx].Value != "." {
			continue
		}
		onIdx := t.NextNonTrivia(dotIdx)
		if onIdx < 0 || t.All[onIdx].Kind != lualint.TokIdent || t.All[onIdx].Value != "on" {
			continue
		}
		parenIdx := t.NextNonTrivia(onIdx)
		if parenIdx < 0 || t.All[parenIdx].Kind != lualint.TokPunct || t.All[parenIdx].Value != "(" {
			continue
		}
		strIdx := t.NextNonTrivia(parenIdx)
		if strIdx < 0 || t.All[strIdx].Kind != lualint.TokString {
			continue
		}
		// The token's Value includes the surrounding quotes. Strip
		// them and compare exact contents. Long-string forms are not
		// expected for an event name; a [[…]] event name would not
		// match either resurrect convention here.
		val := t.All[strIdx].Value
		if len(val) < 2 {
			continue
		}
		inner := val[1 : len(val)-1]
		if inner != event {
			continue
		}
		out = append(out, hit{Line: tk.Line, Col: tk.Col})
	}
	return out
}
