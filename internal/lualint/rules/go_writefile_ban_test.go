package rules

import "testing"

// TestWriteFileBanRule_Positive_RestrictedPkg: an os.WriteFile in
// internal/state/foo.go fires the rule.
func TestWriteFileBanRule_Positive_RestrictedPkg(t *testing.T) {
	src := []byte(`package state

import "os"

func save(p string, b []byte) error {
	return os.WriteFile(p, b, 0o600)
}
`)
	rule := WriteFileBanRule()
	fset, f := mustParseGo(t, src)
	out := rule.Check("internal/state/foo.go", src, fset, f)
	if len(out) == 0 {
		t.Fatalf("want >=1 finding for os.WriteFile in restricted pkg, got 0")
	}
	if out[0].RuleID != "go-restricted-os-write" {
		t.Errorf("rule id: got %q", out[0].RuleID)
	}
}

// TestWriteFileBanRule_Negative_Safefs: the same call inside
// internal/safefs/atomic.go is exempt — safefs IS the wrapping API.
func TestWriteFileBanRule_Negative_Safefs(t *testing.T) {
	src := []byte(`package safefs

import "os"

func atomicWrite(p string, b []byte) error {
	return os.WriteFile(p, b, 0o600)
}
`)
	rule := WriteFileBanRule()
	fset, f := mustParseGo(t, src)
	out := rule.Check("internal/safefs/atomic.go", src, fset, f)
	if len(out) != 0 {
		t.Errorf("want 0 findings inside safefs, got %d: %v", len(out), out)
	}
}

// TestWriteFileBanRule_Negative_TestFile: any *_test.go is exempt.
func TestWriteFileBanRule_Negative_TestFile(t *testing.T) {
	src := []byte(`package state

import "os"
import "testing"

func TestSave(t *testing.T) {
	_ = os.WriteFile("/tmp/x", nil, 0o600)
}
`)
	rule := WriteFileBanRule()
	fset, f := mustParseGo(t, src)
	out := rule.Check("internal/state/state_test.go", src, fset, f)
	if len(out) != 0 {
		t.Errorf("want 0 findings on _test.go, got %d: %v", len(out), out)
	}
}

// TestWriteFileBanRule_Positive_OpenFile: os.OpenFile is also banned.
func TestWriteFileBanRule_Positive_OpenFile(t *testing.T) {
	src := []byte(`package trust

import "os"

func bind(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600)
}
`)
	rule := WriteFileBanRule()
	fset, f := mustParseGo(t, src)
	out := rule.Check("internal/trust/trust.go", src, fset, f)
	if len(out) == 0 {
		t.Fatalf("want >=1 finding for os.OpenFile in restricted pkg, got 0")
	}
}

// TestWriteFileBanRule_Negative_NonRestrictedPkg: a file outside the
// restricted-package set is silent (the rule is package-scoped).
func TestWriteFileBanRule_Negative_NonRestrictedPkg(t *testing.T) {
	src := []byte(`package something

import "os"

func write(p string) {
	_ = os.WriteFile(p, nil, 0o600)
}
`)
	rule := WriteFileBanRule()
	fset, f := mustParseGo(t, src)
	out := rule.Check("internal/something/foo.go", src, fset, f)
	if len(out) != 0 {
		t.Errorf("want 0 findings outside restricted set, got %d: %v", len(out), out)
	}
}
