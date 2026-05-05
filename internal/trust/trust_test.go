package trust

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Grady-Saccullo/wezsesh/internal/safefs"
)

// goldenHash recomputes the expected hash via the spec algorithm so the
// test cross-checks ComputeHash against the rule, not against itself.
func goldenHash(t *testing.T, path string, cmd []byte) string {
	t.Helper()
	h := sha256.New()
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(path)))
	h.Write(hdr[:])
	h.Write([]byte(path))
	binary.BigEndian.PutUint32(hdr[:], uint32(len(cmd)))
	h.Write(hdr[:])
	h.Write(cmd)
	return hex.EncodeToString(h.Sum(nil))
}

// TestComputeHash_Stable — known-answer tests for the length-prefixed
// construction. Empty fields and ASCII fields both check out; the
// returned digest is lowercase hex, 64 chars long.
func TestComputeHash_Stable(t *testing.T) {
	cases := []struct {
		name string
		path string
		cmd  []byte
	}{
		{"empty path empty cmd", "", nil},
		{"empty cmd", "/tmp/foo/.wezsesh.json", nil},
		{"empty path", "", []byte("npm install")},
		{"both populated", "/tmp/foo/.wezsesh.json", []byte("npm install")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeHash(tc.path, tc.cmd)
			want := goldenHash(t, tc.path, tc.cmd)
			if got != want {
				t.Errorf("ComputeHash(%q, %q):\n got  %s\n want %s", tc.path, string(tc.cmd), got, want)
			}
			if len(got) != 64 {
				t.Errorf("hash length: want 64, got %d", len(got))
			}
			if strings.ToLower(got) != got {
				t.Errorf("hash not lowercase hex: %s", got)
			}
		})
	}
}

// TestComputeHash_NoNewlineCollision is the load-bearing CVE-prevention
// test for the length-prefix invariant.
//
// Under a (broken) `path + "\n" + cmd` scheme, these two pairs produce
// identical hash inputs:
//
//	(path="foo", cmd="\nrm -rf ~")
//	(path="foo\nrm", cmd="-rf ~")
//
// Length-prefixed concatenation gives them DIFFERENT hashes because the
// uint32_be(len) prefix encodes the field boundary unambiguously.
//
// If this ever fails, someone replaced ComputeHash with a separator
// scheme — that is a CVE. STOP and revert.
func TestComputeHash_NoNewlineCollision(t *testing.T) {
	a := ComputeHash("foo", []byte("\nrm -rf ~"))
	b := ComputeHash("foo\nrm", []byte("-rf ~"))
	if a == b {
		t.Fatalf("hash collision under separator-style attack:\n a=%s\n b=%s\nlength-prefix invariant violated", a, b)
	}
}

// TestComputeHash_PathMatters — same command bytes at different paths
// MUST produce different hashes (hash binds path + content). This is
// the cross-machine-trust guarantee.
func TestComputeHash_PathMatters(t *testing.T) {
	cmd := []byte("npm install")
	a := ComputeHash("/Users/a/foo/.wezsesh.json", cmd)
	b := ComputeHash("/Users/b/foo/.wezsesh.json", cmd)
	if a == b {
		t.Fatalf("paths must be hash inputs: %s == %s", a, b)
	}
}

// TestComputeHash_CommandMatters — same path with different command
// bytes MUST produce different hashes. Editing the command on the same
// machine fails closed.
func TestComputeHash_CommandMatters(t *testing.T) {
	path := "/Users/a/foo/.wezsesh.json"
	a := ComputeHash(path, []byte("npm install"))
	b := ComputeHash(path, []byte("npm install ")) // trailing space
	if a == b {
		t.Fatalf("command bytes must be hash inputs: %s == %s", a, b)
	}
}

// TestComputeHash_BinaryCommand — hash construction works on arbitrary
// bytes including embedded NULs (the field is `commandBytes`, not a Go
// string).
func TestComputeHash_BinaryCommand(t *testing.T) {
	cmd := []byte{0x00, 0x01, 0x02, 0xff, 0xfe}
	h := ComputeHash("/tmp/foo", cmd)
	want := goldenHash(t, "/tmp/foo", cmd)
	if h != want {
		t.Errorf("binary command bytes mismatch: got %s want %s", h, want)
	}
}

// TestOpen_FreshMkdir — Open creates the trust dir at 0700 if missing.
func TestOpen_FreshMkdir(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "allow")
	s, err := Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s == nil {
		t.Fatal("Open returned nil store with no error")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("lstat trust dir: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("trust dir is not a directory: mode=%v", info.Mode())
	}
	// On Unix the perm bits should be 0700; the umask cannot loosen
	// MkdirAll-with-mode beyond what the OS actually applied, so
	// verify the bits we actually set are present.
	if runtime.GOOS != "windows" {
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("trust dir permits group/other access: mode=%v", info.Mode().Perm())
		}
	}
}

// TestOpen_RefusesSymlinkDir — top-level trust dir symlink is REFUSED
// (fail-CLOSED, not skip-warn). The trust dir is a top-level managed
// dir per §10.5; Refuse is mandatory.
func TestOpen_RefusesSymlinkDir(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "real")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "allow")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), link, nil)
	if err == nil {
		t.Fatal("Open succeeded on symlinked trust dir; want refusal")
	}
	if !errors.Is(err, safefs.ErrIsSymlink) {
		t.Errorf("want ErrIsSymlink, got %v", err)
	}
}

// TestOpen_RejectsRegularFile — double-defence on the `info.IsDir()`
// branch in Open. A regular file at `trustDir` cannot be opened as the
// trust dir; MkdirAll returns an error before Enforce/IsDir gets a
// chance, but the explicit IsDir guard is the second line of defence —
// assert it (or the upstream MkdirAll error) refuses Open.
func TestOpen_RejectsRegularFile(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "allow")
	if err := os.WriteFile(path, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), path, nil)
	if err == nil {
		t.Fatal("Open succeeded on regular file at trustDir; want error")
	}
}

// TestApprove_IsApproved_RoundTrip — Approve writes a file at
// <hash>; IsApproved finds it; same call with mutated command bytes
// returns false.
func TestApprove_IsApproved_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	path := "/Users/grady/code/foo/.wezsesh.json"
	cmd := []byte("npm install")

	if s.IsApproved(ctx, path, cmd) {
		t.Fatal("IsApproved before Approve: want false")
	}
	if err := s.Approve(ctx, path, cmd); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if !s.IsApproved(ctx, path, cmd) {
		t.Fatal("IsApproved after Approve: want true")
	}
	// Mutated command → different hash → not approved.
	if s.IsApproved(ctx, path, []byte("npm install ")) {
		t.Fatal("IsApproved with mutated cmd: want false")
	}
	// Mutated path → different hash → not approved.
	if s.IsApproved(ctx, path+"x", cmd) {
		t.Fatal("IsApproved with mutated path: want false")
	}

	// File mode is 0600 and the body is `{"path":"<absSidecarPath>"}`.
	hash := ComputeHash(path, cmd)
	full := filepath.Join(dir, hash)
	info, err := os.Lstat(full)
	if err != nil {
		t.Fatalf("lstat trust file: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Errorf("trust file permits group/other access: %v", info.Mode().Perm())
	}
	body, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("read trust file: %v", err)
	}
	var parsed fileBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if parsed.Path != path {
		t.Errorf("body.path: want %q got %q", path, parsed.Path)
	}
}

// TestIsApproved_RejectsSymlinkFile — pre-place a symlink at the trust
// file path; IsApproved returns false (Lstat catches the symlink before
// any read). Same-UID attackers cannot redirect approval.
func TestIsApproved_RejectsSymlinkFile(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	path := "/Users/grady/code/foo/.wezsesh.json"
	cmd := []byte("npm install")
	hash := ComputeHash(path, cmd)

	// Drop a symlink in place of the would-be trust file. Target
	// content is a valid JSON body, but the file *type* is a symlink,
	// so IsApproved must refuse.
	target := filepath.Join(dir, "real")
	if err := os.WriteFile(target, []byte(`{"path":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, hash)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if s.IsApproved(ctx, path, cmd) {
		t.Fatal("IsApproved on symlinked trust file: want false (fail-CLOSED)")
	}
}

// TestRevoke_Idempotent — Revoke on a missing entry is not an error.
// Revoke after Approve removes the file.
func TestRevoke_Idempotent(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	path := "/Users/grady/code/foo/.wezsesh.json"
	cmd := []byte("npm install")

	if err := s.Revoke(ctx, path, cmd); err != nil {
		t.Errorf("Revoke on missing: want nil, got %v", err)
	}
	if err := s.Approve(ctx, path, cmd); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := s.Revoke(ctx, path, cmd); err != nil {
		t.Errorf("Revoke after Approve: %v", err)
	}
	if s.IsApproved(ctx, path, cmd) {
		t.Fatal("IsApproved after Revoke: want false")
	}
}

// TestList_FiltersJunk — non-hash filenames in the trust dir are
// ignored by List; only 64-char lowercase-hex names appear.
func TestList_FiltersJunk(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	if err := s.Approve(ctx, "/Users/a/.wezsesh.json", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := s.Approve(ctx, "/Users/b/.wezsesh.json", []byte("b")); err != nil {
		t.Fatal(err)
	}
	// Junk files in the trust dir.
	if err := os.WriteFile(filepath.Join(dir, ".DS_Store"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List: want 2 entries, got %d (%v)", len(list), list)
	}
	want := map[string]string{
		ComputeHash("/Users/a/.wezsesh.json", []byte("a")): "/Users/a/.wezsesh.json",
		ComputeHash("/Users/b/.wezsesh.json", []byte("b")): "/Users/b/.wezsesh.json",
	}
	for _, e := range list {
		if want[e.Hash] != e.Path {
			t.Errorf("entry %s: want path %q got %q", e.Hash, want[e.Hash], e.Path)
		}
	}
}

// TestPrune_RemovesStaleEntries — a recorded path that no longer
// exists on disk gets pruned; entries whose paths still exist survive.
func TestPrune_RemovesStaleEntries(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()

	// Two sidecars: "live" (still exists) and "dead" (will be removed).
	live := filepath.Join(t.TempDir(), ".wezsesh.json")
	dead := filepath.Join(t.TempDir(), ".wezsesh.json")
	if err := os.WriteFile(live, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dead, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Approve(ctx, live, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := s.Approve(ctx, dead, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dead); err != nil {
		t.Fatal(err)
	}

	removed, err := s.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 1 {
		t.Errorf("Prune removed: want 1 got %d", removed)
	}
	if !s.IsApproved(ctx, live, []byte("a")) {
		t.Error("Prune removed the live entry")
	}
	if s.IsApproved(ctx, dead, []byte("a")) {
		t.Error("Prune did not remove the dead entry")
	}
}

// TestRebind_HappyPath — identical command bytes at the new path →
// rebind succeeds; old hash file removed; new hash file present.
//
// (Acceptance gate: "Trust rebind happy path" §17.3.)
func TestRebind_HappyPath(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	oldPath := "/Users/grady/code/foo/.wezsesh.json"
	newPath := "/Users/grady/code/foo-renamed/.wezsesh.json"
	cmd := []byte("npm install")

	if err := s.Approve(ctx, oldPath, cmd); err != nil {
		t.Fatalf("Approve old: %v", err)
	}

	if err := s.Rebind(ctx, oldPath, newPath, cmd); err != nil {
		t.Fatalf("Rebind: %v", err)
	}
	if !s.IsApproved(ctx, newPath, cmd) {
		t.Error("Rebind did not approve new path")
	}
	if s.IsApproved(ctx, oldPath, cmd) {
		t.Error("Rebind did not revoke old path")
	}

	// Defensive: confirm the old hash file is gone from the dir.
	oldHash := ComputeHash(oldPath, cmd)
	if _, err := os.Lstat(filepath.Join(dir, oldHash)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old hash file still present: lstat err=%v", err)
	}
}

// TestRebind_DivergedCommand — the new path has DIFFERENT command
// bytes; the caller must not call Rebind with mismatched cmdBytes.
// VerifyRebindEligible is the gatekeeper: it returns
// ErrTrustRebindDiverged and the caller MUST refuse to invoke Rebind.
//
// This test simulates the full flow: the caller reads each sidecar
// once, asks VerifyRebindEligible to gate the rebind, and on diverged
// command refuses. The old approval remains intact.
//
// (Acceptance gate: "Trust rebind diverged command" §17.3.)
func TestRebind_DivergedCommand(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()

	// Two on-disk sidecars: oldPath has cmdA, newPath has cmdB.
	oldDir := t.TempDir()
	newDir := t.TempDir()
	oldPath := filepath.Join(oldDir, ".wezsesh.json")
	newPath := filepath.Join(newDir, ".wezsesh.json")
	cmdA := []byte("npm install")
	cmdB := []byte("rm -rf ~") // diverged
	if err := os.WriteFile(oldPath, cmdA, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, cmdB, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Approve(ctx, oldPath, cmdA); err != nil {
		t.Fatalf("Approve old: %v", err)
	}

	// CLI surface emulation: read old sidecar once, then ask
	// VerifyRebindEligible whether the new sidecar's bytes match.
	readOnce := func(p string) ([]byte, error) { return os.ReadFile(p) }
	oldCmd, err := readOnce(oldPath)
	if err != nil {
		t.Fatalf("read old sidecar: %v", err)
	}
	if _, err := VerifyRebindEligible(newPath, oldCmd, readOnce); !errors.Is(err, ErrTrustRebindDiverged) {
		t.Fatalf("VerifyRebindEligible: want ErrTrustRebindDiverged, got %v", err)
	}

	// Rebind MUST NOT be called by the CLI. The old approval is
	// untouched.
	if !s.IsApproved(ctx, oldPath, cmdA) {
		t.Error("old approval missing after refusal: state corrupted")
	}
	// New approval MUST NOT exist.
	if s.IsApproved(ctx, newPath, cmdB) {
		t.Error("new approval present after refusal: scope uplift!")
	}
	if s.IsApproved(ctx, newPath, cmdA) {
		t.Error("new approval present (under cmdA) after refusal")
	}
}

// TestRebind_SelfRebind — Rebind(ctx, p, p, cmd) is a no-op when the
// approval already exists. Without the oldPath == newPath short-circuit,
// Approve(new) and Revoke(old) would target the same hash file: the
// idempotent Approve writes the file, then Revoke deletes it, silently
// destroying the approval and returning nil. This regression test pins
// the "preserve approval" post-condition (the trust file at <dir>/<hash>
// must still exist; IsApproved must still return true).
func TestRebind_SelfRebind(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	path := "/Users/grady/code/foo/.wezsesh.json"
	cmd := []byte("npm install")

	if err := s.Approve(ctx, path, cmd); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	if err := s.Rebind(ctx, path, path, cmd); err != nil {
		t.Fatalf("Rebind self: want nil, got %v", err)
	}

	// Approval must survive: the trust file at <dir>/<hash> still exists
	// and IsApproved still returns true.
	hash := ComputeHash(path, cmd)
	full := filepath.Join(dir, hash)
	if _, err := os.Lstat(full); err != nil {
		t.Errorf("trust file missing after self-rebind: %v", err)
	}
	if !s.IsApproved(ctx, path, cmd) {
		t.Error("IsApproved after self-rebind: want true (approval destroyed)")
	}
}

// TestRebind_MissingSource — Rebind without a prior Approve at oldPath
// returns ErrTrustRebindMissing.
func TestRebind_MissingSource(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	cmd := []byte("npm install")
	err = s.Rebind(ctx, "/old", "/new", cmd)
	if !errors.Is(err, ErrTrustRebindMissing) {
		t.Errorf("Rebind missing source: want ErrTrustRebindMissing, got %v", err)
	}
}

// TestVerifyRebindEligible_HappyPath — byte-equal command at the new
// path returns the bytes and a nil error.
func TestVerifyRebindEligible_HappyPath(t *testing.T) {
	dir := t.TempDir()
	side := filepath.Join(dir, ".wezsesh.json")
	want := []byte("npm install")
	if err := os.WriteFile(side, want, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := VerifyRebindEligible(side, want, os.ReadFile)
	if err != nil {
		t.Fatalf("VerifyRebindEligible: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("returned bytes: want %q got %q", want, got)
	}
}

// TestVerifyRebindEligible_Missing — absent new-path sidecar returns
// (nil, nil) so the CLI can surface a "missing" message instead of
// "diverged".
func TestVerifyRebindEligible_Missing(t *testing.T) {
	got, err := VerifyRebindEligible("/no/such/path", []byte("x"), os.ReadFile)
	if err != nil {
		t.Errorf("missing path: want nil err, got %v", err)
	}
	if got != nil {
		t.Errorf("missing path: want nil bytes, got %q", got)
	}
}

// TestVerifyRebindEligible_LengthMismatch — different-length command
// bytes short-circuit to ErrTrustRebindDiverged without a constant-time
// compare. (Behavioural test only — we don't assert timing.)
func TestVerifyRebindEligible_LengthMismatch(t *testing.T) {
	dir := t.TempDir()
	side := filepath.Join(dir, ".wezsesh.json")
	if err := os.WriteFile(side, []byte("npm install"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := VerifyRebindEligible(side, []byte("npm"), os.ReadFile)
	if !errors.Is(err, ErrTrustRebindDiverged) {
		t.Errorf("length mismatch: want ErrTrustRebindDiverged, got %v", err)
	}
}
