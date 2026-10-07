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
// replaces (legacyEnqueue). Under v2, newControllerWiring sets planner before
// the controller socket can deliver anything, and enqueues, relevant bead
// events and bead event gaps mark it dirty instead. Maintenance wakes (config
// reload) reach pokeCh in both modes: the reload runs on the maintenance tick.
type controllerWake struct {
	pokeCh, controlDispatcherCh chan<- struct{}
	planner                     *planner
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

// Reasons a trigger gives Enqueue. The legacy fold ignores them; the planner
// counts its wakes by them.
const (
	wakeReasonAPI           = "api"
	wakeReasonSocket        = "socket"
	wakeReasonTrace         = "trace"
	wakeReasonService       = "service"
	wakeReasonLaneGone      = "lane-gone"
	wakeReasonOnDeath       = "on-death"
	wakeReasonProviderEvent = "provider-event"
	wakeReasonFollowUp      = "follow-up"
	wakeReasonSupervisor    = "supervisor-reload"
)

// Enqueue asks for keys to be reconciled promptly. No keys means the
// allocator. It reports whether a signal landed, for callers that log only
// on a landed wake (the provider event pump). Under the planner any keys, or
// none, the control-dispatch key included (MAINT-056), mark it dirty; every
// mark lands, so it reports landed at most once per routedLandedEvery: a
// replayed backlog burst stays as quiet as the legacy fold's full channel
// keeps it (API-018).
func (w *controllerWake) Enqueue(reason string, keys ...reconcilekey.Key) bool {
	if w == nil {
		return false
	}
	if w.planner != nil {
		w.planner.markDirty(reason)
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
// per echo is the ga-yoix1 churn shape. Under v2 an event on any leg,
// replays included, marks the planner dirty when beadEventRelevant says a
// pass would care, and never pokes the tick.
func (w *controllerWake) OnBeadEvent(evt events.Event, snapshot bool) {
	if w == nil {
		return
	}
	if p := w.planner; p != nil {
		var recent relevantSet
		if r := p.out.relevant.Load(); r != nil {
			recent = *r
		}
		if beadEventRelevant(evt, recent) {
			p.markDirty("bead-event")
		}
		return
	}
	if snapshot {
		return
	}
	legacyEnqueue(w.pokeCh, w.controlDispatcherCh, reconcilekey.Allocator())
}

// OnEventGap reports that the bead event tail broke or regressed, so events
// may be missing. The legacy reconciler re-reads every store each tick and
// needs nothing; the planner runs a pass, which reads every store too.
func (w *controllerWake) OnEventGap() {
	if w == nil || w.planner == nil {
		return
	}
	w.planner.markDirty("event-gap")
}
