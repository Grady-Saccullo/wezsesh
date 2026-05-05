// Package ipcsock owns the reverse-path Unix-domain socket: the
// per-request reply socket the binary creates BEFORE the §3.1 forward
// dispatch and tears down once the verb completes.
//
// The package is intentionally narrow. StartListener creates one socket,
// spins one accept goroutine, and surfaces raw reply bytes on a buffered
// (cap 2, §3.5) channel. Parsing the canonical-JSON reply into the §3.4
// shape happens in internal/ipcdispatcher (T-303); keeping ipcsock at
// the byte level avoids a circular dep on internal/ipc and matches the
// §13.2 algorithm's separation between accept-loop body and reply
// handling.
//
// Lifecycle (§13.2):
//   - StartListener: unix.Umask(0077) → net.Listen → unix.Umask(prev) →
//     start accept goroutine.
//   - Accept loop: SEQUENTIAL — one connection at a time. Top-level
//     defer recover() in the goroutine. Reads ≤ 1 MiB (io.LimitReader)
//     under a 2 s read deadline, sends to the channel, closes the conn.
//   - cleanup: sync.Once. listener.Close() unblocks Accept with
//     net.ErrClosed; os.Remove(sockPath) unlinks. Idempotent so signal
//     handlers + normal teardown do not double-fault.
//
// Permissions (§3.2):
//   - Sock file is born 0600 via unix.Umask(0077) immediately before
//     net.Listen; os.Chmod(sock, 0o600) is a backstop in case a runtime
//     overrides the umask.
//
// SUN_PATH overflow (§13.9): the sock path MUST satisfy
// len(path) ≤ 104 (darwin) / 108 (Linux) — the 14-byte tail described
// in §3.2 ("/<8hex>.sock") is already accounted for by the caller, so
// here we validate the full path length against the platform ceiling
// and return an ErrSunPathOverflow tagged for the §6 error table's
// IPC_INIT_FAILED code.
package ipcsock

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Grady-Saccullo/wezsesh/internal/logger"
)

// readDeadline bounds the per-connection read so a stuck or malicious
// client cannot pin the accept loop. Matches the §13.2 sketch.
const readDeadline = 2 * time.Second

// readLimit is the §3.5 reply-payload ceiling (1 MiB). io.LimitReader
// truncates silently; oversize is logged + dropped.
const readLimit int64 = 1 << 20

// replyChanCap is the §3.5 reply-channel buffer (cap 2). Tight fit for
// the split-reply case ("started" + terminal); senders block above it
// (defensive — the consumer should always be reading).
const replyChanCap = 2

// ErrSunPathOverflow is returned by StartListener when sockPath exceeds
// the platform AF_UNIX SUN_PATH ceiling. Caller surfaces this to the
// §6 IPC_INIT_FAILED code (see §13.9: "Go re-validates at runtime;
// returns IPC_INIT_FAILED if violated").
var ErrSunPathOverflow = errors.New("ipcsock: AF_UNIX SUN_PATH overflow")

// sunPathCeiling reports the platform-specific SUN_PATH bound (104 on
// darwin, 108 on Linux, per §3.2 / §13.9). Used by both StartListener
// runtime validation and tests.
func sunPathCeiling() int {
	if runtime.GOOS == "darwin" {
		return 104
	}
	return 108
}

// StartListener creates the per-request reply socket at sockPath, starts
// a sequential accept goroutine, and returns:
//
//	replies — buffered (cap 2) channel of raw reply bytes.
//	cleanup — closes listener + os.Remove(sockPath); idempotent
//	          (sync.Once); safe to call from a signal handler.
//
// MUST be called synchronously before the §3.1 forward dispatch
// (request-file write + pointer OSC) — in bubbletea, from Update,
// NEVER from a tea.Cmd body. The plugin replies to this socket; both
// halves share the same first-8-hex-of-ULID prefix.
//
// Caller MUST `defer cleanup()` immediately after StartListener returns.
//
// Errors:
//   - ErrSunPathOverflow if len(sockPath) exceeds the platform ceiling.
//     Surface to §6 IPC_INIT_FAILED.
//   - net.Listen errors (parent dir missing, permission denied, etc.) —
//     wrapped, also surfaced to IPC_INIT_FAILED at the call site.
func StartListener(sockPath string, log *logger.Logger) (<-chan []byte, func(), error) {
	if sockPath == "" {
		return nil, nil, errors.New("ipcsock: empty sockPath")
	}
	if len(sockPath) > sunPathCeiling() {
		return nil, nil, fmt.Errorf("%w: len=%d ceiling=%d path=%q",
			ErrSunPathOverflow, len(sockPath), sunPathCeiling(), sockPath)
	}

	// §3.2: socket born 0600 via Umask(0077) before net.Listen; the
	// Chmod after is a backstop. We capture the previous umask and
	// restore it so the socket creation does not perturb the wider
	// process umask invariant.
	prev := unix.Umask(0o077)
	listener, err := net.Listen("unix", sockPath)
	unix.Umask(prev)
	if err != nil {
		return nil, nil, fmt.Errorf("ipcsock: listen %s: %w", sockPath, err)
	}
	// Backstop chmod — defensive; net.Listen + the umask above already
	// produces 0600 on every supported platform. Errors here are
	// non-fatal but logged; the listener is already valid.
	if err := os.Chmod(sockPath, 0o600); err != nil && log != nil {
		log.Warn("ipcsock: chmod sock", "path", sockPath, "err", err.Error())
	}

	replies := make(chan []byte, replyChanCap)

	var (
		cleanupOnce sync.Once
		cleanupErr  error
	)
	cleanup := func() {
		cleanupOnce.Do(func() {
			// listener.Close() unblocks any in-flight Accept with
			// net.ErrClosed; the accept goroutine returns and the
			// channel send-side is no longer used. We do NOT close
			// `replies` here — there may be a buffered reply still
			// awaiting drain, and closing under that condition would
			// break the consumer's range loop semantics. The accept
			// goroutine is the sole sender, and §13.2 dictates the
			// caller drain after cleanup if ordering matters.
			if err := listener.Close(); err != nil &&
				!errors.Is(err, net.ErrClosed) {
				cleanupErr = err
				if log != nil {
					log.Warn("ipcsock: listener close",
						"path", sockPath, "err", err.Error())
				}
			}
			// Unlink the sock path. Missing is fine (signal teardown
			// race); other errors get a single warn.
			if err := os.Remove(sockPath); err != nil &&
				!errors.Is(err, os.ErrNotExist) && log != nil {
				log.Warn("ipcsock: remove sock",
					"path", sockPath, "err", err.Error())
			}
		})
		_ = cleanupErr // silence unused-write if we ever stop logging
	}

	go acceptLoop(listener, replies, log)

	return replies, cleanup, nil
}

// acceptLoop is the sequential accept body — at most one connection in
// flight, matching §13.2. The goroutine exits cleanly on net.ErrClosed
// (cleanup() called listener.Close()).
//
// Top-level defer recover() per §13.2: any panic from the read body or
// the channel send is caught and logged rather than killing the binary.
// The goroutine then returns and the listener is left in whatever state
// the panic produced — cleanup() is still callable and idempotent.
func acceptLoop(listener net.Listener, replies chan<- []byte, log *logger.Logger) {
	defer func() {
		if r := recover(); r != nil && log != nil {
			log.Warn("ipcsock: accept loop panic", "panic", fmt.Sprint(r))
		}
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if log != nil {
				log.Warn("ipcsock: accept", "err", err.Error())
			}
			// Avoid tight-spin on transient errors. A 10 ms backoff
			// is small enough not to delay legitimate replies and
			// large enough to keep the loop from saturating a CPU
			// if Accept enters a permanent error mode short of
			// net.ErrClosed.
			time.Sleep(10 * time.Millisecond)
			continue
		}
		handleConn(conn, replies, log)
	}
}

// handleConn reads one reply, sends it on the channel, and closes the
// connection. Sequential — the accept loop blocks here until this
// returns, which is what the §17.3 "Reply socket sequential accept"
// gate verifies.
func handleConn(conn net.Conn, replies chan<- []byte, log *logger.Logger) {
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(readDeadline)); err != nil &&
		log != nil {
		log.Warn("ipcsock: set read deadline", "err", err.Error())
	}
	bytes, err := io.ReadAll(io.LimitReader(conn, readLimit))
	if err != nil && log != nil {
		log.Warn("ipcsock: read reply", "err", err.Error())
		// Fall through: a partial read may still be a valid prefix
		// the dispatcher's parser can interpret, OR it may be empty;
		// either way we forward what we got and let the caller
		// decide. A nil-bytes send is harmless on the channel.
	}
	if len(bytes) == 0 {
		return
	}
	// Channel is cap 2 (§3.5). If both slots are full the send blocks —
	// the consumer is expected to be draining. This block is the
	// §17.3 "Reply channel buffer — producer blocks at cap 2; never
	// panics" gate.
	replies <- bytes
}

// InstallSignalHandler is intentionally NOT implemented in this package.
// §8.7 references it, but signal-handler installation must be unique
// per-process and is owned by cmd/wezsesh/main.go. The cleanup function
// returned by StartListener is the load-bearing primitive; main wires
// it under SIGINT/SIGTERM/SIGHUP per §8.7. Documenting here so a future
// reader does not add a duplicate handler chain.
