package rules

import (
	"os"
	"path/filepath"
	"testing"
)

// mustWriteFile creates the parent directory (mode 0o755) and writes
// content to the given path. Used by every repo-shape rule's test
// fixture to construct a temp tree without scattering os.MkdirAll +
// os.WriteFile boilerplate across files.
func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
