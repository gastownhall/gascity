package beads

import (
	"errors"
	"strings"
)

// CloseTransition describes one attempt to atomically close a live bead with
// a reason. Before is the operation's pre-close observation; After is an
// authoritative post-attempt snapshot. Backends that close through an external
// process do not guarantee that both snapshots came from one transaction.
// A backend may return an authoritative transition together
// with a non-nil error when the close durably committed but an ancillary hook,
// transport, or acknowledgement failed; callers must inspect the transition
// before treating every error as an uncommitted write. ObserverNotified is true
// when a decorating store already published the corresponding close
// notification. ObserverDelivery is non-nil
// when a decorating store owns that notification; callers can use it to
// sequence follow-up lifecycle records after observer publication without
// blocking when the notification is queued behind a reentrant callback.
type CloseTransition struct {
	Before           Bead
	After            Bead
	Transitioned     bool
	ObserverNotified bool
	ObserverDelivery CloseObserverDelivery
}

// AuthoritativeClosed reports whether the transition contains an authoritative
// durable closed snapshot for id. It remains meaningful when the operation also
// returned an error after the close committed.
func (t CloseTransition) AuthoritativeClosed(id string) bool {
	return id != "" && t.After.ID == id && t.After.Status == "closed"
}

// CloseObserverDelivery sequences follow-up work after the exact bead.closed
// observer notification owned by a CloseTransition. AfterDelivery never waits:
// it retains fn while delivery is pending, or invokes fn immediately when the
// observer callback has already returned. Implementations invoke fn once and
// outside their notification-queue locks.
type CloseObserverDelivery interface {
	AfterDelivery(fn func())
}

// CloseTransitioner atomically closes a live bead and persists its close
// reason in the same mutation. Repeated or racing calls preserve the first
// successful close reason and return the durable winning snapshot.
type CloseTransitioner interface {
	CloseWithReasonIfOpen(id, reason string) (CloseTransition, error)
}

// CloseObserverSuppressor preserves close state semantics while suppressing a
// decorating store's bead.closed observer callback for one call. Backing-store
// hooks are outside this contract. The per-call API avoids a shared mute flag
// that could drop unrelated concurrent notifications.
type CloseObserverSuppressor interface {
	CloseWithoutObserver(id string) error
	CloseAllWithoutObserver(ids []string, metadata map[string]string) (int, error)
}

// SequencedCloseObserverSuppressor extends per-call observer suppression with
// a receipt for the suppressed close's place in the same-ID notification
// order. The receipt completes after every observer notification reserved
// before the close, without publishing a notification for the close itself.
// Callers that own fallback publication can use it to avoid overtaking an
// earlier queued snapshot. A successful close always returns a non-nil receipt;
// implementations reserve it while still holding their per-ID mutation lock.
type SequencedCloseObserverSuppressor interface {
	CloseObserverSuppressor
	CloseWithoutObserverWithDelivery(id string) (CloseObserverDelivery, error)
}

// ObserverBarrier reserves a non-mutating position in a store decorator's
// same-bead observer queue. The returned receipt completes after every
// notification reserved before the barrier. Recovery code uses this to publish
// a lifecycle record from an authoritative live snapshot without letting an
// older queued snapshot overtake it.
type ObserverBarrier interface {
	BeadObserverBarrier(id string) CloseObserverDelivery
}

// ObserverBarrierHandleProvider exposes an observer barrier through a wrapper
// that cannot promote the optional capability directly.
type ObserverBarrierHandleProvider interface {
	ObserverBarrierHandle() (ObserverBarrier, bool)
}

// ObserverBarrierFor resolves non-mutating observer sequencing through known
// wrapper layers.
func ObserverBarrierFor(store Store) (ObserverBarrier, bool) {
	if store == nil {
		return nil, false
	}
	if barrier, ok := store.(ObserverBarrier); ok {
		return barrier, true
	}
	if provider, ok := store.(ObserverBarrierHandleProvider); ok {
		return provider.ObserverBarrierHandle()
	}
	return nil, false
}

// CloseObserverSuppressorHandleProvider exposes observer suppression through a
// wrapper that cannot promote the optional capability directly.
type CloseObserverSuppressorHandleProvider interface {
	CloseObserverSuppressorHandle() (CloseObserverSuppressor, bool)
}

// CloseObserverSuppressorFor resolves per-call observer suppression through
// known wrapper layers.
func CloseObserverSuppressorFor(store Store) (CloseObserverSuppressor, bool) {
	if store == nil {
		return nil, false
	}
	if suppressor, ok := store.(CloseObserverSuppressor); ok {
		return suppressor, true
	}
	if provider, ok := store.(CloseObserverSuppressorHandleProvider); ok {
		return provider.CloseObserverSuppressorHandle()
	}
	return nil, false
}

// CloseTransitionerHandleProvider exposes a close-transition handle for
// wrappers that cannot promote the optional capability directly.
type CloseTransitionerHandleProvider interface {
	CloseTransitionerHandle() (CloseTransitioner, bool)
}

// ErrCloseTransitionUnsupported reports that a store cannot provide the
// atomic close-with-reason contract.
var ErrCloseTransitionUnsupported = errors.New("atomic close with reason is unsupported")

// CloseTransitionerFor returns the atomic close-with-reason capability for
// store when one is available.
func CloseTransitionerFor(store Store) (CloseTransitioner, bool) {
	if store == nil {
		return nil, false
	}
	if transitioner, ok := store.(CloseTransitioner); ok {
		return transitioner, true
	}
	if provider, ok := store.(CloseTransitionerHandleProvider); ok {
		return provider.CloseTransitionerHandle()
	}
	return nil, false
}

// closeReasonForTransition normalizes an explicit reason and preserves a
// reason that was already stamped in metadata when an ordinary Close reaches
// the transition API with an empty reason.
func closeReasonForTransition(before Bead, reason string) string {
	if reason = strings.TrimSpace(reason); reason != "" {
		return reason
	}
	return strings.TrimSpace(before.Metadata["close_reason"])
}
