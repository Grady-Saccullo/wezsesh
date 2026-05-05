package wezcli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Pane mirrors one row of `wezterm cli list --format json`. Field naming
// follows wezterm's snake_case schema (see wezterm/src/cli/list.rs); the
// Go field tags map them to the project's CamelCase struct names.
//
// Trickiest fields:
//
//   - cwd: a `file://`-form URL string OR the empty string `""` (NOT
//     `null`) when the working directory is unreported. wezterm builds it
//     via `url::Url::from_directory_path` (mux/src/localpane.rs:1057);
//     any caller that wants a filesystem path MUST go through CWDPath()
//     so the empty-string guard is applied before url.Parse.
//
//   - is_active: per-tab, NOT global. Multiple panes in different tabs
//     may report is_active=true simultaneously. To find the user-visible
//     focused pane across the mux, cross-reference `cli list-clients` to
//     get the focused_pane_id and look it up here.
//
//   - tty_name: Option<String> upstream → *string here. nil on Windows
//     and on Unix when the pty doesn't report a name (`--deep` mode's
//     ps-walk requires this be non-nil).
type Pane struct {
	PaneID      int    `json:"pane_id"`
	TabID       int    `json:"tab_id"`
	WindowID    int    `json:"window_id"`
	Workspace   string `json:"workspace"`
	Title       string `json:"title"`
	TabTitle    string `json:"tab_title"`
	WindowTitle string `json:"window_title"`
	CWD         string `json:"cwd"` // file:// URL or "" — use CWDPath()
	Size        Size   `json:"size"`
	IsActive    bool   `json:"is_active"` // per-tab, NOT global
	IsZoomed    bool   `json:"is_zoomed"`
	TTYName     *string `json:"tty_name"` // nil on Windows / unreported
	CursorX     int     `json:"cursor_x"`
	CursorY     int     `json:"cursor_y"`
}

// Size is the per-pane dimension block. Matches `cli list --format json`
// `size: { rows, cols, pixel_width, pixel_height, dpi }`.
type Size struct {
	Rows        int `json:"rows"`
	Cols        int `json:"cols"`
	PixelWidth  int `json:"pixel_width"`
	PixelHeight int `json:"pixel_height"`
	DPI         int `json:"dpi"`
}

// CWDPath decodes the per-pane cwd into a filesystem path. Returns
// (path, true) on a parseable `file://` URL; (`""`, false) when the cwd
// is empty (the wezterm-side "unknown" sentinel) or unparseable.
//
// Empty-string guard is load-bearing: `url.Parse("")` returns a non-nil
// URL with an empty Path field. Callers that pattern-match on cwd would
// then accept every pane as a substring match. See §0.1 / spec acceptance
// gate "cwd parsing guards for `""` (NOT `null`)".
func (p Pane) CWDPath() (string, bool) {
	if p.CWD == "" {
		return "", false
	}
	u, err := url.Parse(p.CWD)
	if err != nil || u.Scheme != "file" {
		return "", false
	}
	// Decode percent-encoding in the path; reject malformed escapes.
	path, err := url.PathUnescape(u.Path)
	if err != nil {
		return "", false
	}
	if path == "" {
		return "", false
	}
	return path, true
}

// List runs `cli list --format json` and decodes the result. The internal
// 2 s ceiling applies (§14.1). A timeout, non-zero exit, or invalid JSON
// surfaces as ErrMuxUnreachable.
func (c *Client) List(ctx context.Context) ([]Pane, error) {
	out, err := c.run(ctx, "cli", "list", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("%w: cli list: %v", ErrMuxUnreachable, err)
	}
	// Defensive: wezterm's `cli list` has historically printed a stray
	// trailing newline; json.Unmarshal handles it but `strings.TrimSpace`
	// makes the error path easier to diagnose if a future build adds a
	// preamble line.
	out = []byte(strings.TrimSpace(string(out)))
	var panes []Pane
	if err := json.Unmarshal(out, &panes); err != nil {
		return nil, fmt.Errorf("%w: cli list json: %v", ErrMuxUnreachable, err)
	}
	return panes, nil
}
