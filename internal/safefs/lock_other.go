//go:build !linux

package safefs

import "golang.org/x/sys/unix"

// tryLockFD attempts a non-blocking exclusive POSIX advisory lock on fd
// using F_SETLK — the legacy process-scoped variant. On darwin / BSDs,
// F_OFD_* is not available, so we fall back to F_SETLK with the
// single-fd discipline: callers MUST NOT open the same path again while
// holding the lock, because closing ANY fd to the same inode within the
// process drops the lock for ALL fds.
//
// F_SETLK is non-blocking: contention surfaces as EAGAIN / EACCES, which
// the caller's poll loop handles. F_SETLKW (the blocking variant) is
// deliberately NOT used: fcntl auto-restarts on SA_RESTART signals,
// which makes context cancellation unreliable on darwin.
func tryLockFD(fd int) error {
	lk := &unix.Flock_t{
		Type:   unix.F_WRLCK,
		Whence: unix.SEEK_SET,
		Start:  0,
		Len:    0, // 0 means "to end of file"
	}
	return unix.FcntlFlock(uintptr(fd), unix.F_SETLK, lk)
}

// unlockFD releases the lock held on fd. Same caveat as tryLockFD: the
// lock is process-wide, not fd-scoped.
func unlockFD(fd int) error {
	lk := &unix.Flock_t{
		Type:   unix.F_UNLCK,
		Whence: unix.SEEK_SET,
		Start:  0,
		Len:    0,
	}
	return unix.FcntlFlock(uintptr(fd), unix.F_SETLK, lk)
}
