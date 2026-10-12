package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The stop verb's tests (CONTRACT v5 D3, D4, F2, F3; C6b2).

// signaled is a stop-pending row with the orphaned drain's request at
// generation 3.
func signaled(reason string, extra ...string) []string {
	return append(append(intentBegunAt(reason, time.Minute), stopPendingMeta(rowAt(-time.Minute))...), extra...)
}

// stopRun decides gc-1 with meta and entry (a live own runtime on a fence
// leaf, admittedBeginOn), checks the stop verb is proposed, applies setup, and
// runs it.
func stopRun(t *testing.T, meta []string, entry func(*selectionEntry), setup func(*effectPass, *fenceLeaf, beads.Store, string)) (settlement, beads.Bead, *fenceLeaf) {
	t.Helper()
	p, it, store, leaf := admittedBeginOn(t, meta, entry)
	if it.Kind != intentStop {
		t.Fatalf("decided %+v, want the stop verb", it)
	}
	p.Writers[rowLeg] = fencedWriter{store: atomicClosing{store.(*beads.MemStore)}} // the premise close's store
	if setup != nil {
		setup(p, leaf, store, it.Key.ID)
	}
	s := runTx(context.Background(), p, it, effectSpecs[intentStop], nil)
	got, err := store.Get(it.Key.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, got, leaf
}

func stops(l *fenceLeaf) int { return l.CountCalls("Stop", "s-gc-1") }

// assignWork gives gc-1 an in-progress bead.
func assignWork(t *testing.T, store beads.Store, id string) {
	t.Helper()
	if _, err := store.Create(beads.Bead{Title: "w", Type: "task", Status: "in_progress", Assignee: id}); err != nil {
		t.Fatal(err)
	}
}

// eventTypes are s's events' types.
func eventTypes(s settlement) []string {
	var out []string
	for _, ev := range s.Facts.Events {
		out = append(out, ev.Type)
	}
	return out
}

// TestStopVerbStopsConfirmsAndFinalizes (D3, D4; SESS-035, DRAIN-054).
// Kills a stop that never finalizes, a finalize that keeps the request or
// writes asleep where legacy writes drained, and a silent finalize: a
// signaled named row's live own runtime is stopped, confirmed gone, and the
// row ends drained in one CAS that clears the request, with session.stopped
// and the stop metric.
func TestStopVerbStopsConfirmsAndFinalizes(t *testing.T) {
	s, got, leaf := stopRun(t, signaled(drainOrphaned), wanted, nil)
	m := got.Metadata
	switch {
	case s.Outcome != settledLanded || stops(leaf) != 1:
		t.Fatalf("settlement %+v with %d stops, want landed after one", s, stops(leaf))
	case m["state"] != string(session.StateDrained) || m[session.DrainIntentReasonKey] != "" || m["state_reason"] != "":
		t.Fatalf("row %v, want drained with the request cleared", m)
	case s.Facts.Stopped == nil || len(s.Facts.Events) != 1 || s.Facts.Events[0].Type != events.SessionStopped:
		t.Fatalf("facts %+v, want session.stopped and the stop metric", s.Facts)
	}
}

// TestStopPendingKillHasNoL5 (DRAIN-048/011, DRAIN-055, SESS-036). Kills a
// stop-pending kill that waits on the seat's work: the runtime is stopped
// all the same, and the drain completes as an idle sleep that keeps the
// work, with legacy's drain_acked_with_assigned_work.
func TestStopPendingKillHasNoL5(t *testing.T) {
	s, got, leaf := stopRun(t, signaled(drainOrphaned), undesired(drainOrphaned), func(_ *effectPass, _ *fenceLeaf, store beads.Store, id string) {
		assignWork(t, store, id)
	})
	if s.Outcome != settledLanded || stops(leaf) != 1 || got.Metadata["state"] != "asleep" || got.Metadata["sleep_reason"] != "idle" {
		t.Fatalf("settlement %+v, %d stops, row %v; want stopped and asleep idle", s, stops(leaf), got.Metadata)
	}
	if types := eventTypes(s); len(types) != 2 || types[1] != events.SessionDrainAckedWithAssignedWork {
		t.Fatalf("events %v, want session.stopped and drain_acked_with_assigned_work", types)
	}
}

// TestFencedStopNeverKillsAttached (GUAR-006, C8.1),
// TestFencedStopTokenMismatchNeverStops (INC-025) and
// TestFencedStopUnknownLivenessDefers (GUAR-053). Kills a stop through any
// leg the stop-pending kill keeps: attached, another token of the row, or
// an unreadable runtime refuses with its cause, stops nothing and leaves
// the row signaled.
func TestFencedStopNeverKillsAttached(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(*fenceLeaf)
		cause string
	}{
		{"attached", func(l *fenceLeaf) { l.SetAttached("s-gc-1", true) }, fenceAttached},
		{"another token", func(l *fenceLeaf) {
			if err := l.SetMeta("s-gc-1", "GC_INSTANCE_TOKEN", "tok-9"); err != nil {
				t.Fatal(err)
			}
		}, fenceTokenMismatch},
		{"liveness unreadable", func(l *fenceLeaf) { l.LivenessErrors["s-gc-1"] = runtime.ErrRuntimeUnavailable }, ""},
	} {
		s, got, leaf := stopRun(t, signaled(drainOrphaned), undesired(drainOrphaned), func(_ *effectPass, l *fenceLeaf, _ beads.Store, _ string) { c.setup(l) })
		if s.Outcome != settledRefused || (c.cause != "" && s.Cause != c.cause) || stops(leaf) != 0 || !isDrainAckStopPendingInfo(censusRowOf(t, got).Info) {
			t.Errorf("%s: settlement %+v, %d stops, row %v; want refused %q, no stop, still signaled", c.name, s, stops(leaf), got.Metadata, c.cause)
		}
	}
}

// TestFinalizeRequiresConfirmedAbsence (GUAR-005/022, F3). Kills a finalize
// on a stop the fresh read does not confirm: a runtime still running after
// its Stop leaves the row signaled.
func TestFinalizeRequiresConfirmedAbsence(t *testing.T) {
	s, got, _ := stopRun(t, signaled(drainOrphaned), undesired(drainOrphaned), func(_ *effectPass, l *fenceLeaf, _ beads.Store, _ string) {
		l.StopLeavesRunning = map[string]bool{"s-gc-1": true}
	})
	if s.Outcome != settledRefused || s.Cause != fenceStopUnconfirmed || !isDrainAckStopPendingInfo(censusRowOf(t, got).Info) {
		t.Fatalf("settlement %+v, row %v; want refused stop_unconfirmed and still signaled", s, got.Metadata)
	}
}

// TestRestartStopPendingDeadRuntimeFinalizes (I13, R45). Kills a finalize
// that waits for a stop: a signaled row whose runtime is already gone (it
// died during downtime) is finalized by the verb's own confirmation, with
// no Stop.
func TestRestartStopPendingDeadRuntimeFinalizes(t *testing.T) {
	s, got, leaf := stopRun(t, signaled(drainOrphaned), func(e *selectionEntry) { undesired(drainOrphaned)(e); e.Liveness = livenessGone }, func(_ *effectPass, l *fenceLeaf, _ beads.Store, _ string) {
		if err := l.Stop("s-gc-1"); err != nil {
			t.Fatal(err)
		}
	})
	if s.Outcome != settledLanded || stops(leaf) != 1 || got.Metadata["state"] != string(session.StateDrained) || s.Facts.Stopped != nil {
		t.Fatalf("settlement %+v, %d stops (the setup's one), row %v; want finalized without a stop", s, stops(leaf), got.Metadata)
	}
}

// TestForeignVerdictFinalizesWithoutStop (R52, DRAIN-036/038). Kills a
// stop of another row's runtime, and a request that never ends: the name
// holds another row's runtime, so the row finalizes and nothing is stopped.
func TestForeignVerdictFinalizesWithoutStop(t *testing.T) {
	s, got, leaf := stopRun(t, signaled(drainOrphaned), undesired(drainOrphaned), func(_ *effectPass, l *fenceLeaf, _ beads.Store, _ string) {
		for k, v := range map[string]string{"GC_SESSION_ID": "gc-9", "GC_INSTANCE_TOKEN": "tok-9"} {
			if err := l.SetMeta("s-gc-1", k, v); err != nil {
				t.Fatal(err)
			}
		}
	})
	if s.Outcome != settledLanded || stops(leaf) != 0 || got.Metadata["state"] != string(session.StateDrained) {
		t.Fatalf("settlement %+v, %d stops, row %v; want finalized without a stop", s, stops(leaf), got.Metadata)
	}
}

// TestCorpseFinalizedWhateverItsToken (I16, I23, R46) and
// TestAttachedCorpseFinalizedAndLeftInPlace. Kills a finalize that waits
// for a corpse's token to match, and a corpse removed while a human is
// attached: a corpse counts as stopped, so the row finalizes; the corpse is
// removed by exact object only when L3 passes.
func TestCorpseFinalizedWhateverItsToken(t *testing.T) {
	for _, attached := range []bool{false, true} {
		obs := &freshObserver{Fake: runtime.NewFake(), l: runtime.Liveness{Corpse: true, ObjectID: "$1", ObjectCreated: "1"}, env: map[string]string{"GC_SESSION_ID": "gc-1", "GC_INSTANCE_TOKEN": "tok-9"}}
		if err := obs.Fake.Start(context.Background(), "s-gc-1", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		obs.Fake.SetAttached("s-gc-1", attached)
		s, got, _ := stopRun(t, signaled(drainOrphaned), undesired(drainOrphaned), func(p *effectPass, _ *fenceLeaf, _ beads.Store, _ string) { p.Runtime = obs.tmux() })
		killed := obs.Fake.CountCalls("KillCorpseObject", "s-gc-1")
		if s.Outcome != settledLanded || got.Metadata["state"] != string(session.StateDrained) || (killed == 1) == attached {
			t.Errorf("attached=%v: settlement %+v, row %v, %d corpse kills; want finalized, removed only when detached", attached, s, got.Metadata, killed)
		}
	}
}

// TestBeadScopedNameNeverForeign (v5.2 D3, C8.2(a)). Kills a pool seat's
// own name read as Foreign, which would finalize it without stopping the
// runtime its bead-scoped name holds: L2 still decides, and refuses.
func TestBeadScopedNameNeverForeign(t *testing.T) {
	p, it, _, leaf := admittedBeginOn(t, append(signaled(drainOrphaned), "pool_managed", "true", "session_name", "s-gc-2"), undesired(drainOrphaned))
	if name := "s-" + it.Key.ID; !ownsName(p.World.Census.Rows[it.Key].Info, name) {
		t.Fatalf("fixture: the row's session name %q does not embed its bead ID %s", name, it.Key.ID)
	}
	startRuntime(t, leaf.Fake, "s-"+it.Key.ID, map[string]string{"GC_SESSION_ID": "gc-9", "GC_INSTANCE_TOKEN": "tok-9"})
	s := runTx(context.Background(), p, it, effectSpecs[intentStop], nil)
	if s.Outcome == settledLanded || s.Cause != fenceTokenMismatch || leaf.CountCalls("Stop", "s-"+it.Key.ID) != 0 {
		t.Fatalf("settlement %+v; want the identity leg to refuse, and no stop", s)
	}
}

// TestOperatorDormantStopIgnoresWorkAndKeepsTheHold (C6a ruling 2; owner
// ruling at C6b2: L5 skipped; J42 B/C; #7612 review). Kills a managed
// suspend whose runtime the controller never stops, a stop that waits on the
// seat's work, a finalize that undoes the hold, and one that is not
// legacy's: the runtime is stopped, the row completes the drain as legacy
// does (asleep, idle), and keeps its hold.
func TestOperatorDormantStopIgnoresWorkAndKeepsTheHold(t *testing.T) {
	held := []string{"state", "suspended", "sleep_intent", "user-hold", "held_until", rowAt(100 * 365 * 24 * time.Hour)}
	s, got, leaf := stopRun(t, held, undesired(drainSuspended), func(_ *effectPass, _ *fenceLeaf, store beads.Store, id string) {
		assignWork(t, store, id)
	})
	m := got.Metadata
	if s.Outcome != settledLanded || stops(leaf) != 1 || got.Status == "closed" || m["sleep_intent"] != "user-hold" || m["held_until"] == "" ||
		m["state"] != "asleep" || m["sleep_reason"] != "idle" || m["slept_at"] == "" {
		t.Fatalf("settlement %+v, %d stops, row %v; want stopped, legacy's drain completion (asleep, idle) and the hold kept", s, stops(leaf), m)
	}
}

// TestIdleDrainFinalizesAsAnIdleSleep (D4's idle builder, SESS-601). Kills
// an idle drain that ends drained where legacy idle-sleeps it: the row ends
// asleep with sleep_reason=idle, its latch kept.
func TestIdleDrainFinalizesAsAnIdleSleep(t *testing.T) {
	sleep := func(e *selectionEntry) { e.Desired, e.Reason = desireSleep, reasonIdleSleep }
	s, got, _ := stopRun(t, signaled(drainIdle, "sleep_policy_fingerprint", "latch", "sleep_intent", "idle-stop-pending"), sleep, nil)
	m := got.Metadata
	if s.Outcome != settledLanded || m["state"] != "asleep" || m["sleep_reason"] != "idle" || m["sleep_policy_fingerprint"] != "latch" || m["sleep_intent"] != "" {
		t.Fatalf("settlement %+v, row %v; want asleep idle with the latch, the idle stop's transient cleared (the Sleep cell)", s, m)
	}
}

// TestStopVerbReissuedOnlyAtBackoffExpiry (I17). Kills a stop on every
// pass: a refused stop's backoff defers the next while it is live, but
// never a finalize-only stop (admission's finalizesOnly).
func TestStopVerbReissuedOnlyAtBackoffExpiry(t *testing.T) {
	k := rowKeyOf("gc-1")
	backoff := map[string]backoffRecord{rowBackoffKey(k): {Consecutive: 1, Until: gatherNow.Add(time.Minute), Cause: fenceAttached}}
	for _, finalize := range []bool{false, true} {
		res := admit(admitInput{Now: gatherNow, Backoff: backoff, BootOpen: true}, []intent{{Kind: intentStop, Key: k, Finalize: finalize}})
		if admitted := len(res.Admitted) == 1; admitted != finalize {
			t.Errorf("finalize=%v: admitted %v deferred %+v; want only the finalize through a live backoff", finalize, res.Admitted, res.Deferred)
		}
	}
}

// TestFinalizeWorkReadErrorKeepsOpen (DRAIN-050, D4). Kills a close on an
// unread work leg: a pool seat whose close-gate work read fails is not
// closed; it sleeps idle as one that holds work, while one whose read finds
// none closes.
func TestFinalizeWorkReadErrorKeepsOpen(t *testing.T) {
	for _, failed := range []bool{false, true} {
		s, got, _ := stopRun(t, signaled(drainOrphaned, "pool_managed", "true"), undesired(drainOrphaned), func(p *effectPass, _ *fenceLeaf, store beads.Store, _ string) {
			p.Writers[rowLeg] = fencedWriter{store: atomicClosing{store.(*beads.MemStore)}}
			if failed {
				p.reads.legs = WorkLegs{} // the read fails: errNoReadStore
			}
		})
		closed := got.Status == "closed"
		if s.Outcome != settledLanded || closed == failed || (failed && got.Metadata["sleep_reason"] != "idle") {
			t.Errorf("read failed=%v: settlement %+v, closed %v, row %v; want closed only on a clean read", failed, s, closed, got.Metadata)
		}
	}
}

// atomicClosing is a MemStore with the atomic terminal close the premise
// close needs, as the city's SQLite sessions store has.
type atomicClosing struct{ *beads.MemStore }

func (s atomicClosing) CloseWithMetadataIfMatch(id string, expectedRevision int64, metadata map[string]string) (beads.Bead, error) {
	return beads.AtomicCloserOver(s.MemStore).CloseWithMetadataIfMatch(id, expectedRevision, metadata)
}

// TestNothingLiveStillConfirms (F3, C8.8). Kills a finalize that trusts the
// fence's read of nothing live: a runtime back under the name by the
// confirmation's fresh read refuses the finalize.
func TestNothingLiveStillConfirms(t *testing.T) {
	obs := &freshObserver{Fake: runtime.NewFake(), env: map[string]string{"GC_SESSION_ID": "gc-1", "GC_INSTANCE_TOKEN": "tok-3"}}
	s, got, _ := stopRun(t, signaled(drainOrphaned), undesired(drainOrphaned), func(p *effectPass, _ *fenceLeaf, _ beads.Store, _ string) {
		p.Runtime = obs.tmux()
		p.seam = func(_ context.Context, at txSeam, _ intent, _, _ int) error {
			if at == seamBeforeCall { // the fence read nothing live; C8.8 finds it back
				obs.l = runtime.Liveness{Running: true, Alive: true, ObjectID: "$1", ObjectCreated: "1"}
			}
			return nil
		}
	})
	if s.Outcome != settledRefused || s.Cause != fenceStopUnconfirmed || !isDrainAckStopPendingInfo(censusRowOf(t, got).Info) {
		t.Fatalf("settlement %+v, row %v; want refused stop_unconfirmed, still signaled", s, got.Metadata)
	}
}

// TestFinalizeNeverClosesOverAnHonoredWake (D7 rule 4). Kills a premise
// close over a pending explicit wake AL1 honors: the stopped pool seat is
// drained, not closed, and the wake is left to the next pass's start.
func TestFinalizeNeverClosesOverAnHonoredWake(t *testing.T) {
	wake := []string{"pool_managed", "true", "wake_request", "explicit", "wake_requested_at", rowAt(-time.Second)}
	s, got, _ := stopRun(t, signaled(drainOrphaned, wake...), func(e *selectionEntry) { e.Desired = desireWake }, func(p *effectPass, _ *fenceLeaf, store beads.Store, _ string) {
		p.Writers[rowLeg] = fencedWriter{store: atomicClosing{store.(*beads.MemStore)}}
	})
	if s.Outcome != settledLanded || got.Status == "closed" || got.Metadata["state"] != string(session.StateDrained) || got.Metadata["wake_request"] != "explicit" {
		t.Fatalf("settlement %+v, status %s, row %v; want drained, open, the wake kept", s, got.Status, got.Metadata)
	}
}

// TestStopOnAGoneRuntimeIsFinalizeOnly (D3; P4's finalize exemption). Kills
// a gone row's stop that waits out the row backoff: A4 proposes it as the
// finalize-only verb, which admission lets through a live backoff.
func TestStopOnAGoneRuntimeIsFinalizeOnly(t *testing.T) {
	gone := func(e *selectionEntry) { undesired(drainOrphaned)(e); e.Liveness = livenessGone }
	if it, _ := decideDrain(t, signaled(drainOrphaned), gone); it.Kind != intentStop || !it.Finalize {
		t.Fatalf("%+v, want the finalize-only stop", it)
	}
	if it, _ := decideDrain(t, signaled(drainOrphaned), undesired(drainOrphaned)); it.Kind != intentStop || it.Finalize {
		t.Fatalf("%+v, want the stop with its stop", it)
	}
}

// TestStopVerbDecidesAgainOnTheFreshRow (R2). Kills a stop issued for a row
// that left its stop since the pass (legacy finalized it, or a PreWake): the
// effect decides A4 again on the fresh row, refuses, and stops nothing.
func TestStopVerbDecidesAgainOnTheFreshRow(t *testing.T) {
	s, _, leaf := stopRun(t, signaled(drainOrphaned), undesired(drainOrphaned), func(_ *effectPass, _ *fenceLeaf, store beads.Store, id string) {
		if err := store.SetMetadataBatch(id, map[string]string{"state": "active", "state_reason": ""}); err != nil {
			t.Fatal(err)
		}
	})
	if s.Outcome != settledRefused || s.Cause != causeRedecided || stops(leaf) != 0 {
		t.Fatalf("settlement %+v, %d stops; want refused redecided and no stop", s, stops(leaf))
	}
}

// wanted is an entry the allocation still wants (asleep), so its finalize
// never closes a non-pool row.
func wanted(e *selectionEntry) { e.Desired, e.InDesired = desireSleep, true }

// TestUndesiredNonPoolRowClosesAsLegacy (#7612 review, item 3; legacy's
// closeIfUnassigned = !desired || pool). Kills a finalize that keeps an
// orphaned named or manual session open as drained: an undesired row with no
// work closes as drained, a wanted one ends drained.
func TestUndesiredNonPoolRowClosesAsLegacy(t *testing.T) {
	for _, c := range []struct {
		name   string
		entry  func(*selectionEntry)
		closed bool
	}{
		{"undesired", undesired(drainOrphaned), true},
		{"wanted", wanted, false},
	} {
		s, got, _ := stopRun(t, signaled(drainOrphaned), c.entry, nil)
		if closed := got.Status == "closed"; s.Outcome != settledLanded || closed != c.closed || (!closed && got.Metadata["state"] != string(session.StateDrained)) {
			t.Errorf("%s: settlement %+v, status %s, row %v; want closed=%v", c.name, s, got.Status, got.Metadata, c.closed)
		}
		if c.closed && (got.Metadata["state"] != "drained" || got.Metadata["close_reason"] != session.CanonicalCloseReason("drained")) {
			t.Errorf("%s: row %v, want legacy's drained close", c.name, got.Metadata)
		}
	}
}

// TestWantedPoolSeatWithoutWorkClosesAsLegacy (N04; legacy's
// closeIfUnassigned = !desired || pool). Kills a finalize that keeps a pool
// seat open because the allocation still wants it: a config-drift or
// execution-stalled drain of a wanted pool seat with no work closes as
// drained, as legacy's does.
func TestWantedPoolSeatWithoutWorkClosesAsLegacy(t *testing.T) {
	for _, reason := range []string{drainConfigDrift, executionStalledDrainReason} {
		s, got, _ := stopRun(t, signaled(reason, "pool_managed", "true"), wanted, nil)
		if s.Outcome != settledLanded || got.Status != "closed" || got.Metadata["state"] != "drained" || got.Metadata["close_reason"] != session.CanonicalCloseReason("drained") {
			t.Errorf("%s: settlement %+v, status %s, row %v; want legacy's drained close", reason, s, got.Status, got.Metadata)
		}
	}
}

// TestFinalizeKeepsTheSleepCellsIntents (#7612 review, item 1; owner ruling
// D-J 3, R7c1). Kills a finalize that writes the lifecycle builder raw,
// blanking what the disposition's Sleep cell keeps: a drain that keeps work
// completes as an idle sleep and keeps a wait's intent, and an intent the
// cell cannot read.
func TestFinalizeKeepsTheSleepCellsIntents(t *testing.T) {
	for _, intent := range [][]string{{"sleep_intent", "wait-hold", "wait_hold", "true"}, {"sleep_intent", "something-new"}} {
		s, got, _ := stopRun(t, signaled(drainOrphaned, intent...), wanted, func(_ *effectPass, _ *fenceLeaf, store beads.Store, id string) {
			assignWork(t, store, id)
		})
		m := got.Metadata
		if s.Outcome != settledLanded || m["state"] != "asleep" || m["sleep_reason"] != "idle" || m["sleep_intent"] != intent[1] {
			t.Errorf("intent %v: settlement %+v, row %v; want asleep idle with sleep_intent kept", intent, s, m)
		}
	}
}

// TestDormantStopKeepsL4 (#7612 review, S07). Kills a dormant stop that
// skips the pending leg: a suspended session at a permission prompt is not
// stopped.
func TestDormantStopKeepsL4(t *testing.T) {
	held := []string{"state", "suspended", "sleep_intent", "user-hold", "held_until", rowAt(100 * 365 * 24 * time.Hour)}
	s, _, leaf := stopRun(t, held, undesired(drainSuspended), func(_ *effectPass, l *fenceLeaf, _ beads.Store, _ string) {
		l.SetPendingInteraction("s-gc-1", &runtime.PendingInteraction{RequestID: "r", Kind: "approval"})
	})
	if s.Outcome != settledRefused || s.Cause != fencePending || stops(leaf) != 0 {
		t.Fatalf("settlement %+v, %d stops; want refused pending and no stop", s, stops(leaf))
	}
}

// TestDormantStopOrigins (#7612 review, S04/S05; C6a ruling 2). Kills a
// dormant stop for one origin only: a killed row and a bare user-hold
// intent each get the direct stop, and a killed row keeps
// sleep_reason=killed.
func TestDormantStopOrigins(t *testing.T) {
	for _, c := range []struct {
		name   string
		meta   []string
		work   bool
		reason string
	}{
		{"killed", []string{"state", "asleep", "sleep_reason", "killed", "slept_at", rowAt(-time.Hour)}, false, "killed"},
		{"killed, with work", []string{"state", "asleep", "sleep_reason", "killed", "slept_at", rowAt(-time.Hour)}, true, "killed"},
		{"user-hold intent", []string{"sleep_intent", "user-hold"}, false, ""},
	} {
		s, got, leaf := stopRun(t, c.meta, undesired(drainOrphaned), func(_ *effectPass, _ *fenceLeaf, store beads.Store, id string) {
			if c.work {
				assignWork(t, store, id)
			}
		})
		if s.Outcome != settledLanded || stops(leaf) != 1 || got.Status == "closed" || got.Metadata["sleep_reason"] != c.reason {
			t.Errorf("%s: settlement %+v, %d stops, status %s, row %v; want stopped, open, sleep_reason %q", c.name, s, stops(leaf), got.Status, got.Metadata, c.reason)
		}
	}
}

// TestFinalizeKeepsAnOperatorHeldSeatOpen (#7612 review, S21; legacy's
// held-seat rule). Kills a finalize that closes a pool seat an operator
// holds: it ends drained, open.
func TestFinalizeKeepsAnOperatorHeldSeatOpen(t *testing.T) {
	held := []string{"pool_managed", "true", "sleep_intent", "user-hold", "held_until", rowAt(time.Hour)}
	s, got, _ := stopRun(t, signaled(drainOrphaned, held...), undesired(drainOrphaned), nil)
	if s.Outcome != settledLanded || got.Status == "closed" || got.Metadata["state"] != string(session.StateDrained) {
		t.Fatalf("settlement %+v, status %s, row %v; want drained and open", s, got.Status, got.Metadata)
	}
}

// TestCloseRechecksWorkBeforeClosing (#7612 review, S23). Kills a premise
// close on the finalize section's work read alone: work assigned between
// the finalize and the close refuses the close, and the row stays open.
func TestCloseRechecksWorkBeforeClosing(t *testing.T) {
	s, got, _ := stopRun(t, signaled(drainOrphaned), undesired(drainOrphaned), func(p *effectPass, _ *fenceLeaf, store beads.Store, id string) {
		p.seam = func(_ context.Context, at txSeam, _ intent, section, _ int) error {
			if at == seamAfterCall && section == 1 {
				assignWork(t, store, id)
			}
			return nil
		}
	})
	if s.Outcome == settledLanded || got.Status == "closed" {
		t.Fatalf("settlement %+v, status %s; want the close refused", s, got.Status)
	}
}

// TestSameObjectGuardsForeignAndCorpse (#7612 review, S10/S11). Kills a
// Foreign finalize or a corpse removal decided on a read whose object
// changed between its brackets: a Foreign identity read across a replaced
// object is no verdict, and refuses; a corpse read so is still stopped, but
// not removed.
func TestSameObjectGuardsForeignAndCorpse(t *testing.T) {
	replaced := func(l runtime.Liveness, env map[string]string) *freshObserver {
		o := &freshObserver{Fake: runtime.NewFake(), l: l, env: env}
		reads := 0
		o.read = func() {
			if reads++; reads == 2 {
				o.l.ObjectID = "$2"
			}
		}
		return o
	}
	foreign := replaced(runtime.Liveness{Running: true, Alive: true, ObjectID: "$1", ObjectCreated: "1"}, map[string]string{"GC_SESSION_ID": "gc-9", "GC_INSTANCE_TOKEN": "tok-9"})
	s, _, _ := stopRun(t, signaled(drainOrphaned), undesired(drainOrphaned), func(p *effectPass, _ *fenceLeaf, _ beads.Store, _ string) { p.Runtime = foreign.tmux() })
	if s.Outcome == settledLanded {
		t.Fatalf("foreign across a replaced object: %+v, want no finalize", s)
	}
	corpse := replaced(runtime.Liveness{Corpse: true, ObjectID: "$1", ObjectCreated: "1"}, map[string]string{"GC_SESSION_ID": "gc-1", "GC_INSTANCE_TOKEN": "tok-9"})
	if err := corpse.Fake.Start(context.Background(), "s-gc-1", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	stopRun(t, signaled(drainOrphaned), undesired(drainOrphaned), func(p *effectPass, _ *fenceLeaf, _ beads.Store, _ string) { p.Runtime = corpse.tmux() })
	if n := corpse.Fake.CountCalls("KillCorpseObject", "s-gc-1"); n != 0 {
		t.Fatalf("a corpse read across a replaced object was removed (%d kills)", n)
	}
}
