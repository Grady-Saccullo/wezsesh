package safefs

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/text/unicode/norm"
)

// IsNetworkFS classifies path as local vs network/cloud-sync. Two-layer
// detection per §8.1: Layer 1 is unix.Statfs against the kernel-known
// network FS magic numbers; Layer 2 is a darwin-only path-prefix list
// catching File Provider cloud-sync mounts (iCloud / Dropbox / Google
// Drive / OneDrive / Box / Nextcloud / Proton / Seafile and similar)
// that report a benign "apfs" type but are nevertheless network-backed.
//
// Returns:
//
//	fsType    — kernel-reported FS name when known, else "" or "fileprovider"
//	fsLayer   — which layer made the call: "statfs" or "prefix"
//	isNetwork — true if either layer flags it
//
// Both syscalls run inside a goroutine under context.WithTimeout (2 s for
// Statfs, 500 ms for EvalSymlinks) so a hung NFS ancestor or stalled
// File Provider daemon cannot freeze TUI startup. On overrun: classify
// as network for Statfs (fail-safe — the user is on something sluggish);
// for symlink-resolution, fall back to the unresolved cleaned path with
// no error so the prefix check still has a chance to fire.
func IsNetworkFS(path string) (fsType string, fsLayer string, isNetwork bool, err error) {
	if path == "" {
		return "", "", false, nil
	}

	// Resolve the path via tilde-expand → EvalSymlinks → Clean → NFC.
	resolved, _ := resolvePathSafe(path)

	// Layer 1 — Statfs.
	t, isNet, lerr := statfsClassify(resolved)
	if lerr == nil {
		if isNet {
			return t, "statfs", true, nil
		}
	}

	// Layer 2 — darwin File Provider prefixes. On other platforms, any
	// remaining cloud-sync detection is moot: linux mounts NFS / SMB
	// directly and Statfs catches it; freebsd and friends similarly.
	if runtime.GOOS == "darwin" {
		if isFileProviderPath(resolved) {
			return "fileprovider", "prefix", true, nil
		}
	}

	if lerr != nil {
		// Statfs failed (timed out or errored) AND prefix didn't match.
		// Surface the error so callers can degrade visibly.
		return "", "statfs", false, lerr
	}
	return t, "statfs", false, nil
}

// resolvePathSafe runs the tilde-expand → EvalSymlinks → Clean → NFC
// pipeline with a 500 ms ceiling on EvalSymlinks. On overrun or error,
// returns the cleaned-but-unresolved path so the prefix check still has
// material to work with.
func resolvePathSafe(path string) (string, error) {
	expanded := expandTilde(path)
	cleaned := filepath.Clean(expanded)

	type result struct {
		resolved string
		err      error
	}
	ch := make(chan result, 1)
	go func() {
		r, e := filepath.EvalSymlinks(cleaned)
		ch <- result{r, e}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	select {
	case <-ctx.Done():
		return norm.NFC.String(cleaned), ctx.Err()
	case r := <-ch:
		if r.err != nil || r.resolved == "" {
			return norm.NFC.String(cleaned), r.err
		}
		return norm.NFC.String(r.resolved), nil
	}
}

// expandTilde rewrites a leading ~ or ~/ into the user's home dir. Full
// ~user expansion is intentionally NOT supported; that would be an
// attractive nuisance for a security-sensitive path-touching helper.
func expandTilde(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// statfsClassify runs unix.Statfs in a goroutine bounded by 2 s. The
// timeout matters: a hung NFS ancestor will block Statfs indefinitely
// without it. On overrun we report isNetwork=true (fail-safe: the user
// IS on something stalled, even if we can't name it).
func statfsClassify(path string) (string, bool, error) {
	type result struct {
		t     string
		isNet bool
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		var st unix.Statfs_t
		if err := unix.Statfs(path, &st); err != nil {
			ch <- result{"", false, err}
			return
		}
		t, isNet := classifyStatfs(&st)
		ch <- result{t, isNet, nil}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	select {
	case <-ctx.Done():
		// Hung Statfs. Treat as network — the user is on something
		// sluggish even if we can't name it.
		return "", true, ctx.Err()
	case r := <-ch:
		return r.t, r.isNet, r.err
	}
}

// isFileProviderPath checks the resolved path against the darwin File
// Provider prefix list from §8.1. NFC-normalized comparison;
// case-insensitive on darwin (HFS+/APFS default behaviour).
func isFileProviderPath(resolved string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	home = norm.NFC.String(home)
	r := norm.NFC.String(resolved)

	prefixes := []string{
		filepath.Join(home, "Library/Mobile Documents") + string(os.PathSeparator),
		filepath.Join(home, "Library/CloudStorage") + string(os.PathSeparator),
		filepath.Join(home, "iCloud Drive") + string(os.PathSeparator),
		filepath.Join(home, "iCloud Drive"),
		filepath.Join(home, "Dropbox") + string(os.PathSeparator),
		filepath.Join(home, "Dropbox"),
		filepath.Join(home, "Nextcloud") + string(os.PathSeparator),
		filepath.Join(home, "Nextcloud"),
		filepath.Join(home, "Google Drive") + string(os.PathSeparator),
		filepath.Join(home, "Google Drive"),
		"/Volumes/GoogleDrive",
	}
	for _, p := range prefixes {
		if strings.EqualFold(r, p) {
			return true
		}
		if strings.EqualFold(r[:minInt(len(r), len(p))], p) && len(r) > len(p) {
			// Prefix match — only count when r truly extends p, so the
			// equality case above isn't double-counted.
			return true
		}
	}
	return false
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
