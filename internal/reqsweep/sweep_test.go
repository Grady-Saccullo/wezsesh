package reqsweep

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Grady-Saccullo/wezsesh/internal/doctor"
	"github.com/Grady-Saccullo/wezsesh/internal/safefs"
)

// TestSweepStale_RemovesStaleReqFiles verifies the §12.4 startup sweep:
// a *.json request file with mtime older than the doctor threshold is
// removed; a fresh one is left in place; non-.json entries are ignored.
func TestSweepStale_RemovesStaleReqFiles(t *testing.T) {
	dir := t.TempDir()

	stale := filepath.Join(dir, "deadbeef.json")
	fresh := filepath.Join(dir, "feedface.json")
	other := filepath.Join(dir, "other.txt")

	for _, p := range []string{stale, fresh, other} {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}
	// Backdate stale to (threshold + 30 s); leave fresh + other at "now".
	old := time.Now().Add(-(doctor.ReqOrphanThreshold + 30*time.Second))
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("chtimes stale: %v", err)
	}

	if err := SweepStale(dir, nil); err != nil {
		t.Fatalf("SweepStale: %v", err)
	}

	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale req file not removed: stat err=%v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh req file unexpectedly removed: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("non-.json entry unexpectedly removed: %v", err)
	}
}

// TestSweepStale_LeavesFreshUnderThreshold confirms a *.json entry
// younger than the threshold is preserved. Boundary case: freshly-
// written (mtime ≈ now) must survive even though the loop runs
// immediately after seeding.
func TestSweepStale_LeavesFreshUnderThreshold(t *testing.T) {
	dir := t.TempDir()

	fresh := filepath.Join(dir, "fresh1234.json")
	if err := os.WriteFile(fresh, []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed fresh: %v", err)
	}

	if err := SweepStale(dir, nil); err != nil {
		t.Fatalf("SweepStale: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh req file removed unexpectedly: %v", err)
	}
}

// TestSweepStale_LeavesNonJSON confirms entries without the .json
// suffix are ignored even when older than the threshold (the sweep
// must not remove unrelated files that happen to share the dir).
func TestSweepStale_LeavesNonJSON(t *testing.T) {
	dir := t.TempDir()

	other := filepath.Join(dir, "ancient.txt")
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed other: %v", err)
	}
	old := time.Now().Add(-(doctor.ReqOrphanThreshold + time.Hour))
	if err := os.Chtimes(other, old, old); err != nil {
		t.Fatalf("chtimes other: %v", err)
	}

	if err := SweepStale(dir, nil); err != nil {
		t.Fatalf("SweepStale: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("non-.json entry removed: %v", err)
	}
}

// TestSweepStale_MissingDir is a no-op (returns nil). The dispatcher
// creates <runtime_dir>/req/ on first use; the sweep must not complain
// when it runs first on a clean install.
func TestSweepStale_MissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist", "req")
	if err := SweepStale(dir, nil); err != nil {
		t.Fatalf("SweepStale on missing dir: %v", err)
	}
}

// TestSweepStale_EmptyDirArg rejects empty input — programmer error.
func TestSweepStale_EmptyDirArg(t *testing.T) {
	if err := SweepStale("", nil); err == nil {
		t.Fatal("expected error on empty dir")
	}
}

// TestSweepStale_TopLevelSymlinkRefuses confirms that a symlinked
// req-dir surfaces as ErrIsSymlink (fail-CLOSED on the dir, per safefs
// convention for top-level managed dirs).
func TestSweepStale_TopLevelSymlinkRefuses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks unreliable on windows; package is unix-only")
	}
	root := t.TempDir()
	target := filepath.Join(root, "real-req")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	link := filepath.Join(root, "req")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	err := SweepStale(link, nil)
	if !errors.Is(err, safefs.ErrIsSymlink) {
		t.Fatalf("SweepStale(sym): want ErrIsSymlink, got %v", err)
	}
}

// TestSweepStale_PerEntrySymlinkSkipped confirms that a *.json symlink
// inside the req dir is skipped (per-entry SymlinkSkipWarn) WITHOUT
// aborting the sweep — neighbouring stale files must still be removed.
func TestSweepStale_PerEntrySymlinkSkipped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks unreliable on windows; package is unix-only")
	}
	dir := t.TempDir()

	// One real stale file that should be removed.
	stale := filepath.Join(dir, "deadbeef.json")
	if err := os.WriteFile(stale, []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed stale: %v", err)
	}
	old := time.Now().Add(-(doctor.ReqOrphanThreshold + 30*time.Second))
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("chtimes stale: %v", err)
	}

	// One symlinked .json that should be skipped (left in place).
	target := filepath.Join(dir, ".target")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	sym := filepath.Join(dir, "symlink.json")
	if err := os.Symlink(target, sym); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := SweepStale(dir, nil); err != nil {
		t.Fatalf("SweepStale: %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale not removed despite per-entry symlink: %v", err)
	}
	// The symlink itself must remain (we did not unlink it).
	if _, err := os.Lstat(sym); err != nil {
		t.Fatalf("per-entry symlink unexpectedly removed: %v", err)
	}
}

// TestSweepStale_ThresholdMatchesDoctor is the lockstep gate. If the
// sweep ever drifts from doctor's idea of "orphaned" the user sees a
// doctor row that disagrees with what the sweep just did.
func TestSweepStale_ThresholdMatchesDoctor(t *testing.T) {
	// Seed two files: one just-past, one just-before threshold.
	dir := t.TempDir()
	pastBy := 5 * time.Second
	stale := filepath.Join(dir, "past.json")
	fresh := filepath.Join(dir, "before.json")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}
	staleT := time.Now().Add(-(doctor.ReqOrphanThreshold + pastBy))
	freshT := time.Now().Add(-(doctor.ReqOrphanThreshold - pastBy))
	if err := os.Chtimes(stale, staleT, staleT); err != nil {
		t.Fatalf("chtimes stale: %v", err)
	}
	if err := os.Chtimes(fresh, freshT, freshT); err != nil {
		t.Fatalf("chtimes fresh: %v", err)
	}
	if err := SweepStale(dir, nil); err != nil {
		t.Fatalf("SweepStale: %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file just past threshold not swept: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("file just before threshold removed: %v", err)
	}
}
