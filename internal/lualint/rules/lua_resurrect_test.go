package rules

import (
	"testing"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// TestResurrectErrorOutsideOwner_Positive: a `wezterm.on(
// "resurrect.error", …)` call from any file other than
// resurrect_error.lua fires.
func TestResurrectErrorOutsideOwner_Positive(t *testing.T) {
	src := []byte(`local wezterm = require("wezterm")
wezterm.on("resurrect.error", function(msg) end)`)
	rule := ResurrectErrorOutsideOwner()
	out := lualint.LintBytes("plugin/wezsesh/ipc.lua", src, []lualint.Rule{rule})
	if len(out) == 0 {
		t.Fatalf("want >=1 finding outside resurrect_error.lua, got 0")
	}
	if out[0].RuleID != "lua-resurrect-error-owner" {
		t.Errorf("rule id: got %q", out[0].RuleID)
	}
}

// TestResurrectErrorOutsideOwner_Negative: same call inside the owning
// resurrect_error.lua is exempt.
func TestResurrectErrorOutsideOwner_Negative(t *testing.T) {
	src := []byte(`local wezterm = require("wezterm")
wezterm.on("resurrect.error", function(msg) end)`)
	rule := ResurrectErrorOutsideOwner()
	out := lualint.LintBytes("plugin/wezsesh/resurrect_error.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings inside owner, got %d: %v", len(out), out)
	}
}

// TestRestoreFinishedBan_Positive: workspace_state.restore_workspace.
// finished is banned everywhere.
func TestRestoreFinishedBan_Positive_Workspace(t *testing.T) {
	src := []byte(`local wezterm = require("wezterm")
wezterm.on("resurrect.workspace_state.restore_workspace.finished", function() end)`)
	rule := RestoreFinishedBan()
	out := lualint.LintBytes("plugin/wezsesh/resurrect_error.lua", src, []lualint.Rule{rule})
	if len(out) == 0 {
		t.Fatalf("want >=1 finding for restore_workspace.finished, got 0")
	}
}

// TestRestoreFinishedBan_Positive_Window: the window_state sibling.
func TestRestoreFinishedBan_Positive_Window(t *testing.T) {
	src := []byte(`local wezterm = require("wezterm")
wezterm.on("resurrect.window_state.restore_window.finished", function() end)`)
	rule := RestoreFinishedBan()
	out := lualint.LintBytes("plugin/wezsesh/foo.lua", src, []lualint.Rule{rule})
	if len(out) == 0 {
		t.Fatalf("want >=1 finding for restore_window.finished, got 0")
	}
}

// TestRestoreFinishedBan_Positive_Tab: the tab_state sibling.
func TestRestoreFinishedBan_Positive_Tab(t *testing.T) {
	src := []byte(`local wezterm = require("wezterm")
wezterm.on("resurrect.tab_state.restore_tab.finished", function() end)`)
	rule := RestoreFinishedBan()
	out := lualint.LintBytes("plugin/wezsesh/foo.lua", src, []lualint.Rule{rule})
	if len(out) == 0 {
		t.Fatalf("want >=1 finding for restore_tab.finished, got 0")
	}
}

// TestRestoreFinishedBan_Negative: an unrelated event name does not
// fire.
func TestRestoreFinishedBan_Negative(t *testing.T) {
	src := []byte(`local wezterm = require("wezterm")
wezterm.on("user-var-changed", function() end)`)
	rule := RestoreFinishedBan()
	out := lualint.LintBytes("plugin/wezsesh/foo.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings on unrelated event, got %d: %v", len(out), out)
	}
}
