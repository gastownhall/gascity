package main

import "github.com/gastownhall/gascity/internal/reconcilekey"

// controllerWake is the single path from every reconcile trigger in the
// controller to the session reconciler: the API, the socket, the config
// watcher, the supervisor reload, the inventory and on_death lanes, the
// provider event pump, workspace services and the tick's own follow-ups.
// Nothing else sends on the wake channels or calls legacyEnqueue
// (TestEveryReconcileEnqueueGoesThroughTheWake).
//
// Under the legacy reconciler every method is exactly the channel fold it
// replaces (legacyEnqueue). router is the keyed reconciler's seam: when set,
// keyed enqueues and bead event gaps go to the v2 router instead. Maintenance
// wakes (config reload) still reach pokeCh, and so does OnBeadEvent until
// P2-8 widens it to carry what the router needs. Nothing sets router before
// the switch is wired (P2-8).
type controllerWake struct {
	pokeCh, controlDispatcherCh chan<- struct{}
	router                      *reconcileRouter
}

// newLegacyWake returns the legacy wake over the reconciler's two signals.
// Either may be nil; a nil channel is never signaled.
func newLegacyWake(pokeCh, controlDispatcherCh chan<- struct{}) *controllerWake {
	return &controllerWake{pokeCh: pokeCh, controlDispatcherCh: controlDispatcherCh}
}

// wakeOf returns the wired controller wake, or a legacy wake over cs's own
// signals when none is wired, so a directly-constructed controllerState needs
// no wiring.
func (cs *controllerState) wakeOf() *controllerWake {
	if cs.wake != nil {
		return cs.wake
	}
	return newLegacyWake(cs.pokeCh, cs.controlDispatcherCh)
}

// initWake builds the city runtime's wake once, over the signals its run loop
// selects on: the controllerWiring's signals when an entry point built the
// runtime. newCityRuntime calls it; wakeOf never builds one.
func (cr *CityRuntime) initWake() {
	cr.wake = newLegacyWake(cr.pokeCh, cr.controlDispatcherCh)
}

// wakeOf returns the city runtime's wake (initWake).
func (cr *CityRuntime) wakeOf() *controllerWake {
	return cr.wake
}

// Reasons a trigger gives Enqueue. The legacy fold ignores them; the v2
// router merges them into the item's reasons, and the operator intents
// (api, socket) and the supervisor reload bypass the backoff gate.
const (
	wakeReasonAPI           = "api"
	wakeReasonSocket        = "socket"
	wakeReasonTrace         = "trace"
	wakeReasonService       = "service"
	wakeReasonLaneGone      = "lane-gone"
	wakeReasonOnDeath       = "on-death"
	wakeReasonProviderEvent = "provider-event"
	wakeReasonFollowUp      = "follow-up"
)

// Enqueue asks for keys to be reconciled promptly. No keys means the
// allocator. It reports whether a signal landed, for callers that log only
// on a landed wake; under the router every enqueue lands.
func (w *controllerWake) Enqueue(reason string, keys ...reconcilekey.Key) bool {
	if w == nil {
		return false
	}
	if w.router != nil {
		w.router.Enqueue(reason, keys...)
		return true
	}
	return legacyEnqueue(w.pokeCh, w.controlDispatcherCh, keys...)
}

// WakeMaintenance asks for a maintenance pass after a config change. The
// caller has already set the dirty flag. It is the generic poke in both
// modes: the reload runs on the maintenance tick.
func (w *controllerWake) WakeMaintenance() {
	if w == nil {
		return
	}
	legacyEnqueue(w.pokeCh, nil)
}

// OnBeadEvent wakes the reconciler for one bead event after the caches
// applied it. A cache-reconcile replay (snapshot) never wakes the legacy
// reconciler: the controller's own writes echo back as replays, and a poke
// per echo is the ga-yoix1 churn shape. Routing bead events to the v2 router
// needs the event and whether it landed in the sessions store; the switch
// adds both when it wires the router.
func (w *controllerWake) OnBeadEvent(snapshot bool) {
	if w == nil || snapshot {
		return
	}
	legacyEnqueue(w.pokeCh, w.controlDispatcherCh, reconcilekey.Allocator())
}

// OnEventGap reports that the bead event tail broke or regressed, so events
// may be missing. The legacy reconciler re-reads every store each tick and
// needs nothing; the v2 router rebuilds its indexes.
func (w *controllerWake) OnEventGap() {
	if w == nil || w.router == nil {
		return
	}
	w.router.OnEventGap()
}
