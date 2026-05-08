// Package config implements the binary-side configuration loaded from
// $WEZSESH_CONFIG_JSON_BASE64 (§8.19). The on-disk JSON shape is
// described in §10.7; the per-key schema in §11; the env-vs-file
// resolution table in §11.4; auto-detection in §12.5 / §12.2.
package config

import "regexp"

// Config is the binary-side configuration loaded from
// $WEZSESH_CONFIG_JSON_BASE64. Field shapes follow §8.19; the JSON keys
// match §10.7.
type Config struct {
	// Version is the §10.7 schema version (currently 1). Captured so a
	// future migration can key off the file value; no runtime behaviour
	// today.
	Version int `json:"version"`

	SnapshotDir string `json:"snapshot_dir"`
	StateDir    string `json:"state_dir"`
	RuntimeDir  string `json:"runtime_dir"`
	DataDir     string `json:"data_dir"`
	// TrustDir is NOT a separately configurable JSON key (§8.19): Load
	// derives it as filepath.Join(DataDir, "allow") per §12.1 (the trust
	// store lives at `<data_dir>/allow/`). Captured as a struct field so
	// the §8.20.1 startup callsite (`trust.Open(ctx, cfg.TrustDir, log)`)
	// has a defined source.
	TrustDir                  string `json:"-"`
	LogLevel                  string `json:"log_level"`
	Sort                      string `json:"sort"`
	DefaultAction             string `json:"default_action"`
	DefaultActionLoadNoPrompt bool   `json:"default_action_load_no_prompt"`
	ConfirmDelete             bool   `json:"confirm_delete"`
	ConfirmOverwrite          bool   `json:"confirm_overwrite"`

	// Exclude is the RE2 regex source array as authored. ExcludeCompiled
	// is populated by Load: len(ExcludeCompiled) == len(Exclude); a nil
	// slot marks an invalid entry whose error is recorded in
	// ExcludeErrors. The runtime contract (§17.3 row "Config Exclude
	// invalid regex") is that an invalid element is treated as a no-op
	// match — this package guarantees only the nil slot + error record;
	// the no-op behaviour is implemented downstream.
	Exclude         []string         `json:"exclude"`
	ExcludeCompiled []*regexp.Regexp `json:"-"`
	ExcludeErrors   []ExcludeError   `json:"-"`

	NewWorkspaceCommand string `json:"new_workspace_command"`

	Preview struct {
		Enabled bool    `json:"enabled"`
		Width   float64 `json:"width"`
	} `json:"preview"`

	Markers      Markers  `json:"markers"`
	Columns      []string `json:"columns"`
	NameTruncate string   `json:"name_truncate"`

	Colors Colors `json:"colors"`

	Hooks struct {
		RunHooks          bool `json:"run_hooks"`
		PromptOnUntrusted bool `json:"prompt_on_untrusted"`
		TimeoutSeconds    int  `json:"timeout_seconds"`
	} `json:"hooks"`

	ResurrectArgvAllowlist []string `json:"resurrect_argv_allowlist"`

	Keys KeyMap `json:"keys"`

	PluginVersion string `json:"plugin_version"`
	ProtoVersion  int    `json:"proto_version"`
}

// Markers mirrors the §10.7 markers object; defaults per §11 / §10.7.
type Markers struct {
	Active  string `json:"active"`
	Live    string `json:"live"`
	Marked  string `json:"marked"`
	Unsaved string `json:"unsaved"`
	Pinned  string `json:"pinned"`
}

// Colors mirrors the §10.7 colors object. Each field is the lipgloss-
// compatible string spec or empty for "use default" (§11 row colors.*).
type Colors struct {
	Accent         string `json:"accent"`
	Muted          string `json:"muted"`
	Error          string `json:"error"`
	Success        string `json:"success"`
	FocusBg        string `json:"focus_bg"`
	MatchHighlight string `json:"match_highlight"`
	LiveMarker     string `json:"live_marker"`
	SavedMarker    string `json:"saved_marker"`
}

// KeyMap mirrors §11.1 (default keys table). A field carrying the empty
// string means "use the default at this slot"; a non-empty string is
// either a single key or a multi-key sequence ("gg") per §11.1.
type KeyMap struct {
	Switch     string `json:"switch"`
	Load       string `json:"load"`
	Rename     string `json:"rename"`
	Delete     string `json:"delete"`
	Save       string `json:"save"`
	New        string `json:"new"`
	Pin        string `json:"pin"`
	Tag        string `json:"tag"`
	Mark       string `json:"mark"`
	MarkAlt    string `json:"mark_alt"`
	ClearMarks string `json:"clear_marks"`
	Help       string `json:"help"`
	Filter     string `json:"filter"`
	Quit       string `json:"quit"`
	Up         string `json:"up"`
	Down       string `json:"down"`
	Top        string `json:"top"`
	Bottom     string `json:"bottom"`
}

// ExcludeError records a single invalid Exclude regex element. Index is
// the position in Config.Exclude (0-based); Source is the regex string
// as authored; Reason is the err.Error() from regexp.Compile.
type ExcludeError struct {
	Index  int
	Source string
	Reason string
}
