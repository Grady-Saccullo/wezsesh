package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSelectGoRules_Filter verifies the --rule filter narrows to a
// specific subset by ID.
func TestSelectGoRules_Filter(t *testing.T) {
	set := buildEnabledSet("go-tea-after,go-fofd-setlk-build-tag")
	if !enabledFor(set, "go-tea-after") {
		t.Errorf("go-tea-after should be enabled")
	}
	if enabledFor(set, "go-restricted-os-write") {
		t.Errorf("go-restricted-os-write should NOT be enabled")
	}
}

// TestSelectGoRules_Empty verifies an empty filter enables every rule.
func TestSelectGoRules_Empty(t *testing.T) {
	set := buildEnabledSet("")
	if !enabledFor(set, "go-tea-after") {
		t.Errorf("empty filter should enable every rule")
	}
}

// TestWalkSkipDirs verifies the walker skips dot-prefixed dirs and the
// listed top-level skip names.
func TestWalkSkipDirs(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "vendor", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Place a file that would otherwise fire the tea.After rule.
	if err := os.WriteFile(filepath.Join(dir, "vendor", "deep", "x.go"),
		[]byte("package x\nfunc f() { _ = tea.After(1) }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Place a clean root-level main.go.
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	bin := buildLualintBinary(t)
	out, exit := runBinary(t, bin, dir)
	if exit != 0 {
		t.Errorf("expected exit 0 walking past vendor, got %d. output:\n%s", exit, out)
	}
}

// TestEndToEnd_LintFiresOnSyntheticBadFile verifies the binary surfaces
// a finding from the simplest rule (tea.After) end-to-end through the
// walker → rule → driver pipeline.
func TestEndToEnd_LintFiresOnSyntheticBadFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "internal", "tui"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "tui", "ticker.go"),
		[]byte(`package tui

func tick() interface{} {
	return tea.After(2)
}
`), 0o600); err != nil {
		t.Fatal(err)
	}

	bin := buildLualintBinary(t)
	out, exit := runBinary(t, bin, "--rule=go-tea-after", dir)
	if exit != 1 {
		t.Errorf("expected exit 1 (finding), got %d. output:\n%s", exit, out)
	}
	if !contains(out, "go-tea-after") {
		t.Errorf("expected output to mention rule id, got:\n%s", out)
	}
}

// buildLualintBinary builds the binary into the test's temp dir and
// returns the path. We use go test's exec helper indirectly by
// shelling out to the just-built binary with t.TempDir() as a synthetic
// repo root, so the test does not depend on a real wezsesh checkout.
func buildLualintBinary(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "lualint")
	cmd := goCommand(t, "build", "-o", bin, ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}
