package safefs

import (
	"errors"
	"fmt"
	"os"
)

// Warner is the minimal logger surface this package needs. The concrete
// *logger.Logger satisfies it via its Warn(msg, kv...any) method; the
// interface lives here (not in the logger package) so logger can import
// safefs without a cycle. Pass nil to suppress warnings.
type Warner interface {
	Warn(msg string, kv ...any)
}

// SymlinkPolicy is the centralised enum for every site that touches a path
// that might be a symlink. It replaces ad-hoc per-site reactions: each
// caller picks one of three policies and Enforce applies it uniformly.
type SymlinkPolicy int

const (
	// SymlinkRefuse — error out. Used for top-level managed dirs (state,
	// data, runtime, snapshot, trust). Failure is hard; the caller MUST
	// abort.
	SymlinkRefuse SymlinkPolicy = iota
	// SymlinkSkipWarn — log_warn, treat as absent. Used for individual
	// files inside managed dirs (sidecars, sock files, log files, trust
	// files) during sweeps and resets where one bad file should not abort
	// a batch.
	SymlinkSkipWarn
	// SymlinkRejectOp — return ErrIsSymlink to the caller; the caller
	// decides surfacing. Used by SafeOpenForRead / SafeRemove where the
	// op-level behaviour is context-specific.
	SymlinkRejectOp
)

// Enforce performs Lstat on path and applies the given policy.
//
// Returns:
//
//	ok=true,  err=nil      → not a symlink, proceed
//	ok=false, err=nil      → symlink, policy was SkipWarn (log already emitted)
//	ok=false, err=non-nil  → symlink with Refuse/RejectOp, OR Lstat failure
//
// A missing path is treated as non-symlink (ok=true, err=nil): the caller
// can then check os.ErrNotExist on the next operation and decide whether
// the absence is acceptable. Other Lstat failures are returned as-is.
func Enforce(path string, policy SymlinkPolicy, log Warner) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, fmt.Errorf("safefs: lstat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return true, nil
	}
	// The path IS a symlink. Apply policy.
	switch policy {
	case SymlinkRefuse:
		return false, fmt.Errorf("safefs: refusing symlink at %s: %w", path, ErrIsSymlink)
	case SymlinkSkipWarn:
		if log != nil {
			log.Warn("safefs: skipping symlink", "path", path)
		}
		return false, nil
	case SymlinkRejectOp:
		return false, ErrIsSymlink
	default:
		return false, fmt.Errorf("safefs: unknown symlink policy %d", policy)
	}
}

// SafeRemove unlinks path after verifying it is not a symlink. Behaves as
// Enforce(SymlinkRejectOp): if path is a symlink, ErrIsSymlink is returned
// without touching the filesystem.
//
// Missing path returns os.ErrNotExist (the same os.Remove would surface).
func SafeRemove(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return ErrIsSymlink
	}
	return os.Remove(path)
}

// SafeRemoveTree recursively removes path with Enforce at every step.
// Top-level path symlink: Refuse (hard error). File-level and inner-dir
// symlinks: SkipWarn (left in place). Never delegates to os.RemoveAll on
// the top-level path because that would silently follow a symlinked
// directory and delete the target's contents.
func SafeRemoveTree(path string, log Warner) error {
	ok, err := Enforce(path, SymlinkRefuse, log)
	if err != nil {
		return err
	}
	if !ok {
		return ErrIsSymlink
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return os.Remove(path)
	}
	return removeTreeWalk(path, log)
}

// removeTreeWalk does the recursive descent. Per-entry symlinks are
// SkipWarn (left in place; do not follow); non-symlink entries are removed
// depth-first. The directory itself is removed last after its contents are
// cleared.
func removeTreeWalk(dir string, log Warner) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("safefs: read dir %s: %w", dir, err)
	}
	skipped := false
	for _, e := range entries {
		full := dir + string(os.PathSeparator) + e.Name()
		info, lerr := os.Lstat(full)
		if lerr != nil {
			if errors.Is(lerr, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("safefs: lstat %s: %w", full, lerr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if log != nil {
				log.Warn("safefs: skipping symlink in tree", "path", full)
			}
			skipped = true
			continue
		}
		if info.IsDir() {
			if err := removeTreeWalk(full, log); err != nil {
				return err
			}
			continue
		}
		if err := os.Remove(full); err != nil {
			return fmt.Errorf("safefs: remove %s: %w", full, err)
		}
	}
	if skipped {
		// Directory still has skipped symlinks inside; leave it.
		return nil
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("safefs: remove dir %s: %w", dir, err)
	}
	return nil
}
