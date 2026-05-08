package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// AutoDetect returns a *Config populated with §11 defaults plus the
// platform-specific dir paths defined in §12.5 / §12.2. Used when the
// binary is invoked outside its plugin context (no
// $WEZSESH_CONFIG_JSON_BASE64) — e.g., `wezsesh doctor` from a shell.
func AutoDetect() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("config: home dir: %w", err)
	}
	return autoDetectFor(runtime.GOOS, os.Getenv, home, os.Getuid())
}

// autoDetectFor is the parameterised inner. Tests exercise both
// goos="linux" and goos="darwin" against fabricated env lookups + $HOME
// + uid; production calls it via AutoDetect with the real values.
func autoDetectFor(goos string, env func(string) string, home string, uid int) (*Config, error) {
	if home == "" {
		return nil, errors.New("config: empty home dir")
	}
	cfg := defaultConfig()

	// All four dirs start empty in defaultConfig(), so this fills every
	// one — same path as Load's post-Unmarshal fallthrough (§11.4).
	if err := fillAutoDetectDirs(cfg, goos, env, home, uid); err != nil {
		return nil, err
	}

	// Compile the default Exclude so callers see ExcludeCompiled
	// populated identically to a Load of an on-disk file. (Empty errors
	// list; one valid compiled regex.)
	compileExclude(cfg)
	return cfg, nil
}

// fillAutoDetectDirs populates any of {SnapshotDir, StateDir, RuntimeDir,
// DataDir} that are empty with the §12.5 / §12.2 platform default; non-empty
// values are left untouched. TrustDir is re-derived from DataDir afterwards
// so callers get a consistent value regardless of which fields the file
// supplied. §11.4 resolution-table semantics ("env > file > auto-detect")
// are preserved when this runs BEFORE applyEnvOverrides — the env pass
// then overwrites whatever was filled here.
func fillAutoDetectDirs(cfg *Config, goos string, env func(string) string, home string, uid int) error {
	if home == "" {
		return errors.New("config: empty home dir")
	}
	if cfg.SnapshotDir == "" {
		cfg.SnapshotDir = autoSnapshotDir(goos, env, home)
	}
	if cfg.StateDir == "" {
		cfg.StateDir = autoStateDir(goos, env, home)
	}
	if cfg.RuntimeDir == "" {
		cfg.RuntimeDir = autoRuntimeDir(goos, env, uid)
	}
	if cfg.DataDir == "" {
		cfg.DataDir = autoDataDir(goos, env, home)
	}
	// TrustDir is derived (§8.19): never auto-detected separately.
	cfg.TrustDir = filepath.Join(cfg.DataDir, "allow")
	return nil
}

// xdgStateHome returns $XDG_STATE_HOME or its default <home>/.local/state.
func xdgStateHome(env func(string) string, home string) string {
	if v := env("XDG_STATE_HOME"); v != "" {
		return v
	}
	return filepath.Join(home, ".local", "state")
}

// xdgDataHome returns $XDG_DATA_HOME or its default <home>/.local/share.
func xdgDataHome(env func(string) string, home string) string {
	if v := env("XDG_DATA_HOME"); v != "" {
		return v
	}
	return filepath.Join(home, ".local", "share")
}

func autoSnapshotDir(goos string, env func(string) string, home string) string {
	switch goos {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "wezterm", "resurrect") + string(filepath.Separator)
	default: // linux + other unix
		return filepath.Join(xdgStateHome(env, home), "wezterm", "resurrect") + string(filepath.Separator)
	}
}

func autoStateDir(goos string, env func(string) string, home string) string {
	// §12.5 explicitly: darwin uses XDG semantics for state_dir too —
	// <home>/.local/state/wezsesh/ on both platforms.
	_ = goos
	return filepath.Join(xdgStateHome(env, home), "wezsesh") + string(filepath.Separator)
}

func autoDataDir(goos string, env func(string) string, home string) string {
	// §12.5 explicitly: darwin uses XDG semantics for data_dir too —
	// <home>/.local/share/wezsesh/ on both platforms.
	_ = goos
	return filepath.Join(xdgDataHome(env, home), "wezsesh") + string(filepath.Separator)
}

func autoRuntimeDir(goos string, env func(string) string, uid int) string {
	if goos == "linux" {
		if v := env("XDG_RUNTIME_DIR"); v != "" {
			return filepath.Join(v, "wezsesh") + string(filepath.Separator)
		}
	}
	// darwin always; Linux fallback (§12.2).
	return filepath.Join("/tmp", fmt.Sprintf("wezsesh-%d", uid)) + string(filepath.Separator)
}

// defaultConfig builds a Config with every §11 default filled in
// (everything except the platform-derived dirs). Callers populate the
// dirs and then call compileExclude.
func defaultConfig() *Config {
	cfg := &Config{
		Version:                   1,
		LogLevel:                  "info",
		Sort:                      "live_first",
		DefaultAction:             "switch",
		DefaultActionLoadNoPrompt: false,
		ConfirmDelete:             true,
		ConfirmOverwrite:          true,
		Exclude:                   []string{"^default$"},
		Markers: Markers{
			Active:  "▶",
			Live:    "●",
			Marked:  "✓",
			Unsaved: "(unsaved)",
			Pinned:  "[pinned]",
		},
		Columns:                []string{"marker", "name", "tabs", "age", "tags"},
		NameTruncate:           "middle",
		ResurrectArgvAllowlist: []string{},
		Keys:                   defaultKeyMap(),
		PluginVersion:          "0.1.0",
		ProtoVersion:           1,
	}
	cfg.Preview.Enabled = true
	cfg.Preview.Width = 0.4
	cfg.Hooks.RunHooks = true
	cfg.Hooks.PromptOnUntrusted = false
	cfg.Hooks.TimeoutSeconds = 600
	return cfg
}

// defaultKeyMap mirrors §11.1 verbatim. T-701 (TUI) consumes this; we
// populate it eagerly so the binary has a usable default in the no-config
// auto-detect path.
func defaultKeyMap() KeyMap {
	return KeyMap{
		Switch:     "s",
		Load:       "l",
		Rename:     "r",
		Delete:     "d",
		Save:       "S",
		New:        "n",
		Pin:        "p",
		Tag:        "t",
		Mark:       "Tab",
		MarkAlt:    "Space",
		ClearMarks: "c",
		Help:       "?",
		Filter:     "/",
		Quit:       "q",
		Up:         "k",
		Down:       "j",
		Top:        "gg",
		Bottom:     "G",
	}
}

