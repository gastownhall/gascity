package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

// laneObsConv is the observer tests' conversation.
const laneObsConv = "gc-conv-1"

// laneObsInterruptOp is a well-formed op id for an interrupt the tests run.
const laneObsInterruptOp = "gc-op-abcdefghijklmnopqrstuvwxyz"

// laneRecordingBatcher records every guarded batch it forwards; refuse, when
// set, answers the next batch instead of forwarding it.
type laneRecordingBatcher struct {
	next beads.GuardedBatcher

	mu      sync.Mutex
	batches []beads.GuardedBatch
	refuse  func(beads.GuardedBatch) error
}

func (r *laneRecordingBatcher) ApplyGuarded(ctx context.Context, b beads.GuardedBatch) (beads.GuardedResult, error) {
	r.mu.Lock()
	r.batches = append(r.batches, b)
	refuse := r.refuse
	r.refuse = nil
	r.mu.Unlock()
	if refuse != nil {
		if err := refuse(b); err != nil {
			return beads.GuardedResult{}, err
		}
	}
	return r.next.ApplyGuarded(ctx, b)
}

func (r *laneRecordingBatcher) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.batches)
}

// laneFakeTurns is a fake worker turn handle: fixed capabilities and a
// channel the test sends updates on.
type laneFakeTurns struct {
	worker.TurnHandle
	mu        sync.Mutex
	caps      worker.TurnCapabilities
	capsErr   error
	watchErr  error
	capsReads int
	watches   int
	ch        chan worker.TurnUpdate
}

func (f *laneFakeTurns) TurnCapabilities(context.Context) (worker.TurnCapabilities, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.capsReads++
	return f.caps, f.capsErr
}

func (f *laneFakeTurns) WatchTurns(context.Context) (<-chan worker.TurnUpdate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watches++
	return f.ch, f.watchErr
}

// laneObsFixture is one observer over a memory store, a recording batcher
// and a fake turn handle, at a generation the test sets.
type laneObsFixture struct {
	t       *testing.T
	store   laneTestStore
	batcher *laneRecordingBatcher
	turns   *laneFakeTurns
	clk     *laneTestClock
	notes   *laneNotes
	stats   *laneTurnStats
	gen     int64
	live    bool
	o       *laneTurnObserver
}

// laneObsCaps are a checked transcript session's capabilities.
var laneObsCaps = worker.TurnCapabilities{
	TurnCapabilities: runtime.TurnCapabilities{
		Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded,
		Fence: runtime.TurnFenceChecked, Link: runtime.TurnLinkCorrelated, Lossless: true,
	},
	Source:    worker.TurnSourceTranscript,
	Interrupt: runtime.TurnInterruptKeystroke,
	Replace:   worker.TurnReplaceInterrupt,
}

func newLaneObsFixture(t *testing.T) *laneObsFixture {
	t.Helper()
	store := newLaneTestStore()
	f := &laneObsFixture{
		t: t, store: store, batcher: &laneRecordingBatcher{next: store.MemStore},
		turns: &laneFakeTurns{caps: laneObsCaps, ch: make(chan worker.TurnUpdate, 1)},
		clk:   &laneTestClock{now: laneT0}, notes: &laneNotes{}, stats: &laneTurnStats{},
		gen: 1, live: true,
	}
	f.o = f.observer()
	return f
}

// observer builds a new observer over the fixture, as a new worker does.
func (f *laneObsFixture) observer() *laneTurnObserver {
	return newLaneTurnObserver(laneObsConv, laneTurnDeps{
		handle:     func() (worker.TurnHandle, error) { return f.turns, nil },
		generation: func() (int64, bool, error) { return f.gen, f.live, nil },
		rows: func() (laneTurnRows, error) {
			return laneTurnRows{store: f.store, batcher: f.batcher, prefix: "gc"}, nil
		},
		now: f.clk.Now, note: f.notes.note, stats: f.stats,
	})
}

// restart replaces the observer with a fresh one, as a lane restart does.
func (f *laneObsFixture) restart() {
	f.o.stop()
	f.o = f.observer()
	f.start()
}

func (f *laneObsFixture) start() {
	f.t.Helper()
	f.o.start(context.Background())
	if f.o.state != laneTurnWatching {
		f.t.Fatalf("observer state = %d, want watching; notes %v", f.o.state, f.notes.all())
	}
}

// see folds hist, read now, and returns the batches it wrote.
func (f *laneObsFixture) see(hist runtime.TurnHistory) int {
	f.t.Helper()
	before := f.batcher.count()
	hist.ObservedAt = f.clk.Now()
	f.o.observe(context.Background(), worker.TurnUpdate{History: hist})
	if n := f.notes.all(); len(n) > 0 {
		f.t.Fatalf("observer noted %v", n)
	}
	f.clk.Advance(time.Second)
	return f.batcher.count() - before
}

func (f *laneObsFixture) row() opjournal.RuntimeRow {
	f.t.Helper()
	st, err := opjournal.NewLaneStore(beads.SessionStore{Store: f.store}, f.clk)
	if err != nil {
		f.t.Fatal(err)
	}
	row, _, found, err := st.Runtime(laneObsConv)
	if err != nil || !found {
		f.t.Fatalf("runtime row: found %v, %v", found, err)
	}
	return row
}

// facts lists the conversation's facts as kind:turn[:reason[:cause]],
// oldest first.
func (f *laneObsFixture) facts() []string {
	f.t.Helper()
	rows, err := f.store.ListByLabel(opjournal.FactConversationLabel(laneObsConv), 0, beads.IncludeClosed)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, b := range rows {
		fact, err := opjournal.DecodeFact(b)
		if err != nil {
			f.t.Fatal(err)
		}
		s := fmt.Sprintf("%s:%s", fact.Kind, fact.Turn)
		if fact.Generation != f.gen {
			s += fmt.Sprintf("@g%d", fact.Generation)
		}
		if fact.Reason != "" {
			s += ":" + fact.Reason
		}
		if len(fact.CauseOps) > 0 {
			s += ":" + strings.Join(fact.CauseOps, ",")
		}
		out = append(out, s)
	}
	slices.Reverse(out) // listed newest first
	return out
}

func (f *laneObsFixture) wantFacts(want ...string) {
	f.t.Helper()
	if got := f.facts(); !slices.Equal(got, want) {
		f.t.Fatalf("facts = %q, want %q", got, want)
	}
}

func laneRunning(id string, recent ...runtime.EndedTurn) runtime.TurnHistory {
	return runtime.TurnHistory{Current: runtime.TurnState{TurnID: id}, CurrentStart: laneT0, Recent: recent}
}

func laneIdle(recent ...runtime.EndedTurn) runtime.TurnHistory {
	return runtime.TurnHistory{Recent: recent}
}

func laneEnded(id string, status runtime.TurnStatus) runtime.EndedTurn {
	return runtime.EndedTurn{TurnID: id, StartedAt: laneT0, EndedAt: laneT0.Add(time.Minute), Status: status}
}

// TestLaneTurnObserverJournalsScriptedTurns pins the observer's main line
// (X11 §6 TI-3): each scripted turn gives one turn.started and one
// turn.ended with the row's transitions, in at most two batches per turn,
// and the generation's capabilities are stamped with the first write.
func TestLaneTurnObserverJournalsScriptedTurns(t *testing.T) {
	f := newLaneObsFixture(t)
	f.start()
	writes := f.see(laneRunning("t1"))
	row := f.row()
	if row.Turn.State != opjournal.TurnStateRunning || row.Turn.ID != "t1" || row.Turn.Generation != 1 {
		t.Fatalf("cells = %+v, want t1 running in generation 1", row.Turn)
	}
	if got, want := row.Turn.Caps.String(), "start=recorded,end=recorded,fence=checked,link=correlated,lossless=true,source=transcript,interrupt=keystroke,replace=interrupt"; got != want {
		t.Fatalf("caps = %q, want %q", got, want)
	}
	writes += f.see(laneIdle(laneEnded("t1", runtime.TurnStatusCompleted)))
	if writes > 2 {
		t.Fatalf("turn t1 took %d batches, want at most 2", writes)
	}
	writes = f.see(laneRunning("t2", laneEnded("t1", runtime.TurnStatusCompleted)))
	writes += f.see(laneIdle(laneEnded("t2", runtime.TurnStatusFailed), laneEnded("t1", runtime.TurnStatusCompleted)))
	if writes > 2 {
		t.Fatalf("turn t2 took %d batches, want at most 2", writes)
	}
	f.wantFacts("turn.started:t1", "turn.ended:t1:completed", "turn.started:t2", "turn.ended:t2:failed")
	row = f.row()
	if row.Turn.State != opjournal.TurnStateEnded || row.Turn.LastID != "t2" || row.Turn.LastEnd != opjournal.TurnEndFailed {
		t.Fatalf("cells = %+v, want ended after t2 failed", row.Turn)
	}
	if row.Generation != 0 || row.Seq != 0 || !row.Lease.IsZero() {
		t.Fatalf("lane cells = gen %d seq %d lease %v, want untouched", row.Generation, row.Seq, row.Lease)
	}
	if st := f.stats.status(); st == nil || st.Writes != 4 || st.Facts != 4 || st.Gaps != 0 || st.Watching != 1 {
		t.Fatalf("stats = %+v, want 4 writes of 4 facts and one watch", st)
	}
}

// TestLaneTurnObserverUnchangedWritesNothing pins that an observation that
// changes nothing writes nothing, the first one after a restart included.
func TestLaneTurnObserverUnchangedWritesNothing(t *testing.T) {
	f := newLaneObsFixture(t)
	f.start()
	f.see(laneRunning("t1"))
	if n := f.see(laneRunning("t1")); n != 0 {
		t.Fatalf("an unchanged observation wrote %d batches", n)
	}
	f.restart()
	if n := f.see(laneRunning("t1")); n != 0 {
		t.Fatalf("a restarted observer's unchanged observation wrote %d batches", n)
	}
	f.wantFacts("turn.started:t1")
}

// TestLaneTurnObserverRestartRecovers pins that a lane restart mid-turn
// recovers from the row's cells plus Recent: the turn that ended while no
// observer ran, and one that ran and ended entirely meanwhile, are both
// journaled once, oldest first.
func TestLaneTurnObserverRestartRecovers(t *testing.T) {
	f := newLaneObsFixture(t)
	f.start()
	f.see(laneRunning("t1"))
	f.o.stop()
	f.o = f.observer()
	f.start()
	f.see(laneRunning("t3", laneEnded("t2", ""), laneEnded("t1", runtime.TurnStatusCompleted)))
	f.wantFacts("turn.started:t1", "turn.ended:t1:completed", "turn.started:t2", "turn.ended:t2:superseded", "turn.started:t3")
	if row := f.row(); row.Turn.ID != "t3" || row.Turn.State != opjournal.TurnStateRunning {
		t.Fatalf("cells = %+v, want t3 running", row.Turn)
	}
}

// TestLaneTurnObserverGapIsUnobserved pins that a running turn gone from the
// source's window ends unobserved, counted as a gap.
func TestLaneTurnObserverGapIsUnobserved(t *testing.T) {
	f := newLaneObsFixture(t)
	f.start()
	f.see(laneRunning("t1"))
	f.see(laneRunning("t9"))
	f.wantFacts("turn.started:t1", "turn.ended:t1:unobserved", "turn.started:t9")
	if st := f.stats.status(); st.Gaps != 1 {
		t.Fatalf("gaps = %d, want 1", st.Gaps)
	}
}

// TestLaneTurnObserverGenerationResets pins a generation change: the cells
// reset, the capabilities are read and stamped again, and the prior
// generation's turns a resumed source still shows are not journaled under
// the new one, on this read or a later one.
func TestLaneTurnObserverGenerationResets(t *testing.T) {
	f := newLaneObsFixture(t)
	f.start()
	f.see(laneRunning("t1"))
	f.see(laneIdle(laneEnded("t1", runtime.TurnStatusCompleted)))
	f.gen = 2
	f.turns.caps.Fence = runtime.TurnFenceNone
	f.see(laneIdle(laneEnded("t1", runtime.TurnStatusCompleted)))
	row := f.row()
	if row.Turn.Generation != 2 || row.Turn.State != opjournal.TurnStateUnknown || row.Turn.LastID != "" {
		t.Fatalf("cells = %+v, want reset to generation 2", row.Turn)
	}
	if !strings.Contains(row.Turn.Caps.String(), "fence=none") {
		t.Fatalf("caps = %q, want generation 2's", row.Turn.Caps)
	}
	f.see(laneRunning("u1", laneEnded("t1", runtime.TurnStatusCompleted)))
	f.see(laneIdle(laneEnded("u1", runtime.TurnStatusCompleted), laneEnded("t1", runtime.TurnStatusCompleted)))
	f.restart()
	f.see(laneIdle(laneEnded("u1", runtime.TurnStatusCompleted), laneEnded("t1", runtime.TurnStatusCompleted)))
	f.wantFacts("turn.started:t1@g1", "turn.ended:t1@g1:completed", "turn.started:u1", "turn.ended:u1:completed")
	if f.turns.capsReads != 3 {
		t.Fatalf("caps reads = %d, want one per watch start and one per new generation", f.turns.capsReads)
	}
}

// TestLaneTurnObserverCreditsInterrupt pins that a gc interrupt the observer
// was told of ends its turn interrupted, naming the op; one it was not told
// of is interrupted_locally.
func TestLaneTurnObserverCreditsInterrupt(t *testing.T) {
	f := newLaneObsFixture(t)
	f.start()
	f.see(laneRunning("t1"))
	f.o.setInterrupting(opjournal.InterruptRef{Op: laneObsInterruptOp, Turn: "t1"})
	f.see(laneIdle(laneEnded("t1", runtime.TurnStatusCancelled)))
	f.o.setInterrupting(opjournal.InterruptRef{})
	f.see(laneRunning("t2", laneEnded("t1", runtime.TurnStatusCancelled)))
	f.see(laneIdle(laneEnded("t2", runtime.TurnStatusCancelled), laneEnded("t1", runtime.TurnStatusCancelled)))
	f.wantFacts("turn.started:t1", "turn.ended:t1:interrupted:"+laneObsInterruptOp,
		"turn.started:t2", "turn.ended:t2:interrupted_locally")
}

// TestLaneTurnObserverDowngrades pins a source that cannot identify the
// session's turns: the capabilities go down to boundary for the generation
// in one write, counted once, the cells otherwise as they were; a restarted
// observer keeps the downgrade, and a new generation starts from its
// declaration again.
func TestLaneTurnObserverDowngrades(t *testing.T) {
	f := newLaneObsFixture(t)
	f.start()
	f.see(laneRunning("t1"))
	before := f.batcher.count()
	f.o.observe(context.Background(), worker.TurnUpdate{Err: fmt.Errorf("scan: %w", runtime.ErrTurnIdentityUnsupported)})
	f.o.observe(context.Background(), worker.TurnUpdate{Err: fmt.Errorf("scan: %w", runtime.ErrTurnIdentityUnsupported)})
	if n := f.batcher.count() - before; n != 1 {
		t.Fatalf("downgrade wrote %d batches, want 1", n)
	}
	row := f.row()
	if got, want := row.Turn.Caps.String(), "start=boundary,end=boundary,fence=checked,link=none,lossless=false,source=transcript,interrupt=keystroke,replace=interrupt"; got != want {
		t.Fatalf("caps = %q, want %q", got, want)
	}
	if row.Turn.State != opjournal.TurnStateRunning || row.Turn.ID != "t1" {
		t.Fatalf("cells = %+v, want t1 still running", row.Turn)
	}
	if st := f.stats.status(); st.Downgrades != 1 {
		t.Fatalf("downgrades = %d, want 1", st.Downgrades)
	}
	f.restart()
	f.see(laneIdle(laneEnded("t1", runtime.TurnStatusCompleted)))
	if !strings.HasPrefix(f.row().Turn.Caps.String(), "start=boundary,") {
		t.Fatalf("caps = %q, want the downgrade kept within the generation", f.row().Turn.Caps)
	}
	f.gen = 2
	f.see(laneIdle())
	if !strings.HasPrefix(f.row().Turn.Caps.String(), "start=recorded,") {
		t.Fatalf("caps = %q, want generation 2's declaration", f.row().Turn.Caps)
	}
}

// TestLaneTurnObserverErrorsLeaveCells pins that an unavailable read writes
// nothing, and that a session that is gone ends the watch.
func TestLaneTurnObserverErrorsLeaveCells(t *testing.T) {
	f := newLaneObsFixture(t)
	f.start()
	f.see(laneRunning("t1"))
	before := f.batcher.count()
	f.o.observe(context.Background(), worker.TurnUpdate{Err: fmt.Errorf("read: %w", runtime.ErrRuntimeUnavailable)})
	if f.batcher.count() != before || f.o.state != laneTurnWatching {
		t.Fatalf("an unavailable read wrote %d batches, state %d", f.batcher.count()-before, f.o.state)
	}
	f.o.observe(context.Background(), worker.TurnUpdate{Err: fmt.Errorf("read: %w", runtime.ErrSessionNotFound)})
	if f.batcher.count() != before || f.o.state != laneTurnDone || f.o.updates() != nil {
		t.Fatalf("a gone session wrote %d batches, state %d", f.batcher.count()-before, f.o.state)
	}
	if st := f.stats.status(); st.Watching != 0 {
		t.Fatalf("watching = %d after the session went", st.Watching)
	}
	if row := f.row(); row.Turn.ID != "t1" || row.Turn.State != opjournal.TurnStateRunning {
		t.Fatalf("cells = %+v, want t1 left running for session.ended", row.Turn)
	}
}

// TestLaneTurnObserverRetriesRefusedBatches pins the write's recovery: a
// batch whose row guard was refused is read and folded again, and a fact
// another observer recorded first is dropped from the batch, not written
// twice.
func TestLaneTurnObserverRetriesRefusedBatches(t *testing.T) {
	f := newLaneObsFixture(t)
	f.start()
	f.see(laneRunning("t1"))
	f.batcher.refuse = func(b beads.GuardedBatch) error {
		return &beads.BatchItemError{Index: 0, ID: b.Items[0].Update.ID, Err: &beads.PreconditionFailedError{}}
	}
	if n := f.see(laneIdle(laneEnded("t1", runtime.TurnStatusCompleted))); n != 2 {
		t.Fatalf("a refused row guard took %d batches, want 2", n)
	}
	ended, err := opjournal.FactCreateItem("gc", opjournal.Fact{
		Kind: opjournal.FactTurnStarted, Conversation: laneObsConv, Generation: 1, Turn: "t2",
		Basis: opjournal.FactBasisRuntime, At: laneT0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Create(*ended.Create); err != nil {
		t.Fatal(err)
	}
	if n := f.see(laneRunning("t2", laneEnded("t1", runtime.TurnStatusCompleted))); n != 2 {
		t.Fatalf("a recorded fact took %d batches, want 2", n)
	}
	f.wantFacts("turn.started:t1", "turn.ended:t1:completed", "turn.started:t2")
	if row := f.row(); row.Turn.ID != "t2" {
		t.Fatalf("cells = %+v, want t2 running", row.Turn)
	}
}

// TestLaneTurnObserverNothingToWatch pins the sessions with no observer: no
// turn source, an unsupported watch and a gone session start none and are
// not tried again; a capabilities read that fails is noted and tried again.
func TestLaneTurnObserverNothingToWatch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*laneFakeTurns)
		want  laneTurnWatchState
	}{
		{"no source", func(h *laneFakeTurns) { h.caps = worker.TurnCapabilities{Source: worker.TurnSourceNone} }, laneTurnDone},
		{"unsupported", func(h *laneFakeTurns) { h.watchErr = runtime.ErrTurnIdentityUnsupported }, laneTurnDone},
		{"gone", func(h *laneFakeTurns) { h.capsErr = runtime.ErrSessionNotFound }, laneTurnDone},
		{"unavailable", func(h *laneFakeTurns) { h.capsErr = runtime.ErrRuntimeUnavailable }, laneTurnUnwatched},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaneObsFixture(t)
			tc.setup(f.turns)
			f.o.start(context.Background())
			f.o.start(context.Background())
			if f.o.state != tc.want || f.o.updates() != nil {
				t.Fatalf("state = %d, want %d and no updates", f.o.state, tc.want)
			}
			wantReads := 1
			if tc.want == laneTurnUnwatched {
				wantReads = 2
				if len(f.notes.all()) != 2 {
					t.Fatalf("notes = %v, want each failed start noted", f.notes.all())
				}
			}
			if f.turns.capsReads != wantReads {
				t.Fatalf("caps reads = %d, want %d", f.turns.capsReads, wantReads)
			}
			if f.stats.status() != nil && f.stats.status().Watching != 0 {
				t.Fatalf("stats = %+v, want no watch", f.stats.status())
			}
		})
	}
}

// TestLaneTurnCapsStampRoundTrips pins the stamp's form and that reading it
// back gives its axes.
func TestLaneTurnCapsStampRoundTrips(t *testing.T) {
	stamp := laneTurnCapsStamp(laneObsCaps)
	parsed, err := opjournal.ParseTurnCapsStamp(stamp.String())
	if err != nil {
		t.Fatal(err)
	}
	if got := laneTurnCapsFromStamp(parsed); got != laneObsCaps.TurnCapabilities {
		t.Fatalf("axes = %+v, want %+v", got, laneObsCaps.TurnCapabilities)
	}
	bare := laneTurnCapsStamp(worker.TurnCapabilities{})
	if got, want := bare.String(), "start=none,end=none,fence=none,link=none,lossless=false"; got != want {
		t.Fatalf("bare stamp = %q, want %q", got, want)
	}
	low := laneMinTurnCaps(laneObsCaps.TurnCapabilities, runtime.TurnCapabilities{
		Start: runtime.TurnStartRecorded, End: runtime.TurnEndInferred, Fence: runtime.TurnFenceAtomic, Link: runtime.TurnLinkNone,
	})
	want := runtime.TurnCapabilities{Start: runtime.TurnStartRecorded, End: runtime.TurnEndInferred, Fence: runtime.TurnFenceChecked, Link: runtime.TurnLinkNone}
	if low != want {
		t.Fatalf("min = %+v, want %+v", low, want)
	}
}

// TestLaneHostObservesTurnsOnTheWorker pins the hosting (DES-O1 §7.1): a
// conversation's worker starts its observer after a pass, folds the turn
// updates that arrive while it waits, and stops the watch when it exits.
func TestLaneHostObservesTurnsOnTheWorker(t *testing.T) {
	store := laneSignalStore{laneTestStore: newLaneTestStore(), turnBatches: make(chan struct{}, 8)}
	f := newLaneFixtureOver(t, store)
	st := f.laneStore(t)
	f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "later", func(r *opjournal.Request) {
		r.NotBefore = laneT0.Add(30 * time.Second)
	}))
	hh := newLaneHostHarness(t, f)
	turns := &laneFakeTurns{caps: laneObsCaps, ch: make(chan worker.TurnUpdate, 1)}
	watchCtx := make(chan context.Context, 1)
	hh.h.turnHandle = func(conv string) (worker.TurnHandle, error) {
		if conv != f.id {
			return nil, fmt.Errorf("turn handle for %s", conv)
		}
		return laneCtxTurns{laneFakeTurns: turns, ctxs: watchCtx}, nil
	}
	hh.start()
	if p := hh.nextPass(t); p.out.res.Took != opjournal.TakeNone {
		t.Fatalf("first pass = %+v, want a not_before wait", p.out.res)
	}
	var ctx context.Context
	select {
	case ctx = <-watchCtx:
	case <-time.After(laneWaitBudget):
		t.Fatal("the worker started no turn watch")
	}
	tm := hh.nextTimer(t)
	turns.ch <- worker.TurnUpdate{History: runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t1"}, ObservedAt: f.clk.Now()}}
	select {
	case <-store.turnBatches:
	case <-time.After(laneWaitBudget):
		t.Fatal("the observer wrote no turn batch while the worker waited")
	}
	if row, _, found, err := st.Runtime(f.id); err != nil || !found || row.Turn.ID != "t1" {
		t.Fatalf("runtime row = %+v (found %v, %v), want t1 running", row.Turn, found, err)
	}
	f.clk.Advance(30 * time.Second)
	tm.c <- f.clk.Now()
	if p := hh.nextPass(t); p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass after the timer = %+v, want accepted", p.out.res)
	}
	if hh.nextExit(t) != f.id {
		t.Fatal("the idle worker did not exit")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("the watch outlived its worker")
	}
	if turns.watches != 1 {
		t.Fatalf("watches = %d, want one per worker", turns.watches)
	}
	if s := hh.h.status(f.clk.Now()); s.Turns == nil || s.Turns.Writes != 1 || s.Turns.Watching != 0 {
		t.Fatalf("status turns = %+v, want one write and no watch", s.Turns)
	}
}

// laneSignalStore is a lane test store that signals each turn observer
// batch it applied.
type laneSignalStore struct {
	laneTestStore
	turnBatches chan struct{}
}

func (s laneSignalStore) ApplyGuarded(ctx context.Context, b beads.GuardedBatch) (beads.GuardedResult, error) {
	res, err := s.MemStore.ApplyGuarded(ctx, b)
	if err == nil && b.Reason == laneTurnBatchReason {
		s.turnBatches <- struct{}{}
	}
	return res, err
}

// laneCtxTurns reports each watch's context.
type laneCtxTurns struct {
	*laneFakeTurns
	ctxs chan context.Context
}

func (l laneCtxTurns) WatchTurns(ctx context.Context) (<-chan worker.TurnUpdate, error) {
	ch, err := l.laneFakeTurns.WatchTurns(ctx)
	l.ctxs <- ctx
	return ch, err
}
