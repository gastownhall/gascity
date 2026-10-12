package workqueue

import (
	"context"
	"math/rand/v2"
	"sort"
	"sync"
	"time"
)

// Lane is one of the queue's two FIFO lanes. A lower value is the stronger
// lane: a key added on both is served from the hot one.
type Lane uint8

// The lanes. Get serves LaneHot first and LaneResync every AgingEvery-th
// dequeue.
const (
	LaneHot    Lane = iota // events, timers, API, snapshot diffs
	LaneResync             // boot enqueue-all and periodic resync
	laneCount
)

// Reason says why a key was added. Kind is a short code from a closed set
// (Stats.Adds is keyed by it, so free-form Kinds grow without bound); Detail
// is free text for traces. Urgent reasons bypass the backoff gate.
type Reason struct {
	Kind   string
	Detail string
	Urgent bool
}

// MaxReasons caps the distinct reason Kinds an item carries; the rest are
// counted in Item.Dropped.
const MaxReasons = 8

// Item is one dequeued key and everything merged into it while it waited.
//
// Now().Sub(AddedAt) at Get is enqueue-to-start latency; latency metrics
// should exclude items with Failures > 0, whose wait includes backoff.
type Item[K comparable] struct {
	Key      K
	Lane     Lane      // lane it was dequeued from
	Reasons  []Reason  // merged, deduped by Kind, at most MaxReasons
	Dropped  int       // reasons beyond MaxReasons
	Urgent   bool      // some merged reason was urgent, even one past MaxReasons
	AddedAt  time.Time // first Add since the key last left the queue
	FirstSeq uint64    // seq of that first Add (latency, trace correlation)
	LastSeq  uint64    // seq of the latest Add merged into this item (coverage)
	Failures int       // consecutive failures at dequeue
}

// Defaults for zero Config fields.
const (
	defaultBaseBackoff = 500 * time.Millisecond
	defaultMaxBackoff  = 5 * time.Minute
	defaultAgingEvery  = 8
)

// Config tunes a Queue. Zero BaseBackoff, MaxBackoff and AgingEvery take the
// session-key defaults (500ms, 5m, 8); a zero Jitter means none.
type Config[K comparable] struct {
	BaseBackoff, MaxBackoff time.Duration // backoff is min(base·2^(failures-1), max)·(1 ± Jitter)
	Jitter                  float64       // fraction, e.g. 0.1 for ±10%
	AgingEvery              int           // every AgingEvery-th dequeue serves the resync lane
	// Now defaults to time.Now. Timers run on the time package's clock, so
	// an injected Now must read the same clock.
	Now func() time.Time
	// Rand returns a value in [0, 1) for jitter; defaults to math/rand/v2.
	Rand func() float64
}

// Stats is a point-in-time snapshot of the queue.
type Stats struct {
	Keys           int                  // keys with any state: queued, processing, deferred, timer or failure record
	Depth          [laneCount]int       // queued keys, indexed by Lane
	Oldest         [laneCount]time.Time // earliest AddedAt among queued keys, indexed by Lane; zero if empty
	Processing     int                  // keys handed out by Get and not yet Done
	OldestInFlight time.Time            // earliest Get among processing keys (longest in flight); zero if none
	Dirty          int                  // processing keys added again since Get
	Deferred       int                  // keys with a deferred record: retry reasons and gated adds waiting for retryAt
	Timers         int                  // keys with a pending timer
	Adds           map[string]uint64    // accepted Add, AddAfter and AddRateLimited calls by reason Kind
	Dropped        uint64               // calls refused after ShutDown
	Holds          []string             // active holds, sorted
}

// pending is a set of merged adds that has not been handed out yet.
type pending struct {
	reasons  []Reason
	dropped  int
	urgent   bool
	addedAt  time.Time
	firstSeq uint64
	lastSeq  uint64
}

func (p *pending) empty() bool { return len(p.reasons) == 0 }

func (p *pending) addReason(r Reason) {
	p.urgent = p.urgent || r.Urgent
	for i := range p.reasons {
		if p.reasons[i].Kind == r.Kind {
			p.reasons[i].Urgent = p.reasons[i].Urgent || r.Urgent
			return
		}
	}
	if len(p.reasons) == MaxReasons {
		p.dropped++
		return
	}
	p.reasons = append(p.reasons, r)
}

// merge folds src into p. p keeps its own AddedAt and FirstSeq if it has
// them.
func (p *pending) merge(src pending) {
	if p.firstSeq == 0 {
		p.addedAt, p.firstSeq = src.addedAt, src.firstSeq
	}
	for _, r := range src.reasons {
		p.addReason(r)
	}
	p.dropped += src.dropped
	p.urgent = p.urgent || src.urgent
	p.lastSeq = max(p.lastSeq, src.lastSeq)
}

type keyState uint8

const (
	stateIdle keyState = iota
	stateQueued
	stateProcessing
)

type entry[K comparable] struct {
	state   keyState
	lane    Lane      // queued: its lane; processing: the lane of the dirty requeue
	gen     uint64    // matches the key's live FIFO slot while queued
	next    pending   // the queued item, or the dirty record while processing
	started time.Time // when Get handed the key out, while processing

	deferred pending   // retry reasons and adds held back by the backoff gate
	failures int       // consecutive failures since the last Forget
	retryAt  time.Time // the gate is closed until retryAt; zero when open

	timer     *time.Timer
	timerAt   time.Time
	timerID   uint64  // identifies the live timer, so a stale firing is ignored
	timerNext pending // reasons the timer adds when it fires
}

// live reports whether a FIFO slot still stands for this queued key.
// Promotion and dequeue leave stale slots behind instead of searching for
// them.
func (e *entry[K]) live(lane Lane, gen uint64) bool {
	return e != nil && e.state == stateQueued && e.lane == lane && e.gen == gen
}

type slot[K comparable] struct {
	key K
	gen uint64
}

// Queue is a keyed work queue. The zero value is not usable; call New.
type Queue[K comparable] struct {
	cfg Config[K]

	mu         sync.Mutex
	changed    chan struct{} // closed and replaced on every change a waiter may need
	keys       map[K]*entry[K]
	fifo       [laneCount][]slot[K]
	depth      [laneCount]int
	dequeues   int
	processing int
	seq        uint64
	gens       uint64
	timerIDs   uint64
	holds      map[string]struct{}
	adds       map[string]uint64
	dropped    uint64
	shutdown   bool
}

// New returns an empty queue.
func New[K comparable](cfg Config[K]) *Queue[K] {
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = defaultBaseBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = defaultMaxBackoff
	}
	if cfg.AgingEvery <= 0 {
		cfg.AgingEvery = defaultAgingEvery
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Float64
	}
	return &Queue[K]{
		cfg:     cfg,
		changed: make(chan struct{}),
		keys:    make(map[K]*entry[K]),
		holds:   make(map[string]struct{}),
		adds:    make(map[string]uint64),
	}
}

// Add enqueues k on lane, or merges r into the key's queued item, dirty
// record or deferred record. It returns the Add's seq, and false only after
// ShutDown. Seqs increase but are not dense: releasing a deferred or timer
// record also takes one. An unknown lane is treated as LaneHot.
func (q *Queue[K]) Add(k K, lane Lane, r Reason) (seq uint64, accepted bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.shutdown {
		q.dropped++
		return 0, false
	}
	if lane >= laneCount {
		lane = LaneHot
	}
	q.adds[r.Kind]++
	now := q.cfg.Now()
	q.seq++
	p := pending{addedAt: now, firstSeq: q.seq, lastSeq: q.seq}
	p.addReason(r)
	q.addLocked(k, lane, p, now)
	return q.seq, true
}

// AddAfter adds k on the hot lane after d, with r. A key has at most one
// timer, the earliest: a later deadline only merges r into it, so reasons from
// several AddAfter calls are trace-only and do not say when each was due. A
// d <= 0 adds at once.
func (q *Queue[K]) AddAfter(k K, d time.Duration, r Reason) {
	if d <= 0 {
		q.Add(k, LaneHot, r)
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.shutdown {
		q.dropped++
		return
	}
	q.adds[r.Kind]++
	e := q.entryLocked(k)
	e.timerNext.addReason(r)
	q.armLocked(k, e, q.cfg.Now().Add(d))
}

// AddRateLimited records a failure of k and closes its backoff gate until
// the jittered retry time, when a timer adds k with r. Call it before Done,
// so a dirty requeue sees the gate.
func (q *Queue[K]) AddRateLimited(k K, r Reason) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.shutdown {
		q.dropped++
		return
	}
	q.adds[r.Kind]++
	e := q.entryLocked(k)
	e.failures++
	e.retryAt = q.cfg.Now().Add(q.backoff(e.failures))
	e.deferred.addReason(r)
	q.armLocked(k, e, e.retryAt)
}

// Forget clears k's failure record and opens its backoff gate. Call it
// before Done. If k is processing, the gate's deferred record is discarded:
// it holds only retries and adds that landed before the running reconcile
// started. Otherwise it is added at once.
func (q *Queue[K]) Forget(k K) {
	q.mu.Lock()
	defer q.mu.Unlock()
	e := q.keys[k]
	if e == nil {
		return
	}
	e.failures, e.retryAt = 0, time.Time{}
	if e.timer != nil && e.timerNext.empty() {
		e.timer.Stop()
		e.timer = nil
	}
	held := e.deferred
	e.deferred = pending{}
	if !held.empty() && e.state != stateProcessing && !q.shutdown {
		q.releaseLocked(k, held)
	}
	q.dropIfIdleLocked(k, e)
}

// Get blocks until a key can be handed out and marks it processing. It
// returns false once ctx is done or the queue is shut down, even if items
// remain. It never returns a key that is already processing, and returns
// nothing while a hold is active. Every Get that returns true needs exactly
// one Done; call it via defer so a panicking reconcile cannot wedge the key.
func (q *Queue[K]) Get(ctx context.Context) (Item[K], bool) {
	for {
		q.mu.Lock()
		if q.shutdown || ctx.Err() != nil {
			q.mu.Unlock()
			return Item[K]{}, false
		}
		if len(q.holds) == 0 {
			if k, e, ok := q.popLocked(); ok {
				it := Item[K]{
					Key:      k,
					Lane:     e.lane,
					Reasons:  e.next.reasons,
					Dropped:  e.next.dropped,
					Urgent:   e.next.urgent,
					AddedAt:  e.next.addedAt,
					FirstSeq: e.next.firstSeq,
					LastSeq:  e.next.lastSeq,
					Failures: e.failures,
				}
				e.next = pending{}
				e.state, e.started = stateProcessing, q.cfg.Now()
				q.processing++
				q.mu.Unlock()
				return it, true
			}
		}
		changed := q.changed
		q.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
		}
	}
}

// Done marks k finished. If k was added while processing, it goes back to
// the tail of the strongest lane it was added on, unless the backoff gate
// defers it. Done for a key that is not processing does nothing.
func (q *Queue[K]) Done(k K) {
	q.mu.Lock()
	defer q.mu.Unlock()
	e := q.keys[k]
	if e == nil || e.state != stateProcessing {
		return
	}
	dirty := e.next
	e.state, e.next, e.started = stateIdle, pending{}, time.Time{}
	q.processing--
	if !dirty.empty() {
		q.addLocked(k, e.lane, dirty, q.cfg.Now())
	}
	q.dropIfIdleLocked(k, e)
	q.broadcastLocked()
}

// Hold stops Get from handing out keys until Release(name). Holds are a set:
// holding a name twice needs one Release.
func (q *Queue[K]) Hold(name string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.holds[name] = struct{}{}
}

// Release ends the named hold.
func (q *Queue[K]) Release(name string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.holds[name]; ok {
		delete(q.holds, name)
		q.broadcastLocked()
	}
}

// WaitIdle returns nil once no key is processing, or ctx.Err() if ctx ends
// first. Queued keys do not count.
func (q *Queue[K]) WaitIdle(ctx context.Context) error {
	for {
		q.mu.Lock()
		idle, changed := q.processing == 0, q.changed
		q.mu.Unlock()
		if idle {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// ShutDown stops admission and every timer, and wakes blocked Get calls,
// which return false. Queued keys are never handed out. Done keeps working.
func (q *Queue[K]) ShutDown() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.shutdown {
		return
	}
	q.shutdown = true
	for _, e := range q.keys {
		if e.timer != nil {
			e.timer.Stop()
			e.timer = nil
		}
	}
	q.broadcastLocked()
}

// Stats returns a snapshot of the queue.
func (q *Queue[K]) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	s := Stats{
		Keys:       len(q.keys),
		Depth:      q.depth,
		Processing: q.processing,
		Adds:       make(map[string]uint64, len(q.adds)),
		Dropped:    q.dropped,
		Holds:      make([]string, 0, len(q.holds)),
	}
	for kind, n := range q.adds {
		s.Adds[kind] = n
	}
	for name := range q.holds {
		s.Holds = append(s.Holds, name)
	}
	sort.Strings(s.Holds)
	for lane := range laneCount {
		for _, sl := range q.fifo[lane] {
			e := q.keys[sl.key]
			if e.live(lane, sl.gen) && (s.Oldest[lane].IsZero() || e.next.addedAt.Before(s.Oldest[lane])) {
				s.Oldest[lane] = e.next.addedAt
			}
		}
	}
	for _, e := range q.keys {
		if e.state == stateProcessing {
			if !e.next.empty() {
				s.Dirty++
			}
			if s.OldestInFlight.IsZero() || e.started.Before(s.OldestInFlight) {
				s.OldestInFlight = e.started
			}
		}
		if !e.deferred.empty() {
			s.Deferred++
		}
		if e.timer != nil {
			s.Timers++
		}
	}
	return s
}

// addLocked routes p for k by the key's state: merge into the queued item
// (promoting it to a stronger lane), mark a processing key dirty, defer an
// idle key behind a closed backoff gate, or enqueue it.
func (q *Queue[K]) addLocked(k K, lane Lane, p pending, now time.Time) {
	e := q.entryLocked(k)
	switch e.state {
	case stateQueued:
		e.next.merge(p)
		if lane < e.lane {
			q.depth[e.lane]--
			q.pushLocked(k, e, lane)
		}
	case stateProcessing:
		if e.next.empty() || lane < e.lane {
			e.lane = lane
		}
		e.next.merge(p)
	default:
		if !p.urgent && !e.retryAt.IsZero() && now.Before(e.retryAt) {
			e.deferred.merge(p)
			q.armLocked(k, e, e.retryAt)
			return
		}
		e.next.merge(p)
		q.pushLocked(k, e, lane)
	}
	q.broadcastLocked()
}

// releaseLocked adds a held-back record on the hot lane as one new Add.
func (q *Queue[K]) releaseLocked(k K, held pending) {
	now := q.cfg.Now()
	q.seq++
	held.lastSeq = q.seq
	if held.firstSeq == 0 {
		held.addedAt, held.firstSeq = now, q.seq
	}
	q.addLocked(k, LaneHot, held, now)
}

func (q *Queue[K]) pushLocked(k K, e *entry[K], lane Lane) {
	q.gens++
	e.state, e.lane, e.gen = stateQueued, lane, q.gens
	q.fifo[lane] = append(q.fifo[lane], slot[K]{key: k, gen: e.gen})
	q.depth[lane]++
}

// popLocked removes the next key to serve: the hot head, or the resync head
// when the hot lane is empty or on every AgingEvery-th dequeue.
func (q *Queue[K]) popLocked() (K, *entry[K], bool) {
	if q.depth[LaneHot]+q.depth[LaneResync] == 0 {
		var zero K
		return zero, nil, false
	}
	q.dequeues++
	lane := LaneHot
	if q.depth[LaneResync] > 0 && (q.depth[LaneHot] == 0 || q.dequeues%q.cfg.AgingEvery == 0) {
		lane = LaneResync
	}
	// depth[lane] > 0 guarantees a live slot in that lane.
	for {
		sl := q.fifo[lane][0]
		q.fifo[lane][0] = slot[K]{}
		q.fifo[lane] = q.fifo[lane][1:]
		if e := q.keys[sl.key]; e.live(lane, sl.gen) {
			q.depth[lane]--
			return sl.key, e, true
		}
	}
}

// armLocked makes sure k has a timer at or before at. After ShutDown it
// arms nothing.
func (q *Queue[K]) armLocked(k K, e *entry[K], at time.Time) {
	if q.shutdown || e.timer != nil && !at.Before(e.timerAt) {
		return
	}
	if e.timer != nil {
		e.timer.Stop()
	}
	q.timerIDs++
	id := q.timerIDs
	e.timerID, e.timerAt = id, at
	e.timer = time.AfterFunc(at.Sub(q.cfg.Now()), func() { q.fire(k, id) })
}

// fire runs k's timer. At or after retryAt it opens the backoff gate and
// releases what the gate held; before it, the timer's own reasons are added
// (and deferred again unless urgent) and the gate's timer is re-armed.
func (q *Queue[K]) fire(k K, id uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	e := q.keys[k]
	if q.shutdown || e == nil || e.timer == nil || e.timerID != id {
		return
	}
	at := e.timerAt
	e.timer = nil
	rel := e.timerNext
	e.timerNext = pending{}
	if !e.retryAt.IsZero() {
		if at.Before(e.retryAt) {
			q.armLocked(k, e, e.retryAt)
		} else {
			e.retryAt = time.Time{}
			held := e.deferred
			e.deferred = pending{}
			held.merge(rel)
			rel = held
		}
	}
	if !rel.empty() {
		q.releaseLocked(k, rel)
	}
	q.dropIfIdleLocked(k, e)
}

func (q *Queue[K]) entryLocked(k K) *entry[K] {
	e := q.keys[k]
	if e == nil {
		e = &entry[K]{}
		q.keys[k] = e
	}
	return e
}

// dropIfIdleLocked forgets a key with no state left.
func (q *Queue[K]) dropIfIdleLocked(k K, e *entry[K]) {
	if e.state == stateIdle && e.deferred.empty() && e.timer == nil && e.failures == 0 && e.retryAt.IsZero() {
		delete(q.keys, k)
	}
}

func (q *Queue[K]) broadcastLocked() {
	close(q.changed)
	q.changed = make(chan struct{})
}

// backoff is min(base·2^(failures-1), max), jittered by ±Jitter.
func (q *Queue[K]) backoff(failures int) time.Duration {
	d := min(q.cfg.BaseBackoff, q.cfg.MaxBackoff)
	for i := 1; i < failures; i++ {
		if d > q.cfg.MaxBackoff/2 {
			d = q.cfg.MaxBackoff
			break
		}
		d *= 2
	}
	return time.Duration(float64(d) * (1 + q.cfg.Jitter*(2*q.cfg.Rand()-1)))
}
