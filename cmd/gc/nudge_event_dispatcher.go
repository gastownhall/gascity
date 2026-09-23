package main

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

// nudgeEventRetryEpsilon pads each delayed retry an attempt may schedule, so
// the re-check lands just past the quiescence boundary rather than exactly on
// it. nudgeEventRetryBudget bounds how many such retries one kick may earn:
// the idle EVENT can beat the activity tracker's STAMP of that transition (the
// tracker stamps at its next debounced poll), so the first retry — scheduled
// against a still-working observation — may land just inside the re-stamped
// window (live-verified against herdr 0.7.3); the remaining window shrinks
// every round for a settled-idle agent, so the chain converges, while a busy
// agent burns the whole budget on cheap rejects and the kick dies until its
// next idle event.
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
// All deliveries run on one worker goroutine: a delivery blocks for seconds
// (idle verification, paste, submit confirm), which must never stall the
// reconciler loop, and a single worker means supervisor-side deliveries to
// the same pane cannot interleave.
//
// Providers without an event stream (tmux) leave the dispatcher inactive
// (active() false), and the CLI-side sidecar-poller call sites gate on the
// live wake socket (nudgeDispatcherIsHosting), not on provider capability —
// capability alone cannot tell a healthy stream from a subscribe failure or
// a dead supervisor, and mistaking one for the other would suppress the only
// deliverer for a queued item.
type nudgeEventDispatcher struct {
	parent    context.Context
	cityPath  string
	stderr    io.Writer
	logPrefix string

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

	// streamGen holds the generation of the currently-established stream, 0
	// when none. Forward goroutines clear only their own generation, so a
	// late close from a replaced subscription cannot mask a live one.
	streamGen atomic.Int64

	// fullPassesQueued counts every kickAll (a full pass becoming due),
	// including the leading resync every subscription sends. streaming()
	// alone cannot tell a caller whether that leading resync has actually
	// been forwarded yet — forward() starts as its own goroutine after
	// streamGen is stamped, so a check racing its startup would otherwise
	// see the dispatcher's untouched zero state and mistake "never ran" for
	// "already settled". Tests poll this to know the leading pass has been
	// scheduled before checking pending/fullPassDue for it having drained.
	fullPassesQueued atomic.Int64

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
func newNudgeEventDispatcher(parent context.Context, cityPath string, stderr io.Writer, logPrefix string) *nudgeEventDispatcher {
	d := &nudgeEventDispatcher{
		parent:       parent,
		cityPath:     cityPath,
		stderr:       stderr,
		logPrefix:    logPrefix,
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
// Wake-socket pings, patrol fallbacks, and stream resyncs land here.
func (d *nudgeEventDispatcher) kickAll() {
	d.mu.Lock()
	d.fullPassDue = true
	d.mu.Unlock()
	d.fullPassesQueued.Add(1)
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
// into kicks. Only idle agent-status events and resyncs matter here — session
// liveness kinds have their own consumers.
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
// pass covers (and clears) all targeted kicks; otherwise each due session
// gets a targeted pass. Idle waits between kicks are timer-driven off the
// earliest scheduled attempt.
func (d *nudgeEventDispatcher) worker(ctx context.Context) {
	defer close(d.workerDone)
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
			if full || !kick.dueAt.After(now) {
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

		switch {
		case full:
			d.runPass("", nudgeEventRetryBudget)
		case len(due) > 0:
			for i, name := range due {
				d.runPass(name, dueBudget[i])
			}
		default:
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
	store, err := openNudgeBeadStoreErr(d.cityPath)
	if err != nil {
		fmt.Fprintf(d.stderr, "%s: nudge event dispatch: opening nudge bead store: %v\n", d.logPrefix, err) //nolint:errcheck // best-effort stderr
		return
	}
	if nudgeBeadStoreOwned(d.cityPath) {
		// Only close a handle this pass opened itself. When the nudges class is
		// relocated to a split binding, store.Store is the shared, process-scoped
		// route cliStorageRoutes owns; closing it here would tear it down for
		// every other consumer of that binding after the first pass.
		defer closeBeadStoreHandle(store.Store) //nolint:errcheck // best-effort close
	}
	// Session-class reads route through the session store (identity today);
	// the nudge queue stays on its own store.
	sessStore := cliSessionStore(store.Store, cfg, d.cityPath)
	sessionBeads, err := loadSessionBeadSnapshot(sessStore)
	if err != nil {
		fmt.Fprintf(d.stderr, "%s: nudge event dispatch: loading session beads: %v\n", d.logPrefix, err) //nolint:errcheck // best-effort stderr
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
