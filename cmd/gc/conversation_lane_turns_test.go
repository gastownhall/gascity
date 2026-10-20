package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
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
	// since is the generation's start; genErr fails its read.
	since  time.Time
	genErr error
	// credits are the interrupt ops' refs the observer reads, creditsErr
	// fails that read, and creditReads counts the reads.
	credits     []opjournal.InterruptRef
	creditsErr  error
	creditReads int
	o           *laneTurnObserver
}

// laneObsObserver is the fixture observers' gc.rt.observer stamp.
const laneObsObserver = "ctl:test-host"

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
		handle: func() (worker.TurnHandle, error) { return f.turns, nil },
		generation: func() (laneTurnGeneration, error) {
			return laneTurnGeneration{Gen: f.gen, Live: f.live, Since: f.since}, f.genErr
		},
		rows: func() (laneTurnRows, error) {
			return laneTurnRows{store: f.store, batcher: f.batcher, prefix: "gc"}, nil
		},
		credits: func(gen int64) ([]opjournal.InterruptRef, error) {
			f.creditReads++
			if gen != f.gen {
				f.t.Errorf("credits read for generation %d, want %d", gen, f.gen)
			}
			return f.credits, f.creditsErr
		},
		observer: laneObsObserver,
		now:      f.clk.Now, note: f.notes.note, stats: f.stats,
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
	return laneFactList(f.t, f.store, laneObsConv, f.gen)
}

// laneFactList lists conv's facts in store as kind:turn[@g<gen>][:reason
// [:cause]], oldest first, naming the generation when it is not gen.
func laneFactList(t *testing.T, store beads.Store, conv string, gen int64) []string {
	t.Helper()
	rows, err := store.ListByLabel(opjournal.FactConversationLabel(conv), 0, beads.IncludeClosed)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, b := range rows {
		fact, err := opjournal.DecodeFact(b)
		if err != nil {
			t.Fatal(err)
		}
		s := fmt.Sprintf("%s:%s", fact.Kind, fact.Turn)
		if fact.Generation != gen {
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
// was told of ends its turn interrupted, naming the op, also when the end
// comes reads later; the ref is spent on that end, a zero ref changes
// nothing, and a cancel no interrupt names is interrupted_locally.
func TestLaneTurnObserverCreditsInterrupt(t *testing.T) {
	f := newLaneObsFixture(t)
	f.start()
	f.see(laneRunning("t1"))
	f.o.setInterrupting(opjournal.InterruptRef{Op: laneObsInterruptOp, Turn: "t1"})
	f.o.setInterrupting(opjournal.InterruptRef{})
	f.see(laneRunning("t1"))
	f.see(laneIdle(laneEnded("t1", runtime.TurnStatusCancelled)))
	if ref := f.o.interruptRef(); ref != (opjournal.InterruptRef{}) {
		t.Fatalf("ref = %+v after its end was folded, want it spent", ref)
	}
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
// nothing, and that a not-found one of a session whose row is live does not
// either and leaves the watch running: a protocol runtime answers not found
// until it runs, as while its session is woken.
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
	if f.batcher.count() != before || f.o.state != laneTurnWatching || f.o.updates() == nil {
		t.Fatalf("a not-found read of a live row wrote %d batches, state %d", f.batcher.count()-before, f.o.state)
	}
	if st := f.stats.status(); st.Watching != 1 {
		t.Fatalf("watching = %d after a not-found read of a live row, want 1", st.Watching)
	}
	if row := f.row(); row.Turn.ID != "t1" || row.Turn.State != opjournal.TurnStateRunning {
		t.Fatalf("cells = %+v, want t1 left running", row.Turn)
	}
	f.see(laneIdle(laneEnded("t1", runtime.TurnStatusCompleted)))
	f.wantFacts("turn.started:t1", "turn.ended:t1:completed")
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
// updates that arrive while it waits, stays while its cells show a turn
// running, and stops the watch when it exits.
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
	hh.nextPass(t)
	select {
	case c := <-hh.exits:
		t.Fatalf("the worker of %s exited while its cells show t1 running", c)
	case <-time.After(50 * time.Millisecond):
	}
	t1 := runtime.EndedTurn{TurnID: "t1", EndedAt: f.clk.Now(), Status: runtime.TurnStatusCompleted}
	turns.ch <- worker.TurnUpdate{History: runtime.TurnHistory{Recent: []runtime.EndedTurn{t1}, ObservedAt: f.clk.Now()}}
	select {
	case <-store.turnBatches:
	case <-time.After(laneWaitBudget):
		t.Fatal("the observer wrote no batch for t1's end")
	}
	select {
	case c := <-hh.exits:
		t.Fatalf("the worker of %s exited at the end of t1, which ran before its delivery", c)
	case <-time.After(50 * time.Millisecond):
	}
	t2 := runtime.EndedTurn{TurnID: "t2", EndedAt: f.clk.Now(), Status: runtime.TurnStatusCompleted}
	turns.ch <- worker.TurnUpdate{History: runtime.TurnHistory{Recent: []runtime.EndedTurn{t2, t1}, ObservedAt: f.clk.Now()}}
	if hh.nextExit(t) != f.id {
		t.Fatal("the worker did not exit once the delivered turn t2 ended")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("the watch outlived its worker")
	}
	if turns.watches != 1 {
		t.Fatalf("watches = %d, want one per worker", turns.watches)
	}
	if s := hh.h.status(f.clk.Now()); s.Turns == nil || s.Turns.Writes != 3 || s.Turns.Watching != 0 {
		t.Fatalf("status turns = %+v, want three writes and no watch", s.Turns)
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

// laneCompletedAt is a completed turn the source started at start.
func laneCompletedAt(id string, start time.Time) runtime.EndedTurn {
	return runtime.EndedTurn{TurnID: id, StartedAt: start, EndedAt: start.Add(time.Second), Status: runtime.TurnStatusCompleted}
}

// TestLaneTurnObserverPriorOpenTurnStaysOut pins TurnObservation.Recent's
// contract for the prior generation's open turn, which a resumed source still
// shows current until the new generation's first turn closes it: it is
// journaled under neither the new generation's reset read nor any later
// one, also after the observer restarts, and the new generation's own turns
// are.
func TestLaneTurnObserverPriorOpenTurnStaysOut(t *testing.T) {
	f := newLaneObsFixture(t)
	f.start()
	f.see(laneRunning("t1"))
	f.gen = 2
	f.see(laneRunning("t1"))
	f.restart()
	f.see(laneRunning("t1"))
	if row := f.row(); row.Turn.Generation != 2 || row.Turn.State == opjournal.TurnStateRunning {
		t.Fatalf("cells = %+v, want generation 2 with no turn running", row.Turn)
	}
	f.restart()
	f.see(laneRunning("u1", laneEnded("t1", "")))
	f.see(laneIdle(laneEnded("u1", runtime.TurnStatusCompleted), laneEnded("t1", "")))
	f.wantFacts("turn.started:t1@g1", "turn.started:u1", "turn.ended:u1:completed")
}

// TestLaneTurnObserverRestartBeforeTheFirstEnd pins that the reset leaves the
// boundary in the cells: an observer that restarts before the new
// generation's first end still leaves the prior generation's ended turns out.
func TestLaneTurnObserverRestartBeforeTheFirstEnd(t *testing.T) {
	f := newLaneObsFixture(t)
	f.start()
	f.see(laneRunning("t1"))
	f.see(laneIdle(laneEnded("t1", runtime.TurnStatusCompleted)))
	f.gen = 2
	f.see(laneIdle(laneEnded("t1", runtime.TurnStatusCompleted)))
	f.restart()
	f.see(laneRunning("u1", laneEnded("t1", runtime.TurnStatusCompleted)))
	f.restart()
	f.see(laneRunning("u1", laneEnded("t1", runtime.TurnStatusCompleted)))
	f.wantFacts("turn.started:t1@g1", "turn.ended:t1@g1:completed", "turn.started:u1")
}

// TestLaneTurnObserverPriorWindowStopsAtTheBoundary pins that the prior
// generation's window stops at the newest turn the cells name: an older one
// they do not name is the prior generation's too.
func TestLaneTurnObserverPriorWindowStopsAtTheBoundary(t *testing.T) {
	f := newLaneObsFixture(t)
	f.start()
	f.see(laneIdle(laneEnded("t1", runtime.TurnStatusCompleted), laneEnded("t0", runtime.TurnStatusCompleted)))
	f.gen = 2
	f.see(laneRunning("u1", laneEnded("t1", runtime.TurnStatusCompleted), laneEnded("t0", runtime.TurnStatusCompleted)))
	f.wantFacts("turn.started:t0@g1", "turn.ended:t0@g1:completed", "turn.started:t1@g1", "turn.ended:t1@g1:completed", "turn.started:u1")
}

// TestLaneTurnObserverPriorTurnsByStartTime pins the prior generation's turns
// no observer saw: those the source started before the generation began are
// left out, current or ended, and the generation's own are journaled.
func TestLaneTurnObserverPriorTurnsByStartTime(t *testing.T) {
	f := newLaneObsFixture(t)
	f.start()
	f.see(laneRunning("t1"))
	f.gen, f.since = 2, laneT0.Add(10*time.Minute)
	before, after := laneT0.Add(5*time.Minute), laneT0.Add(11*time.Minute)
	f.clk.Advance(15 * time.Minute)
	f.see(runtime.TurnHistory{
		Current: runtime.TurnState{TurnID: "t3"}, CurrentStart: before,
		Recent: []runtime.EndedTurn{laneCompletedAt("t2", before), laneCompletedAt("t1", laneT0)},
	})
	f.see(laneIdle(laneCompletedAt("t3", before), laneCompletedAt("t2", before)))
	f.see(runtime.TurnHistory{
		Current: runtime.TurnState{TurnID: "u1"}, CurrentStart: after,
		Recent: []runtime.EndedTurn{laneCompletedAt("t3", before)},
	})
	f.see(laneIdle(laneCompletedAt("u1", after), laneCompletedAt("t3", before)))
	f.wantFacts("turn.started:t1@g1", "turn.started:u1", "turn.ended:u1:completed")
}

// TestLaneTurnObserverDropsAReadFromBeforeTheGeneration pins the read's own
// generation: an update read before the current generation began is not
// folded under it.
func TestLaneTurnObserverDropsAReadFromBeforeTheGeneration(t *testing.T) {
	f := newLaneObsFixture(t)
	f.start()
	f.see(laneRunning("t1"))
	f.gen, f.since = 2, f.clk.Now().Add(time.Minute)
	if n := f.see(laneRunning("t9")); n != 0 {
		t.Fatalf("a read from before generation 2 wrote %d batches", n)
	}
	f.wantFacts("turn.started:t1@g1")
	if row := f.row(); row.Turn.Generation != 1 {
		t.Fatalf("cells = %+v, want generation 1's", row.Turn)
	}
	if _, pending := f.o.pending(f.clk.Now()); pending && f.o.retry != nil {
		t.Fatal("a read from before the generation was kept for a retry")
	}
}

// TestLaneTurnObserverCreditsInterruptFromItsOp pins that a gc interrupt's
// credit survives the worker that ran it (X11 §5.4): an observer that was
// never told of it reads the interrupt op's turn from the op rows, and only
// for an end a cancel or an inference reads.
func TestLaneTurnObserverCreditsInterruptFromItsOp(t *testing.T) {
	f := newLaneObsFixture(t)
	f.start()
	f.see(laneRunning("t1"))
	f.o.setInterrupting(opjournal.InterruptRef{Op: laneObsInterruptOp, Turn: "t1"})
	f.restart()
	f.credits = []opjournal.InterruptRef{{Op: laneObsInterruptOp, Turn: "t1"}}
	f.see(laneRunning("t1"))
	if f.creditReads != 0 {
		t.Fatalf("credit reads = %d with no end to credit", f.creditReads)
	}
	f.see(laneIdle(laneEnded("t1", runtime.TurnStatusCancelled)))
	if f.creditReads != 1 {
		t.Fatalf("credit reads = %d, want 1 for the cancel", f.creditReads)
	}
	f.see(laneRunning("t2", laneEnded("t1", runtime.TurnStatusCancelled)))
	f.see(laneIdle(laneEnded("t2", runtime.TurnStatusCompleted), laneEnded("t1", runtime.TurnStatusCancelled)))
	inferred := laneEnded("t3", "")
	inferred.Inferred = true
	f.credits = append(f.credits, opjournal.InterruptRef{Op: laneObsInterruptOp, Turn: "t3"})
	f.see(laneIdle(inferred, laneEnded("t2", runtime.TurnStatusCompleted)))
	if f.creditReads != 2 {
		t.Fatalf("credit reads = %d, want one more for the inferred end only", f.creditReads)
	}
	f.wantFacts("turn.started:t1", "turn.ended:t1:interrupted:"+laneObsInterruptOp,
		"turn.started:t2", "turn.ended:t2:completed",
		"turn.started:t3", "turn.ended:t3:interrupted:"+laneObsInterruptOp)
}

// TestLaneTurnObserverCreditReadFailureWritesNothing pins that an end whose
// credit cannot be read is not journaled without it: the fold waits for its
// retry rather than record interrupted_locally for good.
func TestLaneTurnObserverCreditReadFailureWritesNothing(t *testing.T) {
	f := newLaneObsFixture(t)
	f.start()
	f.see(laneRunning("t1"))
	f.creditsErr = errors.New("list unavailable")
	before := f.batcher.count()
	f.o.observe(context.Background(), worker.TurnUpdate{History: laneIdle(laneEnded("t1", runtime.TurnStatusCancelled))})
	if n := f.batcher.count() - before; n != 0 {
		t.Fatalf("a failed credit read wrote %d batches", n)
	}
	if d, pending := f.o.pending(f.clk.Now()); !pending || d != laneTurnRetryStart || f.o.retry == nil {
		t.Fatalf("pending = %s, %v, want a retry after %s", d, pending, laneTurnRetryStart)
	}
	f.creditsErr = nil
	f.credits = []opjournal.InterruptRef{{Op: laneObsInterruptOp, Turn: "t1"}}
	f.o.retryNow(context.Background())
	f.wantFacts("turn.started:t1", "turn.ended:t1:interrupted:"+laneObsInterruptOp)
}

// TestLaneTurnObserverRetriesADroppedFold pins that an update that could not
// be folded is kept and folded again: after a failed generation read and
// after writes that kept failing, with the retry's backoff growing, and that
// a newer history displaces the kept one while an unavailable read does not.
func TestLaneTurnObserverRetriesADroppedFold(t *testing.T) {
	f := newLaneObsFixture(t)
	f.start()
	f.genErr = errors.New("store unavailable")
	f.o.observe(context.Background(), worker.TurnUpdate{History: laneRunning("t1")})
	if d, pending := f.o.pending(f.clk.Now()); !pending || d != laneTurnRetryStart {
		t.Fatalf("pending = %s, %v, want a retry after %s", d, pending, laneTurnRetryStart)
	}
	f.o.retryNow(context.Background())
	if d, _ := f.o.pending(f.clk.Now()); d != 2*laneTurnRetryStart {
		t.Fatalf("second retry after %s, want %s", d, 2*laneTurnRetryStart)
	}
	f.o.observe(context.Background(), worker.TurnUpdate{Err: fmt.Errorf("read: %w", runtime.ErrRuntimeUnavailable)})
	f.genErr = nil
	f.o.retryNow(context.Background())
	if f.o.retry != nil {
		t.Fatal("the retry that folded is still kept")
	}
	f.wantFacts("turn.started:t1")

	refusals := 0
	var refuse func(beads.GuardedBatch) error
	refuse = func(beads.GuardedBatch) error {
		refusals++
		f.batcher.refuse = refuse // the batcher spends each refusal; re-arm it
		return errors.New("commit unknown")
	}
	f.batcher.refuse = refuse
	f.o.observe(context.Background(), worker.TurnUpdate{History: laneIdle(laneEnded("t1", runtime.TurnStatusCompleted))})
	f.batcher.mu.Lock()
	f.batcher.refuse = nil
	f.batcher.mu.Unlock()
	if refusals != laneTurnWriteAttempts || f.o.retry == nil {
		t.Fatalf("refusals = %d, retry kept %v; want %d attempts and the update kept", refusals, f.o.retry != nil, laneTurnWriteAttempts)
	}
	f.o.observe(context.Background(), worker.TurnUpdate{History: laneRunning("t2", laneEnded("t1", runtime.TurnStatusCompleted))})
	if f.o.retry != nil {
		t.Fatal("a newer history that folded left the old one kept")
	}
	f.wantFacts("turn.started:t1", "turn.ended:t1:completed", "turn.started:t2")
}

// TestLaneTurnObserverManyRecordedFactsDoNotWedge pins create-if-absent for
// a fold whose facts another writer recorded first, however many there are
// (X11 D2): they are dropped from the batch together and the cells advance.
func TestLaneTurnObserverManyRecordedFactsDoNotWedge(t *testing.T) {
	f := newLaneObsFixture(t)
	for _, id := range []string{"t1", "t2", "t3"} {
		for _, kind := range []opjournal.FactKind{opjournal.FactTurnStarted, opjournal.FactTurnEnded} {
			fact := opjournal.Fact{Kind: kind, Conversation: laneObsConv, Generation: 1, Turn: id, Basis: opjournal.FactBasisRuntime, At: laneT0}
			if kind == opjournal.FactTurnEnded {
				fact.Reason = string(opjournal.TurnEndCompleted)
			}
			item, err := opjournal.FactCreateItem("gc", fact)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.Create(*item.Create); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.start()
	if n := f.see(laneIdle(laneEnded("t3", runtime.TurnStatusCompleted), laneEnded("t2", runtime.TurnStatusCompleted), laneEnded("t1", runtime.TurnStatusCompleted))); n != 2 {
		t.Fatalf("the fold took %d batches, want 2", n)
	}
	if row := f.row(); row.Turn.LastID != "t3" {
		t.Fatalf("cells = %+v, want ended after t3", row.Turn)
	}
}

// TestLaneTurnObserverStampsObserverAndLag pins the observer stamp
// (gc.rt.observer, DES-O1 §5.1) on the row's first write and the
// observation-lag metric.
func TestLaneTurnObserverStampsObserverAndLag(t *testing.T) {
	f := newLaneObsFixture(t)
	f.start()
	hist := laneRunning("t1")
	hist.ObservedAt = f.clk.Now().Add(-1500 * time.Millisecond)
	f.o.observe(context.Background(), worker.TurnUpdate{History: hist})
	if row := f.row(); row.Observer != laneObsObserver {
		t.Fatalf("observer = %q, want %q", row.Observer, laneObsObserver)
	}
	if st := f.stats.status(); st.LastLagMS != 1500 {
		t.Fatalf("lag = %d ms, want 1500", st.LastLagMS)
	}
}

// TestLaneTurnObserverLeavesAClosedRow pins that a closed runtime row, the
// history of a closed conversation, is never written.
func TestLaneTurnObserverLeavesAClosedRow(t *testing.T) {
	f := newLaneObsFixture(t)
	f.start()
	f.see(laneRunning("t1"))
	id, err := opjournal.RuntimeRowID("gc", laneObsConv)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(id); err != nil {
		t.Fatal(err)
	}
	if n := f.see(laneIdle(laneEnded("t1", runtime.TurnStatusCompleted))); n != 0 {
		t.Fatalf("a closed row took %d batches", n)
	}
	f.wantFacts("turn.started:t1")
}

// TestLaneTurnObserverPending pins when the observer keeps its worker
// (DES-O1 §7.1): until the watch's first update is folded, while the cells
// show a turn running, and after a take until a later read shows the turn
// it started, or laneTurnExpectFor passes.
func TestLaneTurnObserverPending(t *testing.T) {
	f := newLaneObsFixture(t)
	if _, pending := f.o.pending(f.clk.Now()); pending {
		t.Fatal("an observer that does not watch is pending")
	}
	f.start()
	if d, pending := f.o.pending(f.clk.Now()); !pending || d != laneTurnHold {
		t.Fatalf("before the first update: %s, %v; want pending for %s", d, pending, laneTurnHold)
	}
	f.see(laneIdle())
	if _, pending := f.o.pending(f.clk.Now()); pending {
		t.Fatal("an idle session is pending")
	}
	f.o.expect(f.clk.Now())
	if d, pending := f.o.pending(f.clk.Now()); !pending || d != laneTurnExpectFor {
		t.Fatalf("after a take: %s, %v; want pending for %s", d, pending, laneTurnExpectFor)
	}
	f.see(laneIdle())
	if _, pending := f.o.pending(f.clk.Now()); !pending {
		t.Fatal("a read with no turn yet ended the expectation")
	}
	f.see(laneRunningAt("t1", f.clk.Now()))
	if d, pending := f.o.pending(f.clk.Now()); !pending || d != laneTurnHold {
		t.Fatalf("t1 running: %s, %v; want pending for %s", d, pending, laneTurnHold)
	}
	f.see(laneIdle(laneEnded("t1", runtime.TurnStatusCompleted)))
	if _, pending := f.o.pending(f.clk.Now()); pending {
		t.Fatal("still pending after the expected turn ended")
	}
	f.o.expect(f.clk.Now())
	f.clk.Advance(laneTurnExpectFor)
	if _, pending := f.o.pending(f.clk.Now()); pending {
		t.Fatal("still pending after laneTurnExpectFor")
	}
}

// laneHostTurns is a host harness whose conversation's turn handle is a fake
// the test sends updates on, over a store that signals each turn batch.
type laneHostTurns struct {
	f     *laneFixture
	store laneSignalStore
	hh    *laneHostHarness
	turns *laneFakeTurns
}

func newLaneHostTurns(t *testing.T, kinds ...string) *laneHostTurns {
	t.Helper()
	store := laneSignalStore{laneTestStore: newLaneTestStore(), turnBatches: make(chan struct{}, 32)}
	f := newLaneFixtureOver(t, store)
	hh := newLaneHostHarness(t, f, kinds...)
	turns := &laneFakeTurns{caps: laneObsCaps, ch: make(chan worker.TurnUpdate, 1)}
	hh.h.turnHandle = func(string) (worker.TurnHandle, error) { return turns, nil }
	return &laneHostTurns{f: f, store: store, hh: hh, turns: turns}
}

// send publishes hist, read now, as WatchTurns does.
func (l *laneHostTurns) send(hist runtime.TurnHistory) {
	hist.ObservedAt = l.f.clk.Now()
	l.turns.ch <- worker.TurnUpdate{History: hist}
}

// batch waits for the observer's next turn batch.
func (l *laneHostTurns) batch(t *testing.T, what string) {
	t.Helper()
	select {
	case <-l.store.turnBatches:
	case <-time.After(laneWaitBudget):
		t.Fatalf("no turn batch for %s (turns status %+v)", what, l.hh.h.status(l.f.clk.Now()).Turns)
	}
}

// noExit fails when the conversation's worker exits soon.
func (l *laneHostTurns) noExit(t *testing.T, why string) {
	t.Helper()
	select {
	case c := <-l.hh.exits:
		t.Fatalf("the worker of %s exited %s", c, why)
	case <-time.After(50 * time.Millisecond):
	}
}

func (l *laneHostTurns) watches() int {
	l.turns.mu.Lock()
	defer l.turns.mu.Unlock()
	return l.turns.watches
}

// TestLaneHostJournalsTheTurnItDelivers pins DES-O1 §7.7 for a message to an
// idle session: the worker that delivered it stays until its observer has
// journaled the turn the delivery started, start and end, then exits.
func TestLaneHostJournalsTheTurnItDelivers(t *testing.T) {
	l := newLaneHostTurns(t)
	st := l.f.laneStore(t)
	l.f.propose(t, st, l.f.msg("k1", opjournal.RolePrompt, "hello"))
	l.hh.start()
	if p := l.hh.nextPass(t); p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass = %+v, want the delivery accepted", p.out.res)
	}
	l.hh.nextPass(t)
	l.send(laneIdle())
	l.batch(t, "the first read")
	l.noExit(t, "before the delivered turn started")
	l.send(laneRunning("t1"))
	l.batch(t, "the delivered turn's start")
	l.noExit(t, "while the delivered turn runs")
	l.send(laneIdle(laneEnded("t1", runtime.TurnStatusCompleted)))
	l.batch(t, "the delivered turn's end")
	if l.hh.nextExit(t) != l.f.id {
		t.Fatal("the worker did not exit once the delivered turn ended")
	}
	if got, want := laneFactList(t, l.store, l.f.id, 1), []string{"turn.started:t1", "turn.ended:t1:completed"}; !slices.Equal(got, want) {
		t.Fatalf("facts = %q, want %q", got, want)
	}
	if l.watches() != 1 {
		t.Fatalf("watches = %d, want one", l.watches())
	}
}

// laneReadyExec probes a running live runtime that is busy until ready is
// set, delivers whatever it is given and settles interrupts on the turn it
// is given.
type laneReadyExec struct {
	clk   *laneTestClock
	ready *atomic.Bool
}

func (e laneReadyExec) Probe(_ context.Context, conv opjournal.ConversationView) (opjournal.Probe, error) {
	boundary := opjournal.TriNo
	if e.ready.Load() {
		boundary = opjournal.TriYes
	}
	return opjournal.Probe{
		Available: true, Running: true, Boundary: boundary, Fence: laneFence(conv),
		Caps: opjournal.Caps{Live: true, Input: opjournal.FenceAtomic},
	}, nil
}

func (e laneReadyExec) Deliver(context.Context, opjournal.Delivery) opjournal.DeliveryResult {
	return opjournal.Delivered{Record: opjournal.DeliveryRecord{At: e.clk.Now(), Ack: opjournal.AckDelivered}, Via: "test"}
}

func (laneReadyExec) Interrupt(_ context.Context, c opjournal.InterruptCall) opjournal.ControlResult {
	return opjournal.Settled{Turn: c.Turn, Via: "test"}
}

func (laneReadyExec) Respond(context.Context, opjournal.RespondCall) opjournal.ControlResult {
	return opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonUnsupported}
}

// TestLaneHostJournalsBusyThenDelivered pins DES-O1 §7.7 for a message to a
// busy session: the observer journals the running turn while the worker
// waits for its boundary, then the turn the delivery started.
func TestLaneHostJournalsBusyThenDelivered(t *testing.T) {
	l := newLaneHostTurns(t)
	st := l.f.laneStore(t)
	l.f.propose(t, st, l.f.msg("k1", opjournal.RolePrompt, "hello"))
	var ready atomic.Bool
	l.hh.h.newExec = func(laneExecDeps, session.Info) opjournal.Executor {
		return laneReadyExec{clk: l.f.clk, ready: &ready}
	}
	l.hh.start()
	if p := l.hh.nextPass(t); !p.out.res.Busy {
		t.Fatalf("first pass = %+v, want busy", p.out.res)
	}
	tm := l.hh.nextTimer(t)
	if tm.d != laneBusyFirst {
		t.Fatalf("wait = %s, want the busy backoff %s", tm.d, laneBusyFirst)
	}
	l.send(laneRunning("t0"))
	l.batch(t, "the busy turn")
	ready.Store(true)
	l.f.clk.Advance(tm.d)
	tm.c <- l.f.clk.Now()
	if p := l.hh.nextPass(t); p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass at the boundary = %+v, want the delivery accepted", p.out.res)
	}
	take := l.f.clk.Now()
	l.hh.nextPass(t)
	l.noExit(t, "with t0 running in its cells")
	l.send(laneRunningAt("t1", take, laneEnded("t0", runtime.TurnStatusCompleted)))
	l.batch(t, "the delivered turn's start")
	l.send(laneIdle(laneCompletedAt("t1", take), laneEnded("t0", runtime.TurnStatusCompleted)))
	l.batch(t, "the delivered turn's end")
	if l.hh.nextExit(t) != l.f.id {
		t.Fatal("the worker did not exit once the delivered turn ended")
	}
	want := []string{"turn.started:t0", "turn.ended:t0:completed", "turn.started:t1", "turn.ended:t1:completed"}
	if got := laneFactList(t, l.store, l.f.id, 1); !slices.Equal(got, want) {
		t.Fatalf("facts = %q, want %q", got, want)
	}
}

// TestLaneHostCreditsAnInterruptOpRunThroughTheExecutor pins X11 §5.4 end to
// end: an interrupt op the lane takes and its executor settles on t1 credits
// t1's cancel to the op, read from the op row, so the fact names it.
func TestLaneHostCreditsAnInterruptOpRunThroughTheExecutor(t *testing.T) {
	l := newLaneHostTurns(t)
	st := l.f.laneStore(t)
	op := l.f.propose(t, st, opjournal.Request{
		Conversation: l.f.id, Kind: opjournal.KindInterrupt, Executor: opjournal.ExecutorLane, ClientKey: "stop:1:t1",
		Producer: opjournal.Producer{Principal: "operator:alice", Actor: opjournal.ActorOperator},
		Scope:    opjournal.Scope{Kind: opjournal.ScopeSession, Generation: 1, Turn: "t1"},
	})
	var ready atomic.Bool
	l.hh.h.newExec = func(laneExecDeps, session.Info) opjournal.Executor {
		return laneReadyExec{clk: l.f.clk, ready: &ready}
	}
	l.hh.start()
	if p := l.hh.nextPass(t); p.out.res.Took == opjournal.TakeNone {
		t.Fatalf("pass = %+v, want the interrupt taken", p.out.res)
	}
	if got := l.f.get(t, st, op.ID); got.Outcome.Arm != opjournal.ArmSucceeded || got.Outcome.Result.Turn != "t1" {
		t.Fatalf("interrupt op = %+v, want succeeded on t1", got.Outcome)
	}
	l.hh.nextPass(t)
	l.send(laneIdle(laneEnded("t1", runtime.TurnStatusCancelled)))
	l.batch(t, "the interrupted turn")
	if l.hh.nextExit(t) != l.f.id {
		t.Fatal("the worker did not exit")
	}
	want := []string{"turn.started:t1", "turn.ended:t1:interrupted:" + op.ID}
	if got := laneFactList(t, l.store, l.f.id, 1); !slices.Equal(got, want) {
		t.Fatalf("facts = %q, want %q", got, want)
	}
}

// laneListFailStore fails every List, as a store whose keyed reads fail.
type laneListFailStore struct{ laneSignalStore }

func (laneListFailStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("list unavailable")
}

// TestLaneHostWatchesTurnsOnlyForLaneWork pins DES-O1 §15 for the
// observer: with [ops] authoritative empty, neither a pass that found no lane
// work nor a pass that failed starts a turn watch; with a kind listed, a pass
// does.
func TestLaneHostWatchesTurnsOnlyForLaneWork(t *testing.T) {
	t.Run("no lane work", func(t *testing.T) {
		l := newLaneHostTurns(t)
		l.hh.start()
		if !l.hh.h.Poke(l.f.id) {
			t.Fatal("Poke refused")
		}
		l.hh.nextPass(t)
		if l.hh.nextExit(t) != l.f.id {
			t.Fatal("the idle worker did not exit")
		}
		if l.watches() != 0 {
			t.Fatalf("watches = %d, want none", l.watches())
		}
	})
	t.Run("failed pass", func(t *testing.T) {
		l := newLaneHostTurns(t)
		l.hh.env.setStore(laneListFailStore{l.store})
		l.hh.start()
		if !l.hh.h.Poke(l.f.id) {
			t.Fatal("Poke refused")
		}
		if p := l.hh.nextPass(t); p.out.err == nil {
			t.Fatalf("pass = %+v, want it to fail on the list", p.out)
		}
		l.hh.nextTimer(t)
		if l.watches() != 0 {
			t.Fatalf("watches = %d after a failed pass, want none", l.watches())
		}
	})
	t.Run("kind listed", func(t *testing.T) {
		l := newLaneHostTurns(t, string(opjournal.KindMessage))
		l.hh.start()
		if !l.hh.h.Poke(l.f.id) {
			t.Fatal("Poke refused")
		}
		l.hh.nextPass(t)
		l.send(laneIdle())
		l.batch(t, "the first read")
		if l.hh.nextExit(t) != l.f.id {
			t.Fatal("the worker did not exit after its first read")
		}
		if l.watches() != 1 {
			t.Fatalf("watches = %d, want one", l.watches())
		}
	})
}

// laneTurnFailStore refuses every turn observer batch.
type laneTurnFailStore struct{ laneSignalStore }

func (s laneTurnFailStore) ApplyGuarded(ctx context.Context, b beads.GuardedBatch) (beads.GuardedResult, error) {
	if b.Reason == laneTurnBatchReason {
		return beads.GuardedResult{}, errors.New("commit unknown")
	}
	return s.laneSignalStore.ApplyGuarded(ctx, b)
}

// TestLaneHostTurnFailuresAreNotLaneFailures pins that the observer's
// failures are counted as its own, not as the lane's pass errors the doctor
// reports, and logged under a stage of their own, apart from the lane
// executor's turn-evidence degradations ("turns"), so neither one's rate
// limit silences the other.
func TestLaneHostTurnFailuresAreNotLaneFailures(t *testing.T) {
	l := newLaneHostTurns(t, string(opjournal.KindMessage))
	l.hh.env.setStore(laneTurnFailStore{l.store})
	l.hh.start()
	if !l.hh.h.Poke(l.f.id) {
		t.Fatal("Poke refused")
	}
	l.hh.nextPass(t)
	l.send(laneRunning("t1"))
	logged := func() bool {
		return slices.ContainsFunc(l.hh.logLines(), func(line string) bool {
			return strings.HasPrefix(line, "conversation-lanes: turn-observer failed: ")
		})
	}
	deadline := time.After(laneWaitBudget)
	for {
		s := l.hh.h.status(l.f.clk.Now())
		if s.Turns != nil && s.Turns.Errors > 0 && logged() {
			if s.Errors != 0 || s.ErrorsSinceSuccess != 0 || s.LastError != "" {
				t.Fatalf("lane status = %+v, want the observer's failure kept out of it", s)
			}
			if !strings.Contains(s.Turns.LastError, "commit unknown") {
				t.Fatalf("turns last error = %q", s.Turns.LastError)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("no observer failure counted and logged under its own stage (status %+v, log %q)", s, l.hh.logLines())
		case <-time.After(time.Millisecond):
		}
	}
}

// TestLaneHostRetriesATurnUpdateDroppedWhileQuiescent pins that an update
// that arrives while the city is quiescent is folded once it is not: the
// worker waits for the retry's backoff, not for the next update.
func TestLaneHostRetriesATurnUpdateDroppedWhileQuiescent(t *testing.T) {
	l := newLaneHostTurns(t, string(opjournal.KindMessage))
	l.hh.start()
	if !l.hh.h.Poke(l.f.id) {
		t.Fatal("Poke refused")
	}
	l.hh.nextPass(t)
	if tm := l.hh.nextTimer(t); tm.d != laneTurnHold {
		t.Fatalf("wait = %s, want the observer's hold %s", tm.d, laneTurnHold)
	}
	l.hh.env.quiescent.Store(true)
	l.send(laneRunning("t1"))
	retry := l.hh.nextTimer(t)
	if retry.d != laneTurnRetryStart {
		t.Fatalf("wait = %s, want the retry backoff %s", retry.d, laneTurnRetryStart)
	}
	l.hh.env.quiescent.Store(false)
	l.f.clk.Advance(retry.d)
	retry.c <- l.f.clk.Now()
	l.batch(t, "the retried update")
	if got := laneFactList(t, l.store, l.f.id, 1); !slices.Equal(got, []string{"turn.started:t1"}) {
		t.Fatalf("facts = %q", got)
	}
}

// laneHostParkWatchedTurn delivers a prompt to l's conversation on a host of
// one slot, journals its turn t1 running, and parks the observing worker for
// another queued conversation; it returns once the worker the park starts
// has passed, watching again.
func laneHostParkWatchedTurn(t *testing.T, l *laneHostTurns) {
	t.Helper()
	other := addSession(t, l.f, "b")
	st := l.f.laneStore(t)
	l.f.propose(t, st, l.f.msg("k1", opjournal.RolePrompt, "hello"))
	l.hh.h.maxWorkers = 1
	l.hh.start()
	if p := l.hh.nextPass(t); p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass = %+v, want the delivery accepted", p.out.res)
	}
	l.hh.nextPass(t)
	hold := l.hh.nextTimer(t)
	if hold.d != laneTurnHold {
		t.Fatalf("wait = %s, want the observer's hold %s", hold.d, laneTurnHold)
	}
	l.send(laneRunning("t1"))
	l.batch(t, "the delivered turn's start")
	if !l.hh.h.Poke(other) {
		t.Fatal("Poke refused")
	}
	l.f.clk.Advance(hold.d)
	hold.c <- l.f.clk.Now()
	if l.hh.nextExit(t) != l.f.id {
		t.Fatal("the observing worker kept its slot with a conversation queued")
	}
	if p := l.hh.nextPass(t); p.conv != other {
		t.Fatalf("pass over %s, want the queued %s", p.conv, other)
	}
	if l.hh.nextExit(t) != other {
		t.Fatal("the queued conversation's worker did not exit")
	}
	park := l.hh.nextTimer(t)
	if park.d != laneTurnHold {
		t.Fatalf("park = %s, want %s", park.d, laneTurnHold)
	}
	l.f.clk.Advance(park.d)
	park.c <- l.f.clk.Now()
	if p := l.hh.nextPass(t); p.conv != l.f.id || p.out.res.Took != opjournal.TakeNone {
		t.Fatalf("pass = %+v, want the parked conversation's, with nothing to take", p)
	}
}

// TestLaneHostParkedObservationsResume pins the observer under slot
// contention: a worker holding its slot only for a running turn parks once
// another conversation queues, and the worker its park starts watches again
// even though its pass finds no lane work, so the turn's end is journaled.
func TestLaneHostParkedObservationsResume(t *testing.T) {
	l := newLaneHostTurns(t)
	laneHostParkWatchedTurn(t, l)
	l.send(laneIdle(laneEnded("t1", runtime.TurnStatusCompleted)))
	l.batch(t, "the delivered turn's end")
	if l.hh.nextExit(t) != l.f.id {
		t.Fatal("the resumed worker did not exit once the turn ended")
	}
	if got, want := laneFactList(t, l.store, l.f.id, 1), []string{"turn.started:t1", "turn.ended:t1:completed"}; !slices.Equal(got, want) {
		t.Fatalf("facts = %q, want %q", got, want)
	}
	if l.watches() != 2 {
		t.Fatalf("watches = %d, want one per worker of %s", l.watches(), l.f.id)
	}
}

// laneRunningAt is turn id running, started by the source at start.
func laneRunningAt(id string, start time.Time, recent ...runtime.EndedTurn) runtime.TurnHistory {
	return runtime.TurnHistory{Current: runtime.TurnState{TurnID: id}, CurrentStart: start, Recent: recent}
}

// TestLaneTurnObserverExpectsTheTakesOwnTurn pins DES-O1 §7.7's expectation
// to the turn the take could have started: a turn the source started before
// the take, or one the cells named at the take, does not end it, even read
// after the take; a turn started from the take does, running or ended.
func TestLaneTurnObserverExpectsTheTakesOwnTurn(t *testing.T) {
	t.Run("busy then delivered", func(t *testing.T) {
		f := newLaneObsFixture(t)
		f.start()
		f.see(laneRunning("t0"))
		take := f.clk.Now()
		f.o.expect(take)
		f.see(laneIdle(laneEnded("t0", runtime.TurnStatusCompleted)))
		if _, pending := f.o.pending(f.clk.Now()); !pending {
			t.Fatal("the busy turn's end, read after the take, ended the expectation")
		}
		f.see(laneIdle(laneCompletedAt("t1", take.Add(time.Second)), laneEnded("t0", runtime.TurnStatusCompleted)))
		if _, pending := f.o.pending(f.clk.Now()); pending {
			t.Fatal("still pending after the delivered turn ended")
		}
	})
	t.Run("idle with an earlier unjournaled turn", func(t *testing.T) {
		f := newLaneObsFixture(t)
		f.clk.Advance(time.Hour)
		f.start()
		take := f.clk.Now()
		f.o.expect(take)
		f.see(laneIdle(laneCompletedAt("t0", take.Add(-10*time.Minute))))
		if _, pending := f.o.pending(f.clk.Now()); !pending {
			t.Fatal("an earlier turn the source started before the take ended the expectation")
		}
		f.see(laneRunningAt("t1", take.Add(time.Second), laneCompletedAt("t0", take.Add(-10*time.Minute))))
		f.see(laneIdle(laneCompletedAt("t1", take.Add(time.Second)), laneCompletedAt("t0", take.Add(-10*time.Minute))))
		if _, pending := f.o.pending(f.clk.Now()); pending {
			t.Fatal("still pending after the delivered turn ended")
		}
		f.wantFacts("turn.started:t0", "turn.ended:t0:completed", "turn.started:t1", "turn.ended:t1:completed")
	})
	t.Run("no recorded start", func(t *testing.T) {
		f := newLaneObsFixture(t)
		f.start()
		f.see(runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t0"}})
		f.o.expect(f.clk.Now())
		f.see(laneIdle(runtime.EndedTurn{TurnID: "t0", Status: runtime.TurnStatusCompleted}))
		if _, pending := f.o.pending(f.clk.Now()); !pending {
			t.Fatal("the end of a turn the cells named at the take ended the expectation")
		}
		f.see(laneIdle(runtime.EndedTurn{TurnID: "t1", Status: runtime.TurnStatusCompleted}, runtime.EndedTurn{TurnID: "t0", Status: runtime.TurnStatusCompleted}))
		if _, pending := f.o.pending(f.clk.Now()); pending {
			t.Fatal("a turn the cells did not name at the take, journaled after it, left the expectation")
		}
	})
	t.Run("read before the take", func(t *testing.T) {
		f := newLaneObsFixture(t)
		f.start()
		f.see(laneIdle())
		read := f.clk.Now()
		f.clk.Advance(time.Second)
		f.o.expect(f.clk.Now())
		hist := laneIdle(runtime.EndedTurn{TurnID: "t1", Status: runtime.TurnStatusCompleted})
		hist.ObservedAt = read
		f.o.observe(context.Background(), worker.TurnUpdate{History: hist})
		f.wantFacts("turn.started:t1", "turn.ended:t1:completed")
		if _, pending := f.o.pending(f.clk.Now()); !pending {
			t.Fatal("a read made before the take ended the expectation")
		}
	})
}

// TestLaneTurnObserverExpectationOutlivesARunningTurn pins the expectation's
// window past a turn the take found running: the turn the take started can
// begin only after that one ends, so the window starts again at its end, also
// when it ran past laneTurnExpectFor, and closes laneTurnExpectFor later.
func TestLaneTurnObserverExpectationOutlivesARunningTurn(t *testing.T) {
	t0 := laneEnded("t0", runtime.TurnStatusCompleted)
	runPast := func(t *testing.T) *laneObsFixture {
		t.Helper()
		f := newLaneObsFixture(t)
		f.start()
		f.see(laneRunning("t0"))
		f.o.expect(f.clk.Now())
		f.clk.Advance(2 * laneTurnExpectFor)
		if d, pending := f.o.pending(f.clk.Now()); !pending || d != laneTurnHold {
			t.Fatalf("t0 running past the window: %s, %v; want held for %s", d, pending, laneTurnHold)
		}
		f.see(laneIdle(t0))
		if _, pending := f.o.pending(f.clk.Now()); !pending {
			t.Fatal("the end of the turn the take found running ended the expectation")
		}
		return f
	}
	t.Run("delivered after it", func(t *testing.T) {
		f := runPast(t)
		began := f.clk.Now()
		f.see(laneRunningAt("t1", began, t0))
		f.see(laneIdle(laneCompletedAt("t1", began), t0))
		if _, pending := f.o.pending(f.clk.Now()); pending {
			t.Fatal("still pending after the delivered turn ended")
		}
		f.wantFacts("turn.started:t0", "turn.ended:t0:completed", "turn.started:t1", "turn.ended:t1:completed")
	})
	t.Run("nothing after it", func(t *testing.T) {
		f := runPast(t)
		f.clk.Advance(laneTurnExpectFor)
		if _, pending := f.o.pending(f.clk.Now()); pending {
			t.Fatal("still pending laneTurnExpectFor after the running turn ended")
		}
	})
}

// TestLaneTurnObserverExpectsBlindBeforeTheFirstFold pins the expectation of
// a take made before the observer folded anything, for a source that records
// no start: which turns the take found is unknown, so no turn without a start
// meets it until a fold names them; the turns that fold names are the ones
// the take found, and a later one meets it.
func TestLaneTurnObserverExpectsBlindBeforeTheFirstFold(t *testing.T) {
	bare := func(id string) runtime.EndedTurn {
		return runtime.EndedTurn{TurnID: id, Status: runtime.TurnStatusCompleted}
	}
	for _, tc := range []struct {
		name  string
		first func(*laneObsFixture)
	}{
		{"before any update", func(*laneObsFixture) {}},
		{"after an unavailable read", func(f *laneObsFixture) {
			f.o.observe(context.Background(), worker.TurnUpdate{Err: fmt.Errorf("read: %w", runtime.ErrRuntimeUnavailable)})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaneObsFixture(t)
			f.start()
			tc.first(f)
			f.o.expect(f.clk.Now())
			f.see(laneIdle(bare("t0")))
			if _, pending := f.o.pending(f.clk.Now()); !pending {
				t.Fatal("an earlier turn the first fold journaled ended the expectation")
			}
			f.see(laneIdle(bare("t1"), bare("t0")))
			if _, pending := f.o.pending(f.clk.Now()); pending {
				t.Fatal("a turn after the ones the first fold named left the expectation")
			}
			f.wantFacts("turn.started:t0", "turn.ended:t0:completed", "turn.started:t1", "turn.ended:t1:completed")
		})
	}
}

// TestLaneTurnObserverFirstUpdateEndsItsHold pins the hold for the watch's
// first update: whatever that update brings ends it, so a source that keeps
// failing in one class, which WatchTurns reports once, does not hold the
// worker; a failed read whose generation could not be read either, which
// leaves nothing to keep, included.
func TestLaneTurnObserverFirstUpdateEndsItsHold(t *testing.T) {
	unavailable := worker.TurnUpdate{Err: fmt.Errorf("read: %w", runtime.ErrRuntimeUnavailable)}
	for _, tc := range []struct {
		name   string
		genErr error
		update func(*laneObsFixture) worker.TurnUpdate
	}{
		{"a failed read", nil, func(*laneObsFixture) worker.TurnUpdate { return unavailable }},
		{"a failed read, generation unread", errors.New("sessions unavailable"), func(*laneObsFixture) worker.TurnUpdate { return unavailable }},
		{"a read from before the generation", nil, func(f *laneObsFixture) worker.TurnUpdate {
			f.since = f.clk.Now().Add(time.Second)
			hist := laneRunning("t9")
			hist.ObservedAt = f.clk.Now()
			return worker.TurnUpdate{History: hist}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaneObsFixture(t)
			f.start()
			f.genErr = tc.genErr
			f.o.observe(context.Background(), tc.update(f))
			if d, pending := f.o.pending(f.clk.Now()); pending {
				t.Fatalf("pending %s after the first update, want nothing (retry %+v)", d, f.o.retry)
			}
			if f.o.state != laneTurnWatching {
				t.Fatalf("observer state = %d, want watching", f.o.state)
			}
		})
	}
}

// TestLaneTurnObserverIgnoredUpdatesKeepTheRetry pins that an update with
// nothing to fold, a read from before the generation or a failed read, leaves
// an update kept for its retry as it is, so the retry still writes it.
func TestLaneTurnObserverIgnoredUpdatesKeepTheRetry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kept    worker.TurnUpdate
		ignored func(*laneObsFixture) worker.TurnUpdate
		check   func(*laneObsFixture)
	}{
		{
			name: "a history, then a read from before the generation",
			kept: worker.TurnUpdate{History: laneRunning("t1")},
			ignored: func(f *laneObsFixture) worker.TurnUpdate {
				hist := laneIdle()
				hist.ObservedAt = f.since.Add(-time.Second)
				return worker.TurnUpdate{History: hist}
			},
			check: func(f *laneObsFixture) { f.wantFacts("turn.started:t1") },
		},
		{
			name: "a downgrade, then a failed read",
			kept: worker.TurnUpdate{Err: fmt.Errorf("scan: %w", runtime.ErrTurnIdentityUnsupported)},
			ignored: func(*laneObsFixture) worker.TurnUpdate {
				return worker.TurnUpdate{Err: fmt.Errorf("read: %w", runtime.ErrRuntimeUnavailable)}
			},
			check: func(f *laneObsFixture) {
				if got := f.row().Turn.Caps.String(); !strings.HasPrefix(got, "start=boundary,") {
					t.Fatalf("caps = %q, want the kept downgrade written", got)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaneObsFixture(t)
			f.since = f.clk.Now()
			f.start()
			f.genErr = errors.New("sessions unavailable")
			kept := tc.kept
			kept.History.ObservedAt = f.clk.Now()
			f.o.observe(context.Background(), kept)
			f.genErr = nil
			f.o.observe(context.Background(), tc.ignored(f))
			if f.o.retry == nil || (f.o.retry.Err == nil) != (kept.Err == nil) {
				t.Fatalf("kept %+v after an update with nothing to fold, want %+v", f.o.retry, kept)
			}
			f.o.retryNow(context.Background())
			if f.o.retry != nil {
				t.Fatalf("the retry did not fold; notes %v", f.notes.all())
			}
			tc.check(f)
		})
	}
}

// TestLaneTurnObserverSpendsARetryFromAnEarlierGeneration pins that a kept
// history read before a generation that began meanwhile is spent when it is
// retried, not retried for good.
func TestLaneTurnObserverSpendsARetryFromAnEarlierGeneration(t *testing.T) {
	f := newLaneObsFixture(t)
	f.start()
	f.see(laneIdle())
	f.genErr = errors.New("sessions unavailable")
	hist := laneRunning("t1")
	hist.ObservedAt = f.clk.Now()
	f.o.observe(context.Background(), worker.TurnUpdate{History: hist})
	if f.o.retry == nil {
		t.Fatal("the history was not kept for its retry")
	}
	f.genErr = nil
	f.gen, f.since = 2, f.clk.Now().Add(time.Minute)
	f.o.retryNow(context.Background())
	if f.o.retry != nil || f.o.retries != 0 {
		t.Fatalf("retry %+v (%d attempts) after its generation passed, want it spent", f.o.retry, f.o.retries)
	}
	if _, pending := f.o.pending(f.clk.Now()); pending {
		t.Fatal("pending after the stale retry was spent")
	}
	f.wantFacts()
}

// TestLaneTurnObserverRunningHoldEndsWithTheRuntime pins the bound on the
// hold for a turn the cells show running: when the worker's hold ends on its
// timer the observer looks at the runtime, and one that is not running ends
// the hold, its open turn left to session.ended (X11 D8); a failed look
// keeps it, and the next batch that lands holds again.
func TestLaneTurnObserverRunningHoldEndsWithTheRuntime(t *testing.T) {
	f := newLaneObsFixture(t)
	var up atomic.Bool
	var upErr error
	looks := 0
	f.o.deps.running = func(context.Context) (bool, error) {
		looks++
		return up.Load(), upErr
	}
	f.start()
	f.see(laneRunning("t1"))
	up.Store(true)
	f.o.timerFired(context.Background())
	if _, pending := f.o.pending(f.clk.Now()); !pending || looks != 1 {
		t.Fatalf("pending %v after %d looks, want held while the runtime runs", pending, looks)
	}
	up.Store(false)
	upErr = errors.New("store unavailable")
	f.o.timerFired(context.Background())
	if _, pending := f.o.pending(f.clk.Now()); !pending {
		t.Fatal("a failed look ended the hold")
	}
	if n := f.notes.all(); len(n) != 1 || !strings.Contains(n[0], "store unavailable") {
		t.Fatalf("notes = %v, want the failed look", n)
	}
	f.notes = &laneNotes{}
	f.o.deps.note = f.notes.note
	upErr = nil
	f.o.timerFired(context.Background())
	if _, pending := f.o.pending(f.clk.Now()); pending {
		t.Fatal("a stopped runtime's open turn still holds the worker")
	}
	f.see(laneRunningAt("t2", f.clk.Now(), laneEnded("t1", runtime.TurnStatusCompleted)))
	if _, pending := f.o.pending(f.clk.Now()); !pending {
		t.Fatal("a new running turn does not hold the worker")
	}
	f.see(laneRunning("t2", laneEnded("t1", runtime.TurnStatusCompleted)))
	if looks != 3 {
		t.Fatalf("looks = %d, want one per hold timer", looks)
	}
}

// laneFactGetFailStore fails Get of one fact id, as a transient read.
type laneFactGetFailStore struct {
	laneTestStore
	failID string
}

func (s laneFactGetFailStore) Get(id string) (beads.Bead, error) {
	if id == s.failID {
		return beads.Bead{}, errors.New("store unavailable")
	}
	return s.laneTestStore.Get(id)
}

// TestLaneTurnObserverRetriesAnUnreadRecordedFact pins that a fold whose
// batch was refused for a fact another writer recorded, and whose read-back
// of that fact fails, is kept for its retry rather than dropped.
func TestLaneTurnObserverRetriesAnUnreadRecordedFact(t *testing.T) {
	f := newLaneObsFixture(t)
	fact := opjournal.Fact{Kind: opjournal.FactTurnStarted, Conversation: laneObsConv, Generation: 1, Turn: "t1", Basis: opjournal.FactBasisRuntime, At: laneT0}
	item, err := opjournal.FactCreateItem("gc", fact)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Create(*item.Create); err != nil {
		t.Fatal(err)
	}
	failing := laneFactGetFailStore{laneTestStore: f.store, failID: item.Create.ID}
	rows := laneTurnRows{store: failing, batcher: f.batcher, prefix: "gc"}
	f.o.deps.rows = func() (laneTurnRows, error) { return rows, nil }
	f.start()
	hist := laneRunning("t1")
	hist.ObservedAt = f.clk.Now()
	f.o.observe(context.Background(), worker.TurnUpdate{History: hist})
	if f.o.retry == nil {
		t.Fatalf("dropped without a retry: cells %+v, notes %v", f.o.cells, f.notes.all())
	}
	if n := f.notes.all(); len(n) != 1 || !strings.Contains(n[0], "store unavailable") {
		t.Fatalf("notes = %v, want the failed read", n)
	}
	rows.store = f.store
	f.o.retryNow(context.Background())
	if f.o.retry != nil {
		t.Fatalf("the retry did not fold; notes %v", f.notes.all())
	}
	if row := f.row(); row.Turn.State != opjournal.TurnStateRunning || row.Turn.ID != "t1" {
		t.Fatalf("cells = %+v, want t1 running", row.Turn)
	}
	f.wantFacts("turn.started:t1")
}

// TestLaneTurnObserverResetLeavesOutTheAnchor pins laneTurnPrior at a reset
// whose prior generation journaled no end of its own: the boundary it kept in
// Seen is the prior's, so the next generation journals it neither.
func TestLaneTurnObserverResetLeavesOutTheAnchor(t *testing.T) {
	f := newLaneObsFixture(t)
	f.start()
	f.see(laneIdle(laneEnded("t1", runtime.TurnStatusCompleted)))
	f.gen = 2
	f.see(laneIdle(laneEnded("t1", runtime.TurnStatusCompleted)))
	if row := f.row(); row.Turn.Generation != 2 || row.Turn.LastID != "" || row.Turn.Seen != "t1" {
		t.Fatalf("cells = %+v, want generation 2 anchored at t1 with no end of its own", row.Turn)
	}
	f.gen = 3
	f.see(laneIdle(laneEnded("t1", runtime.TurnStatusCompleted)))
	f.wantFacts("turn.started:t1@g1", "turn.ended:t1@g1:completed")
}

// TestLaneTurnObserverUnsupportedKeepsTheHistory pins keep: an update that
// says the source cannot identify turns, failing while a history waits for
// its retry, does not displace that history, so the retry journals it.
func TestLaneTurnObserverUnsupportedKeepsTheHistory(t *testing.T) {
	f := newLaneObsFixture(t)
	f.start()
	f.genErr = errors.New("sessions unavailable")
	hist := laneRunning("t1")
	hist.ObservedAt = f.clk.Now()
	f.o.observe(context.Background(), worker.TurnUpdate{History: hist})
	f.o.observe(context.Background(), worker.TurnUpdate{Err: fmt.Errorf("reading turns: %w", runtime.ErrTurnIdentityUnsupported)})
	if f.o.retry == nil || f.o.retry.Err != nil {
		t.Fatalf("kept %+v, want the history", f.o.retry)
	}
	f.genErr = nil
	f.o.retryNow(context.Background())
	f.wantFacts("turn.started:t1")
}

// TestLaneTurnObserverStopsForAClosedConversation pins that a generation read
// that finds the session gone or closed ends the watch, so the observer holds
// its worker no longer.
func TestLaneTurnObserverStopsForAClosedConversation(t *testing.T) {
	f := newLaneObsFixture(t)
	f.start()
	f.see(laneRunning("t1"))
	f.live = false
	hist := laneRunning("t1")
	hist.ObservedAt = f.clk.Now()
	f.o.observe(context.Background(), worker.TurnUpdate{History: hist})
	if f.o.state != laneTurnDone {
		t.Fatalf("observer state = %d, want done", f.o.state)
	}
	if _, pending := f.o.pending(f.clk.Now()); pending {
		t.Fatal("an observer of a closed conversation is pending")
	}
	if f.stats.watching.Load() != 0 {
		t.Fatalf("watching = %d, want 0", f.stats.watching.Load())
	}
}

// TestLaneTurnsWatchAfterAFailedPass pins that no pass that failed or was
// skipped starts the watch, with a kind listed in [ops] authoritative too.
func TestLaneTurnsWatchAfterAFailedPass(t *testing.T) {
	for _, out := range []lanePassOutcome{{err: errors.New("list unavailable")}, {skipped: true}} {
		if laneTurnsWatchAfter(out, true) {
			t.Errorf("laneTurnsWatchAfter(%+v, true) = true, want false", out)
		}
	}
	if !laneTurnsWatchAfter(lanePassOutcome{}, true) {
		t.Error("a pass that succeeded with a kind listed starts no watch")
	}
}

// laneWorkerRig drives a conversationLaneWorker over scripted hooks.
type laneWorkerRig struct {
	clk     *laneTestClock
	timers  chan laneTimer
	turns   chan worker.TurnUpdate
	folded  []worker.TurnUpdate
	pending func() (time.Duration, bool)
	pass    func() lanePassOutcome
	release chan *lanePark
}

func newLaneWorkerRig() *laneWorkerRig {
	return &laneWorkerRig{
		clk: &laneTestClock{now: laneT0}, timers: make(chan laneTimer, 16),
		turns: make(chan worker.TurnUpdate, 4), release: make(chan *lanePark, 4),
		pending: func() (time.Duration, bool) { return 0, false },
		pass:    func() lanePassOutcome { return lanePassOutcome{} },
	}
}

// run runs the worker in the background until it releases its slot.
func (r *laneWorkerRig) run(ctx context.Context) {
	w := &conversationLaneWorker{
		ctx: ctx, clk: r.clk, poke: make(chan struct{}), queued: make(chan struct{}),
		newTimer: func(d time.Duration) (<-chan time.Time, func()) {
			tm := laneTimer{d: d, c: make(chan time.Time, 1)}
			r.timers <- tm
			return tm.c, func() {}
		},
		hooks: laneWorkerHooks{
			pass:         func() lanePassOutcome { return r.pass() },
			yield:        func(time.Duration) bool { return false },
			release:      func(_ bool, park *lanePark) bool { r.release <- park; return true },
			turns:        func() <-chan worker.TurnUpdate { return r.turns },
			onTurn:       func(u worker.TurnUpdate, _ bool) { r.folded = append(r.folded, u) },
			turnsPending: func() (time.Duration, bool) { return r.pending() },
		},
	}
	go w.run()
}

// TestLaneWorkerDrainsTurnsBeforeItReleases pins that a worker going idle
// hands every turn update already queued to its observer before it gives
// up its slot, so none is left unfolded.
func TestLaneWorkerDrainsTurnsBeforeItReleases(t *testing.T) {
	r := newLaneWorkerRig()
	r.turns <- worker.TurnUpdate{History: laneRunning("t1")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.run(ctx)
	select {
	case <-r.release:
	case <-time.After(laneWaitBudget):
		t.Fatal("the idle worker did not release its slot")
	}
	if len(r.folded) != 1 || r.folded[0].History.Current.TurnID != "t1" {
		t.Fatalf("folded %+v before the release, want the queued update", r.folded)
	}
}

// TestLaneWorkerWaitsForTheSoonerOfOpsAndObserver pins that a worker with op
// work waits for its observer's look when that comes before the ops' next
// change.
func TestLaneWorkerWaitsForTheSoonerOfOpsAndObserver(t *testing.T) {
	r := newLaneWorkerRig()
	r.pass = func() lanePassOutcome {
		return lanePassOutcome{res: opjournal.PassResult{Next: r.clk.Now().Add(30 * time.Second)}}
	}
	r.pending = func() (time.Duration, bool) { return time.Second, true }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.run(ctx)
	select {
	case tm := <-r.timers:
		if tm.d != time.Second {
			t.Fatalf("wait = %s, want the observer's sooner %s", tm.d, time.Second)
		}
	case <-time.After(laneWaitBudget):
		t.Fatal("the worker set no timer")
	}
}

// TestLaneHostKeepsTheDeliveredTurnPastAnOldEnd pins DES-O1 §7.7 for a
// message to a busy session whose first read after the take shows only the
// busy turn's end: the worker stays for the turn the delivery started.
func TestLaneHostKeepsTheDeliveredTurnPastAnOldEnd(t *testing.T) {
	l := newLaneHostTurns(t)
	st := l.f.laneStore(t)
	l.f.propose(t, st, l.f.msg("k1", opjournal.RolePrompt, "hello"))
	var ready atomic.Bool
	l.hh.h.newExec = func(laneExecDeps, session.Info) opjournal.Executor {
		return laneReadyExec{clk: l.f.clk, ready: &ready}
	}
	l.hh.start()
	if p := l.hh.nextPass(t); !p.out.res.Busy {
		t.Fatalf("first pass = %+v, want busy", p.out.res)
	}
	tm := l.hh.nextTimer(t)
	l.send(laneRunning("t0"))
	l.batch(t, "the busy turn")
	ready.Store(true)
	l.f.clk.Advance(tm.d)
	tm.c <- l.f.clk.Now()
	if p := l.hh.nextPass(t); p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass at the boundary = %+v, want the delivery accepted", p.out.res)
	}
	take := l.f.clk.Now()
	l.hh.nextPass(t)
	l.f.clk.Advance(500 * time.Millisecond)
	t0 := laneEnded("t0", runtime.TurnStatusCompleted)
	l.send(laneIdle(t0))
	l.batch(t, "t0's end, read after the take")
	l.noExit(t, "before the turn its delivery started was seen")
	l.send(laneRunningAt("t1", take.Add(time.Second), t0))
	l.batch(t, "the delivered turn's start")
	l.send(laneIdle(laneCompletedAt("t1", take.Add(time.Second)), t0))
	l.batch(t, "the delivered turn's end")
	if l.hh.nextExit(t) != l.f.id {
		t.Fatal("the worker did not exit once the delivered turn ended")
	}
}

// TestLaneHostKeepsTheDeliveredTurnPastAnEarlierTurn pins DES-O1 §7.7 for a
// message to an idle session whose source still shows an earlier turn the
// cells never journaled: journaling that turn does not end the worker's wait
// for the delivered one.
func TestLaneHostKeepsTheDeliveredTurnPastAnEarlierTurn(t *testing.T) {
	l := newLaneHostTurns(t)
	st := l.f.laneStore(t)
	l.f.propose(t, st, l.f.msg("k1", opjournal.RolePrompt, "hello"))
	l.hh.start()
	if p := l.hh.nextPass(t); p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass = %+v, want the delivery accepted", p.out.res)
	}
	take := l.f.clk.Now()
	l.hh.nextPass(t)
	l.f.clk.Advance(100 * time.Millisecond)
	t0 := laneCompletedAt("t0", take.Add(-10*time.Minute))
	l.send(laneIdle(t0))
	l.batch(t, "the earlier turn, read after the take")
	l.noExit(t, "before the turn its delivery started was seen")
	l.send(laneRunningAt("t1", take.Add(time.Second), t0))
	l.batch(t, "the delivered turn's start")
	l.send(laneIdle(laneCompletedAt("t1", take.Add(time.Second)), t0))
	l.batch(t, "the delivered turn's end")
	if l.hh.nextExit(t) != l.f.id {
		t.Fatal("the worker did not exit once the delivered turn ended")
	}
}

// TestLaneHostObservingWorkerYieldsToAQueuedConversation pins the host's slot
// policy for a worker that waits only for its observer: a conversation that
// queues for a slot wakes it, and it parks at once instead of at its hold's
// end.
func TestLaneHostObservingWorkerYieldsToAQueuedConversation(t *testing.T) {
	l := newLaneHostTurns(t)
	other := addSession(t, l.f, "b")
	st := l.f.laneStore(t)
	l.f.propose(t, st, l.f.msg("k1", opjournal.RolePrompt, "hello"))
	l.hh.h.maxWorkers = 1
	l.hh.start()
	if p := l.hh.nextPass(t); p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass = %+v, want the delivery accepted", p.out.res)
	}
	l.hh.nextPass(t)
	hold := l.hh.nextTimer(t)
	l.send(laneRunning("t1"))
	l.batch(t, "the delivered turn's start")
	l.f.propose(t, st, l.f.msg("k2", opjournal.RolePrompt, "hi", func(r *opjournal.Request) { r.Conversation = other }))
	if !l.hh.h.Poke(other) {
		t.Fatal("Poke refused")
	}
	if l.hh.nextExit(t) != l.f.id {
		t.Fatalf("the observing worker kept its slot until its %s hold ends", hold.d)
	}
	if p := l.hh.nextPass(t); p.conv != other {
		t.Fatalf("pass over %s, want the queued %s", p.conv, other)
	}
}

// TestLaneHostReleasesARunningTurnOfAStoppedRuntime pins the bound on the
// running-turn hold end to end: a session stopped mid-turn, whose source
// keeps the turn open, holds its worker until the hold's first timer.
func TestLaneHostReleasesARunningTurnOfAStoppedRuntime(t *testing.T) {
	l := newLaneHostTurns(t)
	st := l.f.laneStore(t)
	l.f.propose(t, st, l.f.msg("k1", opjournal.RolePrompt, "hello"))
	l.hh.start()
	if p := l.hh.nextPass(t); p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass = %+v, want the delivery accepted", p.out.res)
	}
	l.hh.nextPass(t)
	hold := l.hh.nextTimer(t)
	l.send(laneRunning("t1"))
	l.batch(t, "the delivered turn's start")
	l.f.clk.Advance(hold.d)
	hold.c <- l.f.clk.Now()
	next := l.hh.nextTimer(t)
	l.noExit(t, "while its runtime runs")
	if err := l.f.sp.Stop(l.f.name); err != nil {
		t.Fatal(err)
	}
	l.f.clk.Advance(next.d)
	next.c <- l.f.clk.Now()
	if l.hh.nextExit(t) != l.f.id {
		t.Fatal("the worker kept its slot for a stopped runtime's open turn")
	}
}

// laneSlowDeliverExec is a ready executor whose delivery takes took on the
// test clock, as a prompt written partway through its pass does.
type laneSlowDeliverExec struct {
	laneReadyExec
	took time.Duration
}

func (e laneSlowDeliverExec) Deliver(ctx context.Context, d opjournal.Delivery) opjournal.DeliveryResult {
	e.clk.Advance(e.took)
	return e.laneReadyExec.Deliver(ctx, d)
}

// TestLaneHostExpectsATurnStartedDuringItsPass pins where the expectation
// starts: at the pass's start, so a turn the source started while the pass
// delivered, before the pass ended, is the take's.
func TestLaneHostExpectsATurnStartedDuringItsPass(t *testing.T) {
	l := newLaneHostTurns(t)
	st := l.f.laneStore(t)
	l.f.propose(t, st, l.f.msg("k1", opjournal.RolePrompt, "hello"))
	ready := new(atomic.Bool)
	ready.Store(true)
	l.hh.h.newExec = func(laneExecDeps, session.Info) opjournal.Executor {
		return laneSlowDeliverExec{laneReadyExec: laneReadyExec{clk: l.f.clk, ready: ready}, took: 2 * time.Second}
	}
	began := l.f.clk.Now()
	l.hh.start()
	if p := l.hh.nextPass(t); p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass = %+v, want the delivery accepted", p.out.res)
	}
	l.hh.nextPass(t)
	l.send(laneIdle(laneCompletedAt("t1", began.Add(time.Second))))
	l.batch(t, "the delivered turn")
	if l.hh.nextExit(t) != l.f.id {
		t.Fatal("the worker did not exit once the turn its pass started ended")
	}
}

// TestLaneHostKeepsTheDeliveredTurnPastALongPass pins where the expectation's
// window starts: when the take's pass returns, so a delivery that held its
// pass for longer than laneTurnExpectFor still keeps the worker for the turn
// it started.
func TestLaneHostKeepsTheDeliveredTurnPastALongPass(t *testing.T) {
	l := newLaneHostTurns(t)
	st := l.f.laneStore(t)
	l.f.propose(t, st, l.f.msg("k1", opjournal.RolePrompt, "hello"))
	ready := new(atomic.Bool)
	ready.Store(true)
	took := laneTurnExpectFor + 10*time.Second
	l.hh.h.newExec = func(laneExecDeps, session.Info) opjournal.Executor {
		return laneSlowDeliverExec{laneReadyExec: laneReadyExec{clk: l.f.clk, ready: ready}, took: took}
	}
	began := l.f.clk.Now()
	l.hh.start()
	if p := l.hh.nextPass(t); p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass = %+v, want the delivery accepted", p.out.res)
	}
	l.hh.nextPass(t)
	l.send(laneIdle())
	l.batch(t, "the first read")
	l.noExit(t, "as soon as its long pass returned")
	l.send(laneRunningAt("t1", began.Add(took)))
	l.batch(t, "the delivered turn's start")
	l.send(laneIdle(laneCompletedAt("t1", began.Add(took))))
	l.batch(t, "the delivered turn's end")
	if l.hh.nextExit(t) != l.f.id {
		t.Fatal("the worker did not exit once the delivered turn ended")
	}
}

// TestLaneHostJournalsTheTurnItDeliversAfterAWake pins DES-O1 §7.7 for a
// message to a sleeping session whose turns a protocol runtime reports, which
// answers not found until it runs: that answer, read while the session is
// woken, leaves the watch running, so the turn the delivery starts is
// journaled.
func TestLaneHostJournalsTheTurnItDeliversAfterAWake(t *testing.T) {
	l := newLaneHostTurns(t)
	if err := l.f.store.SetMetadata(l.f.id, "state", string(session.StateAsleep)); err != nil {
		t.Fatal(err)
	}
	st := l.f.laneStore(t)
	l.f.propose(t, st, l.f.msg("k1", opjournal.RolePrompt, "wake up", func(r *opjournal.Request) {
		r.Policy.Message.Wake = opjournal.WakeIfNeeded
	}))
	l.hh.start()
	if p := l.hh.nextPass(t); !p.out.res.Wake || p.out.wake != session.WakeRecorded {
		t.Fatalf("pass = %+v, want a recorded wake", p.out)
	}
	tm := l.hh.nextTimer(t)
	l.turns.ch <- worker.TurnUpdate{Err: fmt.Errorf("no ACP control socket: %w", runtime.ErrSessionNotFound)}
	l.updates(t, 1)
	if err := l.f.store.SetMetadata(l.f.id, "state", string(session.StateAwake)); err != nil {
		t.Fatal(err)
	}
	tm.c <- l.f.clk.Now()
	if p := l.hh.nextPass(t); p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass after the wake = %+v, want the delivery accepted", p.out.res)
	}
	take := l.f.clk.Now()
	l.hh.nextPass(t)
	l.noExit(t, "after delivering to the woken session")
	l.f.clk.Advance(2 * time.Second)
	l.send(laneRunningAt("t1", take.Add(time.Second)))
	l.batch(t, "the delivered turn's start")
	l.send(laneIdle(laneCompletedAt("t1", take.Add(time.Second))))
	l.batch(t, "the delivered turn's end")
	if l.hh.nextExit(t) != l.f.id {
		t.Fatal("the worker did not exit once the delivered turn ended")
	}
	if got, want := laneFactList(t, l.store, l.f.id, 1), []string{"turn.started:t1", "turn.ended:t1:completed"}; !slices.Equal(got, want) {
		t.Fatalf("facts = %q, want %q", got, want)
	}
	if l.watches() != 1 {
		t.Fatalf("watches = %d, want one", l.watches())
	}
}

// updates waits until the observers took n turn updates.
func (l *laneHostTurns) updates(t *testing.T, n int64) {
	t.Helper()
	deadline := time.After(laneWaitBudget)
	for {
		if s := l.hh.h.status(l.f.clk.Now()).Turns; s != nil && s.Updates >= n {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("the observer took fewer than %d turn updates (turns status %+v)", n, l.hh.h.status(l.f.clk.Now()).Turns)
		case <-time.After(time.Millisecond):
		}
	}
}

// TestLaneHostReleasesAfterAQuiescentFailedFirstUpdate pins that a first
// turn update with nothing to keep, a failed read arriving while the city is
// quiescent, ends the hold for it: a worker with no op work exits rather than
// hold its slot until its source reads.
func TestLaneHostReleasesAfterAQuiescentFailedFirstUpdate(t *testing.T) {
	l := newLaneHostTurns(t, string(opjournal.KindMessage))
	l.hh.start()
	if !l.hh.h.Poke(l.f.id) {
		t.Fatal("Poke refused")
	}
	l.hh.nextPass(t)
	if tm := l.hh.nextTimer(t); tm.d != laneTurnHold {
		t.Fatalf("wait = %s, want the observer's hold %s", tm.d, laneTurnHold)
	}
	l.hh.env.quiescent.Store(true)
	l.turns.ch <- worker.TurnUpdate{Err: fmt.Errorf("transcript not written yet: %w", runtime.ErrRuntimeUnavailable)}
	if l.hh.nextExit(t) != l.f.id {
		t.Fatal("the worker kept its slot for a first update it could not fold")
	}
	if l.watches() != 1 {
		t.Fatalf("watches = %d, want one", l.watches())
	}
}

// laneObsV1Caps are a T3 V1 session's capabilities: a protocol source whose
// ends are inferred, since V1 clears latestTurn at most turn ends.
var laneObsV1Caps = worker.TurnCapabilities{
	TurnCapabilities: runtime.TurnCapabilities{
		Start: runtime.TurnStartRecorded, End: runtime.TurnEndInferred,
		Fence: runtime.TurnFenceChecked, Link: runtime.TurnLinkCorrelated,
	},
	Source:    worker.TurnSourceProtocol,
	Interrupt: runtime.TurnInterruptProtocol,
}

// newLaneObsV1Fixture is newLaneObsFixture over a T3 V1 session.
func newLaneObsV1Fixture(t *testing.T) *laneObsFixture {
	t.Helper()
	f := newLaneObsFixture(t)
	f.turns.caps = laneObsV1Caps
	return f
}

// wantTurnEndedBasis checks the basis of turn's turn.ended fact.
func (f *laneObsFixture) wantTurnEndedBasis(turn string, want opjournal.FactBasis) {
	f.t.Helper()
	rows, err := f.store.ListByLabel(opjournal.FactConversationLabel(laneObsConv), 0, beads.IncludeClosed)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, b := range rows {
		fact, err := opjournal.DecodeFact(b)
		if err != nil {
			f.t.Fatal(err)
		}
		if fact.Kind == opjournal.FactTurnEnded && fact.Turn == turn {
			if fact.Basis != want {
				f.t.Fatalf("turn.ended:%s basis = %q, want %q", turn, fact.Basis, want)
			}
			return
		}
	}
	f.t.Fatalf("no turn.ended fact for %s", turn)
}

// TestLaneTurnObserverInfersV1IdleEnds pins the maintainer ruling of
// 2026-10-20 (X11 §5.3): on T3 V1 a turn read running, then idle with the
// turn missing from Recent, ended normally. It is journaled as an inferred
// end, basis observed, and counted as no gap; the source restoring the turn
// later as completed writes no second end.
func TestLaneTurnObserverInfersV1IdleEnds(t *testing.T) {
	f := newLaneObsV1Fixture(t)
	f.start()
	f.see(laneRunning("t1"))
	f.see(laneIdle())
	f.wantFacts("turn.started:t1", "turn.ended:t1:ended")
	f.wantTurnEndedBasis("t1", opjournal.FactBasisObserved)
	row := f.row()
	if row.Turn.State != opjournal.TurnStateEnded || row.Turn.LastID != "t1" || row.Turn.LastEnd != opjournal.TurnEndEnded {
		t.Fatalf("cells = %+v, want ended after t1's inferred end", row.Turn)
	}
	if !strings.Contains(row.Turn.Caps.String(), "end=inferred") {
		t.Fatalf("caps = %q, want V1's inferred end", row.Turn.Caps)
	}
	f.see(laneIdle(laneEnded("t1", runtime.TurnStatusCompleted)))
	f.see(laneRunning("t2"))
	f.see(laneIdle())
	f.wantFacts("turn.started:t1", "turn.ended:t1:ended", "turn.started:t2", "turn.ended:t2:ended")
	if st := f.stats.status(); st.Gaps != 0 {
		t.Fatalf("gaps = %d, want 0 for V1's normal ends", st.Gaps)
	}
}

// TestLaneTurnObserverV1RealGapIsUnobserved pins that the ruling leaves a
// real gap one on T3 V1: a later turn the source shows, running or ended,
// while the running turn was never seen ending.
func TestLaneTurnObserverV1RealGapIsUnobserved(t *testing.T) {
	f := newLaneObsV1Fixture(t)
	f.start()
	f.see(laneRunning("t1"))
	f.see(laneRunning("t9"))
	f.see(laneIdle(laneEnded("t12", runtime.TurnStatusCompleted)))
	f.wantFacts("turn.started:t1", "turn.ended:t1:unobserved", "turn.started:t9", "turn.ended:t9:unobserved",
		"turn.started:t12", "turn.ended:t12:completed")
	if st := f.stats.status(); st.Gaps != 2 {
		t.Fatalf("gaps = %d, want 2", st.Gaps)
	}
}

// TestLaneTurnObserverV1InferredEndCreditsInterrupt pins that a gc interrupt
// of a V1 turn whose end is inferred ends it interrupted, naming the op,
// whether the observer was told of it or reads it from the op rows.
func TestLaneTurnObserverV1InferredEndCreditsInterrupt(t *testing.T) {
	f := newLaneObsV1Fixture(t)
	f.start()
	f.see(laneRunning("t1"))
	f.o.setInterrupting(opjournal.InterruptRef{Op: laneObsInterruptOp, Turn: "t1"})
	f.see(laneIdle())
	f.see(laneRunning("t2"))
	f.credits = []opjournal.InterruptRef{{Op: laneObsInterruptOp, Turn: "t2"}}
	f.see(laneIdle())
	if f.creditReads != 1 {
		t.Fatalf("credit reads = %d, want 1 for t2's inferred end", f.creditReads)
	}
	f.wantFacts("turn.started:t1", "turn.ended:t1:interrupted:"+laneObsInterruptOp,
		"turn.started:t2", "turn.ended:t2:interrupted:"+laneObsInterruptOp)
	f.wantTurnEndedBasis("t2", opjournal.FactBasisObserved)
}

// TestLaneTurnObserverV1RestoredTurnIsNoLaterTurn pins that V1 restoring a
// turn whose end was inferred only after the next turn ran (its diff
// completing late in a git workspace) shows no later turn: the next turn's
// idle end, with only the restored turn in Recent, is still inferred, and
// credited to a gc interrupt an op row names. No gap is counted, and the
// restore moves the cells back to no earlier turn.
func TestLaneTurnObserverV1RestoredTurnIsNoLaterTurn(t *testing.T) {
	f := newLaneObsV1Fixture(t)
	f.start()
	f.see(laneRunning("t1"))
	f.see(laneIdle())
	f.see(laneRunning("t2"))
	f.credits = []opjournal.InterruptRef{{Op: laneObsInterruptOp, Turn: "t2"}}
	f.see(laneIdle(laneEnded("t1", runtime.TurnStatusCompleted)))
	f.wantFacts("turn.started:t1", "turn.ended:t1:ended", "turn.started:t2", "turn.ended:t2:interrupted:"+laneObsInterruptOp)
	if row := f.row(); row.Turn.State != opjournal.TurnStateEnded || row.Turn.LastID != "t2" || row.Turn.LastEnd != opjournal.TurnEndInterrupted {
		t.Fatalf("cells = %+v, want ended on t2's credited end", row.Turn)
	}
	f.see(laneRunning("t3"))
	f.see(laneIdle(laneEnded("t2", runtime.TurnStatusCompleted)))
	f.wantFacts("turn.started:t1", "turn.ended:t1:ended", "turn.started:t2", "turn.ended:t2:interrupted:"+laneObsInterruptOp,
		"turn.started:t3", "turn.ended:t3:ended")
	if row := f.row(); row.Turn.State != opjournal.TurnStateEnded || row.Turn.LastID != "t3" || row.Turn.LastEnd != opjournal.TurnEndEnded {
		t.Fatalf("cells = %+v, want ended on t3's inferred end", row.Turn)
	}
	if st := f.stats.status(); st.Gaps != 0 {
		t.Fatalf("gaps = %d, want 0 with each restore landing after the next turn ran", st.Gaps)
	}
}

// TestLaneTurnObserverIdleWithTheTurnMissingIsAGapElsewhere pins that the
// ruling is V1's alone: a source that records every end (a protocol one
// such as ACP), a transcript whose ends are inferred by the worker's idle
// overlay (tmux Claude), and a V1 session downgraded to boundary, whether
// the same observer or a successor reads the idle snapshot, still end a
// running turn that idles out of Recent unobserved. The successor sees the
// turn run itself before the idle read, so only the row's end=boundary stamp
// keeps that end unobserved.
func TestLaneTurnObserverIdleWithTheTurnMissingIsAGapElsewhere(t *testing.T) {
	recordedProtocol := laneObsV1Caps
	recordedProtocol.End, recordedProtocol.Fence, recordedProtocol.Link, recordedProtocol.Lossless = runtime.TurnEndRecorded, runtime.TurnFenceAtomic, runtime.TurnLinkExact, true
	inferredTranscript := laneObsCaps
	inferredTranscript.End = runtime.TurnEndInferred
	for _, c := range []struct {
		name      string
		caps      worker.TurnCapabilities
		downgrade bool
		restart   bool
	}{
		{"recorded protocol", recordedProtocol, false, false},
		{"recorded transcript", laneObsCaps, false, false},
		{"inferred transcript", inferredTranscript, false, false},
		{"V1 downgraded", laneObsV1Caps, true, false},
		{"V1 downgraded, then restart", laneObsV1Caps, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newLaneObsFixture(t)
			f.turns.caps = c.caps
			f.start()
			f.see(laneRunning("t1"))
			if c.downgrade {
				f.o.observe(context.Background(), worker.TurnUpdate{Err: fmt.Errorf("scan: %w", runtime.ErrTurnIdentityUnsupported)})
			}
			if c.restart {
				if !strings.Contains(f.row().Turn.Caps.String(), "end=boundary") {
					t.Fatalf("caps = %q, want the downgrade stamped before the restart", f.row().Turn.Caps)
				}
				f.restart()
				f.see(laneRunning("t1"))
			}
			f.see(laneIdle())
			f.wantFacts("turn.started:t1", "turn.ended:t1:unobserved")
			if st := f.stats.status(); st.Gaps != 1 {
				t.Fatalf("gaps = %d, want 1", st.Gaps)
			}
		})
	}
}

// TestLaneTurnObserverV1InfersOnlyWhatItsWatchSawRun pins the maintainer
// ruling of 2026-10-20 (review finding 3): V1's normal end is inferred only
// when the watch reading the idle snapshot saw the turn run itself. A
// successor worker's observer, or the same observer's next watch, that
// first reads idle with the running turn missing from Recent ends it
// unobserved and counts a gap (X11 §8), crediting no interrupt, whether the
// observer was told of one or an op row names it; once that watch sees a
// turn run, its idle end is inferred again. (An update the previous watch
// kept for retry is that watch's read: a next watch that folds it before
// the idle read infers, as the same watch would have.)
func TestLaneTurnObserverV1InfersOnlyWhatItsWatchSawRun(t *testing.T) {
	for _, c := range []struct {
		name  string
		renew func(f *laneObsFixture)
	}{
		{"successor observer", (*laneObsFixture).restart},
		{"next watch", func(f *laneObsFixture) { f.o.closed(); f.start() }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newLaneObsV1Fixture(t)
			f.start()
			f.see(laneRunning("t1"))
			c.renew(f)
			f.o.setInterrupting(opjournal.InterruptRef{Op: laneObsInterruptOp, Turn: "t1"})
			f.credits = []opjournal.InterruptRef{{Op: laneObsInterruptOp, Turn: "t1"}}
			f.see(laneIdle())
			f.wantFacts("turn.started:t1", "turn.ended:t1:unobserved")
			if st := f.stats.status(); st.Gaps != 1 {
				t.Fatalf("gaps = %d, want 1 for the end no watch saw", st.Gaps)
			}
			if f.creditReads != 0 {
				t.Fatalf("credit reads = %d, want none for an unobserved end", f.creditReads)
			}
			f.see(laneRunning("t2"))
			f.see(laneIdle())
			f.wantFacts("turn.started:t1", "turn.ended:t1:unobserved", "turn.started:t2", "turn.ended:t2:ended")
			f.wantTurnEndedBasis("t2", opjournal.FactBasisObserved)
			if st := f.stats.status(); st.Gaps != 1 {
				t.Fatalf("gaps = %d, want t2's inferred end to count none", st.Gaps)
			}
		})
	}
}

// TestLaneTurnObserverV1OpRowCreditUnreadForAnUnseenEnd pins that a
// successor told of no interrupt, with only an op row naming the turn it
// never saw run, ends that turn unobserved without reading op rows: the
// credit candidates are gated the same way the inference is.
func TestLaneTurnObserverV1OpRowCreditUnreadForAnUnseenEnd(t *testing.T) {
	f := newLaneObsV1Fixture(t)
	f.start()
	f.see(laneRunning("t1"))
	f.restart()
	f.credits = []opjournal.InterruptRef{{Op: laneObsInterruptOp, Turn: "t1"}}
	f.see(laneIdle())
	f.wantFacts("turn.started:t1", "turn.ended:t1:unobserved")
	if f.creditReads != 0 {
		t.Fatalf("credit reads = %d, want none for an unobserved end", f.creditReads)
	}
	if st := f.stats.status(); st.Gaps != 1 {
		t.Fatalf("gaps = %d, want 1", st.Gaps)
	}
}

// TestLaneTurnObserverV1AnotherWritersRunningTurnIsAGap pins that a watch
// infers only the running turn it last left running: when another observer
// over the same row moves it to t2 behind this watch, this watch never saw
// t2 run, so t2's idle end is unobserved, a gap.
func TestLaneTurnObserverV1AnotherWritersRunningTurnIsAGap(t *testing.T) {
	f := newLaneObsV1Fixture(t)
	f.start()
	f.see(laneRunning("t1"))
	other := f.observer()
	other.start(context.Background())
	hist := laneRunning("t2")
	hist.ObservedAt = f.clk.Now()
	other.observe(context.Background(), worker.TurnUpdate{History: hist})
	f.clk.Advance(time.Second)
	f.see(laneIdle())
	f.wantFacts("turn.started:t1", "turn.ended:t1:unobserved", "turn.started:t2", "turn.ended:t2:unobserved")
}

// TestLaneTurnObserverV1InfersWhatItsWatchSawRunAfterARestart pins the other
// side of the ruling: a successor whose watch reads the turn the row shows
// running as current, then idle with it missing, saw it run and infers its
// end.
func TestLaneTurnObserverV1InfersWhatItsWatchSawRunAfterARestart(t *testing.T) {
	f := newLaneObsV1Fixture(t)
	f.start()
	f.see(laneRunning("t1"))
	f.restart()
	f.see(laneRunning("t1"))
	f.see(laneIdle())
	f.wantFacts("turn.started:t1", "turn.ended:t1:ended")
	if st := f.stats.status(); st.Gaps != 0 {
		t.Fatalf("gaps = %d, want 0", st.Gaps)
	}
}

// TestLaneHostV1ParkedTurnMissingAtIdleIsAGap pins the ruling under slot
// contention: the worker a park starts watches anew, so on T3 V1 its first
// read of idle with the parked worker's running turn missing from Recent
// ends that turn unobserved, one gap.
func TestLaneHostV1ParkedTurnMissingAtIdleIsAGap(t *testing.T) {
	l := newLaneHostTurns(t)
	l.turns.caps = laneObsV1Caps
	laneHostParkWatchedTurn(t, l)
	l.send(laneIdle())
	l.batch(t, "the parked turn's end")
	if l.hh.nextExit(t) != l.f.id {
		t.Fatal("the resumed worker did not exit once the turn ended")
	}
	if got, want := laneFactList(t, l.store, l.f.id, 1), []string{"turn.started:t1", "turn.ended:t1:unobserved"}; !slices.Equal(got, want) {
		t.Fatalf("facts = %q, want %q", got, want)
	}
	if st := l.hh.h.status(l.f.clk.Now()).Turns; st == nil || st.Gaps != 1 {
		t.Fatalf("turns status = %+v, want one gap", st)
	}
	if l.watches() != 2 {
		t.Fatalf("watches = %d, want one per worker of %s", l.watches(), l.f.id)
	}
}
