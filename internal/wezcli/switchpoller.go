package wezcli

import (
	"context"
	"fmt"
	"time"
)

// Cadence parameters per §13.3 (Switch poller adaptive cadence).
const (
	cadenceFastMs = 50
	cadenceSlowMs = 250
	// cadenceFastUpper / cadenceSlowLower are the two thresholds the spec
	// pseudocode flips on. The 100 ms ≤ tick < 1 s band is intentionally
	// silent — see "Accepted findings" / Documentation note in
	// wezcli_test.go. We preserve the previous cadence in that band.
	cadenceFastUpper = 100 * time.Millisecond
	cadenceSlowLower = 1 * time.Second
)

// StartSwitchPoller polls until pre.TargetClientID's focused pane is
// in workspace `target` AND in window pre.TargetWindowID, OR ctx expires.
//
// Cadence is ADAPTIVE: starts at 50 ms, dilates to 250 ms when the prior
// tick took ≥ 1 s (slow mux), returns to 50 ms when the prior tick took
// < 100 ms. Worst-case per-tick latency is bounded by the internal 2 s
// ctx on each wezcli call; in the slow path two 2 s calls = 4 s per tick.
// With a 5 s parent ctx that yields 1–2 effective ticks; the 5 s ceiling
// is the polling budget, not a true polling interval (§14.1).
//
// Two short-circuits matter:
//
//   - "switch to currently-active workspace" (target == pre.ActiveWorkspace
//     AND !isRestoreFlow): the predicate's third clause is false on every
//     tick, so we test it BEFORE issuing any wezcli calls and return
//     success in 0 ticks. The find code path uses this to skip Phase 1
//     entirely; this guard exists for the rare case where Phase 1 was
//     started anyway (e.g. a programmatic switch-then-restore flow that
//     re-uses the same poller seam).
//
//   - "switch+restore to currently-active workspace" (isRestoreFlow=true):
//     bypasses the third clause so the poller still validates that the
//     focused pane is on the target workspace and window — necessary
//     because a restore op may have spawned new panes.
//
// Predicate scope (§13.3 algorithm):
//
//	pane.Workspace == target
//	  AND pane.WindowID == pre.TargetWindowID
//	  AND ((target != pre.ActiveWorkspace) OR isRestoreFlow)
//
// The window-id clause prevents a false positive when the pinned client's
// focused_pane_id rolls over to a different window (e.g. the user closed
// the originating wezterm window mid-Phase-1).
func (c *Client) StartSwitchPoller(
	ctx context.Context,
	pre SwitchPreState,
	target string,
	isRestoreFlow bool,
) error {
	// Same-workspace bypass — see method doc.
	if target == pre.ActiveWorkspace && !isRestoreFlow {
		return nil
	}

	cadence := time.Duration(cadenceFastMs) * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w: switch poller: %v", ErrMuxUnreachable, err)
		}
		tickStart := c.now()

		// --- Tick body: ListClients → find pinned client → List → match
		// focused pane against the predicate. Per §13.3, EVERY tick
		// re-issues both calls; the pinned TargetClientID is NOT
		// recomputed (multi-client disambiguation lives in
		// CapturePreSwitchState).
		ok, evaluated := c.evalPredicate(ctx, pre, target, isRestoreFlow)
		if ok {
			return nil
		}

		// Adaptive cadence: only flip on the two thresholds; the
		// 100 ms ≤ elapsed < 1 s band preserves the previous cadence
		// (silent in the spec; documented in wezcli_test.go).
		//
		// IMPORTANT: cadence updates only run when the tick fully
		// completed both wezcli calls AND evaluated the predicate
		// (`evaluated == true`). On an early-out — ListClients err,
		// pinned client missing, List err, focused pane missing — the
		// previous cadence is preserved per §13.3 pseudocode
		// ("if err != nil { time.Sleep(cadence_ms); continue }").
		// Folding error early-outs into the cadence-update branch
		// would let a transient error reset cadence to 50 ms and burn
		// the polling budget.
		if evaluated {
			elapsed := c.now().Sub(tickStart)
			switch {
			case elapsed < cadenceFastUpper:
				cadence = time.Duration(cadenceFastMs) * time.Millisecond
			case elapsed >= cadenceSlowLower:
				cadence = time.Duration(cadenceSlowMs) * time.Millisecond
			}
		}

		// Sleep before re-checking ctx so a cancelled parent ctx exits
		// promptly without burning a full cadence interval. The parent
		// ctx.Err() check at the top of the loop catches cancellation.
		c.sleep(cadence)
	}
}

// evalPredicate runs one full tick. The second return value reports whether
// the predicate was actually evaluated against observed state:
//
//   - (true, true)  — predicate satisfied; caller exits the poll loop.
//   - (false, true) — both wezcli calls succeeded and the focused pane was
//                     observed, but the predicate is not yet satisfied
//                     (transient mismatch). Caller updates cadence + sleeps.
//   - (false, false) — early-out: ListClients err, pinned client missing,
//                      List err, or focused pane missing. Caller preserves
//                      cadence and sleeps; the parent ctx ceiling is what
//                      ultimately surfaces ErrMuxUnreachable.
//
// The (matched, evaluated) split (instead of returning a Go error) is
// deliberate: every transient is "retryable" per §13.3, and we don't want a
// transient error to reset cadence — see the cadence comment in
// StartSwitchPoller.
func (c *Client) evalPredicate(ctx context.Context, pre SwitchPreState, target string, isRestoreFlow bool) (matched, evaluated bool) {
	clients, err := c.ListClients(ctx)
	if err != nil {
		return false, false
	}
	var pinned *ClientInfo
	for i := range clients {
		if clients[i].ClientID == pre.TargetClientID {
			pinned = &clients[i]
			break
		}
	}
	if pinned == nil {
		// Pinned client disappeared (e.g. mosh client disconnected
		// mid-poll). Per §0.1 row 18 the predicate fails closed; the
		// ctx-deadline branch surfaces ErrMuxUnreachable on expiry.
		return false, false
	}
	panes, err := c.List(ctx)
	if err != nil {
		return false, false
	}
	var pane *Pane
	for i := range panes {
		if panes[i].PaneID == pinned.FocusedPaneID {
			pane = &panes[i]
			break
		}
	}
	if pane == nil {
		return false, false
	}
	wsMatch := pane.Workspace == target
	winMatch := pane.WindowID == pre.TargetWindowID
	progressed := target != pre.ActiveWorkspace || isRestoreFlow
	return wsMatch && winMatch && progressed, true
}
