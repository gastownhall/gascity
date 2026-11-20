package runtime

import (
	"context"
	"time"
)

// ProviderAvailability is a bounded live adapter observation, independent of
// process liveness, native turn state and the program's own OSC status. Absence
// is unsupported/no evidence, never ready. Unknown means a supported observation
// could not establish whether its current native failure widget is present.
type ProviderAvailability struct {
	State             string    `json:"state" enum:"unknown,unavailable"`
	Code              string    `json:"code,omitempty"`
	Source            string    `json:"source"`
	ObservedAt        time.Time `json:"observed_at"`
	RuntimeGeneration string    `json:"runtime_generation,omitempty"`
}

// ProviderAvailabilityRequest binds a read to the resolved provider family and
// the exact runtime incarnation already named by its session row. Fence's raw
// token never appears on the observation; only its existing public Digest does.
type ProviderAvailabilityRequest struct {
	Family string
	Fence  InputFence
}

// ProviderAvailabilityObserver is an optional read-only adapter extension.
// Implementations return nil for unsupported families and unknown on an
// indeterminate supported read. They must never send input or mutate lifecycle.
type ProviderAvailabilityObserver interface {
	ObserveProviderAvailability(context.Context, string, ProviderAvailabilityRequest) *ProviderAvailability
}

// ObserveProviderAvailability reads an optional adapter observation. A runtime
// without the extension remains unsupported, with no probe or ready inference.
func ObserveProviderAvailability(ctx context.Context, provider Provider, name string, req ProviderAvailabilityRequest) *ProviderAvailability {
	if observer, ok := provider.(ProviderAvailabilityObserver); ok {
		return observer.ObserveProviderAvailability(ctx, name, req)
	}
	return nil
}
