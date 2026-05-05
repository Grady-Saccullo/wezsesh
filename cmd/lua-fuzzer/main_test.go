package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// repoRoot resolves the working repo root for this test (the dir
// containing plugin/wezsesh/fuzz/fuzz_spec.lua), starting from this
// test file's directory and walking up.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir,
			"plugin", "wezsesh", "fuzz", "fuzz_spec.lua")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("repoRoot: could not locate plugin/wezsesh/fuzz/fuzz_spec.lua")
	return ""
}

// TestRunBadRoot — `--root=<missing>` should exit 2 (spec not found).
func TestRunBadRoot(t *testing.T) {
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	defer devnull.Close()

	got := run([]string{"--root=/nonexistent/wezsesh-fuzzer-test"},
		devnull, devnull)
	if got != 2 {
		t.Fatalf("exit code: got %d, want 2", got)
	}
}

// TestRunBadSeeds — a missing `--seeds=<path>` should exit 2.
func TestRunBadSeeds(t *testing.T) {
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	defer devnull.Close()

	root := repoRoot(t)
	got := run([]string{
		"--root=" + root,
		"--seeds=/nonexistent/wezsesh-fuzzer-seeds.txt",
	}, devnull, devnull)
	if got != 2 {
		t.Fatalf("exit code: got %d, want 2", got)
	}
}

// TestRunSmoke — a successful `--iters=1` run should exit 0. Skipped
// if `lua` is not on PATH, so the test isn't gated on local install.
func TestRunSmoke(t *testing.T) {
	if _, err := exec.LookPath("lua"); err != nil {
		t.Skip("lua not on PATH; skipping smoke run")
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	defer devnull.Close()

	root := repoRoot(t)
	// Use a temp seeds file with a single seed to keep the smoke run
	// fast and deterministic regardless of how many entries the
	// committed seeds.txt grows to.
	tmp := filepath.Join(t.TempDir(), "smoke-seeds.txt")
	if err := os.WriteFile(tmp, []byte("1\n"), 0o644); err != nil {
		t.Fatalf("write smoke seeds: %v", err)
	}
	got := run([]string{
		"--root=" + root,
		"--iters=1",
		"--seeds=" + tmp,
	}, devnull, devnull)
	if got != 0 {
		t.Fatalf("smoke run exit code: got %d, want 0", got)
	}
}
