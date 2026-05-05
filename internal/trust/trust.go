package trust

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/Grady-Saccullo/wezsesh/internal/logger"
	"github.com/Grady-Saccullo/wezsesh/internal/safefs"
)

const (
	trustDirMode  fs.FileMode = 0o700
	trustFileMode fs.FileMode = 0o600
)

// Entry is the public listing shape returned by List. Hash is the trust
// file's basename (the SHA-256). Path is the absolute sidecar path
// recorded in the file's JSON body — advisory only; the file *name* is
// the truth (§10.5).
type Entry struct {
	Hash string
	Path string
}

// Store is the binary-side handle to the trust directory. Construct via
// Open. Methods are safe for concurrent use within a process.
type Store struct {
	dir string
	log *logger.Logger
}

// fileBody is the JSON shape persisted at <trust_dir>/<hash> (§10.5).
type fileBody struct {
	Path string `json:"path"`
}

// Open verifies trustDir is not a symlink, MkdirAll's it at mode 0700 if
// missing, and returns a Store. log may be nil; symlink Skip-warn calls
// elsewhere will be silent in that case.
func Open(ctx context.Context, trustDir string, log *logger.Logger) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if trustDir == "" {
		return nil, errors.New("trust: empty trustDir")
	}
	// MkdirAll first so a brand-new install doesn't fail on the Enforce
	// call below. MkdirAll is idempotent and refuses to traverse the
	// final component if it exists as a non-directory; the subsequent
	// Enforce(SymlinkRefuse) catches the symlink case (MkdirAll happily
	// returns nil if the path is a symlink to a directory).
	if err := os.MkdirAll(trustDir, trustDirMode); err != nil {
		return nil, fmt.Errorf("trust: mkdir %s: %w", trustDir, err)
	}
	ok, err := safefs.Enforce(trustDir, safefs.SymlinkRefuse, log)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, safefs.ErrIsSymlink
	}
	info, err := os.Lstat(trustDir)
	if err != nil {
		return nil, fmt.Errorf("trust: lstat %s: %w", trustDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("trust: %s is not a directory", trustDir)
	}
	return &Store{dir: trustDir, log: log}, nil
}

// Approve writes the trust file <trust_dir>/<sha256(path, cmd)>. The body
// is `{"path":"<absSidecarPath>"}`. Idempotent: calling Approve twice with
// the same inputs replaces the file atomically.
func (s *Store) Approve(ctx context.Context, absSidecarPath string, commandBytes []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if absSidecarPath == "" {
		return errors.New("trust: Approve: empty absSidecarPath")
	}
	hash := ComputeHash(absSidecarPath, commandBytes)
	body, err := json.Marshal(fileBody{Path: absSidecarPath})
	if err != nil {
		return fmt.Errorf("trust: marshal body: %w", err)
	}
	return safefs.AtomicWriteFile(ctx, s.dir, hash, body, trustFileMode)
}

// IsApproved returns true iff the trust file exists and is a regular
// file (not a symlink). Default is fail-CLOSED: any error or unexpected
// mode returns false.
func (s *Store) IsApproved(ctx context.Context, absSidecarPath string, commandBytes []byte) bool {
	if ctx.Err() != nil {
		return false
	}
	if absSidecarPath == "" {
		return false
	}
	hash := ComputeHash(absSidecarPath, commandBytes)
	path := filepath.Join(s.dir, hash)
	// Use Lstat (NOT Stat) so a symlink at the trust file path is caught
	// before any read. Same-UID attackers cannot redirect approval via
	// symlink swap.
	ok, err := safefs.Enforce(path, safefs.SymlinkSkipWarn, s.log)
	if err != nil {
		return false
	}
	if !ok {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if !info.Mode().IsRegular() {
		return false
	}
	return true
}

// Revoke removes the trust file for (absSidecarPath, commandBytes). A
// missing trust file is NOT an error — Revoke is idempotent so callers
// can use it as a fail-safe sweep without first probing IsApproved.
func (s *Store) Revoke(ctx context.Context, absSidecarPath string, commandBytes []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if absSidecarPath == "" {
		return errors.New("trust: Revoke: empty absSidecarPath")
	}
	hash := ComputeHash(absSidecarPath, commandBytes)
	return s.removeByHash(hash)
}

// List enumerates the trust directory and returns one Entry per regular
// file whose name is a valid lowercase 64-char hex SHA-256. Symlinked
// entries are skip-warned (per §13.5 / §10.5: trust file symlinks are
// untrusted).
func (s *Store) List(ctx context.Context) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("trust: read dir: %w", err)
	}
	out := make([]Entry, 0, len(entries))
	for _, de := range entries {
		name := de.Name()
		if !looksLikeHash(name) {
			continue
		}
		full := filepath.Join(s.dir, name)
		ok, err := safefs.Enforce(full, safefs.SymlinkSkipWarn, s.log)
		if err != nil {
			// Lstat failure on a single entry: skip but continue —
			// listing is best-effort, fail-closed at the per-entry
			// level.
			continue
		}
		if !ok {
			continue
		}
		info, err := os.Lstat(full)
		if err != nil {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		path := readBodyPath(full)
		out = append(out, Entry{Hash: name, Path: path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hash < out[j].Hash })
	return out, nil
}

// Prune removes entries whose recorded path no longer exists on disk.
// Returns the count of removed entries. Symlinked or unparseable entries
// are left untouched so a manual investigation can find them.
func (s *Store) Prune(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	entries, err := s.List(ctx)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		if e.Path == "" {
			continue
		}
		if _, err := os.Lstat(e.Path); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				continue
			}
			if rmErr := s.removeByHash(e.Hash); rmErr == nil {
				removed++
			}
		}
	}
	return removed, nil
}

// removeByHash unlinks <trust_dir>/<hash>. Missing → nil (idempotent).
// Symlinked → ErrIsSymlink so the caller knows not to follow.
func (s *Store) removeByHash(hash string) error {
	if !looksLikeHash(hash) {
		return fmt.Errorf("trust: refusing to remove non-hash name %q", hash)
	}
	path := filepath.Join(s.dir, hash)
	if err := safefs.SafeRemove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("trust: remove %s: %w", hash, err)
	}
	return nil
}

// readBodyPath returns the recorded `path` field, or "" on any error.
// Best-effort: callers treat the path as advisory.
func readBodyPath(file string) string {
	f, err := safefs.SafeOpenForRead(file)
	if err != nil {
		return ""
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	var body fileBody
	if err := json.Unmarshal(data, &body); err != nil {
		return ""
	}
	return body.Path
}

// looksLikeHash returns true iff name is exactly 64 lowercase hex chars.
// Trust dir cleanup tools must not nuke unrelated files (.DS_Store etc).
func looksLikeHash(name string) bool {
	if len(name) != 64 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}
