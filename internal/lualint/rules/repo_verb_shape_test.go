package rules

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// TestVerbShapeParityRule_Positive: keys of verb_args_shape do not
// match dispatch_table — at least one diagnostic fires.
func TestVerbShapeParityRule_Positive(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "plugin", "wezsesh", "canonical_json.lua"), `
local M = {}
M.verb_args_shape = {
    a = { _shape = "object", name = "string" },
    b = { _shape = "object", name = "string" },
}
return M
`)
	mustWriteFile(t, filepath.Join(root, "plugin", "wezsesh", "ops.lua"), `
local M = {}
M.dispatch_table = {
    a = function() end,
    c = function() end,
}
return M
`)
	rule := VerbShapeParityRule()
	out := rule.Check(root)
	if len(out) == 0 {
		t.Fatalf("want >=1 finding on parity violation, got 0")
	}
	hasErr := false
	for _, f := range out {
		if f.Severity == lualint.SevError {
			hasErr = true
		}
	}
	if !hasErr {
		t.Errorf("want at least one SevError, got: %v", out)
	}
}

// TestVerbShapeParityRule_Negative: identical keysets — silent.
func TestVerbShapeParityRule_Negative(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "plugin", "wezsesh", "canonical_json.lua"), `
local M = {}
M.verb_args_shape = {
    a = { _shape = "object" },
    b = { _shape = "object" },
}
return M
`)
	mustWriteFile(t, filepath.Join(root, "plugin", "wezsesh", "ops.lua"), `
local M = {}
M.dispatch_table = {
    a = function() end,
    b = function() end,
}
return M
`)
	rule := VerbShapeParityRule()
	out := rule.Check(root)
	if len(out) != 0 {
		t.Errorf("want 0 findings on matching keysets, got %d: %v", len(out), out)
	}
}

// mustWriteFile is a tiny test helper: creates the parent dir (mode
// 0o755) and writes content to the path. Used by the temp-tree
// builders in this test file and the init.lua presence tests.
func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
