package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
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

// Inside a hook, an ack's pay-and-confirm tail ends with the time the hook's
// deadline leaves, however slow the store: the ack returns before the deadline
// (so gc hook run keeps the output whose delivery the ack committed), and the
// debts it did not settle stay leased to it, for the controller after the
// lease.
func TestNudgeAckTailInsideAHookEndsBeforeTheDeadline(t *testing.T) {
	h := newNQCharHarness(t)
	enqueueAndClaimForTest(t, h, "n-1", "n-2")
	gated := &gatedShadowStore{Store: h.store(), entered: make(chan struct{}), gate: make(chan struct{})}
	prevOpen := openOwnedNudgeBeadStore
	openOwnedNudgeBeadStore = func(string) (beads.NudgesStore, beads.Store) {
		return beads.NudgesStore{Store: gated}, gated
	}
	t.Cleanup(func() { openOwnedNudgeBeadStore = prevOpen })
	abandoned := make(chan (<-chan struct{}), 1)
	prevAbandoned := nudgeHookTailAbandoned
	nudgeHookTailAbandoned = func(done <-chan struct{}) { abandoned <- done }
	t.Cleanup(func() { nudgeHookTailAbandoned = prevAbandoned })
	deadline := inHookForTest(t, 1500*time.Millisecond)

	acked := make(chan error, 1)
	go func() {
		acked <- ackQueuedNudgesWithOutcome(h.city, []string{"n-1", "n-2"}, "accepted_for_injection", "", "hook-transport-accepted")
	}()
	select {
	case err := <-acked:
		if err != nil {
			t.Fatalf("ack: %v", err)
		}
	case <-time.After(time.Until(deadline) + time.Second):
		close(gated.gate)
		<-acked
		t.Fatal("the ack's shadow tail ran past the hook deadline behind a blocked store write")
	}
	if returned := time.Now(); !returned.Before(deadline) {
		t.Fatalf("ack returned %s after the hook deadline", returned.Sub(deadline))
	}
	var done <-chan struct{}
	select {
	case done = <-abandoned:
	default:
		t.Fatal("the ack's blocked tail was not left behind at the hook deadline")
	}
	state := loadNudgeStateForTest(t, h.city)
	if len(state.InFlight) != 0 || len(state.Retired) != 2 {
		t.Fatalf("after the ack in-flight=%v retired=%v, want n-1 and n-2 retired", nqCharItems(state.InFlight), nqCharItems(state.Retired))
	}
	for _, item := range state.Retired {
		if item.Shadow == nil || item.Shadow.Paid || !item.Shadow.NotBefore.After(time.Now()) {
			t.Fatalf("debt of %s = %+v, want unpaid and leased", item.ID, item.Shadow)
		}
	}
	close(gated.gate)
	<-done
	// The tail that ran on past the deadline confirmed nothing: the debts stay
	// leased, and the controller pays them after the lease.
	late := loadNudgeStateForTest(t, h.city).Retired
	if len(late) != 2 {
		t.Fatalf("retired after the late tail = %v, want both debts left unconfirmed", nqCharItems(late))
	}
	for _, item := range late {
		if item.Shadow == nil || item.Shadow.Paid || !item.Shadow.NotBefore.After(time.Now()) {
			t.Fatalf("debt of %s after the late tail = %+v, want it still leased", item.ID, item.Shadow)
		}
	}
	t.Setenv(hookDeadlineEnv, "")
	openOwnedNudgeBeadStore = prevOpen
	if err := controllerPassForTest(h, time.Now().Add(nudgeShadowPayLease+time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := loadNudgeStateForTest(t, h.city).Retired; len(got) != 0 {
		t.Fatalf("retired after the controller paid = %v", nqCharItems(got))
	}
	requireShadowStateForTest(t, h, "n-1", "accepted_for_injection")
	requireShadowStateForTest(t, h, "n-2", "accepted_for_injection")
}

// closingCallerStore is the store a verb's caller holds and closes once the
// verb returns. It blocks shadow writes until gate opens, as gatedShadowStore
// does, and counts the calls that start after its close.
type closingCallerStore struct {
	gatedShadowStore
	closed atomic.Bool
	late   atomic.Int32
}

func (s *closingCallerStore) CloseStore() error { //nolint:unparam // the closer interface returns an error; this fake never fails
	s.closed.Store(true)
	return nil
}

func (s *closingCallerStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.countLate()
	return s.gatedShadowStore.SetMetadataBatch(id, kvs)
}

func (s *closingCallerStore) Close(id string) error {
	s.countLate()
	return s.gatedShadowStore.Close(id)
}

func (s *closingCallerStore) countLate() {
	if s.closed.Load() {
		s.late.Add(1)
	}
}

// Inside a hook, a verb given its caller's store pays its debts through a
// store the tail opens itself: a tail the deadline leaves behind goes on
// writing after the verb returns, and the caller closes its store then.
func TestNudgeHookTailPaysThroughAStoreItOpens(t *testing.T) {
	h := newNQCharHarness(t)
	enqueueAndClaimForTest(t, h, "n-1")
	gate := make(chan struct{})
	own := &gatedShadowStore{Store: h.store(), entered: make(chan struct{}), gate: gate}
	prevOpen := openOwnedNudgeBeadStore
	openOwnedNudgeBeadStore = func(string) (beads.NudgesStore, beads.Store) {
		return beads.NudgesStore{Store: own}, own
	}
	t.Cleanup(func() { openOwnedNudgeBeadStore = prevOpen })
	caller := &closingCallerStore{gatedShadowStore: gatedShadowStore{Store: h.store(), entered: make(chan struct{}), gate: gate}}
	abandoned := make(chan (<-chan struct{}), 1)
	prevAbandoned := nudgeHookTailAbandoned
	nudgeHookTailAbandoned = func(done <-chan struct{}) { abandoned <- done }
	t.Cleanup(func() { nudgeHookTailAbandoned = prevAbandoned })
	inHookForTest(t, 1500*time.Millisecond)

	dead, err := recordQueuedNudgeFailureDetailed(h.city, beads.NudgesStore{Store: caller}, []string{"n-1"}, errNudgeManualDrop, time.Now())
	if err != nil || len(dead) != 1 {
		close(gate)
		t.Fatalf("drop inside a hook = %v, %v", nqCharItems(dead), err)
	}
	var done <-chan struct{}
	select {
	case done = <-abandoned:
	default:
		close(gate)
		t.Fatal("the drop's blocked shadow tail was not left behind at the hook deadline")
	}
	if err := closeBeadStoreHandle(caller); err != nil {
		t.Fatal(err)
	}
	close(gate)
	<-done
	if late := caller.late.Load(); late != 0 {
		t.Fatalf("the tail made %d store calls through the caller's store after the caller closed it", late)
	}
	requireShadowStateForTest(t, h, "n-1", "failed")
}

// takeAckDebtsForTest acks ids with the pay phase dropped, as a crash after
// the commit would, and returns the debts the ack took.
func takeAckDebtsForTest(t *testing.T, h *nqCharHarness, ids ...string) []queuedNudge {
	t.Helper()
	taken := takeDebtsForTest(t, func() error {
		return ackQueuedNudgesWithOutcome(h.city, ids, "injected", "", "provider-nudge-return")
	})
	if len(taken) != len(ids) {
		t.Fatalf("the ack took %d debts, want %d", len(taken), len(ids))
	}
	return taken
}

// takeDebtsForTest runs a verb with its pay phase dropped and returns the
// debts its commit took: they stay leased to it, as to a payer whose store
// write is still in flight.
func takeDebtsForTest(t *testing.T, verb func() error) []queuedNudge {
	t.Helper()
	var taken []queuedNudge
	prev := settleNudgeShadowDebts
	settleNudgeShadowDebts = func(_ string, _ beads.NudgesStore, debts []queuedNudge, _ time.Duration, _ time.Time) {
		taken = append(taken, debts...)
	}
	err := verb()
	settleNudgeShadowDebts = prev
	if err != nil {
		t.Fatal(err)
	}
	return taken
}

// A payer whose budget runs out before it reaches a debt releases that debt
// unpaid: the next payer may take it at once, and its shadow is not marked.
func TestNudgeDebtsAPayerDidNotReachAreReleasedUnpaid(t *testing.T) {
	h := newNQCharHarness(t)
	enqueueAndClaimForTest(t, h, "n-1", "n-2")
	taken := takeAckDebtsForTest(t, h, "n-1", "n-2")
	h.rec.Reset()

	now := time.Now()
	payAndConfirmNudgeShadowDebts(h.city, h.shadow(), taken, -time.Second, now)

	if ops := h.rec.Ops(); len(ops) != 0 {
		t.Fatalf("a spent budget made %d store calls", len(ops))
	}
	state := loadNudgeStateForTest(t, h.city)
	if len(state.Retired) != 2 {
		t.Fatalf("retired = %v, want both debts kept", nqCharItems(state.Retired))
	}
	for _, item := range state.Retired {
		debt := item.Shadow
		if debt == nil || debt.Paid || debt.Attempts != 0 || !debt.NotBefore.IsZero() {
			t.Fatalf("unreached debt of %s = %+v, want unpaid, unattempted and released", item.ID, debt)
		}
	}
	requireShadowStateForTest(t, h, "n-1", "queued")
}

// A payment confirms only the item it paid: an id acked again under a new
// creation time is a different item, and its debt stays unpaid, even under a
// lease that ends when the paid one did.
func TestNudgeDebtPaymentMatchesTheItemNotTheID(t *testing.T) {
	now := time.Now().UTC()
	lease := nudgequeue.ShadowDebt{State: "injected", NotBefore: now.Add(nudgeShadowPayLease), Leased: true}
	paidDebt, rebornDebt := lease, lease
	old := queuedNudge{ID: "n-1", BeadID: "gc-1", CreatedAt: now.Add(-time.Hour), Shadow: &paidDebt}
	reborn := old
	reborn.CreatedAt = now
	reborn.Shadow = &rebornDebt
	state := &nudgeQueueState{Retired: []queuedNudge{reborn}}
	applyNudgeDebtPayments(state, []nudgeDebtPayment{{item: old, reached: true}}, now)
	if len(state.Retired) != 1 || state.Retired[0].Shadow.Paid {
		t.Fatalf("retired after paying the older n-1 = %+v, want the newer n-1 unpaid", state.Retired)
	}
}

// Enqueue drops only the retired debts on the shadow the new item reuses: a
// retired item of the same id whose debt names another shadow keeps it.
func TestDropRetiredDebtsOnShadowMatchesTheShadow(t *testing.T) {
	state := &nudgeQueueState{Retired: []queuedNudge{
		{ID: "n-1", BeadID: "gc-old", Shadow: &nudgequeue.ShadowDebt{State: "injected"}},
		{ID: "n-1", BeadID: "gc-reused", Shadow: &nudgequeue.ShadowDebt{State: "injected"}},
	}}
	if err := dropRetiredDebtsOnShadow(state, queuedNudge{ID: "n-1", BeadID: "gc-reused"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(state.Retired) != 1 || state.Retired[0].BeadID != "gc-old" {
		t.Fatalf("retired = %+v, want only the debt on gc-old kept", state.Retired)
	}
}

// The controller does not pay a paid debt again: a pass over paid dead letters
// makes no store call.
func TestNudgeControllerPassSkipsPaidDebts(t *testing.T) {
	h := newNQCharHarness(t)
	enqueueAndClaimForTest(t, h, "n-1")
	if _, err := recordQueuedNudgeFailureDetailed(h.city, h.shadow(), []string{"n-1"}, errNudgeManualDrop, time.Now()); err != nil {
		t.Fatal(err)
	}
	state := loadNudgeStateForTest(t, h.city)
	if len(state.Dead) != 1 || state.Dead[0].Shadow == nil || !state.Dead[0].Shadow.Paid {
		t.Fatalf("dead after the drop = %+v, want a paid debt", state.Dead)
	}
	h.rec.Reset()
	opens := h.opens
	if err := controllerPassForTest(h, time.Now()); err != nil {
		t.Fatal(err)
	}
	if ops := h.rec.Ops(); len(ops) != 0 || h.opens != opens {
		t.Fatalf("a controller pass over a paid debt made %d store calls and %d opens", len(ops), h.opens-opens)
	}
	if dead := loadNudgeStateForTest(t, h.city).Dead; len(dead) != 1 {
		t.Fatalf("dead after the pass = %v, want the paid dead letter kept inside retention", nqCharItems(dead))
	}
}

// A dead letter an older binary wrote gets a Verify debt; the controller pays
// it by reading the shadow first, and a shadow already terminal keeps its own
// record.
func TestNudgeVerifyDebtLeavesATerminalShadowAlone(t *testing.T) {
	h := newNQCharHarness(t)
	if err := enqueueQueuedNudgeWithStore(h.city, h.shadow(), h.item("n-1", "alpha", -time.Minute, queuedNudgeOptions{})); err != nil {
		t.Fatal(err)
	}
	pending := loadNudgeStateForTest(t, h.city).Pending
	if len(pending) != 1 || pending[0].BeadID == "" {
		t.Fatalf("pending = %+v, want n-1 with a shadow", pending)
	}
	item := pending[0]
	if err := nudgeFrontDoor(h.shadow()).Terminalize(item, "injected", "", "provider-nudge-return", time.Now()); err != nil {
		t.Fatal(err)
	}
	// What an older binary left: the item dead-lettered, its shadow
	// terminalized its own way, and no debt.
	if err := withNudgeQueueState(h.city, "test", func(state *nudgeQueueState) error {
		legacy := item
		legacy.DeadAt = time.Now().UTC()
		legacy.LastError = "boom"
		legacy.Shadow = nil
		state.Pending = nil
		state.Dead = []queuedNudge{legacy}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := controllerPassForTest(h, time.Now()); err != nil {
		t.Fatal(err)
	}
	requireShadowStateForTest(t, h, "n-1", "injected")
	dead := loadNudgeStateForTest(t, h.city).Dead
	if len(dead) != 1 || dead[0].Shadow == nil || !dead[0].Shadow.Verify || !dead[0].Shadow.Paid {
		t.Fatalf("dead after the controller pass = %+v, want a paid Verify debt", dead)
	}
}

// T5a's watchdog arm: in a legacy-dispatcher city the controller's nudge-mail
// watchdog runs the queue's maintenance, which pays a debt a crashed payer
// left.
func TestNudgeMailSweepWatchdogPaysLeftoverDebts(t *testing.T) {
	h := newNQCharHarness(t)
	enqueueAndClaimForTest(t, h, "n-1")
	takeAckDebtsForTest(t, h, "n-1")
	nudgeMaintenanceDebounceMu.Lock()
	delete(nudgeMaintenanceLastSweepAt, h.city)
	nudgeMaintenanceDebounceMu.Unlock()

	cr := &CityRuntime{
		cityName:            "test-city",
		cityPath:            h.city,
		cfg:                 &config.City{Workspace: config.Workspace{Name: "test-city"}},
		standaloneCityStore: h.store(),
		stdout:              io.Discard,
		stderr:              io.Discard,
		logPrefix:           "gc test",
	}
	cr.runNudgeMailSweepWatchdog(cr.cfg, time.Now().Add(nudgeShadowPayLease+time.Second))

	if got := loadNudgeStateForTest(t, h.city).Retired; len(got) != 0 {
		t.Fatalf("retired after the watchdog = %v, want the debt paid", nqCharItems(got))
	}
	requireShadowStateForTest(t, h, "n-1", "injected")
}

// T5c: a wait reminder acked while its shadow write fails is not queued again
// (its shadow still reads queued), and once the controller pays the debt the
// shadow reads delivered, which resolves the wait's attempt.
func TestNudgeWaitReminderAckedWhileItsShadowWriteFails(t *testing.T) {
	h := newNQCharHarness(t)
	item := newQueuedNudgeWithOptions("alpha", "Wait satisfied (w-1): go", "wait", h.at(-time.Minute), queuedNudgeOptions{
		ID:        "wait-w-1-1",
		Reference: &nudgeReference{Kind: "bead", ID: "w-1"},
	})
	if err := enqueueQueuedNudgeWithStore(h.city, h.shadow(), item); err != nil {
		t.Fatal(err)
	}
	if claimed, err := claimDueQueuedNudgesForTarget(h.city, h.target("", ""), time.Now()); err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %v, %v", nqCharItems(claimed), err)
	}
	h.failWrites = true
	if err := ackQueuedNudgesWithOutcome(h.city, []string{item.ID}, "injected", "", "provider-nudge-return"); err != nil {
		t.Fatalf("ack with a failing shadow store: %v", err)
	}
	h.failWrites = false

	queued, err := waitNudgeAttemptQueuedOrResolved(nudgeShadows(h.shadow()), item.ID)
	if err != nil || !queued {
		t.Fatalf("attempt queued-or-resolved after the ack = %v, %v; want true, so the reminder is not queued again", queued, err)
	}
	if err := controllerPassForTest(h, time.Now().Add(nudgeShadowRetryBase+time.Second)); err != nil {
		t.Fatal(err)
	}
	shadow, ok, err := nudgeShadows(h.shadow()).FindIncludingTerminal(item.ID)
	if err != nil || !ok {
		t.Fatalf("shadow after the controller paid: ok=%v err=%v", ok, err)
	}
	if got := nudgequeue.ClassifyTerminalState(shadow.State); got != nudgequeue.OutcomeDelivered {
		t.Fatalf("shadow state %q classifies %v, want delivered", shadow.State, got)
	}
}

// The shim's import reuses an open shadow the way Enqueue does, so it drops a
// retired debt on that shadow: paying it would close the shadow of an item
// the queue serves. Its store calls run outside the queue lock (the harness's
// lock probe).
func TestNudgeImportMigratedDropsRetiredDebtsOnTheReusedShadow(t *testing.T) {
	h := newNQCharHarness(t)
	enqueueAndClaimForTest(t, h, "n-1")
	h.failWrites = true
	if err := ackQueuedNudgesWithOutcome(h.city, []string{"n-1"}, "injected", "", "provider-nudge-return"); err != nil {
		t.Fatal(err)
	}
	h.failWrites = false
	if got := loadNudgeStateForTest(t, h.city).Retired; len(got) != 1 {
		t.Fatalf("retired after the failing ack = %v", nqCharItems(got))
	}

	moved := h.item("n-1", "alpha", 0, queuedNudgeOptions{})
	q := &fileNudgeQueue{cityPath: h.city, store: h.shadow(), clk: clock.Real{}}
	if _, err := q.importMigrated([]nudgequeue.Item{moved}); err != nil {
		t.Fatalf("importMigrated: %v", err)
	}
	state := loadNudgeStateForTest(t, h.city)
	if len(state.Pending) != 1 || state.Pending[0].ID != "n-1" {
		t.Fatalf("pending after the import = %v", nqCharItems(state.Pending))
	}
	if len(state.Retired) != 0 {
		t.Fatalf("retired after the import = %v, want the debt on the reused shadow dropped", nqCharItems(state.Retired))
	}
	if err := controllerPassForTest(h, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	shadow, open, err := nudgeShadows(h.shadow()).Find("n-1")
	if err != nil || !open {
		t.Fatalf("shadow of the imported n-1 = %+v open=%v err=%v, want it open", shadow, open, err)
	}
}

// An acked id queued again while the ack's payer holds its retired debt does
// not reuse the shadow that payer is writing: dropping the debt would not stop
// the write of the copy the payer took, which would close the shadow of an
// item the queue serves. The enqueue or import is refused as busy and queues
// nothing. Once the payer has written and confirmed, the old shadow keeps the
// ack's outcome and a retry queues the item on a new shadow.
func TestNudgeReuseOfAShadowAPayerHoldsIsRefused(t *testing.T) {
	for _, c := range []struct {
		verb  string
		queue func(h *nqCharHarness) error
	}{
		{"enqueue", func(h *nqCharHarness) error {
			return enqueueQueuedNudgeWithStore(h.city, h.shadow(), h.item("n-1", "alpha", 0, queuedNudgeOptions{}))
		}},
		{"import", func(h *nqCharHarness) error {
			q := &fileNudgeQueue{cityPath: h.city, store: h.shadow(), clk: clock.Real{}}
			_, err := q.importMigrated([]nudgequeue.Item{h.item("n-1", "alpha", 0, queuedNudgeOptions{})})
			return err
		}},
	} {
		t.Run(c.verb, func(t *testing.T) {
			h := newNQCharHarness(t)
			enqueueAndClaimForTest(t, h, "n-1")
			taken := takeAckDebtsForTest(t, h, "n-1")
			held := taken[0].BeadID

			if err := c.queue(h); !errors.Is(err, nudgequeue.ErrQueueBusy) {
				t.Fatalf("%s while the payer holds the debt: err = %v, want ErrQueueBusy", c.verb, err)
			}
			state := loadNudgeStateForTest(t, h.city)
			if len(state.Pending) != 0 || len(state.Retired) != 1 {
				t.Fatalf("after the refused %s pending=%v retired=%v, want nothing queued and the debt kept", c.verb, nqCharItems(state.Pending), nqCharItems(state.Retired))
			}

			payAndConfirmNudgeShadowDebts(h.city, h.shadow(), taken, nudgeEnqueueMaintenanceBudget, time.Now())
			requireShadowStateForTest(t, h, "n-1", "injected")
			if err := c.queue(h); err != nil {
				t.Fatalf("%s after the payer wrote: %v", c.verb, err)
			}
			state = loadNudgeStateForTest(t, h.city)
			if len(state.Pending) != 1 || state.Pending[0].BeadID == "" || state.Pending[0].BeadID == held || len(state.Retired) != 0 {
				t.Fatalf("after the retried %s pending=%+v retired=%v, want n-1 on a new shadow", c.verb, state.Pending, nqCharItems(state.Retired))
			}
			shadow, open, err := nudgeShadows(h.shadow()).Find("n-1")
			if err != nil || !open || shadow.BeadID != state.Pending[0].BeadID {
				t.Fatalf("open shadow of the queued n-1 = %+v open=%v err=%v, want %s", shadow, open, err, state.Pending[0].BeadID)
			}
		})
	}
}

// A payer that confirms after its lease ran out, once the controller has taken
// the debt under a lease of its own, records nothing, whatever it reports:
// ending the controller's lease would let an enqueue reuse the shadow while
// the controller's write to it is in flight, and a late success would settle
// the debt out from under the controller's confirm. The controller's confirm
// settles it.
func TestNudgeLateConfirmLeavesASuccessorsLeaseAlone(t *testing.T) {
	for _, c := range []struct {
		name    string
		payment func(item queuedNudge) nudgeDebtPayment
	}{
		{"not_reached", func(item queuedNudge) nudgeDebtPayment { return nudgeDebtPayment{item: item} }},
		{"failed", func(item queuedNudge) nudgeDebtPayment {
			return nudgeDebtPayment{item: item, reached: true, err: errors.New("store unavailable")}
		}},
		{"paid", func(item queuedNudge) nudgeDebtPayment { return nudgeDebtPayment{item: item, reached: true} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newNQCharHarness(t)
			enqueueAndClaimForTest(t, h, "n-1", "n-2")
			first := takeAckDebtsForTest(t, h, "n-1")
			first = append(first, takeDebtsForTest(t, func() error {
				_, err := recordQueuedNudgeFailureDetailed(h.city, h.shadow(), []string{"n-2"}, errNudgeManualDrop, time.Now())
				return err
			})...)
			successor := takeDebtsForTest(t, func() error {
				return controllerPassForTest(h, time.Now().Add(nudgeShadowPayLease+time.Second))
			})
			if len(first) != 2 || len(successor) != 2 {
				t.Fatalf("first payers took %v and the controller %v, want n-1 and n-2 each", nqCharItems(first), nqCharItems(successor))
			}
			held := loadNudgeStateForTest(t, h.city)

			late := make([]nudgeDebtPayment, 0, len(first))
			for _, item := range first {
				late = append(late, c.payment(item))
			}
			if err := confirmNudgeShadowDebts(h.city, late, time.Now()); err != nil {
				t.Fatal(err)
			}
			state := loadNudgeStateForTest(t, h.city)
			if !reflect.DeepEqual(state.Retired, held.Retired) || !reflect.DeepEqual(state.Dead, held.Dead) {
				t.Fatalf("debts after the late confirm: retired %s dead %s; want the controller's leases unchanged: retired %s dead %s",
					shadowDebtsForTest(state.Retired), shadowDebtsForTest(state.Dead), shadowDebtsForTest(held.Retired), shadowDebtsForTest(held.Dead))
			}
			err := enqueueQueuedNudgeWithStore(h.city, h.shadow(), h.item("n-1", "alpha", 0, queuedNudgeOptions{}))
			if !errors.Is(err, errNudgeShadowRetiring) {
				t.Fatalf("enqueue reusing the shadow the controller holds: err = %v, want errNudgeShadowRetiring", err)
			}

			payAndConfirmNudgeShadowDebts(h.city, h.shadow(), successor, nudgeEnqueueMaintenanceBudget, time.Now())
			state = loadNudgeStateForTest(t, h.city)
			if len(state.Retired) != 0 || len(state.Dead) != 1 || state.Dead[0].Shadow == nil || !state.Dead[0].Shadow.Paid {
				t.Fatalf("debts after the controller confirmed: retired %s dead %s; want both settled", shadowDebtsForTest(state.Retired), shadowDebtsForTest(state.Dead))
			}
			requireShadowStateForTest(t, h, "n-1", "injected")
			requireShadowStateForTest(t, h, "n-2", "failed")
		})
	}
}

// shadowDebtsForTest renders each item's id and shadow debt.
func shadowDebtsForTest(items []queuedNudge) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		if item.Shadow == nil {
			parts = append(parts, item.ID+":<none>")
			continue
		}
		parts = append(parts, fmt.Sprintf("%s:%+v", item.ID, *item.Shadow))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// The controller's pass pays within nudgeControllerPayBudget, seconds, not the
// whole-queue sweep's minutes: it runs on the dispatch loop or the orders lane.
func TestNudgeControllerPassPaysWithinASecondsBudget(t *testing.T) {
	h := newNQCharHarness(t)
	enqueueAndClaimForTest(t, h, "n-1")
	takeAckDebtsForTest(t, h, "n-1")
	var budgets []time.Duration
	prev := settleNudgeShadowDebts
	settleNudgeShadowDebts = func(_ string, _ beads.NudgesStore, debts []queuedNudge, budget time.Duration, _ time.Time) {
		if len(debts) > 0 {
			budgets = append(budgets, budget)
		}
	}
	err := controllerPassForTest(h, time.Now().Add(nudgeShadowPayLease+time.Second))
	settleNudgeShadowDebts = prev
	if err != nil {
		t.Fatal(err)
	}
	if len(budgets) != 1 || budgets[0] != nudgeControllerPayBudget || budgets[0] > 10*time.Second {
		t.Fatalf("controller pay budgets = %v, want one of nudgeControllerPayBudget (%s)", budgets, nudgeControllerPayBudget)
	}
}
