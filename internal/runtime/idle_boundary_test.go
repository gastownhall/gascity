package runtime

import (
	"errors"
	"fmt"
	"testing"
)

// scriptedIdleSnapshot is a Fake whose idle snapshot answers idle and err,
// counting the snapshots taken. Its pending interactions are the Fake's.
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

// snapshotOnly takes idle snapshots that answer err but reports no pending
// interactions: the embedded Provider hides the Fake's Pending.
type snapshotOnly struct {
	Provider
	err error
}

func (s snapshotOnly) SnapshotIdle(string) (bool, error) { return false, s.err }

// IdleBoundaryNow is one snapshot, classified: an idle reading is at the
// boundary and a busy one is busy. An indeterminate reading, which an open
// dialog gives (neither a ready prompt nor a busy indicator), is pending when
// the runtime reports a pending interaction for the session, and unknown when
// it reports none or its look fails. Only an indeterminate reading looks.
// Every other answer, a runtime that takes no snapshot included, is unknown.
func TestIdleBoundaryNow(t *testing.T) {
	approval := &PendingInteraction{RequestID: "tmux-1", Kind: "approval", Prompt: "Allow Bash?"}
	indeterminate := fmt.Errorf("scan: %w", ErrIdleIndeterminate)
	for _, tc := range []struct {
		name        string
		idle        bool
		err         error
		pending     *PendingInteraction
		pendingErr  error
		want        IdleBoundary
		wantLookups int
	}{
		{name: "idle", idle: true, pending: approval, want: IdleBoundaryAt},
		{name: "busy", pending: approval, want: IdleBoundaryBusy},
		{name: "indeterminate", err: indeterminate, want: IdleBoundaryUnknown, wantLookups: 1},
		{name: "indeterminate with a pending interaction", err: indeterminate, pending: approval, want: IdleBoundaryPending, wantLookups: 1},
		{name: "indeterminate and the pending look failed", err: indeterminate, pendingErr: errors.New("capture-pane failed"), want: IdleBoundaryUnknown, wantLookups: 1},
		{name: "unsupported", err: fmt.Errorf("routed: %w", ErrInteractionUnsupported), pending: approval, want: IdleBoundaryUnknown},
		{name: "capture failed", err: errors.New("capture-pane failed"), pending: approval, want: IdleBoundaryUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := &scriptedIdleSnapshot{Fake: NewFake(), idle: tc.idle, err: tc.err}
			snap.SetPendingInteraction("s", tc.pending)
			if tc.pendingErr != nil {
				snap.PendingErrors["s"] = tc.pendingErr
			}
			if got := IdleBoundaryNow(snap, "s"); got != tc.want {
				t.Errorf("IdleBoundaryNow = %v, want %v", got, tc.want)
			}
			if snap.calls != 1 {
				t.Errorf("snapshots taken = %d, want exactly 1", snap.calls)
			}
			if got := snap.CountCalls("Pending", "s"); got != tc.wantLookups {
				t.Errorf("pending lookups = %d, want %d", got, tc.wantLookups)
			}
		})
	}
	t.Run("indeterminate on a runtime without pending interactions", func(t *testing.T) {
		fake := NewFake()
		fake.SetPendingInteraction("s", approval)
		if got := IdleBoundaryNow(snapshotOnly{Provider: fake, err: indeterminate}, "s"); got != IdleBoundaryUnknown {
			t.Errorf("IdleBoundaryNow = %v, want %v", got, IdleBoundaryUnknown)
		}
	})
	t.Run("no snapshot", func(t *testing.T) {
		fake := NewFake()
		fake.SetPendingInteraction("s", approval)
		if got := IdleBoundaryNow(fake, "s"); got != IdleBoundaryUnknown {
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
