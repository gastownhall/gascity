package main

import (
	"context"
	"io"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/workladder"
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

// TestStopFinalizeAndCloseRestOnTheirOwnRead (STALE-READ M1, E1). Kills a
// finalize or premise close that rests on section 0's relayed C8.8: a
// runtime back under the name after the confirmed stop, which the finalize
// section's (or the close section's) own read finds, refuses
// runtime-present, writing no drained finalize and closing nothing.
func TestStopFinalizeAndCloseRestOnTheirOwnRead(t *testing.T) {
	for _, c := range []struct {
		name    string
		meta    []string
		entry   func(*selectionEntry)
		section int // the section after whose Call the runtime comes back
	}{
		{"the finalize", signaled(drainOrphaned, "operator_drain", "3"), wanted, 0},
		{"the premise close", signaled(drainOrphaned, "pool_managed", "true"), undesired(drainOrphaned), 1},
	} {
		s, got, leaf := stopRun(t, c.meta, c.entry, func(p *effectPass, leaf *fenceLeaf, _ beads.Store, id string) {
			p.seam = func(_ context.Context, at txSeam, _ intent, section, _ int) error {
				if at == seamAfterCall && section == c.section {
					startRuntime(t, leaf.Fake, "s-gc-1", map[string]string{"GC_SESSION_ID": id, "GC_INSTANCE_TOKEN": "tok-3"})
				}
				return nil
			}
		})
		if !leaf.IsRunning("s-gc-1") {
			t.Fatalf("%s: the runtime did not come back", c.name)
		}
		if s.Outcome != settledRefused || s.Cause != causeRuntimePresent || got.Status == "closed" || got.Metadata["state"] == string(session.StateDrained) {
			t.Errorf("%s: settled %+v, row %s state %q; want refused %q, nothing finalized or closed", c.name, s, got.Status, got.Metadata["state"], causeRuntimePresent)
		}
	}
}

// TestStopVerbStopsConfirmsAndFinalizes (D3, D4; SESS-035, DRAIN-054).
// Kills a stop that never finalizes, a finalize that keeps the request or
// writes asleep where legacy writes drained, and a silent finalize: a
// signaled named row's live own runtime is stopped, confirmed gone, and the
// row ends drained in one CAS that clears the request, with session.stopped
// and the stop metric.
func TestStopVerbStopsConfirmsAndFinalizes(t *testing.T) {
	s, got, leaf := stopRun(t, signaled(drainOrphaned, session.OperatorDrainKey, "3"), wanted, nil)
	m := got.Metadata
	switch {
	case s.Outcome != settledLanded || stops(leaf) != 1:
		t.Fatalf("settlement %+v with %d stops, want landed after one", s, stops(leaf))
	case m["state"] != string(session.StateDrained) || m[session.DrainIntentReasonKey] != "" || m["state_reason"] != "" || m[session.OperatorDrainKey] != "":
		t.Fatalf("row %v, want drained with the request and the operator's mark cleared", m)
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

// TestFinalizeKeepsALateQuarantinesReason (mc-slkxd). Kills a finalize
// that overwrites the reason of a quarantine that landed after the signal:
// while quarantined_until is ahead the quarantine dominates, so the idle
// sleep keeps quarantine, rate_limit or context-churn and its deadline; an
// expired quarantine's reason is the finalize's.
func TestFinalizeKeepsALateQuarantinesReason(t *testing.T) {
	sleep := func(e *selectionEntry) { e.Desired, e.Reason = desireSleep, reasonIdleSleep }
	for _, c := range []struct {
		reason string
		until  time.Duration
		want   string
	}{
		{"quarantine", time.Hour, "quarantine"},
		{"rate_limit", time.Hour, "rate_limit"},
		{"context-churn", time.Hour, "context-churn"},
		{"quarantine", -time.Minute, "idle"},
	} {
		until := time.Now().Add(c.until).Format(time.RFC3339) // the effect's clock is the wall's
		s, got, _ := stopRun(t, signaled(drainIdle, "sleep_intent", "idle-stop-pending", "sleep_reason", c.reason, "quarantined_until", until), sleep, nil)
		m := got.Metadata
		if s.Outcome != settledLanded || m["state"] != "asleep" || m["sleep_reason"] != c.want || m["quarantined_until"] != until {
			t.Fatalf("%s until %s: settlement %+v, row %v; want asleep with sleep_reason %q, the quarantine kept", c.reason, c.until, s, m, c.want)
		}
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

// blockingWork is a work leg whose seat-scoped live lists block while
// armed, until release closes: the finalize's bounded probe expires at
// fenceProbeTimeout and hands the decide a zero finalizeWork. (A panicking
// list would not do: seat_work recovers it per store as a read error.)
type blockingWork struct {
	beads.Store
	armed   *atomic.Bool
	release <-chan struct{}
}

func (s blockingWork) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Assignee != "" && q.Live && s.armed.Load() {
		<-s.release
	}
	return s.Store.List(q)
}

// claimFor gives the row id an in-progress work bead and returns its ID.
func claimFor(t *testing.T, store beads.Store, id string) string {
	t.Helper()
	b, err := store.Create(beads.Bead{Title: "w", Type: "task", Assignee: id})
	if err == nil {
		err = store.Update(b.ID, beads.UpdateOpts{Status: stringPtr("in_progress")})
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.ID
}

// Kills a close over a lost work read (STALE-READ-ARCH §7.1, PR 1), under
// synctest so the expiries cost no real time: a pool seat holding
// in-progress work whose finalize work read expires is not closed, its
// work kept; a seat whose clean finalize read found none, assigned work
// after it, whose premise close's own read expires, is not closed either;
// a clean read with no work closes. The zero value or a probe error holds
// an ordinary row and a soft-suspended pool seat alike in stopChoice.
func TestFinalizeLostWorkReadNeverCloses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		lose   int // the section whose probe expires: 1 the finalize's, 2 the close's; 0 none
		closed bool
	}{{"a clean read", 0, true}, {"the finalize's read expires over work", 1, false}, {"the close's read expires after a late claim", 2, false}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				defer close(release)
				armed := &atomic.Bool{}
				var workID string
				var work beads.Store
				s, got, _ := stopRun(t, signaled(drainOrphaned, "pool_managed", "true"), undesired(drainOrphaned), func(p *effectPass, _ *fenceLeaf, store beads.Store, id string) {
					work = store
					if tc.lose == 1 {
						workID = claimFor(t, store, id)
						armed.Store(true)
					}
					p.reads.legs = testWorkLegs(p.World.CityPath, p.World.Env.Cfg, blockingWork{Store: store, armed: armed, release: release}, nil).readOnly()
					p.seam = func(_ context.Context, at txSeam, _ intent, section, _ int) error {
						if tc.lose == 2 && at == seamAfterCall && section == 1 { // after the finalize's clean read, before the close's
							workID = claimFor(t, store, id)
							armed.Store(true)
						}
						return nil
					}
				})
				if tc.lose != 0 && workID == "" {
					t.Fatal("the run never reached the section whose read it loses")
				}
				closed := got.Status == "closed"
				if closed != tc.closed {
					t.Fatalf("settlement %+v, status %s, row %v; want closed %v", s, got.Status, got.Metadata, tc.closed)
				}
				if workID != "" {
					if wb, _ := work.Get(workID); wb.Status != "in_progress" || wb.Assignee != got.ID {
						t.Fatalf("work %s %q assignee %q; want kept on the seat", workID, wb.Status, wb.Assignee)
					}
				}
			})
		})
	}
	// A soft-suspended pool seat keeps no slot (#7718, KeepsSlot), so only
	// its work read stands between it and a close.
	soft := censusRowOf(t, drainRow("state", "suspended", "pool_managed", "true", "suspended_at", rowAt(-time.Hour))).Info
	if session.DispositionOfInfo(soft).KeepsSlot() {
		t.Fatal("fixture: want a soft suspend, which keeps no slot")
	}
	for _, row := range []session.Info{fenceRow("a", "tok-a"), soft} {
		v := txView{It: intent{Kind: intentStop, Reason: decideStopVerb + drainOrphaned}, Row: row, Now: gatherNow}
		if !stopChoice(v, finalizeWork{free: true}, nil).CloseSeat {
			t.Fatalf("%s: a clean read closes no seat; the cases below prove nothing", row.ID)
		}
		for _, c := range []struct {
			work finalizeWork
			err  error
		}{{finalizeWork{}, nil}, {finalizeWork{free: true}, errProbeExpired}, {finalizeWork{}, errProbeExpired}} {
			if stopChoice(v, c.work, c.err).CloseSeat {
				t.Errorf("%s: work %+v, probe error %v: the finalize closes, want it held as work", row.ID, c.work, c.err)
			}
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

// TestFinalizeNeverClosesOverAnHonoredWake (D7 rule 4, v5.10n). Kills a
// premise close over a pending explicit wake AL1 honors, whatever the
// allocation's desire (a row out of its template's desired set kept at the
// max included), and a finalize that stamps the wake served: the stopped
// pool seat is drained, not closed, its slept_at left, and the wake left to
// the next pass's start.
func TestFinalizeNeverClosesOverAnHonoredWake(t *testing.T) {
	wake := []string{"pool_managed", "true", "wake_request", "explicit", "wake_requested_at", rowAt(-time.Second)}
	for _, d := range []desire{desireWake, desireKeep} {
		s, got, _ := stopRun(t, signaled(drainOrphaned, wake...), func(e *selectionEntry) { e.Desired, e.WakeHonored = d, true }, func(p *effectPass, _ *fenceLeaf, store beads.Store, _ string) {
			p.Writers[rowLeg] = fencedWriter{store: atomicClosing{store.(*beads.MemStore)}}
		})
		if s.Outcome != settledLanded || got.Status == "closed" || got.Metadata["state"] != string(session.StateDrained) || got.Metadata["wake_request"] != "explicit" || got.Metadata["slept_at"] != "" {
			t.Fatalf("%s: settlement %+v, status %s, row %v; want drained, open, no slept_at, the wake kept", d, s, got.Status, got.Metadata)
		}
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
		s, got, _ := stopRun(t, signaled(reason, "pool_managed", "true", session.OperatorDrainKey, "3"), wanted, nil)
		if s.Outcome != settledLanded || got.Status != "closed" || got.Metadata["state"] != "drained" || got.Metadata["close_reason"] != session.CanonicalCloseReason("drained") {
			t.Errorf("%s: settlement %+v, status %s, row %v; want legacy's drained close", reason, s, got.Status, got.Metadata)
		}
		for _, k := range []string{session.DrainIntentReasonKey, session.DrainIntentAtKey, session.DrainIntentIncarnationKey, session.OperatorDrainKey} {
			if got.Metadata[k] != "" {
				t.Errorf("%s: the closed row keeps %s=%q, want the stop request cleared", reason, k, got.Metadata[k])
			}
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

// killedWoken is a killed row the allocation wants awake: a manual or
// always-named seat after `gc session kill` (CONTRACT v5.10d).
func killedWoken(e *selectionEntry) { e.Desired, e.InDesired = desireWake, true }

// killedMeta is a killed row's metadata.
var killedMeta = []string{"state", "asleep", "sleep_reason", "killed", "slept_at", rowAt(-time.Hour)}

// dormantSettled applies a dormant stop's settlement s of row id to a fresh
// planner's backoff table and returns the row's record.
func dormantSettled(id string, s settlement) backoffRecord {
	p := newPlanner(newFakePlannerClock(plannerT0), func() time.Duration { return time.Minute }, nil, newInflightMap(), nil, io.Discard)
	k := rowKey{Leg: rowLeg, ID: id}
	s.Key, s.Kind, s.Reason, s.At = k, intentStop, decideDormantStop, plannerT0
	p.backoffSettled(s)
	return p.backoff.Record(rowBackoffKey(k))
}

// TestDormantStopOfAGoneRuntimeSettlesNoop (CONTRACT v5.10d (a)). Kills a
// killed seat's restart held by a row backoff: the pass read the runtime
// alive before the kill's own stop ended it, so A4 proposed the dormant
// stop; its fresh read finds nothing under the name, and the stop settles as
// a no-op (no Stop, no write) that backs nothing off and notes the runtime
// gone for the next pass.
func TestDormantStopOfAGoneRuntimeSettlesNoop(t *testing.T) {
	var before int
	s, got, leaf := stopRun(t, killedMeta, killedWoken, func(_ *effectPass, l *fenceLeaf, _ beads.Store, _ string) {
		if err := l.Stop("s-gc-1"); err != nil { // the kill's own stop, after the pass read
			t.Fatal(err)
		}
		before = stops(l)
	})
	if s.Outcome != settledNoop || s.Cause != causeAlreadyGone || stops(leaf) != before || got.Metadata["state"] != "asleep" || got.Metadata["sleep_reason"] != "killed" {
		t.Fatalf("settlement %+v, %d stops, row %v; want a no-op %q, no stop, the killed row untouched", s, stops(leaf)-before, got.Metadata, causeAlreadyGone)
	}
	if n := s.Facts.Noted; n == nil || !n.Gone || n.Name != "s-gc-1" {
		t.Fatalf("noted %+v, want s-gc-1 gone", n)
	}
	if r := dormantSettled(got.ID, s); r.live(plannerT0) {
		t.Fatalf("row backoff %+v after the gone runtime's no-op, want none", r)
	}
}

// TestDormantStopRedecidedOverALiveRuntimeBacksOff (CONTRACT v5.10d (a),
// scoped). Kills the already-gone no-op widened to every redecided stop: a
// dormant stop whose fresh read finds a runtime still under the name, no
// longer the row's own, refuses redecided and keeps its row backoff (the
// lease churn the backoff paces).
func TestDormantStopRedecidedOverALiveRuntimeBacksOff(t *testing.T) {
	s, got, leaf := stopRun(t, killedMeta, killedWoken, func(_ *effectPass, l *fenceLeaf, _ beads.Store, _ string) {
		if err := l.Stop("s-gc-1"); err != nil {
			t.Fatal(err)
		}
		startRuntime(t, l.Fake, "s-gc-1", map[string]string{"GC_SESSION_ID": "gc-9", "GC_INSTANCE_TOKEN": "tok-9"})
	})
	if !leaf.IsRunning("s-gc-1") {
		t.Fatal("premise: a runtime runs under the name")
	}
	if s.Outcome != settledRefused || s.Cause != causeRedecided || s.Facts.Noted != nil {
		t.Fatalf("settlement %+v, want refused %q with nothing noted", s, causeRedecided)
	}
	if r := dormantSettled(got.ID, s); !r.live(plannerT0) || r.Cause != causeRedecided {
		t.Fatalf("row backoff %+v, want a live redecided backoff", r)
	}
}

// TestOnlyTheDormantStopOfAGoneRuntimeIsANoop (CONTRACT v5.10d (a),
// scoped). Kills the already-gone no-op taken by any stop the fresh read
// redecides, or over a corpse: on the same killed row and the same absent
// runtime, the operator-dormant stop settles as the no-op, and an idle stop
// refuses redecided, keeping its backoff; over a corpse the dormant stop
// refuses redecided too.
func TestOnlyTheDormantStopOfAGoneRuntimeIsANoop(t *testing.T) {
	p, it, store, _ := admittedBeginOn(t, killedMeta, killedWoken)
	b, err := store.Get(it.Key.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		reason string
		class  runtimeClass
		want   txStep
	}{
		{decideDormantStop, rtAbsent, txStep{Done: true, Cause: causeAlreadyGone}},
		{decideIdleTimeout, rtAbsent, txStep{Refuse: causeRedecided}},
		// A corpse is no runtime gone: still listed, its dead pane the
		// recycle's or the close's to remove (the #7775 review's a4).
		{decideDormantStop, rtCorpse, txStep{Refuse: causeRedecided}},
	} {
		v := txView{
			It: intent{Key: it.Key, Kind: intentStop, Reason: c.reason, Basis: it.Basis}, World: p.World, Alloc: p.Alloc,
			Row: sessionInfoFromBead(b), Meta: b.Metadata, RT: &txRuntime{Class: c.class}, Now: gatherNow,
		}
		if got := stopDecide(v); got.Done != c.want.Done || got.Cause != c.want.Cause || got.Refuse != c.want.Refuse {
			t.Errorf("%s over runtime class %d: %+v, want %+v", c.reason, c.class, got, c.want)
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

// TestStopRefusesARuntimeReplacedBeforeTheStop (mc-rq4d0). Kills a
// name-based Stop that kills whatever holds the name at the call: a stop the
// fence passed on the row's own runtime refuses runtime_moved, stopping
// nothing and finalizing nothing, when the runtime under the name is
// replaced between the fence's read and the Stop, by another row's identity,
// or by another session object carrying the row's own (its id, its creation
// or both differing); with nothing replaced the stop goes ahead.
func TestStopRefusesARuntimeReplacedBeforeTheStop(t *testing.T) {
	s, got, leaf := stopRun(t, signaled(drainOrphaned), undesired(drainOrphaned), func(p *effectPass, leaf *fenceLeaf, _ beads.Store, _ string) {
		p.seam = func(_ context.Context, at txSeam, _ intent, section, _ int) error {
			if at == seamBeforeCall && section == 0 {
				for k, v := range map[string]string{"GC_SESSION_ID": "gc-9", "GC_INSTANCE_TOKEN": "tok-9"} {
					if err := leaf.SetMeta("s-gc-1", k, v); err != nil {
						t.Fatal(err)
					}
				}
			}
			return nil
		}
	})
	if stops(leaf) != 0 || s.Outcome == settledLanded || !signaledRow(got) {
		t.Fatalf("another row's identity before the Stop: settlement %+v, %d stops, row %v; want no stop and no finalize", s, stops(leaf), got.Metadata)
	}

	for _, c := range []struct {
		name, id, created string
	}{
		{"another object", "$2", "2"},
		{"another object id", "$2", "1"},
		{"another creation", "$1", "2"},
		{"nothing replaced", "$1", "1"},
	} {
		replace := c.id != "$1" || c.created != "1"
		var obs *freshObserver
		s, got, _ = stopRun(t, signaled(drainOrphaned), undesired(drainOrphaned), func(p *effectPass, _ *fenceLeaf, _ beads.Store, id string) {
			obs = &freshObserver{Fake: runtime.NewFake(), l: runtime.Liveness{Running: true, Alive: true, ObjectID: "$1", ObjectCreated: "1"}, env: map[string]string{"GC_SESSION_ID": id, "GC_INSTANCE_TOKEN": "tok-3"}}
			if err := obs.Fake.Start(context.Background(), "s-gc-1", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			p.Runtime = obs.tmux()
			p.seam = func(_ context.Context, at txSeam, _ intent, section, _ int) error {
				if at == seamBeforeCall && section == 0 {
					obs.l.ObjectID, obs.l.ObjectCreated = c.id, c.created // the row's identity, on another object when replaced
				}
				return nil
			}
		})
		if n := obs.Fake.CountCalls("Stop", "s-gc-1"); replace && (n != 0 || s.Cause != fenceRuntimeMoved) || !replace && n != 1 {
			t.Fatalf("%s: settlement %+v, %d stops, row %v; want a stop only on the fenced object", c.name, s, n, got.Metadata)
		}
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

// TestDormantFinalizeClearsALeftoverRequest (v5.9n; the C7 builder's lead;
// #7648 review 7). Kills a finalize that reads the stop request off a row
// rebuilt without its stop keys, or that clears only an active request: an
// operator-dormant stop of a row carrying a current ack clears it in its
// finalize, as D4 does always; on a suspended row the ack is residue
// (activeStop's rule 2), cleared just the same. A soft suspend is no
// dormant row (v5.9u): the awake heal takes it.
func TestDormantFinalizeClearsALeftoverRequest(t *testing.T) {
	for _, c := range []struct {
		name string
		meta []string
	}{
		{"user-hold", []string{"sleep_intent", "user-hold"}},
		{"suspended", []string{"state", string(session.StateSuspended), "sleep_intent", "user-hold", "suspended_at", rowAt(-time.Minute)}},
	} {
		s, got, leaf := stopRun(t, append(c.meta, session.DrainAckIncarnationKey, "3", session.DrainAckAtKey, rowAt(-time.Minute)), undesired(drainOrphaned), nil)
		if s.Outcome != settledLanded || stops(leaf) != 1 || got.Metadata[session.DrainAckIncarnationKey] != "" || got.Metadata[session.DrainAckAtKey] != "" {
			t.Errorf("%s: settlement %+v, %d stops, row %v; want the dormant stop finalized with its request cleared", c.name, s, stops(leaf), got.Metadata)
		}
	}
}

// TestExecutionStalledStopReleasesItsClaimAndCloses (v5.10x, maintainer-city
// build 42dd71f94d). Kills an execution-stalled drain that finalizes the seat
// asleep holding the claim it was drained for: the assigned-work wake then
// restarted it on the same claim, and the backstop drained it again, in a
// loop. A confirmed execution-stalled stop gives back the claim its marker
// names, so the seat closes and a fresh seat takes the claim; another
// in-progress bead of the seat's, or the same drain for another reason,
// keeps its work as before.
//
// T9 (mc-358yo, PHASE1-2 §6.5): with the work ladder off (the backstops'
// stop), a claim the seat records that a turn began, by
// execution_claim_started at the row's current token, is kept unless a
// dispatch hold parks it; a work_started entry, or an earlier incarnation's
// stamp, would keep a claim the seat was woken on and never ran, and bring
// the v5.10x loop back, so the claim goes back as before. With the ladder
// on, the stop is the ladder's recycle, which keeps the claim whatever the
// records say: the ladder gives back an unstarted claim itself, by a CAS
// proving it unstarted, and never asks a stop to. A marker the backstops did
// not write for the row's own incarnation names nothing, and a recycle the
// ladder decided at the row's generation stays the ladder's after the
// switch flips to off.
func TestExecutionStalledStopReleasesItsClaimAndCloses(t *testing.T) {
	for _, c := range []struct {
		name     string
		reason   string
		release  bool
		other    bool   // the seat holds a second in-progress bead
		started  string // the seat's record: "cell", "stamp" (current token), "old-stamp" or "unreadable"
		ladder   bool   // the work ladder is on: the stop is its recycle
		hold     string // a dispatch hold label on the claim
		stale    bool   // the backstop's marker is another incarnation's
		recycled bool   // the ladder recycled the seat at its generation, then the switch flipped off
		badCell  bool   // the row's work_ladder cell is unreadable
	}{
		{name: "execution-stalled", reason: executionStalledDrainReason, release: true},
		{name: "execution-stalled, a second claim held", reason: executionStalledDrainReason, release: true, other: true},
		{name: "another drain reason", reason: drainOrphaned},
		{name: "ladder off: work_started names it, released (v5.10x)", reason: executionStalledDrainReason, release: true, started: "cell"},
		{name: "ladder off: an earlier incarnation's stamp, released (v5.10x)", reason: executionStalledDrainReason, release: true, started: "old-stamp"},
		{name: "ladder off: this incarnation's stamp, kept", reason: executionStalledDrainReason, started: "stamp"},
		{name: "ladder off: stamped but hold:mayor, released", reason: executionStalledDrainReason, release: true, started: "stamp", hold: beadmeta.HoldMayorLabel},
		{name: "ladder off: a stale marker, kept", reason: executionStalledDrainReason, stale: true},
		{name: "ladder off mid-recycle: the ladder's recycle at this generation, kept", reason: executionStalledDrainReason, recycled: true},
		{name: "ladder off, an unreadable ladder cell: maybe the ladder's recycle, kept", reason: executionStalledDrainReason, badCell: true},
		{name: "ladder on: kept whatever the records say (the recycle gives back nothing)", reason: executionStalledDrainReason, ladder: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var claimID, otherID string
			var work beads.Store
			s, got, leaf := stopRun(t, signaled(c.reason), undesired(c.reason), func(p *effectPass, _ *fenceLeaf, store beads.Store, id string) {
				work = store
				if !c.ladder {
					p.World.Env.Cfg.Daemon.WorkLadder = "off" // the backstops' stop gives their claim back
				}
				claim, err := store.Create(beads.Bead{Title: "stalled step", Type: "task"})
				if err != nil {
					t.Fatal(err)
				}
				inProgress := "in_progress"
				if err := store.Update(claim.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &id}); err != nil {
					t.Fatal(err)
				}
				claimID = claim.ID
				if c.other {
					other, err := store.Create(beads.Bead{Title: "other step", Type: "task"})
					if err != nil {
						t.Fatal(err)
					}
					if err := store.Update(other.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &id}); err != nil {
						t.Fatal(err)
					}
					otherID = other.ID
				}
				row, err := store.Get(id)
				if err != nil {
					t.Fatal(err)
				}
				token := row.Metadata["instance_token"]
				if c.stale {
					token = "tok-before-the-ladder"
				}
				if err := store.SetMetadataBatch(id, map[string]string{executionClaimNudgeWorkKey: claim.ID, executionClaimNudgeTokenKey: token}); err != nil {
					t.Fatal(err)
				}
				if c.badCell {
					casSetMeta(t, store, id, session.WorkLadderKey, "{")
				}
				if c.recycled {
					gen, _ := strconv.ParseInt(row.Metadata["generation"], 10, 64)
					casSetMeta(t, store, id, session.WorkLadderKey, workladder.EncodeLadder(workladder.Record{V: 1, Gen: gen, RecycledGen: gen}))
				}
				if c.hold != "" {
					if err := store.Update(claim.ID, beads.UpdateOpts{Labels: []string{c.hold}}); err != nil {
						t.Fatal(err)
					}
				}
				row, err = store.Get(id)
				if err != nil {
					t.Fatal(err)
				}
				switch c.started {
				case "cell":
					casSetMeta(t, store, id, session.WorkStartedKey, session.EncodeWorkStarted(session.WorkStarted{}.With(session.WorkStartedEntry{Bead: claim.ID, Via: session.WorkStartedViaClaim})))
				case "stamp":
					if err := store.SetMetadata(id, executionClaimStartedKey, row.Metadata["instance_token"]+"|"+claim.ID); err != nil {
						t.Fatal(err)
					}
				case "old-stamp":
					if err := store.SetMetadata(id, executionClaimStartedKey, "tok-old|"+claim.ID); err != nil {
						t.Fatal(err)
					}
				case "unreadable":
					casSetMeta(t, store, id, session.WorkStartedKey, "{")
				}
				rel := newCloseReleases(p.World, nil, io.Discard)
				rel.legs = testWorkLegs(p.World.CityPath, p.World.Env.Cfg, store, nil) // the pass's raw work legs, over the row's store
				p.held.release = rel
			})
			claim, err := work.Get(claimID)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case s.Outcome != settledLanded || stops(leaf) != 1:
				t.Fatalf("settlement %+v with %d stops, want landed after one", s, stops(leaf))
			case c.other:
				other, err := work.Get(otherID)
				if err != nil {
					t.Fatal(err)
				}
				if claim.Status != "open" || other.Status != "in_progress" || got.Status == "closed" {
					t.Fatalf("named claim %q, other %q, row %s; want only the named claim given back, the other kept and the seat open", claim.Status, other.Status, got.Status)
				}
			case c.release && (claim.Status != "open" || claim.Assignee != "" || got.Status != "closed"):
				t.Fatalf("claim %s %q assignee %q, row %s %v; want the claim given back and the seat closed", claimID, claim.Status, claim.Assignee, got.Status, got.Metadata)
			case !c.release && (claim.Status != "in_progress" || got.Status == "closed"):
				t.Fatalf("claim %s %q, row %s; want the work kept and the seat open", claimID, claim.Status, got.Status)
			}
		})
	}
}

// TestExecutionStalledStopReleasesOnlyOnAConfirmedStop (v5.10x X7). Kills a
// stalled stop that gives the claim back before its stop is confirmed: an
// attached seat the fence refuses, and a stop whose fresh read still finds
// the runtime, leave the claim in_progress on the seat, which stays open.
func TestExecutionStalledStopReleasesOnlyOnAConfirmedStop(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(*fenceLeaf)
	}{
		{"attached", func(l *fenceLeaf) { l.SetAttached("s-gc-1", true) }},
		{"the stop unconfirmed", func(l *fenceLeaf) { l.StopLeavesRunning = map[string]bool{"s-gc-1": true} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			var claimID string
			var work beads.Store
			s, got, _ := stopRun(t, signaled(executionStalledDrainReason), undesired(executionStalledDrainReason), func(p *effectPass, l *fenceLeaf, store beads.Store, id string) {
				work = store
				p.World.Env.Cfg.Daemon.WorkLadder = "off" // the backstops' stop, which would give the claim back
				claim, err := store.Create(beads.Bead{Title: "stalled step", Type: "task"})
				if err != nil {
					t.Fatal(err)
				}
				inProgress := "in_progress"
				if err := store.Update(claim.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &id}); err != nil {
					t.Fatal(err)
				}
				claimID = claim.ID
				markStalledClaim(t, store, id, claim.ID)
				rel := newCloseReleases(p.World, nil, io.Discard)
				rel.legs = testWorkLegs(p.World.CityPath, p.World.Env.Cfg, store, nil)
				p.held.release = rel
				c.setup(l)
			})
			claim, err := work.Get(claimID)
			if err != nil {
				t.Fatal(err)
			}
			if s.Outcome != settledRefused || claim.Status != "in_progress" || got.Status == "closed" {
				t.Fatalf("settlement %+v, claim %q, row %s; want refused, the claim kept, the seat open", s, claim.Status, got.Status)
			}
		})
	}
}

// TestExecutionStalledStopWithNothingLiveReleasesOnConfirmation (v5.10x X7,
// mc-muv2x review M7). Kills a stalled stop that, with no live runtime to
// stop, gives the claim back before C8.8 confirms it: a runtime back under
// the name by the confirmation's fresh read leaves the claim in_progress on
// the seat, which stays open; confirmed, the claim is given back and the
// seat closes.
func TestExecutionStalledStopWithNothingLiveReleasesOnConfirmation(t *testing.T) {
	for _, back := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "back by the confirmation"}[back], func(t *testing.T) {
			obs := &freshObserver{Fake: runtime.NewFake(), env: map[string]string{"GC_SESSION_ID": "gc-1", "GC_INSTANCE_TOKEN": "tok-3"}}
			var claimID string
			var work beads.Store
			s, got, _ := stopRun(t, signaled(executionStalledDrainReason), undesired(executionStalledDrainReason), func(p *effectPass, _ *fenceLeaf, store beads.Store, id string) {
				work = store
				p.World.Env.Cfg.Daemon.WorkLadder = "off" // the backstops' stop gives their claim back
				claim, err := store.Create(beads.Bead{Title: "stalled step", Type: "task"})
				if err != nil {
					t.Fatal(err)
				}
				inProgress := "in_progress"
				if err := store.Update(claim.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &id}); err != nil {
					t.Fatal(err)
				}
				claimID = claim.ID
				markStalledClaim(t, store, id, claim.ID)
				rel := newCloseReleases(p.World, nil, io.Discard)
				rel.legs = testWorkLegs(p.World.CityPath, p.World.Env.Cfg, store, nil)
				p.held.release = rel
				p.Runtime = obs.tmux()
				p.seam = func(_ context.Context, at txSeam, _ intent, _, _ int) error {
					if back && at == seamBeforeCall { // the fence read nothing live; C8.8 finds it back
						obs.l = runtime.Liveness{Running: true, Alive: true, ObjectID: "$1", ObjectCreated: "1"}
					}
					return nil
				}
			})
			claim, err := work.Get(claimID)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case back && (s.Outcome != settledRefused || claim.Status != "in_progress" || got.Status == "closed"):
				t.Fatalf("settlement %+v, claim %q, row %s; want refused, the claim kept, the seat open", s, claim.Status, got.Status)
			case !back && (s.Outcome != settledLanded || claim.Status != "open" || got.Status != "closed"):
				t.Fatalf("settlement %+v, claim %q, row %s; want landed, the claim given back, the seat closed", s, claim.Status, got.Status)
			}
		})
	}
}

// casSetMeta writes key on row id by a CAS at its current revision, as a
// CASOnly key must be written.
func casSetMeta(t *testing.T, store beads.Store, id, key, value string) {
	t.Helper()
	w, ok := beads.ConditionalWriterFor(store)
	if !ok {
		t.Fatalf("store %T has no conditional writer", store)
	}
	b, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.UpdateIfMatch(id, b.Revision, beads.UpdateOpts{Metadata: map[string]string{key: value}}); err != nil {
		t.Fatal(err)
	}
}

// markStalledClaim stamps the execution backstop's marker on row id for
// claim, as the backstops write it for the row's own incarnation.
func markStalledClaim(t *testing.T, store beads.Store, id, claim string) {
	t.Helper()
	row, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadataBatch(id, map[string]string{executionClaimNudgeWorkKey: claim, executionClaimNudgeTokenKey: row.Metadata["instance_token"]}); err != nil {
		t.Fatal(err)
	}
}
