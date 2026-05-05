//go:build !linux && !darwin

package safefs

import "golang.org/x/sys/unix"

// classifyStatfs is a stub for platforms outside the formal build matrix
// (linux, darwin). Returns ("", false) so IsNetworkFS still answers
// without crashing; callers on freebsd/openbsd/netbsd see "local" and
// move on. Full classification is a v0.2 follow-up if those targets
// become first-class.
func classifyStatfs(_ *unix.Statfs_t) (string, bool) {
	return "", false
}
