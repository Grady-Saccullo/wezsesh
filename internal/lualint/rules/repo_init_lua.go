package rules

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// initLuaPath is the path under repoRoot where the §9.1 init.lua
// lives. The two presence checks (package.loaded bust loop and
// resurrect_error.register) walk only this file.
var initLuaPath = filepath.Join("plugin", "init.lua")

// PackageLoadedBustLoopRule asserts that init.lua's apply_to_config
// body contains the `for k in pairs(package.loaded) do … if k:sub(1,
// 8) == "wezsesh." then package.loaded[k] = nil end end` cache-bust
// loop. Per §17.4 the loop MUST be present byte-stable; without it
// `Ctrl+Shift+R` reloads silently keep stale module caches and
// submodule edits don't land.
//
// We perform a structural check rather than a regex: extract the
// apply_to_config function body and confirm it contains the
// `package.loaded` cache-bust pattern (any prefix-comparison shape
// targeting "wezsesh." is acceptable; the spec pins the prefix
// length but plenty of equally-correct shapes exist that compare
// `string.sub(k, 1, 8)` or `k:match("^wezsesh%.")`).
func PackageLoadedBustLoopRule() RepoShapeRule {
	return repoRuleFunc{
		id: "lua-package-loaded-bust-loop",
		fn: func(repoRoot string) []lualint.Finding {
			path := filepath.Join(repoRoot, initLuaPath)
			body, err := readApplyToConfigBody(path)
			if err != nil {
				return []lualint.Finding{warning(path,
					"lua-package-loaded-bust-loop",
					fmt.Sprintf("could not read init.lua apply_to_config body: %v", err))}
			}
			if containsBustLoop(body) {
				return nil
			}
			return []lualint.Finding{{
				Path: path, Line: 1, Col: 1,
				RuleID:   "lua-package-loaded-bust-loop",
				Severity: lualint.SevError,
				Message: "init.lua apply_to_config MUST contain the package.loaded[\"wezsesh.*\"] cache-bust loop " +
					"(§9.1 / §17.4)",
			}}
		},
	}
}

// ResurrectErrorRegisterPresenceRule asserts that init.lua's
// apply_to_config body contains a call to
// `resurrect_error.register()` (or a require()'d alias whose final
// invocation is `<alias>.register()`). Per spike #2 / §17.4 absence
// of this call leaves the resurrect.error listener uninstalled and
// silently breaks the dual-path detector.
func ResurrectErrorRegisterPresenceRule() RepoShapeRule {
	return repoRuleFunc{
		id: "lua-resurrect-error-register-presence",
		fn: func(repoRoot string) []lualint.Finding {
			path := filepath.Join(repoRoot, initLuaPath)
			body, err := readApplyToConfigBody(path)
			if err != nil {
				return []lualint.Finding{warning(path,
					"lua-resurrect-error-register-presence",
					fmt.Sprintf("could not read init.lua apply_to_config body: %v", err))}
			}
			if containsResurrectErrorRegister(body) {
				return nil
			}
			return []lualint.Finding{{
				Path: path, Line: 1, Col: 1,
				RuleID:   "lua-resurrect-error-register-presence",
				Severity: lualint.SevError,
				Message: "init.lua apply_to_config MUST call resurrect_error.register() " +
					"(spike-#2 / §17.4)",
			}}
		},
	}
}

// readApplyToConfigBody returns the textual body of the
// `function M.apply_to_config(…)` declaration in path. Returns an
// error if the file cannot be read or the function declaration is
// not found.
func readApplyToConfigBody(path string) (string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	toks := lualint.Tokenise(path, src)
	all := toks.All
	for i := 0; i < len(all); i++ {
		// Find `function M.apply_to_config` (or any owner; the §9.1
		// contract spells M but a renamed module would still satisfy
		// the spirit).
		if all[i].Kind != lualint.TokKeyword || all[i].Value != "function" {
			continue
		}
		nameIdx := toks.NextNonTrivia(i)
		if nameIdx < 0 || all[nameIdx].Kind != lualint.TokIdent {
			continue
		}
		dotIdx := toks.NextNonTrivia(nameIdx)
		if dotIdx < 0 || all[dotIdx].Kind != lualint.TokPunct || all[dotIdx].Value != "." {
			continue
		}
		fnIdx := toks.NextNonTrivia(dotIdx)
		if fnIdx < 0 || all[fnIdx].Kind != lualint.TokIdent || all[fnIdx].Value != "apply_to_config" {
			continue
		}
		// Walk to the matching `end` keyword. Track nested function /
		// do / if / for blocks so we don't terminate on an inner end.
		// Body bytes start one past the closing `)` of the parameter
		// list.
		parenOpen := -1
		for j := fnIdx + 1; j < len(all); j++ {
			if all[j].Kind == lualint.TokPunct && all[j].Value == "(" {
				parenOpen = j
				break
			}
		}
		if parenOpen < 0 {
			return "", fmt.Errorf("apply_to_config: opening paren not found")
		}
		// Skip past matching `)`.
		depth := 1
		j := parenOpen + 1
		for ; j < len(all); j++ {
			tk := all[j]
			if tk.Kind != lualint.TokPunct {
				continue
			}
			if tk.Value == "(" {
				depth++
			} else if tk.Value == ")" {
				depth--
				if depth == 0 {
					break
				}
			}
		}
		if j >= len(all) {
			return "", fmt.Errorf("apply_to_config: closing paren not found")
		}
		bodyStartTok := j + 1
		// Walk to matching `end`. Lua block grammar:
		//   * function … end, if … end, do … end open a block that
		//     ends with `end`. `if` and `if … else … end` also use a
		//     single `end`.
		//   * repeat … until closes with `until`, NOT `end`.
		//   * for … do … end and while … do … end are CLOSED by the
		//     `end` that pairs with the inner `do` — so we increment
		//     on `do` but NOT on the leading `for`/`while`. Otherwise
		//     `for … do … end` would double-increment.
		blockDepth := 1
		k := bodyStartTok
		for ; k < len(all); k++ {
			tk := all[k]
			if tk.Kind != lualint.TokKeyword {
				continue
			}
			switch tk.Value {
			case "function", "if", "do":
				blockDepth++
			case "repeat":
				blockDepth++
			case "end":
				blockDepth--
				if blockDepth == 0 {
					goto done
				}
			case "until":
				blockDepth--
			}
		}
	done:
		if k >= len(all) {
			return "", fmt.Errorf("apply_to_config: terminating `end` not found")
		}
		// Compute the byte range from the first body token to the
		// `end` keyword. The tokeniser stores absolute offsets via
		// Line/Col only, so we instead reconstruct the body via
		// tokens' lexed bytes by scanning src with the bodyStart and
		// end positions from the token stream.
		startLine, startCol := all[bodyStartTok].Line, all[bodyStartTok].Col
		endLine, endCol := all[k].Line, all[k].Col
		return sliceLineRange(src, startLine, startCol, endLine, endCol), nil
	}
	return "", fmt.Errorf("apply_to_config: function not found")
}

// sliceLineRange returns the bytes of src spanning from
// (startLine, startCol) to (endLine, endCol) inclusive.
func sliceLineRange(src []byte, startLine, startCol, endLine, endCol int) string {
	startOff := -1
	endOff := len(src)
	line, col := 1, 1
	for i := 0; i < len(src); i++ {
		if line == startLine && col == startCol && startOff < 0 {
			startOff = i
		}
		if line == endLine && col == endCol {
			endOff = i
			break
		}
		if src[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	if startOff < 0 {
		return ""
	}
	if endOff < startOff {
		endOff = startOff
	}
	return string(src[startOff:endOff])
}

// containsBustLoop reports whether body contains the
// `package.loaded["wezsesh.*"]` cache-bust pattern. We accept three
// shapes that all satisfy the §9.1 invariant:
//
//	package.loaded[<key>] = nil ... + a "wezsesh" prefix gate
func containsBustLoop(body string) bool {
	if !strings.Contains(body, "package.loaded") {
		return false
	}
	if !strings.Contains(body, "wezsesh.") && !strings.Contains(body, "wezsesh%.") {
		return false
	}
	if !strings.Contains(body, "= nil") && !strings.Contains(body, "=nil") {
		return false
	}
	return true
}

// containsResurrectErrorRegister reports whether body invokes
// `<alias>.register()` against a `resurrect_error` module. Accepts
// `resurrect_error.register()` directly, or `<localvar>.register()`
// where <localvar> was bound via `require("wezsesh.resurrect_error")`.
func containsResurrectErrorRegister(body string) bool {
	if strings.Contains(body, "resurrect_error.register(") {
		return true
	}
	// Look for a require("wezsesh.resurrect_error") binding and a
	// `.register(` call on the same local. We scan both quoted forms
	// (double and single quote).
	for _, pat := range []string{
		`require("wezsesh.resurrect_error")`,
		`require('wezsesh.resurrect_error')`,
	} {
		idx := strings.Index(body, pat)
		if idx < 0 {
			continue
		}
		// Walk back to the nearest "local <name> =" that introduces
		// the binding.
		head := body[:idx]
		localIdx := strings.LastIndex(head, "local ")
		if localIdx < 0 {
			continue
		}
		// Extract the identifier between "local " and " =".
		decl := head[localIdx+len("local "):]
		eq := strings.Index(decl, "=")
		if eq < 0 {
			continue
		}
		name := strings.TrimSpace(decl[:eq])
		if name == "" {
			continue
		}
		if strings.Contains(body, name+".register(") {
			return true
		}
	}
	return false
}
