package ipcdispatcher

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/Grady-Saccullo/wezsesh/internal/canonicaljson"
	whmac "github.com/Grady-Saccullo/wezsesh/internal/hmac"
	"github.com/Grady-Saccullo/wezsesh/internal/ipc"
	"github.com/Grady-Saccullo/wezsesh/internal/ipcsock"
	"github.com/Grady-Saccullo/wezsesh/internal/logger"
	"github.com/Grady-Saccullo/wezsesh/internal/uservar"
)

// TestMain wraps the suite with goleak per §17.3. The dispatcher spawns
// a drain goroutine per Dispatch; cleanup() must close the listener so
// goleak does not catch it.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// runtimeShortDir returns a fresh dir under either t.TempDir() or
// /tmp when t.TempDir() exceeds SUN_PATH after the per-request
// 8-hex.sock suffix (14 B). The dispatcher's reply socket lives directly
// under runtimeDir, so we need len(dir) + 14 ≤ 104 (darwin).
func runtimeShortDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if len(d)+14 <= sunPathCeiling() {
		return d
	}
	alt, err := os.MkdirTemp("/tmp", "ipcd-")
	if err != nil {
		t.Fatalf("fallback tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(alt) })
	return alt
}

func sunPathCeiling() int {
	if runtime.GOOS == "darwin" {
		return 104
	}
	return 108
}

// recordingWriter is a fake OSCWriter that captures the most recent
// payload it was handed, so tests can assert envelope semantics without
// touching /dev/tty. If `wait` is non-nil, WriteOSC blocks on it before
// returning — useful for inspecting on-disk state mid-Dispatch.
type recordingWriter struct {
	mu      sync.Mutex
	calls   int
	payload []byte
	err     error
	wait    chan struct{}
}

func (w *recordingWriter) WriteOSC(ctx context.Context, payload []byte) error {
	w.mu.Lock()
	w.calls++
	cp := make([]byte, len(payload))
	copy(cp, payload)
	w.payload = cp
	wait := w.wait
	err := w.err
	w.mu.Unlock()
	if wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

// hmacKey is a deterministic 64-hex test key. Production keys are
// crypto/rand-drawn at session start; the test value is the repeated
// nibble pattern so fixtures are reproducible.
const hmacKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func mustSigner(t *testing.T) *whmac.Signer {
	t.Helper()
	s, err := whmac.NewSigner(hmacKey)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

// signReply signs `m` against `signer` (using the same field-removal
// sequence as the Lua side) and returns the canonical-JSON bytes
// suitable for pushing through fakeListener.pushReply. Mirrors the Lua
// signer's `sign_envelope` end-to-end: encode-sans-hmac → compute →
// set m["hmac"] → re-encode. The Lua side and Go side must produce
// byte-identical bytes for the same shape — TestGoldenCorpus locks
// down the encoder, this helper locks down the signer.
func signReply(t *testing.T, signer *whmac.Signer, m map[string]any) []byte {
	t.Helper()
	digest, err := signer.Sign(m)
	if err != nil {
		t.Fatalf("signReply: Sign: %v", err)
	}
	m["hmac"] = digest
	out, err := canonicaljson.Marshal(m)
	if err != nil {
		t.Fatalf("signReply: Marshal: %v", err)
	}
	return out
}

// fakeListener is a stub listenerStarter that returns a channel the
// test can push raw reply bytes into. It mirrors ipcsock.StartListener's
// shape so production wiring is unchanged.
type fakeListener struct {
	mu       sync.Mutex
	chans    []chan []byte
	cleanups []func()
	paths    []string
}

func (f *fakeListener) start(sockPath string, _ *logger.Logger) (<-chan []byte, func(), error) {
	ch := make(chan []byte, 4)
	cleanup := func() {}
	f.mu.Lock()
	f.chans = append(f.chans, ch)
	f.cleanups = append(f.cleanups, cleanup)
	f.paths = append(f.paths, sockPath)
	f.mu.Unlock()
	return ch, cleanup, nil
}

func (f *fakeListener) pushReply(i int, raw []byte) {
	f.mu.Lock()
	ch := f.chans[i]
	f.mu.Unlock()
	ch <- raw
}

func (f *fakeListener) closeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ch := range f.chans {
		close(ch)
	}
}

// newTestDispatcher builds a Dispatcher with the given listener stub
// and OSC writer. Real safefs / canonicaljson / hmac are wired through;
// only the listener and the OSC writer are faked because they would
// otherwise need /dev/tty and a real unix socket.
//
// This is the unexported test seam called out in §8.6's design. The
// public Deps.Writer field is concrete (*uservar.Writer); tests bypass
// New() to avoid needing /dev/tty by writing directly to the
// dispatcher's unexported `osc OSCWriter` field.
func newTestDispatcher(t *testing.T, w OSCWriter, l listenerStarter, runtimeDir string) *Dispatcher {
	t.Helper()
	d := &Dispatcher{
		osc:             w,
		signer:          mustSigner(t),
		runtimeDir:      runtimeDir,
		reqDir:          filepath.Join(runtimeDir, reqDirName),
		replyDir:        runtimeDir,
		targetWindowID:  1,
		binarySessionID: testBinarySessionID,
		log:             nil,
		startListener:   l,
		now:             func() time.Time { return time.Unix(1700000000, 0) },
		newID:           newULID,
		dialReply:       dialReplyUnix,
		nameMutexes:     make(map[string]*sync.Mutex),
		outstanding:     make(map[string]outstandingDispatch),
	}
	d.parseReply = func(raw []byte) (ipc.Reply, error) {
		return parseReply(raw, d.signer)
	}
	if err := os.MkdirAll(d.reqDir, 0o700); err != nil {
		t.Fatalf("mkdir req: %v", err)
	}
	return d
}

// testBinarySessionID is the deterministic 26-char ULID stamped onto
// every test request envelope. Distinct from `id` (the per-request
// ULID) so test diagnostics can tell the two apart at a glance.
const testBinarySessionID = "01JABCDEFGHJKMNPQRSTVWXYZB"

// testPluginSessionID is the deterministic 26-char ULID echoed on
// every test reply envelope. The plugin mints this in production at
// apply_to_config; tests pin a known value so the byte-equality gate
// is reproducible.
const testPluginSessionID = "01JABCDEFGHJKMNPQRSTVWXYZC"

// TestNew_ValidatesDeps covers ErrInvalidConfig branches: each missing
// dep produces a wrapping of ErrInvalidConfig. Uses a zero-value
// *uservar.Writer because the validation paths under test never reach
// WriteOSC; the public Deps.Writer field is concrete per §8.6.
func TestNew_ValidatesDeps(t *testing.T) {
	dir := runtimeShortDir(t)
	good := Deps{
		Writer:         &uservar.Writer{},
		Signer:         mustSigner(t),
		RuntimeDir:     dir,
		TargetWindowID: 1,
	}

	cases := []struct {
		name string
		mut  func(*Deps)
	}{
		{"missing writer", func(d *Deps) { d.Writer = nil }},
		{"missing signer", func(d *Deps) { d.Signer = nil }},
		{"empty runtime dir", func(d *Deps) { d.RuntimeDir = "" }},
		{"below sentinel window id", func(d *Deps) { d.TargetWindowID = -2 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := good
			tc.mut(&d)
			_, _, err := New(d)
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("want ErrInvalidConfig, got %v", err)
			}
		})
	}
}

// TestNew_AcceptsSentinelAndZeroWindowID is the §3.3 / T-DOC-052 gate:
// `-1` is the "any window" sentinel and `0` is wezterm's first-window id;
// both MUST be accepted. Any value `< -1` is rejected (covered above).
// This is the load-bearing fix for T-905: a keybinding spawned from
// wezterm's first window emits `WINID = 0`, which under the prior
// `<= 0` gate was incorrectly rejected as "TargetWindowID must be
// positive".
func TestNew_AcceptsSentinelAndZeroWindowID(t *testing.T) {
	cases := []struct {
		name string
		wid  int
	}{
		{"sentinel -1 (any window)", -1},
		{"first window id 0", 0},
		{"second window id 1", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := runtimeShortDir(t)
			_, cleanup, err := New(Deps{
				Writer:         &uservar.Writer{},
				Signer:         mustSigner(t),
				RuntimeDir:     dir,
				TargetWindowID: tc.wid,
			})
			if err != nil {
				t.Fatalf("New rejected TargetWindowID=%d: %v", tc.wid, err)
			}
			cleanup()
		})
	}
}

// TestNew_CreatesReqDir asserts that <runtimeDir>/req is created with
// mode 0700 on construction.
func TestNew_CreatesReqDir(t *testing.T) {
	dir := runtimeShortDir(t)
	_, cleanup, err := New(Deps{
		Writer:         &uservar.Writer{},
		Signer:         mustSigner(t),
		RuntimeDir:     dir,
		TargetWindowID: 1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cleanup()
	info, err := os.Stat(filepath.Join(dir, reqDirName))
	if err != nil {
		t.Fatalf("stat req dir: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("req dir perm = %o, want 0700", info.Mode().Perm())
	}
}

// TestDispatch_ConcurrentDispatchesDisjointFiles is the §17.3
// "Request-file atomic write (spike-#3)" gate: N concurrent Dispatch
// calls produce N distinct <8-hex>.json files under <runtime_dir>/req/.
//
// We hold up the OSC writer until all goroutines have completed
// Phase 1 by blocking inside WriteOSC. This guarantees the on-disk
// fan-out is observable before any goroutine returns.
func TestDispatch_ConcurrentDispatchesDisjointFiles(t *testing.T) {
	const concurrency = 32

	dir := runtimeShortDir(t)
	gate := make(chan struct{})
	w := &recordingWriter{wait: gate}
	fl := &fakeListener{}
	d := newTestDispatcher(t, w, fl.start, dir)

	var wg sync.WaitGroup
	errs := make([]error, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := d.Dispatch(context.Background(), "noop", map[string]any{})
			errs[i] = err
		}(i)
	}

	// Wait until all goroutines are parked inside WriteOSC, which means
	// Phase 1 has completed and the on-disk fan-out is fully realised.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		got := w.calls
		w.mu.Unlock()
		if got >= concurrency {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Snapshot the req-dir before releasing WriteOSC.
	entries, err := os.ReadDir(filepath.Join(dir, reqDirName))
	if err != nil {
		t.Fatalf("read req dir: %v", err)
	}
	jsonFiles := 0
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		if seen[name] {
			t.Fatalf("duplicate req file %q", name)
		}
		seen[name] = true
		jsonFiles++
		stem := strings.TrimSuffix(name, ".json")
		if len(stem) != 8 {
			t.Errorf("req file stem len = %d, want 8: %q", len(stem), name)
		}
		if _, err := hex.DecodeString(stem); err != nil {
			t.Errorf("req file stem not hex: %q (%v)", name, err)
		}
		info, err := e.Info()
		if err != nil {
			t.Fatalf("info: %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("req file %q perm = %o, want 0600", name, info.Mode().Perm())
		}
	}
	if jsonFiles != concurrency {
		t.Fatalf("got %d req files, want %d", jsonFiles, concurrency)
	}

	close(gate)
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Errorf("dispatch %d: %v", i, e)
		}
	}

	fl.closeAll()
	time.Sleep(20 * time.Millisecond)
}

// TestDispatch_PayloadShapeAndHMAC inspects one round-trip's on-disk
// payload to confirm Phase 1 wrote canonical-JSON with a valid HMAC,
// and that the OSC payload is base64 of the §3.1 pointer JSON.
func TestDispatch_PayloadShapeAndHMAC(t *testing.T) {
	dir := runtimeShortDir(t)
	w := &recordingWriter{}
	fl := &fakeListener{}
	d := newTestDispatcher(t, w, fl.start, dir)

	if _, err := d.Dispatch(context.Background(), "noop", map[string]any{}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(dir, reqDirName))
	if err != nil {
		t.Fatalf("read req dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d req files, want 1", len(entries))
	}
	body, err := os.ReadFile(filepath.Join(dir, reqDirName, entries[0].Name()))
	if err != nil {
		t.Fatalf("read req file: %v", err)
	}

	payload, err := decodeAsCanonical(body)
	if err != nil {
		t.Fatalf("parse req body: %v", err)
	}
	supplied, ok := payload["hmac"].(string)
	if !ok || len(supplied) != 64 {
		t.Fatalf("missing or malformed hmac field: %v", payload["hmac"])
	}
	verified, vErr := d.signer.Verify(payload)
	if vErr != nil {
		t.Fatalf("signer.Verify: %v", vErr)
	}
	if !verified {
		t.Fatalf("hmac verify failed")
	}

	for _, f := range []string{"v", "id", "ts", "target_window_id", "reply_sock", "op", "args", "binary_session_id"} {
		if _, ok := payload[f]; !ok {
			t.Errorf("missing required field %q", f)
		}
	}
	if got, _ := payload["v"].(int64); got != int64(protoVersion) {
		t.Errorf("payload v = %d, want %d", got, protoVersion)
	}
	if got, _ := payload["binary_session_id"].(string); got != testBinarySessionID {
		t.Errorf("payload binary_session_id = %q, want %q", got, testBinarySessionID)
	}

	w.mu.Lock()
	osc := w.payload
	w.mu.Unlock()
	if len(osc) == 0 {
		t.Fatalf("WriteOSC was not called")
	}
	pointerJSON, err := base64.StdEncoding.DecodeString(string(osc))
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	var pointer map[string]any
	if err := json.Unmarshal(pointerJSON, &pointer); err != nil {
		t.Fatalf("parse pointer json: %v", err)
	}
	if int(pointer["v"].(float64)) != pointerVersion {
		t.Errorf("pointer.v = %v, want %d", pointer["v"], pointerVersion)
	}
	if pointer["id"] != payload["id"] {
		t.Errorf("pointer.id = %v, want %v", pointer["id"], payload["id"])
	}
	pPath, _ := pointer["path"].(string)
	wantPrefix := filepath.Join(dir, reqDirName) + string(os.PathSeparator)
	if !strings.HasPrefix(pPath, wantPrefix) {
		t.Errorf("pointer.path = %q, want prefix %q", pPath, wantPrefix)
	}

	fl.closeAll()
	time.Sleep(20 * time.Millisecond)
}

// TestDispatch_ReplyChannelDeliversParsedReply confirms the drain
// goroutine forwards a parsed ipc.Reply and closes the channel after a
// terminal status.
func TestDispatch_ReplyChannelDeliversParsedReply(t *testing.T) {
	dir := runtimeShortDir(t)
	w := &recordingWriter{}
	fl := &fakeListener{}
	d := newTestDispatcher(t, w, fl.start, dir)

	ch, err := d.Dispatch(context.Background(), "save", map[string]any{
		"name":          "alpha",
		"overwrite":     true,
		"expected_hash": canonicaljson.Null,
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	id := readRequestID(t, dir)

	reply := signReply(t, d.signer, map[string]any{
		"v":                 int64(protoVersion),
		"id":                id,
		"status":            "completed",
		"ok":                true,
		"data":              map[string]any{"hash": "sha256:abc"},
		"binary_session_id": testBinarySessionID,
		"plugin_session_id": testPluginSessionID,
	})
	fl.pushReply(0, reply)

	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatal("channel closed before reply")
		}
		if got.V != protoVersion || got.Status != "completed" || !got.OK {
			t.Errorf("reply mismatch: %+v", got)
		}
		if got.Data["hash"] != "sha256:abc" {
			t.Errorf("data.hash = %v", got.Data["hash"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no reply on channel")
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("channel produced extra message after terminal reply")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel did not close after terminal reply")
	}
}

// TestDispatch_BadHMACSurfacesMismatch is the reply-signing gate: a
// reply whose hmac field does not verify against the dispatcher's
// signer is surfaced to the consumer as a synth terminal reply with
// error.code=REPLY_HMAC_MISMATCH (per user decision: surface, don't
// silent-drop). The drain forwards the synth and closes the channel
// after — the consumer sees a clear error instead of waiting out the
// IPC budget.
func TestDispatch_BadHMACSurfacesMismatch(t *testing.T) {
	dir := runtimeShortDir(t)
	w := &recordingWriter{}
	fl := &fakeListener{}
	d := newTestDispatcher(t, w, fl.start, dir)

	ch, err := d.Dispatch(context.Background(), "noop", map[string]any{})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	id := readRequestID(t, dir)

	// Build a reply, sign it, then flip a byte of the hmac so the
	// verifier rejects it. Sign+marshal first so the field-removal
	// sequence stays canonical; we mutate the marshalled bytes after.
	envelope := map[string]any{
		"v":                 int64(protoVersion),
		"id":                id,
		"status":            "completed",
		"ok":                true,
		"data":              map[string]any{},
		"binary_session_id": testBinarySessionID,
		"plugin_session_id": testPluginSessionID,
	}
	digest, sErr := d.signer.Sign(envelope)
	if sErr != nil {
		t.Fatalf("Sign: %v", sErr)
	}
	// Flip the last hex digit. Whatever we land on, it cannot collide
	// with the real digest by construction (ConstantTimeCompare is
	// length-strict; the bad digest is still 64 hex chars).
	bad := digest[:len(digest)-1] + "0"
	if bad == digest {
		bad = digest[:len(digest)-1] + "1"
	}
	envelope["hmac"] = bad
	raw, mErr := canonicaljson.Marshal(envelope)
	if mErr != nil {
		t.Fatalf("Marshal: %v", mErr)
	}
	fl.pushReply(0, raw)

	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatal("channel closed before mismatch reply")
		}
		if got.Status != "completed" || got.OK {
			t.Errorf("synth shape wrong: %+v", got)
		}
		if got.Error == nil || got.Error.Code != "REPLY_HMAC_MISMATCH" {
			t.Errorf("error.code = %+v, want REPLY_HMAC_MISMATCH", got.Error)
		}
		if got.ID != id {
			t.Errorf("synth id = %q, want %q", got.ID, id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no synth REPLY_HMAC_MISMATCH on channel")
	}

	// Channel must close after the terminal synth reply.
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("channel produced extra message after terminal synth")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel did not close after terminal synth")
	}
}

// TestDispatch_NameMutexSerialisesSameName is the §17.3 "Save in-process
// serialisation" gate. Two concurrent goroutines acquiring NameLock for
// the same name observe a max-in-flight of 1; distinct names are not
// blocked against each other.
func TestDispatch_NameMutexSerialisesSameName(t *testing.T) {
	dir := runtimeShortDir(t)
	gate := make(chan struct{})
	w := &recordingWriter{wait: gate}
	fl := &fakeListener{}
	d := newTestDispatcher(t, w, fl.start, dir)

	var inFlight int32
	var maxInFlight int32

	releaseOnce := sync.Once{}
	releaseGate := func() { releaseOnce.Do(func() { close(gate) }) }

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer wg.Done()
			d.NameLock("alpha").Lock()
			defer d.NameLock("alpha").Unlock()
			cur := atomic.AddInt32(&inFlight, 1)
			defer atomic.AddInt32(&inFlight, -1)
			for {
				prev := atomic.LoadInt32(&maxInFlight)
				if cur <= prev {
					break
				}
				if atomic.CompareAndSwapInt32(&maxInFlight, prev, cur) {
					break
				}
			}
			go func() {
				time.Sleep(50 * time.Millisecond)
				releaseGate()
			}()
			_, err := d.Dispatch(context.Background(), "save", map[string]any{
				"name":          "alpha",
				"overwrite":     true,
				"expected_hash": canonicaljson.Null,
			})
			if err != nil {
				t.Errorf("dispatch %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&maxInFlight); got != 1 {
		t.Fatalf("max-in-flight under same-name lock = %d, want 1", got)
	}

	if d.NameLock("alpha") == d.NameLock("beta") {
		t.Fatal("NameLock returned the same mutex for distinct names")
	}
	a, b := d.NameLock("alpha"), d.NameLock("alpha")
	if a != b {
		t.Fatal("NameLock returned different mutexes for the same name")
	}

	fl.closeAll()
	time.Sleep(20 * time.Millisecond)
}

// TestDispatch_DifferentNamesDoNotBlock proves NameLock is per-name —
// two goroutines holding NameLock("alpha") and NameLock("beta") run
// concurrently.
func TestDispatch_DifferentNamesDoNotBlock(t *testing.T) {
	dir := runtimeShortDir(t)
	w := &recordingWriter{}
	fl := &fakeListener{}
	d := newTestDispatcher(t, w, fl.start, dir)

	var inFlight int32
	var maxInFlight int32
	start := make(chan struct{})

	var wg sync.WaitGroup
	for _, name := range []string{"alpha", "beta"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			d.NameLock(name).Lock()
			defer d.NameLock(name).Unlock()
			cur := atomic.AddInt32(&inFlight, 1)
			defer atomic.AddInt32(&inFlight, -1)
			for {
				prev := atomic.LoadInt32(&maxInFlight)
				if cur <= prev {
					break
				}
				if atomic.CompareAndSwapInt32(&maxInFlight, prev, cur) {
					break
				}
			}
			<-start
		}(name)
	}
	time.Sleep(50 * time.Millisecond) // let both park
	if got := atomic.LoadInt32(&inFlight); got != 2 {
		t.Fatalf("in-flight = %d, want 2 (distinct names should not block)", got)
	}
	close(start)
	wg.Wait()
	if got := atomic.LoadInt32(&maxInFlight); got != 2 {
		t.Fatalf("max-in-flight = %d, want 2", got)
	}
}

// TestParseReply_RejectsMissingV is the §0.1 row 5 gate: a reply that
// omits the `v` field is rejected. encoding/json would zero-fill silently;
// our parser does an explicit map-key check first.
func TestParseReply_RejectsMissingV(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool // true → expect error
	}{
		{"missing v", `{"id":"x","status":"completed","ok":true,"data":{}}`, true},
		{"empty body", ``, true},
		{"explicit v", `{"v":1,"id":"x","status":"completed","ok":true,"data":{}}`, false},
		{"v=0 explicit", `{"v":0,"id":"x","status":"completed","ok":true,"data":{}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseReply([]byte(tc.raw), nil)
			if tc.want && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.want && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestParseReply_ParsesError covers a "completed, ok=false" reply with
// a nested error object.
func TestParseReply_ParsesError(t *testing.T) {
	raw := `{"v":1,"id":"abc","status":"completed","ok":false,"error":{"code":"SAVE_FAILED","message":"disk full","details":{"raw_error":"ENOSPC"}}}`
	r, err := parseReply([]byte(raw), nil)
	if err != nil {
		t.Fatalf("parseReply: %v", err)
	}
	if r.OK || r.Status != "completed" {
		t.Errorf("status/ok mismatch: %+v", r)
	}
	if r.Error == nil || r.Error.Code != "SAVE_FAILED" {
		t.Fatalf("error object missing or wrong: %+v", r.Error)
	}
	if got := r.Error.Details["raw_error"]; got != "ENOSPC" {
		t.Errorf("details.raw_error = %v", got)
	}
}

// TestParseReply_ParsesPartial covers status="partial" with warnings.
func TestParseReply_ParsesPartial(t *testing.T) {
	raw := `{"v":1,"id":"abc","status":"partial","ok":true,"data":{"name":"foo"},"warnings":[{"code":"RESURRECT_PARTIAL","message":"some panes"}]}`
	r, err := parseReply([]byte(raw), nil)
	if err != nil {
		t.Fatalf("parseReply: %v", err)
	}
	if r.Status != "partial" || !r.OK {
		t.Fatalf("unexpected: %+v", r)
	}
	if len(r.Warnings) != 1 || r.Warnings[0].Code != "RESURRECT_PARTIAL" {
		t.Fatalf("warnings shape mismatch: %+v", r.Warnings)
	}
}

// TestDispatch_AcceptsContext confirms the dispatcher accepts the
// caller's context unchanged: a cancelled "other" context does not
// block Dispatch, and cancelling the dispatcher's own context unblocks
// the drain.
//
// Full §0.1 row 14 budget-independence gate is owned by the save-flow
// caller (T-800). This test only verifies the dispatcher accepts
// the caller's context unchanged; the two-budget interleaving lives
// one layer up.
func TestDispatch_AcceptsContext(t *testing.T) {
	dir := runtimeShortDir(t)
	w := &recordingWriter{}
	fl := &fakeListener{}
	d := newTestDispatcher(t, w, fl.start, dir)

	lockCtx, lockCancel := context.WithTimeout(context.Background(), 5*time.Second)
	lockCancel()
	if err := lockCtx.Err(); err == nil {
		t.Fatal("lockCtx not cancelled (test setup bug)")
	}

	ipcCtx, ipcCancel := context.WithCancel(context.Background())
	ch, err := d.Dispatch(ipcCtx, "save", map[string]any{
		"name":          "beta",
		"overwrite":     false,
		"expected_hash": canonicaljson.Null,
	})
	if err != nil {
		t.Fatalf("dispatch despite lockCtx cancelled: %v", err)
	}

	ipcCancel()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
			// Tolerate any racing in-flight reply, then loop until close.
		case <-deadline:
			t.Fatal("channel did not close within 2 s of ipcCtx cancel")
		}
	}
}

// TestDispatch_PhaseCRehashShape pins the dispatcher half of the §17.3
// "Save Phase C re-hash" gate. The dispatcher itself is verb-agnostic;
// it does not compute hashes — the caller does, post-reply. This test
// confirms a save reply's `data.hash` field is correctly surfaced
// through to the consumer.
//
// Phase C re-hash gate is hooked here; the full Phase A/B/C E2E
// save-flow harness ships in T-500 (Lua canonical_json) integration
// and T-800 (cmd/wezsesh save flow). DO NOT remove this test; it
// locks the dispatcher's parseReply Phase C hash-shape contract.
func TestDispatch_PhaseCRehashShape(t *testing.T) {
	dir := runtimeShortDir(t)
	w := &recordingWriter{}
	fl := &fakeListener{}
	d := newTestDispatcher(t, w, fl.start, dir)

	ch, err := d.Dispatch(context.Background(), "save", map[string]any{
		"name":          "gamma",
		"overwrite":     true,
		"expected_hash": canonicaljson.Null,
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	id := readRequestID(t, dir)

	wantHash := "sha256:" + strings.Repeat("ab", 32)
	reply := signReply(t, d.signer, map[string]any{
		"v":                 int64(protoVersion),
		"id":                id,
		"status":            "completed",
		"ok":                true,
		"data":              map[string]any{"name": "gamma", "hash": wantHash},
		"binary_session_id": testBinarySessionID,
		"plugin_session_id": testPluginSessionID,
	})
	fl.pushReply(0, reply)

	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatal("channel closed before reply")
		}
		if h, _ := got.Data["hash"].(string); h != wantHash {
			t.Fatalf("data.hash = %q, want %q", h, wantHash)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no reply on channel")
	}
}

// TestEncodePointer_ByteShape exercises the §3.1 pointer canonical-JSON
// + base64 envelope to confirm field ordering is `id,path,v`, the
// version is hard-coded to 1, and the b64 form decodes back.
func TestEncodePointer_ByteShape(t *testing.T) {
	id := strings.Repeat("0", ulidLen)
	path := "/tmp/wezsesh-1000/req/deadbeef.json"

	out, err := encodePointer(id, path)
	if err != nil {
		t.Fatalf("encodePointer: %v", err)
	}
	jsonBytes, err := base64.StdEncoding.DecodeString(string(out))
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	want := fmt.Sprintf(`{"id":%q,"path":%q,"v":1}`, id, path)
	if string(jsonBytes) != want {
		t.Fatalf("pointer json = %q, want %q", jsonBytes, want)
	}
}

// TestEncodePointer_RejectsBadInput pins the two error branches.
func TestEncodePointer_RejectsBadInput(t *testing.T) {
	good := "/tmp/x/req/00000000.json"
	if _, err := encodePointer("short", good); !errors.Is(err, ErrBadULID) {
		t.Errorf("short ULID: got %v, want ErrBadULID", err)
	}
	if _, err := encodePointer(strings.Repeat("0", ulidLen), ""); !errors.Is(err, ErrEmptyPath) {
		t.Errorf("empty path: got %v, want ErrEmptyPath", err)
	}
}

// TestNewULID_ShapeAndPrefix confirms the ULID is 26 Crockford-base32
// chars, the 8-hex prefix is 8 lowercase hex chars, and successive
// calls produce distinct values.
func TestNewULID_ShapeAndPrefix(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, p8, err := newULID()
		if err != nil {
			t.Fatalf("newULID: %v", err)
		}
		if len(id) != ulidLen {
			t.Fatalf("ulid len = %d, want %d", len(id), ulidLen)
		}
		for _, c := range id {
			if !strings.ContainsRune(crockford, c) {
				t.Fatalf("ulid char %q not in Crockford alphabet", c)
			}
		}
		if len(p8) != 8 {
			t.Fatalf("prefix len = %d, want 8", len(p8))
		}
		if _, err := hex.DecodeString(p8); err != nil {
			t.Fatalf("prefix not hex: %v", err)
		}
		if seen[id] {
			t.Fatalf("duplicate ulid at i=%d", i)
		}
		seen[id] = true
	}
}

// TestDispatch_RealListenerEndToEnd exercises ipcsock.StartListener
// directly (no fake) to confirm the dispatcher's wiring matches the
// real listener's contract. The reply is sent over a real Unix socket.
func TestDispatch_RealListenerEndToEnd(t *testing.T) {
	dir := runtimeShortDir(t)
	w := &recordingWriter{}
	d := newTestDispatcher(t, w, ipcsock.StartListener, dir)

	ch, err := d.Dispatch(context.Background(), "noop", map[string]any{})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	entries, _ := os.ReadDir(filepath.Join(dir, reqDirName))
	if len(entries) != 1 {
		t.Fatalf("got %d req files, want 1", len(entries))
	}
	body, _ := os.ReadFile(filepath.Join(dir, reqDirName, entries[0].Name()))
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	id := payload["id"].(string)
	sockPath := payload["reply_sock"].(string)

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial reply sock: %v", err)
	}
	reply := signReply(t, d.signer, map[string]any{
		"v":                 int64(protoVersion),
		"id":                id,
		"status":            "completed",
		"ok":                true,
		"data":              map[string]any{},
		"binary_session_id": testBinarySessionID,
		"plugin_session_id": testPluginSessionID,
	})
	if _, err := conn.Write(reply); err != nil {
		t.Fatalf("write reply: %v", err)
	}
	conn.Close()

	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatal("channel closed before reply")
		}
		if got.ID != id || got.Status != "completed" || !got.OK {
			t.Fatalf("unexpected reply: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no reply on channel")
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("channel produced extra message after terminal reply")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel did not close after terminal reply")
	}
}

// TestDispatch_RequestFileNotPinnedOpen confirms that the dispatcher
// does not hold the request file open after Phase 2; a stale-sweep or
// plugin-side unlink (§12.4 / §13.1) must succeed without contention.
func TestDispatch_RequestFileNotPinnedOpen(t *testing.T) {
	dir := runtimeShortDir(t)
	w := &recordingWriter{}
	fl := &fakeListener{}
	d := newTestDispatcher(t, w, fl.start, dir)

	if _, err := d.Dispatch(context.Background(), "noop", map[string]any{}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, reqDirName))
	if len(entries) != 1 {
		t.Fatalf("got %d req files, want 1", len(entries))
	}
	path := filepath.Join(dir, reqDirName, entries[0].Name())
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file still present after remove: %v", err)
	}

	fl.closeAll()
	time.Sleep(20 * time.Millisecond)
}

// readRequestID pulls the id field out of the single request file in
// <runtimeDir>/req/. Used by tests that need the id to forge a matching
// reply.
func readRequestID(t *testing.T, runtimeDir string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(runtimeDir, reqDirName))
	if err != nil {
		t.Fatalf("read req dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d req files, want 1", len(entries))
	}
	body, err := os.ReadFile(filepath.Join(runtimeDir, reqDirName, entries[0].Name()))
	if err != nil {
		t.Fatalf("read req file: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("parse req file: %v", err)
	}
	id, ok := payload["id"].(string)
	if !ok {
		t.Fatalf("req file missing id: %v", payload)
	}
	return id
}

// decodeAsCanonical parses JSON into a map[string]any but converts every
// numeric value to int64 (via json.Number) so the result round-trips
// through canonicaljson.Marshal — which forbids floats per §4.1 rule 3.
// encoding/json's default behaviour produces float64 for every number,
// which is incompatible with the canonical encoder; UseNumber + this
// walker is the standard workaround.
func decodeAsCanonical(b []byte) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	walked, err := walkNumbers(v)
	if err != nil {
		return nil, err
	}
	m, ok := walked.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("decodeAsCanonical: top-level not object: %T", walked)
	}
	return m, nil
}

func walkNumbers(v any) (any, error) {
	switch x := v.(type) {
	case json.Number:
		i, err := x.Int64()
		if err != nil {
			return nil, fmt.Errorf("non-integer number %q: %w", x, err)
		}
		return i, nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			conv, err := walkNumbers(vv)
			if err != nil {
				return nil, err
			}
			out[k] = conv
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			conv, err := walkNumbers(vv)
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

// TestDispatch_WaitBlocksUntilDrains is the §14.2 / §8.20.1 step 12
// gate. Wait() must block until every in-flight drain goroutine has
// returned — main.go's shutdown sequence relies on this to cleanly
// drain deferred-phase replies post-tea.Run before invoking cleanup.
//
// The test spawns a Dispatch, leaves the drain parked on its
// rawReplies select, asserts Wait() does NOT return, then closes the
// listener (which closes the rawReplies chan and unblocks drain) and
// asserts Wait() returns promptly.
func TestDispatch_WaitBlocksUntilDrains(t *testing.T) {
	dir := runtimeShortDir(t)
	w := &recordingWriter{}
	fl := &fakeListener{}
	d := newTestDispatcher(t, w, fl.start, dir)

	if _, err := d.Dispatch(context.Background(), "noop", map[string]any{}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// Run Wait in a goroutine; we expect it to block.
	done := make(chan struct{})
	go func() {
		d.Wait()
		close(done)
	}()

	// Give the goroutine plenty of time to "finish" if Wait were broken.
	select {
	case <-done:
		t.Fatal("Wait() returned while drain was still in flight")
	case <-time.After(100 * time.Millisecond):
		// Expected: drain is parked.
	}

	// Closing the listener channel unblocks drain's select-receive,
	// which falls through the !ok branch and returns. wg.Done() runs
	// last in drain's defer chain.
	fl.closeAll()

	select {
	case <-done:
		// Expected.
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() did not return after drain completion")
	}
}

// TestDispatch_DrainRecoversFromPanic exercises the §13.2 / §14.2
// top-level defer recover() in drain. The test substitutes a parseReply
// implementation that panics, pushes a reply, and asserts:
//
//	(a) the test binary survives the panic (recover caught it);
//	(b) the public reply channel closes (the recover-defer ran and
//	    closed `out` and called listenerCleanup);
//	(c) Wait() still returns — i.e. the deferred wg.Done(), which is
//	    LAST in drain's LIFO defer chain, fires after the recover frame.
//
// §17.4 lint already gates on the presence of `defer recover()`; this
// test catches silent regressions if someone moves the recover or
// breaks the defer ordering that guarantees wg.Done() outlives recover.
//
// The parseReply field is the smallest seam that lets a panic escape
// into drain without rewiring the listener or writer code paths — the
// production parseReply is pure and panic-free.
func TestDispatch_DrainRecoversFromPanic(t *testing.T) {
	dir := runtimeShortDir(t)
	w := &recordingWriter{}
	fl := &fakeListener{}
	d := newTestDispatcher(t, w, fl.start, dir)
	d.parseReply = func(raw []byte) (ipc.Reply, error) {
		panic("ipcdispatcher_test: induced parse panic")
	}

	ch, err := d.Dispatch(context.Background(), "noop", map[string]any{})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// Push any reply payload — d.parseReply will panic on receipt and
	// drain's defer recover() must catch it.
	fl.pushReply(0, []byte(`{"v":1,"id":"x","status":"completed","ok":true,"data":{}}`))

	// The recover defer closes `out` after catching the panic.
	closed := make(chan struct{})
	go func() {
		for range ch {
			// Tolerate any racing forwarded reply (there should be none
			// because the panic fires before the send), then loop to
			// channel close.
		}
		close(closed)
	}()
	select {
	case <-closed:
		// Expected.
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not close out channel after parseReply panic")
	}

	// wg.Done() fires last in drain's defer chain, even past recover.
	waitDone := make(chan struct{})
	go func() {
		d.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
		// Expected.
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() did not return after drain panic recover")
	}
}


// _ keeps logger imported for the listenerStarter signature.
var _ logger.Level = logger.LevelInfo

// fakeDialReply binds a fakeListener to a stand-in dialReplyFunc. The
// sockPath argument supplied to start() is matched against the recorded
// paths; a successful match pushes payload onto the corresponding fake
// channel so the drain goroutine consumes it as if it had arrived
// through a real Unix socket.
func (f *fakeListener) fakeDialReply() dialReplyFunc {
	return func(sockPath string, payload []byte) error {
		f.mu.Lock()
		var ch chan []byte
		for i, p := range f.paths {
			if p == sockPath {
				ch = f.chans[i]
				break
			}
		}
		f.mu.Unlock()
		if ch == nil {
			return fmt.Errorf("fakeDialReply: no fake listener for sock %q", sockPath)
		}
		ch <- payload
		return nil
	}
}

// TestEmergencyReply_OnIPCDispatcherInterface confirms the §17.3 row
// "EmergencyReply() is on the ipc.Dispatcher interface (not just on
// *ipcdispatcher.Dispatcher)". A type assertion at the package boundary
// would fail to compile if the method were missing from the interface.
func TestEmergencyReply_OnIPCDispatcherInterface(t *testing.T) {
	dir := runtimeShortDir(t)
	w := &recordingWriter{}
	fl := &fakeListener{}
	var d ipc.Dispatcher = newTestDispatcher(t, w, fl.start, dir)
	d.EmergencyReply() // empty outstanding map → no-op, must not panic.
}

// TestEmergencyReply_FanoutToOutstandingSockets is the §17.3 row
// "Panic-recover EmergencyReply fan-out". With multiple concurrent
// in-flight dispatches, EmergencyReply must deliver the
// UNEXPECTED_EXIT sentinel to each open reply socket exactly once. We
// substitute a fake dialReply that pushes onto the matching fake
// listener channel, so the drain goroutines consume the sentinel as
// if it had arrived through a real socket — and forward a terminal
// reply on the user channel.
func TestEmergencyReply_FanoutToOutstandingSockets(t *testing.T) {
	const concurrency = 8

	dir := runtimeShortDir(t)
	w := &recordingWriter{}
	fl := &fakeListener{}
	d := newTestDispatcher(t, w, fl.start, dir)
	d.dialReply = fl.fakeDialReply()

	chans := make([]<-chan ipc.Reply, concurrency)
	for i := 0; i < concurrency; i++ {
		ch, err := d.Dispatch(context.Background(), "noop", map[string]any{})
		if err != nil {
			t.Fatalf("dispatch %d: %v", i, err)
		}
		chans[i] = ch
	}

	// Sanity: the outstanding map should hold exactly N entries.
	d.outstandingMu.Lock()
	got := len(d.outstanding)
	d.outstandingMu.Unlock()
	if got != concurrency {
		t.Fatalf("outstanding before fan-out = %d, want %d", got, concurrency)
	}

	d.EmergencyReply()

	// Each user channel must receive exactly one terminal reply
	// carrying error.code=UNEXPECTED_EXIT, then close.
	for i, ch := range chans {
		select {
		case got, ok := <-ch:
			if !ok {
				t.Fatalf("dispatch %d: channel closed before reply", i)
			}
			if got.Status != "completed" || got.OK {
				t.Errorf("dispatch %d: status=%q ok=%v want completed/false",
					i, got.Status, got.OK)
			}
			if got.Error == nil || got.Error.Code != "UNEXPECTED_EXIT" {
				t.Errorf("dispatch %d: error = %+v, want code=UNEXPECTED_EXIT",
					i, got.Error)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("dispatch %d: no sentinel reply", i)
		}
		// Channel must close after the terminal reply.
		select {
		case _, ok := <-ch:
			if ok {
				t.Errorf("dispatch %d: extra message after terminal sentinel", i)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("dispatch %d: channel did not close after sentinel", i)
		}
	}

	// After all drains have terminated, the outstanding map must be
	// empty (drain's deregister defer fired).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d.outstandingMu.Lock()
		got := len(d.outstanding)
		d.outstandingMu.Unlock()
		if got == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	d.outstandingMu.Lock()
	got = len(d.outstanding)
	d.outstandingMu.Unlock()
	if got != 0 {
		t.Fatalf("outstanding after fan-out = %d, want 0", got)
	}
}

// TestEmergencyReply_Idempotent confirms the spec's "exactly once"
// invariant under stacked-defer / repeat-call conditions: a second
// EmergencyReply after the first must not deliver a duplicate sentinel
// to any of the original sockets, and must not panic. After drains
// terminate, a third call against an empty map is also a no-op.
func TestEmergencyReply_Idempotent(t *testing.T) {
	const concurrency = 3

	dir := runtimeShortDir(t)
	w := &recordingWriter{}
	fl := &fakeListener{}
	d := newTestDispatcher(t, w, fl.start, dir)

	// Count dial attempts directly so the assertion does not depend on
	// channel-buffer ordering inside the fake listener.
	var dialCount int32
	realFake := fl.fakeDialReply()
	d.dialReply = func(sockPath string, payload []byte) error {
		atomic.AddInt32(&dialCount, 1)
		return realFake(sockPath, payload)
	}

	chans := make([]<-chan ipc.Reply, concurrency)
	for i := 0; i < concurrency; i++ {
		ch, err := d.Dispatch(context.Background(), "noop", map[string]any{})
		if err != nil {
			t.Fatalf("dispatch %d: %v", i, err)
		}
		chans[i] = ch
	}

	d.EmergencyReply() // round 1 — fans out N sentinels.
	d.EmergencyReply() // round 2 — outstanding map is empty, no-op.
	d.EmergencyReply() // round 3 — still empty, still no-op.

	// Drain each user channel; assert each saw exactly one sentinel.
	for i, ch := range chans {
		gotSentinel := false
		for {
			select {
			case got, ok := <-ch:
				if !ok {
					if !gotSentinel {
						t.Fatalf("dispatch %d: closed without sentinel", i)
					}
					goto next
				}
				if got.Error != nil && got.Error.Code == "UNEXPECTED_EXIT" {
					if gotSentinel {
						t.Fatalf("dispatch %d: duplicate sentinel", i)
					}
					gotSentinel = true
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("dispatch %d: timed out", i)
			}
		}
	next:
	}
	if got := atomic.LoadInt32(&dialCount); got != concurrency {
		t.Fatalf("dial attempts = %d, want %d (exactly once per outstanding socket)",
			got, concurrency)
	}
}

// TestEmergencyReply_RaceFreeWithDispatch confirms the recover path is
// safe to call concurrently with Dispatch — the §13.1 panic could fire
// while another goroutine is mid-Dispatch. Run with -race to surface
// any data races on outstandingMu.
//
// The producer goroutine builds dispatches into a slice; a single
// drainer goroutine consumes each user channel after `stop`. The
// final post-stop EmergencyReply + listener close ensures every
// drain goroutine sees either a sentinel or a listener-channel close
// and exits cleanly so goleak doesn't catch a leak.
func TestEmergencyReply_RaceFreeWithDispatch(t *testing.T) {
	dir := runtimeShortDir(t)
	w := &recordingWriter{}
	fl := &fakeListener{}
	d := newTestDispatcher(t, w, fl.start, dir)
	d.dialReply = fl.fakeDialReply()

	stop := make(chan struct{})
	var collected []<-chan ipc.Reply
	var collectMu sync.Mutex

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			ch, err := d.Dispatch(context.Background(), "noop", map[string]any{})
			if err != nil {
				return
			}
			collectMu.Lock()
			collected = append(collected, ch)
			collectMu.Unlock()
		}
	}()

	// Hammer EmergencyReply concurrently for ~50ms.
	deadline := time.After(50 * time.Millisecond)
loop:
	for {
		select {
		case <-deadline:
			break loop
		default:
			d.EmergencyReply()
		}
	}
	close(stop)
	wg.Wait()

	// Final fan-out for any dispatches registered after the last
	// in-loop EmergencyReply call. Then close the fake listener so
	// any drain that was past the dial-attempt window exits via the
	// listener-channel-close path.
	d.EmergencyReply()
	fl.closeAll()

	// Drain every collected user channel so each drain goroutine
	// completes and Wait() observes wg.Done().
	collectMu.Lock()
	chans := append([]<-chan ipc.Reply(nil), collected...)
	collectMu.Unlock()
	for _, ch := range chans {
		for range ch {
		}
	}
	d.Wait()
}

// TestEmergencyReply_RealSocketDelivery exercises the production wire
// path end-to-end: a real Unix-domain reply socket via
// ipcsock.StartListener, with d.dialReply set to dialReplyUnix. Asserts
// the §13.1 sentinel arrives on the user channel after a dispatched
// request panics out from under us. This pins the production wiring
// against silent regression of the dial+write path.
func TestEmergencyReply_RealSocketDelivery(t *testing.T) {
	dir := runtimeShortDir(t)
	w := &recordingWriter{}
	d := newTestDispatcher(t, w, ipcsock.StartListener, dir)
	d.dialReply = dialReplyUnix

	ch, err := d.Dispatch(context.Background(), "noop", map[string]any{})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	d.EmergencyReply()

	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatal("channel closed before sentinel")
		}
		if got.Error == nil || got.Error.Code != "UNEXPECTED_EXIT" {
			t.Fatalf("got %+v, want UNEXPECTED_EXIT sentinel", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no sentinel reply on real-socket fan-out")
	}
	// Drain any post-terminal close.
	for range ch {
	}
}

// TestBuildUnexpectedExitReply_CanonicalShape pins the byte shape of
// the §13.1 sentinel. Field order on the wire must be (canonical-JSON
// sorted-key) binary_session_id / error / hmac / id / ok /
// plugin_session_id / status / v; error nested keys code / message.
// The shape is verified by re-decoding. The signed form interleaves
// the `hmac` field per byte-sorted order.
func TestBuildUnexpectedExitReply_CanonicalShape(t *testing.T) {
	id := strings.Repeat("0", 26)
	signer := mustSigner(t)
	body, err := buildUnexpectedExitReply(id, testBinarySessionID, signer)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// Recompute the expected signed bytes via the same Sign/Marshal
	// dance the function uses; the digest is deterministic given the
	// fixture key + envelope shape.
	envelope := map[string]any{
		"v":                 int64(protoVersion),
		"id":                id,
		"status":            "completed",
		"ok":                false,
		"binary_session_id": testBinarySessionID,
		// panic-path: plugin id unknown at the point of synthesis.
		"plugin_session_id": "",
		"error": map[string]any{
			"code":    "UNEXPECTED_EXIT",
			"message": "",
		},
	}
	digest, err := signer.Sign(envelope)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	envelope["hmac"] = digest
	wantBytes, err := canonicaljson.Marshal(envelope)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(body) != string(wantBytes) {
		t.Fatalf("sentinel body = %q\n         want %q", body, wantBytes)
	}
	// Round-trip through parseReply confirms the dispatcher reply
	// parser surfaces the sentinel cleanly under HMAC verification.
	r, err := parseReply(body, signer)
	if err != nil {
		t.Fatalf("parseReply: %v", err)
	}
	if r.Status != "completed" || r.OK || r.Error == nil || r.Error.Code != "UNEXPECTED_EXIT" {
		t.Fatalf("round-trip mismatch: %+v", r)
	}
}
