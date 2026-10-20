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
	turns.ch <- worker.TurnUpdate{History: runtime.TurnHistory{
		Recent: []runtime.EndedTurn{{TurnID: "t1", EndedAt: f.clk.Now(), Status: runtime.TurnStatusCompleted}}, ObservedAt: f.clk.Now(),
	}}
	if hh.nextExit(t) != f.id {
		t.Fatal("the worker did not exit once t1 ended")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("the watch outlived its worker")
	}
	if turns.watches != 1 {
		t.Fatalf("watches = %d, want one per worker", turns.watches)
	}
	if s := hh.h.status(f.clk.Now()); s.Turns == nil || s.Turns.Writes != 2 || s.Turns.Watching != 0 {
		t.Fatalf("status turns = %+v, want two writes and no watch", s.Turns)
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
	f.see(laneRunning("t1"))
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
	l.hh.nextPass(t)
	l.noExit(t, "with t0 running in its cells")
	l.send(laneRunning("t1", laneEnded("t0", runtime.TurnStatusCompleted)))
	l.batch(t, "the delivered turn's start")
	l.send(laneIdle(laneEnded("t1", runtime.TurnStatusCompleted), laneEnded("t0", runtime.TurnStatusCompleted)))
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
// reports.
func TestLaneHostTurnFailuresAreNotLaneFailures(t *testing.T) {
	l := newLaneHostTurns(t, string(opjournal.KindMessage))
	l.hh.env.setStore(laneTurnFailStore{l.store})
	l.hh.start()
	if !l.hh.h.Poke(l.f.id) {
		t.Fatal("Poke refused")
	}
	l.hh.nextPass(t)
	l.send(laneRunning("t1"))
	deadline := time.After(laneWaitBudget)
	for {
		s := l.hh.h.status(l.f.clk.Now())
		if s.Turns != nil && s.Turns.Errors > 0 {
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
			t.Fatalf("no observer failure counted (status %+v)", s)
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

// TestLaneHostParkedObservationsResume pins the observer under slot
// contention: a worker holding its slot only for a running turn parks once
// another conversation queues, and the worker its park starts watches again
// even though its pass finds no lane work, so the turn's end is journaled.
func TestLaneHostParkedObservationsResume(t *testing.T) {
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
