//go:build linux

package safefs

import "golang.org/x/sys/unix"

// tryLockFD attempts a non-blocking exclusive POSIX advisory lock on fd
// using F_OFD_SETLK — the Open File Description variant. OFD locks are
// bound to the underlying open file description (the kernel object the
// fd points at), NOT the process. This lets multiple fds in the same
// process coexist without one stomping the other's lock, which is the
// only sane semantics for a library that may be called twice in the same
// binary.
//
// F_OFD_SETLK is Linux 3.15+ (released 2014). The kernel baseline check
// in doctor enforces the floor.
//
// IMPORTANT: unix.F_OFD_SETLK is defined ONLY in
// golang.org/x/sys/unix/zerrors_linux.go. Any reference outside this
// build-tagged file fails to compile under GOOS=darwin / freebsd / etc.
// CI lint additionally blocks the constant outside this file (§16.5).
func tryLockFD(fd int) error {
	lk := &unix.Flock_t{
		Type:   unix.F_WRLCK,
		Whence: unix.SEEK_SET,
		Start:  0,
		Len:    0, // 0 means "to end of file" — locks the whole file
	}
	return unix.FcntlFlock(uintptr(fd), unix.F_OFD_SETLK, lk)
}

// unlockFD releases the OFD lock held on fd. Best-effort: closing the fd
// would drop the lock anyway, but explicit release surfaces errors and
// keeps the lock state symmetric with tryLockFD.
func unlockFD(fd int) error {
	lk := &unix.Flock_t{
		Type:   unix.F_UNLCK,
		Whence: unix.SEEK_SET,
		Start:  0,
		Len:    0,
	}
	return unix.FcntlFlock(uintptr(fd), unix.F_OFD_SETLK, lk)
}
