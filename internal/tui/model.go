// Package tui is the bubbletea v2 picker UI surface (§8.16). Verb dispatch
// goes through the ipc.Dispatcher interface (§8.5); the concrete
// Dispatcher lives in internal/ipcdispatcher. CI lint catches direct
// internal/ipcsock callsites here (§14.2 / §16.5).
//
// Hard invariants this package enforces:
//
//   - tea.After does not exist in any released bubbletea (CLAUDE.md).
//     Retransmit is tea.Tick (§14.1 / §14.2).
//   - OSC writes go through internal/uservar.Writer (writes to /dev/tty,
//     not os.Stdout). The dispatcher owns that path; the TUI only calls
//     Dispatcher.Dispatch.
//   - Every disk-sourced string passes through nameval.SanitizeForDisplay
//     before reaching a render call (§15.4 / §17.3 row "Render-time
//     sanitization").
//   - Model carries replyReceived bool; Update ignores retransmitMsg
//     when set (§14.2).
//   - Quit-mid-op uses an inline status line (§13.8); op_in_flight bool
//     is tracked across the lifetime of an in-flight dispatch.
//
// The package is intentionally self-contained: state.Store, snapshot
// repos, find, etc., feed in via the Data struct that cmd/wezsesh
// assembles before constructing the model.
package tui

import (
	"context"
	"sort"
	"sync"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/Grady-Saccullo/wezsesh/internal/ipc"
	"github.com/Grady-Saccullo/wezsesh/internal/logger"
	"github.com/Grady-Saccullo/wezsesh/internal/nameval"
	"github.com/Grady-Saccullo/wezsesh/internal/snapshots"
	"github.com/Grady-Saccullo/wezsesh/internal/state"
)

// SortMode is the §8.16 / §13.10 enum.
type SortMode string

const (
	SortLiveFirst    SortMode = "live_first"
	SortRecent       SortMode = "recent"
	SortMtime        SortMode = "mtime"
	SortAlphabetical SortMode = "alphabetical"
)

// Action is the §8.16 default-action enum.
type Action string

const (
	ActionSwitch Action = "switch"
	ActionLoad   Action = "load"
	ActionNone   Action = "none"
)

// Column is the §8.16 column enum (used by the row renderer).
type Column string

const (
	ColMarker Column = "marker"
	ColName   Column = "name"
	ColTabs   Column = "tabs"
	ColAge    Column = "age"
	ColTags   Column = "tags"
)

// Markers configures the per-row glyphs. Empty strings render as a
// blank cell. External marks rows that came from a dir_providers
// adapter (zoxide, fd, ad-hoc tables, etc.) — not yet a live workspace
// nor a saved snapshot.
type Markers struct {
	Active   string
	Live     string
	Marked   string
	Unsaved  string
	Pinned   string
	External string
}

// Colors holds optional foreground colour overrides. Nil leaves the
// terminal default in place. (Stored as *string for §8.16 parity; the
// v0.1 renderer ignores them — colour theming lands with T-DOC followups.)
type Colors struct {
	Accent         *string
	Muted          *string
	Error          *string
	Success        *string
	FocusBG        *string
	MatchHighlight *string
	LiveMarker     *string
	SavedMarker    *string
}

// KeyMap is the §8.16 binding map. Empty value disables the binding.
type KeyMap struct {
	Switch     string
	Load       string
	Rename     string
	Delete     string
	Save       string
	New        string
	Pin        string
	Tag        string
	Mark       string
	MarkAlt    string
	ClearMarks string
	Help       string
	Filter     string
	Quit       string
	Up         string
	Down       string
	Top        string
	Bottom     string
	Preview    string // not in §8.16 prose; load-bearing for `P` toggle (T-DOC followup).
}

// DefaultKeyMap returns the §0.5 / PRD baseline bindings. Used by tests
// and any caller that does not want to author a full KeyMap.
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Switch:     "enter",
		Load:       "l",
		Rename:     "r",
		Delete:     "d",
		Save:       "s",
		New:        "n",
		Pin:        "p",
		Tag:        "t",
		Mark:       " ",
		MarkAlt:    "m",
		ClearMarks: "c",
		Help:       "?",
		Filter:     "/",
		Quit:       "q",
		Up:         "k",
		Down:       "j",
		Top:        "g",
		Bottom:     "G",
		Preview:    "P",
	}
}

// Config is the picker configuration assembled by cmd/wezsesh from
// internal/config + env overrides (§11.4). Pure data; no I/O.
type Config struct {
	Sort                      SortMode
	DefaultAction             Action
	DefaultActionLoadNoPrompt bool
	PreviewEnabled            bool
	PreviewWidth              float64
	Markers                   Markers
	Columns                   []Column
	NameTruncate              string // "middle" only in v0.1 (§15.5)
	Colors                    Colors
	Keys                      KeyMap
	ConfirmDelete             bool
	ConfirmOverwrite          bool
}

// Data is the initial picker payload assembled by cmd/wezsesh. The TUI
// does not perform its own filesystem scans; the caller composes
// snapshot rows + state usage + active workspace ahead of time.
type Data struct {
	Workspaces      []WorkspaceRow
	State           *state.Store // may be nil in tests; LastSwitched / SwitchCount fall through
	ActiveWorkspace string
	ActiveWindowID  int
}

// Source classifies where a picker row came from. The value drives
// dispatch routing (live/saved use cwd:"" on switch; external passes
// the provider's CWD), preview rendering, and the live > saved >
// external sort order.
type Source int

const (
	// SourceLive — the workspace currently exists in the wezterm mux.
	SourceLive Source = iota
	// SourceSaved — a snapshot file exists for the name (no live mux entry).
	SourceSaved
	// SourceExternal — a dir_providers adapter surfaced this row at
	// runtime; not yet a live workspace nor a saved snapshot. Selecting
	// it dispatches `switch` with the provider-supplied CWD so the
	// rename-trick path can `cd` the active pane after the rename.
	SourceExternal
)

// WorkspaceRow is one picker row. Snapshot is non-nil when a snapshot
// file backs the row (used by the preview pane). Active is meaningful
// only for SourceLive; CWD is meaningful only for SourceExternal.
type WorkspaceRow struct {
	Name     string
	Source   Source
	Active   bool
	Tabs     int
	Mtime    time.Time
	Tags     []string
	Pinned   bool
	CWD      string
	Snapshot *snapshots.Entry
}

// retransmitMsg is the tea.Tick fired 2 s after dispatch start (§14.1).
// Update short-circuits when replyReceived is already set.
type retransmitMsg struct {
	dispatchID uint64 // matches Model.dispatchSeq at the time the timer was scheduled.
}

// timeoutMsg is the 5 s IPC timeout (§14.1).
type timeoutMsg struct {
	dispatchID uint64
}

// dispatchStartedMsg is the result of the Cmd that synchronously called
// Dispatcher.Dispatch from Update. The Cmd runs synchronously so the
// dispatch handshake (StartListener, OSC write) completes before the
// next Update tick — the message is just the channel handle.
//
// err non-nil means Dispatch itself failed before any reply could be
// observed (e.g., listener bind, OSC ceiling).
type dispatchStartedMsg struct {
	dispatchID uint64
	verb       string
	target     string
	ch         <-chan ipc.Reply
	err        error
}

// replyMsg is the result of a `waitForReply` Cmd: one Reply was read
// from the dispatcher's channel. closed=true means the channel was
// drained (terminal reply already delivered, or ctx cancelled).
type replyMsg struct {
	dispatchID uint64
	reply      ipc.Reply
	closed     bool
}

// mode tracks the modal layer the picker is in. Two-mode discipline:
// nav vs filter. Quit-confirm overlays as an inline bar but does NOT
// claim its own mode — letter keys still need to be evaluated against
// the underlying nav/filter discipline once dismissed.
type mode int

const (
	modeNav mode = iota
	modeFilter
)

// Model is the §8.16 bubbletea model. Fields are intentionally
// unexported per the spec.
type Model struct {
	cfg  Config
	data Data
	disp ipc.Dispatcher

	// log is the structured logger threaded through from cmd/wezsesh
	// (env.log). Nil-safe: every (*logger.Logger) method short-circuits
	// on a nil receiver, so tests that construct Model directly can pass
	// nil and observability calls become no-ops. The logger's Warn/Error
	// fast-path sync-flushes (logger.go:125-143) so an IPC_TIMEOUT line
	// survives even if the user closes the TUI immediately afterwards.
	log *logger.Logger

	// rows is the sanitised + sorted workspace list. Sanitisation is
	// pre-applied at construction so the per-render hot path does not
	// re-allocate; Mtime / Pinned / etc. survive verbatim.
	rows []WorkspaceRow

	// filtered is the indices into rows that match the current filter
	// buffer. Empty filter ⇒ filtered == nil (treated as "all").
	filtered []int

	cursor int // index into the visible (filtered or all) row list.

	// Modal state.
	mode         mode
	filterBuf    string
	previewShown bool
	help         bool

	// Quit-mid-op inline status. confirmQuit toggles when the user
	// presses q/Esc while op_in_flight is true. It is NOT a modal — the
	// next non-y key dismisses it and lets the model continue.
	confirmQuit bool

	// modal is the active overlay (rename, tag-edit, confirm-delete,
	// new-workspace). modalNone when there's no overlay. Update routes
	// keys to the modal first when present.
	modal modalKind

	// textInput backs the active textinput-style modal (currently only
	// modalNewWorkspace). Nil when no input modal is open. The
	// constructor lazily allocates so non-modal renders don't pay any
	// allocation cost.
	textInput *textinput.Model

	// Dispatch tracking.
	opInFlight    bool
	replyReceived bool
	dispatchSeq   uint64 // monotonic ID for matching tea.Tick deliveries to the in-flight op.
	currentVerb   string
	currentTarget string

	// status / toast text. In v0.1 there is one inline status string;
	// the picker renders it on the bottom hint row.
	status string

	// terminal dims (from WindowSizeMsg).
	width, height int

	// quitting marks the model as on the way to tea.Quit so the View
	// renders an empty frame on the final tick.
	quitting bool

	// closeOwnPane signals the host (cmd/wezsesh runTUI) to close the
	// wezterm pane the binary is running in once tea.Run has returned.
	// The TUI is one-shot — leaving the pane behind as a "Process
	// completed" placeholder when the user has `exit_behavior = "Hold"`
	// is poor UX, so every clean tea.Quit path stamps this true.
	closeOwnPane bool

	// reply mailbox: the channel returned from Dispatcher.Dispatch.
	// Set by dispatchStartedMsg, cleared on terminal reply or timeout.
	replyCh <-chan ipc.Reply

	// nowFn is a test seam for deterministic timestamps.
	nowFn func() time.Time

	// retransmitDelay / timeoutDelay are tunable via WithTimings (test only).
	retransmitDelay time.Duration
	timeoutDelay    time.Duration

	// dispatchCtx wraps the lifetime of the most recent in-flight
	// dispatch; cancelled on terminal reply, timeout, or model exit so
	// any goroutine still draining replyCh can drop out.
	dispatchCtx    context.Context //nolint:containedctx // request lifetime, not bg.
	dispatchCancel context.CancelFunc

	// shutdownOnce guards the cancellation that runs at tea.Quit time
	// so the test goroutines do not panic on a double-cancel.
	shutdownOnce sync.Once

	// styles is the cached lipgloss style set, built once from
	// Config.Colors at construction time.
	styles styles
}

// Option is an additive constructor knob. Used so cmd/wezsesh can plumb
// the structured logger (and any future field) into Model without
// changing the existing 3-arg call shape.
type Option func(*Model)

// WithLogger threads the binary-side *logger.Logger into Model so the
// timeout / failure code paths inside Update can write structured log
// lines to <state>/wezsesh.log. Pass nil to disable (the logger
// methods are nil-safe; calls become no-ops). Production wires this
// from env.log; tests typically omit it.
func WithLogger(log *logger.Logger) Option {
	return func(m *Model) {
		m.log = log
	}
}

// New constructs the §8.16 Model. The returned tea.Model is *Model
// directly; pointer-receiver Update / View / Init satisfy the interface.
//
// opts is a variadic options slice (currently WithLogger only). Keeping
// the optional fields out of the positional signature lets cmd/wezsesh
// add observability without ripple-editing every existing call site.
func New(cfg Config, initial Data, d ipc.Dispatcher, opts ...Option) tea.Model {
	return newModel(cfg, initial, d, opts...)
}

// newModel is the internal constructor used by tests so they can keep
// the *Model receiver type without round-tripping through tea.Model.
func newModel(cfg Config, initial Data, d ipc.Dispatcher, opts ...Option) *Model {
	if cfg.Keys == (KeyMap{}) {
		cfg.Keys = DefaultKeyMap()
	}
	if cfg.Sort == "" {
		cfg.Sort = SortLiveFirst
	}
	if cfg.NameTruncate == "" {
		cfg.NameTruncate = "middle"
	}
	if cfg.PreviewWidth <= 0 {
		cfg.PreviewWidth = 0.4
	}
	rows := sanitiseRows(initial.Workspaces)
	sortRows(rows, cfg.Sort, initial.State)
	m := &Model{
		cfg:             cfg,
		data:            initial,
		disp:            d,
		rows:            rows,
		mode:            modeNav,
		previewShown:    cfg.PreviewEnabled,
		nowFn:           time.Now,
		retransmitDelay: 2 * time.Second,
		timeoutDelay:    5 * time.Second,
		styles:          buildStyles(cfg.Colors),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}
	return m
}

// sanitiseRows runs every disk-sourced string field through
// SanitizeForDisplay so the renderer's hot path never sees raw bytes.
// The terminal-injection acceptance test ("snapshot named \x1b[2J does
// not clear terminal") is what this guards. CWD is disk-sourced too
// (provider output) and joins the sanitisation set.
func sanitiseRows(in []WorkspaceRow) []WorkspaceRow {
	if len(in) == 0 {
		return nil
	}
	out := make([]WorkspaceRow, len(in))
	for i, r := range in {
		r.Name = nameval.SanitizeForDisplay(r.Name)
		if r.CWD != "" {
			r.CWD = nameval.SanitizeForDisplay(r.CWD)
		}
		if len(r.Tags) > 0 {
			t := make([]string, len(r.Tags))
			for j, tag := range r.Tags {
				t[j] = nameval.SanitizeForDisplay(tag)
			}
			r.Tags = t
		}
		out[i] = r
	}
	return out
}

// sortRows applies §13.10 sort comparators in place. State is used by
// SortRecent (LastSwitched lookup); nil-safe.
func sortRows(rows []WorkspaceRow, mode SortMode, st *state.Store) {
	switch mode {
	case SortAlphabetical:
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].Pinned != rows[j].Pinned {
				return rows[i].Pinned
			}
			return rows[i].Name < rows[j].Name
		})
	case SortMtime:
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].Pinned != rows[j].Pinned {
				return rows[i].Pinned
			}
			a, b := rows[i].Mtime, rows[j].Mtime
			if a.IsZero() && !b.IsZero() {
				return false
			}
			if !a.IsZero() && b.IsZero() {
				return true
			}
			if !a.Equal(b) {
				return a.After(b)
			}
			return rows[i].Name < rows[j].Name
		})
	case SortRecent:
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].Pinned != rows[j].Pinned {
				return rows[i].Pinned
			}
			ai, bi := lastSwitched(st, rows[i].Name), lastSwitched(st, rows[j].Name)
			if ai != bi {
				return ai > bi
			}
			a, b := rows[i].Mtime, rows[j].Mtime
			if !a.Equal(b) {
				return a.After(b)
			}
			return rows[i].Name < rows[j].Name
		})
	default: // SortLiveFirst
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].Pinned != rows[j].Pinned {
				return rows[i].Pinned
			}
			// Source enum iota is ordered live(0) < saved(1) < external(2),
			// so a smaller Source value sorts first — yielding the
			// "live > saved > external" rank.
			if rows[i].Source != rows[j].Source {
				return rows[i].Source < rows[j].Source
			}
			if rows[i].Source == SourceLive {
				if rows[i].Active != rows[j].Active {
					return rows[i].Active
				}
			}
			a, b := rows[i].Mtime, rows[j].Mtime
			if a.IsZero() && !b.IsZero() {
				return false
			}
			if !a.IsZero() && b.IsZero() {
				return true
			}
			if !a.Equal(b) {
				return a.After(b)
			}
			return rows[i].Name < rows[j].Name
		})
	}
}

func lastSwitched(st *state.Store, name string) int64 {
	if st == nil {
		return 0
	}
	return st.LastSwitched(name)
}

// visibleRows returns the indices into m.rows currently visible (filter
// or all). The result is a fresh slice safe for the renderer to range.
func (m *Model) visibleRows() []int {
	if m.filtered != nil {
		return m.filtered
	}
	out := make([]int, len(m.rows))
	for i := range m.rows {
		out[i] = i
	}
	return out
}

// rowAt returns the row at visible index i, or zero-value + false if
// the cursor is somehow off-screen.
func (m *Model) rowAt(i int) (WorkspaceRow, bool) {
	visible := m.visibleRows()
	if i < 0 || i >= len(visible) {
		return WorkspaceRow{}, false
	}
	return m.rows[visible[i]], true
}

// now returns the model's current time, routed through the nowFn test
// seam so the renderer (humanAge in view.go / preview.go) is
// deterministic in snapshot tests. Production wires nowFn=time.Now.
func (m *Model) now() time.Time {
	if m.nowFn != nil {
		return m.nowFn()
	}
	return time.Now()
}

// shutdown cancels any in-flight dispatch context. Idempotent.
func (m *Model) shutdown() {
	m.shutdownOnce.Do(func() {
		if m.dispatchCancel != nil {
			m.dispatchCancel()
		}
	})
}

// CloseOwnPaneOnExit reports whether the host should close the wezterm
// pane the binary was spawned into once tea.Run returns. Stamped by
// every clean tea.Quit path in update.go; the panic-recovery branch
// leaves it false so the user can read the "Process completed" /
// stderr message in the lingering pane.
func (m *Model) CloseOwnPaneOnExit() bool {
	return m.closeOwnPane
}
