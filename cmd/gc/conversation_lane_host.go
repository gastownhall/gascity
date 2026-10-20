package main

// The controller's conversation-lane host (DES-O1 §7): it runs
// opjournal.ConversationLane passes for every conversation with open lane
// ops, one worker per conversation (conversation_lane_worker.go) in at most
// laneHostMaxWorkers slots, each calling Pass serially with the live executor
// (conversation_lane_exec.go).
//
// Doorbells (DES-O1 §7.3) start or wake a conversation's worker: Poke for
// in-process producers, the controller-socket verb lane:<conversation>[:<op>],
// the ledger feed's page apply (CH-4), which pokes every conversation whose
// op or runtime row an out-of-band write changed (pokeChangedRows), each
// worker's own timer at its PassResult.Next, and the anti-entropy scan
// (opjournal.Store.OpenLaneConversations). The scan runs at boot, and then
// every laneHostScanInterval while a kind is listed in [ops] authoritative or
// the last scan found an open lane op, so with the list empty an idle city
// pays one keyed scan per controller start, and lane ops already open after a
// rollback still drain once whatever gate they wait on clears (DES-O1 §7.1,
// §12). The list is latched at controller start.
//
// A worker holds its slot only while no other conversation waits for one and
// its next pass is at most laneParkAfter away; otherwise it parks the
// conversation on a timer, so neither long waits nor busy runtimes starve the
// queue.
//
// The host runs whenever the sessions store can apply guarded batches,
// whatever [ops] authoritative says. A store that cannot keeps the host off:
// silently with the list empty, and with a kind listed with one logged
// refusal (errLaneStoreIncapable) that the conversation-lanes doctor check
// reports. A sessions store that is missing (the city store did not open) is
// not a refusal: the host waits for it, re-checking on the scan cadence.
//
// Both controller entry points start the host right after the op sweeper:
// the standalone controller (runController) and each supervisor-hosted city
// (startOneCity), with the city's context.
//
// The view, the op rows and the wake decision read live (laneLiveSessions);
// the executor resolves the runtime through the controller's cached sessions
// store. Writes and guarded batches go through the sessions store, so the
// controller's cache and its event bridge see them. Every pass runs as store
// work and is skipped while the city is quiescent. Failures are logged,
// rate-limited, and counted in lane-status. The host emits no events of its
// own.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"reflect"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

const (
	// laneHostName names the controller's lane in lease nonces and
	// transitions.
	laneHostName = "ctl"
	// laneHostMaxWorkers caps the conversations served at once; pokes past
	// it queue until a worker exits.
	laneHostMaxWorkers = 64
	// laneHostScanInterval is the anti-entropy cadence with a kind listed.
	laneHostScanInterval = time.Minute
	// laneHostScanLimit caps each page of the scan's keyed lists.
	laneHostScanLimit = 500
	// The busy re-probe backoff (DES-O1 §4.3): 2 s for the first 30 s, then
	// 5 s, then 10 s.
	laneBusyFirst    = 2 * time.Second
	laneBusyFirstFor = 30 * time.Second
	laneBusyNext     = 5 * time.Second
	laneBusyNextFor  = time.Minute
	laneBusyMax      = 10 * time.Second
	// A failed pass backs off from laneErrBackoffStart, doubling up to
	// laneErrBackoffCap.
	laneErrBackoffStart = time.Second
	laneErrBackoffCap   = time.Minute
	// laneQuiescentRetry is how long a worker waits for a quiescent city.
	laneQuiescentRetry = 30 * time.Second
	// laneWakeRecheck is how long a worker waits after recording a wake
	// before it passes again, and between passes that find the woken
	// session still starting; laneWakeHeldRecheck after a wake the row
	// held, or refused because the row is not dormant.
	laneWakeRecheck     = 5 * time.Second
	laneWakeHeldRecheck = time.Minute
	// laneParkAfter is the longest a worker holds its slot waiting for its
	// next pass; a longer wait parks the conversation on a timer instead, so
	// conversations whose ops wait for hours (not_live, a far not_before)
	// cannot fill every slot. With a conversation queued for a slot, any
	// wait parks.
	laneParkAfter = time.Minute
	// laneStatusIDs caps the quarantined ids, and the waiting conversations,
	// the status lists.
	laneStatusIDs = 8
	// laneStatusWaits caps the waits the status lists per conversation.
	laneStatusWaits = 4
	// laneErrorLogInterval is how often one kind of failure is logged, as
	// the op sweeper's degrade lines are.
	laneErrorLogInterval = 10 * time.Minute
)

// Lane host states, as lane-status reports them.
const (
	laneHostStateRunning = "running"
	laneHostStateOff     = "off"
	laneHostStateRefused = "refused"
	// laneHostStateUnavailable is a host whose controller has no sessions
	// store yet; it re-checks on the scan cadence.
	laneHostStateUnavailable = "unavailable"
	// laneHostStateNone is lane-status's answer when this controller holds
	// no lane host for the city.
	laneHostStateNone = "no_host"
)

// laneStatusCommand is the controller-socket verb that answers the lane
// host's status; laneSocketPrefix starts the doorbell verb.
const (
	laneStatusCommand = "lane-status"
	laneSocketPrefix  = "lane:"
)

// errLaneStoreIncapable is the typed refusal of a sessions store that cannot
// apply guarded batches while [ops] authoritative lists a kind (DES-O1 D2).
var errLaneStoreIncapable = errors.New("conversation lanes need guarded batches")

// laneStoreIncapableError is errLaneStoreIncapable with the store's binding
// and the remedy.
type laneStoreIncapableError struct {
	binding string
}

func (e *laneStoreIncapableError) Error() string {
	return fmt.Sprintf("%s; this city's sessions class is bound to %s. Remove the kind from [ops] authoritative, or bind the sessions class to a native store",
		errLaneStoreIncapable, e.binding)
}

// Is matches errLaneStoreIncapable.
func (e *laneStoreIncapableError) Is(target error) bool { return target == errLaneStoreIncapable }

// errLaneStoreMissing is a controller with no sessions store: the city store
// did not open. It is not a refusal; the host waits for the store.
var errLaneStoreMissing = errors.New("the controller has no sessions store yet (the city store did not open); the lane host starts once it does")

// laneStoreCapability reports whether store can host conversation lanes:
// errLaneStoreMissing with no store, and the typed refusal for a store that
// cannot apply guarded batches.
func laneStoreCapability(store beads.Store) error {
	if isNilBeadStore(store) {
		return errLaneStoreMissing
	}
	if _, ok := beads.GuardedBatcherFor(beads.SessionStore{Store: store}); ok {
		return nil
	}
	return &laneStoreIncapableError{binding: laneStoreBinding(store)}
}

// laneStoreBinding names what store is bound to, for the refusal.
func laneStoreBinding(store beads.Store) string {
	cur := store
	for range 8 {
		if _, ok := cur.(*beads.BdStore); ok {
			return "the bd CLI (BdStore)"
		}
		next := laneUnwrapStore(cur)
		if isNilBeadStore(next) {
			break
		}
		cur = next
	}
	return fmt.Sprintf("a store without guarded batches (%T)", cur)
}

// laneUnwrapStore is the store store wraps, nil when it wraps none: a
// cache's backing, or a wrapper's conditional-writes target.
func laneUnwrapStore(store beads.Store) beads.Store {
	switch s := store.(type) {
	case *beads.CachingStore:
		return s.Backing()
	case beads.ConditionalWritesResolveTargeter:
		return s.ConditionalWritesResolveTarget()
	}
	return nil
}

// isNilBeadStore reports a nil store, or a nil pointer in a store interface.
func isNilBeadStore(s beads.Store) bool {
	if s == nil {
		return true
	}
	v := reflect.ValueOf(s)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

// laneLiveSessions is the sessions store as the lane host reads the view,
// the op rows and the row a wake is decided on: Get and List read live, past
// any cache, and every write and guarded batch goes through the wrapped
// store, so the cache and its event bridge see them.
// ConditionalWritesResolveTarget returns the wrapped store, so the op store
// still finds its conditional writes and id prefix.
type laneLiveSessions struct {
	beads.Store
}

// Get reads id live.
func (s laneLiveSessions) Get(id string) (beads.Bead, error) {
	return beads.HandlesFor(s.Store).Live.Get(id)
}

// List reads q live.
func (s laneLiveSessions) List(q beads.ListQuery) ([]beads.Bead, error) {
	return beads.HandlesFor(s.Store).Live.List(q)
}

// ConditionalWritesResolveTarget implements
// beads.ConditionalWritesResolveTargeter.
func (s laneLiveSessions) ConditionalWritesResolveTarget() beads.Store { return s.Store }

// GuardedBatcherHandle forwards guarded batches to the wrapped store.
func (s laneLiveSessions) GuardedBatcherHandle() (beads.GuardedBatcher, bool) {
	return beads.GuardedBatcherFor(s.Store)
}

// laneHostEnv is the controller state the host runs over.
type laneHostEnv interface {
	SessionsBeadStore() beads.SessionStore
	SessionProvider() runtime.Provider
	Config() *config.City
	CityPath() string
	Enqueue(keys ...reconcilekey.Key)
	beginStoreWork() (end func())
	storesQuiescent() bool
}

// conversationLaneHost is one controller's lane host.
type conversationLaneHost struct {
	env   laneHostEnv
	kinds []string
	clk   clock.Clock
	// maxWorkers and scanInterval are laneHostMaxWorkers and
	// laneHostScanInterval; tests shrink them.
	maxWorkers   int
	scanInterval time.Duration
	// newTimer and newTicker start the workers' timers and the scan ticker;
	// tests drive them by hand.
	newTimer  func(time.Duration) (<-chan time.Time, func())
	newTicker func(time.Duration) (<-chan time.Time, func())
	// newExec builds a pass's executor; production is the live executor.
	newExec func(laneExecDeps, session.Info) opjournal.Executor
	logf    func(format string, args ...any)
	// onPass, onReprobe and onWorkerExit are test seams: each pass's
	// outcome, each re-probe, and each worker's exit. Nil in production.
	onPass       func(conv string, out lanePassOutcome)
	onReprobe    func(conv string, stillBusy bool)
	onWorkerExit func(conv string)

	ctx context.Context
	wg  sync.WaitGroup
	// scanFrom is where the last scan's capped lists stopped; only the scan
	// loop reads and writes it.
	scanFrom opjournal.LaneScanCursor

	mu      sync.Mutex
	state   string
	reason  string
	st      *opjournal.Store
	lane    *opjournal.ConversationLane
	stOver  beads.Store
	workers map[string]*laneWorker
	queued  []string
	inQueue map[string]bool
	// parked holds each conversation whose worker yielded its slot: its park
	// timer's stop, and the state its next worker starts from. A parked
	// conversation has no worker, and stays parked until its next worker
	// starts, through a wait in the queue after its timer fired.
	parked map[string]*laneParked
	// logged is when each kind of failure was last logged.
	logged map[string]time.Time
	// status counters
	passes   int64
	reprobes int64
	// passErrors counts every failure; sinceSuccess those since the last
	// pass that succeeded, and lastFailed whether the last pass failed.
	passErrors   int64
	sinceSuccess int64
	lastFailed   bool
	lastPass     time.Time
	lastSuccess  time.Time
	lastErr      string
	quarantined  map[string][]string
	waits        map[string]laneConversationWaits
	// noticeFailures tallies, per planner notice action (an attempt's
	// proposal or a reminder's read, plannerNoticeTallyKey), its failures in
	// a row; nil until the first. An entry is deleted when its action
	// succeeds or its attempt is spent; one whose reminder is dropped before
	// either stays for the host's life, a few bytes.
	noticeFailures map[string]*laneNoticeFailures
}

// laneWorker is one conversation's slot; poke wakes its worker.
type laneWorker struct {
	poke chan struct{}
}

// laneParked is one parked conversation.
type laneParked struct {
	stop  chan struct{}
	state laneWorkerState
}

// newConversationLaneHost builds the host over env, latching kinds.
func newConversationLaneHost(env laneHostEnv, kinds []string) *conversationLaneHost {
	return &conversationLaneHost{
		env:          env,
		kinds:        append([]string(nil), kinds...),
		clk:          clock.Real{},
		maxWorkers:   laneHostMaxWorkers,
		scanInterval: clock.Backstop(laneHostScanInterval),
		newTimer: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTimer(d)
			return t.C, func() { t.Stop() }
		},
		newTicker: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		},
		newExec: func(deps laneExecDeps, info session.Info) opjournal.Executor {
			return newConversationLaneExec(deps, info)
		},
		logf:        log.Printf,
		workers:     map[string]*laneWorker{},
		inQueue:     map[string]bool{},
		parked:      map[string]*laneParked{},
		logged:      map[string]time.Time{},
		quarantined: map[string][]string{},
		waits:       map[string]laneConversationWaits{},
	}
}

// startConversationLaneHost starts the controller's lane host until ctx
// ends, with [ops] authoritative as the controller loaded it at start, and
// rings it from the controller's ledger feeds.
func (cs *controllerState) startConversationLaneHost(ctx context.Context) *conversationLaneHost {
	var kinds []string
	if cfg := cs.Config(); cfg != nil {
		kinds = cfg.Ops.Authoritative
	}
	h := newConversationLaneHost(cs, kinds)
	h.start(ctx)
	cs.ledgerFeeds.setRowsChanged(h.pokeChangedRows)
	return h
}

// pokeChangedRows is the ledger-feed doorbell: it pokes, once each, the
// conversations whose lane op rows (gc:op), closed op rows or runtime row
// (gc:rt) are among rows, which a refresh pass found changed out of band
// (laneConversationsOfRows). A host that is not running ignores it.
func (h *conversationLaneHost) pokeChangedRows(rows []beads.Bead) {
	for _, conv := range laneConversationsOfRows(rows) {
		h.Poke(conv)
	}
}

// laneConversationsOfRows lists, in first-seen order, the conversations rows
// name through a runtime row's conversation label or a lane-relevant op
// row's: a lane op's, or a closed op's, which a lane op can wait on as its
// after. An open legacy op row rings nothing: no lane executes it, so a pass
// for it would read the session and the conversation's ops live for
// nothing. An op row that does not decode rings, so its pass reports it.
func laneConversationsOfRows(rows []beads.Bead) []string {
	opPrefix, rtPrefix := opjournal.ConversationLabel(""), opjournal.RuntimeConversationLabel("")
	var convs []string
	for _, b := range rows {
		for _, label := range b.Labels {
			conv, ok := strings.CutPrefix(label, rtPrefix)
			if !ok {
				conv, ok = strings.CutPrefix(label, opPrefix)
				ok = ok && laneOpRowRings(b)
			}
			if ok && conv != "" && !slices.Contains(convs, conv) {
				convs = append(convs, conv)
			}
		}
	}
	return convs
}

// laneOpRowRings reports whether op row b rings the doorbell: a lane op, a
// closed op, or a row that does not decode as an op.
func laneOpRowRings(b beads.Bead) bool {
	op, err := opjournal.DecodeOp(b)
	return err != nil || op.Executor == opjournal.ExecutorLane || op.Status == opjournal.StatusClosed
}

// start decides whether the host runs, publishes it for the city's socket
// verbs, and starts the scan loop unless the host is off or refused.
func (h *conversationLaneHost) start(ctx context.Context) {
	h.mu.Lock()
	h.ctx = ctx
	h.mu.Unlock()
	state := h.decide()
	registerLaneHost(ctx, h.env.CityPath(), h)
	if state == laneHostStateRunning || state == laneHostStateUnavailable {
		h.wg.Add(1)
		go h.run()
	}
}

// decide sets the host's state from the current sessions store, logging a
// refusal, and returns it.
func (h *conversationLaneHost) decide() string {
	err := laneStoreCapability(h.env.SessionsBeadStore().Store)
	h.mu.Lock()
	switch {
	case err == nil:
		h.state, h.reason = laneHostStateRunning, ""
	case errors.Is(err, errLaneStoreMissing):
		h.state, h.reason = laneHostStateUnavailable, err.Error()
	case len(h.kinds) > 0:
		h.state, h.reason = laneHostStateRefused, err.Error()
	default:
		h.state, h.reason = laneHostStateOff, err.Error()
	}
	state, reason := h.state, h.reason
	h.mu.Unlock()
	if state == laneHostStateRefused {
		h.logf("conversation-lanes: not started: %s", reason)
	}
	return state
}

// run is the anti-entropy loop: a boot step, then one step every
// scanInterval while a step says another is owed.
func (h *conversationLaneHost) run() {
	defer h.wg.Done()
	if !h.step() {
		return
	}
	ticks, stop := h.newTicker(h.scanInterval)
	defer stop()
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-ticks:
			if !h.step() {
				return
			}
		}
	}
}

// step is one turn of the loop: an unavailable host re-checks its store, a
// running one scans. It reports whether another turn is owed: the host is
// still waiting for its store, a kind is listed, or the scan left work.
func (h *conversationLaneHost) step() bool {
	state, _ := h.stateReason()
	if state == laneHostStateUnavailable {
		state = h.decide()
	}
	switch state {
	case laneHostStateUnavailable:
		return true
	case laneHostStateRunning:
		return h.scan() || len(h.kinds) > 0
	default:
		return false
	}
}

// scan pokes every conversation with an open lane op, and reports whether a
// later scan is owed: it found an open lane op (one whose gate clears with no
// doorbell of its own still drains), the city was quiescent, or the read
// failed or stopped at its page cap, which the next scan reads on from.
func (h *conversationLaneHost) scan() (left bool) {
	defer h.env.beginStoreWork()()
	defer func() {
		if r := recover(); r != nil {
			h.logf("conversation-lanes: scan panicked: %v\n%s", r, debug.Stack())
			left = true
		}
	}()
	if h.env.storesQuiescent() {
		return true
	}
	st, _, err := h.store()
	if err != nil {
		h.noteError("store", err)
		return true
	}
	convs, next, err := st.OpenLaneConversations(laneHostScanLimit, h.scanFrom)
	if err != nil {
		h.noteError("scan", err)
		return true
	}
	h.scanFrom = next
	for _, conv := range convs {
		h.Poke(conv)
	}
	return !next.Done() || len(convs) > 0
}

// Poke asks for a pass over conversation conv, starting its worker when it
// has none. It reports false when the host is not running.
func (h *conversationLaneHost) Poke(conv string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pokeLocked(conv)
}

// pokeLocked is Poke with h.mu held.
func (h *conversationLaneHost) pokeLocked(conv string) bool {
	if h.state != laneHostStateRunning || h.ctx == nil || h.ctx.Err() != nil {
		return false
	}
	if w := h.workers[conv]; w != nil {
		select {
		case w.poke <- struct{}{}:
		default:
		}
		return true
	}
	if len(h.workers) >= h.maxWorkers {
		if !h.inQueue[conv] {
			h.inQueue[conv] = true
			h.queued = append(h.queued, conv)
		}
		return true
	}
	h.startWorkerLocked(conv)
	return true
}

// startWorkerLocked starts conv's worker, from its parked state when it is
// parked, whose timer it stops. The caller holds h.mu.
func (h *conversationLaneHost) startWorkerLocked(conv string) {
	var state laneWorkerState
	if p := h.parked[conv]; p != nil {
		close(p.stop)
		delete(h.parked, conv)
		state = p.state
	}
	w := &laneWorker{poke: make(chan struct{}, 1)}
	h.workers[conv] = w
	h.wg.Add(1)
	go h.runWorker(conv, w, state)
}

// retire removes conv's worker unless force is false and a poke arrived
// since its last pass, parks conv when park is set, and starts the next
// queued conversation in its slot, in one hold of h.mu: a doorbell finds conv
// served or parked, never between the two. It reports whether the worker
// must exit.
func (h *conversationLaneHost) retire(conv string, w *laneWorker, force bool, park *lanePark) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !force {
		select {
		case <-w.poke:
			return false
		default:
		}
	}
	if h.workers[conv] == w {
		delete(h.workers, conv)
		if park != nil {
			h.parkLocked(conv, *park)
		}
	}
	for len(h.queued) > 0 && len(h.workers) < h.maxWorkers && h.ctx.Err() == nil {
		next := h.queued[0]
		h.queued = h.queued[1:]
		delete(h.inQueue, next)
		if h.workers[next] == nil {
			h.startWorkerLocked(next)
		}
	}
	return true
}

// yield is the host's slot policy: a worker gives up its slot for a wait
// longer than laneParkAfter, and for any wait while another conversation is
// queued for a slot.
func (h *conversationLaneHost) yield(d time.Duration) bool {
	if d > laneParkAfter {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return d > 0 && len(h.queued) > 0
}

// parkLocked pokes conv again after park.after without holding a worker
// slot, keeping park.state for its next worker, whichever doorbell starts
// it; a worker started for conv first stops the park. The caller holds h.mu
// and has just removed conv's worker, so conv has no park yet.
func (h *conversationLaneHost) parkLocked(conv string, park lanePark) {
	p := &laneParked{stop: make(chan struct{}), state: park.state}
	h.parked[conv] = p
	fire, stopTimer := h.newTimer(park.after)
	h.wg.Add(1)
	go h.awaitPark(conv, p, fire, stopTimer)
}

// awaitPark pokes conv when its park p fires, unless a worker started for
// conv first or the host ended. A poke that starts conv's worker hands it
// p's state; one that queues conv keeps p until the queue starts it, so the
// state waits in the queue with conv. A refused poke drops p.
func (h *conversationLaneHost) awaitPark(conv string, p *laneParked, fire <-chan time.Time, stopTimer func()) {
	defer h.wg.Done()
	defer stopTimer()
	select {
	case <-h.ctx.Done():
		return
	case <-p.stop:
		return
	case <-fire:
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.parked[conv] == p && !h.pokeLocked(conv) {
		delete(h.parked, conv)
	}
}

// runWorker serves conv in its slot until the worker exits.
func (h *conversationLaneHost) runWorker(conv string, w *laneWorker, state laneWorkerState) {
	defer h.wg.Done()
	if h.onWorkerExit != nil {
		defer h.onWorkerExit(conv)
	}
	cw := &conversationLaneWorker{
		ctx: h.ctx, clk: h.clk, newTimer: h.newTimer, poke: w.poke, state: state, startFor: h.wakeStartFor(),
		hooks: laneWorkerHooks{
			pass:    func() lanePassOutcome { return h.pass(conv) },
			yield:   h.yield,
			release: func(force bool, park *lanePark) bool { return h.retire(conv, w, force, park) },
			onPass: func(out lanePassOutcome) {
				if h.onPass != nil {
					h.onPass(conv, out)
				}
			},
			onReprobe: func(still bool) {
				if h.onReprobe != nil {
					h.onReprobe(conv, still)
				}
			},
		},
	}
	cw.run()
}

// wakeStartFor is how long a woken session may take to start: the city's
// [session] startup_timeout.
func (h *conversationLaneHost) wakeStartFor() time.Duration {
	if cfg := h.env.Config(); cfg != nil {
		return cfg.Session.StartupTimeoutDuration()
	}
	return (&config.SessionConfig{}).StartupTimeoutDuration()
}

// laneReprober is an executor that can look at the runtime again without
// reading any row (conversationLaneExec.Reprobe).
type laneReprober interface {
	Reprobe(ctx context.Context, conv opjournal.ConversationView) (opjournal.Probe, error)
}

// pass runs one pass over conv as store work, recovering a panic; a
// quiescent city is not read.
func (h *conversationLaneHost) pass(conv string) (out lanePassOutcome) {
	defer h.env.beginStoreWork()()
	defer func() {
		if r := recover(); r != nil {
			out = lanePassOutcome{err: fmt.Errorf("pass over %s panicked: %v", conv, r)}
			h.logf("conversation-lanes: pass over %s panicked: %v\n%s", conv, r, debug.Stack())
			h.notePass(conv, opjournal.PassResult{}, out.err)
		}
	}()
	if h.env.storesQuiescent() {
		return lanePassOutcome{skipped: true}
	}
	st, lane, err := h.store()
	if err != nil {
		h.noteError("store", err)
		return lanePassOutcome{err: err}
	}
	sessions := h.env.SessionsBeadStore().Store
	live := sessionFrontDoor(laneLiveSessions{Store: sessions})
	view, info, absent, err := readConversationLaneView(live, conv)
	if err != nil {
		err = fmt.Errorf("reading conversation %s: %w", conv, err)
		h.notePass(conv, opjournal.PassResult{}, err)
		return lanePassOutcome{err: err}
	}
	if absent {
		h.logf("conversation-lanes: conversation %s has no session row on two reads; its open ops close as skipped{conversation_closed}", conv)
	}
	exec := h.newExec(laneExecDeps{
		cityPath: h.env.CityPath(),
		cfg:      h.env.Config(),
		sessions: sessions,
		sp:       h.env.SessionProvider(),
		lane:     st,
		clk:      h.clk,
		note:     h.noteError,
	}, info)
	res, err := lane.Pass(h.ctx, view, exec)
	h.notePass(conv, res, err)
	out = lanePassOutcome{res: res, err: err}
	if err == nil && res.Wake && !view.Closed {
		out.wake, out.wakeErr = h.requestWake(live, conv)
		h.noteWake(conv, out.wake)
	}
	if rp, ok := exec.(laneReprober); ok && err == nil && res.Busy && !res.Wake {
		out.reprobe = func() bool { return h.reprobe(conv, view, rp) }
	}
	return out
}

// reprobe looks at conv's busy runtime again through rp as store work, and
// reports whether it is still busy. A quiescent city, a failure or a panic
// is not still busy: the worker's next full pass sees what changed.
func (h *conversationLaneHost) reprobe(conv string, view opjournal.ConversationView, rp laneReprober) (still bool) {
	defer h.env.beginStoreWork()()
	defer func() {
		if r := recover(); r != nil {
			h.logf("conversation-lanes: re-probe of %s panicked: %v\n%s", conv, r, debug.Stack())
			still = false
		}
	}()
	if h.env.storesQuiescent() {
		return false
	}
	h.mu.Lock()
	h.reprobes++
	h.mu.Unlock()
	p, err := rp.Reprobe(h.ctx, view)
	if err != nil {
		h.noteError("reprobe", fmt.Errorf("re-probing conversation %s: %w", conv, err))
		return false
	}
	return laneProbeStillBusy(p)
}

// requestWake asks for conv's wake (DES-O1 §7.4) and, when one is recorded,
// asks the controller to reconcile the session now. A held row keeps its op
// waiting. live reads the row as the view did, so a cache that still holds
// it awake does not refuse the wake as not dormant.
func (h *conversationLaneHost) requestWake(live *session.Store, conv string) (session.WakeRequestOutcome, error) {
	outcome, err := live.RequestWakeUnlessHeld(conv, false, h.clk.Now().UTC())
	if err != nil {
		err = fmt.Errorf("requesting a wake of %s: %w", conv, err)
		h.noteError("wake", err)
		return 0, err
	}
	if outcome == session.WakeRecorded {
		h.env.Enqueue(reconcilekey.Allocator(), reconcilekey.Session(conv))
	}
	return outcome, nil
}

// store returns the cached op store and lane, opening them over the current
// sessions store when none is cached or that store was replaced.
func (h *conversationLaneHost) store() (*opjournal.Store, *opjournal.ConversationLane, error) {
	over := h.env.SessionsBeadStore().Store
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.st != nil && sameBeadStore(h.stOver, over) {
		return h.st, h.lane, nil
	}
	h.st, h.lane, h.stOver = nil, nil, nil
	st, err := opjournal.NewLaneStore(beads.SessionStore{Store: laneLiveSessions{Store: over}}, h.clk)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the lane store: %w", err)
	}
	lane, err := opjournal.NewLane(st, laneHostName, h.clk)
	if err != nil {
		return nil, nil, err
	}
	h.st, h.lane, h.stOver = st, lane, over
	return st, lane, nil
}

// notePass records a pass in the status, and logs a failed one.
func (h *conversationLaneHost) notePass(conv string, res opjournal.PassResult, err error) {
	now := h.clk.Now()
	h.mu.Lock()
	h.passes++
	h.lastPass = now
	if len(res.Quarantined) > 0 {
		ids := make([]string, len(res.Quarantined))
		for i, q := range res.Quarantined {
			ids[i] = q.ID
		}
		h.quarantined[conv] = ids
	} else if err == nil {
		delete(h.quarantined, conv)
	}
	if err == nil {
		h.lastSuccess, h.lastFailed, h.sinceSuccess = now, false, 0
		if len(res.Waits) > 0 {
			h.waits[conv] = laneWaitsStatus(conv, res.Waits)
		} else {
			delete(h.waits, conv)
		}
	}
	h.mu.Unlock()
	if err != nil {
		h.noteFailure("pass", err, true)
	}
}

// noteWake records the answer to conv's wake request beside its waits.
func (h *conversationLaneHost) noteWake(conv string, outcome session.WakeRequestOutcome) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if w, ok := h.waits[conv]; ok {
		w.Wake = laneWakeName(outcome)
		h.waits[conv] = w
	}
}

// noteError records and logs a failure outside a pass.
func (h *conversationLaneHost) noteError(stage string, err error) {
	h.noteFailure(stage, err, false)
}

// noteFailure records a failure in the status, and logs it at most once per
// laneErrorLogInterval per stage.
func (h *conversationLaneHost) noteFailure(stage string, err error, pass bool) {
	now := h.clk.Now()
	h.mu.Lock()
	h.passErrors++
	h.sinceSuccess++
	h.lastErr = err.Error()
	if pass {
		h.lastFailed = true
	}
	due := true
	if last, seen := h.logged[stage]; seen && now.Before(last.Add(laneErrorLogInterval)) {
		due = false
	} else {
		h.logged[stage] = now
	}
	h.mu.Unlock()
	if due {
		h.logf("conversation-lanes: %s failed: %v", stage, err)
	}
}

// laneConversationWaits is one conversation's waits in the status: why its
// ops wait after its last pass, and the answer to the wake it asked for.
type laneConversationWaits struct {
	Conversation string           `json:"conversation"`
	Waits        []laneOpWaitNote `json:"waits"`
	// Wake is recorded, held or not_dormant, "" when the pass asked none.
	Wake string `json:"wake,omitempty"`
}

// laneOpWaitNote is one op's wait.
type laneOpWaitNote struct {
	Op     string `json:"op"`
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
	Until  string `json:"until,omitempty"`
}

// laneWaitsStatus is conv's waits as the status lists them, at most
// laneStatusWaits.
func laneWaitsStatus(conv string, waits []opjournal.OpWait) laneConversationWaits {
	out := laneConversationWaits{Conversation: conv}
	for _, w := range waits[:min(len(waits), laneStatusWaits)] {
		n := laneOpWaitNote{Op: w.ID, Reason: string(w.Reason), Detail: w.Detail}
		if !w.Until.IsZero() {
			n.Until = w.Until.UTC().Format(time.RFC3339)
		}
		out.Waits = append(out.Waits, n)
	}
	return out
}

// laneWakeName names a wake request's answer.
func laneWakeName(o session.WakeRequestOutcome) string {
	switch o {
	case session.WakeRecorded:
		return "recorded"
	case session.WakeHeld:
		return "held"
	case session.WakeNotDormant:
		return "not_dormant"
	}
	return ""
}

// conversationLaneStatus is the lane host's status, the lane-status verb's
// reply.
type conversationLaneStatus struct {
	State  string   `json:"state"`
	Reason string   `json:"reason,omitempty"`
	Kinds  []string `json:"kinds,omitempty"`
	// Workers are the conversations being served; Queued wait for a slot;
	// Parked gave theirs up and wait, on a timer or in the queue, for their
	// next worker, so a conversation can count in both Queued and Parked.
	Workers  int   `json:"workers"`
	Queued   int   `json:"queued"`
	Parked   int   `json:"parked"`
	Passes   int64 `json:"passes"`
	Reprobes int64 `json:"reprobes"`
	// LastPassAt is the last pass's time, "" before the first;
	// LastSuccessAt the last pass that succeeded.
	LastPassAt       string `json:"last_pass_at,omitempty"`
	LastPassAgeMS    int64  `json:"last_pass_age_ms,omitempty"`
	LastSuccessAt    string `json:"last_success_at,omitempty"`
	LastSuccessAgeMS int64  `json:"last_success_age_ms,omitempty"`
	// LastPassFailed reports that the last pass failed; ErrorsSinceSuccess
	// counts the failures, of passes and of scans, wake requests and row
	// refreshes, since the last pass that succeeded.
	LastPassFailed     bool   `json:"last_pass_failed,omitempty"`
	Errors             int64  `json:"errors"`
	ErrorsSinceSuccess int64  `json:"errors_since_success"`
	LastError          string `json:"last_error,omitempty"`
	// Quarantined counts the op rows the last pass of each conversation
	// could not decode; QuarantinedIDs lists the first few.
	Quarantined    int      `json:"quarantined"`
	QuarantinedIDs []string `json:"quarantined_ids,omitempty"`
	// Waiting lists, for the first few conversations in id order, why
	// their ops wait after their last pass.
	Waiting []laneConversationWaits `json:"waiting,omitempty"`
}

// status reads the host's status at now.
func (h *conversationLaneHost) status(now time.Time) conversationLaneStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := conversationLaneStatus{
		State: h.state, Reason: h.reason, Kinds: append([]string(nil), h.kinds...),
		Workers: len(h.workers), Queued: len(h.queued), Parked: len(h.parked), Passes: h.passes, Reprobes: h.reprobes,
		LastPassFailed: h.lastFailed, Errors: h.passErrors, ErrorsSinceSuccess: h.sinceSuccess, LastError: h.lastErr,
	}
	if !h.lastPass.IsZero() {
		st.LastPassAt = h.lastPass.UTC().Format(time.RFC3339Nano)
		st.LastPassAgeMS = max(now.Sub(h.lastPass).Milliseconds(), 0)
	}
	if !h.lastSuccess.IsZero() {
		st.LastSuccessAt = h.lastSuccess.UTC().Format(time.RFC3339Nano)
		st.LastSuccessAgeMS = max(now.Sub(h.lastSuccess).Milliseconds(), 0)
	}
	for _, conv := range slices.Sorted(maps.Keys(h.quarantined)) {
		ids := h.quarantined[conv]
		st.Quarantined += len(ids)
		for _, id := range ids {
			if len(st.QuarantinedIDs) < laneStatusIDs {
				st.QuarantinedIDs = append(st.QuarantinedIDs, id)
			}
		}
	}
	for _, conv := range slices.Sorted(maps.Keys(h.waits)) {
		if len(st.Waiting) == laneStatusIDs {
			break
		}
		st.Waiting = append(st.Waiting, h.waits[conv])
	}
	return st
}

// conversationLaneHosts maps a normalized city path to its controller's lane
// host, so the socket handler, which knows only the city path, can reach it.
var conversationLaneHosts sync.Map // normalized city path → *conversationLaneHost

// registerLaneHost publishes h for cityPath until ctx ends.
func registerLaneHost(ctx context.Context, cityPath string, h *conversationLaneHost) {
	key := normalizePathForCompare(cityPath)
	conversationLaneHosts.Store(key, h)
	if ctx == nil || ctx.Done() == nil {
		return
	}
	go func() {
		<-ctx.Done()
		conversationLaneHosts.CompareAndDelete(key, h)
	}()
}

// conversationLaneHostFor returns cityPath's lane host, nil when none runs.
func conversationLaneHostFor(cityPath string) *conversationLaneHost {
	v, ok := conversationLaneHosts.Load(normalizePathForCompare(cityPath))
	if !ok {
		return nil
	}
	return v.(*conversationLaneHost)
}

// conversationLaneStatusFor reads cityPath's lane host at now.
func conversationLaneStatusFor(cityPath string, now time.Time) conversationLaneStatus {
	h := conversationLaneHostFor(cityPath)
	if h == nil {
		return conversationLaneStatus{State: laneHostStateNone}
	}
	return h.status(now)
}

// laneSocketIDRe is the shape of a conversation or op id on the lane: verb.
var laneSocketIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)

// laneSocketReply answers lane:<conversation>[:<op id>]: it validates the ids,
// refreshes the op's row in the sessions cache when one is named, and pokes
// the conversation. The reply is ok, no-host, or refused <reason>.
func laneSocketReply(cityPath, payload string) string {
	conv, op, hasOp := strings.Cut(payload, ":")
	switch {
	case !laneSocketIDRe.MatchString(conv):
		return "refused invalid conversation id"
	case hasOp && !laneSocketIDRe.MatchString(op):
		return "refused invalid op id"
	}
	h := conversationLaneHostFor(cityPath)
	if h == nil {
		return "no-host"
	}
	if state, reason := h.stateReason(); state != laneHostStateRunning {
		if reason == "" {
			reason = "conversation lanes are " + state
		}
		return "refused " + reason
	}
	if hasOp {
		h.refreshRow(op)
	}
	if !h.Poke(conv) {
		return "refused the lane host is stopping"
	}
	return "ok"
}

// stateReason reads the host's state and its reason.
func (h *conversationLaneHost) stateReason() (string, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state, h.reason
}

// refreshRow re-reads op id into the sessions cache, so a producer's write
// from another process is visible to the controller's readers at once. A
// quiescent city is not read; a row that is gone is nothing to refresh.
func (h *conversationLaneHost) refreshRow(id string) {
	defer h.env.beginStoreWork()()
	if h.env.storesQuiescent() {
		return
	}
	cache := laneSessionsCache(h.env.SessionsBeadStore().Store)
	if cache == nil {
		return
	}
	if _, err := cache.RefreshRow(id); err != nil && !errors.Is(err, beads.ErrNotFound) {
		h.noteError("refresh", fmt.Errorf("refreshing op %s: %w", id, err))
	}
}

// laneSessionsCache is the controller's cache over the sessions store, nil
// when the store is not cached.
func laneSessionsCache(store beads.Store) *beads.CachingStore {
	cur := store
	for range 8 {
		if cache, ok := cur.(*beads.CachingStore); ok {
			return cache
		}
		t, ok := cur.(beads.ConditionalWritesResolveTargeter)
		if !ok {
			return nil
		}
		next := t.ConditionalWritesResolveTarget()
		if isNilBeadStore(next) {
			return nil
		}
		cur = next
	}
	return nil
}
