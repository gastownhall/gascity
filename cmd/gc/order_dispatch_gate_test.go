package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGateOpenWorkBoundedTimesOutSoLaterOrdersDispatch(t *testing.T) {
	// vc-6qh1 #2: a gate that exceeds its bound must return an error so the
	// dispatch loop skips THAT order and continues to the rest (the existing
	// `if err != nil { continue }` is fail-closed-but-continue). Without the
	// bound a wedged gate blocks every later order on the tick.
	started := make(chan struct{})
	_, err := gateOpenWorkBounded(context.Background(), 10*time.Millisecond, "order:heavy", func() (bool, error) {
		close(started)
		time.Sleep(500 * time.Millisecond)
		return false, nil
	})
	if err == nil {
		t.Fatal("gate exceeding the bound must return a timeout error so the order is skipped")
	}
	<-started // the gate did run (and is left to finish on its own).
}

func TestGateOpenWorkBoundedReturnsFastResult(t *testing.T) {
	has, err := gateOpenWorkBounded(context.Background(), time.Second, "order:x", func() (bool, error) {
		return true, nil
	})
	if err != nil {
		t.Fatalf("fast gate must not error: %v", err)
	}
	if !has {
		t.Fatal("fast gate must return the real has-open-work result")
	}
}

func TestGateOpenWorkBoundedHonorsDispatchContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := gateOpenWorkBounded(ctx, time.Second, "order:x", func() (bool, error) {
		time.Sleep(100 * time.Millisecond)
		return false, nil
	})
	if err == nil {
		t.Fatal("a canceled dispatch context must abort the gate (skip + continue)")
	}
}

func TestGateOpenWorkBoundedPropagatesGateError(t *testing.T) {
	wantErr := context.DeadlineExceeded
	_, err := gateOpenWorkBounded(context.Background(), time.Second, "order:x", func() (bool, error) {
		return false, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("gate error must propagate unchanged: got %v, want %v", err, wantErr)
	}
}
