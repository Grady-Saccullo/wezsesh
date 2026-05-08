package logger_test

import (
	"encoding/json"
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

// testBinarySessionID is the deterministic 26-char ULID stamped onto
// every record by the tests below. Matches the dispatcher-side test
// fixture so a future cross-package fixture-share is straightforward.
const testBinarySessionID = "01JABCDEFGHJKMNPQRSTVWXYZB"

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
	lg, err := logger.New(dir, logger.LevelDebug, testBinarySessionID)
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
	lg, err := logger.New(dir, logger.LevelDebug, testBinarySessionID)
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
	lg, err := logger.New(dir, logger.LevelDebug, testBinarySessionID)
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

func TestLevelString(t *testing.T) {
	cases := []struct {
		l    logger.Level
		want string
	}{
		{logger.LevelDebug, "debug"},
		{logger.LevelInfo, "info"},
		{logger.LevelWarn, "warn"},
		{logger.LevelError, "error"},
		{logger.Level(99), "info"},
	}
	for _, tc := range cases {
		if got := logger.LevelString(tc.l); got != tc.want {
			t.Errorf("LevelString(%d) = %q, want %q", tc.l, got, tc.want)
		}
	}
}

// TestNewRefusesSymlinkAtFile verifies the safefs.OpenAppendOnly
// O_NOFOLLOW defense at the log file leaf.
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
	if _, err := logger.New(dir, logger.LevelInfo, testBinarySessionID); err == nil {
		t.Fatal("logger.New should refuse symlink at log path, got nil error")
	}
}

// TestNewRefusesSymlinkAtStateDir verifies the safefs.Enforce defense
// on the parent dir slot. A symlinked stateDir at startup is the
// CVE-class primitive an attacker would plant before logger.New runs.
func TestNewRefusesSymlinkAtStateDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real-state")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	sym := filepath.Join(tmp, "state-link")
	if err := os.Symlink(real, sym); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := logger.New(sym, logger.LevelInfo, testBinarySessionID); err == nil {
		t.Fatal("logger.New should refuse symlink at state dir, got nil error")
	}
}

// TestNewLogFileMode0600 — the log file is created with mode 0600 by
// safefs.OpenAppendOnly. Verifies the §12.1 path-table guarantee for
// `<state_dir>/wezsesh.log`.
func TestNewLogFileMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode bits not meaningful on Windows")
	}
	dir := t.TempDir()
	lg, err := logger.New(dir, logger.LevelInfo, testBinarySessionID)
	if err != nil {
		t.Fatalf("logger.New: %v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	st, err := os.Stat(filepath.Join(dir, "wezsesh.log"))
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("log mode: got %v, want 0600", st.Mode().Perm())
	}
}

// TestCloseIsIdempotent guards against double-close panics and verifies
// the sync.Once gate.
func TestCloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	lg, err := logger.New(dir, logger.LevelInfo, testBinarySessionID)
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
		lg, err := logger.New(dir, logger.LevelInfo, testBinarySessionID)
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
	lg, err := logger.New(dir, logger.LevelDebug, testBinarySessionID)
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

// readLogRecords returns each newline-delimited JSON record from
// <dir>/wezsesh.log decoded into a generic map. The logger emits one
// JSON object per record; helpers below assert sticky-attr presence by
// inspecting the resulting maps.
func readLogRecords(t *testing.T, dir string) []map[string]any {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "wezsesh.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode record %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// TestBinarySessionIDOnEveryRecord asserts the binary_session_id sticky
// attribute is attached to every record at every level — Debug, Info,
// Warn, Error. The trace/correlation rollout (CLAUDE.md) relies on
// every Go-side log line carrying this without each callsite spelling
// it out.
func TestBinarySessionIDOnEveryRecord(t *testing.T) {
	dir := t.TempDir()
	lg, err := logger.New(dir, logger.LevelDebug, testBinarySessionID)
	if err != nil {
		t.Fatalf("logger.New: %v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })

	lg.Debug("dbg-line")
	lg.Info("info-line")
	lg.Warn("warn-line")
	lg.Error("err-line")

	// Warn/Error sync-flush; Info/Debug ride the bufio buffer until the
	// next tick or another sync-flush. The Warn already flushed both
	// the Info and the Debug ahead of it (bufio is FIFO), so all four
	// are on disk by the time Error returns.
	records := readLogRecords(t, dir)
	if len(records) != 4 {
		t.Fatalf("got %d records, want 4: %v", len(records), records)
	}
	wantMsgs := []string{"dbg-line", "info-line", "warn-line", "err-line"}
	for i, rec := range records {
		got, _ := rec["binary_session_id"].(string)
		if got != testBinarySessionID {
			t.Errorf("record[%d] binary_session_id = %q, want %q (record=%v)",
				i, got, testBinarySessionID, rec)
		}
		msg, _ := rec["msg"].(string)
		if msg != wantMsgs[i] {
			t.Errorf("record[%d] msg = %q, want %q", i, msg, wantMsgs[i])
		}
	}
}

// TestWithComposesChild asserts that (*Logger).With returns a child
// that emits both the parent's sticky attrs (binary_session_id) AND
// the new attrs supplied to With (here trace_id). This is the
// load-bearing seam for the dispatcher's per-request trace_id
// stamping.
func TestWithComposesChild(t *testing.T) {
	dir := t.TempDir()
	lg, err := logger.New(dir, logger.LevelDebug, testBinarySessionID)
	if err != nil {
		t.Fatalf("logger.New: %v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })

	const traceID = "01JABCDEFGHJKMNPQRSTVWXYZD"
	child := lg.With("trace_id", traceID)
	child.Warn("from-child")

	records := readLogRecords(t, dir)
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1: %v", len(records), records)
	}
	rec := records[0]
	if got, _ := rec["binary_session_id"].(string); got != testBinarySessionID {
		t.Errorf("child record binary_session_id = %q, want %q", got, testBinarySessionID)
	}
	if got, _ := rec["trace_id"].(string); got != traceID {
		t.Errorf("child record trace_id = %q, want %q", got, traceID)
	}

	// And the parent must NOT inherit the child's sticky attrs (the
	// parent's slogger is unchanged).
	lg.Warn("from-parent")
	records = readLogRecords(t, dir)
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2 after parent emit: %v", len(records), records)
	}
	if _, has := records[1]["trace_id"]; has {
		t.Errorf("parent record carries trace_id %v (should be child-only)", records[1]["trace_id"])
	}
	if got, _ := records[1]["binary_session_id"].(string); got != testBinarySessionID {
		t.Errorf("parent record binary_session_id = %q, want %q", got, testBinarySessionID)
	}
}

// TestCloseChildSharesParent — the closeOnce is shared, so a Close on
// the parent followed by a Close on the child is a safe no-op (and
// vice versa). After the writer is torn down, follow-on log calls on
// either parent or child must not panic; the rotatingWriter swallows
// post-close writes and the syncFlush guard short-circuits.
func TestCloseChildSharesParent(t *testing.T) {
	dir := t.TempDir()
	lg, err := logger.New(dir, logger.LevelDebug, testBinarySessionID)
	if err != nil {
		t.Fatalf("logger.New: %v", err)
	}
	child := lg.With("trace_id", "01JABCDEFGHJKMNPQRSTVWXYZE")

	if err := lg.Close(); err != nil {
		t.Fatalf("parent Close: %v", err)
	}
	// Child Close must not double-close: it should observe the same
	// closeOnce-gated state and return the cached err (nil here).
	if err := child.Close(); err != nil {
		t.Fatalf("child Close after parent: %v", err)
	}
	// Reverse direction — closing child first, parent second — must
	// also be safe. (Different temp dir to avoid stale state.)
	dir2 := t.TempDir()
	lg2, err := logger.New(dir2, logger.LevelDebug, testBinarySessionID)
	if err != nil {
		t.Fatalf("logger.New: %v", err)
	}
	child2 := lg2.With("trace_id", "01JABCDEFGHJKMNPQRSTVWXYZF")
	if err := child2.Close(); err != nil {
		t.Fatalf("child Close: %v", err)
	}
	if err := lg2.Close(); err != nil {
		t.Fatalf("parent Close after child: %v", err)
	}

	// Subsequent log calls on either side after the writer is torn
	// down must not panic. The rotatingWriter's Write returns an
	// error post-close, and slog's JSON handler swallows write errors
	// silently — we just want to assert no panic.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("post-close log call panicked: %v", r)
		}
	}()
	lg.Info("post-close-parent")
	child.Info("post-close-child")
	lg2.Warn("post-close-parent2")
	child2.Warn("post-close-child2")
}
