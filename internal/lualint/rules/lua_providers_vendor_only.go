package rules

import (
	"strings"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// providersDirPrefix is the path prefix every directory-row provider
// module shares. Files under this prefix may only require `wezterm`
// and `wezsesh.runtime.log` — anything else (state, result, the ipc
// stack, runtime/globals, etc.) would pull plugin internals into the
// public extension surface and is forbidden.
//
// Mirrors `cryptoDirPrefix` / `CryptoVendorOnly` in shape: providers
// are deliberately layered as a leaf, with no inbound edges from the
// rest of the plugin and only two outbound dependencies. The lint
// keeps that boundary intact.
const providersDirPrefix = "plugin/wezsesh/providers/"

// providersAllowedRequires is the exhaustive list of `require(...)`
// targets a provider module may import. wezterm is required for
// `wezterm.run_child_process` (the I/O hatch); runtime/log is the
// log-warn / log-error sink the bundled provider writes to on
// failures.
var providersAllowedRequires = map[string]bool{
	"wezterm":              true,
	"wezsesh.runtime.log":  true,
}

// ProvidersVendorOnly flags any `require("X")` inside the providers/
// directory where `X` is not in the allow list above. Spec files
// (`*_spec.lua`) under providers/ are exempt; their fixtures pull in
// `spec_helpers` and a base64 decoder to assert reply envelopes.
func ProvidersVendorOnly() lualint.Rule {
	return lualint.RuleFunc{
		RuleID: "lua-providers-vendor-only",
		Fn: func(t *lualint.Tokens) []lualint.Finding {
			path := t.Path
			if !strings.Contains(path, providersDirPrefix) {
				return nil
			}
			if strings.HasSuffix(path, "_spec.lua") {
				return nil
			}
			var out []lualint.Finding
			for i := 0; i < len(t.All); i++ {
				if hit, ok := matchProviderForbiddenRequire(t, i); ok {
					tk := t.All[hit]
					out = append(out, lualint.Finding{
						Path:     t.Path,
						Line:     tk.Line,
						Col:      tk.Col,
						RuleID:   "lua-providers-vendor-only",
						Severity: lualint.SevError,
						Message: "providers/* may only require \"wezterm\" and " +
							"\"wezsesh.runtime.log\"; business modules belong " +
							"above the providers layer",
					})
				}
			}
			return out
		},
	}
}

// matchProviderForbiddenRequire detects `require("X")` (or with single
// quotes) where `X` is not in the providers allow list. Returns the
// token index of the require call's `require` identifier on a hit.
func matchProviderForbiddenRequire(t *lualint.Tokens, i int) (int, bool) {
	tk := t.All[i]
	if tk.Kind != lualint.TokIdent || tk.Value != "require" {
		return 0, false
	}
	parenIdx := t.NextNonTrivia(i)
	if parenIdx < 0 || t.All[parenIdx].Kind != lualint.TokPunct || t.All[parenIdx].Value != "(" {
		return 0, false
	}
	strIdx := t.NextNonTrivia(parenIdx)
	if strIdx < 0 || t.All[strIdx].Kind != lualint.TokString {
		return 0, false
	}
	literal := t.All[strIdx].Value
	if len(literal) < 2 {
		return 0, false
	}
	first, last := literal[0], literal[len(literal)-1]
	if (first != '"' && first != '\'') || first != last {
		return 0, false
	}
	inner := literal[1 : len(literal)-1]
	if providersAllowedRequires[inner] {
		return 0, false
	}
	return i, true
}
