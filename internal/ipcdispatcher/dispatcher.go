// Package ipcdispatcher is the only place in the codebase that constructs a
// concrete ipc.Dispatcher. CI lint (§16.5) enforces this — every other
// package consumes the seam through internal/ipc.
//
// The Dispatcher owns the §3.1 two-phase forward dispatch end to end:
//
//  1. ipcsock.StartListener(<replyDir>/<8hex>.sock) — synchronous, before
//     any forward emission.
//  2. canonical-JSON payload assembly (canonicaljson) and HMAC sign
//     (hmac.Signer.Sign).
//  3. Phase 1: safefs.AtomicWriteFile(<runtimeDir>/req/<8hex>.json,
//     payloadBytes, 0600) — tmp+rename, fsync.
//  4. Phase 2: uservar.Writer.WriteOSC(base64(pointer)) — ≤ 256 B on the
//     wire (§3.5).
//  5. Goroutine reads raw reply bytes from the listener channel, parses
//     each reply into an ipc.Reply, forwards on the public reply channel,
//     and closes the channel after a terminal reply or ctx cancellation.
//
// The same first-8-hex-of-ULID prefix is used for both the request file
// and the reply socket (§3.2 / §12.1) so post-mortem inspection can pair
// the two by visual scan. The 8-hex prefix is derived from the raw 16-byte
// ULID (the Crockford 26-char form covers the same bytes; we hex-encode
// the first 4 bytes to produce 8 hex chars per the spec).
//
// Per-name in-process serialisation. The §13.4 save flow needs concurrent
// same-name saves to serialise within this binary so the verify-hash /
// re-hash dance is well-ordered. NameLock(name) returns the per-name
// sync.Mutex (lazily created on first use); callers Lock/Unlock around
// Phase A + Phase B + Phase C as a unit.
package ipcdispatcher

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Grady-Saccullo/wezsesh/internal/canonicaljson"
	whmac "github.com/Grady-Saccullo/wezsesh/internal/hmac"
	"github.com/Grady-Saccullo/wezsesh/internal/ipc"
	"github.com/Grady-Saccullo/wezsesh/internal/ipcsock"
	"github.com/Grady-Saccullo/wezsesh/internal/logger"
	"github.com/Grady-Saccullo/wezsesh/internal/safefs"
	"github.com/Grady-Saccullo/wezsesh/internal/uservar"
)

// protoVersion is the wire schema version echoed in `v` on every request
// and reply (§3.3 / §3.4).
const protoVersion = 1

// reqDirName is the §3.1 / §12.1 sidecar dir under runtimeDir.
const reqDirName = "req"

// replyChanCap mirrors §3.5 — split-reply (started + terminal) fits exactly.
const replyChanCap = 2

// ErrInvalidConfig is returned by New when Deps is missing a required
// field. The §6 universal error code IPC_INIT_FAILED maps to this.
var ErrInvalidConfig = errors.New("ipcdispatcher: invalid config")

// ErrReplyHMACMismatch is returned by parseReply when the reply's
// HMAC field is missing, malformed, or fails verification against the
// session key. The drain treats this as a recoverable parse-class
// error: it logs the mismatch with the request id AND forwards the
// synthesised REPLY_HMAC_MISMATCH terminal reply on the consumer
// channel (per user decision: surface, don't silent-drop).
var ErrReplyHMACMismatch = errors.New("ipcdispatcher: reply hmac mismatch")

// OSCWriter is the unexported test seam: §8.6 names *uservar.Writer
// concretely on the public Deps surface, so tests substitute a fake by
// constructing a Dispatcher value directly (see newTestDispatcher). The
// production wiring stores deps.Writer (*uservar.Writer) into the osc
// field, which satisfies this interface trivially.
type OSCWriter interface {
	WriteOSC(ctx context.Context, payload []byte) error
}

// listenerStarter abstracts ipcsock.StartListener for test substitution.
// Production wires ipcsock.StartListener directly.
type listenerStarter func(sockPath string, log *logger.Logger) (<-chan []byte, func(), error)

// nowFunc abstracts time.Now for deterministic tests.
type nowFunc func() time.Time

// idFunc abstracts ULID generation for deterministic tests. The two
// returns are the canonical 26-char Crockford-base32 ULID (id) and the
// 8-hex prefix (prefix8) derived from the same 16 raw bytes.
type idFunc func() (id, prefix8 string, err error)

// parseFunc abstracts parseReply for tests that need to inject a panic
// into the drain goroutine to exercise its top-level defer recover()
// per §13.2 / §14.2. Production wires the package-level parseReply.
type parseFunc func(raw []byte) (ipc.Reply, error)

// dialReplyFunc is the test seam used by EmergencyReply: in production
// it dials sockPath as a Unix-domain socket and writes the sentinel
// bytes through the §13.2 accept loop. Tests substitute a function that
// pushes the sentinel onto the fake-listener channel directly so the
// drain goroutine consumes it without a real socket round-trip.
type dialReplyFunc func(sockPath string, payload []byte) error

// emergencyDialTimeout bounds each per-socket dial + write during the
// §13.1 panic-recover fan-out. The whole sequence runs synchronously
// inside `defer recover()` immediately before os.Exit(2); the budget
// must be tight so a stuck socket cannot delay the exit beyond what
// the surrounding shell would tolerate.
const emergencyDialTimeout = 200 * time.Millisecond

// Deps bundles the live components the Dispatcher needs. Constructing
// the Dispatcher in one place means cmd/wezsesh only has to wire the
// dependency graph once (§8.6).
type Deps struct {
	// Writer emits the §3.1 Phase-2 pointer OSC. Concrete type per §8.6
	// prose; the dispatcher's internal field is an unexported interface
	// so tests can substitute a recording fake without opening /dev/tty.
	Writer *uservar.Writer
	// Signer signs the canonical-JSON payload (§4.3).
	Signer *whmac.Signer
	// RuntimeDir is the parent for both <runtimeDir>/req/ (request files)
	// and the reply-socket directory.
	RuntimeDir string
	// TargetWindowID is the active wezterm window (§3.3 payload field).
	TargetWindowID int
	// Logger is required; ipcsock surfaces accept-loop warnings through it.
	Logger *logger.Logger
}

// New constructs a Dispatcher backed by Deps. It creates
// <runtimeDir>/req/ with mode 0700 + Enforce(SymlinkRefuse) and verifies
// <runtimeDir> the same way (the reply-socket dir lives directly under
// runtimeDir, so a symlinked runtimeDir would compromise both halves).
//
// Returns the public Dispatcher, a cleanup closure (currently a no-op
// preserved for symmetry with §8.6's signature), and any construction
// error. Cleanup is safe to call multiple times.
func New(deps Deps) (ipc.Dispatcher, func(), error) {
	if deps.Writer == nil {
		return nil, nil, fmt.Errorf("%w: missing Writer", ErrInvalidConfig)
	}
	if deps.Signer == nil {
		return nil, nil, fmt.Errorf("%w: missing Signer", ErrInvalidConfig)
	}
	if deps.RuntimeDir == "" {
		return nil, nil, fmt.Errorf("%w: empty RuntimeDir", ErrInvalidConfig)
	}
	if deps.TargetWindowID < -1 {
		return nil, nil, fmt.Errorf("%w: TargetWindowID must be >= -1 (the §3.3 \"any window\" sentinel is -1; >= 0 are real wezterm window ids including 0)", ErrInvalidConfig)
	}

	if err := ensureDir(deps.RuntimeDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("ipcdispatcher: ensure runtime dir: %w", err)
	}
	if ok, err := safefs.Enforce(deps.RuntimeDir, safefs.SymlinkRefuse, deps.Logger); err != nil {
		return nil, nil, fmt.Errorf("ipcdispatcher: runtime dir: %w", err)
	} else if !ok {
		return nil, nil, fmt.Errorf("ipcdispatcher: runtime dir: %w", safefs.ErrIsSymlink)
	}
	reqDir := filepath.Join(deps.RuntimeDir, reqDirName)
	if err := ensureDir(reqDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("ipcdispatcher: ensure req dir: %w", err)
	}
	if ok, err := safefs.Enforce(reqDir, safefs.SymlinkRefuse, deps.Logger); err != nil {
		return nil, nil, fmt.Errorf("ipcdispatcher: req dir: %w", err)
	} else if !ok {
		return nil, nil, fmt.Errorf("ipcdispatcher: req dir: %w", safefs.ErrIsSymlink)
	}

	d := &Dispatcher{
		osc:            deps.Writer,
		signer:         deps.Signer,
		runtimeDir:     deps.RuntimeDir,
		reqDir:         reqDir,
		replyDir:       deps.RuntimeDir,
		targetWindowID: deps.TargetWindowID,
		log:            deps.Logger,
		startListener:  ipcsock.StartListener,
		now:            time.Now,
		newID:          newULID,
		dialReply:      dialReplyUnix,
		nameMutexes:    make(map[string]*sync.Mutex),
		outstanding:    make(map[string]outstandingDispatch),
	}
	d.parseReply = func(raw []byte) (ipc.Reply, error) {
		return parseReply(raw, d.signer)
	}
	cleanup := func() {}
	return d, cleanup, nil
}

// Dispatcher is the concrete §8.6 implementation. Methods are safe for
// concurrent use across distinct (verb, args) pairs; same-name save
// flows must hold NameLock(name) for the Phase A → Phase C unit (§13.4).
type Dispatcher struct {
	osc            OSCWriter
	signer         *whmac.Signer
	runtimeDir     string
	reqDir         string
	replyDir       string
	targetWindowID int
	log            *logger.Logger

	startListener listenerStarter
	now           nowFunc
	newID         idFunc
	parseReply    parseFunc
	dialReply     dialReplyFunc

	nameMutexesMu sync.Mutex
	nameMutexes   map[string]*sync.Mutex

	// outstanding tracks every in-flight Dispatch keyed by ULID id. The
	// drain goroutine removes its entry on terminal reply or ctx cancel
	// via deregisterOutstanding (LIFO defer in drain). EmergencyReply
	// snapshots this map and fans the §13.1 sentinel out to every open
	// reply socket. Guarded by outstandingMu — held only across map
	// reads/writes, NEVER across the dial-and-write I/O.
	outstandingMu sync.Mutex
	outstanding   map[string]outstandingDispatch

	// wg tracks every drain goroutine spawned by Dispatch. main.go's
	// shutdown sequence (§8.20.1 step 12) calls Wait() post-tea.Run to
	// drain deferred-phase replies before invoking cleanup. This replaces
	// v2's "open replies channels" polling per §14.2.
	wg sync.WaitGroup
}

// outstandingDispatch is the per-Dispatch record consumed by
// EmergencyReply. sockPath is the absolute reply-socket path the drain
// goroutine is listening on; on the §13.1 panic path we dial it and
// write the canonical-JSON UNEXPECTED_EXIT sentinel so the drain
// forwards a terminal reply to the user channel before os.Exit(2).
type outstandingDispatch struct {
	id       string
	sockPath string
}

// Wait blocks until every in-flight drain goroutine has returned. It is
// the §14.2 / §8.20.1 step 12 entry point: cmd/wezsesh's shutdown
// sequence calls this after program.Run() returns and before invoking
// the dispCleanup / cleanup closures.
func (d *Dispatcher) Wait() {
	d.wg.Wait()
}

// NameLock returns the per-name sync.Mutex used by the §13.4 save flow
// to serialise concurrent same-name saves within a single binary run.
// Lazy-created on first call. Callers Lock/Unlock around the full
// Phase A → Phase B → Phase C span.
//
// Distinct names get distinct mutexes — concurrent saves of "alpha" and
// "beta" do not block each other.
func (d *Dispatcher) NameLock(name string) *sync.Mutex {
	d.nameMutexesMu.Lock()
	defer d.nameMutexesMu.Unlock()
	if m, ok := d.nameMutexes[name]; ok {
		return m
	}
	m := &sync.Mutex{}
	d.nameMutexes[name] = m
	return m
}

// Dispatch performs the §3.1 forward dispatch and returns a channel of
// ipc.Reply values. The channel is closed when:
//
//   - a terminal reply ("completed" or "partial") arrives, OR
//   - ctx is cancelled (the listener cleanup unblocks the goroutine), OR
//   - the listener channel closes upstream.
//
// The goroutine that drains the listener has a top-level defer recover()
// so a malformed reply or panic in the parser cannot kill the binary.
func (d *Dispatcher) Dispatch(ctx context.Context, verb string, args map[string]any) (<-chan ipc.Reply, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if verb == "" {
		return nil, errors.New("ipcdispatcher: empty verb")
	}

	id, prefix8, err := d.newID()
	if err != nil {
		return nil, fmt.Errorf("ipcdispatcher: ulid: %w", err)
	}

	sockPath := filepath.Join(d.replyDir, prefix8+".sock")
	rawReplies, listenerCleanup, err := d.startListener(sockPath, d.log)
	if err != nil {
		return nil, fmt.Errorf("ipcdispatcher: start listener: %w", err)
	}

	// Build the §3.3 payload (sans hmac), sign, attach hmac, then encode.
	payload := d.buildPayload(id, verb, args, sockPath)
	digest, err := d.signer.Sign(payload)
	if err != nil {
		listenerCleanup()
		return nil, fmt.Errorf("ipcdispatcher: hmac sign: %w", err)
	}
	payload["hmac"] = digest
	bytesOut, err := canonicaljson.Marshal(payload)
	if err != nil {
		listenerCleanup()
		return nil, fmt.Errorf("ipcdispatcher: canonical encode: %w", err)
	}

	reqFile := prefix8 + ".json"
	reqPath := filepath.Join(d.reqDir, reqFile)
	// Phase 1 — atomic file write (§3.1 / §0.1 row 34).
	if err := safefs.AtomicWriteFile(ctx, d.reqDir, reqFile, bytesOut, 0o600); err != nil {
		listenerCleanup()
		return nil, fmt.Errorf("ipcdispatcher: phase1 write: %w", err)
	}
	// Phase 2 — pointer OSC (§3.1).
	pointerB64, err := encodePointer(id, reqPath)
	if err != nil {
		_ = os.Remove(reqPath)
		listenerCleanup()
		return nil, fmt.Errorf("ipcdispatcher: encode pointer: %w", err)
	}
	if err := d.osc.WriteOSC(ctx, pointerB64); err != nil {
		// On Phase-2 failure we OWN the unlink (the plugin never read
		// the file). §12.4 explicitly allocates this case to the binary.
		_ = os.Remove(reqPath)
		listenerCleanup()
		return nil, fmt.Errorf("ipcdispatcher: phase2 osc: %w", err)
	}

	out := make(chan ipc.Reply, replyChanCap)
	// §13.1 panic-path tracking: register the outstanding dispatch
	// BEFORE spawning the drain goroutine so EmergencyReply observes
	// every in-flight call from the moment Dispatch returns. drain
	// deregisters in its defer chain — runs whether the channel
	// terminates normally, ctx cancels, the listener closes, or the
	// recover frame fires.
	d.registerOutstanding(id, sockPath)
	// §14.2: track the drain goroutine on the WaitGroup BEFORE spawning
	// so Wait() observes it. The matching wg.Done() is the FIRST defer
	// registered inside drain → runs LAST in the LIFO chain, so it
	// fires even if the recover defer itself panics.
	d.wg.Add(1)
	go d.drain(ctx, id, rawReplies, listenerCleanup, out)
	return out, nil
}

// registerOutstanding adds (id, sockPath) to the outstanding map.
// Guarded by outstandingMu.
func (d *Dispatcher) registerOutstanding(id, sockPath string) {
	d.outstandingMu.Lock()
	defer d.outstandingMu.Unlock()
	d.outstanding[id] = outstandingDispatch{id: id, sockPath: sockPath}
}

// deregisterOutstanding removes the per-id record. Idempotent —
// EmergencyReply may have raced ahead and consumed/cleared the map
// already; in that case the missing-key delete is a no-op.
func (d *Dispatcher) deregisterOutstanding(id string) {
	d.outstandingMu.Lock()
	defer d.outstandingMu.Unlock()
	delete(d.outstanding, id)
}

// EmergencyReply implements ipc.Dispatcher. The §13.1 / §8.20.1 step 5
// panic path calls this from inside the top-level defer recover() to
// deliver a sentinel `completed`/`ok=false`/`error.code=UNEXPECTED_EXIT`
// reply to every outstanding reply socket before os.Exit(2).
//
// Concurrency: outstandingMu is held only across the map snapshot. The
// dial+write loop runs unlocked so a slow socket cannot block other
// fan-out targets, and so a Dispatch racing with EmergencyReply can
// still register/deregister freely. Each socket is removed from the
// map BEFORE the dial so a follow-on EmergencyReply call (stacked
// defer, idempotency) is a no-op.
//
// Best-effort: every dial/write error is logged + skipped. The recover
// path's job is to maximise the chance of an in-flight TUI seeing a
// terminal reply, not to guarantee delivery.
func (d *Dispatcher) EmergencyReply() {
	d.outstandingMu.Lock()
	if len(d.outstanding) == 0 {
		d.outstandingMu.Unlock()
		return
	}
	pending := make([]outstandingDispatch, 0, len(d.outstanding))
	for _, o := range d.outstanding {
		pending = append(pending, o)
	}
	// Clear the map so a stacked EmergencyReply (idempotency) is a
	// no-op. drain's deregister still works (delete on a missing key).
	d.outstanding = make(map[string]outstandingDispatch)
	d.outstandingMu.Unlock()

	for _, o := range pending {
		payload, err := buildUnexpectedExitReply(o.id, d.signer)
		if err != nil {
			if d.log != nil {
				d.log.Warn("ipcdispatcher: emergency reply encode",
					"id", o.id, "err", err.Error())
			}
			continue
		}
		if err := d.dialReply(o.sockPath, payload); err != nil {
			if d.log != nil {
				d.log.Warn("ipcdispatcher: emergency reply dial",
					"id", o.id, "sock", o.sockPath, "err", err.Error())
			}
		}
	}
}

// buildUnexpectedExitReply encodes the §13.1 sentinel reply envelope.
// Field set: {v:1, id, status:"completed", ok:false,
//
//	error:{code:"UNEXPECTED_EXIT", message:""}}
//
// `error.message` is required by §3.4 (it's the error envelope's
// non-optional pair); the empty string is the canonical placeholder
// used when no human-facing detail is available — the recover path
// has none.
//
// The sentinel is HMAC-signed via the same field-removal sequence as
// every other reply (Signer.Sign drops `hmac`, canonical-encodes the
// rest, returns the digest). The recovering TUI's parseReply rejects
// any reply whose HMAC does not verify, so an unsigned sentinel would
// be silently dropped; signing it keeps the panic-path delivery
// reliable.
func buildUnexpectedExitReply(id string, signer *whmac.Signer) ([]byte, error) {
	envelope := map[string]any{
		"v":      int64(protoVersion),
		"id":     id,
		"status": "completed",
		"ok":     false,
		"error": map[string]any{
			"code":    "UNEXPECTED_EXIT",
			"message": "",
		},
	}
	if signer != nil {
		digest, err := signer.Sign(envelope)
		if err != nil {
			return nil, err
		}
		envelope["hmac"] = digest
	}
	return canonicaljson.Marshal(envelope)
}

// dialReplyUnix is the production wire path: dial the reply socket as a
// Unix-domain stream, write the sentinel bytes, close. The plugin's
// listener (§13.2) is sequential, so writes are serialised against any
// real plugin reply that races with the panic path.
func dialReplyUnix(sockPath string, payload []byte) error {
	dialer := net.Dialer{Timeout: emergencyDialTimeout}
	conn, err := dialer.Dial("unix", sockPath)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	if err := conn.SetWriteDeadline(time.Now().Add(emergencyDialTimeout)); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}
	if _, err := conn.Write(payload); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// drain consumes raw reply bytes, parses each into an ipc.Reply, and
// forwards on out. Closes out on first terminal reply, ctx cancellation,
// or listener-channel close. Top-level defer recover() per §8.20 / §13.2.
func (d *Dispatcher) drain(
	ctx context.Context,
	requestID string,
	rawReplies <-chan []byte,
	listenerCleanup func(),
	out chan<- ipc.Reply,
) {
	// wg.Done is the FIRST defer registered → runs LAST in the LIFO
	// chain. That is intentional: it must execute even if the recover
	// block itself panics, so Wait() always sees the decrement.
	defer d.wg.Done()
	// Deregister the outstanding-dispatch record after wg.Done is
	// queued (so Wait() ordering is preserved) but before the recover
	// frame — by the time drain returns, EmergencyReply must NOT
	// re-fire onto a socket whose drain already terminated.
	defer d.deregisterOutstanding(requestID)
	defer func() {
		if r := recover(); r != nil && d.log != nil {
			d.log.Warn("ipcdispatcher: drain panic", "panic", fmt.Sprint(r))
		}
		close(out)
		listenerCleanup()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case raw, ok := <-rawReplies:
			if !ok {
				return
			}
			reply, err := d.parseReply(raw)
			if err != nil {
				if errors.Is(err, ErrReplyHMACMismatch) {
					// Surface the mismatch as a terminal reply so the
					// consumer doesn't wait out its IPC budget. The
					// synth reply has Status="completed", OK=false,
					// Error.Code="REPLY_HMAC_MISMATCH" — the drain
					// forwards it like any other terminal reply and
					// closes the channel below.
					if d.log != nil {
						d.log.Warn("ipcdispatcher: reply hmac mismatch",
							"id", requestID)
					}
				} else {
					if d.log != nil {
						d.log.Warn("ipcdispatcher: parse reply",
							"id", requestID, "err", err.Error())
					}
					continue
				}
			}
			if reply.ID != "" && reply.ID != requestID {
				if d.log != nil {
					d.log.Warn("ipcdispatcher: reply id mismatch",
						"want", requestID, "got", reply.ID)
				}
				continue
			}
			select {
			case out <- reply:
			case <-ctx.Done():
				return
			}
			if reply.Status == "completed" || reply.Status == "partial" {
				return
			}
		}
	}
}

// buildPayload assembles the §3.3 request body. The verb-specific args
// shape is the caller's responsibility; canonicaljson rejects any
// non-canonical value during encode.
func (d *Dispatcher) buildPayload(id, verb string, args map[string]any, sockPath string) map[string]any {
	if args == nil {
		args = map[string]any{}
	}
	return map[string]any{
		"v":                int64(protoVersion),
		"id":               id,
		"ts":               d.now().Unix(),
		"target_window_id": int64(d.targetWindowID),
		"reply_sock":       sockPath,
		"op":               verb,
		"args":             args,
	}
}

// ensureDir is os.MkdirAll with the strict perm we want for §12.1
// wezsesh-managed dirs. It is fine to call against an existing dir; we
// fix mode bits if they are looser than perm to enforce the §12.1
// guarantee.
func ensureDir(path string, perm fs.FileMode) error {
	if err := os.MkdirAll(path, perm); err != nil {
		return err
	}
	if err := os.Chmod(path, perm); err != nil {
		return err
	}
	return nil
}

// crockford is the §3.3 ULID alphabet (Crockford base32, sans I L O U).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newULID emits a 26-char Crockford-base32 ULID per §3.3 plus the
// 8-hex prefix used by §3.2 (reply socket) and §3.1 (request file).
//
// Layout: 48-bit ms timestamp (6 bytes) || 80-bit randomness (10 bytes).
// The 8-hex prefix is hex.EncodeToString(raw[6:10]) — the first 4 bytes
// of the random tail. We deliberately do NOT use the timestamp half:
// concurrent dispatches in the same ms would collide on the prefix and
// trip O_EXCL in safefs.AtomicWriteFile. 32 bits of randomness gives
// birthday-paradox collision at ~65k requests; for a single-binary
// session that is comfortably above any plausible request rate.
//
// Importing github.com/oklog/ulid would expand the dep graph for one
// 16-byte value; the encoder fits in a few lines.
func newULID() (id, prefix8 string, err error) {
	var raw [16]byte
	now := time.Now().UnixMilli()
	if now < 0 {
		now = 0
	}
	raw[0] = byte(now >> 40)
	raw[1] = byte(now >> 32)
	raw[2] = byte(now >> 24)
	raw[3] = byte(now >> 16)
	raw[4] = byte(now >> 8)
	raw[5] = byte(now)
	if _, err := rand.Read(raw[6:]); err != nil {
		return "", "", err
	}
	id = crockfordEncode(raw)
	// raw[0:6] = big-endian unix-millis timestamp; raw[6:10] = first 4
	// bytes of the random tail. Prefix uses the random tail so id
	// collision is bounded by birthday on 2^32 random bits per
	// ms-bucket, NOT by clock granularity (concurrent dispatches in
	// the same ms would otherwise collide on the prefix and trip
	// O_EXCL in safefs.AtomicWriteFile). See T-DOC-020.
	prefix8 = hex.EncodeToString(raw[6:10])
	return id, prefix8, nil
}

// crockfordEncode encodes 16 bytes (128 bits) into 26 Crockford-base32
// characters. The encoding consumes 5 bits at a time, MSB-first; the
// 26 × 5 = 130-bit output is right-padded with two zero bits per the
// ULID spec.
func crockfordEncode(b [16]byte) string {
	out := make([]byte, 0, 26)
	var (
		buf  uint64
		bits uint
	)
	for _, by := range b {
		buf = (buf << 8) | uint64(by)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out = append(out, crockford[(buf>>bits)&0x1F])
		}
	}
	if bits > 0 {
		out = append(out, crockford[(buf<<(5-bits))&0x1F])
	}
	if len(out) != 26 {
		panic(fmt.Sprintf("ipcdispatcher: ULID encode produced %d chars", len(out)))
	}
	return string(out)
}

// parseReply decodes §3.4 reply bytes into an ipc.Reply. Rejects any
// reply that is missing the required `v` field per §0.1 row 5, and
// verifies the reply's HMAC against the session key — both directions
// of the wire are now authenticated symmetrically.
//
// On HMAC mismatch, parseReply returns a synthesised terminal reply
// shaped as `{Status:"completed", OK:false, Error:{Code:
// "REPLY_HMAC_MISMATCH"}}` together with ErrReplyHMACMismatch. The
// drain forwards this synth reply on the consumer channel so the
// caller sees a clear error instead of waiting on the ipcCtx
// deadline (per user decision: surface, don't silent-drop).
//
// The wire shape is forgiving in §3.4 (`data` / `warnings` / `error` are
// optional), so the parser permits absence; the §3.4 invariants
// (e.g. `ok == (error is absent)`) are checked at the call site, not here.
func parseReply(raw []byte, signer *whmac.Signer) (ipc.Reply, error) {
	if len(raw) == 0 {
		return ipc.Reply{}, errors.New("ipcdispatcher: empty reply")
	}
	// First decode into a map[string]any so we can detect a missing `v`
	// key before falling through to the typed shape. UseNumber + the
	// integerizing walker below keeps numeric values as int64 so the
	// canonical re-encode inside Signer.Verify does not trip
	// canonicaljson's no-float rule (§4.1 rule 3).
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var raw1 map[string]any
	if err := dec.Decode(&raw1); err != nil {
		return ipc.Reply{}, fmt.Errorf("ipcdispatcher: decode reply: %w", err)
	}
	if _, ok := raw1["v"]; !ok {
		return ipc.Reply{}, errors.New("ipcdispatcher: reply missing v field")
	}
	if signer != nil {
		integerized, err := numbersToInt64(raw1)
		if err != nil {
			return ipc.Reply{}, fmt.Errorf("ipcdispatcher: numerics: %w", err)
		}
		intMap, _ := integerized.(map[string]any)
		ok, vErr := signer.Verify(intMap)
		if vErr != nil || !ok {
			id, _ := raw1["id"].(string)
			synth := ipc.Reply{
				V:      protoVersion,
				ID:     id,
				Status: "completed",
				OK:     false,
				Error: &ipc.ReplyError{
					Code:    "REPLY_HMAC_MISMATCH",
					Message: id,
				},
			}
			return synth, ErrReplyHMACMismatch
		}
	}
	var w wireReply
	if err := json.Unmarshal(raw, &w); err != nil {
		return ipc.Reply{}, fmt.Errorf("ipcdispatcher: decode reply (typed): %w", err)
	}
	out := ipc.Reply{
		V:      w.V,
		ID:     w.ID,
		Status: w.Status,
		OK:     w.OK,
		Data:   w.Data,
	}
	if w.Error != nil {
		out.Error = &ipc.ReplyError{
			Code:    w.Error.Code,
			Message: w.Error.Message,
			Details: w.Error.Details,
		}
	}
	if len(w.Warnings) > 0 {
		out.Warnings = make([]ipc.Warning, 0, len(w.Warnings))
		for _, ww := range w.Warnings {
			out.Warnings = append(out.Warnings, ipc.Warning{
				Code:    ww.Code,
				Message: ww.Message,
				Details: ww.Details,
			})
		}
	}
	return out, nil
}

// numbersToInt64 walks a generic JSON tree (map / slice / scalar) and
// converts every json.Number to int64. Used in parseReply on the
// HMAC-verify substrate: encoding/json with UseNumber surfaces numbers
// as json.Number which canonicaljson.Marshal does not understand;
// passing the unconverted form into Signer.Verify would fail the
// canonical re-encode under §4.1 rule 3 (no floats — but also no
// json.Number). Replies carry only integer numerics (`v` always; `ts`
// not on replies); a non-integer number rejects with a clear error.
func numbersToInt64(v any) (any, error) {
	switch x := v.(type) {
	case json.Number:
		i, err := x.Int64()
		if err != nil {
			return nil, fmt.Errorf("non-integer %q: %w", x, err)
		}
		return i, nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			conv, err := numbersToInt64(vv)
			if err != nil {
				return nil, err
			}
			out[k] = conv
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			conv, err := numbersToInt64(vv)
			if err != nil {
				return nil, err
			}
			out[i] = conv
		}
		return out, nil
	default:
		return v, nil
	}
}

// wireReply mirrors §3.4. `omitempty` on optional fields preserves the
// "data/warnings/error are optional on replies" prose. `Hmac` is the
// reply-path signature (Lua → Go); the actual verify happens on the
// first-pass `map[string]any` substrate so Signer.Verify can drive the
// field-removal sequence without re-marshalling. The typed field is
// here for documentation and so encoding/json round-trips don't drop
// the wire field.
type wireReply struct {
	V        int             `json:"v"`
	ID       string          `json:"id"`
	Status   string          `json:"status"`
	OK       bool            `json:"ok"`
	Hmac     string          `json:"hmac"`
	Data     map[string]any  `json:"data,omitempty"`
	Warnings []wireWarning   `json:"warnings,omitempty"`
	Error    *wireReplyError `json:"error,omitempty"`
}

type wireWarning struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

type wireReplyError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}
