package main

import (
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/reconcilekey"
)

// controllerWake is the single path from every reconcile trigger in the
// controller to the session reconciler: the API, the socket, the config
// watcher, the supervisor reload, the inventory and on_death lanes, the
// provider event pump, workspace services and the tick's own follow-ups.
// Nothing else sends on the wake channels or calls legacyEnqueue
// (TestEveryReconcileEnqueueGoesThroughTheWake).
//
// Under the legacy reconciler every method is exactly the channel fold it
// replaces (legacyEnqueue). Under v2, newControllerWiring sets router before
// the controller socket can deliver anything, and keyed enqueues, bead events
// and bead event gaps go to the v2 router instead. Maintenance wakes (config
// reload) reach pokeCh in both modes: the reload runs on the maintenance tick.
type controllerWake struct {
	pokeCh, controlDispatcherCh chan<- struct{}
	router                      *reconcileRouter
	// now, when set, is the clock routed enqueues rate-limit their landed
	// report by; nil is time.Now, whose readings compare on the monotonic
	// clock, so a wall-clock step neither floods nor silences the report.
	now        func() time.Time
	lastLanded atomic.Pointer[time.Time] // the last routed enqueue reported landed
}

// routedLandedEvery bounds how often a routed enqueue reports that it landed.
const routedLandedEvery = time.Second

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

// initWake installs the city runtime's wake once. An entry point hands in
// its controllerWiring's wake, which is the wake its socket and API state
// already use, so the runtime's follow-ups, lanes and event pump reach the
// same reconciler; a v2 controller always has one (checkReconcilerWiring). A
// directly-built legacy runtime gets a wake over the signals its run loop
// selects on. newCityRuntime calls it; wakeOf never builds one.
func (cr *CityRuntime) initWake(wired *controllerWake) {
	if wired != nil {
		cr.wake = wired
		return
	}
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
// on a landed wake (the provider event pump). Under the router every enqueue
// lands, so it reports landed at most once per routedLandedEvery: a replayed
// backlog burst stays as quiet as the legacy fold's full channel keeps it
// (API-018).
func (w *controllerWake) Enqueue(reason string, keys ...reconcilekey.Key) bool {
	if w == nil {
		return false
	}
	if w.router != nil {
		w.router.Enqueue(reason, keys...)
		return w.reportLanded()
	}
	return legacyEnqueue(w.pokeCh, w.controlDispatcherCh, keys...)
}

func (w *controllerWake) reportLanded() bool {
	now := time.Now
	if w.now != nil {
		now = w.now
	}
	t := now()
	last := w.lastLanded.Load()
	if last != nil && t.Sub(*last) < routedLandedEvery {
		return false
	}
	return w.lastLanded.CompareAndSwap(last, &t)
}

// routes reports whether bead events go to the v2 router, which needs to know
// whether each one landed in the sessions store.
func (w *controllerWake) routes() bool {
	return w != nil && w.router != nil
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
// per echo is the ga-yoix1 churn shape. The v2 router routes replays too,
// enqueue-only (F1), and never pokes the tick; appliedToSessions says the
// event's bead lives in the sessions-class store (routes reports when the
// caller must work it out).
func (w *controllerWake) OnBeadEvent(evt events.Event, snapshot, appliedToSessions bool) {
	if w == nil {
		return
	}
	if w.router != nil {
		w.router.OnBeadEvent(evt, snapshot, appliedToSessions)
		return
	}
	if snapshot {
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
