package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

// keygenRand is the entropy source for `wezsesh keygen`. Production
// keeps it pointed at crypto/rand.Reader; tests swap it for a
// deterministic bytes.Reader (success path) or an always-erroring
// reader (failure path) so the §17.3 "wezsesh keygen output" gate is
// exercised without invoking real entropy.
var keygenRand io.Reader = rand.Reader

// keygenPanicLog is the §13.14 LevelError seam invoked from the
// top-level recover in subcmdKeygen. Production main() does NOT wire a
// logger here — the keygen path runs before the §8.20.1 step-4 logger
// is constructed, so the §13.14 logged-panic requirement degrades to
// best-effort: when keygenPanicLog is nil the stderr line + rc=3 still
// satisfies the §5.2 fallback contract on its own. Tests stub this to
// observe the seam fired with the panic value.
var keygenPanicLog func(r any)

// subcmdKeygen implements §5.2 / §8.20: read 32 bytes from
// crypto/rand, hex-encode (64 lowercase chars), and emit
// `<hex>\n` (exactly 65 bytes) on stdout. Exit 0 on success;
// exit 3 (exitKeygen) on entropy / write failure or panic — the Lua
// `ensure_session_key` chain (§5.2 step 2) treats exit 3 as the
// signal to fall through to the /dev/urandom fallback, so the
// failure path must NOT corrupt stdout.
//
// §13.14: a thin top-level recover logs the panic at LevelError (best
// effort via keygenPanicLog), prints `wezsesh keygen: panic: <err>` on
// stderr, and exits with status 3 so the §5.2 fallback chain engages.
func subcmdKeygen(rest []string, stdout, stderr io.Writer) (rc int) {
	defer func() {
		if r := recover(); r != nil {
			if keygenPanicLog != nil {
				keygenPanicLog(r)
			}
			fmt.Fprintf(stderr, "wezsesh keygen: panic: %v\n", r)
			rc = exitKeygen
		}
	}()

	// §8.20: `wezsesh keygen` accepts no flags or args. A clean stdout
	// + rc=3 is the documented "fall through to /dev/urandom" signal,
	// so we MUST NOT touch stdout when rejecting trailing args.
	if len(rest) > 0 {
		fmt.Fprintf(stderr, "wezsesh keygen: unexpected arguments: %s\n", strings.Join(rest, " "))
		return exitKeygen
	}

	var buf [32]byte
	if _, err := io.ReadFull(keygenRand, buf[:]); err != nil {
		fmt.Fprintf(stderr, "wezsesh keygen: %v\n", err)
		return exitKeygen
	}
	out := make([]byte, hex.EncodedLen(len(buf))+1)
	hex.Encode(out, buf[:])
	out[len(out)-1] = '\n'
	if _, err := stdout.Write(out); err != nil {
		fmt.Fprintf(stderr, "wezsesh keygen: %v\n", err)
		return exitKeygen
	}
	return exitOK
}
