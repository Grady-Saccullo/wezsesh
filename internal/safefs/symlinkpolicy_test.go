package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestEnforceSkipWarnVsRefuse exercises the §17.3 row "safefs.Enforce
// SkipWarn vs Refuse" gate. Top-level dir as symlink → Refuse error;
// file inside as symlink → SkipWarn returns ok=false, no err.
func TestEnforceSkipWarnVsRefuse(t *testing.T) {
	tmp := t.TempDir()
	realDir := filepath.Join(tmp, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	symDir := filepath.Join(tmp, "sym")
	if err := os.Symlink(realDir, symDir); err != nil {
		t.Fatal(err)
	}

	// Top-level dir as symlink → Refuse must error with ErrIsSymlink.
	ok, err := Enforce(symDir, SymlinkRefuse, nil)
	if ok {
		t.Errorf("Enforce(SymlinkRefuse) on symlink dir: expected ok=false, got true")
	}
	if !errors.Is(err, ErrIsSymlink) {
		t.Errorf("Enforce(SymlinkRefuse): expected ErrIsSymlink, got %v", err)
	}

	// File-inside-dir as symlink → SkipWarn must return ok=false, err=nil.
	realFile := filepath.Join(realDir, "real.txt")
	if err := os.WriteFile(realFile, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	symFile := filepath.Join(realDir, "sym.txt")
	if err := os.Symlink(realFile, symFile); err != nil {
		t.Fatal(err)
	}
	ok, err = Enforce(symFile, SymlinkSkipWarn, nil)
	if ok {
		t.Errorf("Enforce(SymlinkSkipWarn) on symlink file: expected ok=false, got true")
	}
	if err != nil {
		t.Errorf("Enforce(SymlinkSkipWarn) on symlink file: expected nil err, got %v", err)
	}

	// Real (non-symlink) file → ok=true, err=nil regardless of policy.
	for _, p := range []SymlinkPolicy{SymlinkRefuse, SymlinkSkipWarn, SymlinkRejectOp} {
		ok, err := Enforce(realFile, p, nil)
		if !ok || err != nil {
			t.Errorf("Enforce(%v) on real file: ok=%v err=%v want true, nil", p, ok, err)
		}
	}

	// SymlinkRejectOp on a symlink → ok=false, err=ErrIsSymlink (no log).
	ok, err = Enforce(symFile, SymlinkRejectOp, nil)
	if ok {
		t.Errorf("Enforce(SymlinkRejectOp) on symlink file: expected ok=false, got true")
	}
	if !errors.Is(err, ErrIsSymlink) {
		t.Errorf("Enforce(SymlinkRejectOp): expected ErrIsSymlink, got %v", err)
	}
}

// TestEnforceMissingPath confirms the missing-path branch returns
// ok=true with no error so callers can treat absence as ENOENT on the
// next operation.
func TestEnforceMissingPath(t *testing.T) {
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "nope")
	ok, err := Enforce(missing, SymlinkRefuse, nil)
	if !ok || err != nil {
		t.Errorf("Enforce on missing path: ok=%v err=%v want true,nil", ok, err)
	}
}

// TestSafeRemoveRejectsSymlink ensures SafeRemove never unlinks a path
// that is itself a symlink (which would unlink the symlink, not the
// target — but the contract is "refuse the op").
func TestSafeRemoveRejectsSymlink(t *testing.T) {
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real.txt")
	if err := os.WriteFile(real, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sym := filepath.Join(tmp, "sym.txt")
	if err := os.Symlink(real, sym); err != nil {
		t.Fatal(err)
	}
	if err := SafeRemove(sym); !errors.Is(err, ErrIsSymlink) {
		t.Errorf("SafeRemove on symlink: want ErrIsSymlink got %v", err)
	}
	// Symlink and target both still present.
	if _, err := os.Lstat(sym); err != nil {
		t.Errorf("symlink unexpectedly removed: %v", err)
	}
	if _, err := os.Stat(real); err != nil {
		t.Errorf("target unexpectedly removed: %v", err)
	}
	// Real file: SafeRemove succeeds.
	if err := SafeRemove(real); err != nil {
		t.Errorf("SafeRemove on real file: %v", err)
	}
	if _, err := os.Stat(real); !os.IsNotExist(err) {
		t.Errorf("real file still present after SafeRemove: %v", err)
	}
}

// TestSafeRemoveTreeTopSymlinkRefuse — top-level path is a symlink →
// SafeRemoveTree refuses. Distinguishes from os.RemoveAll, which would
// silently follow.
func TestSafeRemoveTreeTopSymlinkRefuse(t *testing.T) {
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(real, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("dont touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	sym := filepath.Join(tmp, "sym")
	if err := os.Symlink(real, sym); err != nil {
		t.Fatal(err)
	}
	if err := SafeRemoveTree(sym, nil); !errors.Is(err, ErrIsSymlink) {
		t.Errorf("SafeRemoveTree on symlink: want ErrIsSymlink got %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("sentinel removed despite symlink defense: %v", err)
	}
}

// TestSafeRemoveTreeInnerSymlinkSkipped — internal file symlinks survive
// the recursive sweep (SkipWarn). The dir holding them is left intact.
func TestSafeRemoveTreeInnerSymlinkSkipped(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "tree")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(regular, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(tmp, "elsewhere.txt")
	if err := os.WriteFile(target, []byte("E"), 0o600); err != nil {
		t.Fatal(err)
	}
	sym := filepath.Join(dir, "sym.txt")
	if err := os.Symlink(target, sym); err != nil {
		t.Fatal(err)
	}
	if err := SafeRemoveTree(dir, nil); err != nil {
		t.Fatalf("SafeRemoveTree: %v", err)
	}
	// Regular file removed.
	if _, err := os.Lstat(regular); !os.IsNotExist(err) {
		t.Errorf("regular file should be removed: %v", err)
	}
	// Symlink retained inside (SkipWarn).
	if _, err := os.Lstat(sym); err != nil {
		t.Errorf("symlink should survive SkipWarn: %v", err)
	}
	// Target untouched.
	if _, err := os.Stat(target); err != nil {
		t.Errorf("symlink target should be untouched: %v", err)
	}
}
