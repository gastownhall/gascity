package runtime

import (
	"errors"
	"fmt"
	"testing"
)

// scriptedIdleSnapshot is a Fake whose idle snapshot answers idle and err,
// counting the snapshots taken.
type scriptedIdleSnapshot struct {
	*Fake
	idle  bool
	err   error
	calls int
}

func (s *scriptedIdleSnapshot) SnapshotIdle(string) (bool, error) {
	s.calls++
	return s.idle, s.err
}

// IdleBoundaryNow is one snapshot, classified: an idle reading is at the
// boundary, a busy one is busy, an error wrapping ErrPendingInteraction is
// pending (even when it also wraps ErrIdleIndeterminate), and every other
// answer, a runtime that takes no snapshot included, is unknown.
func TestIdleBoundaryNow(t *testing.T) {
	for _, tc := range []struct {
		name string
		idle bool
		err  error
		want IdleBoundary
	}{
		{name: "idle", idle: true, want: IdleBoundaryAt},
		{name: "busy", want: IdleBoundaryBusy},
		{name: "indeterminate", err: fmt.Errorf("scan: %w", ErrIdleIndeterminate), want: IdleBoundaryUnknown},
		{name: "pending", err: fmt.Errorf("dialog: %w", ErrPendingInteraction), want: IdleBoundaryPending},
		{name: "pending and indeterminate", err: fmt.Errorf("dialog: %w: %w", ErrIdleIndeterminate, ErrPendingInteraction), want: IdleBoundaryPending},
		{name: "unsupported", err: fmt.Errorf("routed: %w", ErrInteractionUnsupported), want: IdleBoundaryUnknown},
		{name: "capture failed", err: errors.New("capture-pane failed"), want: IdleBoundaryUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := &scriptedIdleSnapshot{Fake: NewFake(), idle: tc.idle, err: tc.err}
			if got := IdleBoundaryNow(snap, "s"); got != tc.want {
				t.Errorf("IdleBoundaryNow = %v, want %v", got, tc.want)
			}
			if snap.calls != 1 {
				t.Errorf("snapshots taken = %d, want exactly 1", snap.calls)
			}
		})
	}
	t.Run("no snapshot", func(t *testing.T) {
		if got := IdleBoundaryNow(NewFake(), "s"); got != IdleBoundaryUnknown {
			t.Errorf("IdleBoundaryNow = %v, want %v for a runtime that takes no snapshot", got, IdleBoundaryUnknown)
		}
	})
}

// Only a busy or pending reading rules out a boundary within a short wait.
func TestIdleBoundaryOutOfReach(t *testing.T) {
	for b, want := range map[IdleBoundary]bool{
		IdleBoundaryUnknown: false,
		IdleBoundaryAt:      false,
		IdleBoundaryBusy:    true,
		IdleBoundaryPending: true,
	} {
		if got := b.OutOfReach(); got != want {
			t.Errorf("%v.OutOfReach() = %t, want %t", b, got, want)
		}
	}
}
