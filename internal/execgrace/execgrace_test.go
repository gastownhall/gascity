package execgrace

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// TestMonitorDisabledPassthrough proves the opt-out contract: with neither
// dimension enabled the Monitor must not wrap the context or the writers, so
// existing call sites keep their exact behavior.
func TestMonitorDisabledPassthrough(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := NewMonitor(ctx, 0, 0)
	defer m.Stop()
	if m.Context() != ctx {
		t.Fatal("disabled Monitor must return the parent context unchanged")
	}
	if m.Enabled() {
		t.Fatal("disabled Monitor must report Enabled() == false")
	}
	var buf bytes.Buffer
	if w := m.Writer(&buf); w != io.Writer(&buf) {
		t.Fatal("disabled Monitor must return the inner writer unchanged")
	}
}

// TestMonitorIdleTimeout proves silence cancels with ErrIdle.
func TestMonitorIdleTimeout(t *testing.T) {
	t.Parallel()
	m := NewMonitor(context.Background(), 100*time.Millisecond, 0)
	defer m.Stop()
	select {
	case <-m.Context().Done():
		if cause := context.Cause(m.Context()); !errors.Is(cause, ErrIdle) {
			t.Fatalf("cause = %v, want ErrIdle", cause)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle timeout never fired")
	}
}

// TestMonitorOutputResetsIdle proves output keeps the context alive past the
// idle window (the slow-but-healthy case the fixed deadline killed), and that
// silence afterwards still cancels.
func TestMonitorOutputResetsIdle(t *testing.T) {
	t.Parallel()
	m := NewMonitor(context.Background(), 200*time.Millisecond, 0)
	defer m.Stop()
	w := m.Writer(io.Discard)

	// Write every 50ms for 3x the idle window.
	deadline := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := w.Write([]byte("progress\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
		select {
		case <-m.Context().Done():
			t.Fatalf("context canceled while output was flowing: %v", context.Cause(m.Context()))
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Now go silent; the idle window must fire.
	select {
	case <-m.Context().Done():
		if cause := context.Cause(m.Context()); !errors.Is(cause, ErrIdle) {
			t.Fatalf("cause = %v, want ErrIdle", cause)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle timeout never fired after output stopped")
	}
}

// TestMonitorCeiling proves the runaway backstop: continuous output does not
// save a command from the absolute ceiling.
func TestMonitorCeiling(t *testing.T) {
	t.Parallel()
	m := NewMonitor(context.Background(), 100*time.Millisecond, 400*time.Millisecond)
	defer m.Stop()
	w := m.Writer(io.Discard)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-m.Context().Done():
				return
			case <-time.After(30 * time.Millisecond):
				w.Write([]byte("spinning\n")) //nolint:errcheck
			}
		}
	}()

	select {
	case <-m.Context().Done():
		if cause := context.Cause(m.Context()); !errors.Is(cause, ErrCeiling) {
			t.Fatalf("cause = %v, want ErrCeiling", cause)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ceiling never fired despite continuous output")
	}
	<-done
}

// TestMonitorStopBeforeFire proves Stop releases the timers without canceling
// a healthy context's successor uses.
func TestMonitorStopBeforeFire(t *testing.T) {
	t.Parallel()
	m := NewMonitor(context.Background(), time.Hour, time.Hour)
	if !m.Enabled() {
		t.Fatal("Monitor with both dimensions must be enabled")
	}
	m.Stop()
	m.Stop() // idempotent
}
