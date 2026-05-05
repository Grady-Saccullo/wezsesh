package rules

import (
	"testing"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// TestProvidersVendorOnly_AllowsWezterm: a wezterm require inside
// providers/ is allowed.
func TestProvidersVendorOnly_AllowsWezterm(t *testing.T) {
	src := []byte(`local wezterm = require("wezterm")`)
	rule := ProvidersVendorOnly()
	out := lualint.LintBytes("plugin/wezsesh/providers/zoxide.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings on wezterm require, got %d: %v", len(out), out)
	}
}

// TestProvidersVendorOnly_AllowsRuntimeLog: a runtime.log require
// inside providers/ is allowed.
func TestProvidersVendorOnly_AllowsRuntimeLog(t *testing.T) {
	src := []byte(`local log = require("wezsesh.runtime.log")`)
	rule := ProvidersVendorOnly()
	out := lualint.LintBytes("plugin/wezsesh/providers/zoxide.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings on runtime.log require, got %d: %v", len(out), out)
	}
}

// TestProvidersVendorOnly_BlocksRuntimeState: any require that isn't
// in the allow list fires — runtime.state in particular would let a
// provider reach into per-pane buckets.
func TestProvidersVendorOnly_BlocksRuntimeState(t *testing.T) {
	src := []byte(`local state = require("wezsesh.runtime.state")`)
	rule := ProvidersVendorOnly()
	out := lualint.LintBytes("plugin/wezsesh/providers/badprovider.lua", src, []lualint.Rule{rule})
	if len(out) != 1 {
		t.Fatalf("want 1 finding, got %d: %v", len(out), out)
	}
	if out[0].RuleID != "lua-providers-vendor-only" {
		t.Errorf("rule id: got %q", out[0].RuleID)
	}
}

// TestProvidersVendorOnly_BlocksResult: pulling result.lua into a
// provider would let it craft wire envelopes directly. Forbidden.
func TestProvidersVendorOnly_BlocksResult(t *testing.T) {
	src := []byte(`local result = require("wezsesh.result")`)
	rule := ProvidersVendorOnly()
	out := lualint.LintBytes("plugin/wezsesh/providers/zoxide.lua", src, []lualint.Rule{rule})
	if len(out) != 1 {
		t.Fatalf("want 1 finding, got %d: %v", len(out), out)
	}
}

// TestProvidersVendorOnly_OutsideProviders_Ignored: a non-allowlist
// require OUTSIDE providers/ is none of this rule's business.
func TestProvidersVendorOnly_OutsideProviders_Ignored(t *testing.T) {
	src := []byte(`local state = require("wezsesh.runtime.state")`)
	rule := ProvidersVendorOnly()
	out := lualint.LintBytes("plugin/wezsesh/ipc.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings outside providers/, got %d: %v", len(out), out)
	}
}

// TestProvidersVendorOnly_SpecExempt: spec files inside providers/
// are exempt — fixtures may pull in spec_helpers, b64, etc.
func TestProvidersVendorOnly_SpecExempt(t *testing.T) {
	src := []byte(`local helpers = require("spec_helpers")
local b64 = require("wezsesh.crypto.b64")`)
	rule := ProvidersVendorOnly()
	out := lualint.LintBytes("plugin/wezsesh/providers/zoxide_spec.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings inside spec file, got %d: %v", len(out), out)
	}
}

// TestProvidersVendorOnly_NonWezsesh_Blocked: requiring an arbitrary
// stdlib-or-third-party module from a provider is also flagged.
// Providers are an extension surface, not an entry point for ad-hoc
// dependencies — anything more than wezterm + runtime.log goes into
// the user's own provider code, not into wezsesh-bundled providers.
func TestProvidersVendorOnly_NonWezsesh_Blocked(t *testing.T) {
	src := []byte(`local socket = require("socket")`)
	rule := ProvidersVendorOnly()
	out := lualint.LintBytes("plugin/wezsesh/providers/zoxide.lua", src, []lualint.Rule{rule})
	if len(out) != 1 {
		t.Fatalf("want 1 finding on non-allowlist require, got %d: %v", len(out), out)
	}
}
