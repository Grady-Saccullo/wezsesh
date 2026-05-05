package ipcsock

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestMain wraps the suite with goleak per §17.3 ("goroutines leak-checked
// via goleak"). A leaked accept goroutine indicates cleanup() did not
// drive Accept() to net.ErrClosed, which is the §17.3 lifecycle gate.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// shortSockPath returns a sock path under t.TempDir() that fits in
// SUN_PATH on both darwin (104) and Linux (108). t.TempDir() can be
// long on darwin (e.g. /private/var/folders/...) so we keep the
// per-test filename to 8 hex + ".sock" per the §3.2 contract.
func shortSockPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// Mirror §3.2: <8-hex>.sock. The 8-hex value here is fixed for
	// determinism — the production code uses the request ULID prefix.
	p := filepath.Join(dir, "deadbeef.sock")
	if len(p) > sunPathCeiling() {
		// Some CI runners produce extremely long TempDir paths. Fall
		// back to /tmp/ipcsock-test-* which is safely short on both
		// platforms.
		alt, err := os.MkdirTemp("/tmp", "ipcs-")
		if err != nil {
			t.Fatalf("fallback tempdir: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(alt) })
		p = filepath.Join(alt, "deadbeef.sock")
	}
	return p
}

// TestStartListener_LifecycleCleanupOnce verifies the §17.3 gate
// "Reply socket lifecycle — listener exits via net.ErrClosed; cleanup
// is sync.Once". After cleanup() returns, the sock file is unlinked
// and a second cleanup() is a safe no-op. The accept goroutine is
// leak-checked by goleak.VerifyTestMain.
func TestStartListener_LifecycleCleanupOnce(t *testing.T) {
	sockPath := shortSockPath(t)
	replies, cleanup, err := StartListener(sockPath, nil)
	if err != nil {
		t.Fatalf("StartListener: %v", err)
	}
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("sock not created: %v", err)
	}
	// The chan is non-nil and buffered cap 2.
	if replies == nil {
		t.Fatal("replies channel is nil")
	}

	// First cleanup: unlinks sock, closes listener.
	cleanup()
	if _, err := os.Stat(sockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sock not removed after cleanup: stat err=%v", err)
	}

	// Second cleanup: must be a safe no-op (sync.Once).
	cleanup()
	cleanup()

	// Give the accept goroutine a moment to exit cleanly. If it did
	// not return on net.ErrClosed, goleak fails TestMain.
	time.Sleep(20 * time.Millisecond)
}

// TestStartListener_AcceptExitsOnClose targets the half of the
// lifecycle gate that says the accept loop exits via net.ErrClosed.
// We dial once (to prove the loop is alive), then cleanup, then dial
// again and observe the dial fails because the socket is gone.
func TestStartListener_AcceptExitsOnClose(t *testing.T) {
	sockPath := shortSockPath(t)
	replies, cleanup, err := StartListener(sockPath, nil)
	if err != nil {
		t.Fatalf("StartListener: %v", err)
	}
	defer cleanup()

	// Connect, write a small reply, close.
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := conn.Write([]byte(`{"v":1,"ok":true}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.Close()

	select {
	case b := <-replies:
		if string(b) != `{"v":1,"ok":true}` {
			t.Fatalf("unexpected reply bytes: %q", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no reply received within 2 s")
	}

	// Now teardown and verify a second dial fails (sock unlinked).
	cleanup()
	_, err = net.Dial("unix", sockPath)
	if err == nil {
		t.Fatal("expected dial to fail after cleanup")
	}
}

// TestStartListener_SequentialAccept verifies the §17.3 gate
// "Reply socket sequential accept — second connection waits for first
// to close". The first connection holds the accept-loop body for a
// known duration; we dial a second connection in parallel and assert
// its server-side handling cannot begin until the first finishes.
//
// Implementation: we measure when each conn's bytes land on the
// `replies` channel. Sequential accept implies the second send-time
// is ≥ first send-time + first connection's hold duration.
func TestStartListener_SequentialAccept(t *testing.T) {
	sockPath := shortSockPath(t)
	replies, cleanup, err := StartListener(sockPath, nil)
	if err != nil {
		t.Fatalf("StartListener: %v", err)
	}
	defer cleanup()

	// First connection: writes payload "first", then sleeps 200 ms
	// before closing. The accept-loop body for this conn cannot
	// return (and therefore cannot Accept the second) until close.
	holdDur := 200 * time.Millisecond
	c1Done := make(chan struct{})
	go func() {
		defer close(c1Done)
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			t.Errorf("dial 1: %v", err)
			return
		}
		if _, err := conn.Write([]byte("first")); err != nil {
			t.Errorf("write 1: %v", err)
		}
		// Hold the connection open. The server-side handler is
		// blocked in io.ReadAll until we close (or the 2 s read
		// deadline fires). 200 ms is comfortably under the deadline.
		time.Sleep(holdDur)
		conn.Close()
	}()

	// Wait for the first dial to land server-side. We can detect this
	// by waiting briefly: the goroutine above issues Dial as the
	// first thing. A 30 ms delay before the second dial gives the
	// server time to enter Accept on conn1.
	time.Sleep(30 * time.Millisecond)

	c2Start := time.Now()
	c2Done := make(chan struct{})
	go func() {
		defer close(c2Done)
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			t.Errorf("dial 2: %v", err)
			return
		}
		if _, err := conn.Write([]byte("second")); err != nil {
			t.Errorf("write 2: %v", err)
		}
		conn.Close()
	}()

	// Read both replies and capture timing. Sequential accept means
	// the second reply only arrives AFTER the first connection closes
	// (hold duration elapsed).
	var got1, got2 []byte
	t1 := time.Time{}
	t2 := time.Time{}

	deadline := time.After(3 * time.Second)
	for got1 == nil || got2 == nil {
		select {
		case b := <-replies:
			now := time.Now()
			if string(b) == "first" && got1 == nil {
				got1 = b
				t1 = now
			} else if string(b) == "second" && got2 == nil {
				got2 = b
				t2 = now
			} else {
				t.Fatalf("unexpected/duplicate reply: %q", b)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for replies; got1=%v got2=%v",
				got1 != nil, got2 != nil)
		}
	}
	<-c1Done
	<-c2Done

	// The second send-time must be at or after the first connection's
	// close time, which is >= c2Start + (holdDur - 30 ms gap).
	// Slack: -50 ms tolerance for scheduler jitter on slow CI.
	earliestC2 := c2Start.Add(holdDur - 30*time.Millisecond - 50*time.Millisecond)
	if t2.Before(earliestC2) {
		t.Fatalf("second reply arrived before first conn closed: "+
			"t1=%v t2=%v c2Start=%v earliestC2=%v",
			t1, t2, c2Start, earliestC2)
	}
	// And t2 should be after t1.
	if !t2.After(t1) && !t2.Equal(t1) {
		t.Fatalf("second reply arrived before first: t1=%v t2=%v", t1, t2)
	}
}

// TestStartListener_ChannelBufferBlocksAtCap verifies the §17.3 gate
// "Reply channel buffer — producer blocks at cap 2; never panics".
// We send 3 replies without draining and verify that the third send
// is still pending after a generous timeout. No panic / no deadlock
// in the listener itself (we always cleanup at end).
func TestStartListener_ChannelBufferBlocksAtCap(t *testing.T) {
	sockPath := shortSockPath(t)
	replies, cleanup, err := StartListener(sockPath, nil)
	if err != nil {
		t.Fatalf("StartListener: %v", err)
	}
	defer cleanup()

	// Push 3 replies, NOT draining `replies`. The first 2 fill the
	// buffer; the third's accept-loop send should block. Sequential
	// accept means each conn's handleConn must finish (send included)
	// before Accept returns to take the next conn.
	for i := 0; i < 3; i++ {
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		payload := []byte{'r', byte('0' + i)}
		if _, err := conn.Write(payload); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		conn.Close()
	}

	// Allow some time for the listener to enqueue 2 of the 3.
	time.Sleep(100 * time.Millisecond)

	// Buffer should be full (2). The third reply is parked in the
	// accept goroutine's send statement.
	if got := len(replies); got != replyChanCap {
		t.Fatalf("expected channel len == %d (full), got %d",
			replyChanCap, got)
	}

	// Drain one slot — the parked third reply should now flow in.
	first := <-replies
	_ = first
	time.Sleep(50 * time.Millisecond)

	// Buffer should still be full (the parked reply moved into the
	// freed slot).
	if got := len(replies); got != replyChanCap {
		t.Fatalf("after drain expected channel len == %d, got %d",
			replyChanCap, got)
	}

	// Drain the rest before cleanup so the accept goroutine exits
	// cleanly when listener.Close() is called.
	<-replies
	<-replies
}

// TestStartListener_SunPathOverflow verifies the §17.3 gate
// "SUN_PATH overflow (Go side) — IPC_INIT_FAILED returned for
// over-budget runtime_dir". We construct an artificially long path
// and assert ErrSunPathOverflow.
func TestStartListener_SunPathOverflow(t *testing.T) {
	// Build a path that is guaranteed to exceed the platform ceiling
	// (104 darwin, 108 linux). We don't care if the parent dir
	// exists — the length check fires before net.Listen.
	pad := strings.Repeat("a", sunPathCeiling()+10)
	sockPath := "/tmp/" + pad + ".sock"

	_, _, err := StartListener(sockPath, nil)
	if err == nil {
		t.Fatal("expected ErrSunPathOverflow, got nil")
	}
	if !errors.Is(err, ErrSunPathOverflow) {
		t.Fatalf("expected ErrSunPathOverflow, got %v", err)
	}
}

// TestStartListener_EmptyPath asserts the empty-path argument is
// rejected up front (defensive — the dispatcher will always pass a
// non-empty path, but the API contract is clear).
func TestStartListener_EmptyPath(t *testing.T) {
	_, _, err := StartListener("", nil)
	if err == nil {
		t.Fatal("expected error on empty sockPath")
	}
}

// TestStartListener_SockMode0600 verifies the §3.2 permission contract:
// the sock file is born 0600 via the umask + chmod backstop sequence.
// Skipped on Windows (build tag would be cleaner but this whole
// package is Unix-only).
func TestStartListener_SockMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets only")
	}
	sockPath := shortSockPath(t)
	_, cleanup, err := StartListener(sockPath, nil)
	if err != nil {
		t.Fatalf("StartListener: %v", err)
	}
	defer cleanup()

	info, err := os.Stat(sockPath)
	if err != nil {
		t.Fatalf("stat sock: %v", err)
	}
	mode := info.Mode().Perm()
	if mode != 0o600 {
		t.Fatalf("expected sock mode 0600, got %#o", mode)
	}
}

// TestSweepStale_RemovesStaleSocks verifies the §3.5 / §8.7
// "Reply dir cleanup mtime 60 s" startup sweep: a *.sock file with
// mtime older than 60 s is removed; a fresh one is left in place;
// non-.sock entries are ignored entirely.
func TestSweepStale_RemovesStaleSocks(t *testing.T) {
	dir := t.TempDir()

	stale := filepath.Join(dir, "deadbeef.sock")
	fresh := filepath.Join(dir, "feedface.sock")
	other := filepath.Join(dir, "other.json")

	for _, p := range []string{stale, fresh, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}
	// Backdate stale to 90 s ago; leave fresh + other at "now".
	old := time.Now().Add(-90 * time.Second)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("chtimes stale: %v", err)
	}

	if err := SweepStale(dir, nil); err != nil {
		t.Fatalf("SweepStale: %v", err)
	}

	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale sock not removed: stat err=%v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh sock unexpectedly removed: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("non-.sock entry unexpectedly removed: %v", err)
	}
}

// TestSweepStale_MissingDir is a no-op (returns nil). The dispatcher
// creates the dir on first use; the sweep must not complain when it
// runs first on a clean install.
func TestSweepStale_MissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	if err := SweepStale(dir, nil); err != nil {
		t.Fatalf("SweepStale on missing dir: %v", err)
	}
}

// TestSweepStale_EmptyDirArg rejects empty input.
func TestSweepStale_EmptyDirArg(t *testing.T) {
	if err := SweepStale("", nil); err == nil {
		t.Fatal("expected error on empty dir")
	}
}

// TestStartListener_ConcurrentCleanupSafe ensures sync.Once-guarded
// cleanup is safe under concurrent invocation from multiple goroutines
// (e.g. signal handler + main path racing). Tests the §17.3 lifecycle
// gate's idempotency contract under contention.
func TestStartListener_ConcurrentCleanupSafe(t *testing.T) {
	sockPath := shortSockPath(t)
	_, cleanup, err := StartListener(sockPath, nil)
	if err != nil {
		t.Fatalf("StartListener: %v", err)
	}
	var wg sync.WaitGroup
	var calls atomic.Int32
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cleanup()
			calls.Add(1)
		}()
	}
	wg.Wait()
	if calls.Load() != 16 {
		t.Fatalf("expected 16 cleanup calls completed, got %d",
			calls.Load())
	}
	// Sock must be gone.
	if _, err := os.Stat(sockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sock not removed: %v", err)
	}
}
