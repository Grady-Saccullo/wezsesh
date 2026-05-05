package rules

import "testing"

// TestStartListenerRule_Positive: ipcsock.StartListener call from
// outside internal/ipcdispatcher fires.
func TestStartListenerRule_Positive(t *testing.T) {
	src := []byte(`package state

import "github.com/Grady-Saccullo/wezsesh/internal/ipcsock"

func wire() {
	_, _, _ = ipcsock.StartListener("/tmp/x.sock", nil)
}
`)
	rule := StartListenerRule()
	fset, f := mustParseGo(t, src)
	out := rule.Check("internal/state/foo.go", src, fset, f)
	if len(out) == 0 {
		t.Fatalf("want >=1 finding for StartListener outside ipcdispatcher, got 0")
	}
	if out[0].RuleID != "go-start-listener-boundary" {
		t.Errorf("rule id: got %q", out[0].RuleID)
	}
}

// TestStartListenerRule_Negative_Inside: same call inside
// internal/ipcdispatcher is exempt.
func TestStartListenerRule_Negative_Inside(t *testing.T) {
	src := []byte(`package ipcdispatcher

import "github.com/Grady-Saccullo/wezsesh/internal/ipcsock"

func wire() {
	_, _, _ = ipcsock.StartListener("/tmp/x.sock", nil)
}
`)
	rule := StartListenerRule()
	fset, f := mustParseGo(t, src)
	out := rule.Check("internal/ipcdispatcher/dispatcher.go", src, fset, f)
	if len(out) != 0 {
		t.Errorf("want 0 findings inside ipcdispatcher, got %d: %v", len(out), out)
	}
}
