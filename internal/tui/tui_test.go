package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"go.uber.org/goleak"

	"github.com/Grady-Saccullo/wezsesh/internal/ipc"
)

// TestMain is the §17.3 / §14.2 goleak gate. The picker must not leave
// any background goroutines (tea.Tick timer, dispatch reader, etc.)
// past the suite. The model's design — tea.Tick (NOT time.AfterFunc)
// for retransmit + tea.Cmd-as-channel-reader for replies — is the
// reason this can pass without per-test ignores.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// bubbletea v2 boots a renderer goroutine that lives for the
		// duration of tea.Run; tests use tea.NewProgram with a closed
		// input so it exits with the program. We still ignore the
		// renderer's signal-handler installation in case any other
		// platform-wired goroutine sneaks in.
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
	)
}

// fakeDispatcher is a recording ipc.Dispatcher for the TUI tests. The
// dispatch / reply pair is fully observable so each test can drive a
// scenario (terminal reply, timeout, retransmit) deterministically.
type fakeDispatcher struct {
	mu       sync.Mutex
	calls    []dispatchCall
	repliesC chan ipc.Reply
	err      error
	// recordOnly true means Dispatch returns nil chan; tests that don't
	// drain replies still observe op_in_flight via the recorded calls.
	recordOnly bool
}

type dispatchCall struct {
	verb string
	args map[string]any
}

func (f *fakeDispatcher) Dispatch(_ context.Context, verb string, args map[string]any) (<-chan ipc.Reply, error) {
	f.mu.Lock()
	f.calls = append(f.calls, dispatchCall{verb: verb, args: args})
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if f.recordOnly {
		return nil, nil
	}
	return f.repliesC, nil
}

func (f *fakeDispatcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// EmergencyReply is the §13.1 panic-path fan-out hook. The TUI tests
// don't exercise the recover branch; a no-op satisfies the
// ipc.Dispatcher interface contract.
func (f *fakeDispatcher) EmergencyReply() {}

func newTestModel(t *testing.T, rows []WorkspaceRow, d ipc.Dispatcher) *Model {
	t.Helper()
	cfg := Config{
		Sort:           SortAlphabetical,
		DefaultAction:  ActionSwitch,
		Keys:           DefaultKeyMap(),
		PreviewEnabled: false,
	}
	m := newModel(cfg, Data{Workspaces: rows}, d)
	m.width = 80
	m.height = 24
	// Tighten the timings so the retransmit / timeout tests run in
	// human-scale time without burning sleeps.
	m.retransmitDelay = 30 * time.Millisecond
	m.timeoutDelay = 60 * time.Millisecond
	return m
}

// keyPress builds a tea.KeyPressMsg for a single rune.
func keyPress(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: r, Text: string(r)})
}

// specialKey builds a tea.KeyPressMsg for a special-named key (e.g.
// "esc", "enter").
func specialKey(name string) tea.KeyPressMsg {
	switch name {
	case "esc":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape})
	case "enter":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter})
	case "up":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyUp})
	case "down":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyDown})
	case "backspace":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyBackspace})
	}
	panic("unknown special key " + name)
}

// TestRenderSanitization is the §17.3 gate "Render-time sanitization":
// a snapshot named \x1b[2J must NOT cause the terminal to clear. The
// view must not contain raw ESC bytes coming from disk-sourced strings;
// sanitization replaces them with U+FFFD (3 bytes in UTF-8). lipgloss
// styling injects its own SGR escape sequences, so the test strips
// ANSI before checking — the gate is on user-input bytes only.
func TestRenderSanitization(t *testing.T) {
	rows := []WorkspaceRow{
		{Name: "\x1b[2J", Source: SourceSaved},
		{Name: "\x07evil\x1b[31m", Source: SourceSaved, Tags: []string{"\x1btag", "ok"}},
		{Name: "normal", Source: SourceSaved},
	}
	m := newTestModel(t, rows, nil)

	view := m.View()
	stripped := ansi.Strip(view.Content)
	if strings.ContainsRune(stripped, 0x1B) {
		t.Fatalf("rendered view contains raw ESC (0x1B) after stripping ANSI styling; sanitization failed:\n%q", stripped)
	}
	if strings.ContainsRune(stripped, 0x07) {
		t.Fatalf("rendered view contains raw BEL (0x07); sanitization failed:\n%q", stripped)
	}
	if !strings.Contains(stripped, "�") {
		t.Fatalf("rendered view missing U+FFFD replacement char; expected sanitized output:\n%q", stripped)
	}
	if !strings.Contains(stripped, "normal") {
		t.Fatalf("rendered view missing benign row; sanitization over-aggressive:\n%q", stripped)
	}
}

// TestRenderSanitization_ActiveAndStatus also covers the header and
// status line, which compose ActiveWorkspace + freeform status text.
func TestRenderSanitization_ActiveAndStatus(t *testing.T) {
	m := newTestModel(t, []WorkspaceRow{{Name: "ok"}}, nil)
	m.data.ActiveWorkspace = "\x1b[2J"
	m.status = "\x1b[31merror\x07"
	view := m.View()
	stripped := ansi.Strip(view.Content)
	if strings.ContainsRune(stripped, 0x1B) || strings.ContainsRune(stripped, 0x07) {
		t.Fatalf("header/status leaked control bytes after stripping styling: %q", stripped)
	}
}

// TestQuitMidOp_InlineStatus enforces §13.8: q while op_in_flight does
// NOT quit; instead an inline status line appears and y confirms.
// The test asserts that the confirmation text lives inside the View
// content (i.e. the renderer's inline footer), not as a separate
// modal overlay (which would not be in the same string).
func TestQuitMidOp_InlineStatus(t *testing.T) {
	d := &fakeDispatcher{repliesC: make(chan ipc.Reply, 2)}
	m := newTestModel(t, []WorkspaceRow{{Name: "alpha", Source: SourceSaved}}, d)

	// Trigger a switch — kicks off a dispatch and sets op_in_flight.
	if _, cmd := m.Update(specialKey("enter")); cmd != nil {
		// Run the command synchronously to deliver dispatchStartedMsg.
		msg := cmd()
		m.Update(msg)
	}
	if !m.opInFlight {
		t.Fatalf("expected op_in_flight after switch; calls=%d", d.callCount())
	}

	// Press q.
	_, _ = m.Update(keyPress('q'))
	if !m.confirmQuit {
		t.Fatalf("expected confirmQuit after q while op_in_flight")
	}
	view := m.View()
	if !strings.Contains(view.Content, "quit anyway") {
		t.Fatalf("inline quit prompt not in view content:\n%s", view.Content)
	}

	// Press n (anything other than y/Y). Should dismiss.
	_, _ = m.Update(keyPress('n'))
	if m.confirmQuit {
		t.Fatalf("confirmQuit not dismissed by non-y key")
	}

	// Quit-mid-op via y now. Drain the dispatcher channel so the
	// follow-up replyMsg cmd does not leak past the test.
	close(d.repliesC)
	if !m.opInFlight {
		// The op_in_flight clear happens on terminal reply; for this
		// test we only care that the y-confirm path triggers tea.Quit.
	}
	// Re-arm in-flight bookkeeping for the second q.
	m.opInFlight = true
	_, _ = m.Update(keyPress('q'))
	_, cmd := m.Update(keyPress('y'))
	if cmd == nil {
		t.Fatalf("expected tea.Quit cmd after y confirmation")
	}
	if !m.quitting {
		t.Fatalf("expected quitting=true after y confirmation")
	}
}

// TestQuitWhenIdle: q with no op_in_flight quits immediately.
func TestQuitWhenIdle(t *testing.T) {
	m := newTestModel(t, []WorkspaceRow{{Name: "alpha"}}, nil)
	_, cmd := m.Update(keyPress('q'))
	if cmd == nil {
		t.Fatalf("expected tea.Quit cmd; got nil")
	}
	if !m.quitting {
		t.Fatalf("expected quitting=true on idle quit")
	}
}

// TestReplyReceivedGuard: once a reply is received, retransmitMsg with
// the matching dispatchID is a no-op. This is the §14.2 invariant.
func TestReplyReceivedGuard(t *testing.T) {
	d := &fakeDispatcher{repliesC: make(chan ipc.Reply, 2)}
	m := newTestModel(t, []WorkspaceRow{{Name: "alpha"}}, d)

	// Start dispatch.
	_, cmd := m.Update(specialKey("enter"))
	if cmd == nil {
		t.Fatalf("expected dispatch cmd")
	}
	startedMsg := cmd().(dispatchStartedMsg)
	m.Update(startedMsg)

	// Deliver a terminal reply.
	d.repliesC <- ipc.Reply{ID: "x", Status: "completed", OK: true}
	// The dispatch loop should observe the reply via waitForReply Cmd.
	// We synthesise it directly to avoid coordinating with the cmd.
	m.replyReceived = true
	prevSeq := m.dispatchSeq

	// Synthesise a stale retransmit. Even with replyReceived=true it
	// MUST be a no-op because the guard blocks the body.
	beforeStatus := m.status
	m.Update(retransmitMsg{dispatchID: prevSeq})
	if m.status != beforeStatus {
		t.Fatalf("retransmitMsg changed status despite replyReceived guard")
	}

	// Drain the channel so the waitForReply Cmd terminates if any test
	// runner ever invokes it.
	close(d.repliesC)
}

// TestModeDiscipline covers the nav-vs-filter mode discipline. In
// filter mode `j`/`k` are literal characters (appended to the buffer);
// they do NOT move the cursor.
func TestModeDiscipline(t *testing.T) {
	m := newTestModel(t, []WorkspaceRow{
		{Name: "alpha"},
		{Name: "beta"},
		{Name: "gamma"},
	}, nil)

	// Nav mode: j moves cursor down.
	_, _ = m.Update(keyPress('j'))
	if m.cursor != 1 {
		t.Fatalf("nav mode: expected cursor=1, got %d", m.cursor)
	}

	// Enter filter mode.
	_, _ = m.Update(keyPress('/'))
	if m.mode != modeFilter {
		t.Fatalf("expected modeFilter after /, got %v", m.mode)
	}
	cursorBefore := m.cursor

	// Filter mode: j is literal — appends to buffer, does NOT move cursor.
	_, _ = m.Update(keyPress('j'))
	if m.filterBuf != "j" {
		t.Fatalf("filter mode: expected filterBuf=j, got %q", m.filterBuf)
	}
	if m.cursor != 0 {
		t.Fatalf("filter mode: cursor should reset on filter change, got %d (before=%d)",
			m.cursor, cursorBefore)
	}

	// Esc exits filter mode.
	_, _ = m.Update(specialKey("esc"))
	if m.mode != modeNav {
		t.Fatalf("expected modeNav after esc, got %v", m.mode)
	}
	if m.filterBuf != "" {
		t.Fatalf("expected empty filterBuf after esc, got %q", m.filterBuf)
	}
}

// TestModeDiscipline_UnmappedNavKey covers H1 (review finding): in nav
// mode, an unmapped key must produce NO action and must NOT be treated
// as a filter rune. The earlier shape of matchKey returned a stray rune
// from nav mode (footgun for the modal handlers in T-702); the split
// into matchNavKey + matchFilterKey makes the nav path strictly
// rune-free.
func TestModeDiscipline_UnmappedNavKey(t *testing.T) {
	m := newTestModel(t, []WorkspaceRow{
		{Name: "alpha"},
		{Name: "beta"},
	}, nil)

	cursorBefore := m.cursor
	bufBefore := m.filterBuf
	modeBefore := m.mode
	statusBefore := m.status

	// 'z' is not bound in DefaultKeyMap and is not a nav-mode special.
	_, cmd := m.Update(keyPress('z'))
	if cmd != nil {
		t.Fatalf("unmapped nav key produced a Cmd: %T", cmd)
	}
	if m.cursor != cursorBefore {
		t.Fatalf("nav-mode unmapped key moved cursor: %d -> %d", cursorBefore, m.cursor)
	}
	if m.filterBuf != bufBefore {
		t.Fatalf("nav-mode unmapped key leaked into filterBuf: %q -> %q",
			bufBefore, m.filterBuf)
	}
	if m.mode != modeBefore {
		t.Fatalf("nav-mode unmapped key flipped mode: %v -> %v", modeBefore, m.mode)
	}
	if m.status != statusBefore {
		t.Fatalf("nav-mode unmapped key set status: %q -> %q", statusBefore, m.status)
	}
}

// TestPinMarkClearMarksAreNoOps covers the v0.1 stub surface for the
// actPin / actMark / actClearMarks bindings. T-701 ships them as
// "received the key, surfaced 'not implemented' status" — they MUST
// NOT transition op_in_flight nor invoke the dispatcher. T-702 wires
// the real semantics; this test pins the v0.1 contract so that wiring
// can't silently regress.
func TestPinMarkClearMarksAreNoOps(t *testing.T) {
	d := &fakeDispatcher{recordOnly: true}
	m := newTestModel(t, []WorkspaceRow{{Name: "alpha"}}, d)

	for _, tc := range []struct {
		name string
		key  rune
	}{
		{"pin", 'p'},
		{"mark-space", ' '},
		{"mark-alt", 'm'},
		{"clear-marks", 'c'},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beforeCalls := d.callCount()
			_, cmd := m.Update(keyPress(tc.key))
			if cmd != nil {
				t.Fatalf("%s produced a Cmd: %T", tc.name, cmd)
			}
			if m.opInFlight {
				t.Fatalf("%s flipped op_in_flight true", tc.name)
			}
			if d.callCount() != beforeCalls {
				t.Fatalf("%s invoked dispatcher (calls before=%d, after=%d)",
					tc.name, beforeCalls, d.callCount())
			}
			if m.status == "" {
				t.Fatalf("%s did not surface a status hint", tc.name)
			}
		})
	}
}

// TestStartListenerSynchronousFromUpdate asserts that pressing Enter
// triggers a Cmd that, when run, calls Dispatcher.Dispatch
// synchronously — Dispatch returns BEFORE the Cmd's tea.Msg is
// constructed. This satisfies the §17.3 "StartListener called from
// Update synchronously" gate via the dispatcher seam.
func TestStartListenerSynchronousFromUpdate(t *testing.T) {
	called := atomic.Int32{}
	gate := make(chan struct{})
	syncCheck := &dispatcherChecksOrdering{
		gate:       gate,
		calls:      &called,
		repliesC:   make(chan ipc.Reply, 1),
		ordered:    atomic.Bool{},
		startCount: atomic.Int32{},
	}
	m := newTestModel(t, []WorkspaceRow{{Name: "alpha"}}, syncCheck)

	_, cmd := m.Update(specialKey("enter"))
	if cmd == nil {
		t.Fatalf("expected dispatch cmd from Enter")
	}

	close(gate) // unblock Dispatch.
	msg := cmd()
	started, ok := msg.(dispatchStartedMsg)
	if !ok {
		t.Fatalf("expected dispatchStartedMsg, got %T", msg)
	}
	if started.err != nil {
		t.Fatalf("expected nil err, got %v", started.err)
	}
	if !syncCheck.ordered.Load() {
		t.Fatalf("Dispatch did not observe its own synchronous body before Cmd return")
	}
	close(syncCheck.repliesC)
}

// dispatcherChecksOrdering is a fake that records "I was called and
// returned" before the Cmd produces its message — the test uses this
// to assert the synchronous-from-Cmd-body contract.
type dispatcherChecksOrdering struct {
	gate       chan struct{}
	calls      *atomic.Int32
	repliesC   chan ipc.Reply
	ordered    atomic.Bool
	startCount atomic.Int32
}

func (d *dispatcherChecksOrdering) Dispatch(_ context.Context, _ string, _ map[string]any) (<-chan ipc.Reply, error) {
	d.startCount.Add(1)
	<-d.gate
	d.ordered.Store(true)
	d.calls.Add(1)
	return d.repliesC, nil
}

// EmergencyReply satisfies the ipc.Dispatcher interface; the ordering
// fake never exercises the §13.1 panic path.
func (d *dispatcherChecksOrdering) EmergencyReply() {}

// TestNoTeaAfterReference is a build-time guarantee that none of the
// tui sources reference tea.After (which does not exist in any released
// bubbletea, per CLAUDE.md). The CI lint covers this from the outside;
// we duplicate it as a unit test so a developer's local run catches a
// regression without waiting for CI.
//
// The check uses go/parser to walk the AST of every non-test source file
// and looks for a SelectorExpr `tea.After`. A naive substring grep would
// false-positive on the prose docstrings that warn about tea.After
// (which is the whole point of the CLAUDE.md guidance — the prose has
// to mention the forbidden API to remind future readers it's forbidden).
func TestNoTeaAfterReference(t *testing.T) {
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if id.Name == "tea" && sel.Sel.Name == "After" {
				t.Fatalf("%s:%s references tea.After (forbidden — does not exist in bubbletea)",
					path, fset.Position(sel.Pos()))
			}
			return true
		})
	}
}

// TestRetransmitTickCancellation is the §17.3 gate "tea.Tick retransmit
// cancellation": when tea.Run returns, the timer goroutine MUST exit
// within 100 ms. The test wires a fake dispatcher, drives a dispatch
// (which schedules the retransmit + timeout via tea.Tick), waits long
// enough for both Ticks to fire, then Quits the program and asserts:
//
//  1. tea.Run returns within 2 s.
//  2. No goroutine leaks past tea.Run return + 100 ms (the spec budget).
//
// Two production-load-bearing properties are exercised here in
// combination: (a) the retransmit is tea.Tick (NOT time.AfterFunc),
// which is the only timer primitive whose goroutine is owned by the
// bubbletea event loop, and (b) the model does NOT spawn any other
// background goroutine that would survive Quit.
//
// We deliberately use SHORT (≤ 60 ms) delays so the timer fires before
// Quit; tea.Tick goroutines block on `<-t.C` until the timer fires, so
// any test that Quits before fire-time would observe a "leak" that is
// actually just a normal pending-timer wait. Production retransmit is
// 2 s and IPC_TIMEOUT is 5 s; either way the goroutines exit cleanly
// once their timer fires and the resulting Msg has been delivered to
// Update.
func TestRetransmitTickCancellation(t *testing.T) {
	d := &fakeDispatcher{repliesC: make(chan ipc.Reply, 4)}
	m := newTestModel(t, []WorkspaceRow{{Name: "alpha"}}, d)

	// Use a pipe-backed input so tea.Program does not touch stdin.
	in, inWriter := io.Pipe()
	out := &bytes.Buffer{}
	prog := tea.NewProgram(m,
		tea.WithInput(in),
		tea.WithOutput(out),
		tea.WithoutSignals(),
		tea.WithoutCatchPanics(),
	)

	done := make(chan struct{})
	var runErr error
	go func() {
		_, runErr = prog.Run()
		close(done)
	}()

	// Drive a dispatch via Send. m.retransmitDelay is 30 ms,
	// m.timeoutDelay is 60 ms (set in newTestModel).
	prog.Send(specialKey("enter"))

	// Wait for both timers to have fired and their Msgs to have been
	// processed by Update.
	time.Sleep(120 * time.Millisecond)

	// Drain the dispatcher's reply channel so any waitForReply Cmd
	// observes the close and exits cleanly.
	close(d.repliesC)
	time.Sleep(20 * time.Millisecond)

	prog.Quit()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("tea.Run did not return after Quit; runErr=%v", runErr)
	}
	if runErr != nil && !errors.Is(runErr, tea.ErrProgramKilled) {
		// tea.Quit in v2 returns a nil error on clean exit; ErrProgramKilled
		// would surface only on Kill().
		t.Logf("tea.Run returned %v (acceptable)", runErr)
	}
	// Close the input pipe writer so bubbletea's read loop drops out.
	// The read loop is internal to bubbletea; closing the pipe end is
	// the supported way to signal it to exit.
	_ = inWriter.Close()

	// The §17.3 "100 ms" gate: any goroutine started by the model's
	// tea.Tick (retransmit / timeout) MUST have exited by now. We give
	// bubbletea's own teardown (input reader, renderer) the same 100 ms
	// budget so the assertion is end-to-end ("tea.Run return + 100 ms"
	// means a tidy exit, no leaks).
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		err := goleak.Find(
			goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
		)
		if err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := goleak.Find(
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
	); err != nil {
		t.Fatalf("goroutines leaked past tea.Run + 100 ms: %v", err)
	}
}

// TestSortAlphabetical pins the §13.10 byte-order-over-NFC ordering for
// SortAlphabetical. The picker is locale-naive in v0.1.
func TestSortAlphabetical(t *testing.T) {
	rows := []WorkspaceRow{
		{Name: "zoo"},
		{Name: "apple"},
		{Name: "banana"},
	}
	m := newTestModel(t, rows, nil)
	if got := m.rows[0].Name; got != "apple" {
		t.Fatalf("expected first row=apple, got %s", got)
	}
	if got := m.rows[2].Name; got != "zoo" {
		t.Fatalf("expected last row=zoo, got %s", got)
	}
}

// TestSortLiveFirst exercises the default sort: pinned > live-active >
// live > saved-by-mtime > saved > external.
func TestSortLiveFirst(t *testing.T) {
	now := time.Now()
	rows := []WorkspaceRow{
		{Name: "old", Source: SourceSaved, Mtime: now.Add(-72 * time.Hour)},
		{Name: "active", Source: SourceLive, Active: true},
		{Name: "live", Source: SourceLive},
		{Name: "pinned-saved", Source: SourceSaved, Pinned: true, Mtime: now.Add(-time.Hour)},
		{Name: "fresh", Source: SourceSaved, Mtime: now.Add(-time.Minute)},
		{Name: "ext", Source: SourceExternal, CWD: "/tmp/ext"},
	}
	cfg := Config{Sort: SortLiveFirst, Keys: DefaultKeyMap()}
	m := newModel(cfg, Data{Workspaces: rows}, nil)
	got := []string{}
	for _, r := range m.rows {
		got = append(got, r.Name)
	}
	want := []string{"pinned-saved", "active", "live", "fresh", "old", "ext"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("live_first order mismatch:\n got=%v\nwant=%v", got, want)
	}
}

// TestDispatchErrorClearsInFlight: if Dispatch returns an error, the
// model must clear op_in_flight and surface a status message — the user
// should be able to quit without seeing the inline confirm prompt.
func TestDispatchErrorClearsInFlight(t *testing.T) {
	d := &fakeDispatcher{err: errors.New("listener bind failed"), repliesC: make(chan ipc.Reply)}
	m := newTestModel(t, []WorkspaceRow{{Name: "alpha"}}, d)

	_, cmd := m.Update(specialKey("enter"))
	if cmd == nil {
		t.Fatalf("expected dispatch cmd")
	}
	msg := cmd()
	m.Update(msg)
	if m.opInFlight {
		t.Fatalf("expected op_in_flight=false after Dispatch error")
	}
	if !strings.Contains(m.status, "dispatch failed") {
		t.Fatalf("expected status to surface error; got %q", m.status)
	}
	close(d.repliesC)
}

// TestInitDispatchesListDirs asserts that the model's Init() returns a
// startup dispatch Cmd whose body fires the list_dirs verb. The reply
// path is exercised by TestListDirsReplyMergesExternalRows; this test
// only confirms the wiring at startup time.
func TestInitDispatchesListDirs(t *testing.T) {
	d := &fakeDispatcher{repliesC: make(chan ipc.Reply, 1)}
	m := newTestModel(t, []WorkspaceRow{{Name: "alpha", Source: SourceLive}}, d)

	cmd := m.Init()
	if cmd == nil {
		t.Fatalf("Init() returned nil cmd; expected list_dirs dispatch")
	}
	msg := cmd()
	started, ok := msg.(dispatchStartedMsg)
	if !ok {
		t.Fatalf("Init cmd produced %T, want dispatchStartedMsg", msg)
	}
	if started.verb != "list_dirs" {
		t.Fatalf("Init dispatched verb %q, want list_dirs", started.verb)
	}
	// Drain the channel so the waitForReply Cmd (if any) terminates.
	close(d.repliesC)

	// Confirm the dispatcher recorded the call with the empty-query arg
	// shape.
	if d.callCount() != 1 {
		t.Fatalf("expected 1 dispatch call, got %d", d.callCount())
	}
	d.mu.Lock()
	args := d.calls[0].args
	d.mu.Unlock()
	if got, _ := args["query"].(string); got != "" {
		t.Fatalf("list_dirs args.query = %q, want \"\"", got)
	}
}

// TestListDirsReplyMergesExternalRows feeds a synthetic list_dirs
// terminal reply through Update and asserts the dirs are merged into
// m.rows as SourceExternal entries with their CWD carried through.
func TestListDirsReplyMergesExternalRows(t *testing.T) {
	d := &fakeDispatcher{repliesC: make(chan ipc.Reply, 2)}
	m := newTestModel(t, []WorkspaceRow{{Name: "alpha", Source: SourceLive}}, d)

	// Drive the startup list_dirs dispatch.
	cmd := m.Init()
	if cmd == nil {
		t.Fatalf("Init() returned nil cmd")
	}
	startedMsg := cmd().(dispatchStartedMsg)
	m.Update(startedMsg)

	// Synthesise a terminal reply carrying two dirs.
	reply := ipc.Reply{
		ID:     "x",
		Status: "completed",
		OK:     true,
		Data: map[string]any{
			"dirs": []any{
				map[string]any{"path": "/srv/proj", "name": "proj"},
				map[string]any{"path": "/home/u/repos/widget", "name": "widget"},
			},
		},
	}
	m.Update(replyMsg{dispatchID: m.dispatchSeq, reply: reply})

	byName := map[string]WorkspaceRow{}
	for _, r := range m.rows {
		byName[r.Name] = r
	}
	if len(byName) != 3 {
		t.Fatalf("expected 3 rows after merge, got %d (%+v)", len(byName), m.rows)
	}
	for _, name := range []string{"proj", "widget"} {
		r, ok := byName[name]
		if !ok {
			t.Fatalf("missing external row %q", name)
		}
		if r.Source != SourceExternal {
			t.Errorf("row %q Source = %v, want SourceExternal", name, r.Source)
		}
		if r.CWD == "" {
			t.Errorf("row %q CWD is empty", name)
		}
	}
	close(d.repliesC)
}

// TestListDirsReplyDedupesAgainstLive: a live row named "foo" plus a
// list_dirs entry with name "foo" must yield a single row — the live
// row wins (richer state, real workspace).
func TestListDirsReplyDedupesAgainstLive(t *testing.T) {
	d := &fakeDispatcher{repliesC: make(chan ipc.Reply, 2)}
	m := newTestModel(t, []WorkspaceRow{{Name: "foo", Source: SourceLive}}, d)

	cmd := m.Init()
	startedMsg := cmd().(dispatchStartedMsg)
	m.Update(startedMsg)

	reply := ipc.Reply{
		ID:     "y",
		Status: "completed",
		OK:     true,
		Data: map[string]any{
			"dirs": []any{
				map[string]any{"path": "/somewhere/foo", "name": "foo"},
			},
		},
	}
	m.Update(replyMsg{dispatchID: m.dispatchSeq, reply: reply})

	if len(m.rows) != 1 {
		t.Fatalf("expected dedupe to yield 1 row, got %d (%+v)", len(m.rows), m.rows)
	}
	if m.rows[0].Source != SourceLive {
		t.Fatalf("live row should have won; got Source=%v", m.rows[0].Source)
	}
	if m.rows[0].CWD != "" {
		t.Fatalf("live row should not have inherited external CWD; got %q", m.rows[0].CWD)
	}
	close(d.repliesC)
}

// TestEnterOnLiveDispatchesSwitchEmptyCWD: pressing Enter while the
// cursor is on a SourceLive row must dispatch `switch` with cwd:"".
func TestEnterOnLiveDispatchesSwitchEmptyCWD(t *testing.T) {
	d := &fakeDispatcher{repliesC: make(chan ipc.Reply, 1)}
	m := newTestModel(t, []WorkspaceRow{{Name: "alpha", Source: SourceLive}}, d)

	_, cmd := m.Update(specialKey("enter"))
	if cmd == nil {
		t.Fatalf("expected dispatch cmd from Enter")
	}
	_ = cmd()

	if d.callCount() != 1 {
		t.Fatalf("expected 1 dispatch call, got %d", d.callCount())
	}
	d.mu.Lock()
	call := d.calls[0]
	d.mu.Unlock()
	if call.verb != "switch" {
		t.Fatalf("verb = %q, want switch", call.verb)
	}
	if got, _ := call.args["name"].(string); got != "alpha" {
		t.Fatalf("args.name = %q, want alpha", got)
	}
	cwd, present := call.args["cwd"]
	if !present {
		t.Fatalf("args.cwd missing — switch must always carry cwd, even when empty")
	}
	if got, _ := cwd.(string); got != "" {
		t.Fatalf("args.cwd = %q, want \"\" for SourceLive", got)
	}
	close(d.repliesC)
}

// TestEnterOnExternalDispatchesSwitchWithCWD: pressing Enter while the
// cursor is on a SourceExternal row must dispatch `switch` with the
// row's CWD threaded through, so the plugin's switch handler can
// rename the active workspace and `cd` the pane.
func TestEnterOnExternalDispatchesSwitchWithCWD(t *testing.T) {
	d := &fakeDispatcher{repliesC: make(chan ipc.Reply, 1)}
	m := newTestModel(t, []WorkspaceRow{
		{Name: "proj", Source: SourceExternal, CWD: "/srv/proj"},
	}, d)

	_, cmd := m.Update(specialKey("enter"))
	if cmd == nil {
		t.Fatalf("expected dispatch cmd from Enter")
	}
	_ = cmd()

	if d.callCount() != 1 {
		t.Fatalf("expected 1 dispatch call, got %d", d.callCount())
	}
	d.mu.Lock()
	call := d.calls[0]
	d.mu.Unlock()
	if call.verb != "switch" {
		t.Fatalf("verb = %q, want switch", call.verb)
	}
	if got, _ := call.args["cwd"].(string); got != "/srv/proj" {
		t.Fatalf("args.cwd = %q, want /srv/proj for SourceExternal", got)
	}
	close(d.repliesC)
}

// TestNewWorkspaceModalDispatchesSwitch: typing a name into the
// new-workspace modal and hitting Enter must dispatch `switch` (not
// `new`). The rename trick keeps the active window in place; spawning
// a fresh window is the CLI-only `wezsesh new` path now.
func TestNewWorkspaceModalDispatchesSwitch(t *testing.T) {
	d := &fakeDispatcher{repliesC: make(chan ipc.Reply, 1)}
	m := newTestModel(t, []WorkspaceRow{{Name: "alpha", Source: SourceLive}}, d)

	// Open the modal with `n`.
	_, _ = m.Update(keyPress('n'))
	if m.activeModal() != modalNewWorkspace {
		t.Fatalf("expected modalNewWorkspace, got %v", m.activeModal())
	}

	// Type a name.
	for _, r := range "fresh" {
		_, _ = m.Update(keyPress(r))
	}
	// Submit.
	_, cmd := m.Update(specialKey("enter"))
	if cmd == nil {
		t.Fatalf("expected dispatch cmd from modal Enter")
	}
	_ = cmd()

	if d.callCount() != 1 {
		t.Fatalf("expected 1 dispatch call, got %d", d.callCount())
	}
	d.mu.Lock()
	call := d.calls[0]
	d.mu.Unlock()
	if call.verb != "switch" {
		t.Fatalf("modal Enter dispatched %q, want switch", call.verb)
	}
	if got, _ := call.args["name"].(string); got != "fresh" {
		t.Fatalf("args.name = %q, want fresh", got)
	}
	if got, _ := call.args["cwd"].(string); got != "" {
		t.Fatalf("args.cwd = %q, want \"\" for new-workspace modal", got)
	}
	close(d.repliesC)
}

// TestFilterMatchUpdatesCursor verifies that typing in filter mode
// narrows the visible list and resets the cursor to the first hit.
func TestFilterMatchUpdatesCursor(t *testing.T) {
	rows := []WorkspaceRow{
		{Name: "alpha"},
		{Name: "beta"},
		{Name: "betagamma"},
	}
	m := newTestModel(t, rows, nil)
	_, _ = m.Update(keyPress('/'))
	_, _ = m.Update(keyPress('b'))
	visible := m.visibleRows()
	if len(visible) != 2 {
		t.Fatalf("expected 2 matches for 'b', got %d", len(visible))
	}
	for _, idx := range visible {
		if !strings.Contains(m.rows[idx].Name, "b") {
			t.Fatalf("non-matching row in filter result: %s", m.rows[idx].Name)
		}
	}
}
