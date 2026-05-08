package dirproviders

import (
	"fmt"
	"path/filepath"

	"github.com/Grady-Saccullo/wezsesh/internal/logger"
)

// runStatic surfaces a literal list of paths after running each
// through validatePath. Invalid entries log warn and are dropped;
// valid entries are returned in input order.
func runStatic(cfg *Config, log *logger.Logger) ([]ExternalRow, error) {
	if cfg.Type != TypeStatic {
		return nil, fmt.Errorf("dirproviders: runStatic: type %q", cfg.Type)
	}
	out := make([]ExternalRow, 0, len(cfg.Paths))
	for _, raw := range cfg.Paths {
		path, reason, ok := validatePath(raw)
		if !ok {
			if log != nil {
				log.Warn("dirproviders: static entry rejected",
					"path", raw, "reason", reason)
			}
			continue
		}
		out = append(out, ExternalRow{
			Path: path,
			Name: filepath.Base(path),
		})
	}
	return out, nil
}
