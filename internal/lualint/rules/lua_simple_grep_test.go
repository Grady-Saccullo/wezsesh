package rules

import (
	"testing"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// TestGWeztermBan_Positive: a `_G.wezterm` reference fires.
func TestGWeztermBan_Positive(t *testing.T) {
	src := []byte(`if _G.wezterm then return end`)
	rule := GWeztermBan()
	out := lualint.LintBytes("plugin/wezsesh/foo.lua", src, []lualint.Rule{rule})
	if len(out) == 0 {
		t.Fatalf("want >=1 finding for _G.wezterm, got 0")
	}
	if out[0].RuleID != "lua-g-wezterm" {
		t.Errorf("rule id: got %q", out[0].RuleID)
	}
}

// TestGWeztermBan_Negative: `local wezterm = require("wezterm")` is
// the legitimate import path and does not fire.
func TestGWeztermBan_Negative(t *testing.T) {
	src := []byte(`local wezterm = require("wezterm")
local x = wezterm.GLOBAL.foo`)
	rule := GWeztermBan()
	out := lualint.LintBytes("plugin/wezsesh/foo.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings on require shape, got %d: %v", len(out), out)
	}
}

// TestDebugBan_Positive: `debug.getinfo(1)` fires.
func TestDebugBan_Positive(t *testing.T) {
	src := []byte(`local info = debug.getinfo(1)`)
	rule := DebugBan()
	out := lualint.LintBytes("plugin/wezsesh/foo.lua", src, []lualint.Rule{rule})
	if len(out) == 0 {
		t.Fatalf("want >=1 finding for debug.getinfo, got 0")
	}
}

// TestDebugBan_Negative_StringLiteral: the string "debug." inside a
// Lua string literal must NOT fire (the tokeniser correctly classifies
// it as TokString).
func TestDebugBan_Negative_StringLiteral(t *testing.T) {
	src := []byte(`local s = "debug.getinfo"`)
	rule := DebugBan()
	out := lualint.LintBytes("plugin/wezsesh/foo.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings on string literal mention, got %d: %v", len(out), out)
	}
}

// TestDofileBan_Positive: dofile("foo.lua") fires.
func TestDofileBan_Positive(t *testing.T) {
	src := []byte(`dofile("foo.lua")`)
	rule := DofileBan()
	out := lualint.LintBytes("plugin/init.lua", src, []lualint.Rule{rule})
	if len(out) == 0 {
		t.Fatalf("want >=1 finding for dofile(…), got 0")
	}
}

// TestDofileBan_Negative: loadfile("foo.lua")() is the recommended
// workaround and does not fire.
func TestDofileBan_Negative(t *testing.T) {
	src := []byte(`local fn = loadfile("foo.lua") fn()`)
	rule := DofileBan()
	out := lualint.LintBytes("plugin/init.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings on loadfile, got %d: %v", len(out), out)
	}
}

// TestLineLeadingParenRule_Positive: f({})\n(g) — §9.0.1.1 ambiguity.
func TestLineLeadingParenRule_Positive(t *testing.T) {
	src := []byte(`f({})
(g)`)
	rule := LineLeadingParenRule()
	out := lualint.LintBytes("plugin/init.lua", src, []lualint.Rule{rule})
	if len(out) == 0 {
		t.Fatalf("want >=1 finding for line-leading paren, got 0")
	}
}

// TestLineLeadingParenRule_Negative: explicit `;` separator suppresses
// the warning per §9.0.1.1.
func TestLineLeadingParenRule_Negative(t *testing.T) {
	src := []byte(`f({})
;(g)`)
	rule := LineLeadingParenRule()
	out := lualint.LintBytes("plugin/init.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings on disambiguated form, got %d: %v", len(out), out)
	}
}

// TestWeztermGlobalNestedTableRule_Positive: a literal table assigned
// to wezterm.GLOBAL.foo fires.
func TestWeztermGlobalNestedTableRule_Positive(t *testing.T) {
	src := []byte(`local wezterm = require("wezterm")
wezterm.GLOBAL.foo = { x = 1 }`)
	rule := WeztermGlobalNestedTableRule()
	out := lualint.LintBytes("plugin/wezsesh/foo.lua", src, []lualint.Rule{rule})
	if len(out) == 0 {
		t.Fatalf("want >=1 finding for nested table to GLOBAL, got 0")
	}
}

// TestWeztermGlobalNestedTableRule_Negative_Scalar: a scalar value is OK.
func TestWeztermGlobalNestedTableRule_Negative_Scalar(t *testing.T) {
	src := []byte(`local wezterm = require("wezterm")
wezterm.GLOBAL.foo = "hello"`)
	rule := WeztermGlobalNestedTableRule()
	out := lualint.LintBytes("plugin/wezsesh/foo.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings on scalar assignment, got %d: %v", len(out), out)
	}
}

// TestWeztermGlobalNestedTableRule_Negative_CanonicalJSON: the rule
// is silent inside canonical_json.lua (legitimate canonical-JSON
// helpers may construct tagged tables).
func TestWeztermGlobalNestedTableRule_Negative_CanonicalJSON(t *testing.T) {
	src := []byte(`local wezterm = require("wezterm")
wezterm.GLOBAL.foo = { x = 1 }`)
	rule := WeztermGlobalNestedTableRule()
	out := lualint.LintBytes("plugin/wezsesh/canonical_json.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings inside canonical_json.lua, got %d: %v", len(out), out)
	}
}
