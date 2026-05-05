package rules

import (
	"path/filepath"
	"testing"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// TestPackageLoadedBustLoopRule_Positive: an apply_to_config body
// missing the cache-bust loop fires.
func TestPackageLoadedBustLoopRule_Positive(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "plugin", "init.lua"), `
local M = {}
function M.apply_to_config(config, opts)
    local x = 1
end
return M
`)
	rule := PackageLoadedBustLoopRule()
	out := rule.Check(root)
	if len(out) == 0 {
		t.Fatalf("want >=1 finding on missing bust loop, got 0")
	}
	if out[0].Severity != lualint.SevError {
		t.Errorf("severity: got %v want SevError", out[0].Severity)
	}
}

// TestPackageLoadedBustLoopRule_Negative: an apply_to_config body that
// contains the canonical bust loop is silent.
func TestPackageLoadedBustLoopRule_Negative(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "plugin", "init.lua"), `
local M = {}
function M.apply_to_config(config, opts)
    for k in pairs(package.loaded) do
        if k:sub(1, 8) == "wezsesh." then package.loaded[k] = nil end
    end
    require("wezsesh.resurrect_error").register()
end
return M
`)
	rule := PackageLoadedBustLoopRule()
	out := rule.Check(root)
	if len(out) != 0 {
		t.Errorf("want 0 findings when bust loop present, got %d: %v", len(out), out)
	}
}

// TestResurrectErrorRegisterPresenceRule_Positive: an apply_to_config
// body missing the resurrect_error.register() call fires.
func TestResurrectErrorRegisterPresenceRule_Positive(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "plugin", "init.lua"), `
local M = {}
function M.apply_to_config(config, opts)
    for k in pairs(package.loaded) do
        if k:sub(1, 8) == "wezsesh." then package.loaded[k] = nil end
    end
end
return M
`)
	rule := ResurrectErrorRegisterPresenceRule()
	out := rule.Check(root)
	if len(out) == 0 {
		t.Fatalf("want >=1 finding on missing resurrect_error.register, got 0")
	}
}

// TestResurrectErrorRegisterPresenceRule_Negative: a body that calls
// resurrect_error.register() (via local alias) is silent.
func TestResurrectErrorRegisterPresenceRule_Negative(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "plugin", "init.lua"), `
local M = {}
function M.apply_to_config(config, opts)
    local re = require("wezsesh.resurrect_error")
    re.register()
end
return M
`)
	rule := ResurrectErrorRegisterPresenceRule()
	out := rule.Check(root)
	if len(out) != 0 {
		t.Errorf("want 0 findings when register present, got %d: %v", len(out), out)
	}
}
