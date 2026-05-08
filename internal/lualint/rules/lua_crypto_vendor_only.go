package rules

import (
	"strings"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// cryptoDirPrefix is the path prefix every crypto-primitive module
// shares. Files under this prefix may only require Lua standard
// library modules (no `require` at all is fine), `wezterm`, and
// `wezsesh.vendor.*` (the vendored sha2). Any other `wezsesh.*`
// require pulls business logic into a primitive — flag it.
const cryptoDirPrefix = "plugin/wezsesh/crypto/"

// CryptoVendorOnly flags any `require("wezsesh.<X>")` inside the
// crypto/ directory where `<X>` is not under `wezsesh.vendor.*`.
// Crypto primitives are deliberately layered below business logic
// so a future audit / refactor of (say) the verb dispatcher cannot
// accidentally pull a side-effecting module into the HMAC compute
// path.
//
// Spec files (`*_spec.lua`) under crypto/ are exempt; their fixtures
// occasionally pull in `wezsesh.canonical_json` to cross-check the
// HMAC against canonical-encoded payloads.
func CryptoVendorOnly() lualint.Rule {
	return lualint.RuleFunc{
		RuleID: "lua-crypto-vendor-only",
		Fn: func(t *lualint.Tokens) []lualint.Finding {
			path := t.Path
			if !strings.Contains(path, cryptoDirPrefix) {
				return nil
			}
			if strings.HasSuffix(path, "_spec.lua") {
				return nil
			}
			var out []lualint.Finding
			for i := 0; i < len(t.All); i++ {
				if hit, ok := matchForbiddenWezseshRequire(t, i); ok {
					tk := t.All[hit]
					out = append(out, lualint.Finding{
						Path:     t.Path,
						Line:     tk.Line,
						Col:      tk.Col,
						RuleID:   "lua-crypto-vendor-only",
						Severity: lualint.SevError,
						Message: "crypto/* may only require wezsesh.vendor.*; " +
							"business modules belong above the crypto layer",
					})
				}
			}
			return out
		},
	}
}

// matchForbiddenWezseshRequire detects `require("wezsesh.X")` (or with
// single quotes) where `X` is not under `vendor.*`. Returns the token
// index of the require call's `require` identifier.
func matchForbiddenWezseshRequire(t *lualint.Tokens, i int) (int, bool) {
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
	if !strings.HasPrefix(inner, "wezsesh.") {
		return 0, false
	}
	if strings.HasPrefix(inner, "wezsesh.vendor.") {
		return 0, false
	}
	return i, true
}
