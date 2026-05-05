//go:build linux

package safefs

import "golang.org/x/sys/unix"

// File-level note: classifyStatfs is the Linux specialisation of the §8.1
// IsNetworkFS classifier. The darwin variant lives in netfs_darwin.go;
// other platforms fall through to a stub in netfs_stub.go (the build
// matrix only formally targets linux + darwin, but stubbing keeps
// `go build` honest on freebsd/openbsd if anyone tries it locally).

// classifyStatfs maps a Linux statfs result to (fsName, isNetwork). The
// magic numbers come from <linux/magic.h>; the network set covers NFS
// (all versions), SMB/CIFS, AFS, Ceph, Fuse (when used as a network
// proxy — best-effort), and a handful of cluster filesystems.
//
// tmpfs is NOT a network FS; the spec'd test asserts ("tmpfs", false).
func classifyStatfs(st *unix.Statfs_t) (string, bool) {
	switch st.Type {
	case unix.NFS_SUPER_MAGIC:
		return "nfs", true
	case unix.SMB_SUPER_MAGIC:
		return "smb", true
	case unix.AFS_FS_MAGIC:
		return "afs", true
	case unix.CEPH_SUPER_MAGIC:
		return "ceph", true
	case unix.FUSE_SUPER_MAGIC:
		// FUSE may back a network FS (sshfs, rclone) or a local one
		// (gocryptfs). Conservative: flag as network and let the user
		// override via doctor if it's a false positive.
		return "fuse", true
	case unix.TMPFS_MAGIC:
		return "tmpfs", false
	case unix.EXT4_SUPER_MAGIC:
		return "ext4", false
	case unix.BTRFS_SUPER_MAGIC:
		return "btrfs", false
	case unix.XFS_SUPER_MAGIC:
		return "xfs", false
	case unix.OVERLAYFS_SUPER_MAGIC:
		return "overlayfs", false
	}
	return "", false
}
