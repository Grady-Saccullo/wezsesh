package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"

	"github.com/Grady-Saccullo/wezsesh/internal/logger"
)

// Env vars consulted by Load (§11.3 / §11.4).
const (
	envSnapshotDir = "WEZSESH_SNAPSHOT_DIR"
	envStateDir    = "WEZSESH_STATE_DIR"
	envRuntimeDir  = "WEZSESH_RUNTIME_DIR"
	envDataDir     = "WEZSESH_DATA_DIR"
	envLog         = "WEZSESH_LOG"
	envNoHooks     = "WEZSESH_NO_HOOKS"
)

// Load reads the config file at path (JSON), compiles each Exclude
// element, then applies the §11.4 env overrides. Regex compile failures
// are recorded in cfg.ExcludeErrors (the corresponding ExcludeCompiled
// slot is nil); they are NOT returned as errors — the runtime treats
// each invalid element as a no-op match (§17.3 row "Config Exclude
// invalid regex"). I/O and JSON parse failures ARE returned.
func Load(ctx context.Context, path string) (*Config, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if path == "" {
		return nil, errors.New("config: empty path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	compileExclude(&cfg)
	// §11.4 resolution table: env > file > auto-detect. Fill any
	// dir field that the file left empty with the §12.5 platform
	// default BEFORE env overrides — applyEnvOverrides runs last
	// so an env-set value still wins over both file-empty and the
	// just-filled autodetect default.
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("config: home dir: %w", err)
	}
	if err := fillAutoDetectDirs(&cfg, runtime.GOOS, os.Getenv, home, os.Getuid()); err != nil {
		return nil, err
	}
	applyEnvOverrides(&cfg, os.Getenv)
	return &cfg, nil
}

// LoadFromEnv produces the binary's *Config when invoked outside the
// plugin's TUI spawn path — `wezsesh doctor` / `list` / `find` /
// `trust` / `reset` / `tail` from a shell. AutoDetect populates the
// platform defaults; applyEnvOverrides honours WEZSESH_*_DIR and
// related env vars per §11.4 (env beats auto-detect for those fields,
// without it WEZSESH_NO_HOOKS=1 would silently no-op on
// `wezsesh doctor`).
//
// The TUI startup path uses LoadFromBootstrapData instead (the
// `bootstrap` IPC verb's reply); the env-var transport that used to
// supply the TUI cfg has been retired.
func LoadFromEnv(ctx context.Context) (*Config, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, err := AutoDetect()
	if err != nil {
		return nil, err
	}
	applyEnvOverrides(cfg, os.Getenv)
	return cfg, nil
}

// LoadFromBootstrapData turns the `data` map of a `bootstrap` IPC
// reply into a *Config. Re-marshals via encoding/json so the same
// struct-tag-driven unmarshal pipeline that handled the env-blob
// path now handles the IPC reply, then runs compileExclude +
// fillAutoDetectDirs + applyEnvOverrides for shape parity with
// LoadFromEnvJSONBase64. Returned cfg is ready for safefs enforce +
// repo / state / trust open.
func LoadFromBootstrapData(ctx context.Context, data map[string]any) (*Config, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if data == nil {
		return nil, errors.New("config: bootstrap data is nil")
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("config: bootstrap re-marshal: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("config: bootstrap unmarshal: %w", err)
	}
	compileExclude(&cfg)
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("config: home dir: %w", err)
	}
	if err := fillAutoDetectDirs(&cfg, runtime.GOOS, os.Getenv, home, os.Getuid()); err != nil {
		return nil, err
	}
	applyEnvOverrides(&cfg, os.Getenv)
	return &cfg, nil
}

// compileExclude walks cfg.Exclude and populates ExcludeCompiled +
// ExcludeErrors. Postcondition: len(ExcludeCompiled) == len(Exclude).
// An empty Exclude yields a non-nil zero-length ExcludeCompiled (matches
// the "Empty Exclude" gate).
func compileExclude(cfg *Config) {
	cfg.ExcludeCompiled = make([]*regexp.Regexp, len(cfg.Exclude))
	cfg.ExcludeErrors = nil
	for i, src := range cfg.Exclude {
		re, err := regexp.Compile(src)
		if err != nil {
			cfg.ExcludeCompiled[i] = nil
			cfg.ExcludeErrors = append(cfg.ExcludeErrors, ExcludeError{
				Index:  i,
				Source: src,
				Reason: err.Error(),
			})
			continue
		}
		cfg.ExcludeCompiled[i] = re
	}
}

// applyEnvOverrides realises the §11.4 resolution table. The env getter
// is parameterised so tests can drive it via t.Setenv (which mutates
// os.Getenv) OR via a fabricated lookup; the production callsite passes
// os.Getenv directly.
//
// log_level is the only field that does NOT use first-non-empty-wins;
// it routes through logger.ResolveLevel which picks the more verbose
// (lower numeric) of the two. Env can only make logging noisier, never
// quieter.
func applyEnvOverrides(cfg *Config, env func(string) string) {
	if v := env(envSnapshotDir); v != "" {
		cfg.SnapshotDir = v
	}
	if v := env(envStateDir); v != "" {
		cfg.StateDir = v
	}
	if v := env(envRuntimeDir); v != "" {
		cfg.RuntimeDir = v
	}
	if v := env(envDataDir); v != "" {
		cfg.DataDir = v
	}
	envLogVal := env(envLog)
	if envLogVal != "" || cfg.LogLevel != "" {
		cfg.LogLevel = levelString(logger.ResolveLevel(cfg.LogLevel, envLogVal))
	}
	if env(envNoHooks) == "1" {
		cfg.Hooks.RunHooks = false
	}
	// §8.19 prose: TrustDir is derived, never separately configurable.
	// Resolve at the END so env overrides of DataDir are reflected. Skip
	// when DataDir is empty: callers (e.g., Load before AutoDetect) can
	// then decide what to do with the unset value.
	if cfg.DataDir != "" {
		cfg.TrustDir = filepath.Join(cfg.DataDir, "allow")
	}
}

// levelString is the canonical lowercase encoding for a logger.Level —
// the inverse of logger.parseLevel for the four recognised levels. Kept
// local so internal/config does not depend on a string-export from the
// logger package.
func levelString(l logger.Level) string {
	switch l {
	case logger.LevelDebug:
		return "debug"
	case logger.LevelInfo:
		return "info"
	case logger.LevelWarn:
		return "warn"
	case logger.LevelError:
		return "error"
	}
	return "info"
}
