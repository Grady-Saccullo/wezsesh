package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Grady-Saccullo/wezsesh/internal/config"
	"github.com/Grady-Saccullo/wezsesh/internal/nameval"
	"github.com/Grady-Saccullo/wezsesh/internal/snapshots"
	"github.com/Grady-Saccullo/wezsesh/internal/state"
	"github.com/Grady-Saccullo/wezsesh/internal/wezcli"
)

// listFormatText / listFormatJSON are the two values accepted by
// `wezsesh list --format`. Default is text.
const (
	listFormatText = "text"
	listFormatJSON = "json"
)

// listRow is the per-workspace shape emitted by `wezsesh list`. The JSON
// tag set is the contract callers are allowed to depend on:
//
//	{
//	  "name":   string,    workspace name (NFC-normalised)
//	  "live":   bool,      true if the workspace appears in `wezterm cli list`
//	  "saved":  bool,      true if a snapshot file exists for the name
//	  "pinned": bool,      true if pinned via sidecar (saved) or live_pins (live-only)
//	}
//
// Field order mirrors §13.11's union semantics: live + saved are the two
// presence buckets; pinned is the union of the two sidecar/live_pins
// sources of truth.
type listRow struct {
	Name   string `json:"name"`
	Live   bool   `json:"live"`
	Saved  bool   `json:"saved"`
	Pinned bool   `json:"pinned"`
}

// listOutput is the top-level JSON shape produced by `--format json`.
// Wrapping the rows in an object (rather than emitting a bare array)
// leaves room for forward-compatible additions (e.g., a "warnings"
// sibling) without breaking parsers built against today's shape.
//
//	{
//	  "workspaces": [ listRow, ... ]    sorted by name ascending
//	}
type listOutput struct {
	Workspaces []listRow `json:"workspaces"`
}

// listDeps abstracts the three data sources `wezsesh list` consumes so
// tests can swap in fakes without touching the real config, wezterm
// CLI, or on-disk state. Production wires it up from a real config in
// subcmdList; tests build it directly.
type listDeps struct {
	// livePanes returns the wezterm pane list. A non-nil error here is
	// treated as graceful degradation: warnings go to stderr and the
	// live-set is taken as empty so saved + saved-pinned rows still
	// render.
	livePanes func(ctx context.Context) ([]wezcli.Pane, error)
	// repoEntries returns one entry per saved snapshot (pre-parsed
	// sidecars included). Errors fail the listing — a partially-readable
	// snapshot dir already surfaces per-entry ParseError on the Entry
	// rather than at the slice level.
	repoEntries func(ctx context.Context) ([]snapshots.Entry, error)
	// repoHas mirrors repo.Has; used by the §13.11 sanity-prune to
	// decide whether a state.LivePins entry has been migrated to a
	// sidecar pin and should be excluded from the live-pin set.
	repoHas func(ctx context.Context, name string) (bool, error)
	// livePins returns the contents of state.json.live_pins (the
	// `LIVE-only` arm of §13.11). Returned slice is consumed read-only.
	livePins func() []string
}

// subcmdList implements `wezsesh list [--format text|json]` (§8.20).
// Per §8.20.1 step 3, list runs in the "config.LoadFromEnv (falls back
// to AutoDetect); no listener" group: we load config, build the three
// sources, take their union per §13.11, and print. No dispatcher, no
// trust store, no TUI.
//
// §13.14: top-level recover mirrors keygen / reply. rc on panic is
// exitDoctorOrSubcmd (2). The list path holds no reply socket.
func subcmdList(rest []string, stdout, stderr io.Writer) (rc int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "wezsesh list: panic: %v\n", r)
			rc = exitDoctorOrSubcmd
		}
	}()

	format, err := parseListFlags(rest)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh list: %v\n", err)
		return exitDoctorOrSubcmd
	}

	ctx := context.Background()

	cfg, err := config.LoadFromEnv(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh list: config: %v\n", err)
		return exitDoctorOrSubcmd
	}

	deps, err := buildListDeps(ctx, cfg, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh list: %v\n", err)
		return exitDoctorOrSubcmd
	}

	rows, err := computeListRows(ctx, deps, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh list: %v\n", err)
		return exitDoctorOrSubcmd
	}

	if err := renderListRows(stdout, format, rows); err != nil {
		fmt.Fprintf(stderr, "wezsesh list: render: %v\n", err)
		return exitDoctorOrSubcmd
	}
	return exitOK
}

// parseListFlags accepts the documented flags and returns the chosen
// format. Trailing positional args are rejected (§8.20 lists no
// positional args for `list`).
func parseListFlags(rest []string) (string, error) {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	format := fs.String("format", listFormatText, "output format: text|json")
	if err := fs.Parse(rest); err != nil {
		return "", err
	}
	if fs.NArg() != 0 {
		return "", fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	switch *format {
	case listFormatText, listFormatJSON:
		return *format, nil
	default:
		return "", fmt.Errorf("unknown --format %q (want text|json)", *format)
	}
}

// buildListDeps wires production data sources from a loaded config.
// Errors here are fatal to the subcommand (state.Open failure, snapshot
// dir refusal, etc.). A missing wezterm binary is NOT fatal — the
// returned livePanes degrades to "no live workspaces" with a stderr
// warning; §13.11's union still has the saved + sidecar-pinned arms
// to render.
func buildListDeps(ctx context.Context, cfg *config.Config, stderr io.Writer) (listDeps, error) {
	repo, err := snapshots.NewRepo(cfg.SnapshotDir, nil)
	if err != nil {
		return listDeps{}, fmt.Errorf("snapshots: %w", err)
	}
	repoHasFn := func(name string) bool {
		ok, _ := repo.Has(ctx, name)
		return ok
	}
	store, err := state.Open(ctx, cfg.StateDir, nil, repoHasFn)
	if err != nil {
		return listDeps{}, fmt.Errorf("state: %w", err)
	}

	wcli, wcliErr := wezcli.NewClient(nil)

	deps := listDeps{
		repoEntries: repo.List,
		repoHas:     repo.Has,
		livePins:    store.LivePins,
	}
	if wcliErr != nil {
		fmt.Fprintf(stderr, "wezsesh list: wezterm not available (%v); live workspaces omitted\n", wcliErr)
		deps.livePanes = func(context.Context) ([]wezcli.Pane, error) { return nil, nil }
	} else {
		deps.livePanes = wcli.List
	}
	return deps, nil
}

// computeListRows realises the §13.11 union semantics:
//
//   - A row exists per distinct workspace name across the three sources.
//   - .Live is set iff the name appears in `wezterm cli list`'s panes.
//   - .Saved is set iff the name has a snapshot file (Entry from repo.List
//     with no ParseError on the snapshot side; sidecar errors are tolerated).
//   - .Pinned unions sidecar.Pinned (saved arm) with state.LivePins (live-
//     only arm). The sanity-prune from §13.11 is honoured: a live-pin
//     entry whose name IS in repo.List is excluded from the live-pin set
//     because its pin already migrated to the sidecar.
//
// Rows are sorted by name ascending. The row order is the only stable
// invariant golden-file tests can rely on.
func computeListRows(ctx context.Context, deps listDeps, stderr io.Writer) ([]listRow, error) {
	// Live set — degrade gracefully on failure.
	var liveNames map[string]struct{}
	if deps.livePanes != nil {
		panes, err := deps.livePanes(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "wezsesh list: wezcli list failed (%v); live workspaces omitted\n", err)
		} else {
			liveNames = make(map[string]struct{}, len(panes))
			for _, p := range panes {
				if p.Workspace == "" {
					continue
				}
				liveNames[nameval.NormalizeNFC(p.Workspace)] = struct{}{}
			}
		}
	}
	if liveNames == nil {
		liveNames = map[string]struct{}{}
	}

	// Saved set — repo.List is authoritative here. We DO include entries
	// that failed to parse (saved=true), because §13.11's union is keyed
	// on snapshot-file existence; a parse failure does not unmake the
	// snapshot. Sidecar.Pinned is read off the entry directly.
	var entries []snapshots.Entry
	if deps.repoEntries != nil {
		var err error
		entries, err = deps.repoEntries(ctx)
		if err != nil {
			return nil, fmt.Errorf("snapshots list: %w", err)
		}
	}
	savedPinned := make(map[string]bool, len(entries))
	savedNames := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		name := nameval.NormalizeNFC(e.Name)
		savedNames[name] = struct{}{}
		if e.SidecarOK && e.Sidecar.Pinned {
			savedPinned[name] = true
		}
	}

	// Live-pin set — apply §13.11's sanity-prune. A live_pins entry
	// whose name is NOT in liveNames AND IS in repo.List has migrated
	// to a sidecar pin and should be dropped from the live arm. The
	// pseudocode `if not repo.Has(name)` is the canonical phrasing; we
	// honour it via deps.repoHas so callers without a real repo can
	// inject the answer. (savedNames already covers the same answer
	// from the entries we just listed; preferring repoHas keeps the
	// behaviour identical to runTUI's startup path.)
	livePinSet := make(map[string]struct{})
	if deps.livePins != nil {
		for _, name := range deps.livePins() {
			n := nameval.NormalizeNFC(name)
			if deps.repoHas != nil {
				ok, _ := deps.repoHas(ctx, n)
				if ok {
					continue
				}
			} else if _, ok := savedNames[n]; ok {
				continue
			}
			livePinSet[n] = struct{}{}
		}
	}

	// Union the three sources keyed by NFC-normalised name.
	rowsByName := make(map[string]*listRow)
	get := func(name string) *listRow {
		if r, ok := rowsByName[name]; ok {
			return r
		}
		r := &listRow{Name: name}
		rowsByName[name] = r
		return r
	}
	for name := range liveNames {
		get(name).Live = true
	}
	for _, e := range entries {
		name := nameval.NormalizeNFC(e.Name)
		row := get(name)
		row.Saved = true
		if savedPinned[name] {
			row.Pinned = true
		}
	}
	for name := range livePinSet {
		get(name).Pinned = true
	}

	// Final sort: name ascending. Stable for golden-file comparison.
	out := make([]listRow, 0, len(rowsByName))
	for _, r := range rowsByName {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// renderListRows emits the rows in the requested format. text format is
// a header line + one row per workspace; pin/live/saved render as glyphs
// in fixed columns so awk-style downstream parsing stays cheap.
//
// JSON format renders listOutput with a trailing newline.
func renderListRows(w io.Writer, format string, rows []listRow) error {
	switch format {
	case listFormatJSON:
		// Force a non-nil slice so the empty case marshals as `[]`
		// rather than `null` — downstream JSON consumers depend on
		// .workspaces being an array shape (not a nullable).
		if rows == nil {
			rows = []listRow{}
		}
		out := listOutput{Workspaces: rows}
		// Use Marshal (not MarshalIndent) so the byte shape is stable
		// across Go versions and the golden-file tests can assert exact
		// equality. Append a single trailing newline so terminal cat is
		// readable.
		buf, err := json.Marshal(out)
		if err != nil {
			return err
		}
		if _, err := w.Write(buf); err != nil {
			return err
		}
		_, err = w.Write([]byte{'\n'})
		return err
	case listFormatText:
		// Header + one row per workspace. Disk-sourced names pass
		// through SanitizeForDisplay before hitting a writer that may
		// be a TTY — defends the CLAUDE.md "every render of a disk-
		// sourced string" rule on the CLI surface.
		var b strings.Builder
		b.WriteString("LIVE SAVED PIN NAME\n")
		for _, r := range rows {
			b.WriteString(glyph(r.Live))
			b.WriteString("    ")
			b.WriteString(glyph(r.Saved))
			b.WriteString("     ")
			b.WriteString(glyph(r.Pinned))
			b.WriteString("   ")
			b.WriteString(nameval.SanitizeForDisplay(r.Name))
			b.WriteByte('\n')
		}
		_, err := io.WriteString(w, b.String())
		return err
	default:
		return errors.New("unknown format: " + format)
	}
}

// glyph renders a column-aligned single-character flag (`*` or `-`) so
// the text format is grep-friendly.
func glyph(b bool) string {
	if b {
		return "*"
	}
	return "-"
}
