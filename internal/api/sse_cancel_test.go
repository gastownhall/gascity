package api

import (
	"context"
	"errors"
	"testing"

	"github.com/danielgtaylor/huma/v2/sse"
)

// cancelOnSendError should cancel its context on the first send failure
// and short-circuit subsequent calls so the stream loop exits promptly
// instead of continuing to drain events onto a dead client.
func TestCancelOnSendErrorCancelsContextOnFirstFailure(t *testing.T) {
	sendErr := errors.New("client disconnected")
	var sendCalls int
	raw := sse.Sender(func(msg sse.Message) error {
		sendCalls++
		return sendErr
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wrapped := cancelOnSendError(raw, cancel)

	if err := wrapped(sse.Message{ID: 1, Data: "first"}); !errors.Is(err, sendErr) {
		t.Fatalf("first send err = %v, want sendErr", err)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("context should be canceled after first send failure")
	}

	// Subsequent calls must return the cached error without re-invoking
	// the underlying sender — the stream loop should see the error and
	// exit, and we must not keep writing to a dead pipe.
	if err := wrapped(sse.Message{ID: 2, Data: "second"}); !errors.Is(err, sendErr) {
		t.Fatalf("second send err = %v, want sendErr", err)
	}
	if sendCalls != 1 {
		t.Errorf("underlying sender invoked %d times, want 1 (short-circuit on cached error)", sendCalls)
	}
}

// Happy path: when the underlying sender succeeds, the wrapper must not
// cancel the context or stash an error.
func TestCancelOnSendErrorPassesSuccessThrough(t *testing.T) {
	raw := sse.Sender(func(msg sse.Message) error { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wrapped := cancelOnSendError(raw, cancel)

	for i := 0; i < 3; i++ {
		if err := wrapped(sse.Message{ID: i, Data: "ok"}); err != nil {
			t.Fatalf("send err = %v, want nil", err)
		}
	}
	select {
	case <-ctx.Done():
		t.Fatal("context should not be canceled on successful sends")
	default:
	}
}
