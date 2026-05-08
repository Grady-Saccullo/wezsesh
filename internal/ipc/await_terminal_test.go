package ipc

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeDispatcher exposes a static reply channel and a configurable
// dispatch error so tests can drive AwaitTerminal through every branch
// without spinning a real listener.
type fakeDispatcher struct {
	replies     []Reply
	closeAfter  bool // close channel after replies are sent
	dispatchErr error
}

func (f *fakeDispatcher) Dispatch(ctx context.Context, verb string, args map[string]any) (<-chan Reply, error) {
	if f.dispatchErr != nil {
		return nil, f.dispatchErr
	}
	ch := make(chan Reply, len(f.replies))
	for _, r := range f.replies {
		ch <- r
	}
	if f.closeAfter {
		close(ch)
	}
	return ch, nil
}

func (f *fakeDispatcher) EmergencyReply() {}

func TestAwaitTerminal_CompletedOK(t *testing.T) {
	want := Reply{V: 2, ID: "01J", Status: "completed", OK: true,
		Data: map[string]any{"snapshot_dir": "/sd"}}
	d := &fakeDispatcher{replies: []Reply{want}, closeAfter: true}
	got, err := AwaitTerminal(context.Background(), d, "bootstrap", nil)
	if err != nil {
		t.Fatalf("AwaitTerminal: %v", err)
	}
	if got.Status != "completed" || !got.OK {
		t.Errorf("got = %+v, want completed/ok=true", got)
	}
	if got.Data["snapshot_dir"] != "/sd" {
		t.Errorf("Data missing snapshot_dir: %+v", got.Data)
	}
}

func TestAwaitTerminal_CompletedErrorReturnedNotErrored(t *testing.T) {
	// A `completed` reply with ok=false is the wire's terminal-error
	// shape; AwaitTerminal returns it WITHOUT a Go error so the caller
	// can inspect reply.Error and surface a structured failure.
	want := Reply{V: 2, ID: "01J", Status: "completed", OK: false,
		Error: &ReplyError{Code: "BOOT_FAILED", Message: "missing key"}}
	d := &fakeDispatcher{replies: []Reply{want}, closeAfter: true}
	got, err := AwaitTerminal(context.Background(), d, "bootstrap", nil)
	if err != nil {
		t.Fatalf("AwaitTerminal: %v", err)
	}
	if got.OK {
		t.Errorf("got OK=true, want false")
	}
	if got.Error == nil || got.Error.Code != "BOOT_FAILED" {
		t.Errorf("Error = %+v, want BOOT_FAILED", got.Error)
	}
}

func TestAwaitTerminal_StartedThenCompletedSkipsStarted(t *testing.T) {
	d := &fakeDispatcher{
		replies: []Reply{
			{V: 2, ID: "01J", Status: "started", OK: true},
			{V: 2, ID: "01J", Status: "completed", OK: true},
		},
		closeAfter: true,
	}
	got, err := AwaitTerminal(context.Background(), d, "bootstrap", nil)
	if err != nil {
		t.Fatalf("AwaitTerminal: %v", err)
	}
	if got.Status != "completed" {
		t.Errorf("got status %q, want completed", got.Status)
	}
}

func TestAwaitTerminal_PartialRejected(t *testing.T) {
	d := &fakeDispatcher{
		replies: []Reply{
			{V: 2, ID: "01J", Status: "partial", OK: true,
				Data: map[string]any{"x": "y"}},
		},
		closeAfter: true,
	}
	_, err := AwaitTerminal(context.Background(), d, "bootstrap", nil)
	if !errors.Is(err, ErrPartialNotAllowed) {
		t.Errorf("err = %v, want ErrPartialNotAllowed", err)
	}
}

func TestAwaitTerminal_UnknownStatus(t *testing.T) {
	d := &fakeDispatcher{
		replies:    []Reply{{V: 2, ID: "01J", Status: "weird"}},
		closeAfter: true,
	}
	_, err := AwaitTerminal(context.Background(), d, "bootstrap", nil)
	if err == nil || !contains(err.Error(), "unexpected reply status") {
		t.Errorf("err = %v, want unexpected reply status", err)
	}
}

func TestAwaitTerminal_ChannelClosedEarly(t *testing.T) {
	// No replies queued, but channel closes immediately.
	d := &fakeDispatcher{closeAfter: true}
	_, err := AwaitTerminal(context.Background(), d, "bootstrap", nil)
	if !errors.Is(err, ErrChannelClosedEarly) {
		t.Errorf("err = %v, want ErrChannelClosedEarly", err)
	}
}

func TestAwaitTerminal_DispatchError(t *testing.T) {
	want := errors.New("listener bind failed")
	d := &fakeDispatcher{dispatchErr: want}
	_, err := AwaitTerminal(context.Background(), d, "bootstrap", nil)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrapping %v", err, want)
	}
}

func TestAwaitTerminal_ContextCanceled(t *testing.T) {
	// Channel never delivers; ctx cancels.
	ch := make(chan Reply) // unbuffered, never written
	d := &stuckDispatcher{ch: ch}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	_, err := AwaitTerminal(ctx, d, "bootstrap", nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

type stuckDispatcher struct{ ch chan Reply }

func (s *stuckDispatcher) Dispatch(ctx context.Context, verb string, args map[string]any) (<-chan Reply, error) {
	return s.ch, nil
}
func (s *stuckDispatcher) EmergencyReply() {}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
