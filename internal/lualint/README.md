# lualint

Project-specific CI lints for wezsesh. Enforces the load-bearing invariants
declared in `CLAUDE.md` and `design.md` (§16.5 / §17.4) as fail-closed,
machine-checkable rules. The package compiles into the `lualint` binary
(`cmd/lualint`) and runs in CI plus locally via `go run ./cmd/lualint plugin/`.

## Why this exists

Most rules here guard invariants Go's type system cannot express and a human
reviewer will eventually miss. Many of them have wire-silent or CVE-class
failure modes when violated:

- A verb module shipped without `args_shape` → canonical-JSON re-encode picks
  up `nil` → HMAC verify fails on the binary side → `IPC_TIMEOUT` with no
  stack trace.
- `\n` slipping into the trust hash construction → trust-bound shell exec
  collisions across distinct paths.
- `wezterm.GLOBAL.foo = { nested = "table" }` → mlua snapshots GLOBAL on read,
  partial mutation is silently discarded, state vanishes across reloads.
- `exec.Command("wezterm", ...)` outside `internal/wezcli/` → bypasses the
  single mutation-control point for the wezterm CLI surface.

Off-the-shelf linters do not know these contracts. The package is essentially
`CLAUDE.md` compiled into Go: every load-bearing invariant has a corresponding
rule that fails CI at the commit that introduced the violation, instead of in
production weeks later.

## Layout

```
internal/lualint/
├── lualint.go          Rule / Finding / Severity types; Lint, LintBytes
├── tokeniser.go        Minimal Lua lexer producing *Tokens
├── async_funcs.go      Curated set of wezterm async APIs (§14.3)
└── rules/
    ├── rules.go        GoRule, RepoShapeRule contracts; AllGoRules /
    │                   AllLuaRules / AllRepoRules registries
    ├── exemptions.go   Path-based whitelists (restrictedPkgs, isWezcliPackage,
    │                   isSafefsPackage, …) and per-file accepted findings
    ├── srcpos.go       Line/col conversion + Go comment/string stripping for
    │                   grep-style Go rules (goSpans, indexAllInCode)
    ├── lua_*.go        Per-file Lua rules
    ├── go_*.go         Per-file Go rules
    ├── repo_*.go       Cross-file invariants run once per invocation
    └── *_test.go       Positive + negative + exemption fixtures per rule

cmd/lualint/main.go     Driver: walks repo, dispatches by file kind,
                        sorts findings, exits non-zero on SevError
```

## The three rule kinds

| Kind | Defined in | Input | Used for |
|---|---|---|---|
| `lualint.Rule` | `lualint.go` | `*Tokens` | Per-file Lua lints |
| `rules.GoRule` | `rules/rules.go` | `(path, src, fset, *ast.File)` | Per-file Go lints |
| `rules.RepoShapeRule` | `rules/rules.go` | `repoRoot string` | Cross-file invariants run once per invocation |

Each kind has a registry function (`AllLuaRules`, `AllGoRules`, `AllRepoRules`).
Adding a rule = drop a file in `rules/`, append to one slice.

## Driver flow

`cmd/lualint/main.go`:

1. `filepath.WalkDir` from root. Skips dot-prefixed dirs, `vendor/`, `testdata/`.
2. `.go` files → `lintGoFile` parses with `go/parser` (parse failure tolerated;
   `f` will be `nil`) and runs every `GoRule`.
3. `.lua` files under `plugin/` → `lintLuaFile` tokenises and runs every Lua
   `Rule`. `*_spec.lua` and `plugin/wezsesh/vendor/` are skipped.
4. After the walk, every `RepoShapeRule.Check(absRoot)` runs once.
5. Findings are stably sorted by `(path, line, col, ruleID)` and printed as
   `path:line:col: severity: [rule-id] message` — the standard editor
   problem-matcher form (Vim quickfix, VSCode, GitHub annotations).
6. Exit 1 if any `SevError` finding; exit 0 otherwise (pure warnings pass).

The `--rule=id1,id2` flag filters which rules run. Empty filter = all rules.

## The Lua tokeniser

`tokeniser.go` is a hand-written minimal lexer — *not* a parser. It produces
`TokIdent`, `TokKeyword`, `TokNumber`, `TokString`, `TokLongStr`, `TokLineCmt`,
`TokLongCmt`, `TokPunct`, `TokNewline`, `TokWhitespace`, `TokInvalid`, `TokEOF`.

Properties relied on by rules:

- **Multi-char operators collapse into a single `TokPunct`** (`..`, `==`, `::`,
  `>=`, `<=`, `~=`, `//`, `<<`, `>>`, `...`).
- **Long strings/comments** (`[==[…]==]` / `--[==[…]==]`) are matched with
  equals-count tracking.
- **Never errors.** An unterminated string yields `TokInvalid` spanning the
  bad region so rules keep working on broken input.
- **Position discipline.** `Line` / `Col` are 1-based; `EndLine` / `EndCol`
  point one past the final byte.

Helpers exposed to rules:

- `Tokens.NextNonTrivia(idx)` / `PrevNonTrivia(idx)` — skip whitespace,
  newlines, line-comments, long-comments.
- `Tokens.IsLineLeading(idx)` — first non-whitespace token on its line.
- `Tokens.IsExpressionEnd(idx)` — token could end a Lua expression
  (ident, string, number, `)`, `]`, `...`, `nil`, `true`, `false`, `end`).
- `Tokens.LineLeadingParenAfterExprEnd(idx)` — the §9.0.1.1 ambiguity check
  used by `lua-line-leading-paren`.

## Severity and findings

```go
type Finding struct {
    Path     string
    Line     int       // 1-based
    Col      int       // 1-based
    RuleID   string    // stable, used in output and --rule= filter
    Severity Severity  // SevWarning or SevError
    Message  string    // human-readable; cite the §-section
}
```

`SevError` causes the driver to exit non-zero. Use `SevWarning` for cases
where the rule cannot fully run (e.g. partial tree, missing fixture file)
rather than has-run-and-failed — there is a `warning(path, ruleID, msg)`
constructor in `rules/rules.go` for that.

## Path-based exemptions

`rules/exemptions.go` is the policy boundary. The pattern is consistent across
the codebase: a rule that bans a primitive *exempts the package that
implements the boundary the rule points at*.

```go
restrictedPkgs                  // §16.5 set; rules apply here
isUnderPath(p, prefixes...)     // directory-boundary-aware prefix match
                                // ("internal/ipc" ≠ "internal/ipcsock")
isRestrictedPkg(path)           // shorthand for the §16.5 set
isTestFile(path)                // _test.go suffix
isWezcliPackage(path)           // implementation of the wezterm-CLI boundary
isSafefsPackage(path)           // implementation of the file-write boundary
isIpcdispatcherPackage(path)    // implementation of the dispatcher boundary
isLoggerPackage(path)           // implementation of the logger boundary
isLuaLintTooling(path)          // this package; allowed to print + write to disk
isCodegenTool(path)             // codegen utilities under internal/argvallow/codegen
isE2EHarness(path)              // //go:build e2e harness
```

There are also three single-file accepted findings (`isUservarWriter`,
`isSnapshotsRepo`, `isSafefsNetfs`) — each documented inline with the rationale
and the design-conformance review (T-NNN) that approved it. New per-file
exemptions follow the same pattern: name the function after the file, comment
with the rationale and the §-citation.

## Running locally

```bash
# Run every rule against the whole repo.
go run ./cmd/lualint plugin/

# Run a single rule.
go run ./cmd/lualint --rule=lua-globals-only plugin/

# Run multiple rules.
go run ./cmd/lualint --rule=lua-debug-ban,lua-dofile-ban plugin/

# Run the package's own tests.
go test ./internal/lualint/...
```

The CI invocation is identical to the first form (`go run ./cmd/lualint plugin/`).

## Adding a new rule

Three recipes follow — one per rule kind. The pattern is the same: small file,
small function, ID matches filename, test file alongside.

### Adding a Lua rule (`lualint.Rule`)

Token-walk the stream. The `lua-globals-only` rule is the canonical example.

`rules/lua_my_rule.go`:

```go
package rules

import (
    "github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// MyRule flags <pattern>. Per §X.Y the pattern is forbidden because <reason>;
// failure mode is <wire-silent / sandbox-escape / CVE-class>.
func MyRule() lualint.Rule {
    return lualint.RuleFunc{
        RuleID: "lua-my-rule",
        Fn: func(t *lualint.Tokens) []lualint.Finding {
            // 1. Path-based exemption (if the rule has owner files).
            //    e.g. for _, suffix := range ownerSuffixes {
            //             if strings.HasSuffix(t.Path, suffix) { return nil }
            //         }

            // 2. Token walk looking for the structural pattern.
            var out []lualint.Finding
            for i := 0; i < len(t.All); i++ {
                tk := t.All[i]
                if tk.Kind != lualint.TokIdent || tk.Value != "needle" {
                    continue
                }
                // Use NextNonTrivia / PrevNonTrivia to stitch
                // multi-token patterns. Never index t.All by a hardcoded
                // offset — comments and whitespace will break it.
                next := t.NextNonTrivia(i)
                if next < 0 || t.All[next].Value != "." {
                    continue
                }
                out = append(out, lualint.Finding{
                    Path:     t.Path,
                    Line:     tk.Line,
                    Col:      tk.Col,
                    RuleID:   "lua-my-rule",
                    Severity: lualint.SevError,
                    Message:  "<concrete remediation>; (§X.Y)",
                })
            }
            return out
        },
    }
}
```

Register the rule in `rules/rules.go::AllLuaRules`:

```go
func AllLuaRules() []lualint.Rule {
    return []lualint.Rule{
        // ...existing rules...
        MyRule(),
    }
}
```

**Why token-driven instead of regex:** structural matching ignores prose
mentions inside comments and string literals automatically. A regex over raw
bytes would over-match. See `TestGlobalsOnly_StringMention` for the
test that locks this property in.

### Adding a Go rule (`GoRule`)

Two flavours:

**AST-based** when you need real syntactic context (call sites, function
declarations, defer chains). Parse failures are tolerated; the driver passes
`f == nil` if parsing failed, so guard for it.

**Byte-grep with comment/string stripping** when the check is "did anyone type
this needle outside the owner package?" Use `indexAllInCode(src, "needle")`
from `srcpos.go` — it returns offsets in code regions only, skipping comments
and string literals. Then `posLineCol(src, off)` for the position.

`rules/go_my_rule.go` (grep-style):

```go
package rules

import (
    "go/ast"
    "go/token"

    "github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// MyGoRule flags <pattern> outside <owner package>. Per §16.5 every
// <primitive> call MUST live inside <owner> so the boundary is single-point.
func MyGoRule() GoRule {
    const id = "go-my-rule"
    return goRuleFunc{
        id: id,
        fn: func(path string, src []byte, _ *token.FileSet, _ *ast.File) []lualint.Finding {
            // 1. Path-based exemption.
            if isOwnerPackage(path) || isTestFile(path) {
                return nil
            }
            // 2. Scan code regions only.
            var out []lualint.Finding
            for _, off := range indexAllInCode(src, "needle.Call(") {
                line, col := posLineCol(src, off)
                out = append(out, lualint.Finding{
                    Path:     path,
                    Line:     line,
                    Col:      col,
                    RuleID:   id,
                    Severity: lualint.SevError,
                    Message:  "<remediation>; (§16.5)",
                })
            }
            return out
        },
    }
}
```

Register in `rules/rules.go::AllGoRules`.

For AST-style rules (e.g. `go_tea_after.go`, `go_goroutine_recover.go`), walk
`f` with `ast.Inspect`. The `fset` is needed for `fset.Position(node.Pos())`
when emitting findings.

### Adding a repo-shape rule (`RepoShapeRule`)

Use this when the invariant is "across multiple files at known paths" — e.g.
"every verb module exports both fields", "init.lua contains the bust loop".

`rules/repo_my_rule.go`:

```go
package rules

import (
    "fmt"
    "os"
    "path/filepath"

    "github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

func MyRepoRule() RepoShapeRule {
    return repoRuleFunc{
        id: "lua-my-repo-rule",
        fn: func(repoRoot string) []lualint.Finding {
            target := filepath.Join(repoRoot, "plugin", "wezsesh", "x.lua")
            src, err := os.ReadFile(target)
            if err != nil {
                // Use warning(...) for partial-tree / missing-file cases —
                // SevWarning does not break CI.
                return []lualint.Finding{warning(target, "lua-my-repo-rule",
                    fmt.Sprintf("could not read target: %v", err))}
            }
            toks := lualint.Tokenise(target, src)
            // ...check structural invariants over toks...
            _ = toks
            return nil
        },
    }
}
```

Register in `rules/rules.go::AllRepoRules`.

## Writing tests

Every rule has `*_test.go` next to it with **at least three fixtures**:

1. **Positive** — input that should fire; assert one or more findings, check
   `RuleID` matches.
2. **Negative** — input that should NOT fire; assert zero findings.
3. **Exemption** — owner package / file gets a free pass; assert zero findings.

Lua-rule tests use `lualint.LintBytes(path, src, []lualint.Rule{rule})`. The
`path` argument matters because most rules path-key their exemptions —
`plugin/wezsesh/runtime/globals.lua` is exempt from `lua-globals-only`,
everything else under `plugin/` is not.

```go
func TestMyRule_Positive(t *testing.T) {
    src := []byte(`local k = wezterm.GLOBAL.wezsesh_session_key`)
    rule := MyRule()
    out := lualint.LintBytes("plugin/wezsesh/ipc.lua", src, []lualint.Rule{rule})
    if len(out) != 1 {
        t.Fatalf("want 1 finding, got %d: %v", len(out), out)
    }
    if out[0].RuleID != "lua-my-rule" {
        t.Errorf("rule id: got %q", out[0].RuleID)
    }
}

func TestMyRule_OwnerExempt(t *testing.T) {
    src := []byte(`wezterm.GLOBAL.wezsesh_session_key = "deadbeef"`)
    rule := MyRule()
    out := lualint.LintBytes("plugin/wezsesh/runtime/globals.lua", src, []lualint.Rule{rule})
    if len(out) != 0 {
        t.Errorf("want 0 findings inside owner, got %d: %v", len(out), out)
    }
}
```

Go-rule tests use the shared `mustParseGo(t, src)` helper from
`go_fofd_setlk_test.go`:

```go
func TestMyGoRule_Positive(t *testing.T) {
    src := []byte(`package state

import "os/exec"

func list() ([]byte, error) {
    return exec.Command("wezterm", "cli", "list").Output()
}
`)
    rule := MyGoRule()
    fset, f := mustParseGo(t, src)
    out := rule.Check("internal/state/foo.go", src, fset, f)
    if len(out) == 0 {
        t.Fatalf("want >=1 finding, got 0")
    }
}
```

Repo-shape tests build a temp tree with the shared `mustWriteFile` helper from
`test_helpers_test.go`:

```go
func TestMyRepoRule(t *testing.T) {
    root := t.TempDir()
    mustWriteFile(t, filepath.Join(root, "plugin", "wezsesh", "x.lua"), `
local M = {}
return M
`)
    rule := MyRepoRule()
    out := rule.Check(root)
    if len(out) != 0 {
        t.Errorf("want 0 findings, got %d: %v", len(out), out)
    }
}
```

## Conventions

- **Rule IDs are stable and short.** `lua-<noun>` for Lua rules,
  `go-<noun>` for Go rules, `lua-<noun>` for repo-shape rules whose target is
  Lua. The ID appears in CI output, in `--rule=` filters, and in the agent
  handoff table in `CLAUDE.md`; renaming it is a breaking change.
- **One rule per file.** Filename matches the constructor: `lua_globals_only.go`
  → `GlobalsOnly()`. Keep rules small and self-describing.
- **Cite the §-section in the rule's doc comment AND its message text.** The
  message is what shows up in CI; readers should be able to find the design
  rationale without leaving the diagnostic.
- **Default fail-CLOSED.** New rules emit `SevError` unless there is a
  specific reason to use `SevWarning` (partial tree, can't-run, informational).
- **No regex.** Lua rules walk tokens; Go rules walk AST or use
  `indexAllInCode` (which strips comments and strings). Regex over raw bytes
  produces false positives in doc comments and example strings.
- **Exemptions are policy.** Add new exemptions to `exemptions.go` with a
  comment block explaining the rationale and the design-conformance review
  that approved them. The exemption list is reviewable on its own.

## When a rule should NOT be added here

- **It restates a check `staticcheck` or `govulncheck` already performs.**
  Defer to upstream.
- **It enforces stylistic preference, not an invariant.** This package is for
  CVE-class / wire-silent / boundary-violating patterns; style nits live
  elsewhere.
- **It needs cross-package data flow.** The tokeniser is single-file and
  intentionally shallow. If a rule wants "X is called inside a goroutine
  spawned in package Y", it needs a real Go AST analyser — write it as a
  separate `cmd/<name>` tool and run it alongside `lualint` in CI.
