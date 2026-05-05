package rules

import (
	"testing"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// TestCryptoVendorOnly_Allows_Vendor: a require for wezsesh.vendor.*
// inside crypto/ is allowed.
func TestCryptoVendorOnly_Allows_Vendor(t *testing.T) {
	src := []byte(`local sha = require("wezsesh.vendor.sha2")`)
	rule := CryptoVendorOnly()
	out := lualint.LintBytes("plugin/wezsesh/crypto/hmac.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings on vendor require, got %d: %v", len(out), out)
	}
}

// TestCryptoVendorOnly_Blocks_Business: a require for a non-vendor
// wezsesh module from inside crypto/ fires.
func TestCryptoVendorOnly_Blocks_Business(t *testing.T) {
	src := []byte(`local log = require("wezsesh.runtime.log")`)
	rule := CryptoVendorOnly()
	out := lualint.LintBytes("plugin/wezsesh/crypto/hmac.lua", src, []lualint.Rule{rule})
	if len(out) != 1 {
		t.Fatalf("want 1 finding on non-vendor require, got %d: %v", len(out), out)
	}
	if out[0].RuleID != "lua-crypto-vendor-only" {
		t.Errorf("rule id: got %q", out[0].RuleID)
	}
}

// TestCryptoVendorOnly_OutsideCrypto_Ignored: a non-vendor wezsesh
// require OUTSIDE crypto/ is none of this rule's business.
func TestCryptoVendorOnly_OutsideCrypto_Ignored(t *testing.T) {
	src := []byte(`local log = require("wezsesh.runtime.log")`)
	rule := CryptoVendorOnly()
	out := lualint.LintBytes("plugin/wezsesh/ipc.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings outside crypto/, got %d: %v", len(out), out)
	}
}

// TestCryptoVendorOnly_NonWezsesh_Allowed: a require for a stdlib
// module (or anything else not prefixed with `wezsesh.`) is allowed.
func TestCryptoVendorOnly_NonWezsesh_Allowed(t *testing.T) {
	src := []byte(`local bit = require("bit")
local lpeg = require("lpeg")`)
	rule := CryptoVendorOnly()
	out := lualint.LintBytes("plugin/wezsesh/crypto/b64.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings on non-wezsesh require, got %d: %v", len(out), out)
	}
}

// TestCryptoVendorOnly_SpecExempt: spec files inside crypto/ are
// exempt — fixtures occasionally pull in canonical_json to cross-
// check encoded payloads.
func TestCryptoVendorOnly_SpecExempt(t *testing.T) {
	src := []byte(`local cj = require("wezsesh.canonical_json")`)
	rule := CryptoVendorOnly()
	out := lualint.LintBytes("plugin/wezsesh/crypto/hmac_spec.lua", src, []lualint.Rule{rule})
	if len(out) != 0 {
		t.Errorf("want 0 findings inside spec file, got %d: %v", len(out), out)
	}
}
