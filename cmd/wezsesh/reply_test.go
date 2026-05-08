package main

import (
	"bytes"
	"encoding/base64"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// validReply is a §3.4-compliant reply payload usable as a test fixture
// where the specific shape doesn't matter. Mirrors the dispatcher's
// happy-path "completed, ok=true, data={}" terminal reply. The hmac
// field is a sentinel of 64 zeros — the relay validates *shape* not
// *signature*, so any 64-hex-lowercase value satisfies the gate.
const validReplyJSON = `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"completed","ok":true,"hmac":"0000000000000000000000000000000000000000000000000000000000000000","data":{}}`

// b64 is the StdEncoding base64 helper (matches plugin/wezsesh/b64.lua).
func b64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// shortTempDir returns a short-path tmp dir suitable for AF_UNIX sock
// files on macOS (SUN_PATH ceiling = 104 B). t.TempDir() resolves under
// /var/folders/... on darwin, which combined with a per-test prefix can
// blow past 104 B once we append a sock basename. Bypass via os.MkdirTemp
// on /tmp (≤6 B prefix), with a t.Cleanup hook for parity.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "wsr-")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// listenerSock spins up an in-process AF_UNIX listener at the returned
// path and returns a channel that receives the bytes from the first
// accepted connection plus a cleanup func.
func listenerSock(t *testing.T) (sockPath string, replyCh <-chan []byte, cleanup func()) {
	t.Helper()
	dir := shortTempDir(t)
	sockPath = filepath.Join(dir, "abcdef01.sock")
	if len(sockPath) > 100 {
		t.Fatalf("test sock path too long: %d (%q)", len(sockPath), sockPath)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ch := make(chan []byte, 1)
	go func() {
		defer close(ch)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf, _ := io.ReadAll(io.LimitReader(conn, 1<<20))
		ch <- buf
	}()
	cleanup = func() { _ = ln.Close() }
	return sockPath, ch, cleanup
}

// nonexistentSock returns a path that no listener has bound to. Uses
// shortTempDir to keep paths usable on darwin even though dial-fail
// tests don't actually create a socket.
func nonexistentSock(t *testing.T) string {
	t.Helper()
	return filepath.Join(shortTempDir(t), "missing.sock")
}

// TestSubcmdReply_HappyPath: argv = (sock, b64(validJSON)) → bytes
// arrive verbatim on the listener; rc=0; stderr empty.
func TestSubcmdReply_HappyPath(t *testing.T) {
	sock, ch, cleanup := listenerSock(t)
	defer cleanup()

	var stdout, stderr bytes.Buffer
	rc := subcmdReply([]string{sock, b64(validReplyJSON)}, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	select {
	case got := <-ch:
		if string(got) != validReplyJSON {
			t.Fatalf("listener got %q, want %q", got, validReplyJSON)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("listener never received bytes")
	}
}

// TestSubcmdReply_ArgcZero: zero args → rc=2, single-line stderr.
func TestSubcmdReply_ArgcZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := subcmdReply(nil, &stdout, &stderr)
	if rc != exitDoctorOrSubcmd {
		t.Fatalf("rc = %d, want %d", rc, exitDoctorOrSubcmd)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.HasPrefix(stderr.String(), "wezsesh reply: ") {
		t.Fatalf("stderr = %q, want \"wezsesh reply: \" prefix", stderr.String())
	}
	if !strings.HasSuffix(stderr.String(), "\n") {
		t.Fatalf("stderr missing trailing newline: %q", stderr.String())
	}
}

// TestSubcmdReply_ArgcOne: one arg → rc=2.
func TestSubcmdReply_ArgcOne(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := subcmdReply([]string{"only-one"}, &stdout, &stderr)
	if rc != exitDoctorOrSubcmd {
		t.Fatalf("rc = %d, want %d", rc, exitDoctorOrSubcmd)
	}
}

// TestSubcmdReply_ArgcThree: three args → rc=2.
func TestSubcmdReply_ArgcThree(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := subcmdReply([]string{"a", "b", "c"}, &stdout, &stderr)
	if rc != exitDoctorOrSubcmd {
		t.Fatalf("rc = %d, want %d", rc, exitDoctorOrSubcmd)
	}
}

// TestSubcmdReply_InvalidBase64: arg[1] not valid base64 → rc=2,
// stderr mentions base64.
func TestSubcmdReply_InvalidBase64(t *testing.T) {
	sock := nonexistentSock(t)
	var stdout, stderr bytes.Buffer
	rc := subcmdReply([]string{sock, "not%%valid$$base64"}, &stdout, &stderr)
	if rc != exitDoctorOrSubcmd {
		t.Fatalf("rc = %d, want %d", rc, exitDoctorOrSubcmd)
	}
	if !strings.Contains(stderr.String(), "base64") {
		t.Fatalf("stderr = %q, want \"base64\" mention", stderr.String())
	}
}

// TestSubcmdReply_NonJSON: decoded bytes are not JSON → rc=2.
func TestSubcmdReply_NonJSON(t *testing.T) {
	sock := nonexistentSock(t)
	var stdout, stderr bytes.Buffer
	rc := subcmdReply([]string{sock, b64("not json at all")}, &stdout, &stderr)
	if rc != exitDoctorOrSubcmd {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitDoctorOrSubcmd, stderr.String())
	}
}

// TestSubcmdReply_RejectionsNoDial groups every validation-failure case
// under a single t.Run table and asserts each (a) returns rc=2 and
// (b) does NOT touch the socket — the sock path is intentionally
// non-existent, so a dial would surface as the "dial:" stderr line.
func TestSubcmdReply_RejectionsNoDial(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantSub string
	}{
		{"missing v", `{"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"completed","ok":true,"hmac":"0000000000000000000000000000000000000000000000000000000000000000","data":{}}`, "missing required field: v"},
		{"missing id", `{"v":1,"status":"completed","ok":true,"hmac":"0000000000000000000000000000000000000000000000000000000000000000","data":{}}`, "missing required field: id"},
		{"missing status", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","ok":true,"hmac":"0000000000000000000000000000000000000000000000000000000000000000","data":{}}`, "missing required field: status"},
		{"missing ok", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"completed","hmac":"0000000000000000000000000000000000000000000000000000000000000000","data":{}}`, "missing required field: ok"},
		{"missing hmac", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"completed","ok":true,"data":{}}`, "missing required field: hmac"},
		{"hmac not string", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"completed","ok":true,"hmac":12345,"data":{}}`, "hmac: must be string"},
		{"hmac wrong length", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"completed","ok":true,"hmac":"abcdef","data":{}}`, "hmac: must be 64 hex chars"},
		{"hmac uppercase hex", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"completed","ok":true,"hmac":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","data":{}}`, "hmac: must be lowercase hex"},
		{"id not string", `{"v":1,"id":12345,"status":"completed","ok":true,"hmac":"0000000000000000000000000000000000000000000000000000000000000000","data":{}}`, "id: must be string"},
		{"id wrong length", `{"v":1,"id":"short","status":"completed","ok":true,"hmac":"0000000000000000000000000000000000000000000000000000000000000000","data":{}}`, "id: must be 26 chars"},
		{"status not in set", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"bogus","ok":true,"hmac":"0000000000000000000000000000000000000000000000000000000000000000","data":{}}`, "status: must be one of"},
		{"v not int", `{"v":"one","id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"completed","ok":true,"hmac":"0000000000000000000000000000000000000000000000000000000000000000","data":{}}`, "v: must be int"},
		{"ok not bool", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"completed","ok":"true","hmac":"0000000000000000000000000000000000000000000000000000000000000000","data":{}}`, "ok: must be bool"},
		{"extra top-level key", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"completed","ok":true,"hmac":"0000000000000000000000000000000000000000000000000000000000000000","data":{},"extra":1}`, "unknown top-level key"},
		{"started with data", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"started","ok":true,"hmac":"0000000000000000000000000000000000000000000000000000000000000000","data":{}}`, "status=started must not carry data"},
		{"started with warnings", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"started","ok":true,"hmac":"0000000000000000000000000000000000000000000000000000000000000000","warnings":[]}`, "status=started must not carry warnings"},
		{"completed ok=true no data", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"completed","ok":true,"hmac":"0000000000000000000000000000000000000000000000000000000000000000"}`, "status=completed,ok=true requires data"},
		{"completed ok=false no error", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"completed","ok":false,"hmac":"0000000000000000000000000000000000000000000000000000000000000000"}`, "ok=false but error absent"},
		{"partial no data", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"partial","ok":true,"hmac":"0000000000000000000000000000000000000000000000000000000000000000","warnings":[]}`, "status=partial requires data"},
		{"partial no warnings", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"partial","ok":true,"hmac":"0000000000000000000000000000000000000000000000000000000000000000","data":{}}`, "status=partial requires warnings"},
		{"ok=true with error", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"completed","ok":true,"hmac":"0000000000000000000000000000000000000000000000000000000000000000","data":{},"error":{"code":"X","message":"y"}}`, "ok=true but error present"},
		{"ok=false no error", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"completed","ok":false,"hmac":"0000000000000000000000000000000000000000000000000000000000000000"}`, "ok=false but error absent"},
		{"data not object", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"completed","ok":true,"hmac":"0000000000000000000000000000000000000000000000000000000000000000","data":[1,2]}`, "data: must be object"},
		{"warnings not array", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"partial","ok":true,"hmac":"0000000000000000000000000000000000000000000000000000000000000000","data":{},"warnings":{}}`, "warnings: must be array"},
		{"error not object", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"completed","ok":false,"hmac":"0000000000000000000000000000000000000000000000000000000000000000","error":"oops"}`, "error: must be object"},
		{"trailing data", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"completed","ok":true,"hmac":"0000000000000000000000000000000000000000000000000000000000000000","data":{}}{}`, "trailing data"},
		{"empty payload", ``, "empty payload"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			sock := nonexistentSock(t)
			var stdout, stderr bytes.Buffer
			rc := subcmdReply([]string{sock, b64(c.body)}, &stdout, &stderr)
			if rc != exitDoctorOrSubcmd {
				t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitDoctorOrSubcmd, stderr.String())
			}
			if !strings.Contains(stderr.String(), c.wantSub) {
				t.Fatalf("stderr = %q, want substring %q", stderr.String(), c.wantSub)
			}
			// No "dial:" prefix in the stderr — the rejection path
			// must short-circuit BEFORE the dial.
			if strings.Contains(stderr.String(), "dial:") {
				t.Fatalf("stderr mentions dial — rejection should happen pre-dial: %q", stderr.String())
			}
		})
	}
}

// TestSubcmdReply_StatusVariants exercises every valid (status, ok)
// cell against a real listener so the §3.4 invariants don't accidentally
// reject canonical happy-path shapes.
func TestSubcmdReply_StatusVariants(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"completed ok=true with data", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"completed","ok":true,"hmac":"0000000000000000000000000000000000000000000000000000000000000000","data":{}}`},
		{"completed ok=true with data and warnings", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"completed","ok":true,"hmac":"0000000000000000000000000000000000000000000000000000000000000000","data":{"k":"v"},"warnings":[{"code":"X","message":"y"}]}`},
		{"completed ok=false with error", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"completed","ok":false,"hmac":"0000000000000000000000000000000000000000000000000000000000000000","error":{"code":"X","message":"y"}}`},
		{"started", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"started","ok":true,"hmac":"0000000000000000000000000000000000000000000000000000000000000000"}`},
		{"partial", `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"partial","ok":true,"hmac":"0000000000000000000000000000000000000000000000000000000000000000","data":{},"warnings":[{"code":"X","message":"y"}]}`},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			sock, ch, cleanup := listenerSock(t)
			defer cleanup()
			var stdout, stderr bytes.Buffer
			rc := subcmdReply([]string{sock, b64(c.body)}, &stdout, &stderr)
			if rc != exitOK {
				t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
			}
			select {
			case got := <-ch:
				if string(got) != c.body {
					t.Fatalf("listener got %q, want %q", got, c.body)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("listener never received bytes")
			}
		})
	}
}

// TestSubcmdReply_OversizePayload: decoded > 1 MiB → rc=2, no dial.
// Uses a non-existent sock so a dial attempt would surface as a "dial:"
// stderr line; the assertion confirms it does NOT.
func TestSubcmdReply_OversizePayload(t *testing.T) {
	// Build a >1 MiB JSON-shaped string. The bytes don't have to be
	// valid JSON because the oversize gate fires BEFORE validation.
	big := bytes.Repeat([]byte("A"), (1<<20)+1)
	sock := nonexistentSock(t)

	var stdout, stderr bytes.Buffer
	rc := subcmdReply([]string{sock, base64.StdEncoding.EncodeToString(big)}, &stdout, &stderr)
	if rc != exitDoctorOrSubcmd {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitDoctorOrSubcmd, stderr.String())
	}
	if !strings.Contains(stderr.String(), "oversize") {
		t.Fatalf("stderr = %q, want \"oversize\" mention", stderr.String())
	}
	if strings.Contains(stderr.String(), "dial:") {
		t.Fatalf("stderr mentions dial — oversize must reject pre-dial: %q", stderr.String())
	}
}

// TestSubcmdReply_DialFailure: nonexistent sock → rc=2, single-line
// stderr starting with "wezsesh reply: dial:".
func TestSubcmdReply_DialFailure(t *testing.T) {
	sock := nonexistentSock(t)
	var stdout, stderr bytes.Buffer
	rc := subcmdReply([]string{sock, b64(validReplyJSON)}, &stdout, &stderr)
	if rc != exitDoctorOrSubcmd {
		t.Fatalf("rc = %d, want %d", rc, exitDoctorOrSubcmd)
	}
	if !strings.HasPrefix(stderr.String(), "wezsesh reply: dial:") {
		t.Fatalf("stderr = %q, want \"wezsesh reply: dial:\" prefix", stderr.String())
	}
	if strings.Count(strings.TrimRight(stderr.String(), "\n"), "\n") != 0 {
		t.Fatalf("stderr is multi-line: %q", stderr.String())
	}
}

// TestSubcmdReply_EmptySockPath: sock = "" → rc=2, no dial.
func TestSubcmdReply_EmptySockPath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := subcmdReply([]string{"", b64(validReplyJSON)}, &stdout, &stderr)
	if rc != exitDoctorOrSubcmd {
		t.Fatalf("rc = %d, want %d", rc, exitDoctorOrSubcmd)
	}
	if !strings.Contains(stderr.String(), "empty sock path") {
		t.Fatalf("stderr = %q, want \"empty sock path\"", stderr.String())
	}
}

// TestRun_ReplyRoute: dispatches through run() so the routing table in
// main.go is also covered. Uses a real listener; success branch only
// (failure modes unit-tested above).
func TestRun_ReplyRoute(t *testing.T) {
	sock, ch, cleanup := listenerSock(t)
	defer cleanup()

	var stdout, stderr bytes.Buffer
	rc := run([]string{"reply", sock, b64(validReplyJSON)}, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
	}
	select {
	case got := <-ch:
		if string(got) != validReplyJSON {
			t.Fatalf("listener got %q, want %q", got, validReplyJSON)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("listener never received bytes")
	}
}

// TestSubcmdReply_Panic: a panic in replyValidate is caught by the
// §13.14 top-level recover. Asserts rc=exitDoctorOrSubcmd, stderr is a
// single line matching `^wezsesh reply: panic: .*\n$`, and the
// replyPanicLog seam was invoked exactly once with the panic value.
func TestSubcmdReply_Panic(t *testing.T) {
	prev := replyValidate
	replyValidate = func([]byte) error { panic("synthetic validate panic") }
	t.Cleanup(func() { replyValidate = prev })

	prevLog := replyPanicLog
	var seen any
	var seenCalls int
	replyPanicLog = func(r any) {
		seen = r
		seenCalls++
	}
	t.Cleanup(func() { replyPanicLog = prevLog })

	sock := nonexistentSock(t)
	var stdout, stderr bytes.Buffer
	rc := subcmdReply([]string{sock, b64(validReplyJSON)}, &stdout, &stderr)
	if rc != exitDoctorOrSubcmd {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitDoctorOrSubcmd, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	gate := regexp.MustCompile(`^wezsesh reply: panic: .*\n$`)
	if !gate.MatchString(stderr.String()) {
		t.Fatalf("stderr = %q, want match for %s", stderr.String(), gate)
	}
	if seenCalls != 1 {
		t.Fatalf("replyPanicLog calls = %d, want 1", seenCalls)
	}
	if got, ok := seen.(string); !ok || got != "synthetic validate panic" {
		t.Fatalf("replyPanicLog received %v, want \"synthetic validate panic\"", seen)
	}
}
