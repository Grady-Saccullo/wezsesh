package rules

import "testing"

// TestWeztermCLIRule_Positive: exec.Command("wezterm", …) outside
// internal/wezcli fires.
func TestWeztermCLIRule_Positive(t *testing.T) {
	src := []byte(`package state

import "os/exec"

func list() ([]byte, error) {
	return exec.Command("wezterm", "cli", "list").Output()
}
`)
	rule := WeztermCLIRule()
	fset, f := mustParseGo(t, src)
	out := rule.Check("internal/state/foo.go", src, fset, f)
	if len(out) == 0 {
		t.Fatalf("want >=1 finding, got 0")
	}
	if out[0].RuleID != "go-wezterm-cli-boundary" {
		t.Errorf("rule id: got %q", out[0].RuleID)
	}
}

// TestWeztermCLIRule_Positive_Context: same shape with
// exec.CommandContext also fires.
func TestWeztermCLIRule_Positive_Context(t *testing.T) {
	src := []byte(`package state

import (
	"context"
	"os/exec"
)

func run(ctx context.Context) ([]byte, error) {
	return exec.CommandContext(ctx, "wezterm", "cli", "list").Output()
}
`)
	rule := WeztermCLIRule()
	fset, f := mustParseGo(t, src)
	out := rule.Check("internal/state/foo.go", src, fset, f)
	if len(out) == 0 {
		t.Fatalf("want >=1 finding for exec.CommandContext, got 0")
	}
}

// TestWeztermCLIRule_Negative_Wezcli: same call inside
// internal/wezcli is exempt.
func TestWeztermCLIRule_Negative_Wezcli(t *testing.T) {
	src := []byte(`package wezcli

import "os/exec"

func run() ([]byte, error) {
	return exec.Command("wezterm", "cli", "list").Output()
}
`)
	rule := WeztermCLIRule()
	fset, f := mustParseGo(t, src)
	out := rule.Check("internal/wezcli/client.go", src, fset, f)
	if len(out) != 0 {
		t.Errorf("want 0 findings inside wezcli, got %d: %v", len(out), out)
	}
}

// TestWeztermCLIRule_Negative_NonLiteralBinary: exec.Command(c.binary,
// args...) — first arg is a variable, not the literal "wezterm" — does
// NOT fire. The §16.5 grep is specifically literal-string scoped.
func TestWeztermCLIRule_Negative_NonLiteralBinary(t *testing.T) {
	src := []byte(`package state

import "os/exec"

func run(bin string, args []string) ([]byte, error) {
	return exec.Command(bin, args...).Output()
}
`)
	rule := WeztermCLIRule()
	fset, f := mustParseGo(t, src)
	out := rule.Check("internal/state/foo.go", src, fset, f)
	if len(out) != 0 {
		t.Errorf("want 0 findings for non-literal binary, got %d: %v", len(out), out)
	}
}
