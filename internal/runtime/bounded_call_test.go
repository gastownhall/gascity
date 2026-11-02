package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// callBound is the bound these tests hand CallBounded. They run on synctest's
// fake clock, so waiting it out costs no wall time.
const callBound = 10 * time.Second

// The bounded call is how a caller waits on a provider call that cannot be
// canceled (a hung tmux subprocess, a wedged socket) without waiting forever:
// the wait is raced against the caller's context and abandoned when the
// context ends. These tests pin the runtime-facing form; internal/worker's
// ObserveBounded forwards to it and carries its own suite.

// A call that never answers must end at the context's bound with an error
// wrapping both ErrRuntimeUnavailable and the context's own error, and no
// answer may be invented for it.
func TestCallBoundedWedgedCallReturnsUnavailableAtTheBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		ctx, cancel := context.WithTimeout(context.Background(), callBound)
		defer cancel()
		start := time.Now()

		var runs atomic.Int64
		_, err := CallBounded(ctx, t.Name(), func(context.Context) (int, error) {
			runs.Add(1)
			<-release
			return 1, nil
		})

		if !errors.Is(err, ErrRuntimeUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want it to wrap ErrRuntimeUnavailable and context.DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed > callBound {
			t.Fatalf("returned after %v, want within the bound", elapsed)
		}
		if got := runs.Load(); got != 1 {
			t.Fatalf("call ran %d times, want the one abandoned attempt", got)
		}
	})
}

// A blank key is a caller bug, not an unavailable runtime: it is rejected
// without starting the call, and the rejection must not wrap
// ErrRuntimeUnavailable, which callers read as "no answer".
func TestCallBoundedRejectsBlankKey(t *testing.T) {
	for _, key := range []string{"", " \t"} {
		var runs atomic.Int64
		_, err := CallBounded(context.Background(), key, func(context.Context) (int, error) {
			runs.Add(1)
			return 1, nil
		})
		if err == nil {
			t.Fatalf("key %q: err = nil, want a rejection: a blank key cannot be bounded per session", key)
		}
		if errors.Is(err, ErrRuntimeUnavailable) {
			t.Fatalf("key %q: err = %v, a blank key is a caller bug, not an unavailable runtime", key, err)
		}
		if got := runs.Load(); got != 0 {
			t.Fatalf("key %q: call ran %d times, want 0", key, got)
		}
	}
}

// An already-ended context starts nothing and answers unavailable with the
// context's own error.
func TestCallBoundedAlreadyEndedContextStartsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var runs atomic.Int64

	_, err := CallBounded(ctx, t.Name(), func(context.Context) (int, error) {
		runs.Add(1)
		return 1, nil
	})

	if !errors.Is(err, ErrRuntimeUnavailable) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap ErrRuntimeUnavailable and context.Canceled", err)
	}
	if got := runs.Load(); got != 0 {
		t.Fatalf("call ran %d times under an already-canceled context, want 0", got)
	}
}

// A prompt answer is passed through as-is, including its error.
func TestCallBoundedReturnsPromptAnswer(t *testing.T) {
	providerErr := errors.New("provider said no")

	got, err := CallBounded(context.Background(), t.Name(), func(context.Context) (int, error) { return 42, providerErr })

	if got != 42 || !errors.Is(err, providerErr) {
		t.Fatalf("CallBounded = (%d, %v), want (42, %v)", got, err, providerErr)
	}
	if errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("a provider error that arrived in time was relabeled as unavailable: %v", err)
	}
}
