package main

// The lane's turn observer (X11 §5.4, slice TI-3): one per conversation the
// lane host serves, run on that conversation's worker goroutine (DES-O1
// §7.1). It consumes worker.TurnHandle.WatchTurns, converts each history into
// an opjournal.TurnObservation, folds it with opjournal.FoldTurn and writes
// the runtime row's turn cells and the turn facts the fold returns as one
// guarded batch (X11 D7, D-TURN). It is the only writer of turn facts and
// turn cells (X11 D6, I5), stamps gc.rt.turn.caps at the first observation
// of a generation and at every downgrade, stamps gc.rt.observer, and writes
// no op row: an op's turn is its deliverer's (D13).
//
// The observer runs only while its conversation's worker does, and it keeps
// that worker serving while observations are pending (DES-O1 §7.1): the
// watch's first update, a turn the cells show running while the session's
// runtime runs, a turn the lane's own content take is expected to start, and
// an update that could not be folded and waits for its retry. The worker starts the watch only after a pass that
// succeeded and found lane work, or with a kind listed in
// [ops] authoritative, so with the list empty a conversation with no lane op
// gets no observer. A new worker's observer rebuilds its tracking from the
// row's cells plus the source's Recent window (X11 §8); a turn that left the
// window meanwhile ends unobserved. A source that keeps no record of its
// normal ends (T3 V1) reading idle with no later turn has its running turn's
// end inferred (laneInfersIdleEnds), but only when this watch saw the turn
// run (sawRunning): after a restart, a successor worker or a park the same
// shape is a gap.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

const (
	// laneTurnStage names the observer's failures in the host's log, apart
	// from the lane executor's turn-evidence degradations ("turns").
	laneTurnStage = "turn-observer"
	// laneTurnWriteAttempts bounds one observation's write: a batch a
	// guard refused, or whose commit is unknown, is re-read and refolded.
	laneTurnWriteAttempts = 4
	// laneTurnBatchReason labels the observer's batches.
	laneTurnBatchReason = "gc turn observer"
	// laneTurnHold is how long a worker with no op work waits for its
	// observer's pending observations before it looks again; it is at most
	// laneParkAfter, so with no conversation queued the worker keeps its
	// slot, and with one queued it parks for this long.
	laneTurnHold = laneParkAfter
	// laneTurnExpectFor is how long the observer expects the turn a take's
	// effect starts: from the take's pass's return, and again from the end of
	// a turn the take found running.
	laneTurnExpectFor = time.Minute
	// A fold that could not be written is retried after laneTurnRetryStart,
	// doubling up to laneTurnRetryCap.
	laneTurnRetryStart = time.Second
	laneTurnRetryCap   = time.Minute
	// laneObserverPrefix starts the controller's gc.rt.observer stamp,
	// ctl:<host> (DES-O1 §5.1).
	laneObserverPrefix = laneHostName + ":"
)

// The turn capabilities stamp's keys (gc.rt.turn.caps, X11 §5.3): the axes,
// then source, interrupt and replace.
const (
	laneCapsStart     = "start"
	laneCapsEnd       = "end"
	laneCapsFence     = "fence"
	laneCapsLink      = "link"
	laneCapsLossless  = "lossless"
	laneCapsSource    = "source"
	laneCapsInterrupt = "interrupt"
	laneCapsReplace   = "replace"
)

// laneTurnStats are the turn observers' counters, as lane-status reports
// them (laneTurnStatus). They are apart from the lane passes' counters, so an
// observer failure never reads as a lane failure.
type laneTurnStats struct {
	watching   atomic.Int64
	updates    atomic.Int64
	writes     atomic.Int64
	facts      atomic.Int64
	gaps       atomic.Int64
	downgrades atomic.Int64
	errors     atomic.Int64
	// lagMS is the last written observation's lag: from the source read to
	// its write, which a delivery's link wait stretches (DES-O1 §4.8).
	lagMS atomic.Int64
	// lastErr is the last observer failure.
	lastErr atomic.Pointer[string]
}

// laneTurnStatus is the turn observers' part of lane-status.
type laneTurnStatus struct {
	// Watching counts the conversations whose observer watches now.
	Watching int64 `json:"watching"`
	// Updates counts the WatchTurns updates folded or refused, retries
	// included; Writes the batches that landed, carrying Facts turn facts.
	Updates int64 `json:"updates"`
	Writes  int64 `json:"writes"`
	Facts   int64 `json:"facts"`
	// Gaps counts the turns ended unobserved: gone from the source's window
	// before the observer read them.
	Gaps int64 `json:"gaps"`
	// Downgrades counts the generations whose capabilities were lowered.
	Downgrades int64 `json:"downgrades"`
	// Errors counts the observer's failures; LastError is the last one.
	Errors    int64  `json:"errors"`
	LastError string `json:"last_error,omitempty"`
	// LastLagMS is the last written observation's lag.
	LastLagMS int64 `json:"last_lag_ms"`
}

// status reads the counters, nil before any observer watched.
func (s *laneTurnStats) status() *laneTurnStatus {
	st := &laneTurnStatus{
		Watching: s.watching.Load(), Updates: s.updates.Load(), Writes: s.writes.Load(), Facts: s.facts.Load(),
		Gaps: s.gaps.Load(), Downgrades: s.downgrades.Load(), Errors: s.errors.Load(), LastLagMS: s.lagMS.Load(),
	}
	if p := s.lastErr.Load(); p != nil {
		st.LastError = *p
	}
	if *st == (laneTurnStatus{}) {
		return nil
	}
	return st
}

// laneTurnRows is where an observer reads and writes the runtime row and the
// facts: the sessions store read live, its guarded batcher, and the op
// store's id prefix.
type laneTurnRows struct {
	store   beads.Store
	batcher beads.GuardedBatcher
	prefix  string
}

// laneTurnGeneration is the conversation's current generation as the
// observer reads it.
type laneTurnGeneration struct {
	Gen int64
	// Live is false when the session is gone or closed.
	Live bool
	// Since is when the generation began, on the lane host's clock (the
	// session row's last_woke_at, whole seconds); zero when unknown. A turn
	// the source started before it, and a read made before it, are an
	// earlier generation's.
	Since time.Time
}

// laneTurnDeps are what a turn observer needs from its host.
type laneTurnDeps struct {
	// handle resolves the conversation's turn handle.
	handle func() (worker.TurnHandle, error)
	// generation reads the conversation's current generation.
	generation func() (laneTurnGeneration, error)
	// rows opens the stores the observer writes.
	rows func() (laneTurnRows, error)
	// running reports whether the session's runtime runs, read when the
	// worker's hold for a turn the cells show running ends; nil never ends
	// that hold.
	running func(ctx context.Context) (bool, error)
	// credits reads the turns gc interrupt ops ran on in generation gen
	// (opjournal.Store.InterruptCredits), so a turn's end is credited to
	// its op also after the worker that ran it exited; nil reads none.
	credits func(gen int64) ([]opjournal.InterruptRef, error)
	// observer is the gc.rt.observer stamp; "" stamps nothing.
	observer string
	now      func() time.Time
	// note logs a failure by stage; stats counts.
	note  func(stage string, err error)
	stats *laneTurnStats
}

// laneTurnWatchState is where an observer's watch is.
type laneTurnWatchState int

const (
	// laneTurnUnwatched: no watch yet; the next start tries.
	laneTurnUnwatched laneTurnWatchState = iota
	// laneTurnWatching: the watch runs.
	laneTurnWatching
	// laneTurnDone: nothing to watch for this worker: the session has no
	// turn source, or is gone.
	laneTurnDone
)

// laneTurnObserver is one conversation's turn observer. Only its worker's
// goroutine calls start, expect, observe, retry, pending and stop;
// setInterrupting may be called by the interrupt executor on that goroutine
// too.
type laneTurnObserver struct {
	conv string
	deps laneTurnDeps

	state   laneTurnWatchState
	handle  worker.TurnHandle
	ch      <-chan worker.TurnUpdate
	cancel  context.CancelFunc
	counted bool

	// gen is the generation caps were read for; caps the declaration,
	// downgraded once the source reported it cannot identify turns.
	gen        int64
	caps       worker.TurnCapabilities
	downgraded bool
	// startCaps: caps are the start's read, which the first update's
	// generation adopts without reading them again.
	startCaps bool

	// heard: the watch sent its first update, whatever became of it.
	heard bool
	// folded: a fold of this watch read or wrote the turn cells, which cells
	// are as the last one left them.
	folded bool
	cells  opjournal.TurnCells
	// expected is the turn a content take is expected to start, zero when
	// none is.
	expected laneTurnExpect
	// stopped: the session's runtime was found not running while the cells
	// show a turn running, so that turn holds the worker no longer; the next
	// batch that lands clears it.
	stopped bool
	// retry is the last update that could not be folded, kept until a
	// newer one folds or it does; retries counts its attempts.
	retry   *worker.TurnUpdate
	retries int

	mu           sync.Mutex
	interrupting opjournal.InterruptRef
}

// newLaneTurnObserver returns conversation conv's observer over deps.
func newLaneTurnObserver(conv string, deps laneTurnDeps) *laneTurnObserver {
	if deps.now == nil {
		deps.now = time.Now
	}
	if deps.stats == nil {
		deps.stats = &laneTurnStats{}
	}
	return &laneTurnObserver{conv: conv, deps: deps}
}

// updates is the watch's channel, nil while none runs.
func (o *laneTurnObserver) updates() <-chan worker.TurnUpdate {
	if o == nil || o.state != laneTurnWatching {
		return nil
	}
	return o.ch
}

// start starts the watch under ctx unless one runs or the worker found
// nothing to watch. A session with no turn source, or one that is gone, has
// nothing; a failure to resolve or read it is noted and tried again at the
// next start.
func (o *laneTurnObserver) start(ctx context.Context) {
	if o.state != laneTurnUnwatched {
		return
	}
	h, err := o.deps.handle()
	if err == nil {
		var caps worker.TurnCapabilities
		if caps, err = h.TurnCapabilities(ctx); err == nil {
			if caps.Source == worker.TurnSourceNone || caps.Source == "" {
				o.state = laneTurnDone
				return
			}
			wctx, cancel := context.WithCancel(ctx)
			var ch <-chan worker.TurnUpdate
			if ch, err = h.WatchTurns(wctx); err == nil {
				o.handle, o.ch, o.cancel, o.state = h, ch, cancel, laneTurnWatching
				o.caps, o.startCaps, o.heard, o.folded = caps, true, false, false
				o.counted = true
				o.deps.stats.watching.Add(1)
				return
			}
			cancel()
		}
	}
	if laneTurnsNothingToWatch(err) {
		o.state = laneTurnDone
		return
	}
	o.noteErr(fmt.Errorf("watching conversation %s's turns: %w", o.conv, err))
}

// laneTurnsNothingToWatch reports an error that leaves the worker nothing to
// watch: the session is gone, or its turns cannot be identified.
func laneTurnsNothingToWatch(err error) bool {
	return laneSessionGone(err) || errors.Is(err, runtime.ErrTurnIdentityUnsupported)
}

// laneSessionGone reports an error that says the session is gone.
func laneSessionGone(err error) bool {
	return errors.Is(err, runtime.ErrSessionNotFound) || errors.Is(err, session.ErrSessionNotFound)
}

// stop ends the watch. The observer starts no other.
func (o *laneTurnObserver) stop() {
	if o.cancel != nil {
		o.cancel()
		o.cancel = nil
	}
	if o.counted {
		o.counted = false
		o.deps.stats.watching.Add(-1)
	}
	if o.state == laneTurnWatching {
		o.state = laneTurnDone
	}
}

// closed handles the watch's channel closing: the watch ended, and the next
// start begins another.
func (o *laneTurnObserver) closed() {
	o.stop()
	o.state = laneTurnUnwatched
}

// laneTurnExpect is the turn a content take is expected to start (DES-O1
// §7.7): one the source started from From, or, when the source records no
// start, one not in Known, the turns the cells named at the take. A take made
// before any fold of the watch is Blind: the cells it found are unknown until
// a fold names them.
type laneTurnExpect struct {
	From, Until time.Time
	Known       []string
	Blind       bool
}

// started reports whether turn id, which the source started at start (zero
// when it records none), could be the one the take started. A blind
// expectation tells no turn without a start from one the take found.
func (e laneTurnExpect) started(id string, start time.Time) bool {
	if !start.IsZero() {
		return !start.Before(e.From)
	}
	return !e.Blind && !slices.Contains(e.Known, id)
}

// metBy reports whether the fold of obs, which left the cells next and
// journaled facts, shows a turn the take could have started: running in
// next, or journaled by facts.
func (e laneTurnExpect) metBy(obs opjournal.TurnObservation, next opjournal.TurnCells, facts []opjournal.Fact) bool {
	start := func(id string) time.Time {
		if id == obs.Current {
			return obs.CurrentStart
		}
		if i := slices.IndexFunc(obs.Recent, func(t opjournal.ObservedTurnEnd) bool { return t.ID == id }); i >= 0 {
			return obs.Recent[i].StartedAt
		}
		return time.Time{}
	}
	met := next.State == opjournal.TurnStateRunning && e.started(next.ID, start(next.ID))
	for _, f := range facts {
		met = met || e.started(f.Turn, start(f.Turn))
	}
	return met
}

// expect records that a content take whose pass began at from ran an effect,
// whose turn the observer waits for (DES-O1 §7.7): the worker keeps serving
// until a read made from from shows a turn the take could have started
// running or journaled (laneTurnExpect.started), or for laneTurnExpectFor
// after the pass returned, now. A turn the cells already named is no such
// turn, nor one the source started before from, so an earlier turn's end read
// after the take does not end the wait.
func (o *laneTurnObserver) expect(from time.Time) {
	o.expected = laneTurnExpect{From: from, Until: o.deps.now().Add(laneTurnExpectFor), Blind: !o.folded}
	if o.folded {
		o.expected.Known = laneTurnKnown(o.cells)
	}
}

// laneTurnKnown are the turns cells name: the running one, the last ended
// and the anchor.
func laneTurnKnown(cells opjournal.TurnCells) []string {
	var ids []string
	if cells.State == opjournal.TurnStateRunning {
		ids = append(ids, cells.ID)
	}
	for _, id := range []string{cells.LastID, cells.Seen} {
		if id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// folds records the cells a fold of obs left, at now, and the facts it
// journaled; landed says its batch landed, which clears stopped. The fold
// ends the expectation when it shows the take's turn. Otherwise, when it ends
// the hold for a turn the cells showed running, the expectation's window
// starts again, since the take's turn begins only after that one; and a blind
// expectation takes the turns the cells name as those the take found.
func (o *laneTurnObserver) folds(obs opjournal.TurnObservation, cells opjournal.TurnCells, facts []opjournal.Fact, landed bool, now time.Time) {
	held := o.holdsRunning()
	o.cells, o.folded = cells, true
	if landed {
		o.stopped = false
	}
	e := &o.expected
	switch {
	case e.From.IsZero():
	case !obs.ObservedAt.Before(e.From) && e.metBy(obs, cells, facts):
		*e = laneTurnExpect{}
	default:
		if until := now.Add(laneTurnExpectFor); held && !o.holdsRunning() && until.After(e.Until) {
			e.Until = until
		}
		if e.Blind {
			e.Known, e.Blind = laneTurnKnown(cells), false
		}
	}
}

// pending reports whether observations are pending, and how long the worker
// waits for them before it looks again: a retry's backoff, the time left of
// an expected turn, or laneTurnHold.
func (o *laneTurnObserver) pending(now time.Time) (time.Duration, bool) {
	if o == nil || o.state != laneTurnWatching {
		return 0, false
	}
	if o.retry != nil {
		return min(laneTurnRetryStart<<min(o.retries-1, 10), laneTurnRetryCap), true
	}
	if !o.heard || o.holdsRunning() {
		return laneTurnHold, true
	}
	if now.Before(o.expected.Until) {
		return min(o.expected.Until.Sub(now), laneTurnHold), true
	}
	return 0, false
}

// holdsRunning reports whether the turn the cells show running holds the
// worker: until the session's runtime is found not running, whose open turn
// is then left to session.ended (X11 D8).
func (o *laneTurnObserver) holdsRunning() bool {
	return o.folded && o.cells.State == opjournal.TurnStateRunning && !o.stopped
}

// timerFired runs when the worker's wait for its observer ends on its timer:
// a kept update is folded again, and a hold for a turn the cells show running
// looks at the runtime, and ends when it is not running. A failed look is
// noted and keeps the hold.
func (o *laneTurnObserver) timerFired(ctx context.Context) {
	if o.state != laneTurnWatching {
		return
	}
	if o.retry != nil {
		o.retryNow(ctx)
		return
	}
	if !o.holdsRunning() || o.deps.running == nil {
		return
	}
	up, err := o.deps.running(ctx)
	if err != nil {
		o.noteErr(fmt.Errorf("reading whether conversation %s's runtime runs: %w", o.conv, err))
		return
	}
	o.stopped = !up
}

// setInterrupting names the interrupt gc runs on a turn, so the fold
// credits that turn's end to the op (X11 §5.4). The interrupt executor
// (OJ-15) calls it before the effect and never clears it: the observer keeps
// the ref until it folds that turn's end or a newer ref replaces it; a ref
// whose turn no later fold names credits nothing. A ref without both an op
// and a turn names nothing and changes nothing. A ref lost with its worker is recovered from the op rows
// (laneTurnDeps.credits).
func (o *laneTurnObserver) setInterrupting(ref opjournal.InterruptRef) {
	if ref.Op == "" || ref.Turn == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.interrupting = ref
}

func (o *laneTurnObserver) interruptRef() opjournal.InterruptRef {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.interrupting
}

// creditedEnd drops the interrupt ref once the cells name its turn's end.
func (o *laneTurnObserver) creditedEnd(cells opjournal.TurnCells) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.interrupting.Turn != "" && cells.LastID == o.interrupting.Turn {
		o.interrupting = opjournal.InterruptRef{}
	}
}

// laneTurnFold is what became of one update the observer took.
type laneTurnFold int

const (
	// laneTurnFolded: the update was folded, and its batch landed or it
	// left nothing to write.
	laneTurnFolded laneTurnFold = iota
	// laneTurnIgnored: the update holds nothing to fold: a failed read, a
	// read from before the generation began, or one the watch ended at.
	laneTurnIgnored
	// laneTurnFailed: the update could not be folded and is owed a retry.
	laneTurnFailed
)

// observe folds one update and writes what changed. A history is folded; an
// Err update leaves the cells as they are, except that a source that cannot
// identify the session's turns downgrades the generation's capabilities. A
// generation read that finds the session gone or closed ends the watch (its
// open turn is left to session.ended, X11 D8). An update that could not be
// folded is kept for its retry; one with nothing to fold leaves a kept one
// as it is.
func (o *laneTurnObserver) observe(ctx context.Context, u worker.TurnUpdate) {
	o.deps.stats.updates.Add(1)
	o.heard = true
	switch o.apply(ctx, u) {
	case laneTurnFailed:
		o.keep(u)
	case laneTurnFolded:
		if u.Err == nil || (o.retry != nil && o.retry.Err != nil) {
			o.retry, o.retries = nil, 0
		}
	case laneTurnIgnored:
	}
}

// postpone takes u without folding it, the city being quiescent: it is kept
// for its retry as an update that could not be folded is.
func (o *laneTurnObserver) postpone(u worker.TurnUpdate) {
	o.deps.stats.updates.Add(1)
	o.heard = true
	o.keep(u)
}

// keep holds u for its retry: the newest history wins, and an Err update
// does not displace a history, whose write stamps the same capabilities.
func (o *laneTurnObserver) keep(u worker.TurnUpdate) {
	if u.Err != nil && !errors.Is(u.Err, runtime.ErrTurnIdentityUnsupported) {
		// Nothing to write: the cells stand until the next read.
		return
	}
	if u.Err == nil || o.retry == nil || o.retry.Err != nil {
		o.retry = &u
	}
	o.retries++
}

// retryNow folds the kept update again. The retry is spent: a fold that fails
// keeps it again, with one attempt more.
func (o *laneTurnObserver) retryNow(ctx context.Context) {
	if o.retry == nil || o.state != laneTurnWatching {
		return
	}
	u := *o.retry
	o.retry = nil
	o.observe(ctx, u)
	if o.retry == nil {
		o.retries = 0
	}
}

// apply folds u, reporting what became of it.
func (o *laneTurnObserver) apply(ctx context.Context, u worker.TurnUpdate) laneTurnFold {
	g, err := o.deps.generation()
	if err != nil {
		o.noteErr(fmt.Errorf("reading conversation %s's generation for its turns: %w", o.conv, err))
		return laneTurnFailed
	}
	if !g.Live || g.Gen < 1 {
		o.stop()
		return laneTurnIgnored
	}
	switch {
	case g.Gen == o.gen:
	case o.startCaps:
		o.gen, o.startCaps, o.downgraded = g.Gen, false, false
	default:
		if err := o.readCaps(ctx, g.Gen); err != nil {
			o.noteErr(err)
			return laneTurnFailed
		}
	}
	switch {
	case u.Err == nil:
		if !g.Since.IsZero() && !u.History.ObservedAt.IsZero() && u.History.ObservedAt.Before(g.Since) {
			// Read before this generation began: the history is an
			// earlier generation's, and folding it would journal that
			// generation's turns under this one.
			return laneTurnIgnored
		}
		hist := u.History
		return o.write(ctx, g, &hist)
	case errors.Is(u.Err, runtime.ErrTurnIdentityUnsupported) && !laneSessionGone(u.Err):
		if !o.downgraded {
			o.downgraded = true
			o.deps.stats.downgrades.Add(1)
		}
		return o.write(ctx, g, nil)
	default:
		// A failed read leaves the cells as they stand until the next one.
		// So does a not-found one: the row is live (read above), and a
		// protocol runtime answers not found until it runs, as while its
		// session is woken.
		return laneTurnIgnored
	}
}

// readCaps reads the session's capabilities for generation gen: they are
// fixed for a generation, and a new one starts undowngraded.
func (o *laneTurnObserver) readCaps(ctx context.Context, gen int64) error {
	caps, err := o.handle.TurnCapabilities(ctx)
	if err != nil {
		return fmt.Errorf("reading conversation %s's turn capabilities for generation %d: %w", o.conv, gen, err)
	}
	o.gen, o.caps, o.downgraded = gen, caps, false
	return nil
}

// declared is the capabilities the observer publishes for its generation:
// the declaration, lowered to boundary once downgraded (X11 §8).
func (o *laneTurnObserver) declared() worker.TurnCapabilities {
	if o.downgraded {
		return laneDowngradeTurnCaps(o.caps)
	}
	return o.caps
}

// write folds hist, or with hist nil only the capabilities stamp, into the
// runtime row of generation g and writes the cells and facts as one guarded
// batch, reporting laneTurnFailed when a retry is owed. A batch a guard
// refused, or whose commit is unknown, is read and folded again; facts
// already recorded are dropped from it, since facts are created if absent.
func (o *laneTurnObserver) write(ctx context.Context, g laneTurnGeneration, hist *runtime.TurnHistory) laneTurnFold {
	rows, err := o.deps.rows()
	if err != nil {
		o.noteErr(fmt.Errorf("opening conversation %s's runtime row for its turns: %w", o.conv, err))
		return laneTurnFailed
	}
	recorded := map[string]bool{}
	var last error
	for range laneTurnWriteAttempts {
		done, err := o.writeOnce(ctx, rows, g, hist, recorded)
		if done {
			return laneTurnFolded
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	o.noteErr(fmt.Errorf("writing conversation %s's turns: %w", o.conv, last))
	return laneTurnFailed
}

// writeOnce is one read, fold and batch. It reports done when nothing is
// left to write or nothing can be, and otherwise why the batch did not land.
func (o *laneTurnObserver) writeOnce(ctx context.Context, rows laneTurnRows, g laneTurnGeneration, hist *runtime.TurnHistory, recorded map[string]bool) (bool, error) {
	id, err := opjournal.RuntimeRowID(rows.prefix, o.conv)
	if err != nil {
		o.noteErr(fmt.Errorf("conversation %s's runtime row id: %w", o.conv, err))
		return true, nil
	}
	b, found, err := laneReadRow(rows.store, id)
	if err != nil {
		return false, err
	}
	var row opjournal.RuntimeRow
	if found {
		if row, err = opjournal.DecodeRuntimeRow(b); err != nil {
			o.noteErr(fmt.Errorf("conversation %s's runtime row: %w", o.conv, err))
			return true, nil
		}
		if row.Closed {
			// The conversation closed: the row is history.
			return true, nil
		}
	}
	prev := row.Turn
	now := o.deps.now()
	obs, cut := laneTurnObservationOf(o.conv, g, prev, hist, now)
	caps := laneEffectiveTurnCaps(o.declared(), prev, g.Gen)
	obs.Caps, obs.InferIdleEnds = laneTurnCapsStamp(caps), laneInfersIdleEnds(caps) && o.sawRunning(prev)
	ref, err := o.creditFor(prev, obs)
	if err != nil {
		return false, err
	}
	next, facts, write := opjournal.FoldTurn(prev, obs, ref)
	if write && prev.Generation < g.Gen && next.Seen == "" {
		// The new generation's window starts after the prior one's
		// newest turn, so no later read journals any of the prior's: the
		// anchor is the boundary, kept in the cells (laneTurnPrior).
		if cut != "" {
			next.Seen = cut
		} else {
			next.Seen = prev.Seen
		}
	}
	if !write {
		o.folds(obs, prev, nil, false, now)
		return true, nil
	}
	if !found {
		row.Conversation = o.conv
	}
	items, factIDs, err := laneTurnItems(rows.prefix, b, found, row, next, o.deps.observer, facts, recorded)
	if err != nil {
		o.noteErr(fmt.Errorf("conversation %s's turn batch: %w", o.conv, err))
		return true, nil
	}
	if _, err := rows.batcher.ApplyGuarded(ctx, beads.GuardedBatch{Reason: laneTurnBatchReason, Items: items}); err != nil {
		var item *beads.BatchItemError
		switch {
		case errors.Is(err, beads.ErrInvalidGuardedBatch), errors.Is(err, beads.ErrGuardedBatchUnsupported):
			o.noteErr(fmt.Errorf("conversation %s's turn batch: %w", o.conv, err))
			return true, nil
		case errors.As(err, &item) && errors.Is(err, beads.ErrAlreadyExists) && slices.Contains(factIDs, item.ID):
			// Facts another observer of the occurrences wrote first: keep
			// their records, drop their creates, and write the rest.
			if err := laneFactsRecorded(rows.store, factIDs, facts, recorded); err != nil {
				if errors.Is(err, errLaneFactUnread) {
					return false, err
				}
				o.noteErr(err)
				return true, nil
			}
		}
		return false, err
	}
	o.noteWrite(facts, now, hist)
	o.creditedEnd(next)
	o.folds(obs, next, facts, true, now)
	return true, nil
}

// creditFor is the interrupt ref the fold of obs over prev credits: the one
// the executor named, or else one an interrupt op's row names, when it names
// a turn whose end this fold reads off a cancel or an inferred end. It reads
// the op rows only for such an end.
func (o *laneTurnObserver) creditFor(prev opjournal.TurnCells, obs opjournal.TurnObservation) (opjournal.InterruptRef, error) {
	cands := laneTurnCreditCandidates(prev, obs)
	if len(cands) == 0 {
		return opjournal.InterruptRef{}, nil
	}
	if ref := o.interruptRef(); slices.Contains(cands, ref.Turn) {
		return ref, nil
	}
	if o.deps.credits == nil {
		return opjournal.InterruptRef{}, nil
	}
	refs, err := o.deps.credits(obs.Generation)
	if err != nil {
		return opjournal.InterruptRef{}, fmt.Errorf("reading conversation %s's interrupt ops: %w", o.conv, err)
	}
	for _, ref := range refs {
		if slices.Contains(cands, ref.Turn) {
			return ref, nil
		}
	}
	return opjournal.InterruptRef{}, nil
}

// laneTurnCreditCandidates are the turns whose end the fold of obs over prev
// may credit to a gc interrupt: ended turns of Recent the fold has not
// journaled, newer than the cells' anchor or running in the cells, whose
// source records a cancel or whose end is inferred (FoldTurn's reasons),
// and the running turn whose end the fold infers at idle, which obs infers
// only when the watch saw that turn run (sawRunning).
func laneTurnCreditCandidates(prev opjournal.TurnCells, obs opjournal.TurnObservation) []string {
	if prev.Generation != obs.Generation {
		prev = opjournal.TurnCells{}
	}
	running := ""
	if prev.State == opjournal.TurnStateRunning {
		running = prev.ID
	}
	newer := len(obs.Recent)
	switch prev.Seen {
	case "":
	case obs.Current:
		newer = 0
	default:
		if i := slices.IndexFunc(obs.Recent, func(e opjournal.ObservedTurnEnd) bool { return e.ID == prev.Seen }); i >= 0 {
			newer = i
		}
	}
	var out []string
	for i, e := range obs.Recent {
		if e.ID == prev.LastID || (i >= newer && e.ID != running) {
			continue
		}
		if e.Inferred || e.Status == opjournal.TurnSourceCancelled {
			out = append(out, e.ID)
		}
	}
	if turn := obs.InferredIdleEnd(prev); turn != "" {
		out = append(out, turn)
	}
	return out
}

// sawRunning reports whether a fold of this watch left the cells showing
// prev's running turn running, so the watch itself saw that turn run. Only
// then may the fold infer its end at idle (laneInfersIdleEnds) or credit
// that end to an interrupt; a watch that never saw it run (a restart, a
// successor worker, a park) ends it unobserved, a gap (X11 §8).
func (o *laneTurnObserver) sawRunning(prev opjournal.TurnCells) bool {
	return o.folded && prev.State == opjournal.TurnStateRunning && o.cells.State == opjournal.TurnStateRunning &&
		o.cells.Generation == prev.Generation && o.cells.ID == prev.ID
}

// laneInfersIdleEnds is the capability half of the fold's idle-end
// inference (opjournal.TurnObservation.InferIdleEnds): it reports whether
// the source's capabilities let a running turn it shows neither current nor
// in Recent at the idle boundary be read as a normal end, which holds for a
// protocol source whose End is inferred, as T3 V1, which keeps no record of
// the ends it clears. The fold infers only when sawRunning also holds. A
// transcript source's inferred ends reach Recent through the worker's idle
// overlay, so a turn missing there is a gap.
func laneInfersIdleEnds(caps worker.TurnCapabilities) bool {
	return caps.Source == worker.TurnSourceProtocol && caps.Normalized().End == runtime.TurnEndInferred
}

// noteWrite counts a batch that landed.
func (o *laneTurnObserver) noteWrite(facts []opjournal.Fact, now time.Time, hist *runtime.TurnHistory) {
	s := o.deps.stats
	s.writes.Add(1)
	s.facts.Add(int64(len(facts)))
	for _, f := range facts {
		if f.Kind == opjournal.FactTurnEnded && f.Reason == string(opjournal.TurnEndUnobserved) {
			s.gaps.Add(1)
		}
	}
	if hist != nil && !hist.ObservedAt.IsZero() {
		s.lagMS.Store(max(now.Sub(hist.ObservedAt).Milliseconds(), 0))
	}
}

// noteErr counts and notes a failed observation.
func (o *laneTurnObserver) noteErr(err error) {
	o.deps.stats.errors.Add(1)
	msg := err.Error()
	o.deps.stats.lastErr.Store(&msg)
	if o.deps.note != nil {
		o.deps.note(laneTurnStage, err)
	}
}

// laneReadRow reads row id, found false when it is absent.
func laneReadRow(store beads.Store, id string) (beads.Bead, bool, error) {
	b, err := store.Get(id)
	switch {
	case err == nil:
		return b, true, nil
	case errors.Is(err, beads.ErrNotFound):
		return beads.Bead{}, false, nil
	default:
		return beads.Bead{}, false, fmt.Errorf("reading runtime row %s: %w", id, err)
	}
}

// laneTurnItems are the batch that writes next, and the observer stamp, over
// row b (created when not found) and creates facts, leaving out the facts
// already recorded. It returns the fact items' ids.
func laneTurnItems(prefix string, b beads.Bead, found bool, row opjournal.RuntimeRow, next opjournal.TurnCells, observer string, facts []opjournal.Fact, recorded map[string]bool) ([]beads.GuardedItem, []string, error) {
	var items []beads.GuardedItem
	want := row
	want.Turn = next
	if observer != "" {
		want.Observer = observer
	}
	if found {
		opts, err := opjournal.RuntimeRowPatch(b, want)
		if err != nil {
			return nil, nil, err
		}
		if opts.Metadata != nil || opts.Status != nil {
			rev := b.Revision
			items = append(items, beads.GuardedItem{Update: &beads.GuardedUpdate{ID: b.ID, Opts: opts, ExpectedRevision: &rev}})
		}
	} else {
		created, err := opjournal.EncodeRuntimeRow(prefix, opjournal.RuntimeRow{Conversation: row.Conversation, Turn: next, Observer: want.Observer})
		if err != nil {
			return nil, nil, err
		}
		items = append(items, beads.GuardedItem{Create: &created})
	}
	var ids []string
	for _, f := range facts {
		item, err := opjournal.FactCreateItem(prefix, f)
		if err != nil {
			return nil, nil, err
		}
		if recorded[item.Create.ID] {
			continue
		}
		ids = append(ids, item.Create.ID)
		items = append(items, item)
	}
	if len(items) > beads.MaxGuardedItems {
		return nil, nil, fmt.Errorf("%d items exceed a guarded batch's %d", len(items), beads.MaxGuardedItems)
	}
	return items, ids, nil
}

// errLaneFactUnread is a fact laneFactsRecorded could not read: the write is
// retried, unlike for a fact that records another occurrence.
var errLaneFactUnread = errors.New("fact unread")

// laneFactsRecorded reads every fact of a batch whose create was refused and
// marks in recorded those already stored, each checked to record the
// occurrence one of facts names under its id. A read that fails is
// errLaneFactUnread.
func laneFactsRecorded(store beads.Store, ids []string, facts []opjournal.Fact, recorded map[string]bool) error {
	for _, id := range ids {
		b, err := store.Get(id)
		switch {
		case errors.Is(err, beads.ErrNotFound):
			continue
		case err != nil:
			return fmt.Errorf("reading fact %s after its create was refused: %w: %w", id, errLaneFactUnread, err)
		}
		got, err := opjournal.DecodeFact(b)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(facts, func(f opjournal.Fact) bool {
			return got.Kind == f.Kind && got.Conversation == f.Conversation && got.Generation == f.Generation && got.Turn == f.Turn
		}) {
			return fmt.Errorf("fact %s records another occurrence than the observer's", id)
		}
		recorded[id] = true
	}
	return nil
}

// laneTurnPrior are the turns an observation in generation g must leave out
// as the prior generation's, as the cells prev keep them: at the reset (prev
// is the prior generation's cells) the turns they name; afterwards, until
// the generation's first end, the boundary the reset left in Seen, unless
// the cells run that turn themselves.
func laneTurnPrior(prev opjournal.TurnCells, gen int64) []string {
	switch {
	case prev.Generation > 0 && prev.Generation < gen:
		var ids []string
		running := ""
		if prev.State == opjournal.TurnStateRunning {
			running = prev.ID
		}
		for _, id := range []string{running, prev.LastID, prev.Seen} {
			if id != "" && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
		return ids
	case prev.Generation == gen && prev.LastID == "" && prev.Seen != "" &&
		(prev.State != opjournal.TurnStateRunning || prev.ID != prev.Seen):
		return []string{prev.Seen}
	default:
		return nil
	}
}

// laneTurnObservationOf converts one read of the session's turns, made in
// generation g, into the fold's observation: hist's current turn and its
// ended turns, newest first, less the prior generation's turns a resumed
// source still shows (TurnObservation.Recent's contract). A turn is the
// prior's when the cells name it so (laneTurnPrior) or the source started it
// before g began; Recent stops at the first such turn, since every older one
// is the prior's too. cut is the newest prior turn left out, the current one
// first, "" when none was. With hist nil the observation carries only what
// the cells hold, so it folds into a capabilities stamp alone.
func laneTurnObservationOf(conv string, g laneTurnGeneration, prev opjournal.TurnCells, hist *runtime.TurnHistory, now time.Time) (opjournal.TurnObservation, string) {
	obs := opjournal.TurnObservation{Conversation: conv, Generation: g.Gen, ObservedAt: now}
	if hist == nil {
		if prev.Generation == g.Gen && prev.State == opjournal.TurnStateRunning {
			obs.Current = prev.ID
		}
		return obs, ""
	}
	if !hist.ObservedAt.IsZero() {
		obs.ObservedAt = hist.ObservedAt
	}
	priorIDs := laneTurnPrior(prev, g.Gen)
	prior := func(id string, start time.Time) bool {
		return slices.Contains(priorIDs, id) || (!g.Since.IsZero() && !start.IsZero() && start.Before(g.Since))
	}
	cut := ""
	if hist.Current.Active() {
		if prior(hist.Current.TurnID, hist.CurrentStart) {
			cut = hist.Current.TurnID
		} else {
			obs.Current, obs.CurrentStart = hist.Current.TurnID, hist.CurrentStart
		}
	}
	for _, t := range hist.Recent {
		if prior(t.TurnID, t.StartedAt) {
			if cut == "" {
				cut = t.TurnID
			}
			break
		}
		obs.Recent = append(obs.Recent, opjournal.ObservedTurnEnd{
			ID: t.TurnID, StartedAt: t.StartedAt, EndedAt: t.EndedAt,
			Status: opjournal.TurnSourceStatus(t.Status), Inferred: t.Inferred,
		})
	}
	return obs, cut
}

// laneDowngradeTurnCaps lowers caps for a source that cannot identify the
// session's turns (X11 §8): starts and ends to boundary at most, no link,
// not lossless.
func laneDowngradeTurnCaps(caps worker.TurnCapabilities) worker.TurnCapabilities {
	out := caps
	out.TurnCapabilities = laneMinTurnCaps(caps.TurnCapabilities, runtime.TurnCapabilities{
		Start: runtime.TurnStartBoundary, End: runtime.TurnEndBoundary,
		Fence: runtime.TurnFenceAtomic, Link: runtime.TurnLinkNone,
	})
	return out
}

// laneEffectiveTurnCaps is declared lowered by what the cells already
// publish for generation gen: capabilities only go down within a
// generation (X11 D1, I7), so an observer that restarts keeps an earlier
// downgrade.
func laneEffectiveTurnCaps(declared worker.TurnCapabilities, prev opjournal.TurnCells, gen int64) worker.TurnCapabilities {
	if prev.Generation != gen || len(prev.Caps) == 0 {
		return declared
	}
	out := declared
	out.TurnCapabilities = laneMinTurnCaps(declared.TurnCapabilities, laneTurnCapsFromStamp(prev.Caps))
	return out
}

// laneMinTurnCaps is the lower of a and b on every axis.
func laneMinTurnCaps(a, b runtime.TurnCapabilities) runtime.TurnCapabilities {
	a, b = a.Normalized(), b.Normalized()
	out := a
	if !b.Start.AtLeast(a.Start) {
		out.Start = b.Start
	}
	if !b.End.AtLeast(a.End) {
		out.End = b.End
	}
	if !b.Fence.AtLeast(a.Fence) {
		out.Fence = b.Fence
	}
	if !b.Link.AtLeast(a.Link) {
		out.Link = b.Link
	}
	out.Lossless = a.Lossless && b.Lossless
	return out
}

// laneTurnCapsStamp renders caps as gc.rt.turn.caps: the axes, then source,
// interrupt and replace, leaving out a trait the worker did not name.
func laneTurnCapsStamp(caps worker.TurnCapabilities) opjournal.TurnCapsStamp {
	n := caps.Normalized()
	stamp := opjournal.TurnCapsStamp{
		{Key: laneCapsStart, Value: string(n.Start)},
		{Key: laneCapsEnd, Value: string(n.End)},
		{Key: laneCapsFence, Value: string(n.Fence)},
		{Key: laneCapsLink, Value: string(n.Link)},
		{Key: laneCapsLossless, Value: strconv.FormatBool(n.Lossless)},
	}
	for _, p := range []opjournal.TurnCapsPair{
		{Key: laneCapsSource, Value: string(caps.Source)},
		{Key: laneCapsInterrupt, Value: string(caps.Interrupt)},
		{Key: laneCapsReplace, Value: string(caps.Replace)},
	} {
		if p.Value != "" {
			stamp = append(stamp, p)
		}
	}
	return stamp
}

// laneTurnCapsFromStamp reads a stamp's axes; an axis it does not hold, or
// holds with a value this build does not know, is none.
func laneTurnCapsFromStamp(stamp opjournal.TurnCapsStamp) runtime.TurnCapabilities {
	var caps runtime.TurnCapabilities
	for _, p := range stamp {
		switch p.Key {
		case laneCapsStart:
			caps.Start = runtime.TurnStart(p.Value)
		case laneCapsEnd:
			caps.End = runtime.TurnEnd(p.Value)
		case laneCapsFence:
			caps.Fence = runtime.TurnFence(p.Value)
		case laneCapsLink:
			caps.Link = runtime.TurnLink(p.Value)
		case laneCapsLossless:
			caps.Lossless = p.Value == "true"
		}
	}
	return caps.Normalized()
}

// laneObserverName is the controller's gc.rt.observer stamp: ctl:<host>, or
// ctl alone when the host has no name.
func laneObserverName() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return laneHostName
	}
	return laneObserverPrefix + strings.TrimSpace(host)
}

// laneTurnGenerationOf is info's generation as the observer reads it.
func laneTurnGenerationOf(info session.Info) laneTurnGeneration {
	view := conversationLaneView(info)
	g := laneTurnGeneration{Gen: view.Generation, Live: !view.Closed}
	if at, err := time.Parse(time.RFC3339, strings.TrimSpace(info.LastWokeAt)); err == nil {
		g.Since = at
	}
	return g
}

// newTurnObserver returns conversation conv's observer over the host's
// stores and runtime, as of each call: a store the controller replaces is
// the one the next write uses.
func (h *conversationLaneHost) newTurnObserver(conv string) *laneTurnObserver {
	return newLaneTurnObserver(conv, laneTurnDeps{
		handle: func() (worker.TurnHandle, error) {
			if h.turnHandle != nil {
				return h.turnHandle(conv)
			}
			return workerHandleForSessionWithConfig(h.env.CityPath(), h.env.SessionsBeadStore().Store, h.env.SessionProvider(), h.env.Config(), conv)
		},
		generation: func() (laneTurnGeneration, error) {
			live := sessionFrontDoor(laneLiveSessions{Store: h.env.SessionsBeadStore().Store})
			info, err := live.Get(conv)
			switch {
			case laneSessionGone(err), errors.Is(err, beads.ErrNotFound):
				return laneTurnGeneration{}, nil
			case err != nil:
				return laneTurnGeneration{}, err
			}
			return laneTurnGenerationOf(info), nil
		},
		rows: func() (laneTurnRows, error) {
			st, _, err := h.store()
			if err != nil {
				return laneTurnRows{}, err
			}
			rows := laneLiveSessions{Store: h.env.SessionsBeadStore().Store}
			batcher, ok := beads.GuardedBatcherFor(rows)
			if !ok || batcher == nil {
				return laneTurnRows{}, beads.ErrGuardedBatchUnsupported
			}
			return laneTurnRows{store: rows, batcher: batcher, prefix: st.Prefix()}, nil
		},
		running: func(context.Context) (bool, error) {
			obs, err := workerObserveSessionTargetWithConfig(h.env.CityPath(), h.env.SessionsBeadStore().Store, h.env.SessionProvider(), h.env.Config(), conv)
			switch {
			case laneSessionGone(err):
				return false, nil
			case err != nil:
				return false, err
			}
			return obs.Running && obs.Alive, nil
		},
		credits: func(gen int64) ([]opjournal.InterruptRef, error) {
			st, _, err := h.store()
			if err != nil {
				return nil, err
			}
			return st.InterruptCredits(conv, gen)
		},
		observer: h.observerName,
		now:      h.clk.Now,
		note:     h.logFailure,
		stats:    &h.turnStats,
	})
}

// laneTurnsWatchAfter reports whether a pass's outcome starts the turn
// watch: a pass that succeeded and found lane work, or any that succeeded
// when always is set (a kind listed in [ops] authoritative, or a worker
// resumed from a park with observations pending). With the list empty a
// doorbell for a conversation with no lane op starts no observer.
func laneTurnsWatchAfter(out lanePassOutcome, always bool) bool {
	if out.skipped || out.err != nil {
		return false
	}
	res := out.res
	return always || res.Took != opjournal.TakeNone || res.More || res.Busy || res.Wake ||
		len(res.Waits) > 0 || len(res.Closed) > 0 || len(res.Quarantined) > 0 || !res.Next.IsZero()
}

// watchTurns starts conv's turn watch after a pass, as store work: a
// quiescent city starts none, and a panic is recovered and noted.
func (h *conversationLaneHost) watchTurns(conv string, o *laneTurnObserver) {
	if o.state != laneTurnUnwatched {
		return
	}
	defer h.env.beginStoreWork()()
	defer h.recoverTurns(conv)
	if h.env.storesQuiescent() {
		return
	}
	o.start(h.ctx)
}

// observeTurn folds one of conv's turn updates as store work. An update that
// arrives while the city is quiescent is kept for its retry.
func (h *conversationLaneHost) observeTurn(conv string, o *laneTurnObserver, u worker.TurnUpdate, ok bool) {
	if !ok {
		o.closed()
		return
	}
	defer h.env.beginStoreWork()()
	defer h.recoverTurns(conv)
	if h.env.storesQuiescent() {
		o.postpone(u)
		return
	}
	o.observe(h.ctx, u)
}

// turnTimer runs conv's observer when the worker's wait for it ends on its
// timer (laneTurnObserver.timerFired), as store work. While the city is
// quiescent a kept update counts a retry, and a hold is kept.
func (h *conversationLaneHost) turnTimer(conv string, o *laneTurnObserver) {
	if o.retry == nil && !o.holdsRunning() {
		return
	}
	defer h.env.beginStoreWork()()
	defer h.recoverTurns(conv)
	if h.env.storesQuiescent() {
		if o.retry != nil {
			o.retries++
		}
		return
	}
	o.timerFired(h.ctx)
}

// recoverTurns recovers a panic of conv's observer, logging it.
func (h *conversationLaneHost) recoverTurns(conv string) {
	if r := recover(); r != nil {
		h.logf("conversation-lanes: turn observer of %s panicked: %v\n%s", conv, r, debug.Stack())
		h.turnStats.errors.Add(1)
	}
}
