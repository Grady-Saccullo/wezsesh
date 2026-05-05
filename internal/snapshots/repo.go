// Package snapshots is the boundary between wezsesh and resurrect's
// snapshot files. Resurrect owns the .json file shape; we read it
// tolerantly. All snapshot-side metadata (tags, pinned, hooks, notes)
// lives in the parallel <encoded>.wezsesh.json sidecar that wezsesh
// owns end-to-end.
//
// Invariants (CLAUDE.md / §8.10):
//
//   - NewRepo is BIND-ONLY. The first List call performs the directory
//     scan; startup latency is amortised across the lifetime of the
//     binary, not paid up-front.
//   - Hashes are LAZY. Each Entry carries a HashLazy closure that
//     memoises on first call; List does NOT precompute. Otherwise
//     startup is O(total bytes) instead of O(file count).
//   - Per-file size cap 10 MiB; per-file JSON depth cap 100. Files
//     larger than 10 MiB are skipped with a warning surfaced via
//     Entry.ParseError; depth violations parse-fail the same way.
//   - Per-file errors NEVER abort List. They are surfaced via
//     Entry.ParseError and the caller (TUI / doctor) decides what to
//     show.
//   - Resurrect rewrites snapshot files via non-atomic io.open(path,
//     "w+"). A read concurrent with a write can hit a torn JSON
//     prefix. We retry parse 3× with 25 ms backoff; on continued
//     failure we surface ParseError and move on.
//   - Encoded filename: name:gsub("/", "+"). Must match the Lua side.
package snapshots

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Grady-Saccullo/wezsesh/internal/logger"
	"github.com/Grady-Saccullo/wezsesh/internal/nameval"
	"github.com/Grady-Saccullo/wezsesh/internal/safefs"
)

// Caps from §8.10.
const (
	// MaxFileSize is the hard cap on a single snapshot file. Realistic
	// snapshots are <100 KiB; 10 MiB is generous and bounds the OOM
	// blast radius from a corrupted or maliciously-large file.
	MaxFileSize = 10 * 1024 * 1024 // 10 MiB

	// MaxJSONDepth defends against pathological JSON nesting. Counted
	// via depthCountReader as `{`/`[` minus `}`/`]` while feeding the
	// json.Decoder.
	MaxJSONDepth = 100
)

// Resurrect race retry knobs (§17.3 row "Resurrect race"). 3 attempts
// with 25 ms backoff is enough to survive a typical periodic_save
// rewrite; the writer's effective window is on the order of a few
// milliseconds even on slow disks.
const (
	parseRetryAttempts = 3
	parseRetryBackoff  = 25 * time.Millisecond
)

// Repo binds to <snapshotDir>/workspace/. NewRepo verifies the dir but
// does NOT scan files; the first List call performs the directory scan.
type Repo struct {
	snapshotDir  string // <snapshotDir>
	workspaceDir string // <snapshotDir>/workspace
	log          *logger.Logger

	mu        sync.Mutex
	hashCache map[string]string // encoded basename → "sha256:<hex>"
}

// NewRepo binds to snapshotDir/workspace. snapshotDir is the resolved
// directory per §11.4 (caller supplies cfg.SnapshotDir). The workspace
// subdir is created with 0700 if missing — resurrect uses the same
// path and may not have created it yet.
func NewRepo(snapshotDir string, log *logger.Logger) (*Repo, error) {
	if snapshotDir == "" {
		return nil, errors.New("snapshots: NewRepo: empty snapshotDir")
	}
	if _, err := safefs.Enforce(snapshotDir, safefs.SymlinkRefuse, log); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		return nil, fmt.Errorf("snapshots: mkdir %s: %w", snapshotDir, err)
	}

	workspaceDir := filepath.Join(snapshotDir, "workspace")
	if err := os.MkdirAll(workspaceDir, 0o700); err != nil {
		return nil, fmt.Errorf("snapshots: mkdir %s: %w", workspaceDir, err)
	}
	if _, err := safefs.Enforce(workspaceDir, safefs.SymlinkRefuse, log); err != nil {
		return nil, err
	}

	// Pre-create the sidecar sentinel so concurrent WriteSidecar calls
	// hit a stable inode for the parent-dir lock. AcquireExclusiveOrCreate
	// is the formal contract, but seeding the file at bind time avoids
	// racing creates under load and keeps the first-writer path identical
	// to subsequent ones. The sentinel is left as an empty 0600 file.
	sentinelPath := filepath.Join(workspaceDir, sidecarLockSentinel)
	if err := ensureSidecarSentinel(sentinelPath); err != nil {
		return nil, err
	}

	return &Repo{
		snapshotDir:  snapshotDir,
		workspaceDir: workspaceDir,
		log:          log,
		hashCache:    make(map[string]string),
	}, nil
}

// ensureSidecarSentinel creates the sentinel file at path if missing.
// Symlink at the path is refused (defense in depth — a tampered
// snapshot dir cannot redirect our advisory lock to an external
// inode).
func ensureSidecarSentinel(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("snapshots: sidecar sentinel %s: %w", path, safefs.ErrIsSymlink)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("snapshots: stat sentinel %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		// Lost a create race with another binary; that's fine — the
		// inode now exists.
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return fmt.Errorf("snapshots: create sentinel %s: %w", path, err)
	}
	return f.Close()
}

// SnapshotDir returns the bound snapshot directory.
func (r *Repo) SnapshotDir() string { return r.snapshotDir }

// workspacePath returns the absolute path to the .json snapshot file
// for `name`. Caller-facing API uses the user-visible name; encoding
// is applied here.
//
// `name` is assumed to be NFC-normalised already; by-name accessors at
// the public boundary (Has / ReadAll / Hash / RawHashHex / Sniff /
// Delete / Rename / SidecarPath / WriteSidecar) call nameval.NormalizeNFC
// before reaching this helper, so a caller passing the same logical
// name in NFD form resolves to the same on-disk file (§15.1: NFC at
// storage/comparison boundaries).
func (r *Repo) workspacePath(name string) string {
	return filepath.Join(r.workspaceDir, EncodeName(name)+".json")
}

// EncodeName replaces '/' with '+' to match resurrect's filename
// transform (resurrect/save_state.lua applies the same gsub). NOT
// bijective for names containing literal '+': the TUI surfaces a UI
// warning when a save/rename produces a collision.
func EncodeName(name string) string {
	return strings.ReplaceAll(name, "/", "+")
}

// DecodeName reverses EncodeName. Same caveat: '+' in the encoded form
// is converted to '/', which may collide with literal '+' in the
// original name.
func DecodeName(encoded string) string {
	return strings.ReplaceAll(encoded, "+", "/")
}

// Entry is one row in the snapshot list. Hashes are lazy; State is nil
// when the file is encrypted, oversized, parse-failed, or otherwise
// unreadable.
type Entry struct {
	Name       string
	Path       string
	Mtime      time.Time
	Size       int64
	Encryption Encryption
	State      *WorkspaceState
	SidecarOK  bool
	Sidecar    Sidecar
	ParseError error
	HashLazy   func(ctx context.Context) (string, error)
}

// List performs the directory scan and returns one Entry per snapshot
// file. ctx wraps the scan; per-file errors are surfaced via
// Entry.ParseError and never propagated.
func (r *Repo) List(ctx context.Context) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dents, err := os.ReadDir(r.workspaceDir)
	if err != nil {
		// The dir is created in NewRepo; a missing dir here means a
		// race (e.g., user reset). Return empty rather than error.
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("snapshots: read workspace dir: %w", err)
	}

	out := make([]Entry, 0, len(dents))
	for _, d := range dents {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		nm := d.Name()
		if !strings.HasSuffix(nm, ".json") {
			continue
		}
		// Skip sidecars — they're a different shape and own a separate
		// list channel. The check covers both `<encoded>.wezsesh.json`
		// and any `.wezsesh.json.v<N>.bak` we leave behind on
		// migration. (`.bak` doesn't end in .json so it's already
		// excluded; the wezsesh.json check handles the active sidecar.)
		if strings.HasSuffix(nm, sidecarSuffix) {
			continue
		}
		// Symlink defense: skip per-file symlinks rather than refuse
		// the entire list (SkipWarn semantics).
		full := filepath.Join(r.workspaceDir, nm)
		ok, err := safefs.Enforce(full, safefs.SymlinkSkipWarn, r.log)
		if err != nil {
			// A non-symlink Lstat failure surfaces as a per-entry
			// ParseError; don't kill the list.
			out = append(out, Entry{
				Name:       DecodeName(strings.TrimSuffix(nm, ".json")),
				Path:       full,
				ParseError: err,
			})
			continue
		}
		if !ok {
			continue
		}

		info, statErr := d.Info()
		if statErr != nil {
			out = append(out, Entry{
				Name:       DecodeName(strings.TrimSuffix(nm, ".json")),
				Path:       full,
				ParseError: statErr,
			})
			continue
		}

		decoded := nameval.NormalizeNFC(DecodeName(strings.TrimSuffix(nm, ".json")))

		ent := Entry{
			Name:  decoded,
			Path:  full,
			Mtime: info.ModTime(),
			Size:  info.Size(),
		}

		// Capture the encoded basename for closure stability.
		basename := nm
		ent.HashLazy = r.makeHashLazy(basename, full)

		// Size cap. Files > MaxFileSize are not parsed; they get
		// ParseError set so the TUI / doctor can show them.
		if info.Size() > MaxFileSize {
			ent.ParseError = fmt.Errorf("snapshots: file exceeds %d-byte cap (size=%d)", int64(MaxFileSize), info.Size())
			if r.log != nil {
				r.log.Warn("snapshots: file exceeds size cap",
					"path", full, "size", info.Size(), "cap", int64(MaxFileSize))
			}
			out = append(out, ent)
			continue
		}

		enc, state, parseErr := r.readAndParseWithRetry(ctx, full)
		ent.Encryption = enc
		ent.State = state
		ent.ParseError = parseErr

		s, ok, sErr := r.ReadSidecar(ctx, decoded)
		if sErr == nil && ok {
			ent.Sidecar = s
			ent.SidecarOK = true
		}

		out = append(out, ent)
	}
	return out, nil
}

// Has returns true iff the snapshot file exists on disk for `name`.
// Sidecar presence is irrelevant for Has.
//
// Symlinks at the snapshot path are treated as absent (§8.1.1 SkipWarn):
// safefs.Enforce emits the structured warn, the caller sees Has=false.
// The same policy enum is used by List for consistency.
func (r *Repo) Has(_ context.Context, name string) (bool, error) {
	path := r.workspacePath(nameval.NormalizeNFC(name))
	ok, err := safefs.Enforce(path, safefs.SymlinkSkipWarn, r.log)
	if err != nil {
		return false, err
	}
	if !ok {
		// Symlink → SkipWarn already logged; treat as absent.
		return false, nil
	}
	// Enforce treats missing path as ok=true — confirm existence.
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("snapshots: stat %s: %w", path, err)
	}
	return true, nil
}

// ReadAll returns the raw on-disk bytes of the snapshot file. Used by
// the save flow's hash compare (over raw bytes — encryption-agnostic)
// and by the load path before handing to resurrect.
func (r *Repo) ReadAll(ctx context.Context, name string) ([]byte, error) {
	path := r.workspacePath(nameval.NormalizeNFC(name))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := safefs.Enforce(path, safefs.SymlinkRejectOp, r.log); err != nil {
		return nil, err
	}
	f, err := safefs.SafeOpenForRead(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("snapshots: stat %s: %w", path, err)
	}
	if st.Size() > MaxFileSize {
		return nil, fmt.Errorf("snapshots: ReadAll %s: file exceeds %d-byte cap (size=%d)", path, int64(MaxFileSize), st.Size())
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("snapshots: ReadAll %s: %w", path, err)
	}
	return data, nil
}

// Hash returns the prefixed digest "sha256:<hex>" of the raw on-disk
// bytes of the snapshot file. Memoised on a per-Repo basis until the
// next process exit.
func (r *Repo) Hash(ctx context.Context, name string) (string, error) {
	name = nameval.NormalizeNFC(name)
	encoded := EncodeName(name) + ".json"
	full := filepath.Join(r.workspaceDir, encoded)
	return r.computeAndCacheHash(ctx, encoded, full, true)
}

// RawHashHex returns the bare hex (no "sha256:" prefix) for callers
// that need it (trust hash preimages per §8.12). Same memoisation as
// Hash; the cache stores the prefixed form and we strip on return.
// Hash normalises the name to NFC; we delegate without re-normalising.
func (r *Repo) RawHashHex(ctx context.Context, name string) (string, error) {
	prefixed, err := r.Hash(ctx, name)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(prefixed, "sha256:"), nil
}

// Sniff returns the encryption classification per Appendix B without
// reading the entire file.
func (r *Repo) Sniff(ctx context.Context, name string) (Encryption, error) {
	return sniffFile(ctx, r.workspacePath(nameval.NormalizeNFC(name)))
}

// Delete removes both the .json snapshot file and the .wezsesh.json
// sidecar. Each is acquired exclusively for the duration of its own
// removal; the two locks are independent.
//
// Deletion errors are surfaced as soon as the snapshot file removal
// fails. Sidecar removal is best-effort: a missing or unreadable
// sidecar must not block snapshot deletion.
func (r *Repo) Delete(ctx context.Context, name string) error {
	name = nameval.NormalizeNFC(name)
	snapPath := r.workspacePath(name)
	sidePath := r.SidecarPath(name)

	// Snapshot file: hold lock briefly to serialise against a
	// concurrent saver. ENOENT means already gone — no error.
	if err := r.deleteWithLock(ctx, snapPath); err != nil {
		return err
	}

	// Sidecar: best-effort. We don't fail the snapshot delete on a
	// sidecar issue.
	if err := r.deleteWithLock(ctx, sidePath); err != nil && !errors.Is(err, safefs.ErrNotExist) && !errors.Is(err, os.ErrNotExist) {
		if r.log != nil {
			r.log.Warn("snapshots: sidecar delete failed",
				"path", sidePath, "err", err.Error())
		}
	}

	// Drop the hash cache entry — the file is gone, the digest is
	// stale.
	r.mu.Lock()
	delete(r.hashCache, EncodeName(name)+".json")
	r.mu.Unlock()

	return nil
}

// Rename renames both the snapshot and sidecar files. Each acquires
// its own exclusive lock on the source path.
//
// Atomicity caveat: the two os.Rename calls are independent, so a
// crash between them leaves the snapshot at the new name and the
// sidecar at the old. The caller (TUI rename flow) re-runs Rename on
// the next attempt; the sidecar tail-rename is idempotent because the
// new-name sidecar will already exist after the second attempt.
func (r *Repo) Rename(ctx context.Context, oldName, newName string) error {
	oldName = nameval.NormalizeNFC(oldName)
	newName = nameval.NormalizeNFC(newName)
	if oldName == newName {
		return nil
	}
	oldSnap := r.workspacePath(oldName)
	newSnap := r.workspacePath(newName)
	oldSide := r.SidecarPath(oldName)
	newSide := r.SidecarPath(newName)

	if err := r.renameWithLock(ctx, oldSnap, newSnap); err != nil {
		return err
	}
	// Sidecar: best-effort. If the source doesn't exist, there's
	// nothing to rename.
	if err := r.renameWithLock(ctx, oldSide, newSide); err != nil && !errors.Is(err, safefs.ErrNotExist) && !errors.Is(err, os.ErrNotExist) {
		if r.log != nil {
			r.log.Warn("snapshots: sidecar rename failed",
				"old", oldSide, "new", newSide, "err", err.Error())
		}
	}

	r.mu.Lock()
	delete(r.hashCache, EncodeName(oldName)+".json")
	delete(r.hashCache, EncodeName(newName)+".json")
	r.mu.Unlock()
	return nil
}

// deleteWithLock acquires an exclusive lock on path and unlinks it.
// Missing files are not an error.
func (r *Repo) deleteWithLock(ctx context.Context, path string) error {
	_, release, err := safefs.AcquireExclusive(ctx, path)
	if err != nil {
		if errors.Is(err, safefs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer release()
	if err := safefs.SafeRemove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("snapshots: delete %s: %w", path, err)
	}
	return nil
}

// renameWithLock acquires an exclusive lock on the source and renames
// to dst. Missing source is treated as ErrNotExist for the caller to
// decide.
func (r *Repo) renameWithLock(ctx context.Context, src, dst string) error {
	_, release, err := safefs.AcquireExclusive(ctx, src)
	if err != nil {
		return err
	}
	defer release()
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("snapshots: rename %s -> %s: %w", src, dst, err)
	}
	return nil
}

// makeHashLazy returns the closure stored on Entry.HashLazy. Calling
// it repeatedly on the same Repo returns the cached digest after the
// first computation.
func (r *Repo) makeHashLazy(encodedBase, full string) func(ctx context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		return r.computeAndCacheHash(ctx, encodedBase, full, true)
	}
}

// computeAndCacheHash hashes the full path, caches under the encoded
// basename, and returns the prefixed form. If `prefixed` is true the
// returned string has the "sha256:" prefix; the cache always stores
// the prefixed form.
func (r *Repo) computeAndCacheHash(ctx context.Context, encodedBase, full string, prefixed bool) (string, error) {
	r.mu.Lock()
	if h, ok := r.hashCache[encodedBase]; ok {
		r.mu.Unlock()
		if prefixed {
			return h, nil
		}
		return strings.TrimPrefix(h, "sha256:"), nil
	}
	r.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return "", err
	}
	if _, err := safefs.Enforce(full, safefs.SymlinkRejectOp, r.log); err != nil {
		return "", err
	}
	f, err := safefs.SafeOpenForRead(full)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	// Stream-hash to avoid materialising large files.
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("snapshots: hash %s: %w", full, err)
	}
	digest := "sha256:" + hex.EncodeToString(h.Sum(nil))

	r.mu.Lock()
	r.hashCache[encodedBase] = digest
	r.mu.Unlock()

	if prefixed {
		return digest, nil
	}
	return strings.TrimPrefix(digest, "sha256:"), nil
}

// testParseRetryHook is a test-only seam fired immediately before each
// parse attempt inside readAndParseWithRetry. It receives the
// zero-based attempt index and the file path under read. Tests use it
// to stage a torn-then-valid file deterministically without sleeping
// (see TestList_ResurrectRaceRetry). nil in production.
var testParseRetryHook func(attempt int, path string)

// readAndParseWithRetry handles the resurrect-race retry. On a parse
// error, we retry up to parseRetryAttempts times with parseRetryBackoff
// between attempts; this absorbs a torn-write window of ~75 ms which
// is comfortably above resurrect's typical io.open(w+) duration.
//
// Returns (encryption, state, parseError). state is nil if encryption
// is not plaintext or if parsing failed.
func (r *Repo) readAndParseWithRetry(ctx context.Context, path string) (Encryption, *WorkspaceState, error) {
	var lastErr error
	for attempt := 0; attempt < parseRetryAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return EncryptionUnknown, nil, err
		}
		if testParseRetryHook != nil {
			testParseRetryHook(attempt, path)
		}
		enc, state, err := r.readAndParseOnce(ctx, path)
		if err == nil {
			return enc, state, nil
		}
		// Encryption-classification failures (file open / read) are
		// retried too — a half-written file might be unreadable for a
		// blink — but only if the error is parse-shaped. A symlink
		// ErrIsSymlink is structural and won't change between
		// attempts.
		if errors.Is(err, safefs.ErrIsSymlink) || errors.Is(err, os.ErrNotExist) {
			return enc, nil, err
		}
		lastErr = err
		if attempt+1 < parseRetryAttempts {
			select {
			case <-ctx.Done():
				return EncryptionUnknown, nil, ctx.Err()
			case <-time.After(parseRetryBackoff):
			}
		}
	}
	if r.log != nil {
		r.log.Warn("snapshots: parse failed after retries",
			"path", path, "attempts", parseRetryAttempts, "err", lastErr.Error())
	}
	// Sniff for encryption classification using a fresh open so the
	// caller can degrade preview correctly even if parse never
	// succeeded.
	enc, _ := sniffFile(ctx, path)
	return enc, nil, lastErr
}

// readAndParseOnce performs the per-file read + sniff + parse. State
// is returned only when the file is plaintext-shaped; encrypted /
// unknown shapes return state=nil with no error.
func (r *Repo) readAndParseOnce(_ context.Context, path string) (Encryption, *WorkspaceState, error) {
	f, err := safefs.SafeOpenForRead(path)
	if err != nil {
		return EncryptionUnknown, nil, err
	}
	defer f.Close()

	// First 32 bytes for sniff; we then re-read from the start. The
	// double-read is cheap and avoids an awkward concat between the
	// sniff buffer and the body read.
	var head [32]byte
	n, _ := f.Read(head[:])
	enc := SniffBytes(head[:n])
	if enc != EncryptionPlaintext {
		// Encrypted / unknown — caller still gets the sniff result so
		// preview can degrade. No parse.
		return enc, nil, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return enc, nil, fmt.Errorf("snapshots: seek %s: %w", path, err)
	}
	st, err := f.Stat()
	if err != nil {
		return enc, nil, fmt.Errorf("snapshots: stat %s: %w", path, err)
	}
	if st.Size() > MaxFileSize {
		// Already filtered in List; defensive double-check for
		// callers that take a different path.
		return enc, nil, fmt.Errorf("snapshots: file exceeds %d-byte cap (size=%d)", int64(MaxFileSize), st.Size())
	}

	var ws WorkspaceState
	if err := decodeWithDepthCap(f, &ws); err != nil {
		return enc, nil, err
	}
	return enc, &ws, nil
}

// decodeWithDepthCap reads JSON from r into v while enforcing
// MaxJSONDepth. The wrapper counts `{`/`[` minus `}`/`]` byte-by-byte
// and aborts the decode if the running counter exceeds the cap.
//
// Counting happens only OUTSIDE of JSON strings, tracked via a tiny
// inline state machine so a `{` inside a string literal does not
// increment depth.
//
// json.Decoder wraps Read errors in its own error chain. We check the
// reader's own state flag (`dr.failed`) after Decode returns; that's
// stable regardless of how json.Decoder surfaces the error.
func decodeWithDepthCap(r io.Reader, v interface{}) error {
	dr := &depthCountReader{src: r, cap: MaxJSONDepth}
	dec := json.NewDecoder(dr)
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		if dr.failed {
			return fmt.Errorf("snapshots: JSON depth exceeds %d", MaxJSONDepth)
		}
		return fmt.Errorf("snapshots: decode: %w", err)
	}
	return nil
}

// errDepthExceeded is the sentinel returned from depthCountReader.Read
// when nesting passes MaxJSONDepth. json.Decoder wraps it in its own
// error type, so callers should consult dr.failed rather than
// errors.Is.
var errDepthExceeded = errors.New("snapshots: depth exceeded")

// depthCountReader wraps an io.Reader and tracks JSON nesting depth
// across reads. Strings (between unescaped `"`s) are skipped so a
// `{` inside a JSON string does not increment depth.
type depthCountReader struct {
	src        io.Reader
	cap        int
	depth      int
	inString   bool
	prevEscape bool
	failed     bool
}

func (d *depthCountReader) Read(p []byte) (int, error) {
	if d.failed {
		return 0, errDepthExceeded
	}
	n, err := d.src.Read(p)
	for i := 0; i < n; i++ {
		c := p[i]
		if d.inString {
			if d.prevEscape {
				d.prevEscape = false
				continue
			}
			switch c {
			case '\\':
				d.prevEscape = true
			case '"':
				d.inString = false
			}
			continue
		}
		switch c {
		case '"':
			d.inString = true
		case '{', '[':
			d.depth++
			if d.depth > d.cap {
				d.failed = true
				// Return what we've consumed so far plus the
				// sentinel; json.Decoder will surface a parse
				// error and our wrapper will swap in the
				// depth-specific message based on dr.failed.
				return i + 1, errDepthExceeded
			}
		case '}', ']':
			d.depth--
		}
	}
	return n, err
}

// WorkspaceState mirrors P §6.6 / §10.1. Every field is optional
// (Go pointers / nil-safe slices). Resurrect's README schema lags the
// implementation; treat the source as authoritative.
type WorkspaceState struct {
	Workspace    *string       `json:"workspace,omitempty"`
	WindowStates []WindowState `json:"window_states,omitempty"`
}

// WindowState carries per-window metadata. Title and Size are pointers
// so absent fields don't read as zero-valued.
type WindowState struct {
	Title *string    `json:"title,omitempty"`
	Tabs  []TabState `json:"tabs,omitempty"`
	Size  *Size      `json:"size,omitempty"`
}

// TabState is recursive via PaneTree.
type TabState struct {
	Title    *string   `json:"title,omitempty"`
	IsActive *bool     `json:"is_active,omitempty"`
	IsZoomed *bool     `json:"is_zoomed,omitempty"`
	PaneTree *PaneTree `json:"pane_tree,omitempty"`
}

// Size is the pixel/cell envelope a window restored at.
type Size struct {
	Rows        *int `json:"rows,omitempty"`
	Cols        *int `json:"cols,omitempty"`
	PixelWidth  *int `json:"pixel_width,omitempty"`
	PixelHeight *int `json:"pixel_height,omitempty"`
	DPI         *int `json:"dpi,omitempty"`
}

// PaneTree is the recursive pane split tree. CWD is "" when
// unavailable; Process / Text are pointer-shaped because resurrect
// only emits one of them depending on alt_screen_active.
type PaneTree struct {
	Left            *int           `json:"left,omitempty"`
	Top             *int           `json:"top,omitempty"`
	Height          *int           `json:"height,omitempty"`
	Width           *int           `json:"width,omitempty"`
	CWD             *string        `json:"cwd,omitempty"`
	Domain          *string        `json:"domain,omitempty"`
	Process         *LocalProcInfo `json:"process,omitempty"`
	Text            *string        `json:"text,omitempty"`
	AltScreenActive *bool          `json:"alt_screen_active,omitempty"`
	IsActive        *bool          `json:"is_active,omitempty"`
	IsZoomed        *bool          `json:"is_zoomed,omitempty"`
	Bottom          *PaneTree      `json:"bottom,omitempty"`
	Right           *PaneTree      `json:"right,omitempty"`
}

// LocalProcInfo is the shape resurrect's main branch emits. Pre-2024-08
// snapshots wrote the whole field as a bare string ("/bin/bash"); the
// custom unmarshaller below accepts both.
type LocalProcInfo struct {
	Name       *string  `json:"name,omitempty"`
	Argv       []string `json:"argv,omitempty"`
	CWD        *string  `json:"cwd,omitempty"`
	Executable *string  `json:"executable,omitempty"`

	// LegacyString holds the bare-string form for old snapshots.
	// Populated only when the JSON source was a string scalar; the
	// preview / argv-allow paths consult Argv first then fall back to
	// LegacyString.
	LegacyString *string `json:"-"`
}

// UnmarshalJSON accepts both:
//
//	"process": "/bin/bash"                     (legacy)
//	"process": {"name": "...", "argv": [...]}  (current)
//
// See PRD §6.6 parser policy. Either shape is recorded; neither is an
// error.
func (p *LocalProcInfo) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil
	}
	// Probe shape via the first non-whitespace byte. We avoid a full
	// json.Unmarshal-into-interface{} probe to keep the depth bounded
	// to the depthCountReader's accounting.
	start := 0
	for start < len(data) && (data[start] == ' ' || data[start] == '\t' || data[start] == '\n' || data[start] == '\r') {
		start++
	}
	if start >= len(data) {
		return nil
	}
	switch data[start] {
	case '"':
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf("LocalProcInfo: legacy string parse: %w", err)
		}
		p.LegacyString = &s
		return nil
	case '{':
		// Avoid recursing back into UnmarshalJSON via type alias.
		type rawProc LocalProcInfo
		var r rawProc
		if err := json.Unmarshal(data, &r); err != nil {
			return fmt.Errorf("LocalProcInfo: object parse: %w", err)
		}
		*p = LocalProcInfo(r)
		return nil
	}
	// Anything else (bool, number, null, array) is silently empty —
	// per PRD parser-tolerance policy.
	return nil
}

