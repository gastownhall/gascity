package extmsg

import "github.com/gastownhall/gascity/internal/beads"

// Services bundles the Phase 1 fabric services built over a shared lock pool.
type Services struct {
	Bindings   BindingService
	Delivery   DeliveryContextService
	Groups     GroupService
	Transcript TranscriptService
}

// NewServices creates binding, delivery, and group services that share the
// same per-fabric binding lock pool.
func NewServices(store beads.Store, opts ...BindingServiceOption) Services {
	locks := sharedBindingLockPool(store)
	transcript := newTranscriptService(store, locks)
	delivery := newDeliveryContextService(store, locks, transcript)
	return Services{
		Bindings:   newBindingService(store, delivery, transcript, locks, opts...),
		Delivery:   delivery,
		Groups:     newGroupService(store, locks, transcript),
		Transcript: transcript,
	}
}
