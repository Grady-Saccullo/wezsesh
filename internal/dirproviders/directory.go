package dirproviders

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Grady-Saccullo/wezsesh/internal/logger"
)

// directoryWalkCeiling bounds the number of FS entries the directory
// provider visits, regardless of cfg.Limit. Acts as a runaway guard:
// even with limit=0 (unlimited rows) and depth=10 we won't traverse
// more than this many entries.
const directoryWalkCeiling = 50_000

// runDirectory walks cfg.Path to cfg.Depth using filepath.WalkDir.
// Each visited directory becomes one row (subject to validatePath +
// include_hidden). Depth=1 means "the path itself"; depth=2 means
// "the path and its immediate children"; etc.
func runDirectory(cfg *Config, log *logger.Logger) ([]ExternalRow, error) {
	if cfg.Type != TypeDirectory {
		return nil, fmt.Errorf("dirproviders: runDirectory: type %q", cfg.Type)
	}

	root, reason, ok := validatePath(cfg.Path)
	if !ok {
		return nil, fmt.Errorf("directory.path %q invalid: %s", cfg.Path, reason)
	}
	rootDepth := strings.Count(filepath.Clean(root), string(os.PathSeparator))

	rows := make([]ExternalRow, 0, 64)
	visited := 0
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		visited++
		if visited > directoryWalkCeiling {
			return filepath.SkipAll
		}
		if err != nil {
			// Permission-denied at a child: log warn and skip the
			// subtree. Don't fail the whole walk for one unreadable
			// directory.
			if log != nil {
				log.Warn("dirproviders: directory walk error",
					"path", path, "err", err.Error())
			}
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		// Depth check. depth=1 admits the root only; depth=2 admits
		// the root plus immediate children; etc.
		curDepth := strings.Count(filepath.Clean(path), string(os.PathSeparator)) - rootDepth + 1
		if curDepth > cfg.Depth {
			return filepath.SkipDir
		}
		// Hidden-dir handling. We test the basename for a leading
		// dot. Skip both the entry and (if it's a dir) its subtree
		// when include_hidden is false.
		if !cfg.IncludeHidden && curDepth > 1 {
			base := filepath.Base(path)
			if strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
		}
		if cfg.Limit > 0 && len(rows) >= cfg.Limit {
			return filepath.SkipAll
		}
		rows = append(rows, ExternalRow{
			Path: path,
			Name: filepath.Base(path),
		})
		return nil
	})
	if walkErr != nil && walkErr != filepath.SkipAll {
		// Log but still return the rows we collected.
		if log != nil {
			log.Warn("dirproviders: directory walk terminated",
				"path", root, "err", walkErr.Error())
		}
	}
	return rows, nil
}
