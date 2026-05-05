package rules

import "testing"

// TestDeferRecoverInGoroutineRule_Positive: a `go func() {…}()` body
// in a restricted package without a top-level defer recover fires.
func TestDeferRecoverInGoroutineRule_Positive(t *testing.T) {
	src := []byte(`package state

func wire() {
	go func() {
		_ = 1
	}()
}
`)
	rule := DeferRecoverInGoroutineRule()
	fset, f := mustParseGo(t, src)
	out := rule.Check("internal/state/foo.go", src, fset, f)
	if len(out) == 0 {
		t.Fatalf("want >=1 finding for goroutine without recover, got 0")
	}
	if out[0].RuleID != "go-goroutine-recover" {
		t.Errorf("rule id: got %q", out[0].RuleID)
	}
}

// TestDeferRecoverInGoroutineRule_Negative_HasRecover: a goroutine
// whose first stmt is `defer func() { recover() }()` is silent.
func TestDeferRecoverInGoroutineRule_Negative_HasRecover(t *testing.T) {
	src := []byte(`package state

func wire() {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				_ = r
			}
		}()
		_ = 1
	}()
}
`)
	rule := DeferRecoverInGoroutineRule()
	fset, f := mustParseGo(t, src)
	out := rule.Check("internal/state/foo.go", src, fset, f)
	if len(out) != 0 {
		t.Errorf("want 0 findings when recover is present, got %d: %v", len(out), out)
	}
}

// TestDeferRecoverInGoroutineRule_Negative_NonRestricted: a goroutine
// outside the restricted-package set is not the rule's concern.
func TestDeferRecoverInGoroutineRule_Negative_NonRestricted(t *testing.T) {
	src := []byte(`package other

func wire() {
	go func() {
		_ = 1
	}()
}
`)
	rule := DeferRecoverInGoroutineRule()
	fset, f := mustParseGo(t, src)
	out := rule.Check("internal/other/foo.go", src, fset, f)
	if len(out) != 0 {
		t.Errorf("want 0 findings outside restricted set, got %d: %v", len(out), out)
	}
}

// TestDeferRecoverInGoroutineRule_Negative_TestFile: tests are exempt.
func TestDeferRecoverInGoroutineRule_Negative_TestFile(t *testing.T) {
	src := []byte(`package state

import "testing"

func TestX(t *testing.T) {
	go func() {
		_ = 1
	}()
}
`)
	rule := DeferRecoverInGoroutineRule()
	fset, f := mustParseGo(t, src)
	out := rule.Check("internal/state/state_test.go", src, fset, f)
	if len(out) != 0 {
		t.Errorf("want 0 findings on _test.go, got %d: %v", len(out), out)
	}
}
