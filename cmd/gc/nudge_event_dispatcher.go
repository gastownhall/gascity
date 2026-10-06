package main

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

const (
	nudgeEventRetryEpsilon = 250 * time.Millisecond
	nudgeEventRetryBudget  = 3
	nudgeSweepRetryBudget  = 1
)

type nudgeEventDispatcher struct {
	parent    context.Context
	cancel    context.CancelFunc
	cityPath  string
	stderr    io.Writer
	logPrefix string

	stores func(cfg *config.City) (beads.NudgesStore, beads.Store)

	quiescence   time.Duration
	retryEpsilon time.Duration

	mu                    sync.Mutex
	cfg                   *config.City
	sp                    runtime.Provider
	eventCapable          bool
	pending               map[string]nudgeEventKick
	lastUnattributedSweep time.Time
	fullPassDue           bool
	kicked                chan struct{}

	inflight map[string]bool

	declined map[string]int
	delivery sync.WaitGroup

	passSlots chan struct{}

	passObserver func(sessionFilter string)

	workerDone chan struct{}
}

type nudgeEventKick struct {
	dueAt       time.Time
	retriesLeft int
}

func newNudgeEventDispatcher(parent context.Context, cityPath string, stderr io.Writer, logPrefix string, stores func(cfg *config.City) (beads.NudgesStore, beads.Store)) *nudgeEventDispatcher {
	ctx, cancel := context.WithCancel(parent)
	d := &nudgeEventDispatcher{
		parent:       ctx,
		cancel:       cancel,
		cityPath:     cityPath,
		stderr:       stderr,
		logPrefix:    logPrefix,
		stores:       stores,
		quiescence:   defaultNudgePollQuiescence,
		retryEpsilon: nudgeEventRetryEpsilon,
		pending:      make(map[string]nudgeEventKick),
		kicked:       make(chan struct{}, 1),
		workerDone:   make(chan struct{}),

		passSlots: make(chan struct{}, nudgeEventPassConcurrency),
	}
	go d.worker(ctx)
	return d
}

func (d *nudgeEventDispatcher) stop() {
	if d.cancel != nil {
		d.cancel()
	}
}

func (d *nudgeEventDispatcher) update(sp runtime.Provider, cfg *config.City) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cfg = cfg
	d.sp = sp
}

func (d *nudgeEventDispatcher) active() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.eventCapable
}

func (d *nudgeEventDispatcher) setEventCapable(capable bool) {
	d.mu.Lock()
	d.eventCapable = capable
	d.mu.Unlock()
}

func (d *nudgeEventDispatcher) kickAll() {
	d.mu.Lock()
	d.fullPassDue = true
	d.mu.Unlock()
	d.wakeWorker()
}

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

func (d *nudgeEventDispatcher) wakeWorker() {
	select {
	case d.kicked <- struct{}{}:
	default:
	}
}

const nudgeEventUnattributedSweepInterval = 5 * time.Second

func (d *nudgeEventDispatcher) kickAllUnattributed(now time.Time) {
	d.mu.Lock()
	if !d.lastUnattributedSweep.IsZero() && now.Sub(d.lastUnattributedSweep) < nudgeEventUnattributedSweepInterval {
		d.mu.Unlock()
		return
	}
	d.lastUnattributedSweep = now
	d.mu.Unlock()
	d.kickAll()
}

func (d *nudgeEventDispatcher) handleEvent(ev runtime.SessionEvent) {
	switch ev.Kind {
	case runtime.SessionEventAgentIdle:
		if ev.Session == "" {
			d.kickAllUnattributed(time.Now())
			return
		}
		d.kickSessionAfter(ev.Session, 0, nudgeEventRetryBudget)
	case runtime.SessionEventResync:
		d.kickAll()
	}
}

func (d *nudgeEventDispatcher) worker(ctx context.Context) {
	defer close(d.workerDone)
	defer d.drainDeliveries()
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
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

func (d *nudgeEventDispatcher) observePasses(fn func(sessionFilter string)) {
	d.mu.Lock()
	d.passObserver = fn
	d.mu.Unlock()
}

const nudgeEventDeliveryDrainGrace = 5 * time.Second

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
		fmt.Fprintf(d.stderr, "%s: nudge event dispatch: a queued-nudge delivery was still running after %s; abandoning it so shutdown is not blocked on a hung pane\n", d.logPrefix, nudgeEventDeliveryDrainGrace) //nolint:errcheck
	}
}

const nudgeEventPassConcurrency = 8

func (d *nudgeEventDispatcher) acquirePassSlot() (func(), bool) {
	if d.passSlots == nil {
		return func() {}, true
	}
	select {
	case d.passSlots <- struct{}{}:
	case <-d.parent.Done():
		return func() {}, false
	}
	if d.parent.Err() != nil {
		<-d.passSlots
		return func() {}, false
	}
	return func() { <-d.passSlots }, true
}

func (d *nudgeEventDispatcher) spawnPass(sessionFilter string, retriesLeft int) {
	d.mu.Lock()
	if d.inflight == nil {
		d.inflight = map[string]bool{}
	}
	if d.inflight[sessionFilter] {
		if d.declined == nil {
			d.declined = map[string]int{}
		}
		if prev, ok := d.declined[sessionFilter]; !ok || retriesLeft > prev {
			d.declined[sessionFilter] = retriesLeft
		}
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
			budget, deferred := d.declined[sessionFilter]
			delete(d.declined, sessionFilter)
			d.mu.Unlock()
			if deferred {
				d.rearmDeclined(sessionFilter, budget)
			}
		}()
		release, acquired := d.acquirePassSlot()
		if !acquired {
			return
		}
		defer release()
		d.pass(sessionFilter, retriesLeft)
	}()
}

func (d *nudgeEventDispatcher) rearmDeclined(sessionFilter string, retriesLeft int) {
	if sessionFilter == "" {
		d.kickAll()
		return
	}
	d.kickSessionAfter(sessionFilter, 0, retriesLeft)
}

func (d *nudgeEventDispatcher) pass(sessionFilter string, retriesLeft int) {
	d.runPass(sessionFilter, retriesLeft)
	d.mu.Lock()
	observe := d.passObserver
	d.mu.Unlock()
	if observe != nil {
		observe(sessionFilter)
	}
}

func nudgeDispatchStores(routes *storageRoutes, cityStore beads.Store, cfg *config.City, cityPath string, rec events.Recorder) (beads.NudgesStore, beads.Store) {
	nudges := beads.NudgesStore{Store: resolveNudgesStore(routes, cityStore, cfg, cityPath, rec)}
	return nudges, resolveSessionStore(routes, cityStore, cfg, cityPath, rec)
}

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
	store, sessStore := d.stores(cfg)
	if store.Store == nil || sessStore == nil {
		return
	}
	sessionBeads, err := loadSessionBeadSnapshot(sessStore)
	if err != nil {
		fmt.Fprintf(d.stderr, "%s: nudge event dispatch: loading session beads: %v\n", d.logPrefix, err) //nolint:errcheck
		return
	}
	if sessionFilter == "" {
		targets, err := pendingNudgeTargets(d.cityPath, cfg, sessionBeads)
		if err != nil {
			fmt.Fprintf(d.stderr, "%s: nudge event dispatch sweep: %v\n", d.logPrefix, err) //nolint:errcheck
			return
		}
		for _, target := range targets {
			if d.parent.Err() != nil {
				return
			}
			d.spawnPass(target.sessionName, nudgeSweepRetryBudget)
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
			d.kickSessionAfter(target.sessionName, remaining+d.retryEpsilon, retriesLeft-1)
		}
		return false, nil
	}
	if _, err := deliverPendingQueuedNudges(d.cityPath, cfg, sessStore, sp, sessionBeads, sessionFilter, d.stderr, deliver); err != nil {
		fmt.Fprintf(d.stderr, "%s: nudge event dispatch: %v\n", d.logPrefix, err) //nolint:errcheck
	}
}

func (d *nudgeEventDispatcher) handQueueToPollers() {
	d.mu.Lock()
	cfg := d.cfg
	sp := d.sp
	d.mu.Unlock()
	if cfg == nil || sp == nil || d.stores == nil || nudgeDispatcherIsSupervisor(cfg) {
		return
	}
	_, sessStore := d.stores(cfg)
	if sessStore == nil {
		return
	}
	sessionBeads, err := loadSessionBeadSnapshot(sessStore)
	if err != nil {
		fmt.Fprintf(d.stderr, "%s: nudge poller handoff: loading session beads: %v\n", d.logPrefix, err) //nolint:errcheck
		return
	}
	targets, err := pendingNudgeTargets(d.cityPath, cfg, sessionBeads)
	if err != nil {
		fmt.Fprintf(d.stderr, "%s: nudge poller handoff: %v\n", d.logPrefix, err) //nolint:errcheck
		return
	}
	for _, target := range targets {
		maybeStartNudgePoller(target, sp)
	}
}

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

func providerRetiresNudgePollers(target nudgeTarget, sp runtime.Provider) bool {
	if sp == nil {
		return false
	}
	if _, ok := sp.(runtime.Router); ok {
		leaf, _, known := runtime.ResolveBackend(sp, target.sessionName)
		if !known {
			return false
		}
		_, ok := leaf.(runtime.SessionEventProvider)
		return ok
	}
	_, ok := sp.(runtime.SessionEventProvider)
	return ok
}
