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
	Suspended        bool
	Attached         bool
	LastActivity     *time.Time
	RuntimeSessionID string
	SessionID        string
	SessionName      string
}

// ObserveHandle returns worker-owned runtime observations for handles that
// support them.
func ObserveHandle(ctx context.Context, h Handle) (LiveObservation, error) {
	type observer interface {
		LiveObservation(context.Context) (LiveObservation, error)
	}
	observed, ok := h.(observer)
	if !ok {
		return LiveObservation{}, fmt.Errorf("%w: live observation unavailable", ErrOperationUnsupported)
	}
	return observed.LiveObservation(ctx)
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
	obs := LiveObservation{
		Running:          info.State == sessionpkg.StateActive || info.State == sessionpkg.StateAwake,
		Suspended:        info.State == sessionpkg.StateSuspended,
		Attached:         info.Attached,
		RuntimeSessionID: info.ID,
		SessionID:        info.ID,
		SessionName:      info.SessionName,
	}
	if !info.LastActive.IsZero() {
		last := info.LastActive
		obs.LastActivity = &last
	}
	return obs, nil
}
