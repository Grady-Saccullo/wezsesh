package safefs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestMain implements a subprocess re-exec dance: when invoked with the
// WEZSESH_TEST_LOCK_HOLDER env var set, the binary acts as a "lock
// holder" — it acquires the named file's lock, prints "READY" to
// stdout, then waits on stdin for a single byte before releasing and
// exiting. This is the only reliable way to test cross-process
// POSIX-advisory-lock semantics on darwin (F_SETLK is process-scoped;
// in-process locks "succeed" trivially).
func TestMain(m *testing.M) {
	if path := os.Getenv("WEZSESH_TEST_LOCK_HOLDER"); path != "" {
		runLockHolder(path)
		// runLockHolder calls os.Exit; unreachable.
	}
	os.Exit(m.Run())
}

// runLockHolder is the subprocess entry point. Acquires path with a 5 s
// budget, prints "READY\n" + flushes, then blocks on stdin Read until
// a byte arrives, releases, exits 0. Any error → exit 2 with stderr.
func runLockHolder(path string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, release, err := AcquireExclusive(ctx, path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "holder: acquire: %v\n", err)
		os.Exit(2)
	}
	defer release()
	fmt.Println("READY")
	os.Stdout.Sync()
	buf := make([]byte, 1)
	_, _ = os.Stdin.Read(buf)
	os.Exit(0)
}

// spawnLockHolder forks the test binary with WEZSESH_TEST_LOCK_HOLDER
// set, returns when the child has printed "READY" (i.e. holds the lock),
// and gives back a release closure that signals the child to drop and
// exit. The release closure is idempotent and safe to call multiple
// times (subsequent calls are no-ops).
func spawnLockHolder(t *testing.T, path string) func() {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", "TestMain")
	cmd.Env = append(os.Environ(), "WEZSESH_TEST_LOCK_HOLDER="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Wait for READY.
	readyCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 32)
		n, err := stdout.Read(buf)
		if err != nil {
			readyCh <- err
			return
		}
		if !strings.Contains(string(buf[:n]), "READY") {
			readyCh <- fmt.Errorf("unexpected child output: %q", buf[:n])
			return
		}
		readyCh <- nil
	}()
	select {
	case err := <-readyCh:
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("holder ready: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("holder ready timeout")
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_, _ = stdin.Write([]byte{0})
			_ = stdin.Close()
			_ = cmd.Wait()
		})
	}
}

// TestAcquireExclusiveBasic — happy path: open existing file, acquire,
// release, file unchanged.
func TestAcquireExclusiveBasic(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "snap.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lf, release, err := AcquireExclusive(ctx, path)
	if err != nil {
		t.Fatalf("AcquireExclusive: %v", err)
	}
	defer release()
	bytes, err := lf.ReadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(bytes) != "{}" {
		t.Errorf("ReadAll: got %q want \"{}\"", bytes)
	}
}

// TestAcquireExclusiveMissingFile — missing path → ErrNotExist.
func TestAcquireExclusiveMissingFile(t *testing.T) {
	tmp := t.TempDir()
	_, _, err := AcquireExclusive(context.Background(), filepath.Join(tmp, "nope"))
	if !errors.Is(err, ErrNotExist) {
		t.Errorf("AcquireExclusive(missing): want ErrNotExist, got %v", err)
	}
}

// TestAcquireExclusiveSymlink — symlinked target file → ErrIsSymlink.
func TestAcquireExclusiveSymlink(t *testing.T) {
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real.json")
	if err := os.WriteFile(real, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	sym := filepath.Join(tmp, "sym.json")
	if err := os.Symlink(real, sym); err != nil {
		t.Fatal(err)
	}
	_, _, err := AcquireExclusive(context.Background(), sym)
	if !errors.Is(err, ErrIsSymlink) {
		t.Errorf("AcquireExclusive(sym): want ErrIsSymlink, got %v", err)
	}
}

// TestAcquireExclusiveOrCreate — first-save path: file doesn't exist
// initially, gets created with the requested mode, lock acquired,
// release leaves an empty 0600 file behind.
//
// Covers §17.3 row "Save first-write (no expected_hash)" gate as it
// applies to safefs (the per-name in-process mutex itself ships with
// T-303 / T-800; here we exercise the create-or-lock primitive).
func TestAcquireExclusiveOrCreate(t *testing.T) {
	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lf, release, err := AcquireExclusiveOrCreate(ctx, tmp, "first.snap", 0o600)
	if err != nil {
		t.Fatalf("AcquireExclusiveOrCreate: %v", err)
	}
	if lf == nil {
		t.Fatal("nil LockedFile")
	}
	release()
	st, err := os.Stat(filepath.Join(tmp, "first.snap"))
	if err != nil {
		t.Fatalf("post-release stat: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("created file mode: got %v want 0600", st.Mode().Perm())
	}
	if st.Size() != 0 {
		t.Errorf("created file size: got %d want 0", st.Size())
	}
}

// TestSaveFlockSerialisation_PhaseA — the §17.3 row "Save flock
// serialisation (Phase A)" gate. A subprocess holder acquires the lock
// on the snapshot file; an in-process Acquire under a tight ctx must
// surface ErrLockTimeout (which the dispatcher translates to
// SNAPSHOT_LOCKED in T-303).
//
// We can't satisfy this with two in-process fds on darwin: F_SETLK is
// process-scoped, and a second open within the same process re-uses
// (rather than contests) the lock. The subprocess holder forces the
// cross-process advisory-lock path that POSIX actually defines.
func TestSaveFlockSerialisation_PhaseA(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "snap.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	releaseHolder := spawnLockHolder(t, path)
	defer releaseHolder()

	// In-process acquire under tight budget: must time out.
	ctxB, cancelB := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelB()
	_, _, err := AcquireExclusive(ctxB, path)
	if !errors.Is(err, ErrLockTimeout) {
		t.Errorf("contended AcquireExclusive: want ErrLockTimeout, got %v", err)
	}

	releaseHolder() // release subprocess

	// After release, the slot is free; acquire succeeds.
	ctxC, cancelC := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelC()
	_, releaseC, err := AcquireExclusive(ctxC, path)
	if err != nil {
		t.Fatalf("post-release acquire: %v", err)
	}
	releaseC()
}

// TestFSetlkPollingFairness covers §17.3 "F_SETLK polling fairness".
// A subprocess holder grabs the lock; three in-process contenders race
// to acquire. After 100 ms the subprocess releases. All three contenders
// must complete within 5 s and the lock must hand off cleanly.
//
// On Linux this exercises F_OFD_SETLK; on darwin/BSD it exercises
// F_SETLK. The poll-loop logic is shared (acquireOnFd in
// lockedfile.go). The subprocess is mandatory: F_SETLK is
// process-scoped and an in-process holder would not contend with
// in-process contenders.
func TestFSetlkPollingFairness(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "contended.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	releaseHolder := spawnLockHolder(t, path)
	defer releaseHolder()

	const contenders = 3
	type result struct {
		idx int
		err error
		dur time.Duration
	}
	out := make(chan result, contenders)

	startGate := make(chan struct{})
	for i := 0; i < contenders; i++ {
		go func(i int) {
			<-startGate
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			t0 := time.Now()
			_, release, err := AcquireExclusive(ctx, path)
			out <- result{idx: i, err: err, dur: time.Since(t0)}
			if release != nil {
				release()
			}
		}(i)
	}
	close(startGate)

	// Hold for 100 ms, then release the subprocess.
	time.Sleep(100 * time.Millisecond)
	releaseHolder()

	deadline := time.After(6 * time.Second)
	got := 0
	for got < contenders {
		select {
		case r := <-out:
			if r.err != nil {
				t.Errorf("contender %d: err=%v dur=%v", r.idx, r.err, r.dur)
			}
			if r.dur > 5*time.Second {
				t.Errorf("contender %d: took %v (want < 5s)", r.idx, r.dur)
			}
			got++
		case <-deadline:
			t.Fatalf("contender deadline missed: %d/%d acquired", got, contenders)
		}
	}
}

// TestFSetlkContentionWarn — contended waits emit a WARN-at-1s. The
// subprocess holds for ~1.2 s while a contender waits, crossing the 1 s
// threshold so the WARN fires. We don't capture slog output here (the
// behavioural assertion is "lock acquires after the WARN window"); a
// dedicated TestContentionWarnEmitted below redirects slog and asserts
// the message text directly.
func TestFSetlkContentionWarn(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "warn.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	releaseHolder := spawnLockHolder(t, path)
	defer releaseHolder()

	done := make(chan time.Duration, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		t0 := time.Now()
		_, release, err := AcquireExclusive(ctx, path)
		if err != nil {
			done <- 0
			return
		}
		release()
		done <- time.Since(t0)
	}()
	// Hold for 1.2 s so contender crosses the 1 s WARN threshold but
	// not the 3 s one (avoids artificially extending test time).
	time.Sleep(1200 * time.Millisecond)
	releaseHolder()
	select {
	case d := <-done:
		if d == 0 {
			t.Errorf("contender failed to acquire after WARN window")
		}
		if d < 1*time.Second {
			t.Errorf("contender wait %v didn't cross 1s WARN threshold", d)
		}
	case <-time.After(6 * time.Second):
		t.Fatalf("contender goroutine deadlocked")
	}
}

// TestOCloexecInheritance covers §17.3 "O_CLOEXEC inheritance" — the
// lock fd must NOT be in a fork-spawned child's fd table. We acquire,
// fork-exec a child that lists open fds via /proc/self/fd (or lsof on
// darwin), and confirm the fd is missing.
//
// Robustness: the child runs `lsof -p <pid>` on darwin and `ls
// /proc/self/fd` on linux. The assertion is "the file path we just
// locked does not appear in the child's fd table".
func TestOCloexecInheritance(t *testing.T) {
	if testing.Short() {
		t.Skip("skip child-spawn test in short mode")
	}
	tmp := t.TempDir()
	path := filepath.Join(tmp, "cloexec.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, release, err := AcquireExclusive(ctx, path)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	// Spawn a child and inspect its fd table. Use sh -c to avoid PATH
	// surprises.
	var script string
	switch goos() {
	case "linux":
		script = `for f in /proc/self/fd/*; do readlink "$f"; done`
	case "darwin":
		// lsof prints NAME column for each open file. Use awk to grab.
		script = fmt.Sprintf(`/usr/sbin/lsof -p $$ 2>/dev/null | awk 'NR>1 {print $NF}' | grep -F %q || true`, path)
	default:
		t.Skip("unsupported platform for fd inspection")
	}
	cmd := exec.Command("sh", "-c", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child exec: %v\n%s", err, out)
	}
	if strings.Contains(string(out), filepath.Base(path)) {
		// On linux, the readlink output is the full path; on darwin,
		// the lsof column is the full path. Either way, finding the
		// basename-or-full-path means CLOEXEC didn't kick in.
		t.Errorf("child fd table contains locked file path:\n%s", out)
	}
}

// goos returns runtime.GOOS. Wrapped to keep the import declaration
// adjacent to the test that uses it.
func goos() string { return runtime.GOOS }

// TestAcquireExclusiveOrCreateSequential — covers the §17.3 row "Save
// first-write (no expected_hash)" gate as it applies to safefs: the
// AcquireExclusiveOrCreate primitive is callable repeatedly (creates on
// first call, opens existing on subsequent calls), and each call honors
// release. The per-name in-process mutex that serialises CONCURRENT
// callers ships with T-303 / T-800; this test covers the primitive
// alone.
//
// We deliberately serialise within the test (rather than fanning out
// goroutines) because POSIX F_SETLK on darwin is process-scoped: two
// in-process goroutines wouldn't actually contest. The cross-process
// flock test is TestSaveFlockSerialisation_PhaseA above.
func TestAcquireExclusiveOrCreateSequential(t *testing.T) {
	tmp := t.TempDir()
	const N = 8
	for i := 0; i < N; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, release, err := AcquireExclusiveOrCreate(ctx, tmp, "shared.snap", 0o600)
		cancel()
		if err != nil {
			t.Fatalf("iter %d: acquire: %v", i, err)
		}
		release()
	}
	st, err := os.Stat(filepath.Join(tmp, "shared.snap"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("perm mismatch: %v", st.Mode().Perm())
	}
}

// TestAcquireExclusiveOrCreateCrossProcess — the cross-binary
// serialisation primitive: a subprocess holder grabs the lock; an
// in-process AcquireExclusiveOrCreate under tight ctx must surface
// ErrLockTimeout. After the subprocess releases, the in-process
// acquire succeeds without recreating the file.
func TestAcquireExclusiveOrCreateCrossProcess(t *testing.T) {
	tmp := t.TempDir()
	// Pre-create the file so spawnLockHolder's AcquireExclusive (which
	// targets an existing file) succeeds.
	target := filepath.Join(tmp, "first.snap")
	if err := os.WriteFile(target, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	releaseHolder := spawnLockHolder(t, target)
	defer releaseHolder()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, _, err := AcquireExclusiveOrCreate(ctx, tmp, "first.snap", 0o600)
	if !errors.Is(err, ErrLockTimeout) {
		t.Errorf("contended AcquireExclusiveOrCreate: want ErrLockTimeout got %v", err)
	}
	releaseHolder()

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	_, release, err := AcquireExclusiveOrCreate(ctx2, tmp, "first.snap", 0o600)
	if err != nil {
		t.Fatalf("post-release acquire: %v", err)
	}
	release()
}

// TestContentionWarnEmittedAtThresholds — captures slog output and
// asserts that emitContentionWarn fires at least once at the 1 s
// threshold during a contended hold of ~1.2 s. This is the
// behavioural-content side of the §17.3 "F_SETLK polling fairness"
// gate ("WARN fires at 1 s and 3 s") — the no-deadlock side is
// TestFSetlkContentionWarn above.
func TestContentionWarnEmittedAtThresholds(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "warn-content.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Redirect slog default to capture WARN records. The package default
	// is restored on test exit.
	captured := newCaptureHandler()
	old := slog.Default()
	slog.SetDefault(slog.New(captured))
	defer slog.SetDefault(old)

	releaseHolder := spawnLockHolder(t, path)
	defer releaseHolder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, release, err := AcquireExclusive(ctx, path)
		if err == nil {
			release()
		}
	}()
	// Hold for 1.2 s, crossing the 1 s WARN threshold.
	time.Sleep(1200 * time.Millisecond)
	releaseHolder()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("contender deadlocked")
	}

	if !captured.contains("lock contended") {
		t.Errorf("expected WARN 'lock contended', got %v", captured.snapshot())
	}
}

// captureHandler is a minimal slog.Handler that buffers WARN+ records
// for inspection.
type captureHandler struct {
	mu    sync.Mutex
	warns []string
}

func newCaptureHandler() *captureHandler { return &captureHandler{} }

func (h *captureHandler) Enabled(_ context.Context, lvl slog.Level) bool {
	return lvl >= slog.LevelWarn
}
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.warns = append(h.warns, r.Message)
	return nil
}
func (h *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *captureHandler) contains(needle string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.warns {
		if strings.Contains(m, needle) {
			return true
		}
	}
	return false
}

func (h *captureHandler) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.warns))
	copy(out, h.warns)
	return out
}


// TestLockedFileWriteAllReadAll — round-trip via the locked-fd path used
// by Phase A / Phase C of the save flow.
func TestLockedFileWriteAllReadAll(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "rw.json")
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	lf, release, err := AcquireExclusive(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	want := []byte(`{"hello":"world"}`)
	if err := lf.WriteAll(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := lf.ReadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("round-trip: got %q want %q", got, want)
	}
}

// silenceUnused keeps unix imported in test scope (used by helpers).
var _ = unix.SEEK_SET
