package ipc

import (
	"context"
	"errors"
	"fmt"
)

// ErrPartialNotAllowed surfaces when AwaitTerminal sees a `partial`
// reply. Bootstrap-class verbs (the only AwaitTerminal callers today)
// contract for a single `completed` reply; a `partial` is a
// plugin-side bug and surfaces as an init-protocol violation.
var ErrPartialNotAllowed = errors.New("ipc: partial reply not allowed for AwaitTerminal")

// ErrChannelClosedEarly surfaces when the reply channel closes before
// any terminal reply lands. The dispatcher closes its channel after
// the per-request goroutine exits; closure without a terminal means
// the plugin never replied and the reply socket was torn down by
// caller-side context cancellation or a deadline.
var ErrChannelClosedEarly = errors.New("ipc: reply channel closed before terminal")

// AwaitTerminal performs Dispatch + drain-to-terminal. Returns the
// single terminal Reply, or an error.
//
// Use this from synchronous, pre-TUI / pre-bubbletea callers (today:
// the bootstrap fetch in cmd/wezsesh/main.go::tuiSetup). Inside the
// bubbletea event loop, drive Dispatch directly via the existing
// `tea.Cmd` channel pattern — AwaitTerminal would block the renderer.
//
// Semantics:
//
//   - Status="started" replies are non-terminal and are skipped (the
//     dispatcher will deliver another reply within the §13.1 reply
//     budget).
//   - Status="partial" replies are rejected with ErrPartialNotAllowed:
//     bootstrap-class verbs contract for a single completed reply.
//   - Status="completed" is terminal. Returned regardless of OK/error;
//     the caller checks reply.OK and reply.Error.
//   - Channel close before terminal returns ErrChannelClosedEarly,
//     wrapping ctx.Err() when ctx is done.
//   - ctx cancellation during the wait surfaces as ctx.Err() — the
//     dispatcher's drain goroutine will close the channel shortly.
func AwaitTerminal(ctx context.Context, d Dispatcher, verb string, args map[string]any) (Reply, error) {
	ch, err := d.Dispatch(ctx, verb, args)
	if err != nil {
		return Reply{}, fmt.Errorf("ipc: dispatch %s: %w", verb, err)
	}

	for {
		select {
		case reply, ok := <-ch:
			if !ok {
				if cerr := ctx.Err(); cerr != nil {
					return Reply{}, fmt.Errorf("%w: %w", ErrChannelClosedEarly, cerr)
				}
				return Reply{}, ErrChannelClosedEarly
			}
			switch reply.Status {
			case "started":
				continue
			case "partial":
				return Reply{}, ErrPartialNotAllowed
			case "completed":
				return reply, nil
			default:
				return Reply{}, fmt.Errorf("ipc: unexpected reply status %q for %s", reply.Status, verb)
			}
		case <-ctx.Done():
			return Reply{}, ctx.Err()
		}
	}
}
