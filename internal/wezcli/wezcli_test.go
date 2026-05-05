package wezcli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// fakeMux is the shared test seam for switch-poller / list / list-clients
// fixtures. It serves as the wezcli runner; each invocation increments
// callCount, advances a virtual clock by the per-call latency, and
// returns canned bytes from the per-subcommand fixture map.
//
// virtual-clock model: nowFn returns the cumulative elapsed time the mux
// has "spent" on prior calls + sleeps, so test latency assertions are
// deterministic without ever sleeping wall-clock time. sleepFn advances
// the virtual clock without blocking.
type fakeMux struct {
	mu sync.Mutex

	now time.Time
	// listLatency / listClientsLatency control the virtual time consumed
	// per `cli list` / `cli list-clients` call. The slow-mux switch-poller
	// gate sets these to 1.5 s so the cadence dilation path is exercised.
	listLatency        time.Duration
	listClientsLatency time.Duration

	// listLatencyByCall / listClientsLatencyByCall override the per-subcommand
	// flat latency on a per-call basis (call index → latency). When non-nil,
	// they take precedence over the flat field; out-of-range indices fall
	// back to the flat field. Used by the cadence-preservation regression
	// test to drive a slow tick followed by an instant tick.
	listLatencyByCall        []time.Duration
	listClientsLatencyByCall []time.Duration

	// clientsByCall / panesByCall let a test rotate the response sequence.
	// If only one entry is present it is returned for every call; otherwise
	// the call index modulo len() picks the response.
	clientsByCall [][]ClientInfo
	panesByCall   [][]Pane

	// listClientsErrByCall optionally returns a non-nil error from the Nth
	// list-clients call. Empty slots fall back to the canned-bytes path.
	// Used by the cadence-preservation regression test.
	listClientsErrByCall []error
	// listClientsAlwaysErrAfter, when non-nil and the call index is at
	// least *listClientsAlwaysErrAfter, returns listClientsAlwaysErr from
	// every subsequent list-clients invocation. Used by the cadence-
	// preservation regression test to drive a "first tick succeeds,
	// subsequent ticks error forever" sequence without enumerating every
	// tick.
	listClientsAlwaysErrAfter *int
	listClientsAlwaysErr      error

	listCalls        atomic.Int32
	listClientsCalls atomic.Int32
	otherCalls       atomic.Int32

	// cadences observed per tick — appended after each StartSwitchPoller
	// sleep. The adaptive-cadence test asserts on the trailing entry.
	sleepDurations []time.Duration
}

func (f *fakeMux) nowFn() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeMux) sleepFn(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	f.sleepDurations = append(f.sleepDurations, d)
}

func (f *fakeMux) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *fakeMux) run(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) < 2 || args[0] != "cli" {
		f.otherCalls.Add(1)
		return nil, errors.New("fakeMux: unexpected invocation")
	}
	switch args[1] {
	case "list":
		idx := int(f.listCalls.Add(1)) - 1
		latency := f.listLatency
		if idx < len(f.listLatencyByCall) {
			latency = f.listLatencyByCall[idx]
		}
		f.advance(latency)
		var panes []Pane
		switch len(f.panesByCall) {
		case 0:
			panes = nil
		case 1:
			panes = f.panesByCall[0]
		default:
			if idx >= len(f.panesByCall) {
				idx = len(f.panesByCall) - 1
			}
			panes = f.panesByCall[idx]
		}
		b, err := json.Marshal(panes)
		return b, err
	case "list-clients":
		idx := int(f.listClientsCalls.Add(1)) - 1
		latency := f.listClientsLatency
		if idx < len(f.listClientsLatencyByCall) {
			latency = f.listClientsLatencyByCall[idx]
		}
		f.advance(latency)
		if idx < len(f.listClientsErrByCall) {
			if e := f.listClientsErrByCall[idx]; e != nil {
				return nil, e
			}
		}
		if f.listClientsAlwaysErrAfter != nil && idx >= *f.listClientsAlwaysErrAfter {
			return nil, f.listClientsAlwaysErr
		}
		var clients []ClientInfo
		switch len(f.clientsByCall) {
		case 0:
			clients = nil
		case 1:
			clients = f.clientsByCall[0]
		default:
			if idx >= len(f.clientsByCall) {
				idx = len(f.clientsByCall) - 1
			}
			clients = f.clientsByCall[idx]
		}
		// We re-encode through the wire shape so the (un)marshallers are
		// exercised end-to-end. The wire shape uses snake_case + raw
		// last_input/idle_time RawMessages.
		raw := make([]map[string]any, len(clients))
		for i, c := range clients {
			raw[i] = map[string]any{
				"client_id":       c.ClientID,
				"username":        c.Username,
				"hostname":        c.Hostname,
				"pid":             c.PID,
				"focused_pane_id": c.FocusedPaneID,
				"last_input":      c.LastInput.UTC().Format(time.RFC3339Nano),
				"idle_time":       c.IdleTime.String(),
			}
		}
		b, err := json.Marshal(raw)
		return b, err
	default:
		f.otherCalls.Add(1)
		// cli activate-pane / activate-tab / rename-workspace / spawn —
		// canned ok with empty stdout.
		return nil, nil
	}
}

func (f *fakeMux) lastSleep() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sleepDurations) == 0 {
		return 0
	}
	return f.sleepDurations[len(f.sleepDurations)-1]
}

// -----------------------------------------------------------------------------
// Acceptance gate: cwd parsing guards for "" (NOT null) before URL-decoding.
// -----------------------------------------------------------------------------

func TestPaneCWDPath_EmptyStringGuard(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		cwd     string
		wantOK  bool
		wantStr string
	}{
		{"empty cwd is unknown", "", false, ""},
		{"unparseable cwd is unknown", ":::", false, ""},
		{"non-file URL is unknown", "https://example/", false, ""},
		{"file URL decodes", "file:///home/u/code", true, "/home/u/code"},
		{"file URL with percent-encoding decodes", "file:///home/u/c%20de", true, "/home/u/c de"},
		{"empty file path is unknown", "file://", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Pane{CWD: tc.cwd}
			got, ok := p.CWDPath()
			if ok != tc.wantOK || got != tc.wantStr {
				t.Fatalf("Pane{CWD:%q}.CWDPath() = (%q, %v); want (%q, %v)",
					tc.cwd, got, ok, tc.wantStr, tc.wantOK)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Acceptance gate: switch-poller false-positive — switch to active short-
// circuits in 1 tick (in fact in 0 wezcli calls); switch+restore to active
// bypasses via isRestoreFlow.
// -----------------------------------------------------------------------------

func TestStartSwitchPoller_SameWorkspaceShortCircuits(t *testing.T) {
	t.Parallel()
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	f := &fakeMux{
		clientsByCall: [][]ClientInfo{{{ClientID: "c1", FocusedPaneID: 1}}},
		panesByCall:   [][]Pane{{{PaneID: 1, WindowID: 7, Workspace: "main"}}},
	}
	c := newClientForTesting(nil, f.run, f.nowFn, f.sleepFn)
	pre := SwitchPreState{TargetClientID: "c1", TargetWindowID: 7, ActiveWorkspace: "main"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.StartSwitchPoller(ctx, pre, "main", false); err != nil {
		t.Fatalf("same-workspace switch should short-circuit; got err=%v", err)
	}
	if got := f.listCalls.Load(); got != 0 {
		t.Fatalf("same-workspace short-circuit issued %d list calls; want 0", got)
	}
	if got := f.listClientsCalls.Load(); got != 0 {
		t.Fatalf("same-workspace short-circuit issued %d list-clients calls; want 0", got)
	}
	if got := f.lastSleep(); got != 0 {
		t.Fatalf("same-workspace short-circuit slept %v; want 0", got)
	}
}

func TestStartSwitchPoller_RestoreFlowBypassesShortCircuit(t *testing.T) {
	t.Parallel()
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	f := &fakeMux{
		// One-shot success: pane is already on target workspace + window.
		clientsByCall: [][]ClientInfo{{{ClientID: "c1", FocusedPaneID: 1}}},
		panesByCall:   [][]Pane{{{PaneID: 1, WindowID: 7, Workspace: "main"}}},
	}
	c := newClientForTesting(nil, f.run, f.nowFn, f.sleepFn)
	pre := SwitchPreState{TargetClientID: "c1", TargetWindowID: 7, ActiveWorkspace: "main"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.StartSwitchPoller(ctx, pre, "main", true); err != nil {
		t.Fatalf("switch+restore should still validate via poll; got err=%v", err)
	}
	if got := f.listCalls.Load(); got != 1 {
		t.Fatalf("switch+restore expected exactly one list call; got %d", got)
	}
	if got := f.listClientsCalls.Load(); got != 1 {
		t.Fatalf("switch+restore expected exactly one list-clients call; got %d", got)
	}
}

func TestStartSwitchPoller_PredicateRequiresWindowMatch(t *testing.T) {
	t.Parallel()
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	// The pinned client's focused pane is on the target workspace but in
	// a DIFFERENT window. Predicate must fail; ctx-deadline must surface.
	f := &fakeMux{
		clientsByCall: [][]ClientInfo{{{ClientID: "c1", FocusedPaneID: 1}}},
		panesByCall:   [][]Pane{{{PaneID: 1, WindowID: 99, Workspace: "target"}}},
	}
	c := newClientForTesting(nil, f.run, f.nowFn, f.sleepFn)
	pre := SwitchPreState{TargetClientID: "c1", TargetWindowID: 7, ActiveWorkspace: "main"}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := c.StartSwitchPoller(ctx, pre, "target", false)
	if err == nil || !errors.Is(err, ErrMuxUnreachable) {
		t.Fatalf("expected ErrMuxUnreachable on window mismatch + ctx expiry; got %v", err)
	}
}

// -----------------------------------------------------------------------------
// Acceptance gate: switch poller adaptive cadence — slow ListClients
// (1.5 s tick) → cadence dilates to 250 ms.
// -----------------------------------------------------------------------------

func TestStartSwitchPoller_AdaptiveCadenceDilatesOnSlowTick(t *testing.T) {
	t.Parallel()
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	f := &fakeMux{
		listClientsLatency: 1500 * time.Millisecond, // slow mux
		listLatency:        0,
		clientsByCall:      [][]ClientInfo{{{ClientID: "c1", FocusedPaneID: 1}}},
		// Pane is on the WRONG workspace so the poller keeps ticking.
		panesByCall: [][]Pane{{{PaneID: 1, WindowID: 7, Workspace: "other"}}},
	}
	c := newClientForTesting(nil, f.run, f.nowFn, f.sleepFn)
	pre := SwitchPreState{TargetClientID: "c1", TargetWindowID: 7, ActiveWorkspace: "main"}

	// Bound the loop with a virtual context that expires after a few
	// virtual ticks. Use a real deadline that's generous enough for the
	// virtual clock to advance through several ticks before the real
	// ctx-cancel fires.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_ = c.StartSwitchPoller(ctx, pre, "target", false)
	// Confirm the cadence dilated to 250 ms after at least one slow tick.
	if got := f.lastSleep(); got != 250*time.Millisecond {
		t.Fatalf("adaptive cadence: expected 250 ms after slow tick; got %v (history=%v)",
			got, f.sleepDurations)
	}
}

func TestStartSwitchPoller_AdaptiveCadenceFastTickStaysAt50ms(t *testing.T) {
	t.Parallel()
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	f := &fakeMux{
		// Both calls are instant — the elapsed-tick is well under 100 ms.
		clientsByCall: [][]ClientInfo{{{ClientID: "c1", FocusedPaneID: 1}}},
		panesByCall:   [][]Pane{{{PaneID: 1, WindowID: 7, Workspace: "other"}}},
	}
	c := newClientForTesting(nil, f.run, f.nowFn, f.sleepFn)
	pre := SwitchPreState{TargetClientID: "c1", TargetWindowID: 7, ActiveWorkspace: "main"}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_ = c.StartSwitchPoller(ctx, pre, "target", false)
	if got := f.lastSleep(); got != 50*time.Millisecond {
		t.Fatalf("adaptive cadence: expected 50 ms after fast tick; got %v", got)
	}
}

// -----------------------------------------------------------------------------
// Acceptance gate: pinned TargetClientID is captured at Phase 1 start, NOT
// re-evaluated per tick (conformance §5).
// -----------------------------------------------------------------------------

func TestStartSwitchPoller_DoesNotRePinClientIDPerTick(t *testing.T) {
	t.Parallel()
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	// Tick 1: pinned client "c1" is present, focused pane 1 on wrong ws.
	// Tick 2: a NEW client "c2" appears as the most-recent client, but
	//         the pinned "c1" is still present and focused on the right
	//         pane. Poller must succeed via "c1" and ignore "c2".
	withC1Only := []ClientInfo{
		{ClientID: "c1", FocusedPaneID: 1, LastInput: time.Unix(100, 0)},
	}
	withC2MoreRecent := []ClientInfo{
		{ClientID: "c1", FocusedPaneID: 2, LastInput: time.Unix(100, 0)},
		{ClientID: "c2", FocusedPaneID: 99, LastInput: time.Unix(200, 0)},
	}
	panesTick1 := []Pane{{PaneID: 1, WindowID: 7, Workspace: "other"}}
	panesTick2 := []Pane{
		{PaneID: 2, WindowID: 7, Workspace: "target"},
		{PaneID: 99, WindowID: 7, Workspace: "wrong-ws-for-c2"},
	}

	f := &fakeMux{
		clientsByCall: [][]ClientInfo{withC1Only, withC2MoreRecent},
		panesByCall:   [][]Pane{panesTick1, panesTick2},
	}
	c := newClientForTesting(nil, f.run, f.nowFn, f.sleepFn)
	pre := SwitchPreState{TargetClientID: "c1", TargetWindowID: 7, ActiveWorkspace: "main"}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if err := c.StartSwitchPoller(ctx, pre, "target", false); err != nil {
		t.Fatalf("pinned-client poll should succeed at tick 2; got %v", err)
	}
	// We required two ticks → two list-clients calls.
	if got := f.listClientsCalls.Load(); got != 2 {
		t.Fatalf("expected 2 list-clients calls; got %d", got)
	}
}

func TestStartSwitchPoller_PinnedClientDisappearsFailsClosed(t *testing.T) {
	t.Parallel()
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	// Pinned client "c1" disappears mid-poll; only "c2" present. Per the
	// spec "fails closed", the predicate should never satisfy and the
	// 5 s ceiling surfaces ErrMuxUnreachable.
	f := &fakeMux{
		clientsByCall: [][]ClientInfo{
			{{ClientID: "c2", FocusedPaneID: 99, LastInput: time.Unix(100, 0)}},
		},
		panesByCall: [][]Pane{
			{{PaneID: 99, WindowID: 7, Workspace: "target"}},
		},
	}
	c := newClientForTesting(nil, f.run, f.nowFn, f.sleepFn)
	pre := SwitchPreState{TargetClientID: "c1", TargetWindowID: 7, ActiveWorkspace: "main"}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := c.StartSwitchPoller(ctx, pre, "target", false)
	if err == nil || !errors.Is(err, ErrMuxUnreachable) {
		t.Fatalf("pinned-client disappearance should fail closed; got %v", err)
	}
}

// -----------------------------------------------------------------------------
// CapturePreSwitchState: most-recent last_input wins; ties on client_id.
// -----------------------------------------------------------------------------

func TestCapturePreSwitchState_MostRecentWins(t *testing.T) {
	t.Parallel()
	clients := []ClientInfo{
		{ClientID: "c1", FocusedPaneID: 1, LastInput: time.Unix(100, 0)},
		{ClientID: "c2", FocusedPaneID: 2, LastInput: time.Unix(200, 0)},
	}
	panes := []Pane{
		{PaneID: 1, WindowID: 7, Workspace: "first"},
		{PaneID: 2, WindowID: 8, Workspace: "second"},
	}
	f := &fakeMux{
		clientsByCall: [][]ClientInfo{clients},
		panesByCall:   [][]Pane{panes},
	}
	c := newClientForTesting(nil, f.run, f.nowFn, f.sleepFn)
	pre, err := c.CapturePreSwitchState(context.Background(), 8)
	if err != nil {
		t.Fatalf("CapturePreSwitchState err=%v", err)
	}
	if pre.TargetClientID != "c2" {
		t.Fatalf("most-recent client should be c2; got %s", pre.TargetClientID)
	}
	if pre.ActiveWorkspace != "second" {
		t.Fatalf("active workspace should derive from focused pane of c2; got %s", pre.ActiveWorkspace)
	}
	if pre.TargetWindowID != 8 {
		t.Fatalf("target window id should be the caller-provided 8; got %d", pre.TargetWindowID)
	}
}

func TestCapturePreSwitchState_TieBreakOnClientID(t *testing.T) {
	t.Parallel()
	clients := []ClientInfo{
		{ClientID: "zeta", FocusedPaneID: 1, LastInput: time.Unix(100, 0)},
		{ClientID: "alpha", FocusedPaneID: 2, LastInput: time.Unix(100, 0)},
	}
	panes := []Pane{
		{PaneID: 1, WindowID: 7, Workspace: "first"},
		{PaneID: 2, WindowID: 8, Workspace: "second"},
	}
	f := &fakeMux{
		clientsByCall: [][]ClientInfo{clients},
		panesByCall:   [][]Pane{panes},
	}
	c := newClientForTesting(nil, f.run, f.nowFn, f.sleepFn)
	pre, err := c.CapturePreSwitchState(context.Background(), 8)
	if err != nil {
		t.Fatalf("CapturePreSwitchState err=%v", err)
	}
	if pre.TargetClientID != "alpha" {
		t.Fatalf("tie-break should pick lexicographically first client_id; got %s", pre.TargetClientID)
	}
}

// -----------------------------------------------------------------------------
// ActivatePane retry semantics — exit-1 once with target still present →
// retry; second exit-1 → ErrPaneClosedRace. Target gone after first exit-1
// → ErrPaneClosedRace immediately.
// -----------------------------------------------------------------------------

type retryRunner struct {
	mu          sync.Mutex
	activateErr []error // pop-front sequence of errors for "activate-pane" calls
	listPanes   []Pane
}

func (r *retryRunner) run(ctx context.Context, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(args) >= 2 && args[1] == "list" {
		b, err := json.Marshal(r.listPanes)
		return b, err
	}
	if len(args) >= 2 && args[1] == "activate-pane" {
		if len(r.activateErr) == 0 {
			return nil, nil
		}
		err := r.activateErr[0]
		r.activateErr = r.activateErr[1:]
		return nil, err
	}
	return nil, nil
}

func TestActivatePane_RetryOnceThenSucceed(t *testing.T) {
	t.Parallel()
	r := &retryRunner{
		activateErr: []error{errors.New("exit 1"), nil},
		listPanes:   []Pane{{PaneID: 42}},
	}
	c := newClientForTesting(nil, r.run, nil, nil)
	if err := c.ActivatePane(context.Background(), 42); err != nil {
		t.Fatalf("ActivatePane should retry then succeed; got %v", err)
	}
}

func TestActivatePane_PaneGoneIsClosedRace(t *testing.T) {
	t.Parallel()
	r := &retryRunner{
		activateErr: []error{errors.New("exit 1")},
		// Pane 42 NOT in the list — closed race.
		listPanes: []Pane{{PaneID: 7}},
	}
	c := newClientForTesting(nil, r.run, nil, nil)
	err := c.ActivatePane(context.Background(), 42)
	if !errors.Is(err, ErrPaneClosedRace) {
		t.Fatalf("expected ErrPaneClosedRace; got %v", err)
	}
}

func TestActivatePane_SecondFailureIsClosedRace(t *testing.T) {
	t.Parallel()
	r := &retryRunner{
		activateErr: []error{errors.New("exit 1"), errors.New("exit 1")},
		listPanes:   []Pane{{PaneID: 42}},
	}
	c := newClientForTesting(nil, r.run, nil, nil)
	err := c.ActivatePane(context.Background(), 42)
	if !errors.Is(err, ErrPaneClosedRace) {
		t.Fatalf("expected ErrPaneClosedRace after second failure; got %v", err)
	}
}

// -----------------------------------------------------------------------------
// RenameWorkspace: collision pre-check; same-name no-op.
// -----------------------------------------------------------------------------

func TestRenameWorkspace_CollisionDetected(t *testing.T) {
	t.Parallel()
	r := &retryRunner{
		listPanes: []Pane{{PaneID: 1, Workspace: "new"}},
	}
	c := newClientForTesting(nil, r.run, nil, nil)
	err := c.RenameWorkspace(context.Background(), "old", "new")
	if !errors.Is(err, ErrRenameCollision) {
		t.Fatalf("expected ErrRenameCollision; got %v", err)
	}
}

func TestRenameWorkspace_SameNameNoOp(t *testing.T) {
	t.Parallel()
	r := &retryRunner{
		// If the runner is invoked at all, the test will see a non-nil
		// listPanes count above the no-op short-circuit.
	}
	c := newClientForTesting(nil, r.run, nil, nil)
	if err := c.RenameWorkspace(context.Background(), "same", "same"); err != nil {
		t.Fatalf("same-name rename should be no-op; got %v", err)
	}
}

// -----------------------------------------------------------------------------
// SpawnInWorkspace: parses pane id from stdout.
// -----------------------------------------------------------------------------

func TestSpawnInWorkspace_ParsesPaneID(t *testing.T) {
	t.Parallel()
	stub := func(ctx context.Context, args ...string) ([]byte, error) {
		// Confirm both required arg pairs.
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--workspace ws") {
			return nil, errors.New("missing --workspace")
		}
		if !strings.Contains(joined, "--cwd /tmp") {
			return nil, errors.New("missing --cwd")
		}
		return []byte("777\n"), nil
	}
	c := newClientForTesting(nil, stub, nil, nil)
	id, err := c.SpawnInWorkspace(context.Background(), "ws", "/tmp")
	if err != nil {
		t.Fatalf("SpawnInWorkspace err=%v", err)
	}
	if id != 777 {
		t.Fatalf("expected pane id 777; got %d", id)
	}
}

func TestSpawnInWorkspace_NonIntegerStdoutFails(t *testing.T) {
	t.Parallel()
	stub := func(ctx context.Context, args ...string) ([]byte, error) {
		return []byte("not-an-id\n"), nil
	}
	c := newClientForTesting(nil, stub, nil, nil)
	_, err := c.SpawnInWorkspace(context.Background(), "ws", "")
	if !errors.Is(err, ErrMuxUnreachable) {
		t.Fatalf("expected ErrMuxUnreachable on non-integer; got %v", err)
	}
}

// -----------------------------------------------------------------------------
// Probe: latency reflects elapsed virtual time.
// -----------------------------------------------------------------------------

func TestProbe_ReportsLatency(t *testing.T) {
	t.Parallel()
	f := &fakeMux{listLatency: 50 * time.Millisecond, panesByCall: [][]Pane{nil}}
	c := newClientForTesting(nil, f.run, f.nowFn, f.sleepFn)
	d, err := c.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe err=%v", err)
	}
	if d != 50*time.Millisecond {
		t.Fatalf("Probe latency want 50ms; got %v", d)
	}
}

func TestProbe_ErrorWrapsMuxUnreachable(t *testing.T) {
	t.Parallel()
	stub := func(ctx context.Context, args ...string) ([]byte, error) {
		return nil, errors.New("boom")
	}
	c := newClientForTesting(nil, stub, nil, nil)
	_, err := c.Probe(context.Background())
	if !errors.Is(err, ErrMuxUnreachable) {
		t.Fatalf("expected ErrMuxUnreachable; got %v", err)
	}
}

// -----------------------------------------------------------------------------
// withCeiling sanity: the wrapper applies the documented per-call ceiling.
// -----------------------------------------------------------------------------

func TestWithCeiling_AppliesPerCallCeiling(t *testing.T) {
	t.Parallel()
	parent, parentCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer parentCancel()
	cctx, cancel := withCeiling(parent)
	defer cancel()
	d, ok := cctx.Deadline()
	if !ok {
		t.Fatalf("withCeiling should set a deadline")
	}
	// Effective deadline is min(parent, +2s) = +2s, so the time-until is
	// at most ~2 s (allowing scheduler jitter).
	until := time.Until(d)
	if until <= 0 || until > perCallCeiling+50*time.Millisecond {
		t.Fatalf("withCeiling deadline should be ~2s out; got %v", until)
	}
}

// -----------------------------------------------------------------------------
// list-clients JSON parsing tolerates flexible LastInput / IdleTime shapes.
// -----------------------------------------------------------------------------

// -----------------------------------------------------------------------------
// HIGH-finding regression: cadence updates run ONLY when a tick actually
// completed both wezcli calls and evaluated the predicate. Error early-outs
// (ListClients err, pinned-client missing, List err, pane missing) preserve
// the previous cadence per §13.3 pseudocode.
//
// Strategy: drive tick 1 as a slow-but-successful mismatch (cadence dilates
// to 250 ms), then drive every subsequent tick into an instant error early-
// out. Under the buggy code (cadence updated unconditionally), tick 2's
// instant elapsed would reset cadence to 50 ms; under the fixed code, the
// 250 ms cadence is preserved through every error tick. Asserting that the
// final observed sleep duration is 250 ms (NOT 50 ms) catches the bug.
// -----------------------------------------------------------------------------

func TestStartSwitchPoller_CadencePreservedAcrossListClientsError(t *testing.T) {
	t.Parallel()
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	errAfter := 1
	f := &fakeMux{
		// Tick 1: list-clients takes 1.5 s + returns a pane mismatch.
		// Tick 2..N: list-clients returns ErrMuxUnreachable instantly.
		listClientsLatencyByCall:  []time.Duration{1500 * time.Millisecond},
		clientsByCall:             [][]ClientInfo{{{ClientID: "c1", FocusedPaneID: 1}}},
		panesByCall:               [][]Pane{{{PaneID: 1, WindowID: 7, Workspace: "other"}}},
		listClientsAlwaysErrAfter: &errAfter,
		listClientsAlwaysErr:      errors.New("transient mux failure"),
	}
	c := newClientForTesting(nil, f.run, f.nowFn, f.sleepFn)
	pre := SwitchPreState{TargetClientID: "c1", TargetWindowID: 7, ActiveWorkspace: "main"}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_ = c.StartSwitchPoller(ctx, pre, "target", false)

	// Sanity: at least two ticks ran (tick 1 success-mismatch + ≥1 error tick).
	if got := f.listClientsCalls.Load(); got < 2 {
		t.Fatalf("expected at least 2 list-clients calls; got %d", got)
	}
	// All sleeps after tick 1 should be 250 ms — cadence dilated by tick 1
	// and preserved across the instant error ticks.
	if got := f.lastSleep(); got != 250*time.Millisecond {
		t.Fatalf("cadence preservation: expected last sleep 250 ms after error ticks; got %v (history=%v)",
			got, f.sleepDurations)
	}
	// Stronger assertion: NO sleep in the history was 50 ms. The buggy
	// implementation would reset cadence to 50 ms on every instant
	// error tick.
	for i, d := range f.sleepDurations {
		if d == 50*time.Millisecond {
			t.Fatalf("cadence preservation: sleep[%d]=50 ms — error tick reset cadence (history=%v)",
				i, f.sleepDurations)
		}
	}
}

func TestStartSwitchPoller_CadencePreservedAcrossPinnedClientMissing(t *testing.T) {
	t.Parallel()
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	// Tick 1: slow successful mismatch → cadence dilates to 250 ms.
	// Tick 2+: list-clients returns OK but pinned client "c1" no longer
	// in the list → evalPredicate early-outs at the pinned == nil branch.
	// Under the buggy code, tick 2's instant elapsed resets cadence to 50 ms.
	tick1Clients := []ClientInfo{{ClientID: "c1", FocusedPaneID: 1}}
	// Tick 2+ has only a different client; pinned "c1" is missing.
	tick2Clients := []ClientInfo{{ClientID: "c2", FocusedPaneID: 99}}
	f := &fakeMux{
		listClientsLatencyByCall: []time.Duration{1500 * time.Millisecond},
		clientsByCall:            [][]ClientInfo{tick1Clients, tick2Clients},
		panesByCall: [][]Pane{
			{{PaneID: 1, WindowID: 7, Workspace: "other"}},
			{{PaneID: 99, WindowID: 7, Workspace: "target"}},
		},
	}
	c := newClientForTesting(nil, f.run, f.nowFn, f.sleepFn)
	pre := SwitchPreState{TargetClientID: "c1", TargetWindowID: 7, ActiveWorkspace: "main"}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_ = c.StartSwitchPoller(ctx, pre, "target", false)

	if got := f.listClientsCalls.Load(); got < 2 {
		t.Fatalf("expected at least 2 list-clients calls; got %d", got)
	}
	if got := f.lastSleep(); got != 250*time.Millisecond {
		t.Fatalf("cadence preservation (pinned missing): expected 250 ms; got %v (history=%v)",
			got, f.sleepDurations)
	}
	for i, d := range f.sleepDurations {
		if d == 50*time.Millisecond {
			t.Fatalf("cadence preservation: sleep[%d]=50 ms — pinned-missing early-out reset cadence (history=%v)",
				i, f.sleepDurations)
		}
	}
}

// -----------------------------------------------------------------------------
// CapturePreSwitchState: empty list-clients surfaces ErrMuxUnreachable
// (covers client.go:268-270 — the empty-mux branch).
// -----------------------------------------------------------------------------

func TestCapturePreSwitchState_EmptyClientsIsMuxUnreachable(t *testing.T) {
	t.Parallel()
	stub := func(ctx context.Context, args ...string) ([]byte, error) {
		// `cli list-clients --format json` returns an empty array — the
		// mux is up but no GUI client is connected.
		return []byte("[]"), nil
	}
	c := newClientForTesting(nil, stub, nil, nil)
	_, err := c.CapturePreSwitchState(context.Background(), 7)
	if !errors.Is(err, ErrMuxUnreachable) {
		t.Fatalf("empty list-clients should surface ErrMuxUnreachable; got %v", err)
	}
}

func TestListClients_FlexibleTimeShapes(t *testing.T) {
	t.Parallel()
	stub := func(ctx context.Context, args ...string) ([]byte, error) {
		return []byte(`[
		  {"client_id":"a","username":"u","hostname":"h","pid":1,"focused_pane_id":1,"last_input":"2026-05-03T12:00:00Z","idle_time":"30s"},
		  {"client_id":"b","username":"u","hostname":"h","pid":2,"focused_pane_id":2,"last_input":1620000000,"idle_time":1.5}
		]`), nil
	}
	c := newClientForTesting(nil, stub, nil, nil)
	clients, err := c.ListClients(context.Background())
	if err != nil {
		t.Fatalf("ListClients err=%v", err)
	}
	if len(clients) != 2 {
		t.Fatalf("expected 2 clients; got %d", len(clients))
	}
	if clients[0].LastInput.IsZero() {
		t.Fatalf("RFC3339 last_input should parse; got zero")
	}
	if clients[0].IdleTime != 30*time.Second {
		t.Fatalf("string idle_time '30s' should parse; got %v", clients[0].IdleTime)
	}
	if clients[1].LastInput.IsZero() {
		t.Fatalf("numeric last_input should parse; got zero")
	}
	if clients[1].IdleTime != 1500*time.Millisecond {
		t.Fatalf("numeric idle_time 1.5 should parse; got %v", clients[1].IdleTime)
	}
}
