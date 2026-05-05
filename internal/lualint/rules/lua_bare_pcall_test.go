package rules

import (
	"testing"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// TestBarePcallSaveStateRule_Positive: bare
// pcall(resurrect.state_manager.save_state, …) outside with_capture
// fires.
func TestBarePcallSaveStateRule_Positive(t *testing.T) {
	src := []byte(`local ok, err = pcall(resurrect.state_manager.save_state, state)`)
	rule := BarePcallSaveStateRule()
	out := lualint.LintBytes("plugin/wezsesh/manager.lua", src, []lualint.Rule{rule})
	if len(out) == 0 {
		t.Fatalf("want >=1 finding for bare pcall save_state, got 0")
	}
	if out[0].RuleID != "lua-bare-pcall-save-state" {
		t.Errorf("rule id: got %q", out[0].RuleID)
	}
}

// TestBarePcallSaveStateRule_Negative_InsideCapture: the same shape
// inside resurrect_error.with_capture(function() … end) does not fire.
func TestBarePcallSaveStateRule_Negative_InsideCapture(t *testing.T) {
	src := []byte(`local pok, perr, captured = with_capture(function()
  local ok, err = pcall(resurrect.state_manager.save_state, state)
  return ok, err
end)`)
	rule := BarePcallSaveStateRule()
	out := lualint.LintBytes("plugin/wezsesh/ops.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings inside with_capture, got %d: %v", len(out), out)
	}
}

// TestBarePcallSaveStateRule_Negative_NoPcall: a plain call (no
// pcall wrapper) does not fire — only the bare pcall shape is the
// invariant violation.
func TestBarePcallSaveStateRule_Negative_NoPcall(t *testing.T) {
	src := []byte(`local v = resurrect.state_manager.save_state(state)`)
	rule := BarePcallSaveStateRule()
	out := lualint.LintBytes("plugin/wezsesh/foo.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings on plain call, got %d: %v", len(out), out)
	}
}

// TestBarePcallLoadStateRule_Positive.
func TestBarePcallLoadStateRule_Positive(t *testing.T) {
	src := []byte(`local ok, state = pcall(resurrect.state_manager.load_state, "ws", "workspace")`)
	rule := BarePcallLoadStateRule()
	out := lualint.LintBytes("plugin/wezsesh/manager.lua", src, []lualint.Rule{rule})
	if len(out) == 0 {
		t.Fatalf("want >=1 finding for bare pcall load_state, got 0")
	}
}

// TestBarePcallLoadStateRule_Negative_InsideCapture.
func TestBarePcallLoadStateRule_Negative_InsideCapture(t *testing.T) {
	src := []byte(`local pok = with_capture(function()
  local ok = pcall(resurrect.state_manager.load_state, "ws", "workspace")
  return ok
end)`)
	rule := BarePcallLoadStateRule()
	out := lualint.LintBytes("plugin/wezsesh/ops.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings inside with_capture, got %d: %v", len(out), out)
	}
}
