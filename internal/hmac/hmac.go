// Package hmac implements the §8.3 / §4.3 HMAC-SHA-256 signer and
// verifier for the IPC wire protocol. The Go and Lua sides MUST produce
// byte-identical canonical-JSON for the sans-hmac payload (§4.3 step 2)
// and therefore byte-identical HMAC digests.
//
// Key invariants (do not weaken):
//   - The bytes fed into HMAC NEVER contain "hmac":""; the field is
//     removed from a copy of the payload before canonical encoding.
//   - The hex key is decoded to 32 raw bytes BEFORE feeding hmac.New.
//   - Verify uses crypto/subtle.ConstantTimeCompare; never ==.
//   - Sign returns the digest as lowercase hex; the caller (or a higher
//     layer) sets payload["hmac"] = digest after Sign returns. Per §8.3
//     Sign's contract is "returns lowercase hex digest" — no mutation.
package hmac

import (
	stdhmac "crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"

	canonicaljson "github.com/Grady-Saccullo/wezsesh/internal/canonicaljson"
)

// ErrBadKey is returned by NewSigner when the key is not 64 lowercase
// hex chars.
var ErrBadKey = errors.New("hmac: key must be 64 lowercase hex chars")

// Signer caches the decoded 32-byte raw key. Construct once per request;
// reusable across Sign/Verify calls.
type Signer struct {
	key []byte
}

// NewSigner hex-decodes key (must be 64 lowercase hex chars) into 32
// raw bytes. Uppercase hex is rejected — both sides of the protocol
// agree on lowercase per §5.1 and the §17.2 fixture.
func NewSigner(hexKey string) (*Signer, error) {
	if len(hexKey) != 64 {
		return nil, ErrBadKey
	}
	for i := 0; i < len(hexKey); i++ {
		c := hexKey[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return nil, ErrBadKey
		}
	}
	raw, err := hex.DecodeString(hexKey)
	if err != nil {
		// Unreachable given the lowercase-hex character check above,
		// but keep the guard rather than panic-on-decode.
		return nil, ErrBadKey
	}
	return &Signer{key: raw}, nil
}

// Sign canonical-encodes payload (sans "hmac" key, per §4.3 steps 1–3),
// computes HMAC-SHA-256, and returns the lowercase hex digest. Sign
// does NOT mutate payload; the caller writes payload["hmac"] = digest
// after Sign returns (§4.3 step 5).
//
// The sequence is exactly:
//
//  1. shallow copy payload, remove "hmac" key
//  2. canonicaljson.Marshal(copy)
//  3. HMAC-SHA-256(bytes, rawKey)
//  4. hex.EncodeToString(digest) — lowercase
//
// The forbidden alternative — set payload["hmac"] = "" then encode —
// would emit `"hmac":""` into the bytes at step 2. Both signer and
// verifier MUST agree to operate on the sans-hmac form.
func (s *Signer) Sign(payload map[string]any) (string, error) {
	bytesSansHMAC, err := canonicalSansHMAC(payload)
	if err != nil {
		return "", err
	}
	mac := stdhmac.New(sha256.New, s.key)
	mac.Write(bytesSansHMAC)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// Verify recomputes the digest over a sans-hmac copy of payload and
// constant-time compares it to payload["hmac"]. Returns false on any
// shape error (missing/non-string hmac field, encode failure, hex
// decode failure of the supplied digest, length mismatch). Verify does
// NOT mutate the caller's payload.
//
// §4.3 step 7: REMOVE the hmac key (do not zero) before the recompute.
func (s *Signer) Verify(payload map[string]any) (bool, error) {
	supplied, ok := payload["hmac"].(string)
	if !ok {
		return false, nil
	}
	suppliedRaw, err := hex.DecodeString(supplied)
	if err != nil {
		return false, nil
	}
	bytesSansHMAC, err := canonicalSansHMAC(payload)
	if err != nil {
		return false, err
	}
	mac := stdhmac.New(sha256.New, s.key)
	mac.Write(bytesSansHMAC)
	expected := mac.Sum(nil)
	if len(suppliedRaw) != len(expected) {
		// ConstantTimeCompare returns 0 on length mismatch but does so
		// after a length comparison — early-return is fine here.
		return false, nil
	}
	return subtle.ConstantTimeCompare(suppliedRaw, expected) == 1, nil
}

// canonicalSansHMAC returns canonicaljson.Marshal(payload) with the
// "hmac" key removed via a shallow copy. The shallow copy preserves
// nested maps/slices by reference — Marshal does not mutate them.
func canonicalSansHMAC(payload map[string]any) ([]byte, error) {
	cp := make(map[string]any, len(payload))
	for k, v := range payload {
		if k == "hmac" {
			continue
		}
		cp[k] = v
	}
	return canonicaljson.Marshal(cp)
}
