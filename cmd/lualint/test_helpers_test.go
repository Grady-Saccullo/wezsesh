package main

import (
	"os/exec"
	"strings"
	"testing"
)

// goCommand returns an *exec.Cmd that runs `go <args...>`. The cwd is
// the test's package dir (the directory of cmd/lualint/main.go) so
// `go build .` resolves the package correctly. Using exec.Command with
// the literal "go" first arg is fine — this is the lualint test
// surface itself, NOT a restricted package, so the §16.5
// wezterm-CLI-boundary rule does not apply.
func goCommand(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("go", args...)
	return cmd
}

// runBinary executes bin with args and returns (combined stdout/err,
// exit code). A non-zero exit becomes the returned exit; signal-killed
// runs surface as exit 2 (we don't need richer error handling for the
// synthetic-tree tests in this file).
func runBinary(t *testing.T, bin string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return string(out), exitErr.ExitCode()
		}
		return string(out), 2
	}
	return string(out), 0
}

// contains is a tiny readable wrapper around strings.Contains for
// tests that look for a substring of the binary's stdout.
func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
