package rules

import "testing"

// TestTeaAfterRule_Positive: tea.After in any non-test file fires.
func TestTeaAfterRule_Positive(t *testing.T) {
	src := []byte(`package tui

import tea "charm.land/bubbletea/v2"

func tick() tea.Cmd {
	return tea.After(2)
}
`)
	rule := TeaAfterRule()
	fset, f := mustParseGo(t, src)
	out := rule.Check("internal/tui/foo.go", src, fset, f)
	if len(out) == 0 {
		t.Fatalf("want >=1 finding for tea.After, got 0")
	}
}

// TestTeaAfterRule_Negative: tea.Tick is the legitimate API and does
// not fire.
func TestTeaAfterRule_Negative(t *testing.T) {
	src := []byte(`package tui

import tea "charm.land/bubbletea/v2"

func tick() tea.Cmd {
	return tea.Tick(2)
}
`)
	rule := TeaAfterRule()
	fset, f := mustParseGo(t, src)
	out := rule.Check("internal/tui/foo.go", src, fset, f)
	if len(out) != 0 {
		t.Errorf("want 0 findings for tea.Tick, got %d: %v", len(out), out)
	}
}
