package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// The shadow-debt tests (DES-NQL §5.2, ga-fsp5w9). They replace package seams
// through newNQCharHarness, so they stay serial.

// payDueDebtsInMemoryForTest is the controller's leftover payment over an
// in-memory state: it takes the payable debts, pays them with store and
// records the payments.
func payDueDebtsInMemoryForTest(t *testing.T, state *nudgeQueueState, store beads.NudgesStore, now time.Time) {
	t.Helper()
	ledger := newNudgeShadowLedger(now)
	takeDueNudgeShadowDebts(state, ledger, nudgeShadowPayBatch)
	payments := payNudgeShadowDebts(nudgeFrontDoor(store), ledger.taken, noMaintenanceDeadline())
	for _, payment := range payments {
		if payment.err != nil {
			t.Fatalf("paying the debt of %s: %v", payment.item.ID, payment.err)
		}
	}
	applyNudgeDebtPayments(state, payments, now)
}

func loadNudgeStateForTest(t *testing.T, city string) nudgeQueueState {
	t.Helper()
	state, err := nudgequeue.LoadState(city)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	return state
}

func requireShadowStateForTest(t *testing.T, h *nqCharHarness, id, want string) {
	t.Helper()
	shadow, ok, err := nudgeFrontDoor(h.shadow()).FindIncludingTerminal(id)
	if err != nil || !ok {
		t.Fatalf("shadow of %s: ok=%v err=%v", id, ok, err)
	}
	if shadow.State != want || (nudgequeue.IsTerminalState(want) && shadow.Open) {
		t.Fatalf("shadow of %s = state %q open %v, want %q", id, shadow.State, shadow.Open, want)
	}
}

// enqueueAndClaimForTest enqueues ids for agent alpha and claims them all.
func enqueueAndClaimForTest(t *testing.T, h *nqCharHarness, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := enqueueQueuedNudgeWithStore(h.city, h.shadow(), h.item(id, "alpha", -time.Minute, queuedNudgeOptions{})); err != nil {
			t.Fatalf("enqueue %s: %v", id, err)
		}
	}
	claimed, err := claimDueQueuedNudgesForTarget(h.city, h.target("", ""), time.Now())
	if err != nil || len(claimed) != len(ids) {
		t.Fatalf("claim = %v, %v; want %v", nqCharItems(claimed), err, ids)
	}
}

// T5a: an ack whose shadow writes fail still commits: the items leave the
// serving buckets for retired, carrying their debts, no claim returns them,
// and a later controller pass with a healthy store pays the debts.
func TestNudgeAckCommitsWhenItsShadowWriteFails(t *testing.T) {
	h := newNQCharHarness(t)
	enqueueAndClaimForTest(t, h, "n-1", "n-2")

	h.failWrites = true
	if err := ackQueuedNudgesWithOutcome(h.city, []string{"n-1", "n-2"}, "injected", "", "provider-nudge-return"); err != nil {
		t.Fatalf("ack with a failing shadow store: %v", err)
	}
	h.failWrites = false

	state := loadNudgeStateForTest(t, h.city)
	if len(state.Pending)+len(state.InFlight)+len(state.Dead) != 0 {
		t.Fatalf("serving buckets after the ack = %+v", state)
	}
	if len(state.Retired) != 2 {
		t.Fatalf("retired = %v, want n-1 and n-2", nqCharItems(state.Retired))
	}
	for _, item := range state.Retired {
		debt := item.Shadow
		if debt == nil || debt.Paid || debt.Attempts != 1 || debt.LastError == "" || debt.State != "injected" {
			t.Fatalf("debt of %s = %+v, want one failed attempt owing injected", item.ID, debt)
		}
	}
	claimed, err := claimDueQueuedNudgesForTarget(h.city, h.target("", ""), time.Now().Add(time.Hour))
	if err != nil || len(claimed) != 0 {
		t.Fatalf("a claim after the ack = %v, %v; want nothing", nqCharItems(claimed), err)
	}
	requireShadowStateForTest(t, h, "n-1", "queued")

	// Inside the backoff the controller takes nothing.
	if err := controllerPassForTest(h, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := loadNudgeStateForTest(t, h.city).Retired; len(got) != 2 {
		t.Fatalf("retired inside the backoff = %v", nqCharItems(got))
	}
	// After it, the controller pays and the retired bucket empties.
	if err := controllerPassForTest(h, time.Now().Add(nudgeShadowRetryBase+time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := loadNudgeStateForTest(t, h.city).Retired; len(got) != 0 {
		t.Fatalf("retired after the controller paid = %v", nqCharItems(got))
	}
	requireShadowStateForTest(t, h, "n-1", "injected")
	requireShadowStateForTest(t, h, "n-2", "injected")
}

// The supervisor's maintenance sweep pays leftover debts too.
func TestNudgeMaintenanceSweepPaysLeftoverDebts(t *testing.T) {
	h := newNQCharHarness(t)
	enqueueAndClaimForTest(t, h, "n-1")
	h.failWrites = true
	if err := ackQueuedNudgesWithOutcome(h.city, []string{"n-1"}, "injected", "", "provider-nudge-return"); err != nil {
		t.Fatal(err)
	}
	h.failWrites = false
	if err := runNudgeQueueMaintenanceSweep(h.city, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := loadNudgeStateForTest(t, h.city).Retired; len(got) != 0 {
		t.Fatalf("retired after the sweep = %v", nqCharItems(got))
	}
	requireShadowStateForTest(t, h, "n-1", "injected")
}

// T5b: a payer that crashes after its commit leaves taken debts. Nobody else
// pays them inside the payer's lease; after it, the controller does.
func TestNudgeShadowDebtsOutliveACrashedPayer(t *testing.T) {
	h := newNQCharHarness(t)
	enqueueAndClaimForTest(t, h, "n-1", "n-2")
	prev := settleNudgeShadowDebts
	settleNudgeShadowDebts = func(string, beads.NudgesStore, []queuedNudge, time.Duration, time.Time) {}
	err := ackQueuedNudgesWithOutcome(h.city, []string{"n-1", "n-2"}, "injected", "", "provider-nudge-return")
	settleNudgeShadowDebts = prev
	if err != nil {
		t.Fatal(err)
	}
	state := loadNudgeStateForTest(t, h.city)
	if len(state.Retired) != 2 {
		t.Fatalf("retired after a crashed pay = %v", nqCharItems(state.Retired))
	}
	requireShadowStateForTest(t, h, "n-1", "queued")

	if err := controllerPassForTest(h, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := loadNudgeStateForTest(t, h.city).Retired; len(got) != 2 {
		t.Fatalf("a debt was paid inside its payer's lease: retired = %v", nqCharItems(got))
	}
	if err := controllerPassForTest(h, time.Now().Add(nudgeShadowPayLease+time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := loadNudgeStateForTest(t, h.city).Retired; len(got) != 0 {
		t.Fatalf("retired after the lease = %v", nqCharItems(got))
	}
	requireShadowStateForTest(t, h, "n-1", "injected")
	requireShadowStateForTest(t, h, "n-2", "injected")
}

// A dead letter's debt survives a failing store the same way, and the dead
// letter is not pruned while its debt is unpaid.
func TestNudgeDeadLetterKeepsAnUnpaidDebt(t *testing.T) {
	h := newNQCharHarness(t)
	enqueueAndClaimForTest(t, h, "n-1")
	h.failWrites = true
	dead, err := recordQueuedNudgeFailureDetailed(h.city, h.shadow(), []string{"n-1"}, errNudgeManualDrop, time.Now())
	h.failWrites = false
	if err != nil || len(dead) != 1 {
		t.Fatalf("drop = %v, %v", nqCharItems(dead), err)
	}
	// Past retention, the unpaid dead letter stays.
	later := time.Now().Add(defaultQueuedNudgeDeadRetention + time.Hour)
	if _, _, got, err := listQueuedNudges(h.city, "alpha", later); err != nil || len(got) != 1 {
		t.Fatalf("dead after retention with an unpaid debt = %v, %v", nqCharItems(got), err)
	}
	// Paid, it is marked so, and the next pass past retention prunes it.
	if err := controllerPassForTest(h, later); err != nil {
		t.Fatal(err)
	}
	state := loadNudgeStateForTest(t, h.city)
	if len(state.Dead) != 1 || state.Dead[0].Shadow == nil || !state.Dead[0].Shadow.Paid {
		t.Fatalf("dead after the payment = %+v", state.Dead)
	}
	requireShadowStateForTest(t, h, "n-1", "failed")
	if _, _, got, err := listQueuedNudges(h.city, "alpha", later); err != nil || len(got) != 0 {
		t.Fatalf("dead after its debt was paid = %v, %v", nqCharItems(got), err)
	}
}

// T5d: the stale-shadow sweep treats a retired item and a dead letter whose
// debt is unpaid as live, so it never closes their shadows gc-swept.
func TestLiveNudgeIDSetCoversUnpaidDebts(t *testing.T) {
	paid := &nudgequeue.ShadowDebt{State: "failed", Paid: true}
	unpaid := &nudgequeue.ShadowDebt{State: "failed"}
	state := &nudgequeue.State{
		Pending:  []nudgequeue.Item{{ID: "pending"}},
		InFlight: []nudgequeue.Item{{ID: "in-flight"}},
		Dead:     []nudgequeue.Item{{ID: "dead-paid", Shadow: paid}, {ID: "dead-unpaid", Shadow: unpaid}, {ID: "dead-none"}},
		Retired:  []nudgequeue.Item{{ID: "retired", Shadow: &nudgequeue.ShadowDebt{State: "injected"}}},
	}
	live := liveNudgeIDSet(state)
	for id, want := range map[string]bool{
		"pending": true, "in-flight": true, "dead-unpaid": true, "retired": true,
		"dead-paid": false, "dead-none": false,
	} {
		if live[id] != want {
			t.Errorf("live[%s] = %v, want %v", id, live[id], want)
		}
	}
}

// A retired item whose debt has gone unpaid for a day is dropped with a
// warning, so the file stays bounded.
func TestRetiredDebtUnpaidForADayIsDropped(t *testing.T) {
	h := newNQCharHarness(t)
	enqueueAndClaimForTest(t, h, "n-1")
	h.failWrites = true
	if err := ackQueuedNudgesWithOutcome(h.city, []string{"n-1"}, "injected", "", "provider-nudge-return"); err != nil {
		t.Fatal(err)
	}
	h.failWrites = false
	h.warnings.Reset()
	if err := runNudgeQueueMaintenanceSweepUndebounced(h.city, time.Now().Add(defaultQueuedNudgeTTL+time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := loadNudgeStateForTest(t, h.city).Retired; len(got) != 0 {
		t.Fatalf("retired a day on = %v", nqCharItems(got))
	}
	if h.warnings.Len() == 0 {
		t.Fatal("dropping an unpaid retired debt printed no warning")
	}
}

// controllerPassForTest runs the controller's maintenance (the queue's
// Maintain) at now, with the harness's shadow store.
func controllerPassForTest(h *nqCharHarness, now time.Time) error {
	nudgeMaintenanceDebounceMu.Lock()
	delete(nudgeMaintenanceLastSweepAt, h.city)
	nudgeMaintenanceDebounceMu.Unlock()
	_, err := openNudgeQueue(h.city, h.shadow()).Queue.Maintain(context.Background(), now)
	return err
}

func runNudgeQueueMaintenanceSweepUndebounced(city string, now time.Time) error {
	nudgeMaintenanceDebounceMu.Lock()
	delete(nudgeMaintenanceLastSweepAt, city)
	nudgeMaintenanceDebounceMu.Unlock()
	return runNudgeQueueMaintenanceSweep(city, now)
}

// T6: paid dead letters cost no store call on any maintaining verb, and they
// age out past retention.
func TestPaidDeadLettersCostNoStoreCalls(t *testing.T) {
	h := newNQCharHarness(t)
	now := time.Now().UTC()
	var dead []queuedNudge
	for i := 0; i < 20; i++ {
		item := h.item(fmt.Sprintf("n-dead-%02d", i), "alpha", -time.Hour, queuedNudgeOptions{})
		item.BeadID = fmt.Sprintf("gc-dead-%02d", i)
		item.DeadAt = now.Add(-30 * time.Minute)
		item.LastError = "boom"
		item.Shadow = &nudgequeue.ShadowDebt{State: "failed", Reason: "boom", At: item.DeadAt, Paid: true}
		dead = append(dead, item)
	}
	if err := withNudgeQueueState(h.city, "test", func(state *nudgeQueueState) error {
		state.Dead = dead
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.rec.Reset()
	opens := h.opens
	alpha := h.target("s-new", "2")
	for i := 0; i < 10; i++ {
		if _, err := claimDueQueuedNudgesForTarget(h.city, alpha, now); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := listQueuedNudgesForTarget(h.city, alpha, now); err != nil {
			t.Fatal(err)
		}
	}
	if ops := h.rec.Ops(); len(ops) != 0 || h.opens != opens {
		t.Fatalf("maintaining verbs over paid dead letters made %d store calls and %d opens", len(ops), h.opens-opens)
	}
	if _, _, got, err := listQueuedNudgesForTarget(h.city, alpha, now.Add(time.Hour)); err != nil || len(got) != 0 {
		t.Fatalf("paid dead letters past retention = %v, %v", nqCharItems(got), err)
	}
}

// gatedShadowStore blocks SetMetadataBatch until its gate opens.
type gatedShadowStore struct {
	beads.Store
	entered chan struct{}
	gate    chan struct{}
	once    sync.Once
}

func (s *gatedShadowStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.once.Do(func() { close(s.entered) })
	<-s.gate
	return s.Store.SetMetadataBatch(id, kvs)
}

// T2: an ack's shadow writes run after the queue lock is released, so a
// producer is not held behind a slow store.
func TestNudgeAckShadowWritesDoNotBlockProducers(t *testing.T) {
	h := newNQCharHarness(t)
	enqueueAndClaimForTest(t, h, "n-1", "n-2", "n-3")
	gated := &gatedShadowStore{Store: h.store(), entered: make(chan struct{}), gate: make(chan struct{})}
	prevOpen := openOwnedNudgeBeadStore
	openOwnedNudgeBeadStore = func(string) (beads.NudgesStore, beads.Store) {
		return beads.NudgesStore{Store: gated}, gated
	}
	t.Cleanup(func() { openOwnedNudgeBeadStore = prevOpen })

	acked := make(chan error, 1)
	go func() {
		acked <- ackQueuedNudgesWithOutcome(h.city, []string{"n-1", "n-2", "n-3"}, "injected", "", "provider-nudge-return")
	}()
	<-gated.entered
	start := time.Now()
	if err := enqueueQueuedNudgeWithStore(h.city, beads.NudgesStore{Store: beadstest.NewOpRecordingStore(beads.NewMemStore())}, h.item("n-4", "alpha", 0, queuedNudgeOptions{})); err != nil {
		t.Fatalf("enqueue during a blocked ack shadow write: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("enqueue waited %s behind an ack's shadow write", elapsed)
	}
	close(gated.gate)
	if err := <-acked; err != nil {
		t.Fatalf("ack: %v", err)
	}
	state := loadNudgeStateForTest(t, h.city)
	if len(state.Retired) != 0 {
		t.Fatalf("retired after the ack paid = %v", nqCharItems(state.Retired))
	}
	for _, id := range []string{"n-1", "n-2", "n-3"} {
		requireShadowStateForTest(t, h, id, "injected")
	}
}

// A deferred submit never had a shadow: its ack owes nothing, leaves nothing in
// retired and makes no store call.
func TestNudgeAckOfADeferredSubmitOwesNothing(t *testing.T) {
	h := newNQCharHarness(t)
	now := time.Now().UTC()
	item := nudgequeue.Item{ID: "nudge-sub", Agent: "alpha", Source: nudgequeue.SourceSubmit, Message: "hi", CreatedAt: now, DeliverAfter: now, ExpiresAt: now.Add(time.Hour)}
	if err := openNudgeQueue(h.city, beads.NudgesStore{}).Queue.EnqueueDeferred(item); err != nil {
		t.Fatal(err)
	}
	h.rec.Reset()
	opens := h.opens
	if err := ackQueuedNudgesWithOutcome(h.city, []string{"nudge-sub"}, "injected", "", "provider-nudge-return"); err != nil {
		t.Fatal(err)
	}
	state := loadNudgeStateForTest(t, h.city)
	if len(state.Pending)+len(state.Retired) != 0 {
		t.Fatalf("state after acking a submit = %+v", state)
	}
	if ops := h.rec.Ops(); len(ops) != 0 || h.opens != opens {
		t.Fatalf("acking a submit made %d store calls and %d opens", len(ops), h.opens-opens)
	}
}

// An acked id enqueued again is queued again: retired items are invisible to
// the enqueue dedupe (ForgetsRetiredIDs).
func TestRetiredIDIsForgottenByEnqueue(t *testing.T) {
	h := newNQCharHarness(t)
	enqueueAndClaimForTest(t, h, "n-1")
	h.failWrites = true
	if err := ackQueuedNudgesWithOutcome(h.city, []string{"n-1"}, "injected", "", "provider-nudge-return"); err != nil {
		t.Fatal(err)
	}
	h.failWrites = false
	if err := enqueueQueuedNudgeWithStore(h.city, h.shadow(), h.item("n-1", "alpha", 0, queuedNudgeOptions{})); err != nil {
		t.Fatal(err)
	}
	state := loadNudgeStateForTest(t, h.city)
	if len(state.Pending) != 1 || state.Pending[0].ID != "n-1" {
		t.Fatalf("pending after re-enqueueing a retired id = %v", nqCharItems(state.Pending))
	}
	// The new item reuses the still-open shadow, so the old item's debt is
	// dropped: paying it would close the shadow of an item the queue serves.
	if len(state.Retired) != 0 {
		t.Fatalf("retired after the re-enqueue = %v, want the old debt dropped", nqCharItems(state.Retired))
	}
	if err := controllerPassForTest(h, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	requireShadowStateForTest(t, h, "n-1", "queued")
}
