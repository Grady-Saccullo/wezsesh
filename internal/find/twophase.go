package find

import (
	"context"
	"fmt"
	"time"

	"github.com/Grady-Saccullo/wezsesh/internal/ipc"
	"github.com/Grady-Saccullo/wezsesh/internal/wezcli"
)

// switchPollerCeiling is the §13.7 polling budget. The wezcli switch
// poller's adaptive cadence runs ticks of 50 ms / 250 ms; per-tick the
// internal 2 s ceiling on each `cli list[-clients]` invocation can cap
// at 4 s in the slow path, so 5 s is documented as the budget rather
// than a true polling interval (see §14.1 + §13.3 commentary on
// switchpoller.go).
const switchPollerCeiling = 5 * time.Second

// runPhase1 is the §13.7 switch sequence:
//
//  1. progress(PhaseSwitchStarted)
//  2. Dispatch "switch" — Lua replies "started" then "completed". We
//     ignore the reply contents and poll the mux instead. This is the
//     only general-purpose post-switch detection mechanism: the
//     `smart_workspace_switcher.workspace_switcher.{chosen,created,
//     selected}` events do NOT fire on programmatic
//     act.SwitchToWorkspace calls (§0.1 row 14).
//  3. wezcli.StartSwitchPoller(WithTimeout(5s), pre, match.Workspace,
//     isRestoreFlow=false) — find never restores; that's the
//     switch+restore path.
//  4. On poller success: progress(PhaseSwitchSucceeded), dispCancel,
//     drain. On poller timeout: drain anyway (the dispatcher goroutine
//     still owns the listener), emit PhaseSwitchTimeout, surface
//     ErrMuxUnreachable.
//
// PhaseSwitchSucceeded is emitted BEFORE the drain per §13.7's success
// path: the success-event order is "predicate fires → progress success →
// dispCancel → drain". The drain is internal cleanup; it should not
// block the success event from reaching the TUI.
//
// The drain protocol (`dispCancel(); for range replies {}`) is mandatory.
// dispCancel closes the listener; the drain ensures the dispatcher's
// drain goroutine sees the channel close, runs its deferred cleanup,
// and unlinks the reply socket BEFORE Phase 2 starts. Skipping it
// leaks the goroutine and races with Phase 2 (§0.1 row 18).
func runPhase1(
	ctx context.Context,
	d ipc.Dispatcher,
	m muxOps,
	pre wezcli.SwitchPreState,
	match Match,
	emit func(Phase),
) error {
	emit(PhaseSwitchStarted)

	dispCtx, dispCancel := context.WithCancel(ctx)
	// dispCancel must always run; the drain is what we care about for
	// goroutine cleanup but the cancel is the trigger that makes the
	// dispatcher's drain goroutine exit.
	replies, err := d.Dispatch(dispCtx, "switch", map[string]any{
		"name": match.Workspace,
		"cwd":  "",
	})
	if err != nil {
		dispCancel()
		return fmt.Errorf("find: dispatch switch: %w", err)
	}

	pollCtx, pollCancel := context.WithTimeout(ctx, switchPollerCeiling)
	pollErr := m.StartSwitchPoller(pollCtx, pre, match.Workspace, false)
	pollCancel()

	if pollErr == nil {
		// Emit the success event BEFORE the drain — the TUI sees the
		// state transition without being held up by listener cleanup.
		emit(PhaseSwitchSucceeded)
	}

	dispCancel()
	// Drain — block until the dispatcher closes the channel. Bounded by
	// the dispatcher's own ctx-Done path inside drain(). Without this,
	// the dispatcher goroutine is still resident when Phase 2's
	// `cli activate-pane` fires, racing on the reply socket inode.
	for range replies {
	}

	if pollErr != nil {
		emit(PhaseSwitchTimeout)
		return pollErr
	}
	return nil
}
