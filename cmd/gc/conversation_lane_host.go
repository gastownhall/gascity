package main

// The controller's conversation-lane host (DES-O1 §7): it runs
// opjournal.ConversationLane passes for every conversation with open lane
// ops, one worker goroutine per conversation (at most
// laneHostMaxWorkers), each calling Pass serially with the live executor
// (conversation_lane_exec.go).
//
// Doorbells (DES-O1 §7.3) start or wake a conversation's worker: Poke for
// in-process producers, the controller-socket verb lane:<conversation>[:<op>],
// each worker's own timer at its PassResult.Next, and the anti-entropy scan
// (opjournal.Store.OpenLaneConversations). With [ops] authoritative empty the
// host runs one boot scan and starts no ticker, and no worker unless that
// scan finds an open lane op; afterwards only a poke wakes it, so an idle city
// pays one keyed scan per controller start. With a kind listed the scan runs
// every laneHostScanInterval. The list is latched at controller start.
//
// The host runs whenever the sessions store can apply guarded batches,
// whatever [ops] authoritative says, so lane ops already open drain after a
// rollback (DES-O1 §12). A store that cannot keeps the host off: silently
// with the list empty, and with a kind listed with one logged refusal
// (errLaneStoreIncapable) that the conversation-lanes doctor check reports.
//
// Reads go live (laneLiveSessions), writes and guarded batches go through the
// sessions store, so the controller's cache and its event bridge see them.
// Every pass runs as store work and is skipped while the city is quiescent.
// The host emits no events of its own.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"regexp"
	"runtime/debug"
	"sort"
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
	// laneHostScanLimit caps each of the scan's three keyed lists.
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
	// laneWakeRecheck and laneWakeHeldRecheck are how long a worker waits
	// after asking for a wake before it passes again, recorded or held.
	laneWakeRecheck     = 5 * time.Second
	laneWakeHeldRecheck = time.Minute
	// laneParkAfter is the longest a worker holds its slot waiting for its
	// next pass; a longer wait parks the conversation on a timer instead, so
	// conversations whose ops wait for hours (not_live, a far not_before)
	// cannot fill every slot.
	laneParkAfter = time.Minute
	// laneStatusIDs caps the quarantined ids the status lists.
	laneStatusIDs = 8
)

// Lane host states, as lane-status reports them.
const (
	laneHostStateRunning = "running"
	laneHostStateOff     = "off"
	laneHostStateRefused = "refused"
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

// laneStoreCapability reports whether store can host conversation lanes,
// and the typed refusal when it cannot.
func laneStoreCapability(store beads.Store) error {
	if isNilBeadStore(store) {
		return &laneStoreIncapableError{binding: "no store"}
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
		t, ok := cur.(beads.ConditionalWritesResolveTargeter)
		if !ok {
			break
		}
		next := t.ConditionalWritesResolveTarget()
		if isNilBeadStore(next) {
			break
		}
		cur = next
	}
	return fmt.Sprintf("a store without guarded batches (%T)", cur)
}

// isNilBeadStore reports a nil store, or a nil pointer in a store interface.
func isNilBeadStore(s beads.Store) bool {
	if s == nil {
		return true
	}
	v := reflect.ValueOf(s)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

// laneLiveSessions is the sessions store as the lane host reads it: Get and
// List read live, past any cache, and every write and guarded batch goes
// through the wrapped store, so the cache and its event bridge see them.
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
	// onPass and onWorkerExit are test seams: each pass's outcome, and each
	// worker's exit. Nil in production.
	onPass       func(conv string, out lanePassOutcome)
	onWorkerExit func(conv string)

	ctx context.Context
	wg  sync.WaitGroup

	mu      sync.Mutex
	state   string
	reason  string
	st      *opjournal.Store
	lane    *opjournal.ConversationLane
	stOver  beads.Store
	workers map[string]*laneWorker
	queued  []string
	inQueue map[string]bool
	// parked holds the stop of each parked conversation's timer.
	parked map[string]chan struct{}
	// status counters
	passes      int64
	passErrors  int64
	lastPass    time.Time
	lastErr     string
	quarantined map[string][]string
	tickers     int
}

// laneWorker is one conversation's worker; poke wakes it.
type laneWorker struct {
	poke chan struct{}
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
		parked:      map[string]chan struct{}{},
		quarantined: map[string][]string{},
	}
}

// startConversationLaneHost starts the controller's lane host until ctx
// ends, with [ops] authoritative as the controller loaded it at start.
func (cs *controllerState) startConversationLaneHost(ctx context.Context) *conversationLaneHost {
	var kinds []string
	if cfg := cs.Config(); cfg != nil {
		kinds = cfg.Ops.Authoritative
	}
	h := newConversationLaneHost(cs, kinds)
	h.start(ctx)
	return h
}

// start decides whether the host runs, publishes it for the city's socket
// verbs, and starts the scan loop when it runs.
func (h *conversationLaneHost) start(ctx context.Context) {
	h.mu.Lock()
	h.ctx = ctx
	switch err := laneStoreCapability(h.env.SessionsBeadStore().Store); {
	case err == nil:
		h.state = laneHostStateRunning
	case len(h.kinds) > 0:
		h.state, h.reason = laneHostStateRefused, err.Error()
	default:
		h.state, h.reason = laneHostStateOff, err.Error()
	}
	state, reason := h.state, h.reason
	h.mu.Unlock()
	registerLaneHost(ctx, h.env.CityPath(), h)
	switch state {
	case laneHostStateRefused:
		h.logf("conversation-lanes: not started: %s", reason)
	case laneHostStateRunning:
		h.wg.Add(1)
		go h.run()
	}
}

// run is the anti-entropy loop: a boot scan, then a scan every scanInterval
// while a kind is listed or the last scan left work.
func (h *conversationLaneHost) run() {
	defer h.wg.Done()
	if !h.scan() && len(h.kinds) == 0 {
		return
	}
	ticks, stop := h.newTicker(h.scanInterval)
	h.mu.Lock()
	h.tickers++
	h.mu.Unlock()
	defer stop()
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-ticks:
			if !h.scan() && len(h.kinds) == 0 {
				return
			}
		}
	}
}

// scan pokes every conversation with an open lane op, and reports whether a
// later scan is owed: the city was quiescent, the read failed, or it was
// truncated.
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
		h.noteError(err)
		return true
	}
	convs, truncated, err := st.OpenLaneConversations(laneHostScanLimit)
	if err != nil {
		h.noteError(err)
		return true
	}
	for _, conv := range convs {
		h.Poke(conv)
	}
	return truncated
}

// Poke asks for a pass over conversation conv, starting its worker when it
// has none. It reports false when the host is not running.
func (h *conversationLaneHost) Poke(conv string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
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

// startWorkerLocked starts conv's worker. The caller holds h.mu.
func (h *conversationLaneHost) startWorkerLocked(conv string) {
	w := &laneWorker{poke: make(chan struct{}, 1)}
	h.workers[conv] = w
	h.wg.Add(1)
	go h.runWorker(conv, w)
}

// retire removes conv's worker unless a poke arrived since its last pass,
// and starts the next queued conversation in its slot. It reports whether
// the worker must exit.
func (h *conversationLaneHost) retire(conv string, w *laneWorker, force bool) bool {
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

// park pokes conv again after d without holding a worker slot. A newer park
// of conv replaces an older one.
func (h *conversationLaneHost) park(conv string, d time.Duration) {
	stop := make(chan struct{})
	h.mu.Lock()
	if old := h.parked[conv]; old != nil {
		close(old)
	}
	h.parked[conv] = stop
	h.wg.Add(1)
	h.mu.Unlock()
	fire, stopTimer := h.newTimer(d)
	go func() {
		defer h.wg.Done()
		defer stopTimer()
		select {
		case <-h.ctx.Done():
			return
		case <-stop:
			return
		case <-fire:
		}
		h.mu.Lock()
		if h.parked[conv] == stop {
			delete(h.parked, conv)
		}
		h.mu.Unlock()
		h.Poke(conv)
	}()
}

// laneWorkerState is what a worker carries from pass to pass.
type laneWorkerState struct {
	busySince time.Time
	errors    int
}

// runWorker passes over conv until nothing is left to do, then exits.
func (h *conversationLaneHost) runWorker(conv string, w *laneWorker) {
	defer h.wg.Done()
	if h.onWorkerExit != nil {
		defer h.onWorkerExit(conv)
	}
	var ws laneWorkerState
	for {
		out := h.pass(conv)
		if h.onPass != nil {
			h.onPass(conv, out)
		}
		delay, idle := h.nextDelay(out, &ws)
		if idle || delay > laneParkAfter {
			if h.retire(conv, w, false) {
				if !idle {
					h.park(conv, delay)
				}
				return
			}
			continue
		}
		if !h.wait(w, delay) {
			h.retire(conv, w, true)
			return
		}
	}
}

// wait sleeps for d, or until a poke; it reports false when the host ends.
func (h *conversationLaneHost) wait(w *laneWorker, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-h.ctx.Done():
			return false
		default:
			return true
		}
	}
	fire, stop := h.newTimer(d)
	defer stop()
	select {
	case <-h.ctx.Done():
		return false
	case <-w.poke:
		return true
	case <-fire:
		return true
	}
}

// lanePassOutcome is one worker pass.
type lanePassOutcome struct {
	res     opjournal.PassResult
	err     error
	skipped bool
	wake    session.WakeRequestOutcome
}

// pass runs one pass over conv as store work, recovering a panic; a
// quiescent city is not read.
func (h *conversationLaneHost) pass(conv string) (out lanePassOutcome) {
	defer h.env.beginStoreWork()()
	defer func() {
		if r := recover(); r != nil {
			out = lanePassOutcome{err: fmt.Errorf("pass over %s panicked: %v", conv, r)}
			h.logf("conversation-lanes: pass over %s panicked: %v\n%s", conv, r, debug.Stack())
			h.noteError(out.err)
		}
	}()
	if h.env.storesQuiescent() {
		return lanePassOutcome{skipped: true}
	}
	st, lane, err := h.store()
	if err != nil {
		h.noteError(err)
		return lanePassOutcome{err: err}
	}
	sessions := h.env.SessionsBeadStore().Store
	view, info, err := readConversationLaneView(sessionFrontDoor(laneLiveSessions{Store: sessions}), conv)
	if err != nil {
		err = fmt.Errorf("reading conversation %s: %w", conv, err)
		h.noteError(err)
		return lanePassOutcome{err: err}
	}
	exec := h.newExec(laneExecDeps{
		cityPath: h.env.CityPath(),
		cfg:      h.env.Config(),
		sessions: laneLiveSessions{Store: sessions},
		sp:       h.env.SessionProvider(),
		lane:     st,
		clk:      h.clk,
	}, info)
	res, err := lane.Pass(h.ctx, view, exec)
	h.notePass(conv, res, err)
	out = lanePassOutcome{res: res, err: err}
	if err == nil && res.Wake && !view.Closed {
		out.wake = h.requestWake(sessions, conv)
	}
	return out
}

// requestWake asks for conv's wake (DES-O1 §7.4) and, when one is recorded,
// asks the controller to reconcile the session now. A held row keeps its op
// waiting.
func (h *conversationLaneHost) requestWake(sessions beads.Store, conv string) session.WakeRequestOutcome {
	outcome, err := sessionFrontDoor(sessions).RequestWakeUnlessHeld(conv, false, h.clk.Now().UTC())
	if err != nil {
		h.noteError(fmt.Errorf("requesting a wake of %s: %w", conv, err))
		return 0
	}
	if outcome == session.WakeRecorded {
		h.env.Enqueue(reconcilekey.Allocator(), reconcilekey.Session(conv))
	}
	return outcome
}

// nextDelay is how long a worker waits after out, or idle when nothing it
// can see will change by itself.
func (h *conversationLaneHost) nextDelay(out lanePassOutcome, ws *laneWorkerState) (time.Duration, bool) {
	now := h.clk.Now()
	switch {
	case out.skipped:
		return laneQuiescentRetry, false
	case out.err != nil:
		ws.errors++
		d := laneErrBackoffStart << min(ws.errors-1, 10)
		return min(d, laneErrBackoffCap), false
	}
	ws.errors = 0
	res := out.res
	// A pass that took something planned its waits and Next before the
	// Take moved the conversation: pass again at once to re-plan.
	if res.More || res.Took != opjournal.TakeNone {
		return 0, false
	}
	var delay time.Duration
	have := false
	take := func(d time.Duration) {
		if !have || d < delay {
			delay, have = max(d, 0), true
		}
	}
	if res.Busy || laneWaitsFor(res, opjournal.WaitUnavailable) {
		if ws.busySince.IsZero() {
			ws.busySince = now
		}
		switch since := now.Sub(ws.busySince); {
		case since < laneBusyFirstFor:
			take(laneBusyFirst)
		case since < laneBusyNextFor:
			take(laneBusyNext)
		default:
			take(laneBusyMax)
		}
	} else {
		ws.busySince = time.Time{}
	}
	switch {
	case out.wake == session.WakeHeld:
		take(laneWakeHeldRecheck)
	case res.Wake:
		take(laneWakeRecheck)
	}
	if !res.Next.IsZero() {
		take(res.Next.Sub(now))
	}
	return delay, !have
}

// laneWaitsFor reports whether any op of res waits for reason r.
func laneWaitsFor(res opjournal.PassResult, r opjournal.WaitReason) bool {
	for _, w := range res.Waits {
		if w.Reason == r {
			return true
		}
	}
	return false
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

// notePass records a pass in the status.
func (h *conversationLaneHost) notePass(conv string, res opjournal.PassResult, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.passes++
	h.lastPass = h.clk.Now()
	if len(res.Quarantined) > 0 {
		ids := make([]string, len(res.Quarantined))
		for i, q := range res.Quarantined {
			ids[i] = q.ID
		}
		h.quarantined[conv] = ids
	} else if err == nil {
		delete(h.quarantined, conv)
	}
	if err != nil {
		h.passErrors++
		h.lastErr = err.Error()
	}
}

// noteError records a failure outside a pass.
func (h *conversationLaneHost) noteError(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.passErrors++
	h.lastErr = err.Error()
}

// conversationLaneStatus is the lane host's status, the lane-status verb's
// reply.
type conversationLaneStatus struct {
	State  string   `json:"state"`
	Reason string   `json:"reason,omitempty"`
	Kinds  []string `json:"kinds,omitempty"`
	// Workers are the conversations being served; Queued wait for a slot;
	// Parked wait on a timer longer than a worker holds its slot.
	Workers int   `json:"workers"`
	Queued  int   `json:"queued"`
	Parked  int   `json:"parked"`
	Passes  int64 `json:"passes"`
	// LastPassAt is the last pass's time, "" before the first.
	LastPassAt    string `json:"last_pass_at,omitempty"`
	LastPassAgeMS int64  `json:"last_pass_age_ms,omitempty"`
	Errors        int64  `json:"errors"`
	LastError     string `json:"last_error,omitempty"`
	// Quarantined counts the op rows the last pass of each conversation
	// could not decode; QuarantinedIDs lists the first few.
	Quarantined    int      `json:"quarantined"`
	QuarantinedIDs []string `json:"quarantined_ids,omitempty"`
}

// status reads the host's status at now.
func (h *conversationLaneHost) status(now time.Time) conversationLaneStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := conversationLaneStatus{
		State: h.state, Reason: h.reason, Kinds: append([]string(nil), h.kinds...),
		Workers: len(h.workers), Queued: len(h.queued), Parked: len(h.parked), Passes: h.passes,
		Errors: h.passErrors, LastError: h.lastErr,
	}
	if !h.lastPass.IsZero() {
		st.LastPassAt = h.lastPass.UTC().Format(time.RFC3339Nano)
		st.LastPassAgeMS = max(now.Sub(h.lastPass).Milliseconds(), 0)
	}
	convs := make([]string, 0, len(h.quarantined))
	for conv := range h.quarantined {
		convs = append(convs, conv)
	}
	sort.Strings(convs)
	for _, conv := range convs {
		ids := h.quarantined[conv]
		st.Quarantined += len(ids)
		for _, id := range ids {
			if len(st.QuarantinedIDs) < laneStatusIDs {
				st.QuarantinedIDs = append(st.QuarantinedIDs, id)
			}
		}
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
		h.noteError(fmt.Errorf("refreshing op %s: %w", id, err))
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
