//go:build darwin

package safefs

import (
	"bytes"

	"golang.org/x/sys/unix"
)

// classifyStatfs maps a darwin statfs result to (fsName, isNetwork) based
// on the f_fstypename string that darwin returns. Linux-style super-magic
// numbers don't apply.
//
// The "fileprovider" cloud-sync mounts (iCloud, Dropbox via File Provider)
// generally report fstypename "apfs" because they live underneath an APFS
// volume; Layer 2 (path prefix) catches those. Statfs alone is reliable
// for genuine network mounts: nfs, smbfs, afpfs, webdav.
func classifyStatfs(st *unix.Statfs_t) (string, bool) {
	name := cstrToString(st.Fstypename[:])
	switch name {
	case "nfs":
		return "nfs", true
	case "smbfs":
		return "smbfs", true
	case "afpfs":
		return "afpfs", true
	case "webdav":
		return "webdav", true
	case "fuse", "macfuse", "osxfuse":
		return name, true
	case "apfs", "hfs", "exfat", "msdos", "ntfs", "tmpfs":
		return name, false
	}
	return name, false
}

func cstrToString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}
