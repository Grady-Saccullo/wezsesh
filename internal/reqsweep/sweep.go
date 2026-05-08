// Package reqsweep handles the §12.4 startup sweep of stale `*.json`
// request-pointer files under `<runtime_dir>/req/`.
//
// The Lua handler (`plugin/wezsesh/ipc.lua`) unlinks request files on
// successful processing; orphans are exclusively requests that the
// handler rejected at pre-steps (1)–(3) BEFORE the unlink (empty value,
// pointer JSON parse fail, pointer field-shape invalid, or stat-guard
// fail). Without this sweep, `<runtime_dir>/req/` accumulates indefinitely.
//
// The sweep is the storage-side counterpart to the doctor's
// `runtime.dir.req_orphans` check. The "what counts as orphaned"
// threshold is owned by `doctor.ReqOrphanThreshold` and imported here so
// the two cannot drift.
//
// Symlink policy mirrors `internal/ipcsock`'s sweep:
//   - Top-level `<runtime_dir>/req/` is `SymlinkRefuse` (a managed dir
//     symlinked is a hard refuse-class condition per CLAUDE.md §12.1).
//   - Per-entry symlink is `SymlinkSkipWarn` — a tampered entry must
//     not abort the batch.
//
// Best-effort semantics: a missing dir is a no-op (the dispatcher
// creates `<runtime_dir>/req/` on first use; the sweep can run before
// the first session). Per-entry failures are logged via the passed
// logger and the loop continues. The function returns nil for any
// best-effort failure so callers can invoke it unconditionally before
// listener startup.
package reqsweep

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Grady-Saccullo/wezsesh/internal/doctor"
	"github.com/Grady-Saccullo/wezsesh/internal/logger"
	"github.com/Grady-Saccullo/wezsesh/internal/safefs"
)

// SweepStale unlinks every *.json file in reqDir whose mtime is older
// than `doctor.ReqOrphanThreshold`. Symlinks are skipped with a
// SymlinkSkipWarn log entry; the dir itself is verified non-symlink
// (SymlinkRefuse) before scanning.
//
// reqDir missing is NOT an error — the dispatcher creates it on first
// use and the sweep is best-effort. Empty reqDir is a programmer error
// and is rejected.
func SweepStale(reqDir string, log *logger.Logger) error {
	if reqDir == "" {
		return errors.New("reqsweep: SweepStale: empty dir")
	}
	// Top-level dir: hard-fail on symlink (it is a wezsesh-managed dir
	// per CLAUDE.md §12.1; a symlink there is refuse-class).
	ok, err := safefs.Enforce(reqDir, safefs.SymlinkRefuse, log)
	if err != nil {
		// Missing dir surfaces here as ok=true, err=nil per the
		// Enforce contract. A non-nil err covers both the symlink case
		// (wraps ErrIsSymlink) and other Lstat failures; both are fatal.
		return fmt.Errorf("reqsweep: sweep enforce %s: %w", reqDir, err)
	}
	if !ok {
		// SymlinkRefuse + symlink → ok=false, err non-nil; covered by
		// the branch above. Defensive belt-and-braces.
		return fmt.Errorf("reqsweep: sweep dir %s: %w", reqDir, safefs.ErrIsSymlink)
	}
	entries, err := os.ReadDir(reqDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("reqsweep: read dir %s: %w", reqDir, err)
	}
	cutoff := time.Now().Add(-doctor.ReqOrphanThreshold)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(reqDir, name)
		fileOK, err := safefs.Enforce(path, safefs.SymlinkSkipWarn, log)
		if err != nil {
			// Non-symlink Lstat failure on an entry — log + continue
			// so one bad request file does not abort the sweep.
			if log != nil {
				log.Warn("reqsweep: sweep enforce file",
					"path", path, "err", err.Error())
			}
			continue
		}
		if !fileOK {
			// Symlink + SkipWarn already logged inside Enforce.
			continue
		}
		info, err := os.Lstat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if log != nil {
				log.Warn("reqsweep: sweep lstat",
					"path", path, "err", err.Error())
			}
			continue
		}
		if info.ModTime().After(cutoff) {
			// Younger than threshold — leave it. Could belong to a
			// concurrently-running peer wezsesh, or to a request the
			// Lua handler is about to process.
			continue
		}
		if err := os.Remove(path); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			if log != nil {
				log.Warn("reqsweep: sweep remove",
					"path", path, "err", err.Error())
			}
		}
	}
	return nil
}
