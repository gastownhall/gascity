package session

import (
	"context"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ObserveProviderAvailability observes only the incarnation this session Info
// names. It neither starts a runtime nor changes the row/native turn. A legacy
// row without an input fence is indeterminate for a supported adapter.
func (m *Manager) ObserveProviderAvailability(ctx context.Context, info Info, family string) *runtime.ProviderAvailability {
	if info.Closed || info.State == StateSuspended {
		return nil
	}
	fence, _ := InputFenceFor(info)
	return runtime.ObserveProviderAvailability(ctx, m.sp, info.SessionName, runtime.ProviderAvailabilityRequest{Family: family, Fence: fence})
}
