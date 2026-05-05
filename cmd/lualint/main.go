// Command lualint runs the wezsesh CI lints declared in §16.5 / §17.4.
//
// Usage:
//
//	lualint [--rule=<id>[,<id>...]] [<root>]
//
// Walks <root> (default ".") and dispatches each .go file in the
// §16.5 restricted-package set through the Go-targeting rules and
// each .lua file under plugin/ through the Lua-targeting rules. The
// repo-shape rules (verb / shape parity, init.lua presence checks)
// run once per invocation.
//
// Exit status:
//
//	0 — no findings (or only SevWarning findings)
//	1 — at least one SevError finding
//
// Output is one finding per line in the standard editor problem-
// matcher form: `path:line:col: severity: [rule] message`.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
	"github.com/Grady-Saccullo/wezsesh/internal/lualint/rules"
)

// skipDirs is the set of top-level directory names that the walker
// always skips. Anything beginning with `.` is also skipped.
var skipDirs = map[string]bool{
	"vendor":   true,
	"testdata": true,
}

func main() {
	var ruleFilter string
	flag.StringVar(&ruleFilter, "rule", "",
		"comma-separated rule IDs to run (default: all)")
	flag.Parse()

	root := "."
	if flag.NArg() > 0 {
		root = flag.Arg(0)
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lualint: resolve root: %v\n", err)
		os.Exit(2)
	}

	enabled := buildEnabledSet(ruleFilter)

	goRules := selectGoRules(rules.AllGoRules(), enabled)
	luaRules := selectLuaRules(rules.AllLuaRules(), enabled)
	repoRules := selectRepoRules(rules.AllRepoRules(), enabled)

	var findings []lualint.Finding

	// Per-file walk.
	walkErr := filepath.WalkDir(absRoot, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			name := d.Name()
			if p == absRoot {
				return nil
			}
			if strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			if skipDirs[name] {
				return fs.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(absRoot, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		switch {
		case strings.HasSuffix(rel, ".go"):
			ff, err := lintGoFile(p, rel, goRules)
			if err != nil {
				fmt.Fprintf(os.Stderr, "lualint: %s: %v\n", rel, err)
			}
			findings = append(findings, ff...)
		case strings.HasSuffix(rel, ".lua") && strings.HasPrefix(rel, "plugin/"):
			// Skip non-source spec files: the Lua spec harness is
			// allowed to use _G, debug, etc. for shim wiring.
			if strings.HasSuffix(rel, "_spec.lua") {
				return nil
			}
			// Skip vendored Lua under plugin/wezsesh/vendor.
			if strings.HasPrefix(rel, "plugin/wezsesh/vendor/") {
				return nil
			}
			ff, err := lintLuaFile(p, rel, luaRules)
			if err != nil {
				fmt.Fprintf(os.Stderr, "lualint: %s: %v\n", rel, err)
			}
			findings = append(findings, ff...)
		}
		return nil
	})
	if walkErr != nil {
		fmt.Fprintf(os.Stderr, "lualint: walk: %v\n", walkErr)
		os.Exit(2)
	}

	// Repo-shape rules.
	for _, r := range repoRules {
		findings = append(findings, r.Check(absRoot)...)
	}

	sortFindings(findings)
	hasError := false
	for _, f := range findings {
		fmt.Println(f.String())
		if f.Severity == lualint.SevError {
			hasError = true
		}
	}
	if hasError {
		os.Exit(1)
	}
}

// buildEnabledSet returns the set of rule IDs the user asked for.
// An empty filter enables every rule.
func buildEnabledSet(filter string) map[string]bool {
	if filter == "" {
		return nil
	}
	out := make(map[string]bool)
	for _, id := range strings.Split(filter, ",") {
		id = strings.TrimSpace(id)
		if id != "" {
			out[id] = true
		}
	}
	return out
}

// enabledFor reports whether rule id should run given the filter set.
// A nil set (no filter) means "every rule is enabled".
func enabledFor(set map[string]bool, id string) bool {
	if set == nil {
		return true
	}
	return set[id]
}

func selectGoRules(in []rules.GoRule, set map[string]bool) []rules.GoRule {
	var out []rules.GoRule
	for _, r := range in {
		if enabledFor(set, r.ID()) {
			out = append(out, r)
		}
	}
	return out
}

func selectLuaRules(in []lualint.Rule, set map[string]bool) []lualint.Rule {
	var out []lualint.Rule
	for _, r := range in {
		if enabledFor(set, r.ID()) {
			out = append(out, r)
		}
	}
	return out
}

func selectRepoRules(in []rules.RepoShapeRule, set map[string]bool) []rules.RepoShapeRule {
	var out []rules.RepoShapeRule
	for _, r := range in {
		if enabledFor(set, r.ID()) {
			out = append(out, r)
		}
	}
	return out
}

// lintGoFile runs every Go-targeting rule against the file at absPath.
// rel is the path relative to repo root (used for restricted-package
// classification — rules consume it as the canonical path).
func lintGoFile(absPath, rel string, goRules []rules.GoRule) ([]lualint.Finding, error) {
	src, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var f *ast.File
	parsed, perr := parser.ParseFile(fset, rel, src, parser.ParseComments)
	if perr == nil {
		f = parsed
	}
	var out []lualint.Finding
	for _, r := range goRules {
		out = append(out, r.Check(rel, src, fset, f)...)
	}
	return out, nil
}

// lintLuaFile runs every Lua-targeting rule against the file at
// absPath. rel is the path relative to repo root.
func lintLuaFile(absPath, rel string, luaRules []lualint.Rule) ([]lualint.Finding, error) {
	src, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}
	return lualint.LintBytes(rel, src, luaRules), nil
}

// sortFindings stably sorts findings by (path, line, col, ruleID) so
// CI output is deterministic across runs.
func sortFindings(in []lualint.Finding) {
	sort.SliceStable(in, func(i, j int) bool {
		a, b := in[i], in[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Col != b.Col {
			return a.Col < b.Col
		}
		return a.RuleID < b.RuleID
	})
}
