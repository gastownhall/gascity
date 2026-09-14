package main

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

// nudgeEventRetryEpsilon pads each delayed retry an attempt may schedule, so
// the re-check lands just past the quiescence boundary rather than exactly on
// it. nudgeEventRetryBudget bounds how many such retries one IDLE EVENT may
// earn: the event can beat the activity tracker's STAMP of that transition (the
// tracker stamps at its next debounced poll), so the first retry — scheduled
// against a still-working observation — may land just inside the re-stamped
// window (live-verified against herdr 0.7.3); the remaining window shrinks
// every round for a settled-idle agent, so the chain converges, while a busy
// agent burns the whole budget on cheap rejects and the kick dies until its
// next idle event.
//
// The budget belongs to the event, which is what makes that bound real. A full
// backstop sweep (patrol tick, resync) carries no idle event and so grants no
// retries; handing every sweep a fresh budget would refill a permanently busy
// session's chain on every patrol interval, forever.
const (
	nudgeEventRetryEpsilon = 250 * time.Millisecond
	nudgeEventRetryBudget  = 3
)

// nudgeEventDispatcher delivers queued nudges on the provider's push session
// events: an agent_status→idle event for a session with pending queue items
// earns a targeted delivery attempt, and stream resyncs / wake-socket pings /
// patrol fallbacks earn a full pass. Delivery itself is the canonical queued
// path (tryDeliverQueuedNudgesByPoller: claim, fence, blocked-withdrawal,
// provider Nudge, ack) — this component only decides WHEN to attempt it.
//
// Idle events are level-triggered replayed hints (see runtime.SessionEvent):
// an attempt never trusts the event, it re-reads the queue and the live
// observation. The activity tracker stamps LastActivity at the very idle
// transition the event reports (possibly a poll-debounce beat later), so a
// just-idled agent fails the quiescence gate on the first attempts; each
// fresh-stamp reject schedules a retry for when the observed stamp will have
// aged past quiescence, within a small fixed budget. A settled-idle agent's
// remaining window shrinks every round until delivery; a busy agent
// (continuously fresh activity) burns the budget on cheap rejects and the
// kick dies — bounded work per event, never a poller loop. The provider's
// closed-loop delivery verifies paste+submit; it is the backstop, not the
// gate.
//
// A delivery blocks for seconds (idle verification, paste, submit confirm)
// and cannot be canceled: runtime.Provider.Nudge takes no context, and the
// herdr client applies no default timeout, so a wedged pane holds its caller
// until herdr itself returns. The scheduler goroutine therefore never runs a
// delivery itself. It hands each due session to a goroutine of that session's
// own, at most one in flight per session, which is exactly the blast radius
// the per-session sidecar pollers had: one wedged pane strands its own
// session's queue and no other. Coalescing on the in-flight set also keeps the
// goroutine count bounded by the number of sessions with queued work, rather
// than by the number of events that arrived while one was stuck.
//
// The backstop sweep does not deliver. It walks the sessions with pending
// items and hands each to a pass of its own, so a pane wedged inside Nudge
// stalls only its own session and the sweep still reaches the rest. That
// matters most for a session that is ALREADY idle when its nudge is queued:
// it emits no new idle event, so the sweep is its only path, and an inline
// sweep stranded it behind whichever pane happened to hang first.
//
// Providers without an event stream (tmux) leave the dispatcher inactive and
// every existing path byte-identical: the supervisor tick keeps its inline
// pass, and legacy mode keeps its sidecar pollers.
type nudgeEventDispatcher struct {
	parent    context.Context
	cityPath  string
	stderr    io.Writer
	logPrefix string

	// stores hands back the two coordination-class stores a pass needs, from
	// the stores the CONTROLLER already opened. It must not resolve through
	// the one-shot CLI funnel: cliStorageRoutes returns routes carrying a
	// bead.* emit target (class_store_emit.go), which exists for a process
	// that has no emitter of its own. This dispatcher runs inside the
	// controller, which does have one, so resolving that way would put a
	// second emitter on every relocated-class write. Injecting the stores also
	// means no pass opens a store handle it would then have to close.
	stores func() (beads.NudgesStore, beads.Store)

	// Timing knobs, shrunk by tests. quiescence mirrors the sidecar pollers'
	// idle gate; retryEpsilon pads the aged-stamp retry.
	quiescence   time.Duration
	retryEpsilon time.Duration

	mu           sync.Mutex
	cfg          *config.City
	sp           runtime.Provider
	eventCapable bool
	gen          int64              // subscription generation counter
	cancel       context.CancelFunc // cancels the current subscription
	pending      map[string]nudgeEventKick
	fullPassDue  bool
	kicked       chan struct{} // buffered-1 worker wake

	// inflight holds the passes running, keyed by session name, with the empty
	// key reserved for the enumerating sweep. Because the sweep only fans out,
	// every key that names a delivery is a session name, so the set really does
	// bound deliveries to one per session. A kick for a session already in here
	// is dropped rather than queued: the running pass re-reads the queue and
	// the live observation when it gets there, so it will see whatever the
	// dropped kick would have told it.
	inflight map[string]bool
	delivery sync.WaitGroup

	// streamGen holds the generation of the currently-established stream, 0
	// when none. Forward goroutines clear only their own generation, so a
	// late close from a replaced subscription cannot mask a live one.
	streamGen atomic.Int64

	// passObserver, when non-nil, is called after every delivery pass with the
	// filter that pass ran under (empty for a full pass). Tests wait on the
	// pass itself rather than on elapsed time; production leaves it nil.
	passObserver func(sessionFilter string)

	// workerDone closes when the SCHEDULER goroutine has returned. It does not
	// mean every delivery has finished: drainDeliveries bounds that wait and
	// may abandon a goroutine wedged inside Nudge.
	workerDone chan struct{}
}

// nudgeEventKick is one scheduled targeted attempt. retriesLeft is the
// remaining aged-stamp follow-up budget; an attempt that finds the stamp
// still fresh schedules the next retry only while budget remains.
type nudgeEventKick struct {
	dueAt       time.Time
	retriesLeft int
}

// newNudgeEventDispatcher returns a dispatcher whose subscriptions and worker
// live within parent. Wire a provider with update.
func newNudgeEventDispatcher(parent context.Context, cityPath string, stderr io.Writer, logPrefix string, stores func() (beads.NudgesStore, beads.Store)) *nudgeEventDispatcher {
	d := &nudgeEventDispatcher{
		parent:       parent,
		cityPath:     cityPath,
		stderr:       stderr,
		logPrefix:    logPrefix,
		stores:       stores,
		quiescence:   defaultNudgePollQuiescence,
		retryEpsilon: nudgeEventRetryEpsilon,
		pending:      make(map[string]nudgeEventKick),
		kicked:       make(chan struct{}, 1),
		workerDone:   make(chan struct{}),
	}
	go d.worker(parent)
	return d
}

// update points the dispatcher at the runtime's current provider and config.
// resubscribe tears down and re-establishes the event subscription — pass
// true at startup and on a provider swap; a cfg-only reload passes false and
// keeps the stream. Callers serialize updates (startup and config reload both
// run on the reconciler goroutine).
func (d *nudgeEventDispatcher) update(sp runtime.Provider, cfg *config.City, resubscribe bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cfg = cfg
	d.sp = sp
	_, capable := sp.(runtime.SessionEventProvider)
	d.eventCapable = capable
	if !resubscribe {
		return
	}
	if d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
	d.gen++
	d.streamGen.Store(0)
	sep, ok := sp.(runtime.SessionEventProvider)
	if !ok {
		return
	}
	ctx, cancel := context.WithCancel(d.parent)
	events, err := sep.SubscribeSessionEvents(ctx)
	if err != nil {
		cancel()
		fmt.Fprintf(d.stderr, "%s: nudge event subscribe: %v (queued delivery falls back to wake pings and patrol passes)\n", d.logPrefix, err) //nolint:errcheck // best-effort stderr
		return
	}
	d.cancel = cancel
	d.streamGen.Store(d.gen)
	fmt.Fprintf(d.stderr, "%s: nudge event stream active: idle events deliver queued nudges\n", d.logPrefix) //nolint:errcheck // best-effort stderr
	go d.forward(ctx, d.gen, events)
}

// active reports whether the current provider has an event stream — the
// dispatcher owns supervisor-side queued delivery exactly then (in both
// nudge_dispatcher modes), independent of momentary stream health.
func (d *nudgeEventDispatcher) active() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.eventCapable
}

// streaming reports whether an event subscription is currently established.
func (d *nudgeEventDispatcher) streaming() bool {
	return d.streamGen.Load() != 0
}

// kickAll schedules a full dispatcher pass over every pending queue agent.
// Wake-socket pings, patrol fallbacks, and stream resyncs land here. A full
// pass is a sweep, not an idle transition, so it attempts delivery without
// earning any aged-stamp retry budget (see worker).
func (d *nudgeEventDispatcher) kickAll() {
	d.mu.Lock()
	d.fullPassDue = true
	d.mu.Unlock()
	d.wakeWorker()
}

// kickSessionAfter schedules a targeted attempt for one session with the
// given remaining retry budget. Kicks for the same session coalesce to the
// earliest due time and the largest budget — a fresh idle event (full budget)
// restarts the bounded retry cycle without ever losing an earlier schedule.
func (d *nudgeEventDispatcher) kickSessionAfter(session string, delay time.Duration, retriesLeft int) {
	if session == "" {
		return
	}
	dueAt := time.Now().Add(delay)
	d.mu.Lock()
	if prev, ok := d.pending[session]; ok {
		if prev.dueAt.Before(dueAt) {
			dueAt = prev.dueAt
		}
		if prev.retriesLeft > retriesLeft {
			retriesLeft = prev.retriesLeft
		}
	}
	d.pending[session] = nudgeEventKick{dueAt: dueAt, retriesLeft: retriesLeft}
	d.mu.Unlock()
	d.wakeWorker()
}

// wakeWorker signals the worker without ever blocking; a pending wake covers
// this kick too.
func (d *nudgeEventDispatcher) wakeWorker() {
	select {
	case d.kicked <- struct{}{}:
	default:
	}
}

// forward consumes one subscription until the stream ends, translating events
// into kicks. Only the idle-agent kind and resyncs matter here: the provider
// decides which of its own agent states is the idle-equivalent and translates
// at its boundary, so this consumer never matches a provider's status
// spelling. Session liveness kinds have their own consumers.
func (d *nudgeEventDispatcher) forward(ctx context.Context, gen int64, events <-chan runtime.SessionEvent) {
	for {
		select {
		case <-ctx.Done():
			d.streamGen.CompareAndSwap(gen, 0)
			return
		case ev, ok := <-events:
			if !ok {
				if d.streamGen.CompareAndSwap(gen, 0) && ctx.Err() == nil {
					fmt.Fprintf(d.stderr, "%s: nudge event stream ended; queued delivery falls back to wake pings and patrol passes\n", d.logPrefix) //nolint:errcheck // best-effort stderr
				}
				return
			}
			switch ev.Kind {
			case runtime.SessionEventAgentIdle:
				if ev.Session == "" {
					// The provider saw an agent go idle and could not say
					// which one. That is a reason to reconcile every session
					// with pending work, not to drop the transition: the
					// sidecar that used to cover it has been retired for
					// event-capable providers, so ignoring this waits for the
					// next patrol tick instead of reacting to the idle.
					d.kickAll()
					continue
				}
				d.kickSessionAfter(ev.Session, 0, nudgeEventRetryBudget)
			case runtime.SessionEventResync:
				// A resync means events may have been missed: run a full pass
				// now. Unlike the reconciler's session-event pump — whose
				// resync pokes are trailing-delayed because a reconcile can
				// race an in-flight start wave — this pass only delivers to
				// established, quiescence-idle sessions with pending items,
				// so an immediate pass during a start wave is a no-op.
				d.kickAll()
			}
		}
	}
}

// worker owns every dispatcher delivery. It drains due kicks serially: a full
// pass sweeps every session with pending queue items, and each due targeted
// kick gets its own pass carrying the budget its idle event earned. Idle waits
// between kicks are timer-driven off the earliest scheduled attempt.
func (d *nudgeEventDispatcher) worker(ctx context.Context) {
	// LIFO: drain the delivery goroutines first, then report the worker down.
	// Anything observing workerDone (shutdown, and the tests' teardown) then
	// knows no pass is still writing.
	defer close(d.workerDone)
	defer d.drainDeliveries()
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	for {
		now := time.Now()
		d.mu.Lock()
		full := d.fullPassDue
		d.fullPassDue = false
		var due []string
		var dueBudget []int
		var nextDue time.Time
		for name, kick := range d.pending {
			if !kick.dueAt.After(now) {
				due = append(due, name)
				dueBudget = append(dueBudget, kick.retriesLeft)
				delete(d.pending, name)
				continue
			}
			if nextDue.IsZero() || kick.dueAt.Before(nextDue) {
				nextDue = kick.dueAt
			}
		}
		d.mu.Unlock()

		if full {
			// A full pass is a backstop sweep (patrol tick, stream resync), not
			// an idle transition, so it earns NO retry budget. The aged-stamp
			// retry exists to cover the race between an idle EVENT and the
			// activity tracker's STAMP of that same transition; a sweep carries
			// no such event. Granting it a fresh chain would refill the budget
			// of every busy session on every patrol tick, which is the poll
			// loop this dispatcher replaced rather than a bounded reaction to
			// an event. A still-fresh stamp at sweep time is covered by the
			// next sweep, when it has aged — patrol is the safety net here.
			//
			// Scheduled targeted kicks are deliberately left pending rather
			// than folded into the sweep: each was earned by a real idle event
			// and is due at the moment its target's stamp ages out, which the
			// sweep's own timing has nothing to do with.
			d.spawnPass("", 0)
		}
		for i, name := range due {
			d.spawnPass(name, dueBudget[i])
		}
		if !full && len(due) == 0 {
			if !nextDue.IsZero() {
				timer.Reset(time.Until(nextDue))
				select {
				case <-ctx.Done():
					return
				case <-d.kicked:
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
				case <-timer.C:
				}
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-d.kicked:
			}
			continue
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// observePasses installs fn, called after each completed delivery pass. It is
// the dispatcher's completion signal: a caller that needs to know a pass
// happened waits for the call instead of for a duration. Used by tests; no
// production caller installs one.
func (d *nudgeEventDispatcher) observePasses(fn func(sessionFilter string)) {
	d.mu.Lock()
	d.passObserver = fn
	d.mu.Unlock()
}

// pass runs one delivery pass and then notifies the observer. Every
// worker-driven pass goes through here, including the ones that find nothing
// to deliver and return early, so the notification means "a pass completed",
// nudgeEventDeliveryDrainGrace bounds how long shutdown waits for in-flight
// delivery goroutines. It is a bound rather than an unconditional Wait because
// the thing being waited on is exactly the thing that can wedge: Nudge takes no
// context, so a delivery into a hung pane returns when herdr returns and not
// before. Blocking city shutdown on a hung pane would be a worse failure than
// abandoning the goroutine, so after the grace the process goes on without it.
const nudgeEventDeliveryDrainGrace = 5 * time.Second

// drainDeliveries waits out the in-flight delivery goroutines, up to the
// grace, and says so when the grace expires with deliveries still running.
//
// The report is the point. An expired grace means the process is about to tear
// down the provider and the stores while a goroutine is still inside Nudge, so
// that goroutine's own later logging may go to a writer nobody reads and its
// post-delivery stamp may land on a closed store. Abandoning it is still the
// right call, because blocking city shutdown on a hung pane is the worse
// failure. Doing it silently is not: this line is the only place an operator
// learns that a pane was hung at shutdown.
//
// Note what workerDone does and does not mean after this returns: the SCHEDULER
// is down and will start no further pass. It is not a statement that no
// delivery is still running.
func (d *nudgeEventDispatcher) drainDeliveries() {
	done := make(chan struct{})
	go func() {
		d.delivery.Wait()
		close(done)
	}()
	timer := time.NewTimer(nudgeEventDeliveryDrainGrace)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		fmt.Fprintf(d.stderr, "%s: nudge event dispatch: a queued-nudge delivery was still running after %s; abandoning it so shutdown is not blocked on a hung pane\n", d.logPrefix, nudgeEventDeliveryDrainGrace) //nolint:errcheck // best-effort stderr
	}
}

// spawnPass runs one pass on a goroutine of its own, unless a pass for the
// same session is already running. It returns as soon as the goroutine is
// started (or declined), so the scheduler loop never blocks on a delivery.
//
// Declining a duplicate is not a lost wake-up. The running pass re-reads the
// queue and the live observation for its session, so anything the dropped
// kick would have carried is already in what that pass reads; and the patrol
// tick's full pass is the backstop if the running pass finishes just before
// the item lands.
func (d *nudgeEventDispatcher) spawnPass(sessionFilter string, retriesLeft int) {
	d.mu.Lock()
	if d.inflight == nil {
		d.inflight = map[string]bool{}
	}
	if d.inflight[sessionFilter] {
		d.mu.Unlock()
		return
	}
	d.inflight[sessionFilter] = true
	d.mu.Unlock()

	d.delivery.Add(1)
	go func() {
		defer d.delivery.Done()
		defer func() {
			d.mu.Lock()
			delete(d.inflight, sessionFilter)
			d.mu.Unlock()
		}()
		d.pass(sessionFilter, retriesLeft)
	}()
}

// not "a pass delivered something".
func (d *nudgeEventDispatcher) pass(sessionFilter string, retriesLeft int) {
	d.runPass(sessionFilter, retriesLeft)
	d.mu.Lock()
	observe := d.passObserver
	d.mu.Unlock()
	if observe != nil {
		observe(sessionFilter)
	}
}

// nudgeDispatchStores derives the two coordination-class stores a delivery pass
// needs. Both come from cityStore, and neither is ever derived from the other.
//
// That is the whole point of the helper. resolveClassStore returns its BASE
// verbatim whenever the class is not relocated, so a session store derived from
// the nudges store is correct on every city that relocates both classes together
// or neither — and silently reads session beads out of the nudges database on a
// city that relocates `[beads.classes.nudges]` alone. main carries four
// instances of exactly that shape in cmd_nudge.go; they are filed as
// gastownhall/gascity#6348, and this pass is not a fifth.
func nudgeDispatchStores(routes *storageRoutes, cityStore beads.Store, cfg *config.City, cityPath string, rec events.Recorder) (beads.NudgesStore, beads.Store) {
	nudges := beads.NudgesStore{Store: resolveNudgesStore(routes, cityStore, cfg, cityPath, rec)}
	return nudges, resolveSessionStore(routes, cityStore, cfg, cityPath, rec)
}

// runPass executes one delivery pass: the whole pending queue when
// sessionFilter is empty, one session otherwise. Attempts use the sidecar
// pollers' quiescence gate; an attempt whose target's activity stamp is
// fresher than quiescence schedules an aged-stamp retry while retriesLeft
// budget remains.
func (d *nudgeEventDispatcher) runPass(sessionFilter string, retriesLeft int) {
	d.mu.Lock()
	cfg := d.cfg
	sp := d.sp
	d.mu.Unlock()
	if cfg == nil || sp == nil {
		return
	}
	if d.stores == nil {
		return
	}
	store, sessStore := d.stores()
	if store.Store == nil || sessStore == nil {
		return
	}
	sessionBeads, err := loadSessionBeadSnapshot(sessStore)
	if err != nil {
		fmt.Fprintf(d.stderr, "%s: nudge event dispatch: loading session beads: %v\n", d.logPrefix, err) //nolint:errcheck // best-effort stderr
		return
	}
	if sessionFilter == "" {
		// The sweep ENUMERATES; it never delivers. Handing each due session to
		// its own pass is what makes the in-flight set mean what it says: every
		// actual delivery is keyed by session name, so one session can never be
		// delivered to twice at once, and a pane wedged inside Nudge cannot
		// stall the sweep that would have reached the other sessions. An
		// already-idle session emits no new idle event, so before this the
		// sweep was its ONLY path and a single wedged pane stranded it.
		fanOut := func(target nudgeTarget, _ worker.LiveObservation) (bool, error) {
			d.spawnPass(target.sessionName, 0)
			return false, nil
		}
		if _, err := deliverPendingQueuedNudges(d.cityPath, cfg, sessStore, sp, sessionBeads, "", d.stderr, fanOut); err != nil {
			fmt.Fprintf(d.stderr, "%s: nudge event dispatch sweep: %v\n", d.logPrefix, err) //nolint:errcheck // best-effort stderr
		}
		return
	}
	deliver := func(target nudgeTarget, obs worker.LiveObservation) (bool, error) {
		ok, err := tryDeliverQueuedNudgesByPoller(target, store.Store, sessStore, sp, d.quiescence, obs)
		if ok || err != nil {
			return ok, err
		}
		if retriesLeft <= 0 {
			return false, nil
		}
		if remaining, fresh := nudgeQuiescenceRemaining(obs, d.quiescence, time.Now()); fresh {
			// The activity stamp is younger than the quiescence window —
			// either the idle transition that triggered this attempt (the
			// tracker may stamp it a beat AFTER the event arrives) or an
			// agent that is genuinely busy. Retrying after the observed
			// stamp would age out distinguishes them: a settled-idle agent's
			// remaining window shrinks each round until delivery, a busy
			// agent burns the bounded budget on cheap rejects and the kick
			// dies until its next idle event.
			d.kickSessionAfter(target.sessionName, remaining+d.retryEpsilon, retriesLeft-1)
		}
		return false, nil
	}
	if _, err := deliverPendingQueuedNudges(d.cityPath, cfg, sessStore, sp, sessionBeads, sessionFilter, d.stderr, deliver); err != nil {
		fmt.Fprintf(d.stderr, "%s: nudge event dispatch: %v\n", d.logPrefix, err) //nolint:errcheck // best-effort stderr
	}
}

// nudgeQuiescenceRemaining reports how much of the quiescence window is left
// for obs's activity stamp, and whether the stamp is indeed fresher than the
// window. Targets without an activity signal never report fresh — their idle
// gate is not stamp-based.
func nudgeQuiescenceRemaining(obs worker.LiveObservation, quiescence time.Duration, now time.Time) (time.Duration, bool) {
	if obs.LastActivity == nil || obs.LastActivity.IsZero() || quiescence <= 0 {
		return 0, false
	}
	since := now.Sub(*obs.LastActivity)
	if since < 0 {
		since = 0
	}
	if since >= quiescence {
		return 0, false
	}
	return quiescence - since, true
}

// providerRetiresNudgePollers reports whether sp's event stream retires the
// sidecar poller class: the supervisor-hosted event dispatcher owns queued
// delivery for such providers (in both nudge_dispatcher modes), so a spawned
// poller would only race it. A nil provider fails open — callers without a
// resolved provider keep today's spawn behavior.
func providerRetiresNudgePollers(sp runtime.Provider) bool {
	if sp == nil {
		return false
	}
	_, ok := sp.(runtime.SessionEventProvider)
	return ok
}
