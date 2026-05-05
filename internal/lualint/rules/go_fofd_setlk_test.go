package rules

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/Grady-Saccullo/wezsesh/internal/lualint"
)

// TestFOFDSetlkRule_Positive: a reference to unix.F_OFD_SETLK from any
// file other than internal/safefs/lock_linux.go fires the rule.
func TestFOFDSetlkRule_Positive(t *testing.T) {
	src := []byte(`package foo

import "golang.org/x/sys/unix"

func bar(fd uintptr) {
	_ = unix.F_OFD_SETLK
}
`)
	rule := FOFDSetlkRule()
	fset, f := mustParseGo(t, src)
	out := rule.Check("internal/state/foo.go", src, fset, f)
	if len(out) == 0 {
		t.Fatalf("want >=1 finding, got 0")
	}
	for _, fnd := range out {
		if fnd.RuleID != "go-fofd-setlk-build-tag" {
			t.Errorf("rule id: got %q", fnd.RuleID)
		}
		if fnd.Severity != lualint.SevError {
			t.Errorf("severity: got %v want SevError", fnd.Severity)
		}
	}
}

// TestFOFDSetlkRule_Negative_LockLinux: the symbol is allowed inside
// internal/safefs/lock_linux.go (the build-tag file).
func TestFOFDSetlkRule_Negative_LockLinux(t *testing.T) {
	src := []byte(`package safefs

import "golang.org/x/sys/unix"

func acquire(fd uintptr) {
	_ = unix.F_OFD_SETLK
}
`)
	rule := FOFDSetlkRule()
	fset, f := mustParseGo(t, src)
	out := rule.Check("internal/safefs/lock_linux.go", src, fset, f)
	if len(out) != 0 {
		t.Errorf("want 0 findings on lock_linux.go, got %d: %v", len(out), out)
	}
}

// TestFOFDSetlkRule_Negative_InComment: the symbol mentioned only
// inside a Go comment does not fire (goSpans strips comments before
// matching).
func TestFOFDSetlkRule_Negative_InComment(t *testing.T) {
	src := []byte(`package foo

// On Linux we use unix.F_OFD_SETLK in internal/safefs/lock_linux.go.
func bar() {}
`)
	rule := FOFDSetlkRule()
	fset, f := mustParseGo(t, src)
	out := rule.Check("internal/state/foo.go", src, fset, f)
	if len(out) != 0 {
		t.Errorf("want 0 findings on comment-only mention, got %d: %v", len(out), out)
	}
}

// mustParseGo is the shared test helper for Go-rule tests: parses src
// as Go source and returns (fset, *ast.File). The fset is needed by
// rules that emit positions through resolvePos.
func mustParseGo(t *testing.T, src []byte) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "test.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse Go: %v", err)
	}
	return fset, f
}
