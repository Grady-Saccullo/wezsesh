package logger_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Grady-Saccullo/wezsesh/internal/logger"
)

// envHelperKey selects which sub-process helper body to run when this test
// binary is re-invoked. The parent test sets this; the helper Test* funcs
// branch on the value.
const envHelperKey = "WEZSESH_LOGGER_TEST_HELPER"

// envHelperDir tells the helper where to put its log file. The parent
// passes a t.TempDir() so the parent can read the file post-crash.
const envHelperDir = "WEZSESH_LOGGER_TEST_DIR"

// helperWarnAndExit is the load-bearing acceptance gate for §17.3 row
// "Logger Warn/Error sync flush". When set, the helper opens the logger,
// writes a Warn record, then os.Exit(1)s WITHOUT calling Close — proving
// that the sync flush already happened inside Warn itself.
const helperWarnAndExit = "warn-and-exit"

// helperInfoAndExit is the symmetric proof: Info is buffered and the 1 s
// tick has not had time to fire, so the parent will see an empty file.
const helperInfoAndExit = "info-and-exit"

// TestMain forks the test binary into a helper role when envHelperKey is
// set; otherwise it runs the test suite normally.
func TestMain(m *testing.M) {
	switch os.Getenv(envHelperKey) {
	case helperWarnAndExit:
		runHelperWarnAndExit()
	case helperInfoAndExit:
		runHelperInfoAndExit()
	default:
		os.Exit(m.Run())
	}
}

func runHelperWarnAndExit() {
	dir := os.Getenv(envHelperDir)
	lg, err := logger.New(dir, logger.LevelDebug)
	if err != nil {
		// Use exit code 2 so the parent can distinguish helper-setup
		// failure from the intended exit-1 crash. We deliberately do not
		// call fmt.Println here — the parent reads the log file, not the
		// helper's stdout.
		os.Exit(2)
	}
	lg.Warn("warn-from-helper", "k", "v")
	// CRITICAL: no Close, no defer. The whole point of this test is that
	// Warn synchronously flushed to disk before this os.Exit fires.
	os.Exit(1)
}

func runHelperInfoAndExit() {
	dir := os.Getenv(envHelperDir)
	lg, err := logger.New(dir, logger.LevelDebug)
	if err != nil {
		os.Exit(2)
	}
	lg.Info("info-from-helper", "k", "v")
	// Same: no Close, no defer. Info is buffered through the 1 s tick;
	// since we exit immediately, the kernel may still hold the dirty page.
	// The parent will assert ABSENCE of the line.
	os.Exit(1)
}

// runHelper re-invokes this test binary in helper mode and waits for it
// to exit. Returns the exit code (1 on the intended path, 2 on
// helper-setup failure).
func runHelper(t *testing.T, role, dir string) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(),
		envHelperKey+"="+role,
		envHelperDir+"="+dir,
	)
	err := cmd.Run()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	t.Fatalf("helper failed to launch: %v", err)
	return -1
}

// TestLoggerWarnSyncFlush is the §17.3 acceptance gate. The child writes
// a Warn record then os.Exit(1)s WITHOUT calling Close; the parent reads
// the log file and asserts the Warn line landed on disk.
func TestLoggerWarnSyncFlush(t *testing.T) {
	dir := t.TempDir()
	code := runHelper(t, helperWarnAndExit, dir)
	if code != 1 {
		t.Fatalf("helper exit code: got %d want 1", code)
	}
	data, err := os.ReadFile(filepath.Join(dir, "wezsesh.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(data), "warn-from-helper") {
		t.Fatalf("log did not contain Warn line; contents: %q", string(data))
	}
}

// TestLoggerInfoNotSyncFlushed proves the buffering tradeoff §8.18 calls
// out: an Info record written and then immediately os.Exit'd does NOT
// reach disk because the 1 s tick has not fired. This is the symmetric
// "1 s tick flush for Info; immediate flush on Warn/Error" gate.
func TestLoggerInfoNotSyncFlushed(t *testing.T) {
	dir := t.TempDir()
	code := runHelper(t, helperInfoAndExit, dir)
	if code != 1 {
		t.Fatalf("helper exit code: got %d want 1", code)
	}
	data, err := os.ReadFile(filepath.Join(dir, "wezsesh.log"))
	if err != nil {
		// File missing is also acceptable: append-mode O_CREATE creates
		// it on first Write, but if the OS hasn't flushed metadata...
		// In practice the file does exist (open created it) but may be
		// empty.
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		t.Fatalf("read log: %v", err)
	}
	if strings.Contains(string(data), "info-from-helper") {
		t.Fatalf("Info line landed on disk before tick fired; defeats §8.18 buffering tradeoff. contents: %q", string(data))
	}
}

// TestLoggerWarnIsFlushedInProcess opens the log file from a SECOND handle
// after Warn (no Close) and asserts the line shows up within 50 ms. This
// is the in-process leg of "1 s tick flush for Info; immediate flush on
// Warn/Error" — pairs with TestLoggerInfoNotSyncFlushed (out-of-process).
func TestLoggerWarnIsFlushedInProcess(t *testing.T) {
	dir := t.TempDir()
	lg, err := logger.New(dir, logger.LevelDebug)
	if err != nil {
		t.Fatalf("logger.New: %v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	lg.Warn("inproc-warn", "k", "v")

	deadline := time.Now().Add(50 * time.Millisecond)
	for {
		data, err := os.ReadFile(filepath.Join(dir, "wezsesh.log"))
		if err == nil && strings.Contains(string(data), "inproc-warn") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Warn line did not reach disk within 50ms; last read err=%v contents=%q", err, string(data))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestResolveLevel covers the §11.4 / §8.18 precedence table. The "env
// can only make logging noisier" rule is the load-bearing one.
func TestResolveLevel(t *testing.T) {
	cases := []struct {
		name string
		opts string
		env  string
		want logger.Level
	}{
		{"env empty + opts debug -> debug", "debug", "", logger.LevelDebug},
		{"env info + opts warn -> info (env wins, more verbose)", "warn", "info", logger.LevelInfo},
		{"env warn + opts info -> info (opts wins, more verbose)", "info", "warn", logger.LevelInfo},
		{"both empty -> info default", "", "", logger.LevelInfo},
		{"env debug + opts empty -> debug", "", "debug", logger.LevelDebug},
		{"env error + opts error -> error (equal)", "error", "error", logger.LevelError},
		{"unknown env, valid opts -> opts", "warn", "verbose", logger.LevelWarn},
		{"valid env, unknown opts -> env", "lol", "debug", logger.LevelDebug},
		{"both unknown -> info default", "lol", "verbose", logger.LevelInfo},
		{"case-insensitive opts", "DEBUG", "", logger.LevelDebug},
		{"case-insensitive env", "", "Warn", logger.LevelWarn},
		{"warning alias", "warning", "", logger.LevelWarn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := logger.ResolveLevel(tc.opts, tc.env)
			if got != tc.want {
				t.Fatalf("ResolveLevel(opts=%q, env=%q) = %d, want %d", tc.opts, tc.env, got, tc.want)
			}
		})
	}
}

// TestNewRefusesSymlinkAtFile verifies the inline symlink defense (the
// stand-in for safefs.Enforce(SymlinkRefuse) until T-101 lands).
func TestNewRefusesSymlinkAtFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "decoy")
	if err := os.WriteFile(target, []byte{}, 0o600); err != nil {
		t.Fatalf("seed decoy: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "wezsesh.log")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := logger.New(dir, logger.LevelInfo); err == nil {
		t.Fatal("logger.New should refuse symlink at log path, got nil error")
	}
}

// TestCloseIsIdempotent guards against double-close panics and verifies
// the sync.Once gate.
func TestCloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	lg, err := logger.New(dir, logger.LevelInfo)
	if err != nil {
		t.Fatalf("logger.New: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestTickGoroutineExitsOnClose asserts no goroutine leak after Close.
// We compare runtime.NumGoroutine() before/after across a few open+close
// cycles; the count should not strictly grow.
func TestTickGoroutineExitsOnClose(t *testing.T) {
	dir := t.TempDir()
	// Settle goroutine count.
	runtime.GC()
	time.Sleep(10 * time.Millisecond)
	before := runtime.NumGoroutine()
	for i := 0; i < 5; i++ {
		lg, err := logger.New(dir, logger.LevelInfo)
		if err != nil {
			t.Fatalf("logger.New: %v", err)
		}
		lg.Info("hello")
		if err := lg.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	// Allow scheduler to retire the tick goroutines.
	time.Sleep(20 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()
	if after > before+1 {
		t.Fatalf("goroutine leak suspected: before=%d after=%d", before, after)
	}
}

// TestRotation forces ~1.1 MiB of writes through a single Logger and
// asserts that wezsesh.log.1 exists and the active log was reset to a
// small size. This validates the §8.18 1 MiB rotation policy without
// depending on real disk pressure.
func TestRotation(t *testing.T) {
	dir := t.TempDir()
	lg, err := logger.New(dir, logger.LevelDebug)
	if err != nil {
		t.Fatalf("logger.New: %v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })

	// Each Info call emits a JSON record of well under 200 B; a 1 KiB
	// payload string per call lets us cross 1 MiB in ~1100 calls.
	payload := strings.Repeat("x", 1024)
	for i := 0; i < 1200; i++ {
		lg.Info("rotation-stress", "i", i, "p", payload)
	}
	// Fsync-on-the-mutex: read after a small sleep to let the tick or
	// inline write paths settle.
	time.Sleep(20 * time.Millisecond)

	rotated := filepath.Join(dir, "wezsesh.log.1")
	st, err := os.Stat(rotated)
	if err != nil {
		t.Fatalf("expected rotated file %s: %v", rotated, err)
	}
	if st.Size() == 0 {
		t.Fatal("rotated file is empty")
	}
	active := filepath.Join(dir, "wezsesh.log")
	stActive, err := os.Stat(active)
	if err != nil {
		t.Fatalf("active log gone: %v", err)
	}
	if stActive.Size() > st.Size() {
		t.Fatalf("active log %d bytes > rotated %d; rotation did not reset counter", stActive.Size(), st.Size())
	}
}
