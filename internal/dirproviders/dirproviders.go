// Package dirproviders executes the user-supplied directory-row
// providers carried in the bootstrap-IPC reply. Each provider is one
// of three declarative types — `command` (shell out), `directory`
// (walk a tree), `static` (literal list) — and produces a slice of
// validated absolute directory paths that the TUI surfaces as
// SourceExternal rows in its picker.
//
// Replaces the prior Lua-side `runtime/dir_providers` module + the
// `list_dirs` IPC verb: providers are now data, not callables, and
// execution is Go-native (better tooling for shell-out, walking, and
// validation; the wezterm Lua sandbox has none of that).
//
// Failure isolation: every provider's run is wrapped so a single
// crashing or misbehaving provider can't block the others. A failed
// provider logs a warn and contributes zero rows; the next provider
// runs to completion.
package dirproviders

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/Grady-Saccullo/wezsesh/internal/logger"
)

// ProviderType discriminates Config's tagged-union body. Only the
// declared values pass validation; an unknown type surfaces as
// ErrUnknownType at decode time.
const (
	TypeCommand   = "command"
	TypeDirectory = "directory"
	TypeStatic    = "static"
)

// ErrUnknownType is returned by Config.Validate when `Type` is
// neither "command", "directory", nor "static".
var ErrUnknownType = errors.New("dirproviders: unknown type")

// ExternalRow is one directory entry surfaced to the TUI picker. The
// TUI's `applyExternalDirs` consumes a slice of these via the
// `dirProvidersResultMsg` arm in update.go.
type ExternalRow struct {
	// Path is the validated absolute directory path. Tilde-expanded,
	// stat'd, confirmed to be a directory.
	Path string
	// Name is the display name shown in the picker. Defaults to
	// filepath.Base(Path) when the provider does not supply one.
	Name string
}

// Config is the typed-union config for a directory-row provider.
// Fields outside the active `Type`'s subset are ignored at execution
// time (their presence is permitted on the wire but does nothing —
// keeps the JSON tolerant to plugin-side defaulting).
type Config struct {
	Type string `json:"type"`

	// command-type fields.
	Argv      []string `json:"argv,omitempty"`
	Limit     int      `json:"limit,omitempty"`
	TimeoutMs int      `json:"timeout_ms,omitempty"`

	// directory-type fields.
	Path          string `json:"path,omitempty"`
	Depth         int    `json:"depth,omitempty"`
	IncludeHidden bool   `json:"include_hidden,omitempty"`

	// static-type fields.
	Paths []string `json:"paths,omitempty"`
}

// Validate enforces the per-type required-field set and clamps the
// numeric fields to documented ranges. Defaults for omitted optional
// fields are applied here so per-executor code can rely on the
// fully-populated Config.
func (c *Config) Validate() error {
	switch c.Type {
	case TypeCommand:
		if len(c.Argv) == 0 {
			return errors.New("dirproviders: command.argv must be non-empty")
		}
		for i, a := range c.Argv {
			if a == "" {
				return fmt.Errorf("dirproviders: command.argv[%d] is empty", i)
			}
			if strings.IndexByte(a, 0) >= 0 {
				return fmt.Errorf("dirproviders: command.argv[%d] contains NUL", i)
			}
		}
		if c.Limit < 0 {
			return fmt.Errorf("dirproviders: command.limit must be >= 0, got %d", c.Limit)
		}
		if c.Limit == 0 {
			c.Limit = 200
		}
		if c.TimeoutMs == 0 {
			c.TimeoutMs = 5000
		}
		if c.TimeoutMs < 100 || c.TimeoutMs > 60000 {
			return fmt.Errorf("dirproviders: command.timeout_ms must be 100..60000, got %d", c.TimeoutMs)
		}
	case TypeDirectory:
		if c.Path == "" {
			return errors.New("dirproviders: directory.path must be non-empty")
		}
		if c.Depth == 0 {
			c.Depth = 2
		}
		if c.Depth < 1 || c.Depth > 10 {
			return fmt.Errorf("dirproviders: directory.depth must be 1..10, got %d", c.Depth)
		}
		if c.Limit < 0 {
			return fmt.Errorf("dirproviders: directory.limit must be >= 0, got %d", c.Limit)
		}
		if c.Limit == 0 {
			c.Limit = 200
		}
	case TypeStatic:
		if len(c.Paths) == 0 {
			return errors.New("dirproviders: static.paths must be non-empty")
		}
	default:
		return fmt.Errorf("%w: %q", ErrUnknownType, c.Type)
	}
	return nil
}

// RunAll executes every provider in `configs` sequentially and returns
// the union of validated rows. Per-provider failures are logged at
// warn (one record per failure) and contribute zero rows; the rest
// run to completion.
func RunAll(ctx context.Context, configs []Config, log *logger.Logger) []ExternalRow {
	if len(configs) == 0 {
		return nil
	}
	out := make([]ExternalRow, 0, 64)
	for i := range configs {
		cfg := configs[i] // local copy; Validate mutates defaults
		if err := cfg.Validate(); err != nil {
			if log != nil {
				log.Warn("dirproviders: invalid config",
					"index", i, "err", err.Error())
			}
			continue
		}
		rows, err := runOne(ctx, &cfg, log)
		if err != nil {
			if log != nil {
				log.Warn("dirproviders: provider failed",
					"index", i, "type", cfg.Type, "err", err.Error())
			}
			continue
		}
		out = append(out, rows...)
	}
	return out
}

func runOne(ctx context.Context, cfg *Config, log *logger.Logger) ([]ExternalRow, error) {
	switch cfg.Type {
	case TypeCommand:
		return runCommand(ctx, cfg, log)
	case TypeDirectory:
		return runDirectory(cfg, log)
	case TypeStatic:
		return runStatic(cfg, log)
	}
	return nil, fmt.Errorf("%w: %q", ErrUnknownType, cfg.Type)
}

// validatePath is the shared per-path validator. Mirrors
// pathpicker.validateLine but without the slog-direct logging — the
// caller decides where to surface skips. Returns (canonical, true)
// when the path is acceptable; (reason, false) otherwise.
func validatePath(raw string) (string, string, bool) {
	line := strings.TrimRight(raw, "\r")
	if strings.TrimSpace(line) == "" {
		return "", "blank line", false
	}
	if !utf8.ValidString(line) {
		return "", "invalid utf-8", false
	}
	if strings.IndexByte(line, 0) >= 0 {
		return "", "nul byte", false
	}
	expanded, ok := expandTilde(line)
	if !ok {
		return "", "tilde expansion failed", false
	}
	if !filepath.IsAbs(expanded) {
		return "", "not absolute", false
	}
	info, err := os.Stat(expanded)
	if err != nil {
		return "", "stat failed: " + err.Error(), false
	}
	if !info.IsDir() {
		return "", "not a directory", false
	}
	return expanded, "", true
}

// expandTilde handles `~` and `~/...`; rejects `~user/...`. Mirrors
// pathpicker.expandTilde without re-exporting the helper there.
func expandTilde(line string) (string, bool) {
	if line == "" || line[0] != '~' {
		return line, true
	}
	if line == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		return home, true
	}
	if line[1] != '/' {
		return "", false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(home, line[2:]), true
}
