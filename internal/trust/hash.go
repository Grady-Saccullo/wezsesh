// Package trust backs $XDG_DATA_HOME/wezsesh/allow/. The trust file *name*
// is the SHA-256 of (path, command_bytes); content is advisory only
// (§10.5). Hash construction is length-prefixed (§8.12, P §6.11): any
// `\n`-style separator scheme allows forgery via a workspace name that
// embeds a literal `\n`, so the only sanctioned construction is
//
//	sha256( uint32_be(len(path)) || path_bytes ||
//	        uint32_be(len(cmd))  || cmd_bytes )
//
// Default fail-CLOSED. Missing trust file (or symlink at the trust file
// path) → no exec, no implicit trust.
package trust

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// ComputeHash returns the lowercase hex SHA-256 of the length-prefixed
// concatenation of absSidecarPath and commandBytes.
//
// Length prefixing closes the forgery window that a `\n` (or any byte)
// separator opens: with `path + "\n" + cmd`, a workspace named
// `foo\nrm -rf ~` collides with path `foo` and command `rm -rf ~`. With
// uint32_be length prefixes, every (path, cmd) pair has a unique
// pre-image regardless of the bytes inside either field.
func ComputeHash(absSidecarPath string, commandBytes []byte) string {
	h := sha256.New()
	var hdr [4]byte
	pathBytes := []byte(absSidecarPath)
	binary.BigEndian.PutUint32(hdr[:], uint32(len(pathBytes)))
	h.Write(hdr[:])
	h.Write(pathBytes)
	binary.BigEndian.PutUint32(hdr[:], uint32(len(commandBytes)))
	h.Write(hdr[:])
	h.Write(commandBytes)
	return hex.EncodeToString(h.Sum(nil))
}
