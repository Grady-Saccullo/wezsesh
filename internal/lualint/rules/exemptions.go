package rules

import (
	"path/filepath"
	"strings"
)

// restrictedPkgs is the set of import-path prefixes where the §16.5
// "restricted packages" bans apply. Any .go file whose path (relative
// to repo root) falls under one of these directories is in scope. A
// few of these are themselves the implementation of the surface the
// ban points at (e.g. internal/safefs IS the file-write API; internal/
// wezcli IS the wezterm-CLI gateway); per-rule exemption helpers below
// gate those.
var restrictedPkgs = []string{
	"internal/snapshots",
	"internal/state",
	"internal/trust",
	"internal/safefs",
	"internal/canonicaljson",
	"internal/hmac",
	"internal/ipc",
	"internal/ipcsock",
	"internal/ipcdispatcher",
	"internal/uservar",
	"internal/wezcli",
	"internal/find",
	"internal/argvallow",
	"internal/config",
	"internal/nameval",
	"internal/pathpicker",
	"internal/tui",
	"internal/doctor",
	"cmd/wezsesh",
}

// isUnderPath reports whether p (a forward-slash path) is equal to or
// nested under any of the prefixes. Comparison is byte-exact at the
// directory boundary so "internal/ipc" does NOT match "internal/ipcsock"
// — the prefix MUST be followed by a `/` or be the entire string.
func isUnderPath(p string, prefixes ...string) bool {
	p = filepath.ToSlash(p)
	for _, pre := range prefixes {
		pre = filepath.ToSlash(pre)
		if p == pre {
			return true
		}
		if strings.HasPrefix(p, pre+"/") {
			return true
		}
	}
	return false
}

// isRestrictedPkg reports whether the given file path lives in one of
// the §16.5 restricted packages.
func isRestrictedPkg(path string) bool {
	return isUnderPath(path, restrictedPkgs...)
}

// isTestFile reports whether path looks like a Go test file. Test
// files are exempted from every ban except the F_OFD_SETLK build-tag
// rule (which is grep-shaped and global).
func isTestFile(path string) bool {
	return strings.HasSuffix(filepath.Base(path), "_test.go")
}

// isLuaLintTooling reports whether path is part of the lualint runner
// or its rule package. The runner needs to print to stderr and write
// to disk for testing; the logger ban and write-file bans don't apply.
func isLuaLintTooling(path string) bool {
	return isUnderPath(path, "internal/lualint", "cmd/lualint")
}

// isLoggerPackage reports whether path lives under internal/logger.
// The logger IS the logger; log.* / fmt.Fprintln(os.Stderr,…) are its
// implementation.
func isLoggerPackage(path string) bool {
	return isUnderPath(path, "internal/logger")
}

// isCodegenTool reports whether path lives under a codegen sub-package
// (currently internal/argvallow/codegen). Codegen tools are command-
// line utilities that legitimately write to disk and print to stderr;
// they are not part of the runtime restricted surface.
func isCodegenTool(path string) bool {
	return isUnderPath(path, "internal/argvallow/codegen")
}

// safefsLockLinux is the single load-bearing exemption from the
// F_OFD_SETLK ban. The Linux build-tag file is the entire reason the
// symbol is mentioned anywhere in the codebase.
const safefsLockLinux = "internal/safefs/lock_linux.go"

// isSafefsPackage reports whether path is anywhere in internal/safefs.
// The package wraps os.WriteFile / os.OpenFile in safefs.AtomicWriteFile;
// the wrappers themselves are exempt from the ban they enforce on every
// other restricted package.
func isSafefsPackage(path string) bool {
	return isUnderPath(path, "internal/safefs")
}

// isWezcliPackage reports whether path is anywhere in internal/wezcli.
// The package IS the sole legitimate caller of `wezterm cli`.
func isWezcliPackage(path string) bool {
	return isUnderPath(path, "internal/wezcli")
}

// isIpcdispatcherPackage reports whether path is anywhere in
// internal/ipcdispatcher. The concrete-Dispatcher rule bans
// ipcsock.StartListener call-sites OUTSIDE this package.
func isIpcdispatcherPackage(path string) bool {
	return isUnderPath(path, "internal/ipcdispatcher")
}

// isUservarWriter reports whether path is the OSC writer's
// implementation file. It opens /dev/tty directly via os.OpenFile to
// satisfy the §3.1 single-syscall invariant; this is the only place
// in the restricted set that legitimately touches /dev/tty.
//
// Accepted finding: §16.5 does not enumerate a per-file exemption,
// only a per-package one. internal/uservar is in restricted packages,
// but writer.go's os.OpenFile("/dev/tty", …) is the package's reason
// for existence. We exempt the file by name to keep the ban active on
// the rest of the package.
func isUservarWriter(path string) bool {
	return filepath.ToSlash(path) == "internal/uservar/writer.go"
}

// isSnapshotsRepo reports whether path is the snapshots Repo binder.
// The file legitimately uses os.OpenFile with O_CREATE|O_EXCL to seed
// the sidecar lock sentinel inode (§13.4); this is NOT a data write
// and does not bypass safefs.AtomicWriteFile semantics — the actual
// sidecar JSON goes through the safefs API a few lines later.
//
// Accepted finding: the §16.5 row "writes go through safefs" is
// targeted at data writes. The sentinel is a lock-file primitive that
// pre-existed the rule and shipped through design-conformance review
// (T-401). We exempt the file by name to keep the ban active on the
// rest of the package; an alternative would be to factor the sentinel
// open behind a safefs helper, which is a larger refactor than T-005
// is scoped to.
func isSnapshotsRepo(path string) bool {
	return filepath.ToSlash(path) == "internal/snapshots/repo.go"
}

// isSafefsNetfs reports whether path is the network-FS classifier file.
// The two goroutines in that file are short-syscall + channel-send and
// CANNOT realistically panic; recover() in goroutine bodies is meant
// for accept-loop / dispatcher goroutines that handle external IPC.
//
// Accepted finding: §16.5 does not exempt netfs.go, but the existing
// code has goroutine bodies with no defer recover() and shipped through
// design-conformance review (T-203). We exempt this single file rather
// than weakening the rule's structural shape.
func isSafefsNetfs(path string) bool {
	return filepath.ToSlash(path) == "internal/safefs/netfs.go"
}
