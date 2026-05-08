package main

import (
	"strings"
	"testing"
)

// TestVersion_DefaultIsDev locks the in-tree default. Release builds
// override `version` via `-ldflags="-X main.version=..."`; if a future
// refactor accidentally renames the package var, the linker -X flag
// silently no-ops, so a default-value sentinel guards the contract.
func TestVersion_DefaultIsDev(t *testing.T) {
	if version != "dev" {
		t.Fatalf("default version = %q, want %q", version, "dev")
	}
}

// TestRun_VersionFlag_PrintsLDFlagsValue covers the T-801 acceptance
// gate: `--version` prints `main.version` and exits 0. We mutate the
// package var to mimic what `go build -ldflags="-X main.version=..."`
// produces at link time, then assert the binary's `--version` output
// carries that exact value.
func TestRun_VersionFlag_PrintsLDFlagsValue(t *testing.T) {
	prev := version
	t.Cleanup(func() { version = prev })
	version = "v1.2.3-test"

	var stdout, stderr strings.Builder
	rc := run([]string{"--version"}, &stdout, &stderr, testBinarySessionID)

	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
	}
	if got, want := stdout.String(), "wezsesh v1.2.3-test\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}
