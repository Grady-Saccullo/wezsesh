package safefs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestVerifyDirHappyPath ensures a normal directory passes.
func TestVerifyDirHappyPath(t *testing.T) {
	dir := t.TempDir()
	fd, info, err := VerifyDir(dir)
	if err != nil {
		t.Fatalf("VerifyDir: %v", err)
	}
	defer closeFD(fd)
	if !info.IsDir() {
		t.Fatalf("info.IsDir false")
	}
}

// TestVerifyDirRejectsSymlink — a symlink at the parent dir slot must
// refuse via ErrIsSymlink. This is the load-bearing invariant: O_NOFOLLOW
// at the path level via unix.Open with O_DIRECTORY|O_NOFOLLOW catches
// the leaf, AND the post-Lstat check belt-and-braces catches anything
// the kernel misinterprets.
func TestVerifyDirRejectsSymlink(t *testing.T) {
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	sym := filepath.Join(tmp, "sym")
	if err := os.Symlink(real, sym); err != nil {
		t.Fatal(err)
	}
	_, _, err := VerifyDir(sym)
	if !errors.Is(err, ErrIsSymlink) {
		t.Fatalf("VerifyDir(sym): want ErrIsSymlink, got %v", err)
	}
}

// TestSafeOpenForReadRejectsSymlink covers the SafeOpenForRead policy.
func TestSafeOpenForReadRejectsSymlink(t *testing.T) {
	tmp := t.TempDir()
	real := filepath.Join(tmp, "r.txt")
	if err := os.WriteFile(real, []byte("r"), 0o600); err != nil {
		t.Fatal(err)
	}
	sym := filepath.Join(tmp, "s.txt")
	if err := os.Symlink(real, sym); err != nil {
		t.Fatal(err)
	}
	_, err := SafeOpenForRead(sym)
	if !errors.Is(err, ErrIsSymlink) {
		t.Errorf("SafeOpenForRead(sym): want ErrIsSymlink, got %v", err)
	}
	f, err := SafeOpenForRead(real)
	if err != nil {
		t.Fatalf("SafeOpenForRead(real): %v", err)
	}
	defer f.Close()
}

// TestSafeOpenForReadMissing — missing path → os.ErrNotExist (wrapped).
func TestSafeOpenForReadMissing(t *testing.T) {
	tmp := t.TempDir()
	_, err := SafeOpenForRead(filepath.Join(tmp, "nope"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("SafeOpenForRead(missing): want ErrNotExist, got %v", err)
	}
}

// TestAtomicWriteFileBasic — file lands with the requested bytes + perm.
func TestAtomicWriteFileBasic(t *testing.T) {
	tmp := t.TempDir()
	if err := AtomicWriteFile(context.Background(), tmp, "out.txt", []byte("hello"), 0o600); err != nil {
		t.Fatalf("AtomicWriteFile: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(tmp, "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("contents mismatch: %q", got)
	}
	st, err := os.Stat(filepath.Join(tmp, "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("perm mismatch: %v", st.Mode().Perm())
	}
}

// TestAtomicWriteFileRejectsSlashInName — filename arg must not contain
// path separators (the dirfd-anchored design relies on this).
func TestAtomicWriteFileRejectsSlashInName(t *testing.T) {
	tmp := t.TempDir()
	err := AtomicWriteFile(context.Background(), tmp, "sub/x.txt", []byte("x"), 0o600)
	if err == nil {
		t.Errorf("expected rejection for filename with separator")
	}
}

// TestAtomicWriteFileRejectsSymlinkParent — symlinked parent dir → fail.
func TestAtomicWriteFileRejectsSymlinkParent(t *testing.T) {
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	sym := filepath.Join(tmp, "sym")
	if err := os.Symlink(real, sym); err != nil {
		t.Fatal(err)
	}
	err := AtomicWriteFile(context.Background(), sym, "out.txt", []byte("x"), 0o600)
	if !errors.Is(err, ErrIsSymlink) {
		t.Errorf("AtomicWriteFile through symlink parent: want ErrIsSymlink, got %v", err)
	}
}

// TestAtomicWriteFileConcurrentDisjoint — covers the §17.3 "Request-file
// atomic write (spike-#3)" gate as it applies to safefs: concurrent
// AtomicWriteFile calls produce DIFFERENT files when they target
// different filenames; tmp+rename never leaves a half-written file in
// place; O_EXCL prevents temp-name collisions.
func TestAtomicWriteFileConcurrentDisjoint(t *testing.T) {
	tmp := t.TempDir()
	const N = 32
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("payload_%d.json", i)
			payload := []byte(fmt.Sprintf(`{"i":%d,"v":1}`, i))
			if err := AtomicWriteFile(context.Background(), tmp, name, payload, 0o600); err != nil {
				errs <- err
				return
			}
			got, err := os.ReadFile(filepath.Join(tmp, name))
			if err != nil {
				errs <- err
				return
			}
			if string(got) != string(payload) {
				errs <- fmt.Errorf("content mismatch for %s: got %q want %q", name, got, payload)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// Sweep for stale temp files. AtomicWriteFile names them
	// .<filename>.<8-hex>.tmp; a successful run leaves none behind.
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("stale temp file found: %s", e.Name())
		}
	}
	if got := len(entries); got != N {
		t.Errorf("entry count: got %d want %d", got, N)
	}
}

// TestAtomicWriteFileSameNameOverwrite — concurrent writers to the SAME
// filename are serialised by renameat: every observation sees a complete
// payload (one of the N candidates), never a torn one.
func TestAtomicWriteFileSameNameOverwrite(t *testing.T) {
	tmp := t.TempDir()
	const N = 16
	payloads := make([][]byte, N)
	hashes := make(map[string]bool, N)
	for i := 0; i < N; i++ {
		payloads[i] = []byte(fmt.Sprintf("attempt-%d-%s", i, hex.EncodeToString([]byte{byte(i)})))
		h := sha256.Sum256(payloads[i])
		hashes[hex.EncodeToString(h[:])] = true
	}

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = AtomicWriteFile(context.Background(), tmp, "shared", payloads[i], 0o600)
		}(i)
	}
	wg.Wait()
	got, err := os.ReadFile(filepath.Join(tmp, "shared"))
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(got)
	if !hashes[hex.EncodeToString(h[:])] {
		t.Errorf("final file is torn or unrecognised; sha=%s", hex.EncodeToString(h[:]))
	}
}

// TestAtomicWriteFileCtxCanceled — a cancelled ctx surfaces immediately,
// before any disk mutation.
func TestAtomicWriteFileCtxCanceled(t *testing.T) {
	tmp := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := AtomicWriteFile(ctx, tmp, "out", []byte("x"), 0o600); !errors.Is(err, context.Canceled) {
		t.Errorf("ctx canceled: want context.Canceled, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "out")); !os.IsNotExist(err) {
		t.Errorf("file should not exist when ctx pre-canceled: %v", err)
	}
}

// closeFD is a small helper for fds returned by VerifyDir.
func closeFD(fd int) {
	_ = closeUnix(fd)
}

// closeUnix wraps unix.Close to keep the test file free of unix imports
// where possible. Implementation lives in safefs; using io.Closer-style
// here would require a *os.File round-trip we don't want.
func closeUnix(fd int) error {
	return closeFn(fd)
}

// closeFn is set in init() to the unix.Close binding so the test file
// stays platform-agnostic. (Avoids re-importing golang.org/x/sys in
// _test.go just for one syscall.)
var closeFn func(int) error

func init() {
	closeFn = closeFdViaOSFile
}

// closeFdViaOSFile closes a raw fd by wrapping it in *os.File and
// Calling Close. This is safe at end-of-test where ownership is
// transferred.
func closeFdViaOSFile(fd int) error {
	f := os.NewFile(uintptr(fd), "")
	if f == nil {
		return errors.New("invalid fd")
	}
	return f.Close()
}

