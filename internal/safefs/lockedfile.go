package safefs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// LockedFile is the only public handle to a locked file. It deliberately
// exposes ReadAt/WriteAt/Truncate/Sync/Stat/Size plus context-aware
// ReadAll/WriteAll, but NOT Close: closing any other fd that points at
// the same inode within this process would silently drop the POSIX
// advisory lock the caller paid for. Release ONLY via the closure
// returned alongside this handle.
type LockedFile struct {
	fd   int
	path string
}

// ReadAt implements io.ReaderAt over the locked fd.
func (lf *LockedFile) ReadAt(p []byte, off int64) (int, error) {
	if lf == nil || lf.fd < 0 {
		return 0, fs.ErrClosed
	}
	return unix.Pread(lf.fd, p, off)
}

// WriteAt implements io.WriterAt over the locked fd.
func (lf *LockedFile) WriteAt(p []byte, off int64) (int, error) {
	if lf == nil || lf.fd < 0 {
		return 0, fs.ErrClosed
	}
	return unix.Pwrite(lf.fd, p, off)
}

// Truncate sets the file size, possibly shrinking or extending it.
func (lf *LockedFile) Truncate(size int64) error {
	if lf == nil || lf.fd < 0 {
		return fs.ErrClosed
	}
	return unix.Ftruncate(lf.fd, size)
}

// Sync fsyncs the underlying fd.
func (lf *LockedFile) Sync() error {
	if lf == nil || lf.fd < 0 {
		return fs.ErrClosed
	}
	return unix.Fsync(lf.fd)
}

// Stat returns os.FileInfo for the locked fd by wrapping it in *os.File
// briefly. The wrapping does NOT take ownership of the fd; it is the
// caller's responsibility (well, the release closure's) to close.
func (lf *LockedFile) Stat() (os.FileInfo, error) {
	if lf == nil || lf.fd < 0 {
		return nil, fs.ErrClosed
	}
	var st unix.Stat_t
	if err := unix.Fstat(lf.fd, &st); err != nil {
		return nil, err
	}
	// Using os.NewFile.Stat() would dup-close the fd. Build a thin
	// FileInfo via os.Lstat at the path instead — same inode is open via
	// our fd, so the metadata is consistent for the brief lock window.
	return os.Lstat(lf.path)
}

// Size returns the on-disk size via fstat(2).
func (lf *LockedFile) Size() (int64, error) {
	if lf == nil || lf.fd < 0 {
		return 0, fs.ErrClosed
	}
	var st unix.Stat_t
	if err := unix.Fstat(lf.fd, &st); err != nil {
		return 0, err
	}
	return st.Size, nil
}

// ReadAll reads the entire file via repeated ReadAt; respects ctx
// cancellation between syscalls. Used by Phase A and Phase C of the save
// flow (§13.4) where the caller wants the whole snapshot for hashing.
func (lf *LockedFile) ReadAll(ctx context.Context) ([]byte, error) {
	if lf == nil || lf.fd < 0 {
		return nil, fs.ErrClosed
	}
	size, err := lf.Size()
	if err != nil {
		return nil, err
	}
	if size < 0 {
		return nil, errors.New("safefs: negative file size")
	}
	buf := make([]byte, 0, size)
	tmp := make([]byte, 32*1024)
	var off int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, rerr := lf.ReadAt(tmp, off)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			off += int64(n)
		}
		if rerr == io.EOF {
			return buf, nil
		}
		if rerr != nil {
			// pread on Linux/darwin returns 0,nil at EOF instead of
			// io.EOF; treat zero-length non-error read as EOF.
			return nil, rerr
		}
		if n == 0 {
			return buf, nil
		}
	}
}

// WriteAll truncates and writes; respects ctx between syscalls.
func (lf *LockedFile) WriteAll(ctx context.Context, p []byte) error {
	if lf == nil || lf.fd < 0 {
		return fs.ErrClosed
	}
	if err := lf.Truncate(0); err != nil {
		return err
	}
	var off int64
	for len(p) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := unix.Pwrite(lf.fd, p, off)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		if n == 0 {
			return errors.New("safefs: pwrite returned 0 bytes")
		}
		off += int64(n)
		p = p[n:]
	}
	return nil
}

// AcquireExclusive opens path under a verified parent dirfd and acquires
// an exclusive POSIX advisory lock. Polls non-blocking with 10 ms → 100 ms
// exponential backoff (capped) until ctx is Done; logs a structured WARN
// at the 1 s and 3 s contention thresholds (§17.3 row "F_SETLK polling
// fairness").
//
// Linux uses F_OFD_SETLK (Open File Description locks, bound to fd, not
// process — the only sane multi-fd-per-process model). Darwin / BSDs use
// F_SETLK with the single-fd discipline (callers must not open the same
// path again while holding the lock). The split is mandatory at the
// build-tag layer because unix.F_OFD_SETLK is only defined in
// zerrors_linux.go and referencing it from a darwin build would fail to
// compile.
//
// Returns ErrLockTimeout when ctx expires; ErrNotExist if the file is
// missing. The release closure is the ONLY way to drop the lock.
func AcquireExclusive(ctx context.Context, path string) (*LockedFile, func(), error) {
	if path == "" {
		return nil, nil, errors.New("safefs: AcquireExclusive: empty path")
	}
	parentDir, filename := filepath.Split(path)
	if parentDir == "" {
		parentDir = "."
	}
	dirfd, _, err := VerifyDir(parentDir)
	if err != nil {
		return nil, nil, err
	}
	defer unix.Close(dirfd)

	fd, err := unix.Openat(dirfd, filename, openatFlagsRDWR, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, nil, ErrNotExist
		}
		if errors.Is(err, unix.ELOOP) {
			return nil, nil, ErrIsSymlink
		}
		return nil, nil, fmt.Errorf("safefs: openat %s: %w", path, err)
	}
	return acquireOnFd(ctx, fd, path)
}

// AcquireExclusiveOrCreate is the first-save path (§13.4): the file may
// not yet exist, in which case it is created with O_CREAT | the same
// hardened flags. perm is the mode bits at creation (typically 0600).
//
// The dirfd-anchored openat means the create is symlink-safe and
// race-resistant in a way that path-based os.OpenFile is not.
func AcquireExclusiveOrCreate(ctx context.Context, parentDir, filename string, perm fs.FileMode) (*LockedFile, func(), error) {
	if filename == "" {
		return nil, nil, errors.New("safefs: AcquireExclusiveOrCreate: empty filename")
	}
	if filepath.Base(filename) != filename {
		return nil, nil, fmt.Errorf("safefs: AcquireExclusiveOrCreate: filename %q must not contain separators", filename)
	}
	dirfd, _, err := VerifyDir(parentDir)
	if err != nil {
		return nil, nil, err
	}
	defer unix.Close(dirfd)

	fd, err := unix.Openat(dirfd, filename, openatFlagsCreate, uint32(perm.Perm()))
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, nil, ErrIsSymlink
		}
		return nil, nil, fmt.Errorf("safefs: openat %s/%s: %w", parentDir, filename, err)
	}
	return acquireOnFd(ctx, fd, filepath.Join(parentDir, filename))
}

// acquireOnFd is the shared poll loop that drives F_OFD_SETLK on Linux
// and F_SETLK elsewhere via the build-tag-split tryLockFD helper. The
// 10 ms initial → 100 ms cap progression keeps the worst-case acquire
// latency under 100 ms once the holder releases, while still being light
// enough on CPU during longer holds.
func acquireOnFd(ctx context.Context, fd int, path string) (*LockedFile, func(), error) {
	const (
		initialBackoff = 10 * time.Millisecond
		maxBackoff     = 100 * time.Millisecond
		warn1          = 1 * time.Second
		warn3          = 3 * time.Second
	)
	start := time.Now()
	backoff := initialBackoff
	warnedAt1 := false
	warnedAt3 := false

	for {
		err := tryLockFD(fd)
		if err == nil {
			lf := &LockedFile{fd: fd, path: path}
			release := func() {
				// Best-effort lock release before close. Closing the fd
				// drops the lock anyway, but explicit release surfaces
				// errors if any.
				_ = unlockFD(fd)
				_ = unix.Close(fd)
				lf.fd = -1
			}
			return lf, release, nil
		}
		if !isLockContention(err) {
			_ = unix.Close(fd)
			return nil, nil, fmt.Errorf("safefs: lock %s: %w", path, err)
		}
		// Contention: wait + retry until ctx fires.
		elapsed := time.Since(start)
		if !warnedAt1 && elapsed >= warn1 {
			warnedAt1 = true
			emitContentionWarn(path, elapsed)
		}
		if !warnedAt3 && elapsed >= warn3 {
			warnedAt3 = true
			emitContentionWarn(path, elapsed)
		}
		select {
		case <-ctx.Done():
			_ = unix.Close(fd)
			return nil, nil, ErrLockTimeout
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// emitContentionWarn writes a structured WARN line to slog at the 1 s and
// 3 s thresholds. We use the package-default slog (the central logger.
// Logger isn't passed through this API in §8.1) so callers get the line
// regardless of which subsystem owns the lock site.
func emitContentionWarn(path string, elapsed time.Duration) {
	slog.Warn("safefs: lock contended",
		"path", path,
		"elapsed_ms", elapsed.Milliseconds())
}

// isLockContention identifies the errno values that mean "another holder
// has it; retry later" as opposed to a hard failure.
func isLockContention(err error) bool {
	return errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.EWOULDBLOCK)
}
