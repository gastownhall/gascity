package worker

import (
	"context"
	"fmt"
	"time"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// LiveObservation is the worker-owned runtime observation surface used by API
// and CLI read models.
type LiveObservation struct {
	Running          bool
	Alive            bool
	Suspended        bool
	Attached         bool
	LastActivity     *time.Time
	RuntimeSessionID string
	SessionID        string
	SessionName      string
}

// ObserveHandle returns worker-owned runtime observations for handles that
// support them.
func ObserveHandle(ctx context.Context, h LiveObservationHandle) (LiveObservation, error) {
	if h == nil {
		return LiveObservation{}, fmt.Errorf("%w: live observation unavailable", ErrOperationUnsupported)
	}
	return h.LiveObservation(ctx)
}

// ObserveSessionInfo reports live runtime state for a session record the caller
// has already loaded. It deliberately does not reread the store; callers own
// the freshness of info and use this path when an authoritative read already
// happened in the same operation.
func (f *Factory) ObserveSessionInfo(ctx context.Context, info sessionpkg.Info, processNames []string) (LiveObservation, error) {
	if ctx == nil {
		return LiveObservation{}, fmt.Errorf("observing loaded session: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return LiveObservation{}, err
	}
	if f == nil || f.manager == nil {
		return LiveObservation{}, fmt.Errorf("%w: manager is required", ErrHandleConfig)
	}
	return liveObservationFromSessionInfo(f.manager, info, processNames), nil
}

func liveObservationFromSessionInfo(manager *sessionpkg.Manager, info sessionpkg.Info, processNames []string) LiveObservation {
	runtimeObs := manager.ObserveRuntimeForInfo(info, processNames)
	obs := LiveObservation{
		Running:          runtimeObs.Running,
		Alive:            runtimeObs.Alive,
		Suspended:        info.State == sessionpkg.StateSuspended,
		Attached:         runtimeObs.Attached,
		RuntimeSessionID: info.ID,
		SessionID:        info.ID,
		SessionName:      runtimeObs.SessionName,
	}
	if !runtimeObs.LastActive.IsZero() {
		last := runtimeObs.LastActive
		obs.LastActivity = &last
	}
	return obs
}

// LiveObservation reports runtime presence and attachment metadata for a
// bead-backed session handle.
func (h *SessionHandle) LiveObservation(_ context.Context) (LiveObservation, error) {
	id := h.currentSessionID()
	if id == "" {
		return LiveObservation{}, nil
	}
	info, err := h.manager.Get(id)
	if err != nil {
		return LiveObservation{}, err
	}
	return liveObservationFromSessionInfo(h.manager, info, h.runtimeHints().ProcessNames), nil
}
