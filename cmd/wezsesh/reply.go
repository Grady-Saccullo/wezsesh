package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

// replyDialTimeout bounds the connect+write half of the reply path. The
// listener side reads under a 2 s deadline (`internal/ipcsock.readDeadline`);
// matching that on the writer side keeps the symmetry. §3.4 + §14.1 row
// "Per-connection reply read".
const replyDialTimeout = 2 * time.Second

// replyMaxBytes mirrors the listener's `io.LimitReader` cap (§3.5: reply
// payload size 1 MiB). Decoded payloads larger than this are rejected
// pre-dial: the listener would silently truncate them, producing a
// canonical-shape parse failure on the dispatcher side that surfaces as
// IPC_TIMEOUT — better to fail fast at the writer with a one-line stderr.
const replyMaxBytes = 1 << 20

// replyValidate is the §3.4 reply-shape gate. Production points it at
// validateReplyShape; tests swap in a panicking stub to drive the
// §13.14 top-level recover branch in subcmdReply (mirrors keygenRand /
// keygenPanicLog from `keygen.go`).
var replyValidate func([]byte) error = validateReplyShape

// replyPanicLog is the §13.14 LevelError seam. Production main() does
// NOT wire a logger — `wezsesh reply` runs in §8.20.1 step 3 (minimal
// env, no listener, no §8.20.1 step 4.3 logger). Tests stub this to
// observe the seam fired with the panic value.
var replyPanicLog func(r any)

// subcmdReply implements `wezsesh reply <sock> <b64json>` (§8.20).
//
// Flow:
//  1. argc == 2.
//  2. base64-decode arg[1] using StdEncoding (matches the Lua side's
//     b64.encode — see plugin/wezsesh/b64.lua and the b64_spec.lua
//     comment at line 97 documenting the Go-side decoder pairing).
//  3. Validate the decoded bytes against the §3.4 reply shape.
//  4. Reject decoded payloads > 1 MiB (the listener's cap) WITHOUT
//     dialing the socket.
//  5. net.DialTimeout("unix", sock, 2 s) + 2 s write deadline; one
//     write of the decoded bytes; close.
//  6. rc=0 on clean write+close; rc=2 on any error.
//
// §13.14: top-level `defer recover()` matching keygen's pattern. The
// reply path holds no reply socket, so on panic we just stderr+rc=2.
// rc for reply per §13.14 is 2 (exitDoctorOrSubcmd).
func subcmdReply(rest []string, _, stderr io.Writer) (rc int) {
	defer func() {
		if r := recover(); r != nil {
			if replyPanicLog != nil {
				replyPanicLog(r)
			}
			fmt.Fprintf(stderr, "wezsesh reply: panic: %v\n", r)
			rc = exitDoctorOrSubcmd
		}
	}()

	// §3.3 v=2 trace correlation: the spawning plugin (result.lua) sets
	// WEZSESH_BINARY_SESSION_ID on the spawn env to the originating
	// dispatcher's id (echoed off the reply envelope). reply.go does not
	// currently construct a structured logger — `wezsesh reply` is a
	// minimal short-lived process per §8.20.1 step 3 — so this read is
	// future-proofing for a logger.New(stateDir, level, binarySessionID)
	// wiring in a later phase. Keeping the read here documents the
	// contract surface so a future reader sees the env var is consumed.
	_ = os.Getenv("WEZSESH_BINARY_SESSION_ID")

	if len(rest) != 2 {
		fmt.Fprintf(stderr, "wezsesh reply: usage: wezsesh reply <sock> <b64json>\n")
		return exitDoctorOrSubcmd
	}
	sockPath := rest[0]
	b64 := rest[1]

	if sockPath == "" {
		fmt.Fprintf(stderr, "wezsesh reply: empty sock path\n")
		return exitDoctorOrSubcmd
	}

	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh reply: base64 decode: %v\n", err)
		return exitDoctorOrSubcmd
	}

	if len(decoded) > replyMaxBytes {
		fmt.Fprintf(stderr, "wezsesh reply: oversize payload: %d bytes (max %d)\n",
			len(decoded), replyMaxBytes)
		return exitDoctorOrSubcmd
	}

	if err := replyValidate(decoded); err != nil {
		fmt.Fprintf(stderr, "wezsesh reply: %v\n", err)
		return exitDoctorOrSubcmd
	}

	conn, err := net.DialTimeout("unix", sockPath, replyDialTimeout)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh reply: dial: %v\n", err)
		return exitDoctorOrSubcmd
	}
	defer conn.Close()

	if err := conn.SetWriteDeadline(time.Now().Add(replyDialTimeout)); err != nil {
		fmt.Fprintf(stderr, "wezsesh reply: set deadline: %v\n", err)
		return exitDoctorOrSubcmd
	}

	if _, err := conn.Write(decoded); err != nil {
		fmt.Fprintf(stderr, "wezsesh reply: write: %v\n", err)
		return exitDoctorOrSubcmd
	}

	return exitOK
}

// validateReplyShape enforces the §3.4 structural invariants on the
// decoded reply bytes. Returns nil iff the bytes parse as a single JSON
// object that:
//   - has the four required fields v(int), id(26-char string),
//     status(∈ {completed,started,partial}), ok(bool);
//   - carries the right combination of optional data / warnings / error
//     for the (status, ok) cell;
//   - has no extra top-level keys.
//
// The Lua side does NOT canonical-encode replies (canonical-JSON is the
// request-signing format only — see §4 / §3.3); "non-canonical reply
// shape" in T-803 means the §3.4 schema invariants here, not byte-level
// canonical form.
func validateReplyShape(decoded []byte) error {
	if len(decoded) == 0 {
		return errors.New("empty payload")
	}

	// Decode into a generic map first so we can enforce field-presence
	// and reject extras. Using json.Decoder with DisallowUnknownFields
	// is impractical here — there is no struct that perfectly matches
	// the union (data/warnings/error are all optional), and the §3.4
	// invariants are about presence, not just type.
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(decoded))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("json decode: %w", err)
	}
	// Reject trailing data — §3.4 "Single JSON object, one write per
	// reply". A pile of additional tokens after the object body is not
	// a single object.
	if dec.More() {
		return errors.New("trailing data after json object")
	}

	allowed := map[string]struct{}{
		"v":                 {},
		"id":                {},
		"status":            {},
		"ok":                {},
		"hmac":              {},
		"data":              {},
		"warnings":          {},
		"error":             {},
		"binary_session_id": {},
		"plugin_session_id": {},
	}
	for k := range raw {
		if _, ok := allowed[k]; !ok {
			return fmt.Errorf("unknown top-level key %q", k)
		}
	}

	// Required fields.
	vRaw, ok := raw["v"]
	if !ok {
		return errors.New("missing required field: v")
	}
	var vNum json.Number
	if err := json.Unmarshal(vRaw, &vNum); err != nil {
		return fmt.Errorf("v: must be int: %w", err)
	}
	if _, err := vNum.Int64(); err != nil {
		return fmt.Errorf("v: must be int: %w", err)
	}

	idRaw, ok := raw["id"]
	if !ok {
		return errors.New("missing required field: id")
	}
	var idStr string
	if err := json.Unmarshal(idRaw, &idStr); err != nil {
		return fmt.Errorf("id: must be string: %w", err)
	}
	if len(idStr) != 26 {
		return fmt.Errorf("id: must be 26 chars (got %d)", len(idStr))
	}

	hmacRaw, ok := raw["hmac"]
	if !ok {
		return errors.New("missing required field: hmac")
	}
	var hmacStr string
	if err := json.Unmarshal(hmacRaw, &hmacStr); err != nil {
		return fmt.Errorf("hmac: must be string: %w", err)
	}
	if len(hmacStr) != 64 {
		return fmt.Errorf("hmac: must be 64 hex chars (got %d)", len(hmacStr))
	}
	for i := 0; i < len(hmacStr); i++ {
		c := hmacStr[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return fmt.Errorf("hmac: must be lowercase hex (got %q at offset %d)", c, i)
		}
	}

	statusRaw, ok := raw["status"]
	if !ok {
		return errors.New("missing required field: status")
	}
	var status string
	if err := json.Unmarshal(statusRaw, &status); err != nil {
		return fmt.Errorf("status: must be string: %w", err)
	}
	switch status {
	case "completed", "started", "partial":
	default:
		return fmt.Errorf("status: must be one of completed/started/partial (got %q)", status)
	}

	okRaw, ok := raw["ok"]
	if !ok {
		return errors.New("missing required field: ok")
	}
	var okVal bool
	if err := json.Unmarshal(okRaw, &okVal); err != nil {
		return fmt.Errorf("ok: must be bool: %w", err)
	}

	_, hasData := raw["data"]
	_, hasWarnings := raw["warnings"]
	_, hasError := raw["error"]

	// Invariant 1: ok == (error is absent).
	if okVal && hasError {
		return errors.New("ok=true but error present")
	}
	if !okVal && !hasError {
		return errors.New("ok=false but error absent")
	}

	// Type-check the optional fields if present (data must be an
	// object, warnings an array, error an object).
	if hasData {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(raw["data"], &probe); err != nil {
			return fmt.Errorf("data: must be object: %w", err)
		}
	}
	if hasWarnings {
		var probe []json.RawMessage
		if err := json.Unmarshal(raw["warnings"], &probe); err != nil {
			return fmt.Errorf("warnings: must be array: %w", err)
		}
	}
	if hasError {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(raw["error"], &probe); err != nil {
			return fmt.Errorf("error: must be object: %w", err)
		}
	}

	// Invariants 2-4 (status-specific).
	switch status {
	case "started":
		if !okVal {
			return errors.New("status=started requires ok=true")
		}
		if hasData {
			return errors.New("status=started must not carry data")
		}
		if hasWarnings {
			return errors.New("status=started must not carry warnings")
		}
		if hasError {
			return errors.New("status=started must not carry error")
		}
	case "completed":
		if okVal && !hasData {
			return errors.New("status=completed,ok=true requires data")
		}
		if !okVal && !hasError {
			return errors.New("status=completed,ok=false requires error")
		}
	case "partial":
		if !okVal {
			return errors.New("status=partial requires ok=true")
		}
		if !hasData {
			return errors.New("status=partial requires data")
		}
		if !hasWarnings {
			return errors.New("status=partial requires warnings")
		}
	}

	return nil
}

