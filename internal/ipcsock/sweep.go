// Sweep handles the §8.7 / §3.5 "Reply dir cleanup mtime 60 s" startup
// duty. cmd/wezsesh calls SweepStale(replyDir) before tea.Run() so any
// leftover sock files from a crashed prior run do not collide with this
// run's sockPath choice (paths are first-8-hex-of-ULID — collisions are
// astronomically rare in practice but the sweep is still cheap and
// fail-closed).
//
// Per-file Enforce policy is SymlinkSkipWarn: a single tampered entry
// must not abort the batch. Missing dir is a no-op (treated as "nothing
// to sweep" — the dispatcher will create the dir on first use).
package ipcsock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Grady-Saccullo/wezsesh/internal/logger"
	"github.com/Grady-Saccullo/wezsesh/internal/safefs"
)

// staleAge is the §3.5 "Reply dir cleanup mtime 60 s" threshold.
const staleAge = 60 * time.Second

// SweepStale unlinks every *.sock file in dir whose mtime is older than
// 60 s. Symlinks are skipped with a SymlinkSkipWarn log entry; the dir
// itself is verified non-symlink (SymlinkRefuse) before scanning.
//
// dir missing is NOT an error — the dispatcher creates it on first use
// and the sweep is best-effort. The function returns nil so callers can
// invoke it unconditionally before listener startup.
func SweepStale(dir string, log *logger.Logger) error {
	if dir == "" {
		return errors.New("ipcsock: SweepStale: empty dir")
	}
	// Top-level dir: hard-fail on symlink (it is a wezsesh-managed dir
	// per §12.1; a symlink there is a refuse-class condition).
	ok, err := safefs.Enforce(dir, safefs.SymlinkRefuse, log)
	if err != nil {
		// Missing dir surfaces here as ok=true, err=nil per the
		// Enforce contract. A non-nil err means Lstat failed for a
		// reason other than ENOENT, which is fatal.
		return fmt.Errorf("ipcsock: sweep enforce %s: %w", dir, err)
	}
	if !ok {
		// SymlinkRefuse + symlink → ok=false, err non-nil; covered by
		// the branch above. Defensive belt-and-braces.
		return fmt.Errorf("ipcsock: sweep dir %s: %w", dir, safefs.ErrIsSymlink)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("ipcsock: read dir %s: %w", dir, err)
	}
	cutoff := time.Now().Add(-staleAge)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sock") {
			continue
		}
		path := filepath.Join(dir, name)
		fileOK, err := safefs.Enforce(path, safefs.SymlinkSkipWarn, log)
		if err != nil {
			// Non-symlink Lstat failure on an entry — log + continue
			// so one bad sock does not abort the sweep.
			if log != nil {
				log.Warn("ipcsock: sweep enforce file",
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
				log.Warn("ipcsock: sweep lstat",
					"path", path, "err", err.Error())
			}
			continue
		}
		if info.ModTime().After(cutoff) {
			// Younger than 60 s — leave it. Could belong to a
			// concurrently-running peer wezsesh.
			continue
		}
		if err := os.Remove(path); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			if log != nil {
				log.Warn("ipcsock: sweep remove",
					"path", path, "err", err.Error())
			}
		}
	}
	return nil
}
