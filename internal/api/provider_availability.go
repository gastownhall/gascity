package api

import (
	"context"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// SessionProviderAvailability is a current native-UI diagnostic. It says
// nothing about process liveness, transcript activity or provider readiness.
type SessionProviderAvailability struct {
	State             string    `json:"state" enum:"unknown,unavailable" doc:"A current native UI failure diagnostic or an indeterminate observation. Never a readiness or liveness report."`
	Code              string    `json:"code,omitempty" enum:"connection_restore_failed"`
	Source            string    `json:"source" enum:"native_ui"`
	ObservedAt        time.Time `json:"observed_at"`
	RuntimeGeneration string    `json:"runtime_generation,omitempty" doc:"Opaque current runtime incarnation identity; clients must compare it as a whole, never parse it."`
}

// SessionProviderAvailabilityEvent carries the current view for exactly one
// session. A null diagnostic clears the previous view. These live snapshots
// have no durable event cursor and are not transcript or history records.
type SessionProviderAvailabilityEvent struct {
	ID                   string                       `json:"id"`
	ProviderAvailability *SessionProviderAvailability `json:"provider_availability"`
}

func providerAvailabilityView(observation *runtime.ProviderAvailability) *SessionProviderAvailability {
	if observation == nil {
		return nil
	}
	return &SessionProviderAvailability{
		State: observation.State, Code: observation.Code, Source: observation.Source,
		ObservedAt: observation.ObservedAt, RuntimeGeneration: observation.RuntimeGeneration,
	}
}

// sessionProviderAvailabilityForRecord consumes the already-resolved typed
// session record and manager. A diagnostic never opens a new raw-store edge or
// re-resolves the target; the existing worker factory preserves its persisted
// profile and the manager's runtime/transport policy.
func sessionProviderAvailabilityForRecord(ctx context.Context, manager *session.Manager, info session.Info, record session.PersistedResponse, searchPaths []string) *SessionProviderAvailability {
	factory, err := worker.NewFactoryFromManager(manager, searchPaths)
	if err != nil {
		return nil
	}
	handle, err := factory.SessionByRecord(info, record)
	if err != nil {
		return nil
	}
	return sessionProviderAvailability(ctx, info.ID, handle)
}

func sessionProviderAvailability(ctx context.Context, id string, handle any) *SessionProviderAvailability {
	observer, ok := handle.(worker.LiveObservationHandle)
	if !ok {
		return nil
	}
	observation, err := worker.ObserveHandleWithAvailability(ctx, observer)
	if err != nil || observation.SessionID != id {
		return nil
	}
	return providerAvailabilityView(observation.ProviderAvailability)
}

type providerAvailabilityFrames struct {
	initialized bool
	id          string
	read        func() *SessionProviderAvailability
	last        *SessionProviderAvailability
}

func newProviderAvailabilityFrames(ctx context.Context, info session.Info, handle any) *providerAvailabilityFrames {
	return &providerAvailabilityFrames{id: info.ID, read: func() *SessionProviderAvailability {
		return sessionProviderAvailability(ctx, info.ID, handle)
	}}
}

func (frames *providerAvailabilityFrames) next() (SessionProviderAvailabilityEvent, bool) {
	view := frames.read()
	if frames.initialized && sameProviderAvailability(frames.last, view) {
		return SessionProviderAvailabilityEvent{}, false
	}
	frames.initialized = true
	if view == nil {
		frames.last = nil
	} else {
		copyView := *view
		frames.last = &copyView
	}
	return SessionProviderAvailabilityEvent{ID: frames.id, ProviderAvailability: view}, true
}

func sameProviderAvailability(a, b *SessionProviderAvailability) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.State == b.State && a.Code == b.Code && a.Source == b.Source && a.RuntimeGeneration == b.RuntimeGeneration
}
