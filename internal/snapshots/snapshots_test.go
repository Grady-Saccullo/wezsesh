package snapshots

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestRepo creates an isolated workspace dir for a test and returns
// the bound Repo plus the workspaceDir path for fixtures to write to.
func newTestRepo(t *testing.T) (*Repo, string) {
	t.Helper()
	dir := t.TempDir()
	r, err := NewRepo(dir, nil)
	if err != nil {
		t.Fatalf("NewRepo: %v", err)
	}
	return r, r.workspaceDir
}

// writeSnapshot writes <encoded(name)>.json with the given bytes and
// returns the absolute path.
func writeSnapshot(t *testing.T, wsDir, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(wsDir, EncodeName(name)+".json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// writeSidecar writes <encoded(name)>.wezsesh.json with the given JSON
// blob and returns the absolute path.
func writeSidecar(t *testing.T, wsDir, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(wsDir, EncodeName(name)+sidecarSuffix)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write sidecar %s: %v", path, err)
	}
	return path
}

func TestEncodeDecodeName(t *testing.T) {
	cases := []struct {
		raw, encoded string
	}{
		{"plain", "plain"},
		{"foo/bar", "foo+bar"},
		{"a/b/c", "a+b+c"},
		{"", ""},
	}
	for _, c := range cases {
		if got := EncodeName(c.raw); got != c.encoded {
			t.Errorf("EncodeName(%q) = %q, want %q", c.raw, got, c.encoded)
		}
		if got := DecodeName(c.encoded); got != strings.ReplaceAll(c.encoded, "+", "/") {
			t.Errorf("DecodeName(%q) = %q, want %q", c.encoded, got, strings.ReplaceAll(c.encoded, "+", "/"))
		}
	}
}

func TestNewRepo_BindOnly(t *testing.T) {
	dir := t.TempDir()
	// Write a snapshot before NewRepo. NewRepo MUST NOT scan, so the
	// hashCache should be empty after construction.
	wsDir := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(wsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, "ws.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := NewRepo(dir, nil)
	if err != nil {
		t.Fatalf("NewRepo: %v", err)
	}
	if len(r.hashCache) != 0 {
		t.Errorf("NewRepo populated hashCache (%d entries) — must be bind-only", len(r.hashCache))
	}
}

// Encryption magic-byte sniff per Appendix B.
func TestSniffBytes(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want Encryption
	}{
		{"plaintext-brace", []byte(`{"workspace": "x"}`), EncryptionPlaintext},
		{"plaintext-bracket", []byte(`[1,2,3]`), EncryptionPlaintext},
		{"plaintext-leading-space", []byte("  {}"), EncryptionPlaintext},
		{"plaintext-leading-newline", []byte("\n{}"), EncryptionPlaintext},
		{"age-magic", []byte("age-encryption.org/v1\nrest..."), EncryptionAge},
		{"openpgp-high-bit", []byte{0x85, 0x02, 0x0d, 0x03}, EncryptionOpenPGP},
		{"openpgp-new-format", []byte{0xc1, 0x09}, EncryptionOpenPGP},
		{"unknown-letter", []byte("plaintext"), EncryptionUnknown},
		{"empty", []byte{}, EncryptionUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SniffBytes(c.in); got != c.want {
				t.Errorf("SniffBytes(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestSniff_OnDisk(t *testing.T) {
	r, wsDir := newTestRepo(t)
	writeSnapshot(t, wsDir, "ws", []byte(`{"workspace": "x"}`))
	got, err := r.Sniff(context.Background(), "ws")
	if err != nil {
		t.Fatalf("Sniff: %v", err)
	}
	if got != EncryptionPlaintext {
		t.Errorf("Sniff = %v, want plaintext", got)
	}
}

// Acceptance gate: 10 MiB cap.
func TestList_SizeCap(t *testing.T) {
	r, wsDir := newTestRepo(t)
	body := []byte("{" + strings.Repeat(" ", MaxFileSize+1) + "}")
	writeSnapshot(t, wsDir, "huge", body)

	entries, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].ParseError == nil {
		t.Errorf("expected ParseError on oversize file; got nil")
	}
	if entries[0].State != nil {
		t.Errorf("oversize file must not be parsed (State=nil), got %+v", entries[0].State)
	}
}

// Acceptance gate: depth 100.
func TestList_DepthCap(t *testing.T) {
	r, wsDir := newTestRepo(t)
	// Build a JSON value with 200 levels of object nesting, exceeding
	// MaxJSONDepth=100.
	depth := 200
	body := strings.Repeat(`{"a":`, depth) + "true" + strings.Repeat(`}`, depth)
	writeSnapshot(t, wsDir, "deep", []byte(body))

	entries, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].ParseError == nil {
		t.Errorf("expected ParseError on deep nesting; got nil")
	}
	if !strings.Contains(entries[0].ParseError.Error(), "depth") {
		t.Errorf("ParseError should mention depth: %v", entries[0].ParseError)
	}
}

// Parse tolerance: bad JSON in one file does not abort List; entry is
// surfaced with ParseError.
func TestList_ParseTolerant(t *testing.T) {
	r, wsDir := newTestRepo(t)
	writeSnapshot(t, wsDir, "good", []byte(`{"workspace": "g"}`))
	writeSnapshot(t, wsDir, "torn", []byte(`{"workspace": "t"`)) // truncated

	entries, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error (must not abort): %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	var good, torn *Entry
	for i := range entries {
		switch entries[i].Name {
		case "good":
			good = &entries[i]
		case "torn":
			torn = &entries[i]
		}
	}
	if good == nil || torn == nil {
		t.Fatalf("missing entry: good=%v torn=%v", good, torn)
	}
	if good.ParseError != nil {
		t.Errorf("good entry has ParseError: %v", good.ParseError)
	}
	if good.State == nil || good.State.Workspace == nil || *good.State.Workspace != "g" {
		t.Errorf("good entry State malformed: %+v", good.State)
	}
	if torn.ParseError == nil {
		t.Errorf("torn entry must have ParseError")
	}
}

// Acceptance gate: Hash returns "sha256:<hex>"; RawHashHex returns hex.
func TestHashAndRawHashHex(t *testing.T) {
	r, wsDir := newTestRepo(t)
	body := []byte(`{"workspace": "x"}`)
	writeSnapshot(t, wsDir, "ws", body)

	want := sha256.Sum256(body)
	wantHex := hex.EncodeToString(want[:])

	got, err := r.Hash(context.Background(), "ws")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if got != "sha256:"+wantHex {
		t.Errorf("Hash = %q, want %q", got, "sha256:"+wantHex)
	}

	rawGot, err := r.RawHashHex(context.Background(), "ws")
	if err != nil {
		t.Fatalf("RawHashHex: %v", err)
	}
	if rawGot != wantHex {
		t.Errorf("RawHashHex = %q, want %q", rawGot, wantHex)
	}

	// Cache hit: second call must return the same prefixed digest.
	got2, err := r.Hash(context.Background(), "ws")
	if err != nil {
		t.Fatalf("Hash#2: %v", err)
	}
	if got2 != got {
		t.Errorf("Hash#2 = %q, want %q", got2, got)
	}
}

// Hashes are LAZY: List does NOT precompute. The hashCache is empty
// until HashLazy / Hash / RawHashHex is invoked.
func TestList_HashesLazy(t *testing.T) {
	r, wsDir := newTestRepo(t)
	writeSnapshot(t, wsDir, "ws1", []byte(`{}`))
	writeSnapshot(t, wsDir, "ws2", []byte(`{}`))

	entries, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d, want 2", len(entries))
	}
	if len(r.hashCache) != 0 {
		t.Errorf("hashCache populated by List (%d) — must be lazy", len(r.hashCache))
	}
	for _, e := range entries {
		if e.HashLazy == nil {
			t.Errorf("entry %q missing HashLazy closure", e.Name)
		}
	}
	// Force computation; cache should fill.
	if _, err := entries[0].HashLazy(context.Background()); err != nil {
		t.Fatalf("HashLazy: %v", err)
	}
	if len(r.hashCache) != 1 {
		t.Errorf("hashCache after one HashLazy = %d, want 1", len(r.hashCache))
	}
}

// Acceptance gate: schema migration sidecar v=2 → backed up,
// ReadSidecar returns ok=false.
func TestReadSidecar_FutureVersion(t *testing.T) {
	r, wsDir := newTestRepo(t)
	body := []byte(`{"version": 2, "tags": ["x"], "pinned": true}`)
	writeSidecar(t, wsDir, "ws", body)

	s, ok, err := r.ReadSidecar(context.Background(), "ws")
	if err != nil {
		t.Fatalf("ReadSidecar: %v", err)
	}
	if ok {
		t.Errorf("ok must be false for v>1; got Sidecar=%+v", s)
	}
	// The original file should now be at <encoded>.wezsesh.json.v2.bak.
	bak := filepath.Join(wsDir, EncodeName("ws")+sidecarSuffix+".v2.bak")
	if _, err := os.Stat(bak); err != nil {
		t.Errorf("expected backup at %s: %v", bak, err)
	}
	// Original path should no longer exist.
	orig := filepath.Join(wsDir, EncodeName("ws")+sidecarSuffix)
	if _, err := os.Stat(orig); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("original sidecar still exists at %s", orig)
	}
}

// ReadSidecar v=0 (missing/unversioned) returns ok=false, nil err.
func TestReadSidecar_MissingFile(t *testing.T) {
	r, _ := newTestRepo(t)
	s, ok, err := r.ReadSidecar(context.Background(), "missing")
	if err != nil {
		t.Errorf("ReadSidecar missing: %v", err)
	}
	if ok {
		t.Errorf("ok = true for missing sidecar; got %+v", s)
	}
}

// ReadSidecar v=1 round-trip via WriteSidecar.
func TestWriteReadSidecar_RoundTrip(t *testing.T) {
	r, _ := newTestRepo(t)
	notes := "hello"
	want := Sidecar{
		Tags:   []string{"a", "b"},
		Pinned: true,
		Notes:  &notes,
	}
	if err := r.WriteSidecar(context.Background(), "ws", want); err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}
	got, ok, err := r.ReadSidecar(context.Background(), "ws")
	if err != nil {
		t.Fatalf("ReadSidecar: %v", err)
	}
	if !ok {
		t.Fatalf("ReadSidecar ok=false")
	}
	if got.Version != SidecarSchemaVersion {
		t.Errorf("Version = %d, want %d", got.Version, SidecarSchemaVersion)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "a" || got.Tags[1] != "b" {
		t.Errorf("Tags = %v, want [a b]", got.Tags)
	}
	if !got.Pinned {
		t.Errorf("Pinned = false, want true")
	}
	if got.Notes == nil || *got.Notes != "hello" {
		t.Errorf("Notes = %v, want hello", got.Notes)
	}
}

// Acceptance gate: Resurrect race — a torn write recovers via the 3×
// retry loop in Repo.List (not just the helper directly).
//
// Determinism: a test-only seam (testParseRetryHook) fires immediately
// before each parse attempt. Attempt 0 leaves the on-disk file in its
// torn shape; attempt 1 atomically renames the valid replacement over
// it. List must surface the entry with ParseError == nil and a
// fully-parsed State — proving Repo.List integrates the retry path,
// not just readAndParseWithRetry in isolation.
func TestList_ResurrectRaceRetry(t *testing.T) {
	r, wsDir := newTestRepo(t)
	encoded := EncodeName("ws") + ".json"
	path := filepath.Join(wsDir, encoded)
	staged := filepath.Join(wsDir, encoded+".staged")

	tornBody := []byte(`{"workspace": "x"`)        // missing closing brace
	finalBody := []byte(`{"workspace": "x"}`)      // valid
	if err := os.WriteFile(path, tornBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, finalBody, 0o600); err != nil {
		t.Fatal(err)
	}

	var attempts atomic.Int32
	swappedAt := int32(-1)
	prev := testParseRetryHook
	t.Cleanup(func() { testParseRetryHook = prev })
	testParseRetryHook = func(attempt int, p string) {
		if p != path {
			return
		}
		n := attempts.Add(1)
		// Swap on attempt 1 (zero-based) — this is BEFORE the second
		// readAndParseOnce reads the file, so attempt 0 sees torn,
		// attempt 1 sees valid. os.Rename is atomic on POSIX so the
		// reader cannot observe a half-state.
		if n == 2 && atomic.CompareAndSwapInt32(&swappedAt, -1, int32(attempt)) {
			if err := os.Rename(staged, path); err != nil {
				t.Errorf("rename staged → path: %v", err)
			}
		}
	}

	entries, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].ParseError != nil {
		t.Fatalf("retry must recover; got ParseError = %v (attempts=%d, swappedAt=%d)",
			entries[0].ParseError, attempts.Load(), atomic.LoadInt32(&swappedAt))
	}
	if entries[0].State == nil || entries[0].State.Workspace == nil || *entries[0].State.Workspace != "x" {
		t.Errorf("State malformed despite no ParseError: %+v", entries[0].State)
	}
	if got := attempts.Load(); got < 2 {
		t.Errorf("expected ≥2 parse attempts; got %d", got)
	}
}

// Same gate, deterministically: a parse-failed first attempt followed
// by a valid file at attempt 2 should succeed via the retry loop. We
// drive readAndParseWithRetry directly so the test is timing-stable.
func TestReadAndParseWithRetry_RecoversAfterTear(t *testing.T) {
	r, wsDir := newTestRepo(t)
	path := writeSnapshot(t, wsDir, "ws", []byte(`{"workspace": "x"`)) // torn

	// Rewrite as soon as one parseRetryBackoff window passes.
	go func() {
		time.Sleep(parseRetryBackoff + 5*time.Millisecond)
		_ = os.WriteFile(path, []byte(`{"workspace": "x"}`), 0o600)
	}()

	enc, state, err := r.readAndParseWithRetry(context.Background(), path)
	if err != nil {
		t.Fatalf("retry path returned err = %v", err)
	}
	if enc != EncryptionPlaintext {
		t.Errorf("enc = %v, want plaintext", enc)
	}
	if state == nil || state.Workspace == nil || *state.Workspace != "x" {
		t.Errorf("state malformed: %+v", state)
	}
}

// Encrypted snapshots: List surfaces them with State=nil and the right
// Encryption classification — preview can degrade gracefully.
func TestList_EncryptedSnapshotsDegrade(t *testing.T) {
	r, wsDir := newTestRepo(t)
	writeSnapshot(t, wsDir, "age-ws", []byte("age-encryption.org/v1\nbody..."))
	writeSnapshot(t, wsDir, "gpg-ws", []byte{0x85, 0x02, 0x0d, 0x03, 0x00})

	entries, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d, want 2", len(entries))
	}
	for _, e := range entries {
		if e.State != nil {
			t.Errorf("encrypted entry %q must have State=nil", e.Name)
		}
		if e.ParseError != nil {
			t.Errorf("encrypted entry %q must NOT have ParseError, got %v", e.Name, e.ParseError)
		}
		switch e.Name {
		case "age-ws":
			if e.Encryption != EncryptionAge {
				t.Errorf("age-ws encryption = %v, want age", e.Encryption)
			}
		case "gpg-ws":
			if e.Encryption != EncryptionOpenPGP {
				t.Errorf("gpg-ws encryption = %v, want openpgp", e.Encryption)
			}
		}
	}
}

// LocalProcInfo accepts both the legacy bare-string shape and the
// current object shape (PRD §6.6 parser policy).
func TestLocalProcInfo_Unmarshal_BothShapes(t *testing.T) {
	t.Run("legacy string", func(t *testing.T) {
		var p LocalProcInfo
		if err := json.Unmarshal([]byte(`"/bin/bash"`), &p); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if p.LegacyString == nil || *p.LegacyString != "/bin/bash" {
			t.Errorf("LegacyString = %v, want /bin/bash", p.LegacyString)
		}
		if len(p.Argv) != 0 {
			t.Errorf("Argv populated for legacy: %v", p.Argv)
		}
	})
	t.Run("object", func(t *testing.T) {
		var p LocalProcInfo
		body := `{"name":"bash","argv":["/bin/bash","-l"],"cwd":"/tmp","executable":"/bin/bash"}`
		if err := json.Unmarshal([]byte(body), &p); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if p.Name == nil || *p.Name != "bash" {
			t.Errorf("Name = %v, want bash", p.Name)
		}
		if len(p.Argv) != 2 || p.Argv[1] != "-l" {
			t.Errorf("Argv = %v, want [/bin/bash -l]", p.Argv)
		}
		if p.LegacyString != nil {
			t.Errorf("LegacyString set for object shape: %v", p.LegacyString)
		}
	})
	t.Run("null", func(t *testing.T) {
		var p LocalProcInfo
		if err := json.Unmarshal([]byte(`null`), &p); err != nil {
			t.Fatalf("Unmarshal null: %v", err)
		}
	})
}

// Has, Delete, Rename basic happy paths.
func TestHasDeleteRename(t *testing.T) {
	r, wsDir := newTestRepo(t)
	body := []byte(`{"workspace": "x"}`)
	writeSnapshot(t, wsDir, "ws", body)
	writeSidecar(t, wsDir, "ws", []byte(`{"version":1,"pinned":true}`))

	// Has
	got, err := r.Has(context.Background(), "ws")
	if err != nil {
		t.Fatalf("Has: %v", err)
	}
	if !got {
		t.Errorf("Has=false for existing ws")
	}

	// Rename
	if err := r.Rename(context.Background(), "ws", "ws-2"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if has, _ := r.Has(context.Background(), "ws"); has {
		t.Errorf("Has(ws) still true after rename")
	}
	if has, _ := r.Has(context.Background(), "ws-2"); !has {
		t.Errorf("Has(ws-2) false after rename")
	}
	// Sidecar followed.
	_, ok, _ := r.ReadSidecar(context.Background(), "ws-2")
	if !ok {
		t.Errorf("sidecar did not follow rename")
	}

	// Delete
	if err := r.Delete(context.Background(), "ws-2"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if has, _ := r.Has(context.Background(), "ws-2"); has {
		t.Errorf("Has(ws-2) still true after delete")
	}
	if _, err := os.Stat(filepath.Join(wsDir, EncodeName("ws-2")+sidecarSuffix)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("sidecar still exists after delete")
	}
}

// ReadAll returns raw bytes; respects size cap.
func TestReadAll(t *testing.T) {
	r, wsDir := newTestRepo(t)
	body := []byte(`{"workspace": "x"}`)
	writeSnapshot(t, wsDir, "ws", body)

	got, err := r.ReadAll(context.Background(), "ws")
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("ReadAll = %q, want %q", got, body)
	}
}

// Symlinked snapshot files are skipped (not followed) by List.
func TestList_SkipsSymlinks(t *testing.T) {
	r, wsDir := newTestRepo(t)
	// Real file outside the workspace dir.
	outside := filepath.Join(t.TempDir(), "x.json")
	if err := os.WriteFile(outside, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(wsDir, "linked.json")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported on this fs: %v", err)
	}
	// And a real sibling.
	writeSnapshot(t, wsDir, "real", []byte(`{}`))

	entries, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, e := range entries {
		if e.Name == "linked" {
			t.Errorf("symlinked entry surfaced: %+v", e)
		}
	}
	// Real one should still be there.
	found := false
	for _, e := range entries {
		if e.Name == "real" {
			found = true
		}
	}
	if !found {
		t.Errorf("real entry missing")
	}
}

// Sidecar JSON corruption is tolerated: ok=false, no panic.
func TestReadSidecar_MalformedTolerated(t *testing.T) {
	r, wsDir := newTestRepo(t)
	writeSidecar(t, wsDir, "ws", []byte(`not-json`))
	_, ok, err := r.ReadSidecar(context.Background(), "ws")
	if err != nil {
		t.Errorf("ReadSidecar must not error on malformed: %v", err)
	}
	if ok {
		t.Errorf("ok=true for malformed sidecar")
	}
}

func TestEncryption_String(t *testing.T) {
	cases := map[Encryption]string{
		EncryptionPlaintext: "plaintext",
		EncryptionAge:       "age",
		EncryptionOpenPGP:   "openpgp",
		EncryptionUnknown:   "unknown",
	}
	for e, want := range cases {
		if got := e.String(); got != want {
			t.Errorf("Encryption(%d).String() = %q, want %q", int(e), got, want)
		}
	}
}

// SidecarPath uses encoded names — '/' → '+'.
func TestSidecarPath_UsesEncoding(t *testing.T) {
	r, _ := newTestRepo(t)
	got := r.SidecarPath("foo/bar")
	want := filepath.Join(r.workspaceDir, "foo+bar.wezsesh.json")
	if got != want {
		t.Errorf("SidecarPath = %q, want %q", got, want)
	}
}

// depthCountReader internal sanity: nested string with brackets does
// not falsely trigger.
func TestDepthCountReader_StringBrackets(t *testing.T) {
	body := fmt.Sprintf(`{"k": "%s"}`, strings.Repeat("{[", 200))
	dr := &depthCountReader{src: strings.NewReader(body), cap: 10}
	buf := make([]byte, 1024)
	for {
		_, err := dr.Read(buf)
		if err != nil {
			break
		}
	}
	if dr.failed {
		t.Errorf("depthCountReader incorrectly tripped on bracket-in-string")
	}
}

// HIGH fix regression: a symlink at <wsDir>/<name>.json must surface as
// Has = false (SkipWarn semantics from §8.1.1). The previous os.Lstat
// path also returned false but bypassed the centralised Enforce surface;
// this test pins the new behaviour to the safefs.Enforce contract.
func TestHas_SkipsSymlink(t *testing.T) {
	r, wsDir := newTestRepo(t)
	outside := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(outside, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(wsDir, EncodeName("symlinked")+".json")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported on this fs: %v", err)
	}
	got, err := r.Has(context.Background(), "symlinked")
	if err != nil {
		t.Fatalf("Has: %v", err)
	}
	if got {
		t.Errorf("Has(symlinked) = true; want false (symlink → SkipWarn → absent)")
	}
}

// NFC normalisation at the by-name boundary: NFD form of "é" (U+0065
// + U+0301) and the NFC form (U+00E9) must resolve to the same on-disk
// file. Covers Has, ReadAll, Hash, RawHashHex, Sniff, ReadSidecar,
// WriteSidecar, SidecarPath, Delete, Rename. §15.1 mandates NFC at
// storage/comparison boundaries.
func TestByName_NFC_NormalisesAtBoundary(t *testing.T) {
	const (
		nfc = "café"        // U+0063 U+0061 U+0066 U+00E9
		nfd = "café"       // U+0063 U+0061 U+0066 U+0065 U+0301
	)
	if nfc == nfd {
		t.Fatal("test fixture broken: nfc == nfd")
	}

	t.Run("Has + Sniff + Hash + RawHashHex + ReadAll", func(t *testing.T) {
		r, wsDir := newTestRepo(t)
		body := []byte(`{"workspace": "x"}`)
		// Write under NFC name; lookup via NFD must succeed.
		writeSnapshot(t, wsDir, nfc, body)

		ok, err := r.Has(context.Background(), nfd)
		if err != nil {
			t.Fatalf("Has(nfd): %v", err)
		}
		if !ok {
			t.Errorf("Has(nfd) = false; want true (NFC-normalise)")
		}

		got, err := r.ReadAll(context.Background(), nfd)
		if err != nil {
			t.Fatalf("ReadAll(nfd): %v", err)
		}
		if string(got) != string(body) {
			t.Errorf("ReadAll(nfd) bytes mismatch")
		}

		enc, err := r.Sniff(context.Background(), nfd)
		if err != nil {
			t.Fatalf("Sniff(nfd): %v", err)
		}
		if enc != EncryptionPlaintext {
			t.Errorf("Sniff(nfd) = %v, want plaintext", enc)
		}

		hNFC, err := r.Hash(context.Background(), nfc)
		if err != nil {
			t.Fatalf("Hash(nfc): %v", err)
		}
		hNFD, err := r.Hash(context.Background(), nfd)
		if err != nil {
			t.Fatalf("Hash(nfd): %v", err)
		}
		if hNFC != hNFD {
			t.Errorf("Hash(nfc) %q != Hash(nfd) %q", hNFC, hNFD)
		}

		rNFC, err := r.RawHashHex(context.Background(), nfc)
		if err != nil {
			t.Fatalf("RawHashHex(nfc): %v", err)
		}
		rNFD, err := r.RawHashHex(context.Background(), nfd)
		if err != nil {
			t.Fatalf("RawHashHex(nfd): %v", err)
		}
		if rNFC != rNFD {
			t.Errorf("RawHashHex(nfc) %q != RawHashHex(nfd) %q", rNFC, rNFD)
		}
	})

	t.Run("SidecarPath + ReadSidecar + WriteSidecar", func(t *testing.T) {
		r, _ := newTestRepo(t)
		// SidecarPath of NFC and NFD must be identical.
		if pNFC, pNFD := r.SidecarPath(nfc), r.SidecarPath(nfd); pNFC != pNFD {
			t.Errorf("SidecarPath(nfc) %q != SidecarPath(nfd) %q", pNFC, pNFD)
		}
		notes := "n"
		if err := r.WriteSidecar(context.Background(), nfd, Sidecar{Notes: &notes, Pinned: true}); err != nil {
			t.Fatalf("WriteSidecar(nfd): %v", err)
		}
		s, ok, err := r.ReadSidecar(context.Background(), nfc)
		if err != nil {
			t.Fatalf("ReadSidecar(nfc): %v", err)
		}
		if !ok {
			t.Fatalf("ReadSidecar(nfc) ok=false after WriteSidecar(nfd)")
		}
		if !s.Pinned || s.Notes == nil || *s.Notes != "n" {
			t.Errorf("sidecar payload mismatch: %+v", s)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		r, wsDir := newTestRepo(t)
		writeSnapshot(t, wsDir, nfc, []byte(`{}`))
		if err := r.Delete(context.Background(), nfd); err != nil {
			t.Fatalf("Delete(nfd): %v", err)
		}
		ok, err := r.Has(context.Background(), nfc)
		if err != nil {
			t.Fatalf("Has(nfc): %v", err)
		}
		if ok {
			t.Errorf("Has(nfc) still true after Delete(nfd)")
		}
	})

	t.Run("Rename", func(t *testing.T) {
		r, wsDir := newTestRepo(t)
		writeSnapshot(t, wsDir, nfc, []byte(`{}`))
		if err := r.Rename(context.Background(), nfd, "renamed"); err != nil {
			t.Fatalf("Rename(nfd, renamed): %v", err)
		}
		ok, _ := r.Has(context.Background(), "renamed")
		if !ok {
			t.Errorf("Has(renamed) false after Rename(nfd → renamed)")
		}
		ok, _ = r.Has(context.Background(), nfc)
		if ok {
			t.Errorf("Has(nfc) still true after Rename(nfd, renamed)")
		}
	})
}

// MEDIUM fix: WriteSidecar serialisation. Two goroutines hammering the
// SAME sidecar with two distinct payloads must produce a final on-disk
// file that parses cleanly to ONE of the two payloads — never a torn
// or interleaved hybrid. The parent-dir sentinel lock provides actual
// mutual exclusion across the AtomicWriteFile call (the previous
// per-file lock raced because each writer locked a different inode
// after the first rename).
func TestWriteSidecar_ConcurrentWritersAtomic(t *testing.T) {
	r, _ := newTestRepo(t)
	const name = "ws"
	const iters = 50

	notesA := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" // 52 'A'
	notesB := "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB" // 52 'B'
	a := Sidecar{Tags: []string{"a"}, Pinned: true, Notes: &notesA}
	b := Sidecar{Tags: []string{"b"}, Pinned: false, Notes: &notesB}

	var wg sync.WaitGroup
	errCh := make(chan error, 2*iters)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if err := r.WriteSidecar(context.Background(), name, a); err != nil {
				errCh <- fmt.Errorf("writerA: %w", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if err := r.WriteSidecar(context.Background(), name, b); err != nil {
				errCh <- fmt.Errorf("writerB: %w", err)
				return
			}
		}
	}()
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Fatalf("concurrent WriteSidecar: %v", e)
	}

	// Final read must yield exactly one of the two payloads, not a
	// torn or interleaved file.
	got, ok, err := r.ReadSidecar(context.Background(), name)
	if err != nil {
		t.Fatalf("ReadSidecar after concurrency: %v", err)
	}
	if !ok {
		t.Fatalf("ReadSidecar ok=false after concurrent writes (would indicate torn file)")
	}
	matchA := got.Pinned && len(got.Tags) == 1 && got.Tags[0] == "a" && got.Notes != nil && *got.Notes == notesA
	matchB := !got.Pinned && len(got.Tags) == 1 && got.Tags[0] == "b" && got.Notes != nil && *got.Notes == notesB
	if !matchA && !matchB {
		t.Errorf("final sidecar matches neither payload exactly: %+v", got)
	}
}
