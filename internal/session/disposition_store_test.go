package session

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
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
	d, err := front.DecideFresh(id, FactsOperatorAct)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestApplyDispositionWritesTheMoveWhole: a move lands as the whole class
// encoded, with its lifecycle, and the row decodes to the disposition
// returned.
func TestApplyDispositionWritesTheMoveWhole(t *testing.T) {
	store, front, id := dispositionRow(t)
	if err := store.SetMetadataBatch(id, map[string]string{"wait_hold": "w"}); err != nil {
		t.Fatal(err)
	}
	life := func(Disposition, Disposition, Info) MetadataPatch {
		return MetadataPatch{"state": string(StateSuspended)}
	}
	res, to, err := front.ApplyDisposition(freshDecision(t, front, id), ActorOperator, DispEvent{Kind: EvSuspend}, life, dispT0)
	if err != nil || res != CommitLanded || to.Operator != OperatorSuspended || !to.Wait {
		t.Fatalf("ApplyDisposition = %v, %+v, %v; want landed, suspended over the wait", res, to, err)
	}
	b, _ := store.Get(id)
	if b.Metadata["state"] != string(StateSuspended) || b.Metadata["wait_hold"] != "true" || b.Metadata["sleep_intent"] != string(SleepReasonUserHold) {
		t.Fatalf("row = %v, want the suspend over the wait, state suspended", b.Metadata)
	}
	if got := DecodeDisposition(b.Metadata); got.Operator != to.Operator || !got.Until.Equal(to.Until) || got.Wait != to.Wait {
		t.Fatalf("the row decodes to %+v, want %+v", got, to)
	}
}

// TestApplyDispositionRefusesAndMoves: a refused cell, a moved premise and
// a closed row write nothing; a move that changes nothing lands unwritten;
// a lifecycle that writes a class key is an error.
func TestApplyDispositionRefusesAndMoves(t *testing.T) {
	store, front, id := dispositionRow(t)
	if _, err := front.OperatorSuspend(id, dispT0); err != nil {
		t.Fatal(err)
	}
	before, _ := store.Get(id)
	d := freshDecision(t, front, id)

	_, _, err := front.ApplyDisposition(d, ActorBackground, DispEvent{Kind: EvResume}, nil, dispT0)
	var refused *ErrDispositionRefused
	if !errors.As(err, &refused) {
		t.Fatalf("a background resume of a suspended row = %v, want refused", err)
	}
	if res, _, err := front.ApplyDisposition(d, ActorOperator, DispEvent{Kind: EvSuspend}, nil, dispT0.Add(time.Minute)); err != nil || res != CommitLanded {
		t.Fatalf("a suspend in force = %v, %v; want landed unwritten", res, err)
	}
	after, _ := store.Get(id)
	if after.Revision != before.Revision {
		t.Fatalf("revision %d -> %d: a refused or unchanged move wrote", before.Revision, after.Revision)
	}
	bad := func(Disposition, Disposition, Info) MetadataPatch { return MetadataPatch{"held_until": ""} }
	if _, _, err := front.ApplyDisposition(d, ActorOperator, DispEvent{Kind: EvKill}, bad, dispT0); err == nil {
		t.Fatal("a lifecycle that writes a class key was written")
	}
	if err := store.SetMetadataBatch(id, map[string]string{"generation": "2"}); err != nil {
		t.Fatal(err)
	}
	if res, _, err := front.ApplyDisposition(d, ActorOperator, DispEvent{Kind: EvKill}, nil, dispT0); err != nil || res != CommitMoved {
		t.Fatalf("a kill of a moved incarnation = %v, %v; want moved", res, err)
	}
	if err := store.Close(id); err != nil {
		t.Fatal(err)
	}
	if res, _, err := front.ApplyDisposition(freshDecision(t, front, id), ActorOperator, DispEvent{Kind: EvKill}, nil, dispT0); err != nil || res != CommitClosed {
		t.Fatalf("a kill of a closed row = %v, %v; want closed", res, err)
	}
	if _, _, err := front.ApplyDisposition(Decided{}, ActorOperator, DispEvent{Kind: EvKill}, nil, dispT0); err == nil {
		t.Fatal("a zero decision was applied")
	}
}

// TestApplyDispositionContended: a CAS lost on every attempt writes nothing
// and reports contended.
func TestApplyDispositionContended(t *testing.T) {
	stamped := stampedMemStore(t, gate.Auto, beads.NewMemStore())
	b, err := stamped.Create(beads.Bead{Title: "w", Type: BeadType, Labels: []string{LabelSession}, Metadata: map[string]string{"state": string(StateActive)}})
	if err != nil {
		t.Fatal(err)
	}
	loser := &wakeCASLoser{rowWriteRecorder: &rowWriteRecorder{Store: stamped}, backing: stamped}
	front := NewStore(beads.SessionStore{Store: loser})
	res, _, err := front.ApplyDisposition(freshDecision(t, front, b.ID), ActorOperator, DispEvent{Kind: EvSuspend}, nil, dispT0)
	if err != nil || res != CommitContended {
		t.Fatalf("ApplyDisposition under a CAS that always loses = %v, %v; want contended", res, err)
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
	front := NewStore(beads.SessionStore{Store: racer})
	res, to, err := front.ApplyDisposition(freshDecision(t, front, b.ID), ActorOperator, DispEvent{Kind: EvSuspend}, nil, dispT0)
	if err != nil || res != CommitLanded || !to.Wait {
		t.Fatalf("ApplyDisposition over a racing wait = %v, %+v, %v; want landed, deciding again over the wait", res, to, err)
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

// TestOperatorSuspendInForceWritesNothing: a suspend over one still in force
// (here drained to asleep) leaves the row as it is: no state, no wake
// request cleared.
func TestOperatorSuspendInForceWritesNothing(t *testing.T) {
	store, front, id := dispositionRow(t)
	if _, err := front.OperatorSuspend(id, dispT0); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadataBatch(id, map[string]string{"state": string(StateAsleep), "wake_request": "explicit"}); err != nil {
		t.Fatal(err)
	}
	before, _ := store.Get(id)
	if _, err := front.OperatorSuspend(id, dispT0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	after, _ := store.Get(id)
	if after.Revision != before.Revision || after.Metadata["state"] != string(StateAsleep) || after.Metadata["wake_request"] != "explicit" {
		t.Fatalf("a suspend in force rewrote the row: %v (revision %d -> %d)", after.Metadata, before.Revision, after.Revision)
	}
}
