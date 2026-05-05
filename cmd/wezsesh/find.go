package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/Grady-Saccullo/wezsesh/internal/config"
	"github.com/Grady-Saccullo/wezsesh/internal/find"
	whmac "github.com/Grady-Saccullo/wezsesh/internal/hmac"
	"github.com/Grady-Saccullo/wezsesh/internal/ipc"
	"github.com/Grady-Saccullo/wezsesh/internal/ipcdispatcher"
	"github.com/Grady-Saccullo/wezsesh/internal/nameval"
	"github.com/Grady-Saccullo/wezsesh/internal/uservar"
	"github.com/Grady-Saccullo/wezsesh/internal/wezcli"
)

// findFormatText / findFormatJSON are the two values accepted by
// `wezsesh find --format`. Default is text.
const (
	findFormatText = "text"
	findFormatJSON = "json"
)

// findResultRow is the per-match shape emitted by `wezsesh find`. Field
// set is a stable contract callers can depend on.
//
//	{
//	  "pane_id":      int,
//	  "tab_id":       int,
//	  "window_id":    int,
//	  "workspace":    string,
//	  "title":        string,
//	  "cwd":          string,    decoded filesystem path or ""
//	  "score":        int,
//	  "source_field": string,    "title"|"tab_title"|"window_title"|"cwd"|"ps"
//	}
type findResultRow struct {
	PaneID      int    `json:"pane_id"`
	TabID       int    `json:"tab_id"`
	WindowID    int    `json:"window_id"`
	Workspace   string `json:"workspace"`
	Title       string `json:"title"`
	CWD         string `json:"cwd"`
	Score       int    `json:"score"`
	SourceField string `json:"source_field"`
}

// findOutput is the §8.20 JSON shape: a wrapping object so future
// fields (warnings, activated_pane, etc.) can grow without breaking
// downstream parsers built against today's bytes.
type findOutput struct {
	Matches       []findResultRow `json:"matches"`
	ActivatedPane *int            `json:"activated_pane,omitempty"`
}

// findFlags carries the parsed CLI flags.
type findFlags struct {
	Pattern   string
	Workspace string
	CWDOnly   bool
	Deep      bool
	Format    string
}

// findSearcher is the search surface of `internal/find`. The default
// production wiring (defaultFindSearcher) constructs a wezcli.Client
// and forwards to find.Search; tests inject a fake to drive the
// outside-wezterm gate without touching the wezterm binary.
type findSearcher func(ctx context.Context, pattern string, opts find.Options) ([]find.Match, error)

// findActivator runs the §13.7 two-phase activation. The default
// production wiring (defaultFindActivator) calls find.Activate; tests
// inject a fake to assert the inside-wezterm gate fires (and the
// outside-wezterm gate does NOT fire).
//
// currentWindowID is the user's currently-focused window id at find
// invocation time — the same value tuiSetup threads into the
// dispatcher's TargetWindowID stamp. PRD §6.13 step 1 captures it as
// `target_window_id` of the focused client; the find layer's poller
// predicate compares it against `pane.WindowID`.
type findActivator func(ctx context.Context, d ipc.Dispatcher, currentWindowID int, match find.Match) error

// findInsideContext bundles the per-call inside-wezterm context. It is
// returned by buildFindInsideContext; nil means "not inside wezterm,
// or inside-init failed — treat as outside-wezterm".
type findInsideContext struct {
	dispatcher      ipc.Dispatcher
	activator       findActivator
	currentWindowID int
	cleanup         func()
}

// findDeps groups the seams subcmdFind reaches through. Production
// builds the deps from a real config + wezcli + dispatcher; tests
// inject hand-rolled fakes.
type findDeps struct {
	// search runs the haystack scan.
	search findSearcher
	// inside, when non-nil, indicates the binary is running inside
	// wezterm and the returned dispatcher/activator are ready to drive
	// Phase 1 + Phase 2. nil means outside-wezterm: print only.
	inside *findInsideContext
}

// findInsideHook is the test seam for the inside-wezterm context
// builder. Production main() leaves it nil; the real builder
// (buildFindInsideContext) is invoked in that case. Tests set it to a
// closure returning a fake dispatcher + activator so subcmdFind takes
// the inside-wezterm path without standing up a real listener.
//
// A nil return from the hook means "force the outside path" — used by
// the outside-wezterm acceptance test to assert the dispatcher branch
// is NOT taken when WEZTERM_PANE is unset.
var findInsideHook func(ctx context.Context, cfg *config.Config, getEnv func(string) string) (*findInsideContext, error)

// findSearchHook is the test seam for the search side. Production
// leaves it nil; a non-nil hook short-circuits the wezcli construction
// path so unit tests can drive subcmdFind without a wezterm binary on
// PATH.
var findSearchHook findSearcher

// subcmdFind implements `wezsesh find [PATTERN] [flags]` (§8.20).
// Per §8.20.1 step 3, find runs in the "config load; constructs an
// in-process Dispatcher iff invoked from inside wezterm" group.
//
// §13.14: top-level recover mirrors keygen / list / reply. rc on panic
// is exitDoctorOrSubcmd (2). The find path holds no reply socket on
// the outside-wezterm branch; the inside-wezterm branch's dispatcher
// listener is drained inside find.Activate before this body returns.
func subcmdFind(rest []string, stdout, stderr io.Writer) (rc int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "wezsesh find: panic: %v\n", r)
			rc = exitDoctorOrSubcmd
		}
	}()

	flags, err := parseFindFlags(rest)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh find: %v\n", err)
		return exitDoctorOrSubcmd
	}

	ctx := context.Background()

	cfg, err := config.LoadFromEnv(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh find: config: %v\n", err)
		return exitDoctorOrSubcmd
	}

	deps, err := buildFindDeps(ctx, cfg, os.Getenv, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh find: %v\n", err)
		return exitDoctorOrSubcmd
	}
	if deps.inside != nil && deps.inside.cleanup != nil {
		defer deps.inside.cleanup()
	}

	return runFind(ctx, deps, flags, stdout, stderr)
}

// parseFindFlags accepts the documented flags and returns the parsed
// findFlags. The pattern is the optional positional arg (empty pattern
// matches every pane that survives the workspace / cwd-only filters).
func parseFindFlags(rest []string) (findFlags, error) {
	fs := flag.NewFlagSet("find", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var p findFlags
	fs.StringVar(&p.Workspace, "workspace", "", "restrict to a single workspace name")
	fs.BoolVar(&p.CWDOnly, "cwd-only", false, "match only against per-pane cwd")
	fs.BoolVar(&p.Deep, "deep", false, "include `ps -t` haystacks (best-effort)")
	fs.StringVar(&p.Format, "format", findFormatText, "output format: text|json")
	if err := fs.Parse(rest); err != nil {
		return findFlags{}, err
	}
	tail := fs.Args()
	switch len(tail) {
	case 0:
		// empty pattern is allowed.
	case 1:
		p.Pattern = tail[0]
	default:
		return findFlags{}, fmt.Errorf("expected at most one positional pattern; got %d (%s)",
			len(tail), strings.Join(tail, " "))
	}
	switch p.Format {
	case findFormatText, findFormatJSON:
	default:
		return findFlags{}, fmt.Errorf("unknown --format %q (want text|json)", p.Format)
	}
	return p, nil
}

// buildFindDeps wires production data sources from a loaded config.
// Both halves of the deps surface are honoured by their respective
// test seams (findSearchHook / findInsideHook) — when set, the hooks
// short-circuit the production builder so unit tests can drive
// subcmdFind without standing up a real wezterm + dispatcher.
//
// inside-wezterm gate: WEZTERM_PANE must be set AND the listener-init
// must succeed. A failure on the inside-init path is NOT fatal — per
// §8.20.1 step 3, find degrades to "prints results only" in that case.
// Stderr carries a one-line warning so the user knows the activate
// branch was skipped.
func buildFindDeps(ctx context.Context, cfg *config.Config, getEnv func(string) string, stderr io.Writer) (findDeps, error) {
	deps := findDeps{}

	// search side.
	if findSearchHook != nil {
		deps.search = findSearchHook
	} else {
		wcli, err := wezcli.NewClient(nil)
		if err != nil {
			// No wezterm on PATH → no search results possible. Surface as
			// a hard error: the find subcommand has nothing to do without
			// access to the mux.
			return deps, fmt.Errorf("wezcli: %w", err)
		}
		deps.search = func(ctx context.Context, pattern string, opts find.Options) ([]find.Match, error) {
			return find.Search(ctx, wcli, pattern, opts)
		}
	}

	// inside-wezterm gate.
	if findInsideHook != nil {
		ictx, err := findInsideHook(ctx, cfg, getEnv)
		if err != nil {
			fmt.Fprintf(stderr, "wezsesh find: inside-wezterm init failed: %v\n", err)
		} else {
			deps.inside = ictx
		}
		return deps, nil
	}

	if getEnv("WEZTERM_PANE") == "" {
		// Outside wezterm — print results only.
		return deps, nil
	}

	ictx, err := buildFindInsideContext(ctx, cfg, getEnv)
	if err != nil {
		// Init failure: degrade to outside-wezterm semantics (print only)
		// per §8.20.1 step 3.
		fmt.Fprintf(stderr, "wezsesh find: inside-wezterm init failed: %v\n", err)
		return deps, nil
	}
	deps.inside = ictx
	return deps, nil
}

// buildFindInsideContext is the production inside-wezterm wiring. It
// mirrors the §8.20.1 step 4 sub-steps that the dispatcher needs:
// resolve paneID → windowID via wcli.List, validate the HMAC key,
// open /dev/tty for the OSC writer, construct the Signer, and call
// ipcdispatcher.New.
//
// The resulting findInsideContext owns a cleanup closure that closes
// the OSC writer and tears down any dispatcher resources.
func buildFindInsideContext(ctx context.Context, cfg *config.Config, getEnv func(string) string) (*findInsideContext, error) {
	paneStr := strings.TrimSpace(getEnv("WEZTERM_PANE"))
	if paneStr == "" {
		return nil, errors.New("WEZTERM_PANE not set")
	}
	paneID, err := strconv.Atoi(paneStr)
	if err != nil || paneID <= 0 {
		return nil, fmt.Errorf("WEZTERM_PANE %q: invalid pane id", paneStr)
	}

	hexKey := strings.TrimSpace(getEnv("WEZSESH_HMAC_KEY"))
	if !hexKey64.MatchString(hexKey) {
		return nil, errBadHMACKey
	}

	wcli, err := wezcli.NewClient(nil)
	if err != nil {
		return nil, fmt.Errorf("wezcli: %w", err)
	}
	panes, err := wcli.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("wezcli list: %w", err)
	}
	windowID, err := resolveWindowID(panes, paneID)
	if err != nil {
		return nil, fmt.Errorf("resolve window id: %w", err)
	}

	signer, err := whmac.NewSigner(hexKey)
	if err != nil {
		return nil, fmt.Errorf("hmac: %w", err)
	}
	uvw, err := uservar.New()
	if err != nil {
		return nil, fmt.Errorf("uservar: %w", err)
	}
	disp, dispCleanup, err := ipcdispatcher.New(ipcdispatcher.Deps{
		Writer:         uvw,
		Signer:         signer,
		RuntimeDir:     cfg.RuntimeDir,
		TargetWindowID: windowID,
		Logger:         nil,
	})
	if err != nil {
		_ = uvw.Close()
		return nil, fmt.Errorf("ipc init: %w", err)
	}

	cleanup := func() {
		// Drain any in-flight drain goroutines first so the dispatcher's
		// per-listener cleanup runs to completion before we close the
		// OSC writer's /dev/tty fd.
		if d, ok := disp.(*ipcdispatcher.Dispatcher); ok {
			d.Wait()
		}
		if dispCleanup != nil {
			dispCleanup()
		}
		_ = uvw.Close()
	}

	return &findInsideContext{
		dispatcher:      disp,
		activator:       defaultFindActivator(wcli),
		currentWindowID: windowID,
		cleanup:         cleanup,
	}, nil
}

// defaultFindActivator wraps find.Activate so the production seam has
// the same shape as the test seam (findActivator). The wezcli.Client
// is captured in the closure; ctx + dispatcher + match thread through
// from the call site.
func defaultFindActivator(c *wezcli.Client) findActivator {
	return func(ctx context.Context, d ipc.Dispatcher, currentWindowID int, match find.Match) error {
		return find.Activate(ctx, d, c, currentWindowID, match, nil)
	}
}

// runFind is the body subcmdFind executes after deps are built. It
// performs the search, picks the top-scored match (when inside
// wezterm + at least one match), runs Phase 1 + Phase 2 via the
// activator, and emits the result rows.
//
// Render order: Activate runs FIRST (so the user sees the workspace
// switch + pane focus before reading the result list); the matches
// table follows. JSON format batches the activated_pane id alongside
// the matches so a single read covers both.
func runFind(ctx context.Context, deps findDeps, flags findFlags, stdout, stderr io.Writer) int {
	matches, err := deps.search(ctx, flags.Pattern, find.Options{
		Deep:      flags.Deep,
		CWDOnly:   flags.CWDOnly,
		Workspace: flags.Workspace,
	})
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh find: search: %v\n", err)
		return exitDoctorOrSubcmd
	}

	rows := buildFindRows(matches)

	var activated *int
	if deps.inside != nil && len(matches) > 0 && deps.inside.activator != nil {
		// Pick the top-scored match (stable: ties fall to the first
		// pane id encountered in the wezcli list output, which mirrors
		// the TUI's row order).
		top := pickTopMatch(matches)
		if err := deps.inside.activator(ctx, deps.inside.dispatcher, deps.inside.currentWindowID, top); err != nil {
			fmt.Fprintf(stderr, "wezsesh find: activate: %v\n", err)
			// The match table still renders — the user can retry the
			// activation manually from a TUI if needed.
		} else {
			id := top.PaneID
			activated = &id
		}
	}

	if err := renderFindRows(stdout, flags.Format, rows, activated); err != nil {
		fmt.Fprintf(stderr, "wezsesh find: render: %v\n", err)
		return exitDoctorOrSubcmd
	}
	return exitOK
}

// buildFindRows projects [find.Match] into the on-the-wire shape. CWD
// passes through nameval.SanitizeForDisplay only on the text rendering
// path (renderFindRows); JSON callers receive the raw decoded path.
func buildFindRows(matches []find.Match) []findResultRow {
	rows := make([]findResultRow, 0, len(matches))
	for _, m := range matches {
		rows = append(rows, findResultRow{
			PaneID:      m.PaneID,
			TabID:       m.TabID,
			WindowID:    m.WindowID,
			Workspace:   m.Workspace,
			Title:       m.Title,
			CWD:         m.CWD,
			Score:       m.Score,
			SourceField: m.SourceField,
		})
	}
	return rows
}

// pickTopMatch returns the highest-scored match, falling back to the
// first match on ties. The TUI's §13.10 sort comparators do richer
// ordering when the user can pick interactively; the find subcommand's
// scripted entry point picks deterministically so a wrapper script
// gets the same behaviour across runs.
func pickTopMatch(matches []find.Match) find.Match {
	best := matches[0]
	for _, m := range matches[1:] {
		if m.Score > best.Score {
			best = m
		}
	}
	return best
}

// renderFindRows emits the rows in the requested format. text format
// is a header line + one row per match. JSON format renders findOutput
// with a trailing newline so terminal cat is readable.
func renderFindRows(w io.Writer, format string, rows []findResultRow, activated *int) error {
	switch format {
	case findFormatJSON:
		if rows == nil {
			rows = []findResultRow{}
		}
		out := findOutput{Matches: rows, ActivatedPane: activated}
		buf, err := json.Marshal(out)
		if err != nil {
			return err
		}
		if _, err := w.Write(buf); err != nil {
			return err
		}
		_, err = w.Write([]byte{'\n'})
		return err
	case findFormatText:
		// Sort rows for stable text output: score descending then pane
		// id ascending. JSON callers get the original Search order
		// (empirically pane-id ordered from `cli list`); the text
		// layout's purpose is human inspection, so a top-scored-first
		// ordering reads better.
		sorted := append([]findResultRow(nil), rows...)
		sort.SliceStable(sorted, func(i, j int) bool {
			if sorted[i].Score != sorted[j].Score {
				return sorted[i].Score > sorted[j].Score
			}
			return sorted[i].PaneID < sorted[j].PaneID
		})
		var b strings.Builder
		b.WriteString("PANE WORKSPACE      SOURCE        SCORE TITLE\n")
		for _, r := range sorted {
			fmt.Fprintf(&b, "%-4d %-15s %-13s %-5d %s\n",
				r.PaneID,
				nameval.SanitizeForDisplay(r.Workspace),
				r.SourceField,
				r.Score,
				nameval.SanitizeForDisplay(r.Title))
		}
		if activated != nil {
			fmt.Fprintf(&b, "activated pane %d\n", *activated)
		}
		_, err := io.WriteString(w, b.String())
		return err
	default:
		return errors.New("unknown format: " + format)
	}
}
