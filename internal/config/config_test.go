package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// minimalConfigJSON returns the smallest §10.7-shaped JSON document that
// round-trips through Load cleanly. Tests build on this via overrides.
func minimalConfigJSON(t *testing.T, overrides map[string]any) []byte {
	t.Helper()
	doc := map[string]any{
		"version":                       1,
		"snapshot_dir":                  "/snap",
		"state_dir":                     "/state",
		"runtime_dir":                   "/run",
		"data_dir":                      "/data",
		"log_level":                     "info",
		"sort":                          "live_first",
		"default_action":                "switch",
		"default_action_load_no_prompt": false,
		"confirm_delete":                true,
		"confirm_overwrite":             true,
		"exclude":                       []string{"^default$"},
		"new_workspace_command":         nil,
		"preview":                       map[string]any{"enabled": true, "width": 0.4},
		"markers": map[string]any{
			"active":  "▶",
			"live":    "●",
			"marked":  "✓",
			"unsaved": "(unsaved)",
			"pinned":  "[pinned]",
		},
		"columns":                  []string{"marker", "name", "tabs", "age", "tags"},
		"name_truncate":            "middle",
		"colors":                   map[string]any{},
		"hooks":                    map[string]any{"run_hooks": true, "prompt_on_untrusted": false, "timeout_seconds": 600},
		"resurrect_argv_allowlist": []string{},
		"keys":                     map[string]any{},
		"plugin_version":           "0.1.0",
		"proto_version":            1,
	}
	for k, v := range overrides {
		doc[k] = v
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal config doc: %v", err)
	}
	return b
}

func writeConfigFile(t *testing.T, body []byte) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "wezsesh.json")
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
	return p
}

// clearConfigEnv strips all of Load's env knobs for the duration of the
// test, so a host-set var (e.g. someone exporting WEZSESH_LOG=debug)
// does not silently flip an assertion.
func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"WEZSESH_SNAPSHOT_DIR",
		"WEZSESH_STATE_DIR",
		"WEZSESH_RUNTIME_DIR",
		"WEZSESH_DATA_DIR",
		"WEZSESH_LOG",
		"WEZSESH_NO_HOOKS",
		"WEZSESH_CONFIG_FILE",
		"XDG_STATE_HOME",
		"XDG_DATA_HOME",
		"XDG_RUNTIME_DIR",
	} {
		t.Setenv(k, "")
	}
}

// §17.3 row "Config Exclude invalid regex".
func TestLoad_ExcludeInvalidRegex(t *testing.T) {
	clearConfigEnv(t)
	body := minimalConfigJSON(t, map[string]any{
		"exclude": []string{"^valid$", "[invalid", "^also_valid$"},
	})
	p := writeConfigFile(t, body)

	cfg, err := Load(context.Background(), p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := len(cfg.ExcludeCompiled), len(cfg.Exclude); got != want {
		t.Fatalf("len(ExcludeCompiled)=%d, want %d (parity with Exclude)", got, want)
	}
	if cfg.ExcludeCompiled[0] == nil {
		t.Errorf("ExcludeCompiled[0] is nil; expected a compiled regex for ^valid$")
	}
	if cfg.ExcludeCompiled[1] != nil {
		t.Errorf("ExcludeCompiled[1] is non-nil; expected nil for invalid regex")
	}
	if cfg.ExcludeCompiled[2] == nil {
		t.Errorf("ExcludeCompiled[2] is nil; expected a compiled regex for ^also_valid$")
	}
	if got, want := len(cfg.ExcludeErrors), 1; got != want {
		t.Fatalf("len(ExcludeErrors)=%d, want %d", got, want)
	}
	e := cfg.ExcludeErrors[0]
	if e.Index != 1 {
		t.Errorf("ExcludeErrors[0].Index=%d, want 1", e.Index)
	}
	if e.Source != "[invalid" {
		t.Errorf("ExcludeErrors[0].Source=%q, want %q", e.Source, "[invalid")
	}
	if e.Reason == "" {
		t.Errorf("ExcludeErrors[0].Reason is empty; expected the regexp.Compile err string")
	}
}

func TestLoad_ExcludeEmpty(t *testing.T) {
	clearConfigEnv(t)
	body := minimalConfigJSON(t, map[string]any{
		"exclude": []string{},
	})
	p := writeConfigFile(t, body)

	cfg, err := Load(context.Background(), p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ExcludeCompiled == nil {
		t.Errorf("ExcludeCompiled is nil; want non-nil zero-length slice")
	}
	if len(cfg.ExcludeCompiled) != 0 {
		t.Errorf("len(ExcludeCompiled)=%d, want 0", len(cfg.ExcludeCompiled))
	}
	if len(cfg.ExcludeErrors) != 0 {
		t.Errorf("len(ExcludeErrors)=%d, want 0", len(cfg.ExcludeErrors))
	}
}

func TestLoad_RoundTripDefaults(t *testing.T) {
	clearConfigEnv(t)
	body := minimalConfigJSON(t, nil)
	p := writeConfigFile(t, body)

	cfg, err := Load(context.Background(), p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SnapshotDir != "/snap" {
		t.Errorf("SnapshotDir=%q, want /snap", cfg.SnapshotDir)
	}
	if cfg.StateDir != "/state" {
		t.Errorf("StateDir=%q, want /state", cfg.StateDir)
	}
	if cfg.RuntimeDir != "/run" {
		t.Errorf("RuntimeDir=%q, want /run", cfg.RuntimeDir)
	}
	if cfg.DataDir != "/data" {
		t.Errorf("DataDir=%q, want /data", cfg.DataDir)
	}
	// §8.19 prose: TrustDir = filepath.Join(DataDir, "allow"); not a
	// separately configurable JSON key.
	if got, want := cfg.TrustDir, filepath.Join("/data", "allow"); got != want {
		t.Errorf("TrustDir=%q, want %q", got, want)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel=%q, want info", cfg.LogLevel)
	}
	if !cfg.Hooks.RunHooks {
		t.Errorf("Hooks.RunHooks=false, want true (file value)")
	}
	if cfg.Hooks.TimeoutSeconds != 600 {
		t.Errorf("Hooks.TimeoutSeconds=%d, want 600", cfg.Hooks.TimeoutSeconds)
	}
	if cfg.ProtoVersion != 1 {
		t.Errorf("ProtoVersion=%d, want 1", cfg.ProtoVersion)
	}
	if cfg.Version != 1 {
		t.Errorf("Version=%d, want 1 (§10.7 schema version)", cfg.Version)
	}
}

// §11.4 — env-vs-file resolution table for snapshot_dir, state_dir,
// runtime_dir. Env wins outright when set non-empty.
func TestLoad_EnvOverrides_Dirs(t *testing.T) {
	cases := []struct {
		name      string
		envVar    string
		fileKey   string
		envValue  string
		fileValue string
		check     func(*Config) string
		wantSet   string
	}{
		{
			name:      "snapshot_dir",
			envVar:    "WEZSESH_SNAPSHOT_DIR",
			fileKey:   "snapshot_dir",
			envValue:  "/from-env-snap",
			fileValue: "/from-file-snap",
			check:     func(c *Config) string { return c.SnapshotDir },
			wantSet:   "/from-env-snap",
		},
		{
			name:      "state_dir",
			envVar:    "WEZSESH_STATE_DIR",
			fileKey:   "state_dir",
			envValue:  "/from-env-state",
			fileValue: "/from-file-state",
			check:     func(c *Config) string { return c.StateDir },
			wantSet:   "/from-env-state",
		},
		{
			name:      "runtime_dir",
			envVar:    "WEZSESH_RUNTIME_DIR",
			fileKey:   "runtime_dir",
			envValue:  "/from-env-rt",
			fileValue: "/from-file-rt",
			check:     func(c *Config) string { return c.RuntimeDir },
			wantSet:   "/from-env-rt",
		},
		{
			name:      "data_dir",
			envVar:    "WEZSESH_DATA_DIR",
			fileKey:   "data_dir",
			envValue:  "/from-env-data",
			fileValue: "/from-file-data",
			check:     func(c *Config) string { return c.DataDir },
			wantSet:   "/from-env-data",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/env_set", func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv(tc.envVar, tc.envValue)
			body := minimalConfigJSON(t, map[string]any{tc.fileKey: tc.fileValue})
			p := writeConfigFile(t, body)
			cfg, err := Load(context.Background(), p)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := tc.check(cfg); got != tc.wantSet {
				t.Errorf("got %q, want env override %q", got, tc.wantSet)
			}
		})
		t.Run(tc.name+"/env_unset", func(t *testing.T) {
			clearConfigEnv(t)
			body := minimalConfigJSON(t, map[string]any{tc.fileKey: tc.fileValue})
			p := writeConfigFile(t, body)
			cfg, err := Load(context.Background(), p)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := tc.check(cfg); got != tc.fileValue {
				t.Errorf("got %q, want file value %q", got, tc.fileValue)
			}
		})
	}
}

// §11.4 — when the file leaves a dir field empty (and no env override
// is set), Load fills it from §12.5 auto-detect rather than leaving it
// blank for the binary to choke on at logger.New / state.Open. Live
// `apply_to_config` shakedown surfaced this gap: a JSON file with
// state_dir="" reached logger.New as "stateDir is empty".
func TestLoad_EmptyDirsFallthroughToAutoDetect(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	cases := []struct {
		name    string
		fileKey string
		check   func(*Config) string
		want    func() string
	}{
		{
			name:    "snapshot_dir",
			fileKey: "snapshot_dir",
			check:   func(c *Config) string { return c.SnapshotDir },
			want:    func() string { return autoSnapshotDir(runtime.GOOS, os.Getenv, home) },
		},
		{
			name:    "state_dir",
			fileKey: "state_dir",
			check:   func(c *Config) string { return c.StateDir },
			want:    func() string { return autoStateDir(runtime.GOOS, os.Getenv, home) },
		},
		{
			name:    "runtime_dir",
			fileKey: "runtime_dir",
			check:   func(c *Config) string { return c.RuntimeDir },
			want:    func() string { return autoRuntimeDir(runtime.GOOS, os.Getenv, os.Getuid()) },
		},
		{
			name:    "data_dir",
			fileKey: "data_dir",
			check:   func(c *Config) string { return c.DataDir },
			want:    func() string { return autoDataDir(runtime.GOOS, os.Getenv, home) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			body := minimalConfigJSON(t, map[string]any{tc.fileKey: ""})
			p := writeConfigFile(t, body)
			cfg, err := Load(context.Background(), p)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got, want := tc.check(cfg), tc.want(); got != want {
				t.Errorf("%s = %q, want autodetect %q", tc.name, got, want)
			}
		})
	}
}

// §11.4 — file value still wins over auto-detect when non-empty. (The
// fallthrough must NOT clobber an explicit file path.) Pair with
// TestLoad_EmptyDirsFallthroughToAutoDetect: file-non-empty=>file,
// file-empty=>autodetect.
func TestLoad_NonEmptyFileDirsBeatAutoDetect(t *testing.T) {
	cases := []struct {
		name    string
		fileKey string
		value   string
		check   func(*Config) string
	}{
		{"snapshot_dir", "snapshot_dir", "/explicit-snap", func(c *Config) string { return c.SnapshotDir }},
		{"state_dir", "state_dir", "/explicit-state", func(c *Config) string { return c.StateDir }},
		{"runtime_dir", "runtime_dir", "/explicit-rt", func(c *Config) string { return c.RuntimeDir }},
		{"data_dir", "data_dir", "/explicit-data", func(c *Config) string { return c.DataDir }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			body := minimalConfigJSON(t, map[string]any{tc.fileKey: tc.value})
			p := writeConfigFile(t, body)
			cfg, err := Load(context.Background(), p)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := tc.check(cfg); got != tc.value {
				t.Errorf("%s = %q, want file value %q (file > auto-detect)", tc.name, got, tc.value)
			}
		})
	}
}

// §11.4 — env still beats file-empty-then-autodetect. A user with
// state_dir="" in their JSON and WEZSESH_STATE_DIR set must see the
// env value, not the autodetect.
func TestLoad_EnvBeatsAutoDetectFillForEmptyDir(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("WEZSESH_STATE_DIR", "/from-env-state")
	body := minimalConfigJSON(t, map[string]any{"state_dir": ""})
	p := writeConfigFile(t, body)
	cfg, err := Load(context.Background(), p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.StateDir != "/from-env-state" {
		t.Errorf("StateDir=%q, want /from-env-state (env beats autodetect-fill)", cfg.StateDir)
	}
}

// §8.19 — when Load fills DataDir from autodetect (file's data_dir==""),
// TrustDir must be derived from the filled value, mirroring the
// LoadFromEnv no-file branch.
func TestLoad_TrustDirDerivesFromAutoDetectFilledDataDir(t *testing.T) {
	clearConfigEnv(t)
	body := minimalConfigJSON(t, map[string]any{"data_dir": ""})
	p := writeConfigFile(t, body)
	cfg, err := Load(context.Background(), p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DataDir == "" {
		t.Fatalf("DataDir empty after Load; expected autodetect fill")
	}
	if got, want := cfg.TrustDir, filepath.Join(cfg.DataDir, "allow"); got != want {
		t.Errorf("TrustDir=%q, want %q (derived from autodetect-filled DataDir)", got, want)
	}
}

// §8.19 prose — TrustDir is derived from DataDir (filepath.Join(DataDir,
// "allow")), not a separately configurable JSON key. Load must populate
// it after env overrides so an env-driven DataDir is reflected in
// TrustDir.
func TestLoad_TrustDirDerivedFromDataDir(t *testing.T) {
	t.Run("file_value", func(t *testing.T) {
		clearConfigEnv(t)
		body := minimalConfigJSON(t, map[string]any{"data_dir": "/x/y"})
		p := writeConfigFile(t, body)
		cfg, err := Load(context.Background(), p)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.DataDir != "/x/y" {
			t.Errorf("DataDir=%q, want /x/y", cfg.DataDir)
		}
		if got, want := cfg.TrustDir, filepath.Join("/x/y", "allow"); got != want {
			t.Errorf("TrustDir=%q, want %q", got, want)
		}
	})
	t.Run("env_overrides_data_dir_propagates_to_trust_dir", func(t *testing.T) {
		clearConfigEnv(t)
		t.Setenv("WEZSESH_DATA_DIR", "/from-env-data")
		body := minimalConfigJSON(t, map[string]any{"data_dir": "/from-file-data"})
		p := writeConfigFile(t, body)
		cfg, err := Load(context.Background(), p)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.DataDir != "/from-env-data" {
			t.Errorf("DataDir=%q, want /from-env-data (env override)", cfg.DataDir)
		}
		if got, want := cfg.TrustDir, filepath.Join("/from-env-data", "allow"); got != want {
			t.Errorf("TrustDir=%q, want %q (env-overridden DataDir)", got, want)
		}
	})
	t.Run("auto_detect_path_also_derives", func(t *testing.T) {
		clearConfigEnv(t)
		t.Setenv("WEZSESH_DATA_DIR", "/from-env-data")
		cfg, err := LoadFromEnv(context.Background())
		if err != nil {
			t.Fatalf("LoadFromEnv: %v", err)
		}
		if got, want := cfg.TrustDir, filepath.Join("/from-env-data", "allow"); got != want {
			t.Errorf("TrustDir=%q, want %q (LoadFromEnv autodetect path)", got, want)
		}
	})
}

// §11.4 — log_level uses ResolveLevel: env can only make logging more
// verbose (lower numeric Level), never quieter.
func TestLoad_EnvOverride_LogLevel(t *testing.T) {
	cases := []struct {
		name    string
		fileLvl string
		envLvl  string
		wantLvl string
	}{
		{"env_more_verbose_wins", "info", "debug", "debug"},
		{"env_less_verbose_loses", "info", "warn", "info"},
		{"env_unset_keeps_file", "debug", "", "debug"},
		{"file_empty_env_set", "", "warn", "warn"},
		{"both_unset_unchanged", "", "", ""},
		{"equal_levels", "warn", "warn", "warn"},
		{"unknown_file_value_coerces_to_info", "verbose", "", "info"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			if tc.envLvl != "" {
				t.Setenv("WEZSESH_LOG", tc.envLvl)
			}
			body := minimalConfigJSON(t, map[string]any{"log_level": tc.fileLvl})
			p := writeConfigFile(t, body)
			cfg, err := Load(context.Background(), p)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.LogLevel != tc.wantLvl {
				t.Errorf("LogLevel=%q, want %q (file=%q env=%q)",
					cfg.LogLevel, tc.wantLvl, tc.fileLvl, tc.envLvl)
			}
		})
	}
}

// §11.4 — hooks.run_hooks: WEZSESH_NO_HOOKS=1 forces false; any other
// value (or unset) leaves the file value alone.
func TestLoad_EnvOverride_NoHooks(t *testing.T) {
	cases := []struct {
		name    string
		envVal  string
		fileVal bool
		want    bool
	}{
		{"unset_keeps_true", "", true, true},
		{"unset_keeps_false", "", false, false},
		{"one_forces_false", "1", true, false},
		{"one_keeps_false", "1", false, false},
		{"zero_keeps_true", "0", true, true},
		{"other_keeps_true", "yes", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			if tc.envVal != "" {
				t.Setenv("WEZSESH_NO_HOOKS", tc.envVal)
			}
			body := minimalConfigJSON(t, map[string]any{
				"hooks": map[string]any{
					"run_hooks":           tc.fileVal,
					"prompt_on_untrusted": false,
					"timeout_seconds":     600,
				},
			})
			p := writeConfigFile(t, body)
			cfg, err := Load(context.Background(), p)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Hooks.RunHooks != tc.want {
				t.Errorf("Hooks.RunHooks=%v, want %v", cfg.Hooks.RunHooks, tc.want)
			}
		})
	}
}

func TestLoad_EmptyPath(t *testing.T) {
	clearConfigEnv(t)
	if _, err := Load(context.Background(), ""); err == nil {
		t.Fatal("Load with empty path: want error, got nil")
	}
}

func TestLoad_MissingFile(t *testing.T) {
	clearConfigEnv(t)
	miss := filepath.Join(t.TempDir(), "does-not-exist.json")
	if _, err := Load(context.Background(), miss); err == nil {
		t.Fatal("Load with missing file: want error, got nil")
	}
}

func TestLoad_BadJSON(t *testing.T) {
	clearConfigEnv(t)
	p := writeConfigFile(t, []byte("{not-json"))
	if _, err := Load(context.Background(), p); err == nil {
		t.Fatal("Load with bad JSON: want error, got nil")
	}
}

func TestLoadFromEnv_UnsetFallsBackToAutoDetect(t *testing.T) {
	clearConfigEnv(t)
	cfg, err := LoadFromEnv(context.Background())
	if err != nil {
		t.Fatalf("LoadFromEnv (unset): %v", err)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel=%q, want info from auto-detect default", cfg.LogLevel)
	}
	if !cfg.Hooks.RunHooks {
		t.Errorf("Hooks.RunHooks=false, want true (auto-detect default)")
	}
}

// §11.4 — env beats auto-detect too, not just the on-disk file. The
// no-config-file path runs on `wezsesh doctor` / `list` / `trust` /
// `reset` / shell-invoked `find`; without these overrides those
// subcommands silently ignore WEZSESH_* (regression coverage).
func TestLoadFromEnv_EnvOverridesAutoDetect_Dirs(t *testing.T) {
	cases := []struct {
		name    string
		envVar  string
		envVal  string
		check   func(*Config) string
	}{
		{"snapshot_dir", "WEZSESH_SNAPSHOT_DIR", "/from-env-snap", func(c *Config) string { return c.SnapshotDir }},
		{"state_dir", "WEZSESH_STATE_DIR", "/from-env-state", func(c *Config) string { return c.StateDir }},
		{"runtime_dir", "WEZSESH_RUNTIME_DIR", "/from-env-rt", func(c *Config) string { return c.RuntimeDir }},
		{"data_dir", "WEZSESH_DATA_DIR", "/from-env-data", func(c *Config) string { return c.DataDir }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv(tc.envVar, tc.envVal)
			cfg, err := LoadFromEnv(context.Background())
			if err != nil {
				t.Fatalf("LoadFromEnv: %v", err)
			}
			if got := tc.check(cfg); got != tc.envVal {
				t.Errorf("got %q, want env override %q (auto-detect path)", got, tc.envVal)
			}
		})
	}
}

func TestLoadFromEnv_EnvOverridesAutoDetect_LogLevel(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("WEZSESH_LOG", "debug")
	cfg, err := LoadFromEnv(context.Background())
	if err != nil {
		t.Fatalf("LoadFromEnv: %v", err)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel=%q, want debug (env beats auto-detect default 'info')", cfg.LogLevel)
	}
}

func TestLoadFromEnv_EnvOverridesAutoDetect_NoHooks(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("WEZSESH_NO_HOOKS", "1")
	cfg, err := LoadFromEnv(context.Background())
	if err != nil {
		t.Fatalf("LoadFromEnv: %v", err)
	}
	if cfg.Hooks.RunHooks {
		t.Errorf("Hooks.RunHooks=true, want false (WEZSESH_NO_HOOKS=1 over auto-detect)")
	}
}

func TestLoadFromEnv_SetButMissing(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("WEZSESH_CONFIG_FILE", filepath.Join(t.TempDir(), "no-such-file.json"))
	if _, err := LoadFromEnv(context.Background()); err == nil {
		t.Fatal("LoadFromEnv with missing file: want error, got nil")
	}
}

func TestLoadFromEnv_SetAndPresent(t *testing.T) {
	clearConfigEnv(t)
	body := minimalConfigJSON(t, map[string]any{"snapshot_dir": "/from-file"})
	p := writeConfigFile(t, body)
	t.Setenv("WEZSESH_CONFIG_FILE", p)
	cfg, err := LoadFromEnv(context.Background())
	if err != nil {
		t.Fatalf("LoadFromEnv: %v", err)
	}
	if cfg.SnapshotDir != "/from-file" {
		t.Errorf("SnapshotDir=%q, want /from-file", cfg.SnapshotDir)
	}
}

// fakeEnv constructs a deterministic env lookup for autoDetectFor tests.
func fakeEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// §12.5 / §12.2 — Linux auto-detect with $XDG_STATE_HOME and
// $XDG_RUNTIME_DIR set.
func TestAutoDetectFor_Linux_XDGSet(t *testing.T) {
	cfg, err := autoDetectFor("linux", fakeEnv(map[string]string{
		"XDG_STATE_HOME":  "/xdg/state",
		"XDG_DATA_HOME":   "/xdg/data",
		"XDG_RUNTIME_DIR": "/run/user/1000",
	}), "/home/grady", 1000)
	if err != nil {
		t.Fatalf("autoDetectFor: %v", err)
	}
	if got, want := cfg.SnapshotDir, "/xdg/state/wezterm/resurrect/"; got != want {
		t.Errorf("SnapshotDir=%q, want %q", got, want)
	}
	if got, want := cfg.StateDir, "/xdg/state/wezsesh/"; got != want {
		t.Errorf("StateDir=%q, want %q", got, want)
	}
	if got, want := cfg.RuntimeDir, "/run/user/1000/wezsesh/"; got != want {
		t.Errorf("RuntimeDir=%q, want %q", got, want)
	}
	if got, want := cfg.DataDir, "/xdg/data/wezsesh/"; got != want {
		t.Errorf("DataDir=%q, want %q", got, want)
	}
	// §8.19 prose: TrustDir derived from DataDir.
	if got, want := cfg.TrustDir, filepath.Join("/xdg/data/wezsesh/", "allow"); got != want {
		t.Errorf("TrustDir=%q, want %q", got, want)
	}
}

// Linux fallback when XDG_STATE_HOME is unset: ~/.local/state/...
func TestAutoDetectFor_Linux_XDGStateHomeUnset(t *testing.T) {
	cfg, err := autoDetectFor("linux", fakeEnv(map[string]string{}), "/home/grady", 1000)
	if err != nil {
		t.Fatalf("autoDetectFor: %v", err)
	}
	if got, want := cfg.SnapshotDir, "/home/grady/.local/state/wezterm/resurrect/"; got != want {
		t.Errorf("SnapshotDir=%q, want %q", got, want)
	}
	if got, want := cfg.StateDir, "/home/grady/.local/state/wezsesh/"; got != want {
		t.Errorf("StateDir=%q, want %q", got, want)
	}
	// XDG_RUNTIME_DIR unset on Linux falls through to /tmp/wezsesh-<uid>/.
	if got, want := cfg.RuntimeDir, "/tmp/wezsesh-1000/"; got != want {
		t.Errorf("RuntimeDir=%q, want %q", got, want)
	}
	// §12.5: $XDG_DATA_HOME unset → <home>/.local/share/wezsesh/.
	if got, want := cfg.DataDir, "/home/grady/.local/share/wezsesh/"; got != want {
		t.Errorf("DataDir=%q, want %q", got, want)
	}
}

// §12.2 — Linux without $XDG_RUNTIME_DIR uses /tmp/wezsesh-<uid>/.
func TestAutoDetectFor_Linux_NoXDGRuntime(t *testing.T) {
	cfg, err := autoDetectFor("linux", fakeEnv(map[string]string{
		"XDG_STATE_HOME": "/xdg/state",
	}), "/home/grady", 4242)
	if err != nil {
		t.Fatalf("autoDetectFor: %v", err)
	}
	if got, want := cfg.RuntimeDir, "/tmp/wezsesh-4242/"; got != want {
		t.Errorf("RuntimeDir=%q, want %q", got, want)
	}
}

// §12.5 / §12.2 — darwin: snapshot under Library/Application Support;
// state_dir uses XDG semantics on darwin too; runtime_dir always
// /tmp/wezsesh-<uid>/ regardless of $XDG_RUNTIME_DIR.
func TestAutoDetectFor_Darwin(t *testing.T) {
	// $XDG_RUNTIME_DIR set is ignored on darwin per §12.2.
	cfg, err := autoDetectFor("darwin", fakeEnv(map[string]string{
		"XDG_RUNTIME_DIR": "/run/user/501",
		"XDG_STATE_HOME":  "/xdg/state",
	}), "/Users/grady", 501)
	if err != nil {
		t.Fatalf("autoDetectFor: %v", err)
	}
	if got, want := cfg.SnapshotDir, "/Users/grady/Library/Application Support/wezterm/resurrect/"; got != want {
		t.Errorf("SnapshotDir=%q, want %q", got, want)
	}
	if got, want := cfg.StateDir, "/xdg/state/wezsesh/"; got != want {
		t.Errorf("StateDir=%q, want %q (XDG semantics on darwin too)", got, want)
	}
	if got, want := cfg.RuntimeDir, "/tmp/wezsesh-501/"; got != want {
		t.Errorf("RuntimeDir=%q, want %q (darwin always /tmp)", got, want)
	}
}

// darwin without XDG_STATE_HOME falls back to <home>/.local/state — same
// as Linux semantics (§12.5 explicit).
func TestAutoDetectFor_Darwin_NoXDGStateHome(t *testing.T) {
	cfg, err := autoDetectFor("darwin", fakeEnv(map[string]string{}), "/Users/grady", 501)
	if err != nil {
		t.Fatalf("autoDetectFor: %v", err)
	}
	if got, want := cfg.StateDir, "/Users/grady/.local/state/wezsesh/"; got != want {
		t.Errorf("StateDir=%q, want %q", got, want)
	}
	// §12.5: darwin uses XDG semantics for data_dir too —
	// <home>/.local/share/wezsesh/ when $XDG_DATA_HOME is unset.
	if got, want := cfg.DataDir, "/Users/grady/.local/share/wezsesh/"; got != want {
		t.Errorf("DataDir=%q, want %q", got, want)
	}
}

// §12.5 — darwin with $XDG_DATA_HOME set uses that root, mirroring the
// state_dir behaviour. (XDG semantics on darwin too, matching PRD §6.8.)
func TestAutoDetectFor_Darwin_XDGDataHomeSet(t *testing.T) {
	cfg, err := autoDetectFor("darwin", fakeEnv(map[string]string{
		"XDG_DATA_HOME": "/xdg/data",
	}), "/Users/grady", 501)
	if err != nil {
		t.Fatalf("autoDetectFor: %v", err)
	}
	if got, want := cfg.DataDir, "/xdg/data/wezsesh/"; got != want {
		t.Errorf("DataDir=%q, want %q", got, want)
	}
}

// §8.19 prose — autodetect populates TrustDir = DataDir/allow.
func TestAutoDetectFor_TrustDirDerived(t *testing.T) {
	cfg, err := autoDetectFor("linux", fakeEnv(map[string]string{}), "/home/grady", 1000)
	if err != nil {
		t.Fatalf("autoDetectFor: %v", err)
	}
	wantTrust := filepath.Join(cfg.DataDir, "allow")
	if cfg.TrustDir != wantTrust {
		t.Errorf("TrustDir=%q, want %q (derived from DataDir)", cfg.TrustDir, wantTrust)
	}
}

// AutoDetect's defaults from §11 are populated regardless of host.
func TestAutoDetectFor_Defaults(t *testing.T) {
	cfg, err := autoDetectFor("linux", fakeEnv(map[string]string{}), "/home/grady", 1000)
	if err != nil {
		t.Fatalf("autoDetectFor: %v", err)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel=%q, want info", cfg.LogLevel)
	}
	if !cfg.Hooks.RunHooks {
		t.Errorf("Hooks.RunHooks=false, want true")
	}
	if cfg.Hooks.TimeoutSeconds != 600 {
		t.Errorf("Hooks.TimeoutSeconds=%d, want 600", cfg.Hooks.TimeoutSeconds)
	}
	if cfg.ProtoVersion != 1 {
		t.Errorf("ProtoVersion=%d, want 1", cfg.ProtoVersion)
	}
	if cfg.Version != 1 {
		t.Errorf("Version=%d, want 1 (§10.7 schema version)", cfg.Version)
	}
	if len(cfg.Exclude) != 1 || cfg.Exclude[0] != "^default$" {
		t.Errorf("Exclude=%v, want [\"^default$\"]", cfg.Exclude)
	}
	if len(cfg.ExcludeCompiled) != 1 || cfg.ExcludeCompiled[0] == nil {
		t.Errorf("ExcludeCompiled=%v, want one compiled regex", cfg.ExcludeCompiled)
	}
	if cfg.Sort != "live_first" {
		t.Errorf("Sort=%q, want live_first", cfg.Sort)
	}
	if cfg.DefaultAction != "switch" {
		t.Errorf("DefaultAction=%q, want switch", cfg.DefaultAction)
	}
	if !cfg.ConfirmDelete {
		t.Errorf("ConfirmDelete=false, want true")
	}
	if !cfg.ConfirmOverwrite {
		t.Errorf("ConfirmOverwrite=false, want true")
	}
	// Default Markers (§11 / §10.7).
	if cfg.Markers.Active == "" || cfg.Markers.Live == "" {
		t.Errorf("Markers default empty: %+v", cfg.Markers)
	}
	// Default Keys map (§11.1).
	if cfg.Keys.Switch != "s" || cfg.Keys.Quit != "q" || cfg.Keys.Top != "gg" {
		t.Errorf("Keys default unexpected: %+v", cfg.Keys)
	}
}

// Empty home is a hard error (callers come in via os.UserHomeDir, which
// is allowed to fail).
func TestAutoDetectFor_EmptyHomeErrors(t *testing.T) {
	if _, err := autoDetectFor("linux", fakeEnv(map[string]string{}), "", 0); err == nil {
		t.Fatal("autoDetectFor with empty home: want error, got nil")
	}
}
