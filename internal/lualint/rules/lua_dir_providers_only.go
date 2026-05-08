package rules

import (
	"strings"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// dirProvidersOwnerSuffixes are the only Lua paths permitted to
// `require("wezsesh.runtime.dir_providers")`:
//   - the accessor module itself (source of truth);
//   - its spec;
//   - the `list_dirs` verb (the sole read consumer);
//   - the verb's spec;
//   - `plugin/init.lua` and its spec — apply_to_config calls
//     `dir_providers.set(opts.dir_providers)` to stash the user's list
//     once at config-eval time, mirroring the resurrect_ref.set pattern.
//
// Every other module that wants picker rows belongs above the verb
// dispatcher and routes through `list_dirs` — the lint catches a
// regression where another verb / runtime module reaches into the
// provider list directly and bypasses the pcall-isolation +
// log-surfacing that dir_providers.invoke_all owns.
var dirProvidersOwnerSuffixes = []string{
	"plugin/wezsesh/runtime/dir_providers.lua",
	"plugin/wezsesh/runtime/dir_providers_spec.lua",
	"plugin/wezsesh/verbs/list_dirs.lua",
	"plugin/wezsesh/verbs/list_dirs_spec.lua",
	"plugin/init.lua",
	"plugin/init_spec.lua",
}

// dirProvidersRequireTarget is the require-path string this rule gates.
const dirProvidersRequireTarget = "wezsesh.runtime.dir_providers"

// DirProvidersOnly flags any `require("wezsesh.runtime.dir_providers")`
// outside the two authorised consumers. Mirrors `ResurrectRefOnly` — a
// single accessor module is the canonical entry point, and a lint
// keeps that boundary from rotting silently.
//
// Spec files for the two owner modules are exempt — the spec for the
// list_dirs verb naturally pulls in the accessor to seed providers
// per test, and the accessor's own spec drives the API directly.
func DirProvidersOnly() lualint.Rule {
	return lualint.RuleFunc{
		RuleID: "lua-dir-providers-only",
		Fn: func(t *lualint.Tokens) []lualint.Finding {
			for _, suf := range dirProvidersOwnerSuffixes {
				if strings.HasSuffix(t.Path, suf) {
					return nil
				}
			}
			var out []lualint.Finding
			for i := 0; i < len(t.All); i++ {
				if hit, ok := matchDirProvidersRequire(t, i); ok {
					tk := t.All[hit]
					out = append(out, lualint.Finding{
						Path:     t.Path,
						Line:     tk.Line,
						Col:      tk.Col,
						RuleID:   "lua-dir-providers-only",
						Severity: lualint.SevError,
						Message: "require(\"" + dirProvidersRequireTarget +
							"\") MUST live in plugin/wezsesh/runtime/" +
							"dir_providers.lua or plugin/wezsesh/verbs/" +
							"list_dirs.lua; route picker-row reads through " +
							"the list_dirs verb",
					})
				}
			}
			return out
		},
	}
}

// matchDirProvidersRequire detects `require("wezsesh.runtime.dir_providers")`
// (single or double quotes). Returns the token index of the `require`
// identifier on a hit so the finding can be located precisely.
func matchDirProvidersRequire(t *lualint.Tokens, i int) (int, bool) {
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
	if inner != dirProvidersRequireTarget {
		return 0, false
	}
	return i, true
}
