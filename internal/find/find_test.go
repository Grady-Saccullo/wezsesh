package find

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/Grady-Saccullo/wezsesh/internal/ipc"
	"github.com/Grady-Saccullo/wezsesh/internal/wezcli"
)

// TestMain wraps the suite with goleak per the §17.3 acceptance gate
// "no leaked goroutines". The dispatcher fakes here all close their
// reply channels themselves; if Activate's drain protocol ever forgets
// to drain, the goleak verify in the dispatcher fake's Close path will
// flag the regression.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// -----------------------------------------------------------------------------
// Test fakes — fakeMux + fakeDispatcher
// -----------------------------------------------------------------------------

// fakeMux implements muxOps. Every method counts invocations and returns
// canned responses. The pollerFn lets a test substitute the poller body
// (since we don't want every Activate test to drive the real wezcli
// switch-poller — that's covered in internal/wezcli/wezcli_test.go).
type fakeMux struct {
	mu sync.Mutex

	listResp []wezcli.Pane
	listErr  error

	preResp wezcli.SwitchPreState
	preErr  error

	activateErr error

	// pollerFn lets each test plug in the desired poller behaviour.
	// Default is "instant success".
	pollerFn func(ctx context.Context, pre wezcli.SwitchPreState, target string, isRestoreFlow bool) error

	listCalls     atomic.Int32
	preCalls      atomic.Int32
	pollerCalls   atomic.Int32
	activateCalls atomic.Int32

	// preWindowIDArgs records the targetWindowID arg passed to each
	// CapturePreSwitchState call. The MEDIUM-finding test asserts the
	// caller threads currentWindowID through (NOT match.WindowID).
	preWindowIDArgs []int

	// activateAt records the time of each ActivatePane invocation so
	// tests can assert ordering vs the dispatcher's drain completion.
	activateAt []time.Time
}

func (f *fakeMux) List(ctx context.Context) ([]wezcli.Pane, error) {
	f.listCalls.Add(1)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listResp, nil
}

func (f *fakeMux) ActivatePane(ctx context.Context, paneID int) error {
	f.activateCalls.Add(1)
	f.mu.Lock()
	f.activateAt = append(f.activateAt, time.Now())
	f.mu.Unlock()
	return f.activateErr
}

func (f *fakeMux) CapturePreSwitchState(ctx context.Context, targetWindowID int) (wezcli.SwitchPreState, error) {
	f.preCalls.Add(1)
	f.mu.Lock()
	f.preWindowIDArgs = append(f.preWindowIDArgs, targetWindowID)
	f.mu.Unlock()
	if f.preErr != nil {
		return wezcli.SwitchPreState{}, f.preErr
	}
	pre := f.preResp
	if pre.TargetWindowID == 0 {
		pre.TargetWindowID = targetWindowID
	}
	return pre, nil
}

func (f *fakeMux) StartSwitchPoller(ctx context.Context, pre wezcli.SwitchPreState, target string, isRestoreFlow bool) error {
	f.pollerCalls.Add(1)
	if f.pollerFn != nil {
		return f.pollerFn(ctx, pre, target, isRestoreFlow)
	}
	return nil
}

// fakeDispatcher implements ipc.Dispatcher. Every Dispatch returns a
// channel and starts a goroutine that closes it when its dispCtx is
// cancelled (mirroring the real dispatcher's drain) or when the test
// pushes a terminal reply via injectReply. The waitForCancel switch
// records the cancellation latency so the drain-protocol test can
// assert on it.
type fakeDispatcher struct {
	mu       sync.Mutex
	dispatch atomic.Int32

	// repliesPerCall, if non-empty, queues replies the goroutine sends
	// before its dispCtx fires. Each Dispatch consumes one slot from
	// the head; out-of-range calls send no replies.
	repliesPerCall [][]ipc.Reply

	// closedAt records the wall-clock time each Dispatch's reply
	// channel was closed (i.e. when the goroutine returned). The drain
	// test asserts (closedAt - dispatchedAt) < 100ms.
	dispatchedAt []time.Time
	closedAt     []time.Time

	// returnedDispatchErr causes the n-th Dispatch call to return an
	// error instead of opening a channel. Used by the dispatch-failure
	// shape test.
	returnedDispatchErr []error

	// wg blocks Wait() until every drain goroutine has exited.
	wg sync.WaitGroup
}

func (d *fakeDispatcher) Dispatch(ctx context.Context, verb string, args map[string]any) (<-chan ipc.Reply, error) {
	idx := int(d.dispatch.Add(1)) - 1
	if idx < len(d.returnedDispatchErr) && d.returnedDispatchErr[idx] != nil {
		return nil, d.returnedDispatchErr[idx]
	}
	d.mu.Lock()
	d.dispatchedAt = append(d.dispatchedAt, time.Now())
	// Reserve the closedAt slot so the goroutine can write to it by
	// index without a second mutex grab on the slice header.
	d.closedAt = append(d.closedAt, time.Time{})
	closedIdx := len(d.closedAt) - 1
	d.mu.Unlock()

	var replies []ipc.Reply
	if idx < len(d.repliesPerCall) {
		replies = d.repliesPerCall[idx]
	}

	out := make(chan ipc.Reply, len(replies)+1)
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		// Send queued replies first, then block on ctx-Done to mirror
		// the real dispatcher's "drain until terminal-reply or cancel"
		// loop.
		for _, r := range replies {
			select {
			case <-ctx.Done():
				goto done
			case out <- r:
			}
		}
		<-ctx.Done()
	done:
		d.mu.Lock()
		d.closedAt[closedIdx] = time.Now()
		d.mu.Unlock()
		close(out)
	}()
	return out, nil
}

func (d *fakeDispatcher) Wait() {
	d.wg.Wait()
}

// EmergencyReply is the §13.1 panic-path fan-out hook. The find tests
// don't exercise the recover branch; a no-op satisfies the
// ipc.Dispatcher interface contract.
func (d *fakeDispatcher) EmergencyReply() {}

// -----------------------------------------------------------------------------
// Acceptance gate: Two-phase find drain — post-poller dispCancel + drain
// → channel closes within 100 ms; goroutines exit cleanly.
// -----------------------------------------------------------------------------

func TestActivate_PhaseOneDrainCloseWithin100ms(t *testing.T) {
	t.Parallel()
	mux := &fakeMux{
		preResp: wezcli.SwitchPreState{
			TargetClientID:  "c1",
			TargetWindowID:  7,
			ActiveWorkspace: "main",
		},
		// Poller succeeds instantly (the spec lets us bypass the
		// real wezcli poller's wall-clock cadence by injecting the
		// outcome directly).
		pollerFn: func(_ context.Context, _ wezcli.SwitchPreState, _ string, _ bool) error {
			return nil
		},
	}
	disp := &fakeDispatcher{
		// One non-terminal reply so the dispatcher's goroutine is
		// active when Phase 1 calls dispCancel — exercising the
		// drain path rather than a no-op close.
		repliesPerCall: [][]ipc.Reply{{
			{V: 1, ID: "x", Status: "started", OK: true},
		}},
	}
	match := Match{
		PaneID:    42,
		WindowID:  9, // different from current window 7
		Workspace: "target",
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	start := time.Now()
	if err := activate(ctx, disp, mux, 7, match, nil); err != nil {
		t.Fatalf("activate: %v", err)
	}
	elapsed := time.Since(start)

	// Phase 2 must have run.
	if mux.activateCalls.Load() != 1 {
		t.Fatalf("ActivatePane should run after Phase 1 success; got %d calls", mux.activateCalls.Load())
	}
	// The dispatcher's drain goroutine must have exited before Activate
	// returned (drain is synchronous on `for range replies`). Wait
	// guards the test from a flake if the goroutine somehow leaked.
	disp.Wait()

	// Cross-check: closedAt - dispatchedAt < 100ms. This is the spec's
	// "drain → channel closes within 100 ms" gate.
	disp.mu.Lock()
	closeLatency := disp.closedAt[0].Sub(disp.dispatchedAt[0])
	disp.mu.Unlock()
	if closeLatency > 100*time.Millisecond {
		t.Fatalf("drain close latency %v exceeds 100 ms ceiling", closeLatency)
	}
	// Sanity: the whole Activate should be fast under the fakes.
	if elapsed > 100*time.Millisecond {
		t.Fatalf("Activate elapsed %v exceeds 100 ms with instant fakes", elapsed)
	}

	// The drain MUST complete before ActivatePane runs (Phase 2 starts
	// only after Phase 1's drain). Compare timestamps.
	mux.mu.Lock()
	activateTime := mux.activateAt[0]
	mux.mu.Unlock()
	if activateTime.Before(disp.closedAt[0]) {
		t.Fatalf("ActivatePane fired before drain completed: activate=%v close=%v",
			activateTime, disp.closedAt[0])
	}
}

// -----------------------------------------------------------------------------
// Acceptance gate: Two-phase find client pinning — second client gaining
// "most recent" mid-poll does NOT flip predicate.
//
// At the find layer, the load-bearing assertion is: CapturePreSwitchState
// is called EXACTLY ONCE before Phase 1, and the same SwitchPreState is
// passed to the poller. Re-querying mid-poll would let a newly-most-recent
// client flip TargetClientID. The poller-internal "do not re-pin" gate is
// covered in internal/wezcli/wezcli_test.go
// (TestStartSwitchPoller_DoesNotRePinClientIDPerTick).
// -----------------------------------------------------------------------------

func TestActivate_CapturesPreSwitchStateExactlyOnce(t *testing.T) {
	t.Parallel()
	pinned := wezcli.SwitchPreState{
		TargetClientID:  "c1",
		TargetWindowID:  9,
		ActiveWorkspace: "main",
	}
	var pollerSawPre wezcli.SwitchPreState
	mux := &fakeMux{
		preResp: pinned,
		pollerFn: func(_ context.Context, pre wezcli.SwitchPreState, _ string, _ bool) error {
			pollerSawPre = pre
			return nil
		},
	}
	disp := &fakeDispatcher{}
	match := Match{
		PaneID:    42,
		WindowID:  9,
		Workspace: "target",
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := activate(ctx, disp, mux, 9, match, nil); err != nil {
		t.Fatalf("activate: %v", err)
	}

	if got := mux.preCalls.Load(); got != 1 {
		t.Fatalf("CapturePreSwitchState should be called exactly once; got %d", got)
	}
	if pollerSawPre.TargetClientID != pinned.TargetClientID {
		t.Fatalf("poller received TargetClientID=%q; want %q (pinned at Phase 1 start)",
			pollerSawPre.TargetClientID, pinned.TargetClientID)
	}
	if pollerSawPre.TargetWindowID != pinned.TargetWindowID {
		t.Fatalf("poller received TargetWindowID=%d; want %d",
			pollerSawPre.TargetWindowID, pinned.TargetWindowID)
	}
	if pollerSawPre.ActiveWorkspace != pinned.ActiveWorkspace {
		t.Fatalf("poller received ActiveWorkspace=%q; want %q",
			pollerSawPre.ActiveWorkspace, pinned.ActiveWorkspace)
	}
	disp.Wait()
}

// -----------------------------------------------------------------------------
// Acceptance gate: Two-phase find window scoping — closing wezterm window
// mid-Phase-1 → MUX_UNREACHABLE.
//
// We model "window closed" as the poller returning ErrMuxUnreachable
// (which is what the real switch-poller surfaces when the predicate
// fails until ctx-deadline; the predicate fails because the focused
// pane's WindowID no longer matches pre.TargetWindowID). The find layer
// must propagate that error AND emit PhaseSwitchTimeout AND skip Phase 2.
// -----------------------------------------------------------------------------

func TestActivate_WindowClosedMidPhase1SurfacesMuxUnreachable(t *testing.T) {
	t.Parallel()
	mux := &fakeMux{
		preResp: wezcli.SwitchPreState{
			TargetClientID:  "c1",
			TargetWindowID:  9,
			ActiveWorkspace: "main",
		},
		pollerFn: func(_ context.Context, _ wezcli.SwitchPreState, _ string, _ bool) error {
			return wezcli.ErrMuxUnreachable
		},
	}
	disp := &fakeDispatcher{}
	match := Match{
		PaneID:    42,
		WindowID:  9,
		Workspace: "target",
	}

	var phases []Phase
	progress := func(p Phase) { phases = append(phases, p) }

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := activate(ctx, disp, mux, 9, match, progress)
	if !errors.Is(err, wezcli.ErrMuxUnreachable) {
		t.Fatalf("expected ErrMuxUnreachable; got %v", err)
	}

	// Phase 2 must NOT have run.
	if got := mux.activateCalls.Load(); got != 0 {
		t.Fatalf("ActivatePane must NOT run when Phase 1 fails; got %d calls", got)
	}

	// Progress must include PhaseSwitchTimeout (NOT PhaseSwitchSucceeded
	// or PhaseActivateStarted).
	wantSeen := map[Phase]bool{PhaseSwitchStarted: false, PhaseSwitchTimeout: false}
	for _, p := range phases {
		if _, ok := wantSeen[p]; ok {
			wantSeen[p] = true
		}
		if p == PhaseSwitchSucceeded || p == PhaseActivateStarted || p == PhaseActivateDone {
			t.Fatalf("unexpected phase %q after Phase 1 failure (sequence=%v)", p, phases)
		}
	}
	for p, seen := range wantSeen {
		if !seen {
			t.Fatalf("missing phase %q in sequence %v", p, phases)
		}
	}

	disp.Wait()
}

// -----------------------------------------------------------------------------
// Acceptance gate: Phase 2 NEVER runs without Phase 1 when
// match.Workspace != currentActiveWorkspace.
//
// We assert two things:
//
//  1. Different-workspace find triggers Phase 1 (poller called) before
//     ActivatePane.
//  2. Same-workspace find SKIPS Phase 1 (poller NOT called) and goes
//     straight to Phase 2.
//
// The acceptance gate is the negative form of (1) — without Phase 1,
// cross-workspace activate-pane is a no-op from the user's POV (§0.1
// row 13).
// -----------------------------------------------------------------------------

func TestActivate_DifferentWorkspaceRunsPhaseOneBeforePhaseTwo(t *testing.T) {
	t.Parallel()
	var (
		pollerStartedAt   time.Time
		pollerHasReturned atomic.Bool
	)
	mux := &fakeMux{
		preResp: wezcli.SwitchPreState{
			TargetClientID:  "c1",
			TargetWindowID:  9,
			ActiveWorkspace: "main",
		},
		pollerFn: func(_ context.Context, _ wezcli.SwitchPreState, _ string, _ bool) error {
			pollerStartedAt = time.Now()
			// Tiny sleep so the activate-after-poller ordering is
			// observable even on very fast CI machines.
			time.Sleep(2 * time.Millisecond)
			pollerHasReturned.Store(true)
			return nil
		},
	}
	disp := &fakeDispatcher{}
	match := Match{
		PaneID:    42,
		WindowID:  9,
		Workspace: "target",
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := activate(ctx, disp, mux, 9, match, nil); err != nil {
		t.Fatalf("activate: %v", err)
	}

	if got := mux.pollerCalls.Load(); got != 1 {
		t.Fatalf("Phase 1 poller should run exactly once for cross-workspace find; got %d", got)
	}
	if got := mux.activateCalls.Load(); got != 1 {
		t.Fatalf("Phase 2 ActivatePane should run exactly once; got %d", got)
	}
	if !pollerHasReturned.Load() {
		t.Fatalf("ActivatePane fired before poller returned")
	}
	if disp.dispatch.Load() != 1 {
		t.Fatalf("Phase 1 dispatch should fire exactly once; got %d", disp.dispatch.Load())
	}
	mux.mu.Lock()
	activateAt := mux.activateAt[0]
	mux.mu.Unlock()
	if activateAt.Before(pollerStartedAt) {
		t.Fatalf("ActivatePane fired before poller started: activate=%v poller=%v",
			activateAt, pollerStartedAt)
	}
	disp.Wait()
}

func TestActivate_SameWorkspaceSkipsPhaseOne(t *testing.T) {
	t.Parallel()
	mux := &fakeMux{
		preResp: wezcli.SwitchPreState{
			TargetClientID:  "c1",
			TargetWindowID:  9,
			ActiveWorkspace: "main",
		},
	}
	disp := &fakeDispatcher{}
	match := Match{
		PaneID:    42,
		WindowID:  9,
		Workspace: "main", // same as ActiveWorkspace
	}

	var phases []Phase
	progress := func(p Phase) { phases = append(phases, p) }

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := activate(ctx, disp, mux, 9, match, progress); err != nil {
		t.Fatalf("activate: %v", err)
	}

	if got := disp.dispatch.Load(); got != 0 {
		t.Fatalf("same-workspace find should NOT dispatch switch verb; got %d dispatches", got)
	}
	if got := mux.pollerCalls.Load(); got != 0 {
		t.Fatalf("same-workspace find should NOT run poller; got %d poller calls", got)
	}
	if got := mux.activateCalls.Load(); got != 1 {
		t.Fatalf("same-workspace find must still run Phase 2; got %d activate calls", got)
	}
	for _, p := range phases {
		if p == PhaseSwitchStarted || p == PhaseSwitchSucceeded || p == PhaseSwitchTimeout {
			t.Fatalf("same-workspace find emitted Phase 1 progress %q (sequence=%v)", p, phases)
		}
	}
	disp.Wait()
}

// -----------------------------------------------------------------------------
// HIGH-finding regression: same-workspace BUT cross-window find MUST run
// Phase 1. §13.7 skip-Phase-1 condition is `workspace == active AND
// windowID == currentWindow`. Either inequality drops us into Phase 1.
// Without this gate, the user invoking find on a pane in window B while
// focused on window A would see the pane "activate" (server-side) but
// remain visually invisible.
// -----------------------------------------------------------------------------

func TestActivate_SameWorkspaceCrossWindowRunsPhaseOne(t *testing.T) {
	t.Parallel()
	mux := &fakeMux{
		preResp: wezcli.SwitchPreState{
			TargetClientID:  "c1",
			TargetWindowID:  9, // user is in window 9
			ActiveWorkspace: "main",
		},
	}
	disp := &fakeDispatcher{}
	match := Match{
		PaneID:    42,
		WindowID:  10, // target pane is in a different window
		Workspace: "main", // same workspace
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := activate(ctx, disp, mux, 9, match, nil); err != nil {
		t.Fatalf("activate: %v", err)
	}

	if got := mux.pollerCalls.Load(); got != 1 {
		t.Fatalf("same-workspace cross-window find MUST run Phase 1; got %d poller calls", got)
	}
	if got := disp.dispatch.Load(); got != 1 {
		t.Fatalf("same-workspace cross-window find MUST dispatch switch verb; got %d", got)
	}
	if got := mux.activateCalls.Load(); got != 1 {
		t.Fatalf("Phase 2 must run; got %d activate calls", got)
	}
	disp.Wait()
}

// -----------------------------------------------------------------------------
// MEDIUM-finding regression: structurally pin parity between Activate's
// `currentWindowID` parameter and (a) the targetWindowID arg passed into
// CapturePreSwitchState, (b) the pre.TargetWindowID consumed by the
// switch poller. Without this assertion a future regression of the
// CRITICAL bug — passing match.WindowID to CapturePreSwitchState — would
// pass every other test silently (because matched-window tests happen to
// align by accident); only this parity check catches it.
// -----------------------------------------------------------------------------

func TestActivate_ForwardsCurrentWindowIDToCapturePreSwitchState(t *testing.T) {
	t.Parallel()
	const currentWindow = 7
	const matchWindow = 9 // distinct from currentWindow on purpose
	var pollerSawPre wezcli.SwitchPreState
	mux := &fakeMux{
		// preResp.TargetWindowID is intentionally 0 here so the fakeMux
		// echoes whatever targetWindowID was passed into Capture — that
		// way the poller observes the value Activate threaded through,
		// not whatever the test pre-stuffed.
		preResp: wezcli.SwitchPreState{
			TargetClientID:  "c1",
			ActiveWorkspace: "main",
		},
		pollerFn: func(_ context.Context, pre wezcli.SwitchPreState, _ string, _ bool) error {
			pollerSawPre = pre
			return nil
		},
	}
	disp := &fakeDispatcher{}
	match := Match{
		PaneID:    42,
		WindowID:  matchWindow,
		Workspace: "target",
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := activate(ctx, disp, mux, currentWindow, match, nil); err != nil {
		t.Fatalf("activate: %v", err)
	}

	mux.mu.Lock()
	args := append([]int(nil), mux.preWindowIDArgs...)
	mux.mu.Unlock()

	// (a) targetWindowID into CapturePreSwitchState MUST be currentWindow,
	// NOT match.WindowID. This is the structural CRITICAL-bug guard.
	if len(args) != 1 {
		t.Fatalf("CapturePreSwitchState should be called exactly once; got %d", len(args))
	}
	if args[0] != currentWindow {
		t.Fatalf("CapturePreSwitchState targetWindowID=%d; want currentWindowID=%d (NOT match.WindowID=%d)",
			args[0], currentWindow, matchWindow)
	}

	// (b) The same value MUST flow through pre.TargetWindowID into the
	// poller — that's the field the §13.3 predicate compares against
	// pane.WindowID.
	if pollerSawPre.TargetWindowID != currentWindow {
		t.Fatalf("poller observed pre.TargetWindowID=%d; want %d (parity with currentWindowID)",
			pollerSawPre.TargetWindowID, currentWindow)
	}
	disp.Wait()
}

// -----------------------------------------------------------------------------
// Auxiliary: dispatcher Dispatch failure is surfaced (not silently dropped).
// -----------------------------------------------------------------------------

func TestActivate_DispatchErrorSurfaces(t *testing.T) {
	t.Parallel()
	mux := &fakeMux{
		preResp: wezcli.SwitchPreState{
			TargetClientID:  "c1",
			TargetWindowID:  9,
			ActiveWorkspace: "main",
		},
	}
	want := errors.New("dispatcher: intentional")
	disp := &fakeDispatcher{returnedDispatchErr: []error{want}}
	match := Match{
		PaneID:    42,
		WindowID:  9,
		Workspace: "target",
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := activate(ctx, disp, mux, 9, match, nil)
	if err == nil || !errors.Is(err, want) {
		t.Fatalf("expected wrapped dispatcher error; got %v", err)
	}
	if got := mux.activateCalls.Load(); got != 0 {
		t.Fatalf("Phase 2 must NOT run when Dispatch fails; got %d", got)
	}
	disp.Wait()
}

// -----------------------------------------------------------------------------
// Search smoke tests — the §8.14 surface returns matches across the
// haystack columns. The two-phase tests above carry the load-bearing
// invariants; these confirm the simple filter shape compiles and behaves.
// -----------------------------------------------------------------------------

func TestSearch_FiltersByPattern(t *testing.T) {
	t.Parallel()
	mux := &fakeMux{
		listResp: []wezcli.Pane{
			{PaneID: 1, Workspace: "main", Title: "vim foo.go"},
			{PaneID: 2, Workspace: "main", Title: "shell"},
			{PaneID: 3, Workspace: "other", TabTitle: "vim build"},
			{PaneID: 4, Workspace: "main", CWD: "file:///home/u/proj"},
		},
	}
	got, err := search(context.Background(), mux, "vim", Options{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 matches; got %d (%+v)", len(got), got)
	}
	want := map[int]string{1: "title", 3: "tab_title"}
	for _, m := range got {
		if want[m.PaneID] != m.SourceField {
			t.Fatalf("pane %d source=%q; want %q", m.PaneID, m.SourceField, want[m.PaneID])
		}
	}
}

func TestSearch_WorkspaceFilter(t *testing.T) {
	t.Parallel()
	mux := &fakeMux{
		listResp: []wezcli.Pane{
			{PaneID: 1, Workspace: "main", Title: "x"},
			{PaneID: 2, Workspace: "other", Title: "x"},
		},
	}
	got, err := search(context.Background(), mux, "x", Options{Workspace: "main"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 || got[0].PaneID != 1 {
		t.Fatalf("workspace filter failed; got %+v", got)
	}
}

func TestSearch_CWDOnlyRejectsEmptyCWD(t *testing.T) {
	t.Parallel()
	mux := &fakeMux{
		listResp: []wezcli.Pane{
			{PaneID: 1, Workspace: "main", CWD: ""},                  // empty cwd guard
			{PaneID: 2, Workspace: "main", CWD: "file:///home/u/p"},  // ok
			{PaneID: 3, Workspace: "main", CWD: "file:///etc"},       // no needle hit
		},
	}
	got, err := search(context.Background(), mux, "home", Options{CWDOnly: true})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 || got[0].PaneID != 2 {
		t.Fatalf("CWDOnly should match only pane 2; got %+v", got)
	}
}
