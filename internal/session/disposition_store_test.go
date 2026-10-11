package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
)

// dispositionRow is a CAS-capable store with one active session row.
func dispositionRow(t *testing.T) (beads.Store, *Store, string) {
	t.Helper()
	f := newLeaseFixture(t)
	if err := f.store.SetMetadataBatch(f.id, map[string]string{"state": string(StateActive), "generation": "1", "instance_token": "tok"}); err != nil {
		t.Fatal(err)
	}
	return f.store, f.front, f.id
}

func freshDecision(t *testing.T, front *Store, id string) Decided {
	t.Helper()
	b, err := front.freshBead(id)
	if err != nil {
		t.Fatal(err)
	}
	return Decide(infoFromPersistedBead(b), FactsResumeConsume)
}

// TestApplyDispositionWritesOnlyTheMove: a move writes the class keys it
// changed and its lifecycle, and no other key; the row decodes to the
// disposition returned, and Was holds what the write replaced.
func TestApplyDispositionWritesOnlyTheMove(t *testing.T) {
	store, front, id := dispositionRow(t)
	if err := store.SetMetadataBatch(id, map[string]string{"wait_hold": "w", "quarantined_until": "later"}); err != nil {
		t.Fatal(err)
	}
	life := func(Disposition, Disposition, Info) MetadataPatch {
		return MetadataPatch{"state": string(StateSuspended)}
	}
	w, err := front.ApplyDisposition(freshDecision(t, front, id), ActorOperator, DispEvent{Kind: EvSuspend}, life, dispT0)
	if err != nil || w.Result != CommitLanded || w.To.Operator != OperatorSuspended || !w.To.Wait {
		t.Fatalf("ApplyDisposition = %+v, %v; want landed, suspended over the wait", w, err)
	}
	for _, k := range []string{"wait_hold", "quarantined_until"} {
		if _, wrote := w.Wrote[k]; wrote {
			t.Errorf("the suspend wrote %s, which it did not move", k)
		}
	}
	b, _ := store.Get(id)
	if b.Metadata["state"] != string(StateSuspended) || b.Metadata["wait_hold"] != "w" || b.Metadata["quarantined_until"] != "later" || b.Metadata["sleep_intent"] != string(SleepReasonUserHold) {
		t.Fatalf("row = %v, want the suspend over the untouched wait and quarantine", b.Metadata)
	}
	if w.Was["sleep_intent"] != "" || w.Was["state"] != string(StateActive) {
		t.Fatalf("Was = %v, want the replaced values", w.Was)
	}
}

// TestApplyDispositionRefusesAndMoves: a refused cell, a moved premise and a
// closed row write nothing; a move that changes nothing lands unwritten; a
// lifecycle that writes a class key is an error.
func TestApplyDispositionRefusesAndMoves(t *testing.T) {
	store, front, id := dispositionRow(t)
	if _, err := front.OperatorSuspend(id, dispT0); err != nil {
		t.Fatal(err)
	}
	before, _ := store.Get(id)
	d := freshDecision(t, front, id)
	_, err := front.ApplyDisposition(d, ActorBackground, DispEvent{Kind: EvResume}, nil, dispT0)
	var refused *ErrDispositionRefused
	if !errors.As(err, &refused) {
		t.Fatalf("a background resume of a suspended row = %v, want refused", err)
	}
	if w, err := front.ApplyDisposition(d, ActorOperator, DispEvent{Kind: EvRestart}, nil, dispT0); err != nil || w.Result != CommitLanded || w.Wrote != nil {
		t.Fatalf("a restart = %+v, %v; want landed unwritten", w, err)
	}
	after, _ := store.Get(id)
	if after.Revision != before.Revision {
		t.Fatalf("revision %d -> %d: a refused or unchanged move wrote", before.Revision, after.Revision)
	}
	bad := func(Disposition, Disposition, Info) MetadataPatch { return MetadataPatch{"held_until": ""} }
	if _, err := front.ApplyDisposition(d, ActorOperator, DispEvent{Kind: EvKill}, bad, dispT0); err == nil {
		t.Fatal("a lifecycle that writes a class key was written")
	}
	if err := store.SetMetadataBatch(id, map[string]string{"generation": "2"}); err != nil {
		t.Fatal(err)
	}
	if w, err := front.ApplyDisposition(d, ActorOperator, DispEvent{Kind: EvKill}, nil, dispT0); err != nil || w.Result != CommitMoved {
		t.Fatalf("a kill of a moved premise = %+v, %v; want moved", w, err)
	}
	if err := store.Close(id); err != nil {
		t.Fatal(err)
	}
	if w, err := front.ApplyDispositionFresh(id, ActorOperator, DispEvent{Kind: EvKill}, nil, dispT0); err != nil || w.Result != CommitClosed {
		t.Fatalf("a kill of a closed row = %+v, %v; want closed", w, err)
	}
	if _, err := front.ApplyDisposition(Decided{}, ActorOperator, DispEvent{Kind: EvKill}, nil, dispT0); err == nil {
		t.Fatal("a zero decision was applied")
	}
}

// TestApplyDispositionContended: a CAS lost on every attempt writes nothing
// and reports contended, after an operator's full budget.
func TestApplyDispositionContended(t *testing.T) {
	stamped := stampedMemStore(t, gate.Auto, beads.NewMemStore())
	b, err := stamped.Create(beads.Bead{Title: "w", Type: BeadType, Labels: []string{LabelSession}, Metadata: map[string]string{"state": string(StateActive)}})
	if err != nil {
		t.Fatal(err)
	}
	loser := &wakeCASLoser{rowWriteRecorder: &rowWriteRecorder{Store: stamped}, backing: stamped}
	w, err := NewStore(beads.SessionStore{Store: loser}).ApplyDispositionFresh(b.ID, ActorOperator, DispEvent{Kind: EvSuspend}, nil, dispT0)
	if err != nil || w.Result != CommitContended {
		t.Fatalf("ApplyDispositionFresh under a CAS that always loses = %+v, %v; want contended", w, err)
	}
	if loser.n != operatorSuspendAttempts {
		t.Fatalf("an operator's write tried %d times, want %d", loser.n, operatorSuspendAttempts)
	}
	if got, _ := stamped.Get(b.ID); got.Metadata["sleep_intent"] != "" {
		t.Fatalf("a contended move wrote: %v", got.Metadata)
	}
}

// TestApplyDispositionFencesItsWrite: a write that lands between the read and
// the CAS makes the move decide again on the newer read.
func TestApplyDispositionFencesItsWrite(t *testing.T) {
	stamped := stampedMemStore(t, gate.Auto, beads.NewMemStore())
	b, err := stamped.Create(beads.Bead{Title: "w", Type: BeadType, Labels: []string{LabelSession}, Metadata: map[string]string{"state": string(StateActive)}})
	if err != nil {
		t.Fatal(err)
	}
	racer := &firstCASRacer{
		rowWriteRecorder: &rowWriteRecorder{Store: stamped}, backing: stamped,
		race: map[string]string{"wait_hold": "true"},
	}
	w, err := NewStore(beads.SessionStore{Store: racer}).ApplyDispositionFresh(b.ID, ActorOperator, DispEvent{Kind: EvSuspend}, nil, dispT0)
	if err != nil || w.Result != CommitLanded || !w.To.Wait {
		t.Fatalf("ApplyDispositionFresh over a racing wait = %+v, %v; want landed, deciding again over the wait", w, err)
	}
	if got, _ := stamped.Get(b.ID); got.Metadata["wait_hold"] != "true" || got.Metadata["sleep_intent"] != string(SleepReasonUserHold) {
		t.Fatalf("row = %v, want the suspend over the racing wait", got.Metadata)
	}
}

// firstCASRacer lands race on the row before the first UpdateIfMatch.
type firstCASRacer struct {
	*rowWriteRecorder
	backing beads.Store
	race    map[string]string
	raced   bool
}

func (c *firstCASRacer) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	if !c.raced {
		c.raced = true
		_ = c.backing.SetMetadataBatch(id, c.race)
	}
	return c.rowWriteRecorder.UpdateIfMatch(id, rev, opts)
}

// TestOperatorSuspendInForceStampsAgain: a suspend over one in force writes a
// fresh suspended_at and its lifecycle (state=suspended, the wake request
// cleared, D7.3), so a resume that read the old stamp refuses.
func TestOperatorSuspendInForceStampsAgain(t *testing.T) {
	store, front, id := dispositionRow(t)
	if _, err := front.OperatorSuspend(id, dispT0); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadataBatch(id, map[string]string{"state": string(StateAsleep), "wake_request": "explicit"}); err != nil {
		t.Fatal(err)
	}
	if _, err := front.OperatorSuspend(id, dispT0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	after, _ := store.Get(id)
	if after.Metadata["suspended_at"] != dispT0.Add(time.Minute).Format(time.RFC3339) || after.Metadata["state"] != string(StateSuspended) || after.Metadata["wake_request"] != "" {
		t.Fatalf("a suspend in force left %v, want stamped again, suspended, the wake cleared", after.Metadata)
	}
}

// TestNewerSuspendDuringResumeKeepsTheHold is the max review's P1: an
// operator's real suspend landing while another operator's resume starts the
// runtime refuses the consume; the hold stands.
func TestNewerSuspendDuringResumeKeepsTheHold(t *testing.T) {
	store := beads.NewMemStore()
	b := userHeldRow(t, store)
	sp := &startHookProvider{Fake: runtime.NewFake()}
	var suspendErr error
	sp.hook = func() {
		_, suspendErr = NewStore(beads.SessionStore{Store: store}).OperatorSuspend(b.ID, time.Now().Add(time.Second))
	}
	mgr := newTestManager(t, store, sp)
	err := mgr.Attach(context.Background(), testActor(mgr, ActorOperator), b.ID, "claude", runtime.Config{})
	if suspendErr != nil || !errors.Is(err, ErrStateSync) || heldKeys(t, store, b.ID)["sleep_intent"] != string(SleepReasonUserHold) {
		t.Fatalf("suspend %v, attach %v, intent %q: want the newer suspend to stand", suspendErr, err, heldKeys(t, store, b.ID)["sleep_intent"])
	}
}

// TestFailedStopRollbackRestoresOnlyItsOwnKeys is the max review's P2 and P3:
// a failed stop's rollback restores only the keys its suspend changed that
// still read as it wrote them; a wait or quarantine written during the stop
// stands, whether the suspend took hold or stamped one in force.
func TestFailedStopRollbackRestoresOnlyItsOwnKeys(t *testing.T) {
	for _, inForce := range []bool{false, true} {
		store := beads.NewMemStore()
		b := wakeRow(t, store)
		if inForce {
			if err := store.SetMetadataBatch(b.ID, map[string]string{
				"sleep_intent": "user-hold",
				"held_until":   time.Now().Add(IndefiniteHoldDuration).UTC().Format(time.RFC3339), "suspended_at": "2026-10-09T11:00:00Z",
			}); err != nil {
				t.Fatal(err)
			}
		}
		before, _ := store.Get(b.ID)
		q := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		sp := &stopSeesProvider{Fake: runtime.NewFake(), store: store, id: b.ID, fail: true}
		sp.during = func() {
			if err := store.SetMetadataBatch(b.ID, map[string]string{"quarantined_until": q, "wait_hold": "true"}); err != nil {
				t.Error(err)
			}
		}
		if err := sp.Start(context.Background(), "s-wake", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		if err := operatorSuspend(newTestManager(t, store, sp), b.ID); err == nil {
			t.Fatal("the failed stop's suspend succeeded")
		}
		got, _ := store.Get(b.ID)
		if got.Metadata["quarantined_until"] != q || got.Metadata["wait_hold"] != "true" {
			t.Errorf("in force %v: the rollback reverted a write made during the stop: %v", inForce, got.Metadata)
		}
		for _, k := range []string{"sleep_intent", "held_until", "suspended_at", "state"} {
			if got.Metadata[k] != before.Metadata[k] {
				t.Errorf("in force %v: %s = %q after the rollback, want %q", inForce, k, got.Metadata[k], before.Metadata[k])
			}
		}
	}
}

// TestSuspendRollbackOnlyWhileTheStampIsItsOwn: the rollback restores nothing
// once a newer suspend stamped the row, and leaves a key changed since.
func TestSuspendRollbackOnlyWhileTheStampIsItsOwn(t *testing.T) {
	w := DispositionWrite{
		Wrote: MetadataPatch{"suspended_at": "t1", "state": "suspended", "sleep_intent": "user-hold"},
		Was:   MetadataPatch{"suspended_at": "", "state": "active", "sleep_intent": ""},
	}
	if got := suspendRollback(w, map[string]string{"suspended_at": "t2", "state": "suspended"}); got != nil {
		t.Fatalf("rollback over a newer stamp = %v, want nothing", got)
	}
	got := suspendRollback(w, map[string]string{"suspended_at": "t1", "state": "draining", "sleep_intent": "user-hold"})
	if _, ok := got["state"]; ok || got["suspended_at"] != "" || got["sleep_intent"] != "" || len(got) != 2 {
		t.Fatalf("rollback = %v, want the stamp and intent restored, the state changed since left", got)
	}
	if got := suspendRollback(DispositionWrite{}, map[string]string{}); got != nil {
		t.Fatalf("rollback of an unwritten suspend = %v", got)
	}
}
