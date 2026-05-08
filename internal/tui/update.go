package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Grady-Saccullo/wezsesh/internal/canonicaljson"
	"github.com/Grady-Saccullo/wezsesh/internal/ipc"
	"github.com/Grady-Saccullo/wezsesh/internal/nameval"
)

// dispatchSeqCounter is a process-global monotonic counter for matching
// tea.Tick deliveries to the in-flight op. The model keeps its own
// dispatchSeq snapshot so an in-flight op's Tick is ignored once the
// model has moved on (terminal reply, timeout, or manual cancellation).
var dispatchSeqCounter uint64

// verbExitsOnSuccess returns true for verbs whose successful terminal
// reply ends the picker's purpose: the user has navigated away and the
// TUI tab is now in the way. The remaining verbs (`save`, `delete`,
// `rename`, `new`, `list_dirs`, etc.) keep the picker open so the user
// can do more work. `new` deliberately stays open: the binary spawns
// the workspace without switching the active client into it, and the
// picker re-surfaces the freshly-created row with the cursor on it so
// the user can decide whether to switch (`s`) or stay. `list_dirs` is
// a startup data fetch — its reply trickles external rows into the
// picker, never quits.
func verbExitsOnSuccess(verb string) bool {
	switch verb {
	case "switch", "load":
		return true
	}
	return false
}

// Init satisfies tea.Model. Fires the startup `list_dirs` dispatch so
// the picker can surface external (provider-supplied) rows alongside
// live + saved entries. The reply is merged into m.rows by the
// list_dirs branch in Update; failures degrade to "no external rows"
// without blocking the picker.
func (m *Model) Init() tea.Cmd {
	return m.startDispatch("list_dirs", "", map[string]any{"query": ""})
}

// Update handles every tea.Msg. The function is intentionally large but
// flat — each branch is one screen tall and self-contained — so the
// invariants (replyReceived guard, op_in_flight bookkeeping, mode
// discipline) are visible at a glance.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case dispatchStartedMsg:
		// dispatchStartedMsg is the synchronous result of the Cmd that
		// called Dispatcher.Dispatch. err non-nil means the dispatch
		// never made it onto the wire (listener bind failed, OSC ceiling,
		// etc.). The op is no longer in flight.
		if msg.dispatchID != m.dispatchSeq {
			return m, nil // stale; another op already replaced this one.
		}
		if msg.err != nil {
			m.opInFlight = false
			m.replyCh = nil
			m.status = nameval.SanitizeForDisplay(
				fmt.Sprintf("dispatch failed: %s", msg.err.Error()))
			return m, nil
		}
		m.replyCh = msg.ch
		m.currentVerb = msg.verb
		m.currentTarget = msg.target
		// Schedule the §14.1 retransmit + IPC timeout. Both are tea.Tick
		// (NOT tea.After / time.AfterFunc) so the goroutines exit cleanly
		// when tea.Run returns.
		return m, tea.Batch(
			m.scheduleRetransmit(msg.dispatchID),
			m.scheduleTimeout(msg.dispatchID),
			m.waitForReply(msg.dispatchID, msg.ch),
		)

	case replyMsg:
		if msg.dispatchID != m.dispatchSeq {
			return m, nil
		}
		if msg.closed {
			// Channel drained — terminal already delivered or ctx cancelled.
			m.finishOp("")
			return m, nil
		}
		m.replyReceived = true
		// Non-terminal "started" replies leave op_in_flight true; the
		// dispatcher will deliver another reply (terminal). Terminal
		// replies are "completed" or "partial" (§13.1).
		if msg.reply.Status == "started" {
			// Continue draining for the follow-up.
			return m, m.waitForReply(msg.dispatchID, m.replyCh)
		}
		// Terminal reply.
		status := ""
		if msg.reply.OK {
			status = nameval.SanitizeForDisplay(
				fmt.Sprintf("%s completed", m.currentVerb))
		} else if msg.reply.Error != nil {
			status = nameval.SanitizeForDisplay(
				fmt.Sprintf("%s failed: %s", m.currentVerb, msg.reply.Error.Message))
		} else {
			status = nameval.SanitizeForDisplay(
				fmt.Sprintf("%s: %s", m.currentVerb, msg.reply.Status))
		}
		// Capture verb before finishOp clears m.currentVerb; the post-
		// finish handler for `new` needs it to gate the row insert.
		verb := m.currentVerb
		var newName string
		if verb == "new" && msg.reply.OK {
			if v, ok := msg.reply.Data["name"].(string); ok {
				newName = v
			}
		}
		var dirRows []map[string]any
		if verb == "list_dirs" && msg.reply.OK {
			if raw, ok := msg.reply.Data["dirs"].([]any); ok {
				for _, e := range raw {
					if entry, ok := e.(map[string]any); ok {
						dirRows = append(dirRows, entry)
					}
				}
			}
		}

		autoQuit := msg.reply.OK && verbExitsOnSuccess(verb)
		m.finishOp(status)
		if autoQuit {
			m.quitting = true
			m.closeOwnPane = true
			m.shutdown()
			return m, tea.Quit
		}
		if newName != "" {
			m.applyNewWorkspace(newName)
		}
		if verb == "list_dirs" && len(dirRows) > 0 {
			m.applyExternalDirs(dirRows)
		}
		// Continue reading until the channel closes so we observe the
		// drain goroutine's clean-up; the dispatcher closes the channel
		// after a terminal reply.
		return m, m.waitForReply(msg.dispatchID, m.replyCh)

	case retransmitMsg:
		// The §14.2 replyReceived guard. Without it the second OSC fires
		// for ops that completed in <2 s (replay-guard suppressed on the
		// Lua side, but it is still a wasted roundtrip).
		if msg.dispatchID != m.dispatchSeq {
			return m, nil
		}
		if m.replyReceived || !m.opInFlight {
			return m, nil
		}
		// In a fully wired binary the retransmit re-emits the SAME
		// pointer OSC against the existing request file. The current
		// Dispatcher seam does not expose a Retransmit verb yet — that
		// surface lands with the reply-socket lifecycle work. For T-701
		// we surface a status hint so the gate ("timer goroutine exits
		// within 100 ms of tea.Run return") is testable: the Cmd has
		// already fired, the model state is updated, and no new
		// goroutine is spawned.
		statusMsg := fmt.Sprintf("%s: awaiting reply (retransmit due)", m.currentVerb)
		if m.currentTarget != "" {
			statusMsg = fmt.Sprintf("%s %s: awaiting reply (retransmit due)",
				m.currentVerb, m.currentTarget)
		}
		m.status = nameval.SanitizeForDisplay(statusMsg)
		return m, nil

	case timeoutMsg:
		if msg.dispatchID != m.dispatchSeq {
			return m, nil
		}
		if m.replyReceived || !m.opInFlight {
			return m, nil
		}
		// Structured log BEFORE finishOp clears m.currentVerb /
		// m.currentTarget. m.currentTarget is disk-sourced (a workspace
		// name) and must be sanitised before it lands in the JSON log
		// line; m.currentVerb comes from a fixed catalog and is safe
		// verbatim. Logger.Error sync-flushes (logger.go:125-143) so the
		// line survives even if the user closes the TUI immediately.
		m.log.Error("ipc timeout",
			"verb", m.currentVerb,
			"target", nameval.SanitizeForDisplay(m.currentTarget),
			"dispatch_id", msg.dispatchID)
		m.finishOp(nameval.SanitizeForDisplay(
			fmt.Sprintf("%s: IPC_TIMEOUT", m.currentVerb)))
		return m, nil
	}
	return m, nil
}

// handleKey routes a key press through the modal discipline. The two
// modes (nav, filter) plus the inline quit-confirm overlay are the only
// state machines that touch user input.
func (m *Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Modal overlays claim the key stream first. Currently only
	// modalNewWorkspace ships a real handler; the others (rename,
	// tag_edit, confirm_delete) are reserved enums.
	if m.activeModal() == modalNewWorkspace {
		return m.handleNewWorkspaceKey(msg)
	}

	// Quit-mid-op inline confirm overlay. Per §13.8: y → quit; any
	// other key dismisses the prompt and returns control to the
	// underlying mode.
	if m.confirmQuit {
		switch msg.String() {
		case "y", "Y":
			m.quitting = true
			m.closeOwnPane = true
			m.shutdown()
			return m, tea.Quit
		}
		m.confirmQuit = false
		m.status = ""
		return m, nil
	}

	var action keyAction
	var r rune
	if m.mode == modeFilter {
		action, r = matchFilterKey(msg)
	} else {
		action = matchNavKey(m.cfg.Keys, msg)
	}

	switch action {
	case actUp:
		if m.cursor > 0 {
			m.cursor--
		}
	case actDown:
		visible := m.visibleRows()
		if m.cursor+1 < len(visible) {
			m.cursor++
		}
	case actTop:
		m.cursor = 0
	case actBottom:
		visible := m.visibleRows()
		if len(visible) > 0 {
			m.cursor = len(visible) - 1
		}
	case actSwitch, actLoad:
		row, ok := m.rowAt(m.cursor)
		if !ok {
			return m, nil
		}
		verb := "switch"
		if action == actLoad {
			verb = "load"
		}
		var args map[string]any
		if verb == "switch" {
			cwd := ""
			if row.Source == SourceExternal {
				cwd = row.CWD
			}
			args = map[string]any{"name": row.Name, "cwd": cwd}
		}
		return m, m.startDispatch(verb, row.Name, args)
	case actSave:
		// §6.3 save: name = the active workspace (the one whose state
		// we'd be persisting). Cursor row is irrelevant — wezterm.mux
		// only exposes the live workspace's state. expected_hash=null
		// disables the §13.4 race gate; overwrite=true keeps the v0.1
		// UX simple. The confirm-overwrite modal is v0.2 work.
		name := m.data.ActiveWorkspace
		if name == "" {
			if row, ok := m.rowAt(m.cursor); ok && row.Source == SourceLive {
				name = row.Name
			}
		}
		if name == "" {
			m.status = nameval.SanitizeForDisplay(
				"save: no active workspace to save")
			return m, nil
		}
		return m, m.startDispatch("save", name, map[string]any{
			"name":          name,
			"overwrite":     true,
			"expected_hash": canonicaljson.Null,
		})
	case actNew:
		// §6.4 new: prompts for the workspace name. cwd defaults to
		// empty so wezterm picks a sensible default (typically the
		// spawning pane's cwd). A future revision may add a second
		// input line for cwd selection.
		m.openNewWorkspaceModal()
		return m, m.textInput.Focus()
	case actFilterEnter:
		m.mode = modeFilter
		m.filterBuf = ""
		m.refreshFilter()
	case actFilterExit:
		m.mode = modeNav
		m.filterBuf = ""
		m.refreshFilter()
	case actFilterClear:
		m.filterBuf = ""
		m.refreshFilter()
	case actFilterDel:
		if len(m.filterBuf) > 0 {
			// Trim one rune from the right.
			r := []rune(m.filterBuf)
			m.filterBuf = string(r[:len(r)-1])
			m.refreshFilter()
		}
	case actHelp:
		m.help = !m.help
	case actPreview:
		m.previewShown = !m.previewShown
	case actQuit:
		if m.opInFlight {
			// §13.8: render inline status; do NOT quit yet.
			m.confirmQuit = true
			m.status = "op in progress, quit anyway? [y/N]"
			return m, nil
		}
		m.quitting = true
		m.closeOwnPane = true
		m.shutdown()
		return m, tea.Quit
	case actMark, actClearMarks, actPin:
		// Marks / pins land in T-702+; for v0.1 the action surface is
		// stubbed so the bindings parse without crashing. The status
		// line surfaces the no-op so users see we received the key.
		m.status = nameval.SanitizeForDisplay("not implemented yet")
	case actNone:
		if m.mode == modeFilter && r != 0 {
			m.filterBuf += string(r)
			m.refreshFilter()
		}
	}
	return m, nil
}

// startDispatch kicks off a verb against `target`. It allocates the
// dispatch sequence, marks op_in_flight, and returns a Cmd whose body
// performs Dispatcher.Dispatch synchronously. The synchronous call is
// what satisfies the §14.2 / §17.3 gate ("StartListener called from
// Update synchronously"): the dispatcher's StartListener runs inside
// Dispatch before the OSC is emitted.
//
// args is the verb-specific argument map (per `verb_args_shape` on the
// Lua side). When nil, defaults to `{"name": target}` — the shape that
// switch and load expect. Save and new pass their own maps.
func (m *Model) startDispatch(verb, target string, args map[string]any) tea.Cmd {
	if m.disp == nil {
		m.status = nameval.SanitizeForDisplay("dispatch unavailable")
		return nil
	}
	id := atomic.AddUint64(&dispatchSeqCounter, 1)
	m.dispatchSeq = id
	m.opInFlight = true
	m.replyReceived = false
	m.status = ""
	if m.dispatchCancel != nil {
		m.dispatchCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.dispatchCtx = ctx
	m.dispatchCancel = cancel
	m.currentVerb = verb
	m.currentTarget = target
	disp := m.disp
	if args == nil {
		args = map[string]any{"name": target}
	}
	return func() (resultMsg tea.Msg) {
		// §16.5 / §17.4: every goroutine in internal/tui (tea.Cmd bodies
		// run as goroutines per tea.go handleCommands) MUST top-level
		// defer recover(). On panic we surface a dispatchStartedMsg with
		// err set so Update clears op_in_flight without crashing the
		// program.
		defer func() {
			if r := recover(); r != nil {
				resultMsg = dispatchStartedMsg{
					dispatchID: id,
					verb:       verb,
					target:     target,
					err:        fmt.Errorf("dispatch panic: %v", r),
				}
			}
		}()
		// SYNCHRONOUS — Dispatch returns only after StartListener has
		// bound and the OSC has been written. The "defer cleanup()
		// immediately following" semantic is owned by the dispatcher's
		// drain goroutine (§13.2): when the channel closes the listener
		// cleanup runs.
		ch, err := disp.Dispatch(ctx, verb, args)
		return dispatchStartedMsg{
			dispatchID: id,
			verb:       verb,
			target:     target,
			ch:         ch,
			err:        err,
		}
	}
}

// scheduleRetransmit returns a tea.Tick Cmd that fires at +retransmitDelay.
// Crucially this is tea.Tick (NOT tea.After — which does not exist in
// any released bubbletea — and NOT time.AfterFunc, which would leak a
// goroutine past tea.Run return).
//
// The returned Tick callback is a tea.Cmd body running on a bubbletea
// goroutine; §16.5 / §17.4 require a top-level defer recover() so a
// panic in the message constructor does not tear down the program.
func (m *Model) scheduleRetransmit(id uint64) tea.Cmd {
	return tea.Tick(m.retransmitDelay, func(time.Time) (resultMsg tea.Msg) {
		defer func() {
			if r := recover(); r != nil {
				resultMsg = nil // dropped silently; tea ignores nil msgs.
			}
		}()
		return retransmitMsg{dispatchID: id}
	})
}

// scheduleTimeout returns a tea.Tick Cmd that fires the IPC_TIMEOUT.
//
// As with scheduleRetransmit, the Tick callback is wrapped in a
// defer recover() per §16.5 / §17.4.
func (m *Model) scheduleTimeout(id uint64) tea.Cmd {
	return tea.Tick(m.timeoutDelay, func(time.Time) (resultMsg tea.Msg) {
		defer func() {
			if r := recover(); r != nil {
				resultMsg = nil
			}
		}()
		return timeoutMsg{dispatchID: id}
	})
}

// waitForReply returns a Cmd that reads ONE reply from ch (or detects
// channel close). Re-issued from Update on each reply so the bubbletea
// dispatch stays single-threaded; this avoids needing program.Send.
//
// The Cmd body runs as a goroutine; §16.5 / §17.4 require a top-level
// defer recover(). A panic during the channel read surfaces as a
// "closed" replyMsg so the model's finishOp path runs.
func (m *Model) waitForReply(id uint64, ch <-chan ipc.Reply) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() (resultMsg tea.Msg) {
		defer func() {
			if r := recover(); r != nil {
				resultMsg = replyMsg{dispatchID: id, closed: true}
			}
		}()
		reply, ok := <-ch
		if !ok {
			return replyMsg{dispatchID: id, closed: true}
		}
		return replyMsg{dispatchID: id, reply: reply}
	}
}

// finishOp resets in-flight bookkeeping and stamps an optional status
// line. dispatchSeq is bumped so any pending tea.Tick deliveries
// (retransmit / timeout) fall through the dispatchID guard in Update.
func (m *Model) finishOp(status string) {
	m.opInFlight = false
	m.replyReceived = false
	m.dispatchSeq = atomic.AddUint64(&dispatchSeqCounter, 1)
	if status != "" {
		m.status = status
	}
	if m.dispatchCancel != nil {
		m.dispatchCancel()
		m.dispatchCancel = nil
	}
	m.replyCh = nil
	m.currentVerb = ""
	m.currentTarget = ""
}

// applyNewWorkspace inserts (or marks live) the workspace named `name`
// after a successful `new` reply, re-runs the configured sort, clears
// any active filter, and parks the cursor on the new row. The Lua
// `mux.spawn_window` call creates the workspace but does NOT activate
// it — the picker stays open so the user can decide to `s`witch into
// it or move on; positioning the cursor on the row is what makes that
// decision a single keystroke.
func (m *Model) applyNewWorkspace(name string) {
	if name == "" {
		return
	}
	matched := false
	for i := range m.rows {
		if m.rows[i].Name == name {
			m.rows[i].Source = SourceLive
			matched = true
			break
		}
	}
	if !matched {
		m.rows = append(m.rows, WorkspaceRow{Name: name, Source: SourceLive})
	}
	sortRows(m.rows, m.cfg.Sort, m.data.State)

	if m.mode == modeFilter {
		m.mode = modeNav
	}
	m.filterBuf = ""
	m.filtered = nil

	for i := range m.rows {
		if m.rows[i].Name == name {
			m.cursor = i
			break
		}
	}
}

// applyExternalDirs merges provider-supplied directory entries into
// m.rows as SourceExternal rows. Each entry is a map carrying `path`
// and (optionally) `name` strings. Names are sanitised + validated;
// invalid entries are dropped silently. External rows whose name
// already matches a live or saved row are dropped — the live/saved
// row wins because it carries richer state (mtime, snapshot pointer,
// pin, etc.). After the merge the configured sort runs again and the
// active filter is re-applied so the cursor lands somewhere reasonable.
func (m *Model) applyExternalDirs(entries []map[string]any) {
	if len(entries) == 0 {
		return
	}
	existing := make(map[string]struct{}, len(m.rows))
	for _, r := range m.rows {
		existing[r.Name] = struct{}{}
	}
	added := false
	for _, e := range entries {
		path, _ := e["path"].(string)
		name, _ := e["name"].(string)
		path = nameval.SanitizeForDisplay(path)
		name = nameval.SanitizeForDisplay(name)
		if name == "" {
			if path == "" {
				continue
			}
			name = filepath.Base(path)
		}
		if name == "" {
			continue
		}
		if err := nameval.ValidateWorkspaceName(name); err != nil {
			continue
		}
		if _, dup := existing[name]; dup {
			continue
		}
		existing[name] = struct{}{}
		m.rows = append(m.rows, WorkspaceRow{
			Name:   name,
			Source: SourceExternal,
			CWD:    path,
		})
		added = true
	}
	if !added {
		return
	}
	sortRows(m.rows, m.cfg.Sort, m.data.State)
	m.refreshFilter()
}

// refreshFilter applies the current filterBuf to m.rows and resets the
// cursor to the first match. Empty filter ⇒ filtered=nil ("show all").
func (m *Model) refreshFilter() {
	defer func() {
		visible := m.visibleRows()
		if m.cursor >= len(visible) {
			m.cursor = max(0, len(visible)-1)
		}
	}()
	if m.filterBuf == "" {
		m.filtered = nil
		return
	}
	needle := strings.ToLower(m.filterBuf)
	out := make([]int, 0, len(m.rows))
	for i, r := range m.rows {
		if strings.Contains(strings.ToLower(r.Name), needle) {
			out = append(out, i)
		}
	}
	m.filtered = out
	m.cursor = 0
}
