package lualint

import (
	"path/filepath"
	"testing"
)

// TestLintNoRules: with zero rules registered, Lint MUST return zero
// findings and no error for every fixture in the corpus. Proves the
// integration shape of the API: tokenise + iterate is wired up
// correctly and rules are the only source of findings.
func TestLintNoRules(t *testing.T) {
	fixtures := []string{
		"short_string_escapes.lua",
		"long_string_levels.lua",
		"long_comment_punct.lua",
		"lineleading_paren_ambiguous.lua",
		"lineleading_paren_disambiguated.lua",
		"clean.lua",
	}
	for _, f := range fixtures {
		t.Run(f, func(t *testing.T) {
			path := filepath.Join("testdata", f)
			findings, err := Lint(path, nil)
			if err != nil {
				t.Fatalf("Lint(%s, nil): %v", path, err)
			}
			if len(findings) != 0 {
				t.Errorf("Lint(%s, nil): want 0 findings, got %d: %v", path, len(findings), findings)
			}
		})
	}
}

// TestLintRulePlugging registers an in-test rule that flags every
// identifier whose Value == "forbidden". This proves the Rule interface
// is wired to the token stream and that returned Findings carry
// position info from the tokeniser.
func TestLintRulePlugging(t *testing.T) {
	src := []byte(`local forbidden = 1
local ok = 2
local also_forbidden = forbidden + ok
return also_forbidden
`)
	rule := RuleFunc{
		RuleID: "test-forbidden-ident",
		Fn: func(toks *Tokens) []Finding {
			var out []Finding
			for _, tk := range toks.All {
				if tk.Kind == TokIdent && tk.Value == "forbidden" {
					out = append(out, Finding{
						Path:     toks.Path,
						Line:     tk.Line,
						Col:      tk.Col,
						RuleID:   "test-forbidden-ident",
						Severity: SevWarning,
						Message:  "identifier `forbidden` is banned by the test rule",
					})
				}
			}
			return out
		},
	}

	findings := LintBytes("inline.lua", src, []Rule{rule})
	if len(findings) != 2 {
		t.Fatalf("want 2 findings (line 1 col 7 and line 3 col 24-ish), got %d: %v", len(findings), findings)
	}
	for _, f := range findings {
		if f.Path != "inline.lua" {
			t.Errorf("path: got %q want %q", f.Path, "inline.lua")
		}
		if f.RuleID != "test-forbidden-ident" {
			t.Errorf("rule id: got %q", f.RuleID)
		}
	}
	// First finding is on line 1, second on line 3.
	if findings[0].Line != 1 {
		t.Errorf("first finding line: got %d want 1", findings[0].Line)
	}
	if findings[1].Line != 3 {
		t.Errorf("second finding line: got %d want 3", findings[1].Line)
	}
}

// TestLintReadError verifies that a missing file surfaces as an error
// (not a panic, not a silent zero-finding return).
func TestLintReadError(t *testing.T) {
	_, err := Lint(filepath.Join("testdata", "does-not-exist.lua"), nil)
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

// TestLintNilRuleSkipped guarantees a nil entry in the rules slice is
// silently skipped rather than panicking. Tooling that builds the rule
// list dynamically may include nils for disabled-via-config rules.
func TestLintNilRuleSkipped(t *testing.T) {
	src := []byte(`local x = 1`)
	findings := LintBytes("nil.lua", src, []Rule{nil, nil})
	if len(findings) != 0 {
		t.Errorf("want 0 findings, got %d: %v", len(findings), findings)
	}
}

// TestFindingString verifies the human-readable formatter so editor
// problem matchers stay stable.
func TestFindingString(t *testing.T) {
	f := Finding{
		Path:     "plugin/init.lua",
		Line:     12,
		Col:      4,
		RuleID:   "lua-handler-async",
		Severity: SevError,
		Message:  "wezterm.run_child_process inside (a)-(h)",
	}
	got := f.String()
	want := "plugin/init.lua:12:4: error: [lua-handler-async] wezterm.run_child_process inside (a)-(h)"
	if got != want {
		t.Errorf("Finding.String:\n got  %q\n want %q", got, want)
	}
}
