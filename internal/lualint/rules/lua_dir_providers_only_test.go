package rules

import (
	"testing"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// TestDirProvidersOnly_OwnerExempt: the accessor module itself may
// require its own path (no-op in practice but the lint must not flag
// it).
func TestDirProvidersOnly_OwnerExempt(t *testing.T) {
	src := []byte(`local d = require("wezsesh.runtime.dir_providers")`)
	rule := DirProvidersOnly()
	out := lualint.LintBytes("plugin/wezsesh/runtime/dir_providers.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings inside owner, got %d: %v", len(out), out)
	}
}

// TestDirProvidersOnly_ListDirsVerbAllowed: list_dirs.lua is the sole
// consumer outside the runtime module; its require must pass.
func TestDirProvidersOnly_ListDirsVerbAllowed(t *testing.T) {
	src := []byte(`local dp = require("wezsesh.runtime.dir_providers")`)
	rule := DirProvidersOnly()
	out := lualint.LintBytes("plugin/wezsesh/verbs/list_dirs.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings inside list_dirs.lua, got %d: %v", len(out), out)
	}
}

// TestDirProvidersOnly_OtherVerbBlocked: a require from inside another
// verb file (the regression we're guarding against) must fire.
func TestDirProvidersOnly_OtherVerbBlocked(t *testing.T) {
	src := []byte(`local dp = require("wezsesh.runtime.dir_providers")`)
	rule := DirProvidersOnly()
	out := lualint.LintBytes("plugin/wezsesh/verbs/save.lua", src, []lualint.Rule{rule})
	if len(out) != 1 {
		t.Fatalf("want 1 finding from save.lua, got %d: %v", len(out), out)
	}
	if out[0].RuleID != "lua-dir-providers-only" {
		t.Errorf("rule id: got %q", out[0].RuleID)
	}
}

// TestDirProvidersOnly_RuntimeNeighbourBlocked: another runtime module
// reaching into dir_providers also fires — the accessor is private to
// list_dirs as the only consumer.
func TestDirProvidersOnly_RuntimeNeighbourBlocked(t *testing.T) {
	src := []byte(`local dp = require("wezsesh.runtime.dir_providers")`)
	rule := DirProvidersOnly()
	out := lualint.LintBytes("plugin/wezsesh/runtime/state.lua", src, []lualint.Rule{rule})
	if len(out) != 1 {
		t.Fatalf("want 1 finding from runtime/state.lua, got %d: %v", len(out), out)
	}
}

// TestDirProvidersOnly_SingleQuoted: the rule matches single-quoted
// require strings the same as double-quoted.
func TestDirProvidersOnly_SingleQuoted(t *testing.T) {
	src := []byte(`local dp = require('wezsesh.runtime.dir_providers')`)
	rule := DirProvidersOnly()
	out := lualint.LintBytes("plugin/wezsesh/verbs/save.lua", src, []lualint.Rule{rule})
	if len(out) != 1 {
		t.Fatalf("want 1 finding on single-quoted require, got %d: %v", len(out), out)
	}
}

// TestDirProvidersOnly_UnrelatedRequire: a require for a different
// path doesn't fire.
func TestDirProvidersOnly_UnrelatedRequire(t *testing.T) {
	src := []byte(`local r = require("wezsesh.runtime.resurrect_ref")`)
	rule := DirProvidersOnly()
	out := lualint.LintBytes("plugin/wezsesh/verbs/save.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings on unrelated require, got %d: %v", len(out), out)
	}
}

// TestDirProvidersOnly_StringLiteralMention: a string mentioning the
// path inside a comment / string body does not fire — the lint matches
// the require call structure, not free-text occurrences.
func TestDirProvidersOnly_StringLiteralMention(t *testing.T) {
	src := []byte(`local s = "the wezsesh.runtime.dir_providers module is private"`)
	rule := DirProvidersOnly()
	out := lualint.LintBytes("plugin/wezsesh/verbs/save.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings on free-text mention, got %d: %v", len(out), out)
	}
}
