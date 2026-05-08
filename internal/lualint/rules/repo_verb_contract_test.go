package rules

import (
	"path/filepath"
	"testing"
)

// TestVerbContractRule_Negative: a verb module that exports both
// args_shape and dispatch is silent.
func TestVerbContractRule_Negative(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "plugin", "wezsesh", "verbs", "save.lua"), `
local M = {}
M.args_shape = { _shape = "object", name = "string" }
function M.dispatch(payload, window, pane) end
return M
`)
	rule := VerbContractRule()
	out := rule.Check(root)
	if len(out) != 0 {
		t.Errorf("want 0 findings on conformant verb module, got %d: %v", len(out), out)
	}
}

// TestVerbContractRule_MissingShape: dispatch without args_shape fires.
func TestVerbContractRule_MissingShape(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "plugin", "wezsesh", "verbs", "broken.lua"), `
local M = {}
function M.dispatch(payload) end
return M
`)
	rule := VerbContractRule()
	out := rule.Check(root)
	if len(out) != 1 {
		t.Fatalf("want 1 finding for missing args_shape, got %d: %v", len(out), out)
	}
	if out[0].RuleID != "lua-verb-contract" {
		t.Errorf("rule id: got %q", out[0].RuleID)
	}
}

// TestVerbContractRule_MissingDispatch: args_shape without dispatch
// fires.
func TestVerbContractRule_MissingDispatch(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "plugin", "wezsesh", "verbs", "broken.lua"), `
local M = {}
M.args_shape = { _shape = "object" }
return M
`)
	rule := VerbContractRule()
	out := rule.Check(root)
	if len(out) != 1 {
		t.Fatalf("want 1 finding for missing dispatch, got %d: %v", len(out), out)
	}
}

// TestVerbContractRule_DispatchAsField: `M.dispatch = function() end`
// is also accepted (alternative to `function M.dispatch()`).
func TestVerbContractRule_DispatchAsField(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "plugin", "wezsesh", "verbs", "save.lua"), `
local M = {}
M.args_shape = { _shape = "object" }
M.dispatch = function(payload) end
return M
`)
	rule := VerbContractRule()
	out := rule.Check(root)
	if len(out) != 0 {
		t.Errorf("want 0 findings on field-style dispatch, got %d: %v", len(out), out)
	}
}

// TestVerbContractRule_HelperSkipped: `_deps.lua` and `_restore.lua`
// are helper modules and are exempt.
func TestVerbContractRule_HelperSkipped(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "plugin", "wezsesh", "verbs", "_deps.lua"), `
local M = {}
return M
`)
	mustWriteFile(t, filepath.Join(root, "plugin", "wezsesh", "verbs", "_restore.lua"), `
local M = {}
function M.load_and_restore() end
return M
`)
	rule := VerbContractRule()
	out := rule.Check(root)
	if len(out) != 0 {
		t.Errorf("want 0 findings on underscore-prefixed helpers, got %d: %v", len(out), out)
	}
}

// TestVerbContractRule_InitSkipped: `init.lua` is the registry, not a
// verb, and is exempt.
func TestVerbContractRule_InitSkipped(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "plugin", "wezsesh", "verbs", "init.lua"), `
local M = {}
function M.register(name, mod) end
return M
`)
	rule := VerbContractRule()
	out := rule.Check(root)
	if len(out) != 0 {
		t.Errorf("want 0 findings on init.lua (registry, not a verb), got %d: %v", len(out), out)
	}
}

// TestVerbContractRule_SpecSkipped: `*_spec.lua` files under verbs/
// are tests, not verbs, and are exempt.
func TestVerbContractRule_SpecSkipped(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "plugin", "wezsesh", "verbs", "verbs_spec.lua"), `
local M = {}
return M
`)
	rule := VerbContractRule()
	out := rule.Check(root)
	if len(out) != 0 {
		t.Errorf("want 0 findings on _spec.lua, got %d: %v", len(out), out)
	}
}
