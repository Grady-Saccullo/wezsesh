// Package safefs is the only sanctioned writer to wezsesh-managed
// directories. Every disk mutation under state, data, snapshot, runtime,
// or trust dirs lands here: atomic writes via openat(2)+renameat(2) on a
// verified parent dirfd, POSIX advisory locks via build-tag-split linux/
// !linux files, and a centralised SymlinkPolicy enum used at every
// path-touching site.
//
// Why openat: O_NOFOLLOW only protects the final path component. Open()
// on a path string would silently traverse a symlinked ancestor. Opening
// the parent dir first with O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC, then doing
// every subsequent op via *at(2) syscalls relative to that fd, closes the
// gap.
//
// Why advisory-only: POSIX locks release ALL locks on a file when ANY fd
// referring to that file is closed within the process. Locks are held
// briefly around the verify-hash and re-hash steps of the save flow
// (§13.4); they are NEVER held across the IPC roundtrip. resurrect.lua
// uses io.open("w+") which ignores fcntl locks anyway, so the lock is
// only effective against other wezsesh binaries.
//
// See §8.1 for the public surface and §13.4 for the save flow.
package safefs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Public errors. ErrIsSymlink is returned wherever the SymlinkRejectOp
// policy applies; ErrLockTimeout when context.Done fires before the lock
// can be acquired; ErrNotExist when AcquireExclusive cannot find the
// target file.
var (
	ErrIsSymlink   = errors.New("safefs: path is a symlink")
	ErrLockTimeout = errors.New("safefs: lock acquire timed out")
	ErrNotExist    = errors.New("safefs: file does not exist")
)

// openatFlagsRDWR is the canonical flag bag for opening an existing file
// under a verified dirfd: read-write, refuse symlinks at the leaf, and
// CLOEXEC so a fork-spawned child cannot inherit the lock fd. CLOEXEC is
// belt-and-braces: the Go runtime marks fds CloseOnExec for os/exec but
// the brief window before that, plus raw syscall.ForkExec users, makes
// the explicit flag the safer default.
const openatFlagsRDWR = unix.O_RDWR | unix.O_NOFOLLOW | unix.O_CLOEXEC

// openatFlagsCreate adds O_CREAT for the first-save path
// (AcquireExclusiveOrCreate). O_EXCL is NOT set: the caller wants
// open-or-create-if-missing, not open-or-fail.
const openatFlagsCreate = unix.O_RDWR | unix.O_CREAT | unix.O_NOFOLLOW | unix.O_CLOEXEC

// VerifyDir opens parentDir with O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC. The
// returned fd is the safe handle every *at(2) syscall in this package
// uses; callers MUST Close it. Errors when parentDir is a symlink, does
// not exist, or is not a directory.
//
// We Lstat first so the symlink case surfaces as ErrIsSymlink uniformly
// across kernels. Linux returns ELOOP under O_DIRECTORY|O_NOFOLLOW for a
// symlink target; darwin returns ENOTDIR (because O_DIRECTORY is checked
// before the symlink-rejection path). Both are correct kernel behaviour
// — but the public contract here is one consistent error. The Lstat
// happens before Open, so a path-shape symlink is caught regardless.
func VerifyDir(parentDir string) (int, fs.FileInfo, error) {
	if parentDir == "" {
		return -1, nil, errors.New("safefs: VerifyDir: empty parentDir")
	}
	info, err := os.Lstat(parentDir)
	if err != nil {
		return -1, nil, fmt.Errorf("safefs: VerifyDir lstat %s: %w", parentDir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return -1, nil, fmt.Errorf("safefs: VerifyDir %s: %w", parentDir, ErrIsSymlink)
	}
	if !info.IsDir() {
		return -1, nil, fmt.Errorf("safefs: VerifyDir %s: not a directory", parentDir)
	}
	fd, err := unix.Open(parentDir, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return -1, nil, fmt.Errorf("safefs: VerifyDir %s: %w", parentDir, ErrIsSymlink)
		}
		return -1, nil, fmt.Errorf("safefs: VerifyDir open %s: %w", parentDir, err)
	}
	return fd, info, nil
}

// SafeOpenForRead opens path with O_RDONLY|O_NOFOLLOW|O_CLOEXEC. If the
// final component is a symlink, returns ErrIsSymlink. Missing files
// surface os.ErrNotExist via the underlying *os.File error wrapping.
func SafeOpenForRead(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("safefs: SafeOpenForRead %s: %w", path, ErrIsSymlink)
		}
		if errors.Is(err, unix.ENOENT) {
			return nil, fmt.Errorf("safefs: SafeOpenForRead %s: %w", path, os.ErrNotExist)
		}
		return nil, fmt.Errorf("safefs: SafeOpenForRead %s: %w", path, err)
	}
	return os.NewFile(uintptr(fd), path), nil
}

// AtomicWriteFile writes data to <parentDir>/<filename> via the
// dirfd-anchored temp+rename dance:
//
//  1. VerifyDir(parentDir) → dirfd
//  2. Openat(dirfd, ".<filename>.<8-hex>.tmp", O_WRONLY|O_CREAT|O_EXCL|
//     O_NOFOLLOW|O_CLOEXEC, perm) → tmpfd
//  3. Write(tmpfd, data); Fsync(tmpfd); Close(tmpfd)
//  4. Renameat(dirfd, tmpname, dirfd, filename) — atomic replacement
//
// On any failure after step 2, the temp file is best-effort removed via
// Unlinkat. Concurrent callers cannot collide on the temp name because
// the random suffix is drawn from crypto/rand and O_EXCL would catch the
// astronomically unlikely collision.
//
// perm is the final mode bits applied to the temp file at create time;
// the rename preserves them. Caller-supplied perm is masked through
// unix.S_IRWXU|S_IRWXG|S_IRWXO to keep the semantics of os.WriteFile.
func AtomicWriteFile(ctx context.Context, parentDir, filename string, data []byte, perm fs.FileMode) error {
	if filename == "" {
		return errors.New("safefs: AtomicWriteFile: empty filename")
	}
	if filepath.Base(filename) != filename {
		return fmt.Errorf("safefs: AtomicWriteFile: filename %q must not contain separators", filename)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dirfd, _, err := VerifyDir(parentDir)
	if err != nil {
		return err
	}
	defer unix.Close(dirfd)

	suffix, err := randHex8()
	if err != nil {
		return fmt.Errorf("safefs: AtomicWriteFile: rand suffix: %w", err)
	}
	tmpname := "." + filename + "." + suffix + ".tmp"

	// O_EXCL ensures we fail (and surface the bug) if the chosen name
	// already exists rather than silently truncating a stranger's file.
	tmpfd, err := unix.Openat(
		dirfd,
		tmpname,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		uint32(perm.Perm()),
	)
	if err != nil {
		return fmt.Errorf("safefs: AtomicWriteFile openat tmp: %w", err)
	}

	cleanup := func() { _ = unix.Unlinkat(dirfd, tmpname, 0) }

	if err := writeAll(tmpfd, data); err != nil {
		_ = unix.Close(tmpfd)
		cleanup()
		return fmt.Errorf("safefs: AtomicWriteFile write: %w", err)
	}
	if err := unix.Fsync(tmpfd); err != nil {
		_ = unix.Close(tmpfd)
		cleanup()
		return fmt.Errorf("safefs: AtomicWriteFile fsync: %w", err)
	}
	if err := unix.Close(tmpfd); err != nil {
		cleanup()
		return fmt.Errorf("safefs: AtomicWriteFile close: %w", err)
	}
	if err := unix.Renameat(dirfd, tmpname, dirfd, filename); err != nil {
		cleanup()
		return fmt.Errorf("safefs: AtomicWriteFile renameat: %w", err)
	}
	// fsync the parent dir so the rename hits disk; otherwise crash
	// recovery could find the new inode unreferenced.
	if err := unix.Fsync(dirfd); err != nil {
		// Best-effort — some filesystems return EINVAL on directory fsync;
		// crash semantics degrade gracefully.
		_ = err
	}
	return nil
}

func writeAll(fd int, data []byte) error {
	for len(data) > 0 {
		n, err := unix.Write(fd, data)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		if n == 0 {
			return errors.New("safefs: write returned 0 bytes")
		}
		data = data[n:]
	}
	return nil
}

func randHex8() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

