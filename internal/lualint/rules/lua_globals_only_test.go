package rules

import (
	"testing"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// TestGlobalsOnly_DotRead: a bare `wezterm.GLOBAL.wezsesh_session_key`
// read fires outside the owner files.
func TestGlobalsOnly_DotRead(t *testing.T) {
	src := []byte(`local k = wezterm.GLOBAL.wezsesh_session_key`)
	rule := GlobalsOnly()
	out := lualint.LintBytes("plugin/wezsesh/ipc.lua", src, []lualint.Rule{rule})
	if len(out) != 1 {
		t.Fatalf("want 1 finding, got %d: %v", len(out), out)
	}
	if out[0].RuleID != "lua-globals-only" {
		t.Errorf("rule id: got %q", out[0].RuleID)
	}
}

// TestGlobalsOnly_DotWrite: assignment fires the same way as read.
func TestGlobalsOnly_DotWrite(t *testing.T) {
	src := []byte(`wezterm.GLOBAL.wezsesh_bin_path = "/usr/local/bin/wezsesh"`)
	rule := GlobalsOnly()
	out := lualint.LintBytes("plugin/wezsesh/manager.lua", src, []lualint.Rule{rule})
	if len(out) != 1 {
		t.Fatalf("want 1 finding, got %d: %v", len(out), out)
	}
}

// TestGlobalsOnly_BracketAccess: `wezterm.GLOBAL["wezsesh_<key>"]`
// is also flagged.
func TestGlobalsOnly_BracketAccess(t *testing.T) {
	src := []byte(`local v = wezterm.GLOBAL["wezsesh_plugin_version"]`)
	rule := GlobalsOnly()
	out := lualint.LintBytes("plugin/wezsesh/foo.lua", src, []lualint.Rule{rule})
	if len(out) != 1 {
		t.Fatalf("want 1 finding, got %d: %v", len(out), out)
	}
}

// TestGlobalsOnly_GlobalsOwnerExempt: runtime/globals.lua may touch the
// surface freely — that's where the accessors live.
func TestGlobalsOnly_GlobalsOwnerExempt(t *testing.T) {
	src := []byte(`wezterm.GLOBAL.wezsesh_session_key = "deadbeef"
local p = wezterm.GLOBAL.wezsesh_bin_path
local v = wezterm.GLOBAL["wezsesh_plugin_version"]`)
	rule := GlobalsOnly()
	out := lualint.LintBytes("plugin/wezsesh/runtime/globals.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings inside runtime/globals.lua, got %d: %v", len(out), out)
	}
}

// TestGlobalsOnly_StateOwnerExempt: runtime/state.lua is the second
// authorised accessor (per-pane buckets like wezsesh_state /
// wezsesh_seen_ids). It uses bracket-form indexing for variable bucket
// names; the rule must not fire there either.
func TestGlobalsOnly_StateOwnerExempt(t *testing.T) {
	src := []byte(`local b = wezterm.GLOBAL["wezsesh_state"]
wezterm.GLOBAL["wezsesh_seen_ids"] = encoded`)
	rule := GlobalsOnly()
	out := lualint.LintBytes("plugin/wezsesh/runtime/state.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings inside runtime/state.lua, got %d: %v", len(out), out)
	}
}

// TestGlobalsOnly_NonWezseshKey: GLOBAL keys that don't start with
// `wezsesh_` are not the rule's concern.
func TestGlobalsOnly_NonWezseshKey(t *testing.T) {
	src := []byte(`local v = wezterm.GLOBAL.some_other_thing`)
	rule := GlobalsOnly()
	out := lualint.LintBytes("plugin/wezsesh/ipc.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings on non-wezsesh GLOBAL key, got %d: %v", len(out), out)
	}
}

// TestGlobalsOnly_StringMention: a string literal mentioning the
// pattern in prose does NOT fire — the match is structural.
func TestGlobalsOnly_StringMention(t *testing.T) {
	src := []byte(`local s = "wezterm.GLOBAL.wezsesh_session_key is forbidden here"`)
	rule := GlobalsOnly()
	out := lualint.LintBytes("plugin/wezsesh/ipc.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings on string-literal mention, got %d: %v", len(out), out)
	}
}
