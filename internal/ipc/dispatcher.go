// Package ipc declares the Dispatcher seam consumed by internal/tui and
// internal/find; the concrete implementation lives in internal/ipcdispatcher.
package ipc

import "context"

// Dispatcher performs the two-phase forward dispatch and streams replies.
//
// EmergencyReply is the §13.1 panic-path escape hatch. The TUI subcommand's
// top-level defer recover() invokes it to fan out an UNEXPECTED_EXIT
// sentinel reply to every outstanding reply socket so any in-flight
// caller observes a terminal `completed`/`ok=false` reply (rather than
// timing out at IPC_TIMEOUT) before os.Exit(2). Implementations MUST
// be safe to call concurrently with Dispatch and idempotent on repeat
// calls (the recover path can fire once, but a second call from a
// stacked defer must not panic).
type Dispatcher interface {
	Dispatch(ctx context.Context, verb string, args map[string]any) (<-chan Reply, error)
	EmergencyReply()
}

// Reply mirrors the wire reply envelope.
type Reply struct {
	V        int
	ID       string
	Status   string
	OK       bool
	Data     map[string]any
	Warnings []Warning
	Error    *ReplyError
}

// Warning mirrors a single reply warning entry.
type Warning struct {
	Code, Message string
	Details       map[string]any
}

// ReplyError mirrors a terminal reply's error object.
type ReplyError struct {
	Code, Message string
	Details       map[string]any
}
