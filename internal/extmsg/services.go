package extmsg

import (
	"errors"
	"reflect"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

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
	return newServicesWithSessionDirectory(store, session.NewStore(beads.SessionStore{Store: store}), opts...)
}

// NewServicesWithSessionDirectory creates the split Messaging/Sessions form:
// bindings, groups, participants, delivery contexts, memberships, and
// transcripts persist only in messaging, while identity and liveness reads use
// the typed Sessions directory. It rejects a nil (including typed-nil)
// directory rather than silently falling back to the wrong persistence class.
func NewServicesWithSessionDirectory(messaging beads.Store, sessions session.AddressDirectory, opts ...BindingServiceOption) (Services, error) {
	if nilAddressDirectory(sessions) {
		return Services{}, errors.New("extmsg services: session address directory is required")
	}
	return newServicesWithSessionDirectory(messaging, sessions, opts...), nil
}

func newServicesWithSessionDirectory(store beads.Store, sessions session.AddressDirectory, opts ...BindingServiceOption) Services {
	locks := sharedBindingLockPool(store)
	transcript := newTranscriptService(store, locks)
	delivery := newDeliveryContextService(store, locks, transcript)
	return Services{
		Bindings:   newBindingServiceWithSessionDirectory(store, sessions, delivery, transcript, locks, opts...),
		Delivery:   delivery,
		Groups:     newGroupServiceWithSessionDirectory(store, sessions, locks, transcript),
		Transcript: transcript,
	}
}

func nilAddressDirectory(directory session.AddressDirectory) bool {
	if directory == nil {
		return true
	}
	value := reflect.ValueOf(directory)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
