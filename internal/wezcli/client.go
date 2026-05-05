// Package wezcli is the project's sole gateway to `wezterm cli`. Every
// invocation of the wezterm CLI in wezsesh — `cli list`, `cli list-clients`,
// `cli activate-pane`, `cli activate-tab`, `cli rename-workspace`,
// `cli spawn` — flows through this package, so the project-wide CI lint
// (`grep exec.Command(...,"wezterm",...)`) can guard the boundary at a
// single place. See §8.9 and §0.1 row 18 / row 19 for the contract.
//
// All exported methods take a `context.Context` and apply an internal 2 s
// ceiling per call (§14.1 row "Single `wezterm cli` invocation"); the
// effective per-call deadline is `min(callerCtx, 2s)`. A timeout, non-zero
// exit, or invalid JSON surfaces as ErrMuxUnreachable. The two-phase find
// flow in §13.7 + the §13.3 switch-poller assume this 2 s ceiling; do not
// raise it without revisiting both.
//
// Test seam: NewClient resolves wezterm via exec.LookPath and sets a
// default `runner` that shells out via exec.CommandContext. Tests construct
// the Client through newClientForTesting and inject a `runner` that returns
// canned bytes (or simulates slow ticks for the switch-poller adaptive
// cadence test). The seam is unexported so the public surface is "wezterm
// CLI in, typed Go out".
package wezcli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Grady-Saccullo/wezsesh/internal/logger"
)

// Sentinel errors returned by Client. All four match §8.9.
var (
	ErrWeztermNotFound = errors.New("wezcli: wezterm not on PATH")
	ErrMuxUnreachable  = errors.New("wezcli: mux unreachable")
	ErrRenameCollision = errors.New("wezcli: rename collision")
	ErrPaneClosedRace  = errors.New("wezcli: pane closed race")
)

// perCallCeiling is the §14.1 ceiling on a single `wezterm cli` invocation.
// The effective deadline is min(callerCtx, perCallCeiling); see withCeiling.
const perCallCeiling = 2 * time.Second

// runner is the unexported test seam. The default implementation is
// runWeztermCLI (which shells out via exec.CommandContext); tests inject
// a canned-bytes / slow-tick implementation through newClientForTesting.
type runner func(ctx context.Context, args ...string) ([]byte, error)

// Client is the thin holder for the resolved wezterm path + logger. It is
// safe for concurrent use; the only mutable state is a logger handle.
//
// The unexported `now` and `sleep` fields exist so the §13.3 switch-poller
// adaptive-cadence test can inject a virtual clock; production code uses
// time.Now and time.Sleep.
type Client struct {
	binary string
	log    *logger.Logger
	run    runner

	now   func() time.Time
	sleep func(time.Duration)
}

// NewClient resolves `wezterm` via exec.LookPath. Returns ErrWeztermNotFound
// when the binary is absent. The logger is permitted to be nil (the
// *logger.Logger methods are nil-safe; production code wires a real one).
func NewClient(log *logger.Logger) (*Client, error) {
	bin, err := exec.LookPath("wezterm")
	if err != nil {
		return nil, ErrWeztermNotFound
	}
	c := &Client{
		binary: bin,
		log:    log,
		now:    time.Now,
		sleep:  time.Sleep,
	}
	c.run = c.runWeztermCLI
	return c, nil
}

// newClientForTesting constructs a Client with a stubbed runner, clock,
// and sleep hook. The binary path is recorded for debug parity but is
// never invoked.
func newClientForTesting(log *logger.Logger, r runner, now func() time.Time, sleep func(time.Duration)) *Client {
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	return &Client{
		binary: "/test/wezterm",
		log:    log,
		run:    r,
		now:    now,
		sleep:  sleep,
	}
}

// withCeiling returns a context whose deadline is min(parent, +perCallCeiling).
// The cancel must always be invoked — either via defer or after the call
// completes — to release the timer goroutine.
func withCeiling(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, perCallCeiling)
}

// runWeztermCLI is the default runner. It applies the per-call ceiling and
// shells out via exec.CommandContext.
//
// Returns the child's stdout only (cmd.Output); stderr is captured by the
// exec package and surfaced via (*exec.ExitError).Stderr on a non-zero exit.
// JSON-emitting subcommands (cli list / list-clients) write the payload to
// stdout, so this is the right shape for them; void subcommands
// (activate-pane / activate-tab / rename-workspace) return empty stdout on
// success, and on failure callers can type-assert err to (*exec.ExitError)
// to inspect Stderr if needed. A non-zero exit, ctx-deadline, or any
// process-start failure surfaces as a non-nil error; callers map it to the
// appropriate sentinel.
func (c *Client) runWeztermCLI(ctx context.Context, args ...string) ([]byte, error) {
	cctx, cancel := withCeiling(ctx)
	defer cancel()
	cmd := exec.CommandContext(cctx, c.binary, args...)
	out, err := cmd.Output()
	if err != nil {
		return out, err
	}
	return out, nil
}

// Probe runs `cli list` once and reports observed latency. Used by doctor
// (§8.17). On error, the returned latency is the time spent before failure.
func (c *Client) Probe(ctx context.Context) (time.Duration, error) {
	start := c.now()
	_, err := c.run(ctx, "cli", "list", "--format", "json")
	elapsed := c.now().Sub(start)
	if err != nil {
		return elapsed, fmt.Errorf("%w: %v", ErrMuxUnreachable, err)
	}
	return elapsed, nil
}

// RenameWorkspace performs a pre-collision check via List, then issues
// `cli rename-workspace <old> <new>`. Same-name short-circuits as no-op
// success. Returns ErrRenameCollision when <new> already exists in the
// mux. Any other CLI failure surfaces as ErrMuxUnreachable.
func (c *Client) RenameWorkspace(ctx context.Context, oldName, newName string) error {
	if oldName == newName {
		return nil
	}
	panes, err := c.List(ctx)
	if err != nil {
		return err
	}
	for _, p := range panes {
		if p.Workspace == newName {
			return ErrRenameCollision
		}
	}
	if _, err := c.run(ctx, "cli", "rename-workspace", oldName, newName); err != nil {
		return fmt.Errorf("%w: %v", ErrMuxUnreachable, err)
	}
	return nil
}

// ActivatePane runs `cli activate-pane --pane-id N`. On exit 1 (pane went
// away between List and Activate), re-list once and retry; second failure
// returns ErrPaneClosedRace.
//
// Cross-workspace caveat: server-side activate-pane does NOT change the
// client's active workspace. Callers crossing workspaces MUST run the §13.3
// switch poller (Phase 1) before invoking ActivatePane (Phase 2). See §13.7.
func (c *Client) ActivatePane(ctx context.Context, paneID int) error {
	return c.activateRetry(ctx,
		[]string{"cli", "activate-pane", "--pane-id", strconv.Itoa(paneID)},
		func(panes []Pane) bool {
			for _, p := range panes {
				if p.PaneID == paneID {
					return true
				}
			}
			return false
		},
	)
}

// ActivateTab runs `cli activate-tab --tab-id N`. Same retry semantics as
// ActivatePane.
func (c *Client) ActivateTab(ctx context.Context, tabID int) error {
	return c.activateRetry(ctx,
		[]string{"cli", "activate-tab", "--tab-id", strconv.Itoa(tabID)},
		func(panes []Pane) bool {
			for _, p := range panes {
				if p.TabID == tabID {
					return true
				}
			}
			return false
		},
	)
}

// activateRetry is the shared retry-on-failure helper for ActivatePane /
// ActivateTab. On the first failure, it re-lists the mux and consults the
// presenceCheck closure: if the target is no longer present, the failure
// IS the closed race and ErrPaneClosedRace surfaces immediately. If the
// target is still present, retry once; a second failure also surfaces
// ErrPaneClosedRace per §8.9.
func (c *Client) activateRetry(ctx context.Context, args []string, presenceCheck func([]Pane) bool) error {
	if _, err := c.run(ctx, args...); err == nil {
		return nil
	}
	panes, listErr := c.List(ctx)
	if listErr != nil {
		// Cannot determine presence; surface as mux-unreachable rather
		// than misclassifying a network failure as a closed-race.
		return listErr
	}
	if !presenceCheck(panes) {
		return ErrPaneClosedRace
	}
	if _, err := c.run(ctx, args...); err != nil {
		return ErrPaneClosedRace
	}
	return nil
}

// SpawnInWorkspace runs `cli spawn --workspace <name> --cwd <cwd>` and
// returns the pane ID printed on stdout. An empty cwd is allowed and
// causes wezterm to inherit the spawning pane's working directory.
//
// NOTE (§0.1 / §13.3 caveat): wezterm's `act.SwitchToWorkspace.spawn` only
// runs the spawn block when the workspace doesn't yet exist; spawning into
// an existing workspace MUST go through this method (which uses
// `cli spawn --workspace`, equivalent to `mux.spawn_window { workspace, cwd }`)
// rather than a SwitchToWorkspace action. The caller is expected to know
// whether the workspace already exists; SpawnInWorkspace itself is
// unconditional.
func (c *Client) SpawnInWorkspace(ctx context.Context, workspace, cwd string) (int, error) {
	args := []string{"cli", "spawn", "--workspace", workspace}
	if cwd != "" {
		args = append(args, "--cwd", cwd)
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrMuxUnreachable, err)
	}
	id, perr := strconv.Atoi(strings.TrimSpace(string(out)))
	if perr != nil {
		return 0, fmt.Errorf("%w: spawn returned non-integer pane id %q", ErrMuxUnreachable, string(out))
	}
	return id, nil
}

// CapturePreSwitchState reads ListClients, picks the most-recent
// last_input client (tie-break on client_id, lexicographic), then reads
// List once to find the active workspace of that client's focused pane.
// Returns the pre-state used by StartSwitchPoller (§13.3).
//
// Multi-client disambiguation lives at this seam: the chosen client_id
// is pinned in SwitchPreState.TargetClientID and never re-evaluated by
// the poller. A second connected GUI client (mosh, SSH-mux, another local
// wezterm) becoming "most recent" mid-poll would otherwise flip the
// poller's predicate to the wrong workspace.
func (c *Client) CapturePreSwitchState(ctx context.Context, targetWindowID int) (SwitchPreState, error) {
	clients, err := c.ListClients(ctx)
	if err != nil {
		return SwitchPreState{}, err
	}
	if len(clients) == 0 {
		return SwitchPreState{}, fmt.Errorf("%w: no wezterm clients", ErrMuxUnreachable)
	}
	chosen := pickMostRecentClient(clients)
	panes, err := c.List(ctx)
	if err != nil {
		return SwitchPreState{}, err
	}
	var active string
	for _, p := range panes {
		if p.PaneID == chosen.FocusedPaneID {
			active = p.Workspace
			break
		}
	}
	return SwitchPreState{
		TargetClientID:  chosen.ClientID,
		TargetWindowID:  targetWindowID,
		ActiveWorkspace: active,
	}, nil
}

// pickMostRecentClient returns the client with the most-recent LastInput
// timestamp. Ties are broken on ClientID lexicographic order so the
// selection is stable across calls.
func pickMostRecentClient(clients []ClientInfo) ClientInfo {
	best := clients[0]
	for _, c := range clients[1:] {
		if c.LastInput.After(best.LastInput) {
			best = c
			continue
		}
		if c.LastInput.Equal(best.LastInput) && c.ClientID < best.ClientID {
			best = c
		}
	}
	return best
}

// SwitchPreState is captured at Phase 1 start and consumed by
// StartSwitchPoller. The TargetClientID pin is the load-bearing field —
// see §0.1 row 18 / the §13.3 algorithm for the rationale.
type SwitchPreState struct {
	TargetClientID  string
	TargetWindowID  int
	ActiveWorkspace string
}
