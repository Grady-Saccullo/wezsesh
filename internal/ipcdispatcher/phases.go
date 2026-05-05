// phases.go isolates the §3.1 Phase-2 pointer-OSC payload assembly so
// tests can exercise the byte shape without driving a full Dispatch.
//
// The pointer's canonical-JSON shape is fixed by §3.1:
//
//	{"id":"<26-char ULID>","path":"<absolute>","v":1}
//
// Field order on the wire is unsigned UTF-8 byte order (id, path, v) —
// produced naturally by canonicaljson.Marshal's sorted-key encoding.
// The bytes are then base64-StdEncoded by the caller and handed to
// uservar.Writer.WriteOSC, which enforces the 256 B envelope ceiling.
//
// Why a separate file: the §17.3 conformance gate "Pointer JSON →
// base64 → OSC envelope ≤ 256 B" needs an end-to-end byte-shape test
// that is independent of the Dispatch path. Keeping encodePointer here
// (vs. inlined in Dispatch) gives the test suite a stable seam.
package ipcdispatcher

import (
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/Grady-Saccullo/wezsesh/internal/canonicaljson"
)

// pointerVersion is the §3.1 pointer schema version. The spec pins this
// to 1; the plugin's pointer field-shape validator (§9.3.1.A) rejects
// any other value.
const pointerVersion = 1

// ulidLen is the §3.3 ULID length in characters (Crockford base32, 26).
const ulidLen = 26

// ErrEmptyPath is returned by encodePointer when reqPath is empty —
// indicates a programming bug at the call site.
var ErrEmptyPath = errors.New("ipcdispatcher: encodePointer: empty path")

// ErrBadULID is returned by encodePointer when id is not exactly
// ulidLen chars. The Dispatcher's ULID generator always emits the
// right length; this guard catches test fakes that drift.
var ErrBadULID = errors.New("ipcdispatcher: encodePointer: bad ULID length")

// encodePointer canonical-encodes the §3.1 pointer JSON, base64-encodes
// the result, and returns the bytes ready to feed uservar.Writer.WriteOSC.
//
// The canonical encoder produces sorted-key output, so the on-the-wire
// byte order is `id, path, v` regardless of insert order in the input
// map. That is the order the §3.1 prose's "byte order on the wire"
// comment requires, and the Lua plugin's pointer field-shape validator
// re-canonicalises into the same order before any further parsing.
func encodePointer(id, reqPath string) ([]byte, error) {
	if len(id) != ulidLen {
		return nil, fmt.Errorf("%w: got %d", ErrBadULID, len(id))
	}
	if reqPath == "" {
		return nil, ErrEmptyPath
	}
	canonical, err := canonicaljson.Marshal(map[string]any{
		"v":    int64(pointerVersion),
		"id":   id,
		"path": reqPath,
	})
	if err != nil {
		return nil, fmt.Errorf("ipcdispatcher: encodePointer: %w", err)
	}
	enc := base64.StdEncoding
	out := make([]byte, enc.EncodedLen(len(canonical)))
	enc.Encode(out, canonical)
	return out, nil
}
