package rules

import (
	"testing"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// TestBackgroundChildProcessPcallRule_Positive: a bare
// wezterm.background_child_process(...) call fires.
func TestBackgroundChildProcessPcallRule_Positive(t *testing.T) {
	src := []byte(`local wezterm = require("wezterm")
wezterm.background_child_process({"wezsesh"})`)
	rule := BackgroundChildProcessPcallRule()
	out := lualint.LintBytes("plugin/wezsesh/result.lua", src, []lualint.Rule{rule})
	if len(out) == 0 {
		t.Fatalf("want >=1 finding for unwrapped background_child_process, got 0")
	}
	if out[0].RuleID != "lua-bg-child-process-pcall" {
		t.Errorf("rule id: got %q", out[0].RuleID)
	}
}

// TestBackgroundChildProcessPcallRule_Negative: pcall(wezterm.
// background_child_process, argv) is the legitimate shape.
func TestBackgroundChildProcessPcallRule_Negative(t *testing.T) {
	src := []byte(`local wezterm = require("wezterm")
local ok = pcall(wezterm.background_child_process, {"wezsesh"})`)
	rule := BackgroundChildProcessPcallRule()
	out := lualint.LintBytes("plugin/wezsesh/result.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings on pcall-wrapped form, got %d: %v", len(out), out)
	}
}
