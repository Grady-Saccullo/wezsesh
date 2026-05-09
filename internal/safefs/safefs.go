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
	"strings"

	"golang.org/x/sys/unix"
)

// RotatedName turns an active log leaf like "wezsesh.log" into the
// rotated form "wezsesh.<n>.log" — the numeric slot lands BEFORE the
// extension so a file picker / `ls *.log` glob still surfaces every
// generation as a `.log` file. Pure on its inputs; both the safefs
// `RotateSingleDeep` path and `internal/logger`'s `rotateLocked` route
// every numbered-slot name through this helper.
//
// If the leaf has no `.` (or the only `.` is the leading char), falls
// back to the legacy `<leaf>.<n>` shape so a future caller passing a
// dotless leaf still gets a deterministic, unique name.
func RotatedName(leaf string, n int) string {
	if i := strings.LastIndexByte(leaf, '.'); i > 0 {
		return fmt.Sprintf("%s.%d%s", leaf[:i], n, leaf[i:])
	}
	return fmt.Sprintf("%s.%d", leaf, n)
}

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

// OpenAppendOnly opens <parentDir>/<filename> for append-only writes
// under a verified parent dirfd. The flag bag is
// O_WRONLY|O_CREAT|O_APPEND|O_NOFOLLOW|O_CLOEXEC; if the leaf is a
// symlink, the open fails with ErrIsSymlink. Caller owns the returned
// *os.File and must Close it.
//
// Why dirfd+openat: O_NOFOLLOW only protects the final path component,
// so a path-based os.OpenFile would silently traverse a symlinked
// ancestor. Opening parentDir first with O_DIRECTORY|O_NOFOLLOW closes
// the gap.
func OpenAppendOnly(parentDir, filename string, mode fs.FileMode) (*os.File, error) {
	if filename == "" {
		return nil, errors.New("safefs: OpenAppendOnly: empty filename")
	}
	if filepath.Base(filename) != filename {
		return nil, fmt.Errorf("safefs: OpenAppendOnly: filename %q must not contain separators", filename)
	}
	dirfd, _, err := VerifyDir(parentDir)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dirfd)

	fd, err := unix.Openat(
		dirfd,
		filename,
		unix.O_WRONLY|unix.O_CREAT|unix.O_APPEND|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		uint32(mode.Perm()),
	)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("safefs: OpenAppendOnly %s/%s: %w", parentDir, filename, ErrIsSymlink)
		}
		return nil, fmt.Errorf("safefs: OpenAppendOnly %s/%s: %w", parentDir, filename, err)
	}
	return os.NewFile(uintptr(fd), filepath.Join(parentDir, filename)), nil
}

// MkdirEnforce ensures <parentDir>/<name> exists as a regular directory
// owned by the current effective uid, refusing every symlink shape. The
// helper is idempotent: pre-existing dir owned by self → success; missing
// → created at mode 0700; symlinked → ErrIsSymlink; pre-existing dir
// owned by another uid → error. Either succeeds with the dir in the
// expected shape or returns an error — never half-state.
//
// Why dirfd+mkdirat+fstatat: the mkdir is anchored to a parent dirfd
// opened with O_NOFOLLOW so an ancestor symlink cannot redirect the
// create. After the mkdir, fstatat with AT_SYMLINK_NOFOLLOW on the same
// dirfd verifies the leaf is a regular dir, not a symlink-to-dir.
func MkdirEnforce(parentDir, name string, mode fs.FileMode) error {
	if name == "" {
		return errors.New("safefs: MkdirEnforce: empty name")
	}
	if filepath.Base(name) != name {
		return fmt.Errorf("safefs: MkdirEnforce: name %q must not contain separators", name)
	}
	dirfd, _, err := VerifyDir(parentDir)
	if err != nil {
		return err
	}
	defer unix.Close(dirfd)

	if err := unix.Mkdirat(dirfd, name, uint32(mode.Perm())); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("safefs: MkdirEnforce mkdirat %s/%s: %w", parentDir, name, err)
		}
	}

	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("safefs: MkdirEnforce fstatat %s/%s: %w", parentDir, name, err)
	}
	if (st.Mode & unix.S_IFMT) == unix.S_IFLNK {
		return fmt.Errorf("safefs: MkdirEnforce %s/%s: %w", parentDir, name, ErrIsSymlink)
	}
	if (st.Mode & unix.S_IFMT) != unix.S_IFDIR {
		return fmt.Errorf("safefs: MkdirEnforce %s/%s: not a directory", parentDir, name)
	}
	if euid := os.Geteuid(); euid >= 0 && int(st.Uid) != euid {
		return fmt.Errorf("safefs: MkdirEnforce %s/%s: owned by uid %d (expected %d)", parentDir, name, st.Uid, euid)
	}
	return nil
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

// RotateSingleDeep performs a one-deep rotation of <dir>/<leaf>: if the
// file exists and is strictly larger than thresholdBytes, drop any
// pre-existing rotated slot (RotatedName(leaf, 1)) then rename the
// active file to that slot. The next writer's open-with-O_APPEND-or-
// O_CREAT will create a fresh active file. For leaf="plugin.log" the
// rotated slot is "plugin.1.log" (extension preserved at the tail);
// see RotatedName for the rule.
//
// Symlink discipline: the parent directory is symlink-refused via
// VerifyDir; the active leaf and the rotated destination are inline
// Lstat-checked and refused if either is a symlink. The dirfd is held
// only for the parent-validation step — the rename and unlink calls are
// path-based today, mirroring the existing logger rotation pattern, and
// will move to dirfd-anchored *at(2) syscalls when safefs grows
// SafeRenameAt / SafeUnlinkAt primitives.
//
// Threshold semantics are strict greater-than: a file at exactly
// thresholdBytes does NOT rotate. This matches the existing
// rotatingWriter discipline (size+incoming > threshold).
//
// Best-effort contract: a missing file returns nil (nothing to rotate);
// a missing dir returns the underlying VerifyDir error so the caller
// can decide whether to surface it. Callers that treat rotation as
// advisory should log-and-continue.
//
// Race window: a concurrent appender (e.g. the wezsesh Lua plugin) can
// write a single line between the rename and the next writer's
// open(O_APPEND|O_CREAT) — that line lands in the rotated slot.
// Single-line writes are POSIX-atomic up to PIPE_BUF (Darwin: 512 B,
// Linux: 4 KiB), so as long as each writer caps its records at 512 B
// the rotation cannot interleave a partial line. Documented in the
// plan.
func RotateSingleDeep(dir, leaf string, thresholdBytes int64) error {
	if leaf == "" {
		return errors.New("safefs: RotateSingleDeep: empty leaf")
	}
	if filepath.Base(leaf) != leaf {
		return fmt.Errorf("safefs: RotateSingleDeep: leaf %q must not contain separators", leaf)
	}
	dirfd, _, err := VerifyDir(dir)
	if err != nil {
		return err
	}
	// We don't use the dirfd for the rename/unlink yet (see comment
	// above); close it now so the caller's process table stays clean
	// regardless of the rotate path taken.
	_ = unix.Close(dirfd)

	active := filepath.Join(dir, leaf)
	rotated := filepath.Join(dir, RotatedName(leaf, 1))

	// Leaf-level symlink refuse for the active path. Missing → ok=true,
	// nothing to rotate.
	ok, err := Enforce(active, SymlinkRefuse, nil)
	if err != nil {
		return err
	}
	if !ok {
		// Refuse hit — Enforce returned an error above, unreachable.
		return ErrIsSymlink
	}
	info, err := os.Lstat(active)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("safefs: RotateSingleDeep lstat %s: %w", active, err)
	}
	if info.Size() <= thresholdBytes {
		return nil
	}

	// Rotated destination must not be a symlink either.
	ok, err = Enforce(rotated, SymlinkRefuse, nil)
	if err != nil {
		return err
	}
	if !ok {
		return ErrIsSymlink
	}
	if err := os.Remove(rotated); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("safefs: RotateSingleDeep drop %s: %w", rotated, err)
	}
	if err := os.Rename(active, rotated); err != nil {
		return fmt.Errorf("safefs: RotateSingleDeep rename %s -> %s: %w", active, rotated, err)
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
