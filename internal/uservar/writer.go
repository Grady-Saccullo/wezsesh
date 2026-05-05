// Package uservar emits the §3.1 pointer OSC over /dev/tty under a
// package-local mutex.
//
// The writer is intentionally minimal: a single fd held open for the
// lifetime of the binary, a single mutex to serialise wezsesh's own
// pointer-OSC writes, and a runtime ceiling that rejects any payload
// whose on-the-wire envelope exceeds 256 B (§3.5). The ceiling is
// correctness-load-bearing — spike #3 demonstrated that OSC sequences
// large enough to span more than one kernel TTY-write syscall race with
// bubbletea's renderer at the byte level and abort wezterm's OSC parser
// per ECMA-48. The full request payload travels via a sidecar file in
// <runtime_dir>/req/ and never touches the TTY.
//
// /dev/tty (NOT os.Stdout): bubbletea's renderer holds a mutex on
// os.Stdout that this package cannot share. Writing pointer-OSC bytes
// down a different fd to the controlling terminal driver sidesteps that
// mutex and keeps the pointer OSC single-syscall on every supported
// platform.
package uservar

import (
	"context"
	"errors"
	"os"
	"sync"
	"syscall"
)

// oscPrefix is the fixed 29-byte ESC ] 1337 ; SetUserVar=wezsesh_op=
// header (§3.1). oscTerminator is BEL.
const (
	oscPrefix     = "\x1B]1337;SetUserVar=wezsesh_op="
	oscTerminator = "\x07"
	oscMaxBytes   = 256
)

// ErrOSCTooBig is returned by WriteOSC when the assembled on-the-wire
// envelope (prefix + payload + BEL) exceeds 256 B. Spelled as a sentinel
// so callers can distinguish ceiling violations from I/O failures.
var ErrOSCTooBig = errors.New("uservar: OSC envelope exceeds 256 B")

// Writer wraps /dev/tty under a mutex. SAFE to call from tea.Cmd
// bodies — but ONLY for ≤ 256 B payloads (§3.5).
type Writer struct {
	mu        sync.Mutex
	f         writeCloser
	closeOnce sync.Once
}

// writeCloser is the minimal interface the writer needs from its
// underlying fd. Production code uses *os.File; tests substitute a
// pipe-backed fake via newWithWriter.
type writeCloser interface {
	Write(p []byte) (int, error)
	Close() error
}

// New opens /dev/tty for writing. O_WRONLY|O_CLOEXEC keeps the fd out
// of any forked child's fd table.
func New() (*Writer, error) {
	f, err := os.OpenFile("/dev/tty", os.O_WRONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return newWithWriter(f), nil
}

// newWithWriter wraps an arbitrary writeCloser. Test-only entry point;
// production New() always passes the /dev/tty fd. Keeping the production
// envelope-assembly + ceiling check in WriteOSC means the §16.5 grep
// gate fires on the real body, never on the test fake.
func newWithWriter(f writeCloser) *Writer {
	return &Writer{f: f}
}

// WriteOSC emits one OSC 1337 SetUserVar=wezsesh_op=<payload> sequence
// to /dev/tty. payload is the base64-encoded canonical-JSON pointer
// (§3.1); the caller MUST have base64-encoded it already.
//
// The full envelope (29-byte prefix + payload + 1-byte BEL) is bounded
// at 256 B. Anything larger is rejected with ErrOSCTooBig before any
// write(2) — the ceiling guards the §3.1 single-syscall property and is
// the exact check the §16.5 lint grep targets.
func (w *Writer) WriteOSC(ctx context.Context, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	out := make([]byte, 0, len(oscPrefix)+len(payload)+len(oscTerminator))
	out = append(out, oscPrefix...)
	out = append(out, payload...)
	out = append(out, oscTerminator...)
	if len(out) > 256 {
		return ErrOSCTooBig
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err := w.f.Write(out)
	return err
}

// Close releases the /dev/tty fd. Idempotent via sync.Once so
// cmd/wezsesh's shutdown can call it without coordination.
func (w *Writer) Close() error {
	var err error
	w.closeOnce.Do(func() {
		err = w.f.Close()
	})
	return err
}
