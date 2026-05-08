package config

import (
	"context"
	"encoding/base64"
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
	envSnapshotDir      = "WEZSESH_SNAPSHOT_DIR"
	envStateDir         = "WEZSESH_STATE_DIR"
	envRuntimeDir       = "WEZSESH_RUNTIME_DIR"
	envDataDir          = "WEZSESH_DATA_DIR"
	envLog              = "WEZSESH_LOG"
	envNoHooks          = "WEZSESH_NO_HOOKS"
	envConfigJSONBase64 = "WEZSESH_CONFIG_JSON_BASE64"
)

// ErrNoConfigEnv is returned by LoadFromEnvJSONBase64 when the env var
// is unset / empty. Callers (cmd/wezsesh:tuiSetup) decide whether to
// error out or fall through to AutoDetect on this sentinel.
var ErrNoConfigEnv = errors.New("config: WEZSESH_CONFIG_JSON_BASE64 not set")

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

// LoadFromEnv consults $WEZSESH_CONFIG_JSON_BASE64 first; when that env
// var carries a base64-encoded JSON config (the wezterm plugin's spawn
// path), decode + parse it. Otherwise fall back to AutoDetect (the
// binary was invoked outside its plugin context — `wezsesh doctor` from
// a shell, etc.; §12.5 covers that path). The previous
// $WEZSESH_CONFIG_FILE tmp-file route is gone — that handoff was a
// CVE-class TOCTOU surface (predictable filename, mode-0644 leak).
//
// §11.4 says env beats both the loaded config and auto-detect, so the
// env-override pass runs on the AutoDetect result too — without it,
// e.g. WEZSESH_NO_HOOKS=1 silently no-ops on `wezsesh doctor`.
func LoadFromEnv(ctx context.Context) (*Config, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, err := LoadFromEnvJSONBase64(ctx)
	if err == nil {
		return cfg, nil
	}
	if !errors.Is(err, ErrNoConfigEnv) {
		return nil, err
	}
	cfg, err = AutoDetect()
	if err != nil {
		return nil, err
	}
	applyEnvOverrides(cfg, os.Getenv)
	return cfg, nil
}

// LoadFromEnvJSONBase64 reads $WEZSESH_CONFIG_JSON_BASE64, base64-decodes
// the bytes, JSON-unmarshals into a Config, and applies the §11.4 env
// override pass. Returns ErrNoConfigEnv when the env var is unset/empty
// so callers can decide whether to fall back to AutoDetect (the doctor /
// list / find / trust / reset / shell-invoked path) or error out (the
// TUI startup path which requires the plugin to have spawned us).
func LoadFromEnvJSONBase64(ctx context.Context) (*Config, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw := os.Getenv(envConfigJSONBase64)
	if raw == "" {
		return nil, ErrNoConfigEnv
	}
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("config: %s: base64 decode: %w", envConfigJSONBase64, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: %s: parse: %w", envConfigJSONBase64, err)
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
