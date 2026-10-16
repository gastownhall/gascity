package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Turn observation pass-throughs (X11 TI-2b). These read what the session's
// routed runtime reports about its turns, so the worker boundary can compose a
// session's turn capabilities without reaching around the Manager to its
// provider. They change no session state and send nothing to the session.
// Format and provider knowledge stays out of this package (X11 I9): the
// runtime answers for itself, and transcripts are read in internal/sessionlog.

// RuntimeTurnCapabilities reports what the runtime serving session id can tell
// about its turns, normalized as runtime.TurnCapabilitiesFor reports it, and
// how it interrupts the session's turn (runtime.TurnInterruptModeFor).
func (m *Manager) RuntimeTurnCapabilities(id string) (runtime.TurnCapabilities, runtime.TurnInterruptMode, error) {
	_, sessName, err := m.sessionBead(id)
	if err != nil {
		return runtime.TurnCapabilities{}, "", err
	}
	return runtime.TurnCapabilitiesFor(m.sp, sessName), runtime.TurnInterruptModeFor(m.sp, sessName), nil
}

// RuntimeTurnHistory reads session id's turns from its runtime now. It returns
// an error wrapping runtime.ErrTurnIdentityUnsupported when the runtime does
// not observe turns, including a composite whose backend for the session
// does not (runtime.ErrInteractionUnsupported). Any other failed read is the
// runtime's error; it never reports idle instead.
func (m *Manager) RuntimeTurnHistory(ctx context.Context, id string) (runtime.TurnHistory, error) {
	_, sessName, err := m.sessionBead(id)
	if err != nil {
		return runtime.TurnHistory{}, err
	}
	tp, ok := m.sp.(runtime.TurnObservationProvider)
	if !ok {
		return runtime.TurnHistory{}, fmt.Errorf("turns of session %s: %w", id, runtime.ErrTurnIdentityUnsupported)
	}
	h, err := tp.TurnHistory(ctx, sessName)
	if errors.Is(err, runtime.ErrInteractionUnsupported) {
		return runtime.TurnHistory{}, fmt.Errorf("turns of session %s: %w: %w", id, runtime.ErrTurnIdentityUnsupported, err)
	}
	return h, err
}

// RuntimeIdleSnapshot takes one idle observation of session id from its
// runtime (runtime.IdleSnapshotProvider). supported is false, with a nil
// error, when the runtime cannot take one for the session: it does not
// implement the snapshot, or it is a composite whose backend for the session
// does not. Otherwise idle and err are the runtime's answer, and an error is
// never idle.
func (m *Manager) RuntimeIdleSnapshot(id string) (idle, supported bool, err error) {
	_, sessName, err := m.sessionBead(id)
	if err != nil {
		return false, false, err
	}
	sp, ok := m.sp.(runtime.IdleSnapshotProvider)
	if !ok {
		return false, false, nil
	}
	idle, err = sp.SnapshotIdle(sessName)
	if errors.Is(err, runtime.ErrInteractionUnsupported) {
		return false, false, nil
	}
	if err != nil {
		return false, true, err
	}
	return idle, true, nil
}

// SubscribeRuntimeTurnEvents subscribes to the turn events of every session
// the Manager's runtime serves (runtime.TurnEventProvider). Callers filter by
// session name. It returns runtime.ErrInteractionUnsupported when the runtime
// reports no turn events.
func (m *Manager) SubscribeRuntimeTurnEvents(ctx context.Context) (<-chan runtime.TurnEvent, error) {
	tp, ok := m.sp.(runtime.TurnEventProvider)
	if !ok {
		return nil, runtime.ErrInteractionUnsupported
	}
	return tp.SubscribeTurnEvents(ctx)
}
