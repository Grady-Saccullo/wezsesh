package rules

import (
	"testing"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// TestResurrectRefOnly_RawGet_Outside: a `rawget(_G, "resurrect")` in a
// non-owner file fires.
func TestResurrectRefOnly_RawGet_Outside(t *testing.T) {
	src := []byte(`local r = rawget(_G, "resurrect")`)
	rule := ResurrectRefOnly()
	out := lualint.LintBytes("plugin/wezsesh/ops.lua", src, []lualint.Rule{rule})
	if len(out) != 1 {
		t.Fatalf("want 1 finding, got %d: %v", len(out), out)
	}
	if out[0].RuleID != "lua-resurrect-ref-only" {
		t.Errorf("rule id: got %q", out[0].RuleID)
	}
}

// TestResurrectRefOnly_RawGet_Wezsesh: the wezsesh_resurrect key fires
// the same way as the legacy `resurrect` key.
func TestResurrectRefOnly_RawGet_Wezsesh(t *testing.T) {
	src := []byte(`local r = rawget(_G, "wezsesh_resurrect")`)
	rule := ResurrectRefOnly()
	out := lualint.LintBytes("plugin/wezsesh/ops.lua", src, []lualint.Rule{rule})
	if len(out) != 1 {
		t.Fatalf("want 1 finding, got %d: %v", len(out), out)
	}
}

// TestResurrectRefOnly_RawSet: `rawset(_G, "wezsesh_resurrect", v)` is
// also flagged (the init.lua write-through path before the migration).
func TestResurrectRefOnly_RawSet(t *testing.T) {
	src := []byte(`rawset(_G, "wezsesh_resurrect", mod)`)
	rule := ResurrectRefOnly()
	out := lualint.LintBytes("plugin/init.lua", src, []lualint.Rule{rule})
	if len(out) != 1 {
		t.Fatalf("want 1 finding, got %d: %v", len(out), out)
	}
}

// TestResurrectRefOnly_DotAccess: `_G.resurrect` (read) is flagged.
func TestResurrectRefOnly_DotAccess(t *testing.T) {
	src := []byte(`local r = _G.resurrect`)
	rule := ResurrectRefOnly()
	out := lualint.LintBytes("plugin/wezsesh/foo.lua", src, []lualint.Rule{rule})
	if len(out) != 1 {
		t.Fatalf("want 1 finding, got %d: %v", len(out), out)
	}
}

// TestResurrectRefOnly_BracketAccess: `_G["resurrect"]` is flagged.
func TestResurrectRefOnly_BracketAccess(t *testing.T) {
	src := []byte(`local r = _G["wezsesh_resurrect"]`)
	rule := ResurrectRefOnly()
	out := lualint.LintBytes("plugin/wezsesh/foo.lua", src, []lualint.Rule{rule})
	if len(out) != 1 {
		t.Fatalf("want 1 finding, got %d: %v", len(out), out)
	}
}

// TestResurrectRefOnly_OwnerExempt: the owning module
// (runtime/resurrect_ref.lua) is exempt — that's where the lookup
// rule lives.
func TestResurrectRefOnly_OwnerExempt(t *testing.T) {
	src := []byte(`local r = rawget(_G, "wezsesh_resurrect")
local s = rawget(_G, "resurrect")
rawset(_G, "wezsesh_resurrect", x)`)
	rule := ResurrectRefOnly()
	out := lualint.LintBytes("plugin/wezsesh/runtime/resurrect_ref.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings inside owner, got %d: %v", len(out), out)
	}
}

// TestResurrectRefOnly_UnrelatedKey: `rawget(_G, "something_else")`
// must NOT fire. The rule is keyed on the two specific names.
func TestResurrectRefOnly_UnrelatedKey(t *testing.T) {
	src := []byte(`local r = rawget(_G, "wezsesh_session_key")`)
	rule := ResurrectRefOnly()
	out := lualint.LintBytes("plugin/wezsesh/ops.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings on unrelated key, got %d: %v", len(out), out)
	}
}

// TestResurrectRefOnly_StringLiteralMention: a string mentioning the
// key name in a comment or string literal does NOT fire — the match is
// structural against the rawget/rawset/_G access pattern.
func TestResurrectRefOnly_StringLiteralMention(t *testing.T) {
	src := []byte(`local s = "rawget(_G, 'resurrect') is forbidden"`)
	rule := ResurrectRefOnly()
	out := lualint.LintBytes("plugin/wezsesh/ops.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings on string-literal mention, got %d: %v", len(out), out)
	}
}
