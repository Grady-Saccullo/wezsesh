// Package find implements the §8.14 search + §13.7 two-phase activation
// flow for `wezsesh find`. The package is the sole consumer of
// internal/wezcli's Phase 1 / Phase 2 seams, plus the only place outside
// internal/tui that issues a `switch` verb to the Lua plugin.
//
// Two-phase invariant (§13.7, §0.1 row 18). Cross-workspace
// `cli activate-pane` does NOT change the user's active workspace —
// upstream wezterm's `SetFocusedPane` handler skips
// `mux.set_active_workspace_for_client(...)`. Without an explicit Phase 1
// (programmatic `act.SwitchToWorkspace` via the dispatcher + a switch
// poller waiting for the mux to actually settle on the target workspace),
// `wezsesh find` of a cross-workspace pane appears to do nothing. Activate
// therefore ALWAYS runs Phase 1 when `match.Workspace !=
// currentActiveWorkspace`; same-workspace finds skip directly to Phase 2.
//
// Drain protocol (§0.1 row 18). After the switch poller returns success,
// Activate calls dispCancel() then drains the replies channel until the
// dispatcher closes it. Skipping the drain leaks the dispatcher's drain
// goroutine and races with Phase 2 on the reply socket; CI's goleak gate
// catches the regression.
//
// Test seam. `Activate` accepts a `*wezcli.Client` per the §8.14 surface
// but routes its mux operations through the unexported `muxOps` interface
// (which `*wezcli.Client` satisfies). Tests inject a fake muxOps via
// `activate` (lower-case); production callers go through `Activate`.
package find

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/Grady-Saccullo/wezsesh/internal/ipc"
	"github.com/Grady-Saccullo/wezsesh/internal/wezcli"
)

// Phase enumerates the §8.14 progress callback values. The TUI renders
// a one-line progress status on each transition (§13.7).
type Phase string

const (
	PhaseSwitchStarted   Phase = "switch_started"
	PhaseSwitchSucceeded Phase = "switch_succeeded"
	PhaseSwitchTimeout   Phase = "switch_timeout"
	PhaseActivateStarted Phase = "activate_started"
	PhaseActivateDone    Phase = "activate_done"
)

// Options controls Search behaviour. Empty Workspace means "all
// workspaces"; non-empty restricts to that workspace.
type Options struct {
	Deep      bool
	CWDOnly   bool
	Workspace string
}

// Match is the §8.14 result row. SourceField records which haystack
// produced the hit so the TUI can render an annotation.
type Match struct {
	PaneID      int
	TabID       int
	WindowID    int
	Workspace   string
	Title       string
	CWD         string
	Score       int
	SourceField string // "title"|"tab_title"|"window_title"|"cwd"|"ps"
}

// Search issues a single `cli list` and filters panes whose haystacks
// match `pattern` (case-insensitive substring). The score is currently a
// crude length-of-needle relative to length-of-haystack heuristic; the
// detailed scoring lives in T-700+ once the TUI threads it through. Empty
// pattern returns every pane that survives the Workspace / CWDOnly
// filters, with Score=0.
//
// `--deep` mode (Options.Deep) is NOT implemented here — that's the §8.14.1
// `ps -t` walk and lives in T-805's CLI surface. Callers passing Deep=true
// today get the same shallow scan; a slog WARN surfaces so the dropped flag
// is visible in the log until T-805 wires it.
func Search(ctx context.Context, c *wezcli.Client, pattern string, opts Options) ([]Match, error) {
	return search(ctx, c, pattern, opts)
}

// search is the test-seam variant of Search. It accepts the unexported
// `muxOps` interface so tests can drive a fake without depending on the
// concrete wezcli.Client.
func search(ctx context.Context, m muxOps, pattern string, opts Options) ([]Match, error) {
	if opts.Deep {
		// Deep mode is the §8.14.1 ps-walk; T-805 wires the CLI flag. Log
		// here so callers passing Deep=true today see the silent fallback
		// in the structured log rather than mistaking the shallow scan
		// for the deep behaviour.
		slog.Warn("find: --deep ignored (not implemented; tracked in T-805); using shallow scan")
	}
	panes, err := m.List(ctx)
	if err != nil {
		return nil, err
	}
	needle := strings.ToLower(pattern)
	out := make([]Match, 0, len(panes))
	for _, p := range panes {
		if opts.Workspace != "" && p.Workspace != opts.Workspace {
			continue
		}
		cwd, cwdOK := p.CWDPath()
		if opts.CWDOnly && !cwdOK {
			continue
		}
		score, source, ok := matchPane(needle, p, cwd, cwdOK, opts.CWDOnly)
		if !ok {
			continue
		}
		out = append(out, Match{
			PaneID:      p.PaneID,
			TabID:       p.TabID,
			WindowID:    p.WindowID,
			Workspace:   p.Workspace,
			Title:       p.Title,
			CWD:         cwd,
			Score:       score,
			SourceField: source,
		})
	}
	return out, nil
}

// matchPane runs the haystack scan for a single pane. CWDOnly forces the
// match to come from the cwd column; otherwise the first hit across
// title / tab_title / window_title / cwd wins.
//
// An empty needle matches every pane (score 0) and reports the cwd column
// when CWDOnly is set, else "title".
func matchPane(needle string, p wezcli.Pane, cwd string, cwdOK, cwdOnly bool) (score int, source string, ok bool) {
	if cwdOnly {
		if !cwdOK {
			return 0, "", false
		}
		if needle == "" {
			return 0, "cwd", true
		}
		if strings.Contains(strings.ToLower(cwd), needle) {
			return scoreSubstring(needle, cwd), "cwd", true
		}
		return 0, "", false
	}
	if needle == "" {
		return 0, "title", true
	}
	for _, h := range []struct {
		val string
		src string
	}{
		{p.Title, "title"},
		{p.TabTitle, "tab_title"},
		{p.WindowTitle, "window_title"},
	} {
		if strings.Contains(strings.ToLower(h.val), needle) {
			return scoreSubstring(needle, h.val), h.src, true
		}
	}
	if cwdOK && strings.Contains(strings.ToLower(cwd), needle) {
		return scoreSubstring(needle, cwd), "cwd", true
	}
	return 0, "", false
}

// scoreSubstring is a small relative-coverage heuristic so longer
// substring matches outrank tiny-needle matches in a giant haystack. The
// real scoring threads through the §13.10 sort comparators in T-700+.
func scoreSubstring(needle, haystack string) int {
	if haystack == "" {
		return 0
	}
	return (len(needle) * 100) / len(haystack)
}

// muxOps is the subset of wezcli.Client that the find package needs.
// Defining the seam here (rather than re-exporting from wezcli) keeps
// the public §8.14 surface concrete while letting tests inject a fake.
//
// *wezcli.Client satisfies muxOps trivially.
type muxOps interface {
	List(ctx context.Context) ([]wezcli.Pane, error)
	ActivatePane(ctx context.Context, paneID int) error
	CapturePreSwitchState(ctx context.Context, targetWindowID int) (wezcli.SwitchPreState, error)
	StartSwitchPoller(ctx context.Context, pre wezcli.SwitchPreState, target string, isRestoreFlow bool) error
}

// Activate performs the two-phase find sequence. Phase 1 (workspace
// switch) runs whenever the match is not already in the user's active
// workspace+window pair; Phase 2 (pane activation) ALWAYS runs.
//
// `currentWindowID` is the user's currently-focused window id at the
// moment Find was invoked. §13.7 / PRD §6.13 step 1 calls for it
// explicitly: the poller's success predicate matches `pane.WindowID ==
// pre.TargetWindowID`, and that field is sourced HERE (the user's
// current window), NOT from `match.WindowID` (which is the target pane's
// window — the destination, not the origin). Passing `match.WindowID`
// would make the predicate impossible to satisfy for any cross-window
// find: the poller would tick to its 5 s ceiling and surface
// ErrMuxUnreachable.
//
// Errors:
//   - wezcli.ErrMuxUnreachable surfaces from CapturePreSwitchState or the
//     switch poller (5 s ceiling). Phase 2's ActivatePane surfaces
//     ErrMuxUnreachable on a hard mux failure.
//   - wezcli.ErrPaneClosedRace surfaces from ActivatePane if the target
//     pane went away between Phase 1 and Phase 2. The two-phase find
//     widens this race window slightly (5 s vs ~0 ms in same-workspace
//     finds); the wezcli layer's single-retry covers it.
//   - errors from ipc.Dispatcher (notably ctx cancellation) surface as
//     a wrapping of the dispatcher error.
//
// progress is invoked synchronously at every phase transition. nil is
// permitted (used by non-TUI callers).
func Activate(ctx context.Context, d ipc.Dispatcher, c *wezcli.Client, currentWindowID int, match Match, progress func(Phase)) error {
	return activate(ctx, d, c, currentWindowID, match, progress)
}

// activate is the test-seam variant. It takes muxOps directly so tests
// can plug in a fake mux without involving the wezcli.Client surface.
func activate(ctx context.Context, d ipc.Dispatcher, m muxOps, currentWindowID int, match Match, progress func(Phase)) error {
	if d == nil {
		return errors.New("find: nil dispatcher")
	}
	if m == nil {
		return errors.New("find: nil mux ops")
	}
	emit := func(p Phase) {
		if progress != nil {
			progress(p)
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// Phase 1 setup: capture the pinned client + active workspace BEFORE
	// emitting the switch. The pinned client_id is what the poller uses;
	// re-evaluating "most recent" mid-poll would let a second wezterm
	// client flip the predicate to the wrong workspace (§0.1 row 18).
	//
	// `currentWindowID` (the user's current window) — NOT `match.WindowID`
	// (the target pane's window) — is what the poller predicate compares
	// against `pane.WindowID`. PRD §6.13 step 1 captures this field as
	// `target_window_id` of the focused client; we accept it as a parameter
	// so callers (cmd/wezsesh) thread the value from the §6.5 capture into
	// Find without reaching back into wezcli for a second probe.
	pre, err := m.CapturePreSwitchState(ctx, currentWindowID)
	if err != nil {
		return err
	}

	// §13.7 skip-Phase-1 condition: skip ONLY when the match is already in
	// the user's active workspace AND already in the user's current window.
	// A same-workspace cross-window find still requires Phase 1 — without
	// it, `cli activate-pane` won't move the user's focus across windows
	// the way they expect. Either inequality (workspace OR window) drops
	// us into Phase 1.
	if match.Workspace != pre.ActiveWorkspace || match.WindowID != pre.TargetWindowID {
		if err := runPhase1(ctx, d, m, pre, match, emit); err != nil {
			return err
		}
	}

	emit(PhaseActivateStarted)
	if err := m.ActivatePane(ctx, match.PaneID); err != nil {
		return err
	}
	emit(PhaseActivateDone)
	return nil
}
