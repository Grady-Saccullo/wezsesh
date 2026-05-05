// Snapshot sidecar (§10.2). Stored at <encoded>.wezsesh.json next to
// the snapshot file resurrect owns. Carries pinned/tags/notes/hooks
// metadata that live ENTIRELY on our side — resurrect never reads
// them. Sidecar is the single source of truth for `pinned` on saved
// workspaces (§13.11).
//
// Schema migration:
//
//	v == 0 (missing)  → ReadSidecar returns zero, ok=false, nil err
//	v == 1            → parsed, ok=true, nil err
//	v >  1            → log_warn, rename to .wezsesh.json.v<N>.bak,
//	                     return zero, ok=false, nil err
//
// Future versions can read older versions (we promote v0 absence and
// v1 reads forward). Older versions never break on newer.
package snapshots

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Grady-Saccullo/wezsesh/internal/nameval"
	"github.com/Grady-Saccullo/wezsesh/internal/safefs"
)

// SidecarSchemaVersion is the value stored in .version on every WriteSidecar.
const SidecarSchemaVersion = 1

// Sidecar is the on-disk shape for <encoded>.wezsesh.json.
type Sidecar struct {
	Version   int      `json:"version"`
	Tags      []string `json:"tags,omitempty"`
	Pinned    bool     `json:"pinned,omitempty"`
	OnCreate  *string  `json:"on_create,omitempty"`
	OnRestore *string  `json:"on_restore,omitempty"`
	Notes     *string  `json:"notes,omitempty"`
}

// SidecarPath returns the absolute path to the sidecar for a workspace.
// `name` is the user-facing name; encoding is applied here so callers
// don't have to remember. Name is NFC-normalised at the boundary so
// the same logical name in NFD/NFC resolves to one on-disk file
// (§15.1).
func (r *Repo) SidecarPath(name string) string {
	encoded := EncodeName(nameval.NormalizeNFC(name))
	return filepath.Join(r.workspaceDir, encoded+sidecarSuffix)
}

const sidecarSuffix = ".wezsesh.json"

// sidecarLockSentinel is the parent-dir-anchored lock file
// AcquireExclusiveOrCreate'd around every sidecar write. Two writers
// racing on the SAME sidecar name would otherwise lock different
// inodes (writer-1 locks inode-A, AtomicWriteFile renames inode-B
// over the path, writer-2 locks inode-B). A single shared sentinel
// inode gives us actual mutual exclusion across the AtomicWriteFile
// call. The sentinel is created on first use and never removed; size
// stays at 0.
const sidecarLockSentinel = ".wezsesh.sidecar.lock"

// ReadSidecar parses the sidecar for `name`. See package doc for the
// version migration story.
//
// Read path is symlink-safe (SafeOpenForRead); a sidecar that points
// at an unrelated file via symlink is treated as missing rather than
// followed.
func (r *Repo) ReadSidecar(_ context.Context, name string) (Sidecar, bool, error) {
	path := r.SidecarPath(nameval.NormalizeNFC(name))

	// Symlink defense — sidecars are individual files inside a managed
	// dir, so SkipWarn-equivalent: treat as absent on symlink.
	ok, err := safefs.Enforce(path, safefs.SymlinkSkipWarn, r.log)
	if err != nil {
		return Sidecar{}, false, err
	}
	if !ok {
		return Sidecar{}, false, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Sidecar{}, false, nil
		}
		return Sidecar{}, false, fmt.Errorf("snapshots: read sidecar %s: %w", path, err)
	}

	if int64(len(data)) > MaxFileSize {
		if r.log != nil {
			r.log.Warn("snapshots: sidecar exceeds size cap; treating as missing",
				"path", path, "size", len(data), "cap", int64(MaxFileSize))
		}
		return Sidecar{}, false, nil
	}

	// Peek at the version *before* full parse, so we can reroute
	// future-version files without surfacing a parse error to the
	// caller. A v=2 file may well have new required fields that fail
	// our v=1 schema; the version check is gate-1.
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		// Malformed JSON. Per the parse-tolerance invariant we do NOT
		// abort; the entry surfaces with SidecarOK=false. Log_warn so
		// doctor / debug can correlate.
		if r.log != nil {
			r.log.Warn("snapshots: sidecar parse failed; treating as missing",
				"path", path, "err", err.Error())
		}
		return Sidecar{}, false, nil
	}

	switch {
	case probe.Version == 0:
		// Pre-versioned or missing. Treat as absent.
		return Sidecar{}, false, nil
	case probe.Version == SidecarSchemaVersion:
		// Fall through to parse.
	case probe.Version > SidecarSchemaVersion:
		// Migration: back up to .v<N>.bak, treat as absent. Subsequent
		// writes from older binaries will create a fresh v=1 file
		// while preserving the v>1 content for whoever can read it.
		bakName := filepath.Base(path) + fmt.Sprintf(".v%d.bak", probe.Version)
		bakPath := filepath.Join(filepath.Dir(path), bakName)
		// os.Rename is fine here — we already verified the source is
		// not a symlink via Enforce above; and the target is in the
		// same managed dir.
		if rerr := os.Rename(path, bakPath); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			if r.log != nil {
				r.log.Warn("snapshots: sidecar future-version rename failed",
					"path", path, "version", probe.Version, "err", rerr.Error())
			}
			// Even if rename fails we still return ok=false so the
			// caller does not act on a future-shape we cannot trust.
			return Sidecar{}, false, nil
		}
		if r.log != nil {
			r.log.Warn("snapshots: sidecar future version backed up",
				"path", path, "backup", bakPath, "version", probe.Version)
		}
		return Sidecar{}, false, nil
	default: // negative version
		if r.log != nil {
			r.log.Warn("snapshots: sidecar invalid version; treating as missing",
				"path", path, "version", probe.Version)
		}
		return Sidecar{}, false, nil
	}

	var s Sidecar
	if err := json.Unmarshal(data, &s); err != nil {
		if r.log != nil {
			r.log.Warn("snapshots: sidecar parse failed; treating as missing",
				"path", path, "err", err.Error())
		}
		return Sidecar{}, false, nil
	}
	return s, true, nil
}

// WriteSidecar atomically writes the sidecar. The actual on-disk
// write goes through safefs.AtomicWriteFile (temp + renameat under a
// dirfd-anchored open). Mutual exclusion across concurrent writers is
// provided by an exclusive POSIX advisory lock on a parent-dir
// SENTINEL (.wezsesh.sidecar.lock) — locking the sidecar path itself
// is decorative, because the AtomicWriteFile rename swaps a fresh
// inode over the path mid-write, so writer-2 would lock a different
// inode than writer-1 and the two operations would interleave.
//
// The sentinel is shared across all sidecars in the workspace dir.
// That coarsens the lock from per-name to per-dir, but sidecar writes
// are infrequent (TUI tag/pin updates) so contention is negligible
// and the correctness gain is worth it. The lock is released as soon
// as AtomicWriteFile returns.
//
// If s.Version is zero, it is set to the current schema version.
func (r *Repo) WriteSidecar(ctx context.Context, name string, s Sidecar) error {
	if s.Version == 0 {
		s.Version = SidecarSchemaVersion
	}
	if s.Version != SidecarSchemaVersion {
		return fmt.Errorf("snapshots: WriteSidecar: refusing to write version %d (current is %d)", s.Version, SidecarSchemaVersion)
	}

	path := r.SidecarPath(nameval.NormalizeNFC(name))
	parent, file := filepath.Split(path)

	// Sentinel symlink defense is built into AcquireExclusiveOrCreate:
	// it opens the sentinel via openat(dirfd, name, O_NOFOLLOW|O_CREAT|...)
	// which returns ELOOP → ErrIsSymlink if the sentinel is a symlink.
	// We surface that as a wrapped error rather than calling Enforce
	// pre-emptively (which would TOCTOU with the open).
	_, release, err := safefs.AcquireExclusiveOrCreate(ctx, parent, sidecarLockSentinel, 0o600)
	if err != nil {
		return fmt.Errorf("snapshots: WriteSidecar acquire sentinel %s/%s: %w", parent, sidecarLockSentinel, err)
	}
	defer release()

	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("snapshots: WriteSidecar marshal: %w", err)
	}

	if err := safefs.AtomicWriteFile(ctx, parent, file, data, 0o600); err != nil {
		return fmt.Errorf("snapshots: WriteSidecar write %s: %w", path, err)
	}
	return nil
}
