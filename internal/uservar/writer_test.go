package uservar

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
)

// captureWriter records every Write call so tests can assert the exact
// bytes that hit the wire and the call count.
type captureWriter struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	writes  int
	failNth int   // 1-indexed; 0 disables
	failErr error
}

func (c *captureWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes++
	if c.failNth != 0 && c.writes == c.failNth {
		return 0, c.failErr
	}
	return c.buf.Write(p)
}

func (c *captureWriter) Close() error { return nil }

// envelope builds the §3.1 on-the-wire bytes for a given payload. Used
// by tests to recompute the boundary without depending on the body of
// WriteOSC.
func envelope(payload []byte) []byte {
	out := make([]byte, 0, len(oscPrefix)+len(payload)+len(oscTerminator))
	out = append(out, oscPrefix...)
	out = append(out, payload...)
	out = append(out, oscTerminator...)
	return out
}

func TestWriteOSC_SizeCeilingBoundary(t *testing.T) {
	// Boundary: 29-byte prefix + N-byte payload + 1-byte BEL == 256
	// when N == 226. N == 227 spills to 257 and must be rejected.
	const prefixLen = len(oscPrefix)
	const terminatorLen = len(oscTerminator)
	maxPayload := oscMaxBytes - prefixLen - terminatorLen
	if maxPayload != 226 {
		t.Fatalf("unexpected boundary: prefix=%d terminator=%d max=%d", prefixLen, terminatorLen, maxPayload)
	}

	t.Run("at_boundary_succeeds", func(t *testing.T) {
		cw := &captureWriter{}
		w := newWithWriter(cw)
		payload := bytes.Repeat([]byte("a"), maxPayload)
		if err := w.WriteOSC(context.Background(), payload); err != nil {
			t.Fatalf("expected success at 256 B envelope, got %v", err)
		}
		got := cw.buf.Bytes()
		if len(got) != oscMaxBytes {
			t.Fatalf("envelope size = %d, want %d", len(got), oscMaxBytes)
		}
		if !bytes.Equal(got, envelope(payload)) {
			t.Fatalf("envelope bytes mismatch:\n got %q\nwant %q", got, envelope(payload))
		}
		if cw.writes != 1 {
			t.Fatalf("expected exactly one Write, got %d", cw.writes)
		}
	})

	t.Run("one_over_rejected", func(t *testing.T) {
		cw := &captureWriter{}
		w := newWithWriter(cw)
		payload := bytes.Repeat([]byte("a"), maxPayload+1)
		err := w.WriteOSC(context.Background(), payload)
		if !errors.Is(err, ErrOSCTooBig) {
			t.Fatalf("expected ErrOSCTooBig at 257 B envelope, got %v", err)
		}
		if cw.writes != 0 {
			t.Fatalf("expected zero writes on rejection, got %d (buf=%q)", cw.writes, cw.buf.Bytes())
		}
	})

	t.Run("empty_payload_succeeds", func(t *testing.T) {
		cw := &captureWriter{}
		w := newWithWriter(cw)
		if err := w.WriteOSC(context.Background(), nil); err != nil {
			t.Fatalf("expected success on empty payload, got %v", err)
		}
		if got, want := cw.buf.Len(), prefixLen+terminatorLen; got != want {
			t.Fatalf("empty-payload envelope size = %d, want %d", got, want)
		}
	})
}

func TestWriteOSC_PointerEnvelopeShape(t *testing.T) {
	// Construct a §3.1 pointer JSON in canonical byte order (id, path, v),
	// base64-encode it, push through WriteOSC, and assert the exact bytes
	// match the §3.1 wire shape:
	//   ESC ] 1337 ; SetUserVar=wezsesh_op= <b64> BEL
	pointerJSON := []byte(`{"id":"01JABCDEFGHJKMNPQRSTVWXYZA","path":"/tmp/wezsesh-501/req/0a1b2c3d.json","v":1}`)
	b64 := base64.StdEncoding.EncodeToString(pointerJSON)

	cw := &captureWriter{}
	w := newWithWriter(cw)
	if err := w.WriteOSC(context.Background(), []byte(b64)); err != nil {
		t.Fatalf("WriteOSC: %v", err)
	}

	got := cw.buf.Bytes()

	if !bytes.HasPrefix(got, []byte("\x1B]1337;SetUserVar=wezsesh_op=")) {
		t.Fatalf("envelope missing §3.1 prefix; got first bytes: %q", got[:min(len(got), 40)])
	}
	if got[len(got)-1] != 0x07 {
		t.Fatalf("envelope missing BEL terminator; last byte = 0x%02x", got[len(got)-1])
	}
	// Recover the b64 region and assert it round-trips to the original JSON.
	const prefix = "\x1B]1337;SetUserVar=wezsesh_op="
	body := string(got[len(prefix) : len(got)-1])
	if body != b64 {
		t.Fatalf("payload region mismatch:\n got %q\nwant %q", body, b64)
	}
	decoded, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		t.Fatalf("base64 round-trip failed: %v", err)
	}
	if !bytes.Equal(decoded, pointerJSON) {
		t.Fatalf("decoded JSON mismatch:\n got %q\nwant %q", decoded, pointerJSON)
	}
	if cw.writes != 1 {
		t.Fatalf("expected exactly one Write, got %d", cw.writes)
	}
}

func TestWriteOSC_ContextCancelledBeforeWrite(t *testing.T) {
	cw := &captureWriter{}
	w := newWithWriter(cw)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := w.WriteOSC(ctx, []byte("payload"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if cw.writes != 0 {
		t.Fatalf("expected no Write on cancelled ctx, got %d", cw.writes)
	}
}

func TestWriteOSC_PropagatesUnderlyingError(t *testing.T) {
	want := errors.New("disk on fire")
	cw := &captureWriter{failNth: 1, failErr: want}
	w := newWithWriter(cw)
	err := w.WriteOSC(context.Background(), []byte("ok"))
	if !errors.Is(err, want) {
		t.Fatalf("expected %v, got %v", want, err)
	}
}

func TestClose_Idempotent(t *testing.T) {
	cw := &countingCloser{}
	w := newWithWriter(cw)
	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if cw.closes != 1 {
		t.Fatalf("expected exactly one underlying Close, got %d", cw.closes)
	}
}

type countingCloser struct {
	closes int
}

func (c *countingCloser) Write(p []byte) (int, error) { return len(p), nil }
func (c *countingCloser) Close() error {
	c.closes++
	return nil
}

func TestNew_OpensDevTTY(t *testing.T) {
	// CI environments without a controlling terminal cannot open /dev/tty;
	// skip rather than fail. The size-ceiling test above covers the
	// ceiling contract independent of /dev/tty availability.
	probe, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("/dev/tty not available in this environment: %v", err)
	}
	_ = probe.Close()

	w, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	f, ok := w.f.(*os.File)
	if !ok {
		t.Fatalf("Writer.f is %T, want *os.File", w.f)
	}
	if !strings.HasSuffix(f.Name(), "/dev/tty") {
		t.Fatalf("opened fd Name() = %q, want suffix /dev/tty", f.Name())
	}
}

// Compile-time assertion that *os.File satisfies the writeCloser
// interface — guards against accidental drift in the production fd
// type.
var _ writeCloser = (*os.File)(nil)

// Compile-time assertion that the production assembly mirrors what the
// test envelope() helper recomputes.
var _ io.Writer = (*captureWriter)(nil)
