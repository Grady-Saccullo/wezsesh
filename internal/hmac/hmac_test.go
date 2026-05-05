// HMAC test suite — pins the §17.2 round-trip fixture, exercises the
// §4.3 field-removal sequence on both signer and verifier, and asserts
// constant-time compare is delegated to crypto/subtle.
//
// openssl cross-check (the canonical sans-hmac bytes go on stdin):
//
//	printf '%s' '{"args":{},"id":"01JABCDEFGHJKMNPQRSTVWXYZA","op":"noop","reply_sock":"/tmp/x.sock","target_window_id":1,"ts":1700000000,"v":1}' \
//	  | openssl dgst -sha256 -mac HMAC \
//	      -macopt hexkey:a0b1c2d3e4f5a0b1c2d3e4f5a0b1c2d3e4f5a0b1c2d3e4f5a0b1c2d3e4f5a0b1
//
// Expected output:
//
//	HMAC-SHA2-256(stdin)= 52d0003484acc868ce5762d065e2360f98b37b777009306b3cec8e7177dd14b5
//
// (older openssl: "HMAC-SHA256(stdin)= ..."). The hex digest is the
// expected_hmac in testdata/roundtrip.json and §17.2.
package hmac

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// roundtripFixture mirrors testdata/roundtrip.json. The on-disk fixture
// is the source of truth; we decode into this struct so the test fails
// loudly if a field disappears.
type roundtripFixture struct {
	KeyHex                  string         `json:"key_hex"`
	Payload                 map[string]any `json:"payload"`
	CanonicalSansHMAC       string         `json:"canonical_sans_hmac"`
	CanonicalSansHMACSHA256 string         `json:"canonical_sans_hmac_sha256"`
	ExpectedHMAC            string         `json:"expected_hmac"`
}

func loadFixture(t *testing.T) roundtripFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "roundtrip.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fx roundtripFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	// JSON numbers decode as float64; the §17.2 payload uses integers
	// for v / ts / target_window_id. Promote to int64 so the canonical
	// encoder doesn't reject them.
	for _, k := range []string{"v", "ts", "target_window_id"} {
		v, ok := fx.Payload[k]
		if !ok {
			t.Fatalf("fixture payload missing %q", k)
		}
		f, ok := v.(float64)
		if !ok {
			t.Fatalf("fixture payload %q is %T, want JSON number", k, v)
		}
		fx.Payload[k] = int64(f)
	}
	return fx
}

// TestRoundtripFixture is the §17.2 acceptance gate. The pinned
// expected_hmac MUST match what Sign produces over the canonical
// sans-hmac bytes. Verify must accept the freshly-signed payload AND
// the fixture's pinned digest. Sub-assertion: the SHA-256 of the
// canonical sans-hmac bytes localises a failure to canonical-JSON
// (encoder bug) vs HMAC (this package's bug).
func TestRoundtripFixture(t *testing.T) {
	fx := loadFixture(t)
	signer, err := NewSigner(fx.KeyHex)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	// Localising sub-assertion: canonical sans-hmac bytes match the
	// pinned literal AND the pinned SHA-256.
	got, err := canonicalSansHMAC(fx.Payload)
	if err != nil {
		t.Fatalf("canonicalSansHMAC: %v", err)
	}
	if string(got) != fx.CanonicalSansHMAC {
		t.Fatalf(
			"canonical sans-hmac bytes diverge:\n got: %s\nwant: %s",
			string(got), fx.CanonicalSansHMAC,
		)
	}
	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) != fx.CanonicalSansHMACSHA256 {
		t.Fatalf(
			"canonical sans-hmac sha256 diverges:\n got: %s\nwant: %s",
			hex.EncodeToString(sum[:]), fx.CanonicalSansHMACSHA256,
		)
	}

	// Sign produces the pinned hex digest.
	digest, err := signer.Sign(fx.Payload)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if digest != fx.ExpectedHMAC {
		t.Fatalf("Sign digest = %s, want %s", digest, fx.ExpectedHMAC)
	}

	// Verify accepts the round-trip: caller sets payload["hmac"] AFTER
	// Sign returns (per §4.3 step 5 and §8.3's "Sign returns digest"
	// contract — Sign itself does not mutate).
	signed := shallowCopy(fx.Payload)
	signed["hmac"] = digest
	ok, err := signer.Verify(signed)
	if err != nil {
		t.Fatalf("Verify (round-trip): err=%v", err)
	}
	if !ok {
		t.Fatalf("Verify (round-trip) returned false; want true")
	}

	// Sign MUST NOT have mutated the caller's payload.
	if _, ok := fx.Payload["hmac"]; ok {
		t.Fatalf("Sign mutated caller payload: hmac key was added")
	}
}

// TestSignDoesNotEmitHMACEmptyString is the §4.3 field-removal gate.
// The forbidden alternative — setting payload["hmac"] = "" before
// encoding — produces `"hmac":""` in the bytes at step 2 and a
// different digest. We assert (a) Sign's intermediate bytes never
// contain `"hmac":`, (b) signing a payload with hmac="" yields the
// same digest as signing without the hmac key, proving the field is
// removed (not zeroed).
func TestSignDoesNotEmitHMACEmptyString(t *testing.T) {
	fx := loadFixture(t)
	signer, err := NewSigner(fx.KeyHex)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	// (a) The bytes Sign feeds into HMAC contain no "hmac":"" / "hmac":
	// fragment of any kind.
	bytesSansHMAC, err := canonicalSansHMAC(fx.Payload)
	if err != nil {
		t.Fatalf("canonicalSansHMAC: %v", err)
	}
	if strings.Contains(string(bytesSansHMAC), `"hmac":`) {
		t.Fatalf(
			"sans-hmac bytes leak hmac key: %s",
			string(bytesSansHMAC),
		)
	}

	// (b) Signing with hmac="" present must match signing without it.
	withEmpty := shallowCopy(fx.Payload)
	withEmpty["hmac"] = ""
	digestWithEmpty, err := signer.Sign(withEmpty)
	if err != nil {
		t.Fatalf("Sign(with empty hmac): %v", err)
	}
	digestWithout, err := signer.Sign(fx.Payload)
	if err != nil {
		t.Fatalf("Sign(without hmac): %v", err)
	}
	if digestWithEmpty != digestWithout {
		t.Fatalf(
			"removal vs zero divergence: with-empty=%s without=%s",
			digestWithEmpty, digestWithout,
		)
	}
	if digestWithEmpty != fx.ExpectedHMAC {
		t.Fatalf(
			"with-empty digest = %s, want pinned %s",
			digestWithEmpty, fx.ExpectedHMAC,
		)
	}

	// Verify must also strip hmac (not zero) and accept either form.
	withEmpty["hmac"] = fx.ExpectedHMAC
	ok, err := signer.Verify(withEmpty)
	if err != nil {
		t.Fatalf("Verify(with empty-then-set hmac): err=%v", err)
	}
	if !ok {
		t.Fatalf("Verify(with empty-then-set hmac) = false; want true")
	}
}

// TestVerifyRejectsTampered exercises the negative path: a single-byte
// edit to any signed field MUST flip Verify to false.
func TestVerifyRejectsTampered(t *testing.T) {
	fx := loadFixture(t)
	signer, err := NewSigner(fx.KeyHex)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	digest, err := signer.Sign(fx.Payload)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	signed := shallowCopy(fx.Payload)
	signed["hmac"] = digest
	signed["op"] = "switch" // verb tamper
	ok, err := signer.Verify(signed)
	if err != nil {
		t.Fatalf("Verify(tampered): err=%v", err)
	}
	if ok {
		t.Fatalf("Verify(tampered) = true; want false")
	}
}

// TestVerifyRejectsBadHMACShape covers the shape-validation negatives:
// missing hmac, non-string hmac, non-hex hmac, wrong-length hex hmac.
// All return (false, nil) — never panic, never (true, ...).
func TestVerifyRejectsBadHMACShape(t *testing.T) {
	fx := loadFixture(t)
	signer, err := NewSigner(fx.KeyHex)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	cases := []struct {
		name  string
		mutate func(p map[string]any)
	}{
		{"missing hmac", func(p map[string]any) {}},
		{"non-string hmac", func(p map[string]any) { p["hmac"] = 42 }},
		{"non-hex hmac", func(p map[string]any) { p["hmac"] = "zz" + strings.Repeat("0", 62) }},
		{"short hex hmac", func(p map[string]any) { p["hmac"] = "deadbeef" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := shallowCopy(fx.Payload)
			tc.mutate(p)
			ok, err := signer.Verify(p)
			if err != nil {
				t.Fatalf("Verify: err=%v", err)
			}
			if ok {
				t.Fatalf("Verify = true; want false")
			}
		})
	}
}

// TestVerifyDoesNotMutate covers §4.3 step 7 wording "REMOVE hmac key
// (do not zero)" applied to the caller-side guarantee: Verify must not
// remove the key from the caller's payload either.
func TestVerifyDoesNotMutate(t *testing.T) {
	fx := loadFixture(t)
	signer, err := NewSigner(fx.KeyHex)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	signed := shallowCopy(fx.Payload)
	signed["hmac"] = fx.ExpectedHMAC
	ok, err := signer.Verify(signed)
	if err != nil || !ok {
		t.Fatalf("Verify(round-trip): ok=%v err=%v", ok, err)
	}
	if v, present := signed["hmac"]; !present || v != fx.ExpectedHMAC {
		t.Fatalf(
			"Verify mutated caller payload: hmac=%v present=%v",
			v, present,
		)
	}
}

// TestNewSignerKeyShape pins ErrBadKey emission for non-conforming
// keys — must be 64 lowercase hex chars (§5.1, §8.3).
func TestNewSignerKeyShape(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"too short", strings.Repeat("a", 63)},
		{"too long", strings.Repeat("a", 65)},
		{"uppercase rejected", strings.ToUpper(strings.Repeat("a", 64))},
		{"mixed case rejected", "A" + strings.Repeat("0", 63)},
		{"non-hex char", strings.Repeat("g", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSigner(tc.key)
			if !errors.Is(err, ErrBadKey) {
				t.Fatalf("NewSigner(%q) err = %v; want ErrBadKey", tc.key, err)
			}
		})
	}

	// Happy path: 64 lowercase hex chars succeeds.
	if _, err := NewSigner(strings.Repeat("a", 64)); err != nil {
		t.Fatalf("NewSigner(valid key): %v", err)
	}
}

// TestConstantTimeCompareIsUsed asserts the package source delegates
// HMAC equality to crypto/subtle.ConstantTimeCompare. This is a
// belt-and-braces gate: if a future refactor swaps in `==` or
// bytes.Equal we want the test to fail before the leak ships.
func TestConstantTimeCompareIsUsed(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "hmac.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse hmac.go: %v", err)
	}

	// Confirm the import is present.
	importsSubtle := false
	for _, imp := range file.Imports {
		if imp.Path != nil && imp.Path.Value == `"crypto/subtle"` {
			importsSubtle = true
			break
		}
	}
	if !importsSubtle {
		t.Fatalf("hmac.go does not import crypto/subtle")
	}

	// Confirm a call to subtle.ConstantTimeCompare exists in source.
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if ident.Name == "subtle" && sel.Sel.Name == "ConstantTimeCompare" {
			found = true
			return false
		}
		return true
	})
	if !found {
		t.Fatalf("hmac.go does not call subtle.ConstantTimeCompare")
	}

	// And confirm the runtime semantics still match: equal bytes → 1.
	a := []byte{1, 2, 3, 4}
	b := []byte{1, 2, 3, 4}
	if subtle.ConstantTimeCompare(a, b) != 1 {
		t.Fatal("crypto/subtle.ConstantTimeCompare semantics drift")
	}
}

func shallowCopy(m map[string]any) map[string]any {
	cp := make(map[string]any, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}
