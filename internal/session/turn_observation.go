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
//
// Each takes the Info the caller already loaded with Get, which also routes
// the session on a composite runtime, so a read costs no further store load.

// RuntimeTurnTraits is what the runtime serving a session reports about
// observing and replacing its turns. Every field is a fixed trait of the
// routed backend, or of the session's row on it, read without calling the
// runtime, so composing them costs no runtime call and gives the same answer
// for as long as the session's route stands.
type RuntimeTurnTraits struct {
	// Capabilities is what the runtime itself can tell about the session's
	// turns, normalized as runtime.TurnCapabilitiesFor reports it.
	Capabilities runtime.TurnCapabilities
	// Interrupt is how the runtime interrupts the session's turn
	// (runtime.TurnInterruptModeFor).
	Interrupt runtime.TurnInterruptMode
	// IdleSnapshots reports that the routed backend takes idle snapshots
	// (runtime.IdleSnapshotsFor).
	IdleSnapshots bool
	// LocalTranscripts reports that the routed backend runs the agent on
	// this host, so its transcript can be read here
	// (runtime.LocalTranscriptsFor).
	LocalTranscripts bool
	// Replace is what interrupt_now does to the session's running turn on
	// that backend, decided as interrupt_now decides it: the session's own
	// family and transport with the backend's interrupt mode and reset.
	Replace TurnReplacement
}

// RuntimeTurnTraits reports the turn-observation traits of the runtime
// serving the session info describes.
func (m *Manager) RuntimeTurnTraits(info Info) RuntimeTurnTraits {
	name := info.SessionName
	mode := runtime.TurnInterruptModeFor(m.sp, name)
	return RuntimeTurnTraits{
		Capabilities:     runtime.TurnCapabilitiesFor(m.sp, name),
		Interrupt:        mode,
		IdleSnapshots:    runtime.IdleSnapshotsFor(m.sp, name),
		LocalTranscripts: runtime.LocalTranscriptsFor(m.sp, name),
		Replace:          turnReplacementFor(info, mode, runtime.InterruptedTurnResetsFor(m.sp, name)),
	}
}

// RuntimeTurnHistory reads the turns of the session info describes from its
// runtime now. It returns an error wrapping runtime.ErrTurnIdentityUnsupported
// when the runtime does not observe turns, including a composite whose backend
// for the session does not (runtime.ErrInteractionUnsupported). Any other
// failed read is the runtime's error, with the zero history: it never reports
// idle, nor a partial history, instead.
func (m *Manager) RuntimeTurnHistory(ctx context.Context, info Info) (runtime.TurnHistory, error) {
	tp, ok := m.sp.(runtime.TurnObservationProvider)
	if !ok {
		return runtime.TurnHistory{}, fmt.Errorf("turns of session %s: %w", info.ID, runtime.ErrTurnIdentityUnsupported)
	}
	h, err := tp.TurnHistory(ctx, info.SessionName)
	switch {
	case errors.Is(err, runtime.ErrInteractionUnsupported):
		return runtime.TurnHistory{}, fmt.Errorf("turns of session %s: %w: %w", info.ID, runtime.ErrTurnIdentityUnsupported, err)
	case err != nil:
		return runtime.TurnHistory{}, err
	}
	return h, nil
}

// RuntimeIdleSnapshot takes one idle observation of the session info
// describes from its runtime (runtime.IdleSnapshotProvider). supported is
// false, with a nil error, when the runtime cannot take one for the session:
// it does not implement the snapshot, or it is a composite whose backend for
// the session does not. Otherwise idle and err are the runtime's answer, and
// an error is never idle.
func (m *Manager) RuntimeIdleSnapshot(info Info) (idle, supported bool, err error) {
	sp, ok := m.sp.(runtime.IdleSnapshotProvider)
	if !ok {
		return false, false, nil
	}
	idle, err = sp.SnapshotIdle(info.SessionName)
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
