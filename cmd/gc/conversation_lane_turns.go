package main

// The lane's turn observer (X11 §5.4, slice TI-3): one per conversation the
// lane host serves, run on that conversation's worker goroutine (DES-O1
// §7.1). It consumes worker.TurnHandle.WatchTurns, converts each history into
// an opjournal.TurnObservation, folds it with opjournal.FoldTurn and writes
// the runtime row's turn cells and the turn facts the fold returns as one
// guarded batch (X11 D7, D-TURN). It is the only writer of turn facts and
// turn cells (X11 D6, I5), stamps gc.rt.turn.caps at the first observation
// of a generation and at every downgrade, and writes no op row: an op's turn
// is its deliverer's (D13).
//
// The observer runs only while its conversation's worker does, so a
// conversation with no open lane op has none, and with the ops gates off
// nothing here runs. A new worker's observer rebuilds its tracking from the
// row's cells plus the source's Recent window (X11 §8); a turn that left
// the window meanwhile ends unobserved.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
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
	// laneTurnStage names the observer's failures in the host's notes.
	laneTurnStage = "turns"
	// laneTurnWriteAttempts bounds one observation's write: a batch a
	// guard refused, or whose commit is unknown, is re-read and refolded.
	laneTurnWriteAttempts = 4
	// laneTurnBatchReason labels the observer's batches.
	laneTurnBatchReason = "gc turn observer"
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
// them (laneTurnStatus).
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
}

// laneTurnStatus is the turn observers' part of lane-status.
type laneTurnStatus struct {
	// Watching counts the conversations whose observer watches now.
	Watching int64 `json:"watching"`
	// Updates counts the WatchTurns updates folded or refused; Writes the
	// batches that landed, carrying Facts turn facts.
	Updates int64 `json:"updates"`
	Writes  int64 `json:"writes"`
	Facts   int64 `json:"facts"`
	// Gaps counts the turns ended unobserved: gone from the source's window
	// before the observer read them.
	Gaps int64 `json:"gaps"`
	// Downgrades counts the generations whose capabilities were lowered.
	Downgrades int64 `json:"downgrades"`
	// Errors counts the observations that could not be written.
	Errors int64 `json:"errors"`
	// LastLagMS is the last written observation's lag.
	LastLagMS int64 `json:"last_lag_ms"`
}

// status reads the counters, nil before any observer watched.
func (s *laneTurnStats) status() *laneTurnStatus {
	st := &laneTurnStatus{
		Watching: s.watching.Load(), Updates: s.updates.Load(), Writes: s.writes.Load(), Facts: s.facts.Load(),
		Gaps: s.gaps.Load(), Downgrades: s.downgrades.Load(), Errors: s.errors.Load(), LastLagMS: s.lagMS.Load(),
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

// laneTurnDeps are what a turn observer needs from its host.
type laneTurnDeps struct {
	// handle resolves the conversation's turn handle.
	handle func() (worker.TurnHandle, error)
	// generation reads the conversation's current generation; live false
	// when its session is gone or closed.
	generation func() (gen int64, live bool, err error)
	// rows opens the stores the observer writes.
	rows func() (laneTurnRows, error)
	now  func() time.Time
	// note records a failure by stage; stats counts.
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
// goroutine calls start, observe and stop; setInterrupting may be called by
// the interrupt executor on that goroutine too.
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
	// priorGen and prior are the turns of the generation before gen, read
	// off the cells at their reset: the source's window can still hold them
	// (a resumed transcript), and they are not gen's (TurnObservation.Recent).
	priorGen int64
	prior    []string

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
				o.caps, o.startCaps = caps, true
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

// setInterrupting names the interrupt gc runs on a turn, so the fold
// credits that turn's end to the op (X11 §5.4); the zero ref clears it.
func (o *laneTurnObserver) setInterrupting(ref opjournal.InterruptRef) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.interrupting = ref
}

func (o *laneTurnObserver) interruptRef() opjournal.InterruptRef {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.interrupting
}

// observe folds one update and writes what changed. A history is folded; an
// Err update leaves the cells as they are, except that a source that cannot
// identify the session's turns downgrades the generation's capabilities, and
// a session that is gone ends the watch (its open turn is left to
// session.ended, X11 D8).
func (o *laneTurnObserver) observe(ctx context.Context, u worker.TurnUpdate) {
	o.deps.stats.updates.Add(1)
	gen, live, err := o.deps.generation()
	if err != nil {
		o.noteErr(fmt.Errorf("reading conversation %s's generation for its turns: %w", o.conv, err))
		return
	}
	if !live || gen < 1 {
		o.stop()
		return
	}
	switch {
	case gen == o.gen:
	case o.startCaps:
		o.gen, o.startCaps, o.downgraded = gen, false, false
	default:
		if err := o.readCaps(ctx, gen); err != nil {
			o.noteErr(err)
			return
		}
	}
	switch {
	case u.Err == nil:
		hist := u.History
		o.write(ctx, gen, &hist)
	case laneSessionGone(u.Err):
		o.stop()
	case errors.Is(u.Err, runtime.ErrTurnIdentityUnsupported):
		if !o.downgraded {
			o.downgraded = true
			o.deps.stats.downgrades.Add(1)
		}
		o.write(ctx, gen, nil)
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
// runtime row of generation gen and writes the cells and facts as one
// guarded batch. A batch a guard refused, or whose commit is unknown, is
// read and folded again; a fact already recorded is dropped from it, since
// facts are created if absent.
func (o *laneTurnObserver) write(ctx context.Context, gen int64, hist *runtime.TurnHistory) {
	rows, err := o.deps.rows()
	if err != nil {
		o.noteErr(fmt.Errorf("opening conversation %s's runtime row for its turns: %w", o.conv, err))
		return
	}
	recorded := map[string]bool{}
	var last error
	for range laneTurnWriteAttempts {
		done, err := o.writeOnce(ctx, rows, gen, hist, recorded)
		if done {
			return
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	o.noteErr(fmt.Errorf("writing conversation %s's turns: %w", o.conv, last))
}

// writeOnce is one read, fold and batch. It reports done when nothing is
// left to write, and otherwise why the batch did not land.
func (o *laneTurnObserver) writeOnce(ctx context.Context, rows laneTurnRows, gen int64, hist *runtime.TurnHistory, recorded map[string]bool) (bool, error) {
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
			return true, nil
		}
	}
	prev := row.Turn
	reset := prev.Generation < gen
	if reset && prev.Generation > 0 && o.priorGen != gen {
		o.priorGen, o.prior = gen, laneTurnIDsOf(prev)
	}
	prior := o.prior
	if o.priorGen != gen {
		prior = nil
	}
	now := o.deps.now()
	obs, cut := laneTurnObservationOf(o.conv, gen, prev, hist, prior, now)
	obs.Caps = laneTurnCapsStamp(laneEffectiveTurnCaps(o.declared(), prev, gen))
	next, facts, write := opjournal.FoldTurn(prev, obs, o.interruptRef())
	if reset && write && next.Seen == "" {
		// The new generation's window starts after the prior one's
		// newest turn, so a later read journals none of the prior's.
		if cut != "" {
			next.Seen = cut
		} else {
			next.Seen = prev.Seen
		}
	}
	if !write {
		return true, nil
	}
	if !found {
		row.Conversation = o.conv
	}
	items, factIDs, err := laneTurnItems(rows.prefix, b, found, row, next, facts, recorded)
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
			// A fact another observer of the occurrence wrote first:
			// keep its record, drop the create, and write the rest.
			if err := laneFactRecorded(rows.store, item.ID, facts); err != nil {
				o.noteErr(err)
				return true, nil
			}
			recorded[item.ID] = true
		}
		return false, err
	}
	o.noteWrite(facts, now, hist)
	return true, nil
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

// laneTurnItems are the batch that writes next over row b (created when not
// found) and creates facts, leaving out the facts already recorded. It
// returns the fact items' ids.
func laneTurnItems(prefix string, b beads.Bead, found bool, row opjournal.RuntimeRow, next opjournal.TurnCells, facts []opjournal.Fact, recorded map[string]bool) ([]beads.GuardedItem, []string, error) {
	var items []beads.GuardedItem
	if found {
		want := row
		want.Turn = next
		opts, err := opjournal.RuntimeRowPatch(b, want)
		if err != nil {
			return nil, nil, err
		}
		if opts.Metadata != nil || opts.Status != nil {
			rev := b.Revision
			items = append(items, beads.GuardedItem{Update: &beads.GuardedUpdate{ID: b.ID, Opts: opts, ExpectedRevision: &rev}})
		}
	} else {
		created, err := opjournal.EncodeRuntimeRow(prefix, opjournal.RuntimeRow{Conversation: row.Conversation, Turn: next})
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

// laneFactRecorded checks that the fact stored at id records the occurrence
// one of facts names under it.
func laneFactRecorded(store beads.Store, id string, facts []opjournal.Fact) error {
	b, err := store.Get(id)
	if err != nil {
		return fmt.Errorf("reading fact %s after its create was refused: %w", id, err)
	}
	got, err := opjournal.DecodeFact(b)
	if err != nil {
		return err
	}
	for _, f := range facts {
		if got.Kind == f.Kind && got.Conversation == f.Conversation && got.Generation == f.Generation && got.Turn == f.Turn {
			return nil
		}
	}
	return fmt.Errorf("fact %s records another occurrence than the observer's", id)
}

// laneTurnIDsOf are the turns cells name.
func laneTurnIDsOf(c opjournal.TurnCells) []string {
	var ids []string
	for _, id := range []string{c.ID, c.LastID, c.Seen} {
		if id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// laneTurnObservationOf converts one read of the session's turns, made in
// generation gen, into the fold's observation: hist's current turn and its
// ended turns, newest first, less the turns of the prior generation (prior)
// a resumed source still shows. cut is the newest prior turn dropped from
// Recent, "" when none was. With hist nil the observation carries only what
// the cells hold, so it folds into a capabilities stamp alone.
func laneTurnObservationOf(conv string, gen int64, prev opjournal.TurnCells, hist *runtime.TurnHistory, prior []string, now time.Time) (opjournal.TurnObservation, string) {
	obs := opjournal.TurnObservation{Conversation: conv, Generation: gen, ObservedAt: now}
	if hist == nil {
		if prev.Generation == gen && prev.State == opjournal.TurnStateRunning {
			obs.Current = prev.ID
		}
		return obs, ""
	}
	if !hist.ObservedAt.IsZero() {
		obs.ObservedAt = hist.ObservedAt
	}
	if hist.Current.Active() && !slices.Contains(prior, hist.Current.TurnID) {
		obs.Current, obs.CurrentStart = hist.Current.TurnID, hist.CurrentStart
	}
	cut := ""
	for _, t := range hist.Recent {
		if slices.Contains(prior, t.TurnID) {
			cut = t.TurnID
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
