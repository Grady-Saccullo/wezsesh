package main

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
)

// errReader is the failure-path stub for keygenRand: every Read
// returns errInjected so subcmdKeygen exercises its entropy-error
// branch without depending on platform behaviour.
type errReader struct{}

var errInjected = errors.New("injected rand failure")

func (errReader) Read(_ []byte) (int, error) { return 0, errInjected }

// withKeygenRand swaps the package-level keygenRand for the duration
// of one subtest, restoring the previous value on cleanup so tests
// stay independent.
func withKeygenRand(t *testing.T, r io.Reader) {
	t.Helper()
	prev := keygenRand
	keygenRand = r
	t.Cleanup(func() { keygenRand = prev })
}

// TestSubcmdKeygen_Success: deterministic 32-byte input → exactly
// 65-byte stdout (64 lowercase hex + '\n'), rc 0, stderr empty. This
// is the §17.3 byte-shape gate driven through a fixed source.
func TestSubcmdKeygen_Success(t *testing.T) {
	seed := []byte{
		0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77,
		0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff,
		0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef,
		0xfe, 0xdc, 0xba, 0x98, 0x76, 0x54, 0x32, 0x10,
	}
	withKeygenRand(t, bytes.NewReader(seed))

	var stdout, stderr bytes.Buffer
	rc := subcmdKeygen(nil, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	got := stdout.Bytes()
	if len(got) != 65 {
		t.Fatalf("stdout len = %d, want 65 (got=%q)", len(got), got)
	}
	if got[64] != '\n' {
		t.Fatalf("trailing byte = %q, want '\\n'", got[64])
	}
	if !hexKey64.Match(got[:64]) {
		t.Fatalf("first 64 bytes = %q, want match for %s", got[:64], hexKey64)
	}
	const want = "00112233445566778899aabbccddeeff0123456789abcdeffedcba9876543210\n"
	if g := string(got); g != want {
		t.Fatalf("stdout = %q, want %q", g, want)
	}
}

// TestSubcmdKeygen_RandError: rand source always errors → rc is
// exitKeygen (3), stdout untouched (the §5.2 fallback chain depends
// on a clean stdout), stderr is a single non-empty line.
func TestSubcmdKeygen_RandError(t *testing.T) {
	withKeygenRand(t, errReader{})

	var stdout, stderr bytes.Buffer
	rc := subcmdKeygen(nil, &stdout, &stderr)
	if rc != exitKeygen {
		t.Fatalf("rc = %d, want %d", rc, exitKeygen)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	s := stderr.String()
	if s == "" {
		t.Fatalf("stderr empty, want non-empty diagnostic")
	}
	if strings.Count(strings.TrimRight(s, "\n"), "\n") != 0 {
		t.Fatalf("stderr is multi-line: %q", s)
	}
	if !strings.HasSuffix(s, "\n") {
		t.Fatalf("stderr missing trailing newline: %q", s)
	}
}

// TestSubcmdKeygen_ShortRead: only 16 bytes available → io.ReadFull
// errors out before any hex/encode work, rc is exitKeygen, stdout
// stays empty. Defends the §5.2 fallback contract from a short-read
// source that a naive rand.Read could otherwise let slip.
func TestSubcmdKeygen_ShortRead(t *testing.T) {
	withKeygenRand(t, bytes.NewReader(make([]byte, 16)))

	var stdout, stderr bytes.Buffer
	rc := subcmdKeygen(nil, &stdout, &stderr)
	if rc != exitKeygen {
		t.Fatalf("rc = %d, want %d", rc, exitKeygen)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if stderr.Len() == 0 {
		t.Fatalf("stderr empty, want diagnostic")
	}
}

// TestSubcmdKeygen_RealRandSmoke: with the live crypto/rand source,
// stdout matches `^[a-f0-9]{64}\n$` and len == 65. This is the
// §17.3 acceptance gate exercised against the real reader.
func TestSubcmdKeygen_RealRandSmoke(t *testing.T) {
	withKeygenRand(t, rand.Reader)

	var stdout, stderr bytes.Buffer
	rc := subcmdKeygen(nil, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	got := stdout.String()
	if len(got) != 65 {
		t.Fatalf("stdout len = %d, want 65", len(got))
	}
	gate := regexp.MustCompile(`^[a-f0-9]{64}\n$`)
	if !gate.MatchString(got) {
		t.Fatalf("stdout = %q, want match for %s", got, gate)
	}
}

// panicReader is the §13.14 panic-recover stub for keygenRand: every
// Read panics with a fixed value so subcmdKeygen exercises its top-
// level recover branch without depending on actual entropy faults.
type panicReader struct{}

func (panicReader) Read(_ []byte) (int, error) { panic("synthetic entropy panic") }

// TestSubcmdKeygen_Panic: a panic during entropy Read is caught by the
// §13.14 top-level recover. Asserts rc=exitKeygen, stdout untouched
// (so the §5.2 fallback chain can rely on a clean stdout), stderr is a
// single line matching `^wezsesh keygen: panic: .*\n$`, and the
// keygenPanicLog seam was invoked exactly once with the panic value.
func TestSubcmdKeygen_Panic(t *testing.T) {
	withKeygenRand(t, panicReader{})

	prevLog := keygenPanicLog
	var seen any
	var seenCalls int
	keygenPanicLog = func(r any) {
		seen = r
		seenCalls++
	}
	t.Cleanup(func() { keygenPanicLog = prevLog })

	var stdout, stderr bytes.Buffer
	rc := subcmdKeygen(nil, &stdout, &stderr)
	if rc != exitKeygen {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitKeygen, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	gate := regexp.MustCompile(`^wezsesh keygen: panic: .*\n$`)
	if !gate.MatchString(stderr.String()) {
		t.Fatalf("stderr = %q, want match for %s", stderr.String(), gate)
	}
	if seenCalls != 1 {
		t.Fatalf("keygenPanicLog calls = %d, want 1", seenCalls)
	}
	if got, ok := seen.(string); !ok || got != "synthetic entropy panic" {
		t.Fatalf("keygenPanicLog received %v, want \"synthetic entropy panic\"", seen)
	}
}

// TestSubcmdKeygen_TrailingArgs: §8.20 lists `wezsesh keygen` with no
// flags or args. Trailing args are rejected with rc=exitKeygen, stdout
// untouched (the §5.2 fallback chain reads stdout — a clean stdout +
// rc=3 is the documented "fall through to /dev/urandom" signal), and
// stderr carries a one-line "unexpected arguments" diagnostic.
func TestSubcmdKeygen_TrailingArgs(t *testing.T) {
	// keygenRand stays on its production value here — the rejection
	// fires before any entropy read, so we must not see a hex key on
	// stdout even with the live rand source available.
	var stdout, stderr bytes.Buffer
	rc := subcmdKeygen([]string{"foo", "bar"}, &stdout, &stderr)
	if rc != exitKeygen {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitKeygen, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	s := stderr.String()
	if !strings.HasPrefix(s, "wezsesh keygen: unexpected arguments: ") {
		t.Fatalf("stderr = %q, want \"unexpected arguments\" prefix", s)
	}
	if !strings.HasSuffix(s, "\n") {
		t.Fatalf("stderr missing trailing newline: %q", s)
	}
	if strings.Count(strings.TrimRight(s, "\n"), "\n") != 0 {
		t.Fatalf("stderr is multi-line: %q", s)
	}
}

// TestRun_KeygenRoute: dispatches through run() so the routing table
// in main.go is also covered. Exercises the success path only — the
// failure modes are unit-tested above.
func TestRun_KeygenRoute(t *testing.T) {
	seed := bytes.Repeat([]byte{0xa5}, 32)
	withKeygenRand(t, bytes.NewReader(seed))

	var stdout, stderr bytes.Buffer
	rc := run([]string{"keygen"}, &stdout, &stderr, testBinarySessionID)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
	}
	got := stdout.String()
	if len(got) != 65 || got[64] != '\n' {
		t.Fatalf("stdout = %q (len=%d), want 65 bytes ending in '\\n'", got, len(got))
	}
	if !hexKey64.MatchString(got[:64]) {
		t.Fatalf("hex segment = %q, want match for %s", got[:64], hexKey64)
	}
}

// TestRun_KeygenRoute_TrailingArgs confirms `wezsesh keygen foo` flows
// through run() and surfaces the §8.20 trailing-args rejection (rc=3,
// clean stdout) — i.e. the rejection is in the routed path, not just
// the unit-tested function body.
func TestRun_KeygenRoute_TrailingArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := run([]string{"keygen", "foo"}, &stdout, &stderr, testBinarySessionID)
	if rc != exitKeygen {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitKeygen, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "unexpected arguments") {
		t.Fatalf("stderr missing \"unexpected arguments\": %q", stderr.String())
	}
}
