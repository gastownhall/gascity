package worker

import (
	"context"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// ProviderAvailabilityHandle is an optional live diagnostic read. The ordinary
// LiveObservation and control paths intentionally do not perform this probe.
type ProviderAvailabilityHandle interface {
	ProviderAvailability(context.Context, string) *runtime.ProviderAvailability
}

// ObserveHandleWithAvailability opts a read model into a bounded availability
// observation. Existing liveness/error semantics remain independent. No result
// (including nil/unknown) authorizes input, a restart, or a ready claim.
func ObserveHandleWithAvailability(ctx context.Context, h LiveObservationHandle) (LiveObservation, error) {
	obs, err := ObserveHandle(ctx, h)
	if err != nil || !obs.Running || obs.Suspended {
		return obs, err
	}
	if availability, ok := h.(ProviderAvailabilityHandle); ok {
		obs.ProviderAvailability = availability.ProviderAvailability(ctx, obs.SessionID)
	}
	return obs, nil
}

// ProviderAvailability reads the exact session anew before asking its runtime
// adapter. Profile/recorded family identity wins over user-selected aliases.
func (h *SessionHandle) ProviderAvailability(ctx context.Context, expectedSessionID string) *runtime.ProviderAvailability {
	id := h.currentSessionID()
	if expectedSessionID == "" || id != expectedSessionID {
		return unknownProviderAvailability()
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	key := "worker.provider-availability\x00" + h.turnObservationScope() + "\x00" + id
	observed, err := ObserveShared(ctx, key, func(ctx context.Context) (*runtime.ProviderAvailability, error) {
		info, err := h.manager.Get(id)
		if err != nil {
			return nil, err
		}
		family := profileFamily(h.session.Profile)
		if family == "" {
			family = sessionpkg.ProviderFamilyFromInfo(info, h.session.Provider)
		}
		observed := h.manager.ObserveProviderAvailability(ctx, info, family)
		if h.currentSessionID() != expectedSessionID {
			return unknownProviderAvailability(), nil
		}
		return observed, nil
	})
	if err != nil || h.currentSessionID() != expectedSessionID {
		return unknownProviderAvailability()
	}
	return observed
}

func unknownProviderAvailability() *runtime.ProviderAvailability {
	return &runtime.ProviderAvailability{State: "unknown", Source: "native_ui", ObservedAt: time.Now().UTC()}
}
