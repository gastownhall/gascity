package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

// A claimed batch is delivered under the key pinned on its items; items
// claimed without a pin carry no key.
func TestQueuedNudgeDeliveryKey(t *testing.T) {
	a := queuedNudge{ID: "nudge-a", DeliveryBatch: "nudge-batch-1"}
	b := queuedNudge{ID: "nudge-b", DeliveryBatch: "nudge-batch-1"}
	if got := queuedNudgeDeliveryKey([]queuedNudge{a, b}); got != "nudge-batch-1" {
		t.Fatalf("delivery key = %q, want the pinned batch nudge-batch-1", got)
	}
	if got := queuedNudgeDeliveryKey([]queuedNudge{{ID: "nudge-c"}}); got != "" {
		t.Fatalf("delivery key of an unpinned batch = %q, want empty", got)
	}
}

func TestFailedQueuedNudgeRotatesTheBatchOnlyOnRejection(t *testing.T) {
	now := time.Now()
	pinned := queuedNudge{ID: "n", DeliveryBatch: "nudge-batch-1", ExpiresAt: now.Add(time.Hour)}
	ambiguous, _ := failedQueuedNudge(pinned, errors.New("i/o timeout"), now)
	if ambiguous.DeliveryBatch != "nudge-batch-1" {
		t.Fatalf("batch after an ambiguous failure = %q, want nudge-batch-1: the redelivery must replay the first attempt", ambiguous.DeliveryBatch)
	}
	rejected, _ := failedQueuedNudge(pinned, fmt.Errorf("sending: %w: no", runtime.ErrNudgeRejected), now)
	if rejected.DeliveryBatch != "" {
		t.Fatalf("batch after a rejection = %q, want it cleared so the next claim pins a new one", rejected.DeliveryBatch)
	}
}

// The poller hands the provider one delivery key per batch: the same key when
// it redelivers a batch whose first delivery had an unknown outcome, and a
// new key once the runtime definitively rejected it.
func TestTryDeliverQueuedNudgesByPollerPinsTheDeliveryKey(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	now := time.Now().Add(-1 * time.Minute)
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "check hook output", now)); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	fake := runtime.NewFake()
	sp := runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: fake})
	if err := sp.Start(context.Background(), "sess-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	idleSince := time.Now().Add(-10 * time.Second)
	fake.Activity = map[string]time.Time{"sess-worker": idleSince}
	target := nudgeTarget{
		cityPath:    dir,
		transport:   "acp",
		agent:       config.Agent{Name: "worker", Session: "acp"},
		resolved:    &config.ResolvedProvider{Name: "codex"},
		sessionName: "sess-worker",
	}
	obs := worker.LiveObservation{Running: true, Alive: true, LastActivity: &idleSince}
	pass := func(nudgeErr error) {
		t.Helper()
		fake.NudgeErrors = map[string]error{}
		if nudgeErr != nil {
			fake.NudgeErrors["sess-worker"] = nudgeErr
		}
		if _, err := tryDeliverQueuedNudgesByPoller(target, openNudgeBeadStore(dir).Store, openNudgeBeadStore(dir).Store, sp, 3*time.Second, obs); err != nil {
			t.Fatalf("tryDeliverQueuedNudgesByPoller: %v", err)
		}
		// Make the failed item due again at once.
		if err := withNudgeQueueState(dir, func(state *nudgeQueueState) error {
			for i := range state.Pending {
				state.Pending[i].DeliverAfter = time.Now().Add(-time.Second)
			}
			return nil
		}); err != nil {
			t.Fatalf("withNudgeQueueState: %v", err)
		}
	}

	pass(errors.New("read tcp: i/o timeout"))
	pass(fmt.Errorf("%w: archived", runtime.ErrNudgeRejected))
	pass(nil)

	var keys []string
	for _, call := range fake.Calls {
		if call.Method == "Nudge" {
			keys = append(keys, call.DeliveryKey)
		}
	}
	if len(keys) != 3 {
		t.Fatalf("nudge deliveries = %d (%v), want 3", len(keys), keys)
	}
	if keys[0] == "" || keys[0] != keys[1] {
		t.Fatalf("keys after an ambiguous failure = %q then %q, want one non-empty key", keys[0], keys[1])
	}
	if keys[2] == keys[1] {
		t.Fatalf("key after a rejection = %q, want a new key", keys[2])
	}
}

// keyedPollerHarness drives tryDeliverQueuedNudgesByPoller against a fake
// keyed runtime for the delivery-key tests.
type keyedPollerHarness struct {
	t      *testing.T
	dir    string
	fake   *runtime.Fake
	sp     runtime.Provider
	target nudgeTarget
	obs    worker.LiveObservation
}

func newKeyedPollerHarness(t *testing.T) *keyedPollerHarness {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	fake := runtime.NewFake()
	sp := runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: fake})
	if err := sp.Start(context.Background(), "sess-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	idleSince := time.Now().Add(-10 * time.Second)
	fake.Activity = map[string]time.Time{"sess-worker": idleSince}
	return &keyedPollerHarness{
		t:    t,
		dir:  dir,
		fake: fake,
		sp:   sp,
		target: nudgeTarget{
			cityPath:    dir,
			transport:   "acp",
			agent:       config.Agent{Name: "worker", Session: "acp"},
			resolved:    &config.ResolvedProvider{Name: "codex"},
			sessionName: "sess-worker",
		},
		obs: worker.LiveObservation{Running: true, Alive: true, LastActivity: &idleSince},
	}
}

func (h *keyedPollerHarness) enqueue(message string) {
	h.t.Helper()
	if err := enqueueQueuedNudge(h.dir, newQueuedNudge("worker", message, time.Now().Add(-time.Minute))); err != nil {
		h.t.Fatalf("enqueueQueuedNudge: %v", err)
	}
}

// pass runs one poller pass whose nudge fails with nudgeErr (nil delivers),
// then makes every pending item due again at once.
func (h *keyedPollerHarness) pass(nudgeErr error) {
	h.t.Helper()
	h.fake.NudgeErrors = map[string]error{}
	if nudgeErr != nil {
		h.fake.NudgeErrors["sess-worker"] = nudgeErr
	}
	store := openNudgeBeadStore(h.dir).Store
	if _, err := tryDeliverQueuedNudgesByPoller(h.target, store, store, h.sp, 3*time.Second, h.obs); err != nil {
		h.t.Fatalf("tryDeliverQueuedNudgesByPoller: %v", err)
	}
	h.makeDue()
}

func (h *keyedPollerHarness) makeDue() {
	h.t.Helper()
	if err := withNudgeQueueState(h.dir, func(state *nudgeQueueState) error {
		for i := range state.Pending {
			state.Pending[i].DeliverAfter = time.Now().Add(-time.Second)
		}
		return nil
	}); err != nil {
		h.t.Fatalf("withNudgeQueueState: %v", err)
	}
}

// expireLeases lapses every in-flight claim, as when its claimer died.
func (h *keyedPollerHarness) expireLeases() {
	h.t.Helper()
	if err := withNudgeQueueState(h.dir, func(state *nudgeQueueState) error {
		for i := range state.InFlight {
			state.InFlight[i].LeaseUntil = time.Now().Add(-time.Second)
		}
		return nil
	}); err != nil {
		h.t.Fatalf("withNudgeQueueState: %v", err)
	}
}

// deliveries returns each Nudge the runtime received: its key and its text.
func (h *keyedPollerHarness) deliveries() [][2]string {
	var out [][2]string
	for _, call := range h.fake.Calls {
		if call.Method == "Nudge" {
			out = append(out, [2]string{call.DeliveryKey, call.Message})
		}
	}
	return out
}

// enqueueWaitNudge queues a nudge announcing a new wait bead in state, and
// returns the nudge's id and the wait's.
func (h *keyedPollerHarness) enqueueWaitNudge(store beads.Store, message, state string) (nudgeID, waitID string) {
	h.t.Helper()
	wait, err := store.Create(beads.Bead{
		Type:     waitBeadType,
		Status:   "open",
		Labels:   []string{waitBeadLabel, "session:s-1"},
		Metadata: map[string]string{"session_id": "s-1", "state": state},
	})
	if err != nil {
		h.t.Fatalf("create wait: %v", err)
	}
	item := newQueuedNudgeWithOptions("worker", message, "wait", time.Now().Add(-time.Minute), queuedNudgeOptions{
		Reference: &nudgeReference{Kind: "bead", ID: wait.ID},
	})
	if err := enqueueQueuedNudge(h.dir, item); err != nil {
		h.t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	return item.ID, wait.ID
}

// failBeadWritesOf makes every metadata write to the bead behind queued nudge
// nudgeID fail, as a store outage fails its withdraw, until restore is called.
func (h *keyedPollerHarness) failBeadWritesOf(store beads.Store, nudgeID string) (restore func()) {
	h.t.Helper()
	item, ok, err := nudgeFrontDoor(beads.NudgesStore{Store: store}).Find(nudgeID)
	if err != nil || !ok {
		h.t.Fatalf("finding the bead of %s: ok=%v err=%v", nudgeID, ok, err)
	}
	prev := openOwnedNudgeBeadStore
	openOwnedNudgeBeadStore = func(cityPath string) (beads.NudgesStore, beads.Store) {
		nudges, opened := prev(cityPath)
		if nudges.Store != nil {
			nudges.Store = metadataFailingStore{Store: nudges.Store, failID: item.BeadID}
		}
		return nudges, opened
	}
	restore = func() { openOwnedNudgeBeadStore = prev }
	h.t.Cleanup(restore)
	return restore
}

// metadataFailingStore fails every metadata batch write to one bead.
type metadataFailingStore struct {
	beads.Store
	failID string
}

func (s metadataFailingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if id == s.failID {
		return fmt.Errorf("setting metadata on %q: connection refused", id)
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

// A batch whose delivery had an unknown outcome is redelivered under its
// pinned key with exactly its own items: a nudge that became due meanwhile
// waits for a batch of its own, so the redelivery cannot get a new key and
// start a second turn with the first batch's content.
func TestTryDeliverQueuedNudgesByPollerKeepsTheBatchWhenNewNudgesArrive(t *testing.T) {
	h := newKeyedPollerHarness(t)
	h.enqueue("nudge A")
	h.pass(errors.New("read tcp: i/o timeout"))
	h.enqueue("nudge B")
	h.makeDue()
	h.pass(nil)
	h.pass(nil)

	got := h.deliveries()
	if len(got) != 3 {
		t.Fatalf("deliveries = %d (%v), want 3", len(got), got)
	}
	if got[0][0] == "" || got[1][0] != got[0][0] {
		t.Fatalf("redelivery key = %q, want the first delivery's %q", got[1][0], got[0][0])
	}
	if strings.Contains(got[1][1], "nudge B") || !strings.Contains(got[1][1], "nudge A") {
		t.Fatalf("redelivery text = %q, want nudge A alone", got[1][1])
	}
	if got[2][0] == got[0][0] || !strings.Contains(got[2][1], "nudge B") || strings.Contains(got[2][1], "nudge A") {
		t.Fatalf("third delivery = %v, want nudge B alone under a new key", got[2])
	}
}

// A claim whose lease lapsed (the delivering process died mid-delivery) is
// redelivered under the key pinned when it was claimed.
func TestTryDeliverQueuedNudgesByPollerKeepsTheBatchAcrossLeaseExpiry(t *testing.T) {
	h := newKeyedPollerHarness(t)
	h.enqueue("nudge A")
	claimed, err := claimDueQueuedNudgeBatchForTarget(h.dir, h.target, time.Now())
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %v, %v; want nudge A", claimed, err)
	}
	pinned := claimed[0].DeliveryBatch
	if pinned == "" {
		t.Fatal("claimed item carries no delivery batch")
	}
	// The claimer died: its lease lapses with the batch undelivered.
	h.expireLeases()
	h.enqueue("nudge B")
	h.pass(nil)

	got := h.deliveries()
	if len(got) != 1 {
		t.Fatalf("deliveries = %d (%v), want 1", len(got), got)
	}
	if got[0][0] != pinned {
		t.Fatalf("delivery key after lease expiry = %q, want the pinned %q", got[0][0], pinned)
	}
	if strings.Contains(got[0][1], "nudge B") {
		t.Fatalf("redelivery text = %q, want nudge A alone", got[0][1])
	}
}

// A pinned batch is only ever delivered whole. When the live-session census
// stops vouching for one of its items between passes, the batch waits rather
// than going out without that item: T3 would commit the partial text under the
// batch key and later replay that receipt for the missing item, which would
// then be acked without ever reaching the agent.
func TestTryDeliverQueuedNudgesByPollerNeverSplitsAPinnedBatch(t *testing.T) {
	h := newKeyedPollerHarness(t)
	h.target.sessionID = "S2"
	census := map[string]struct{}{"S2": {}}
	old := liveNudgeFenceSessionIDs
	liveNudgeFenceSessionIDs = func(string) map[string]struct{} { return census }
	t.Cleanup(func() { liveNudgeFenceSessionIDs = old })

	// A is addressed to the predecessor S1; successor S2 may claim it only
	// while the census vouches that S2 is live and S1 is not.
	a := newQueuedNudgeWithOptions("worker", "nudge A", "session", time.Now().Add(-time.Minute), queuedNudgeOptions{SessionID: "S1"})
	if err := enqueueQueuedNudge(h.dir, a); err != nil {
		t.Fatal(err)
	}
	h.enqueue("nudge B")

	h.pass(errors.New("read tcp: i/o timeout")) // {A,B} under K: unknown outcome
	// S3 replaces S2 before the redelivery. The claim moved A onto S2's fence,
	// so S3 may claim the batch only while the census vouches that S3 is live
	// and S2 is not.
	h.target.sessionID = "S3"
	census = nil // the census read fails transiently
	h.pass(nil)
	census = map[string]struct{}{"S3": {}}
	h.pass(nil)

	got := h.deliveries()
	if len(got) == 0 {
		t.Fatal("no deliveries")
	}
	key := got[0][0]
	for i, d := range got {
		if d[0] != key {
			t.Fatalf("delivery %d key = %q, want the batch key %q (deliveries %v)", i, d[0], key, got)
		}
		if !strings.Contains(d[1], "nudge A") || !strings.Contains(d[1], "nudge B") {
			t.Fatalf("delivery %d under the batch key = %q, want the whole batch (deliveries %v)", i, d[1], got)
		}
	}
	if len(got) != 2 {
		t.Fatalf("deliveries = %d (%v), want the ambiguous attempt and one whole redelivery", len(got), got)
	}
	pending, inFlight, dead, err := listQueuedNudges(h.dir, "worker", time.Now())
	if err != nil || len(pending)+len(inFlight)+len(dead) != 0 {
		t.Fatalf("queue after delivery: pending=%v inFlight=%v dead=%v err=%v, want empty", pending, inFlight, dead, err)
	}
}

// A nudge that successor S2 claimed from its drained predecessor S1's fence
// moves onto S2's fence in the queue. When S1 wakes again before the
// redelivery, the batch is still S2's to redeliver whole under its key, and S1
// claims none of it. Left on S1's fence, that nudge would split the batch for
// both sessions, each holding an item the other may not claim, until both
// items expired undelivered.
func TestTryDeliverQueuedNudgesByPollerKeepsTheBatchWhenAFencedPredecessorWakes(t *testing.T) {
	h := newKeyedPollerHarness(t)
	h.target.sessionID = "S2"
	census := map[string]struct{}{"S2": {}}
	old := liveNudgeFenceSessionIDs
	liveNudgeFenceSessionIDs = func(string) map[string]struct{} { return census }
	t.Cleanup(func() { liveNudgeFenceSessionIDs = old })

	for _, n := range []struct{ message, sessionID string }{{"nudge A", "S1"}, {"nudge B", "S2"}} {
		item := newQueuedNudgeWithOptions("worker", n.message, "session", time.Now().Add(-time.Minute), queuedNudgeOptions{SessionID: n.sessionID})
		if err := enqueueQueuedNudge(h.dir, item); err != nil {
			t.Fatal(err)
		}
	}

	h.pass(errors.New("read tcp: i/o timeout"))      // {A,B} under K to S2: unknown outcome
	census = map[string]struct{}{"S1": {}, "S2": {}} // S1 wakes again
	predecessor := h.target
	predecessor.sessionID = "S1"
	claimed, err := claimDueQueuedNudgeBatchForTarget(h.dir, predecessor, time.Now())
	if err != nil || len(claimed) != 0 {
		t.Fatalf("woken predecessor S1 claimed %+v (err %v), want nothing: the batch is S2's", claimed, err)
	}
	h.pass(nil)

	got := h.deliveries()
	if len(got) != 2 {
		t.Fatalf("deliveries = %d (%v), want the ambiguous attempt and one whole redelivery", len(got), got)
	}
	if got[0][0] == "" || got[1][0] != got[0][0] {
		t.Fatalf("redelivery key = %q, want the batch key %q", got[1][0], got[0][0])
	}
	if !strings.Contains(got[1][1], "nudge A") || !strings.Contains(got[1][1], "nudge B") {
		t.Fatalf("redelivery text = %q, want the whole batch", got[1][1])
	}
	pending, inFlight, dead, err := listQueuedNudges(h.dir, "worker", time.Now())
	if err != nil || len(pending)+len(inFlight)+len(dead) != 0 {
		t.Fatalf("queue after delivery: pending=%v inFlight=%v dead=%v err=%v, want empty", pending, inFlight, dead, err)
	}
}

// A claim for delivery under a key moves each nudge it takes onto the claiming
// session's fence in the queue, both when it pins a new batch and when it
// reclaims a pinned one: the batch carries the fence of the session that last
// tried to deliver it.
func TestClaimDueQueuedNudgeBatchMovesItsNudgesOntoTheClaimingFence(t *testing.T) {
	h := newKeyedPollerHarness(t)
	var census map[string]struct{}
	old := liveNudgeFenceSessionIDs
	liveNudgeFenceSessionIDs = func(string) map[string]struct{} { return census }
	t.Cleanup(func() { liveNudgeFenceSessionIDs = old })
	a := newQueuedNudgeWithOptions("worker", "nudge A", "session", time.Now().Add(-time.Minute), queuedNudgeOptions{SessionID: "S1"})
	if err := enqueueQueuedNudge(h.dir, a); err != nil {
		t.Fatal(err)
	}

	// S2 replaces S1 and pins A in a new batch; S3 then replaces S2 and
	// reclaims that batch after S2's claim lapsed.
	for _, successor := range []string{"S2", "S3"} {
		census = map[string]struct{}{successor: {}}
		target := h.target
		target.sessionID = successor
		claimed, err := claimDueQueuedNudgeBatchForTarget(h.dir, target, time.Now())
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim for %s = %+v, %v; want nudge A", successor, claimed, err)
		}
		_, inFlight, _, err := listQueuedNudges(h.dir, "worker", time.Now())
		if err != nil || len(inFlight) != 1 || inFlight[0].SessionID != successor {
			t.Fatalf("in flight after the claim for %s = %+v (err %v), want nudge A on %s's fence", successor, inFlight, err, successor)
		}
		h.expireLeases()
	}
}

// While part of a pinned batch is still in flight under a live lease, the rest
// of it is not claimed: that would be a second, partial delivery under the key.
// Unpinned items still go out as a batch of their own.
func TestClaimDueQueuedNudgeBatchSkipsAPinnedBatchPartlyInFlight(t *testing.T) {
	h := newKeyedPollerHarness(t)
	h.enqueue("nudge A")
	h.enqueue("nudge B")
	if err := withNudgeQueueState(h.dir, func(state *nudgeQueueState) error {
		for i := range state.Pending {
			state.Pending[i].DeliveryBatch = "batch-k"
		}
		first := state.Pending[0]
		first.ClaimedAt = time.Now().UTC()
		first.LeaseUntil = time.Now().Add(time.Hour).UTC()
		state.InFlight = append(state.InFlight, first)
		state.Pending = state.Pending[1:]
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.enqueue("nudge C")

	claimed, err := claimDueQueuedNudgeBatchForTarget(h.dir, h.target, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].Message != "nudge C" || claimed[0].DeliveryBatch == "batch-k" || claimed[0].DeliveryBatch == "" {
		t.Fatalf("claimed = %+v, want nudge C alone under a new batch", claimed)
	}
}

// A nudge withheld from a pinned batch whose withdraw write failed stays in
// flight on the batch key, and lease expiry returns it to pending still pinned
// there. The rest of the batch therefore goes out under a new key: once the
// withheld nudge's wait fires it goes out alone under the old key, and T3 must
// not answer it with the receipt of a delivery that never contained it.
func TestTryDeliverQueuedNudgesByPollerRekeysTheBatchWhenAWithdrawFails(t *testing.T) {
	h := newKeyedPollerHarness(t)
	store := openNudgeBeadStore(h.dir).Store
	h.enqueue("nudge A")
	nudgeB, waitB := h.enqueueWaitNudge(store, "nudge B", waitStatePending)
	restore := h.failBeadWritesOf(store, nudgeB)

	// {A,B} is claimed under one key. B's wait has not fired, so B is
	// withheld, and its withdraw fails: B stays in flight while A goes out.
	if _, err := tryDeliverQueuedNudgesByPoller(h.target, store, store, h.sp, 3*time.Second, h.obs); err == nil || !strings.Contains(err.Error(), "withdrawing blocked nudges") {
		t.Fatalf("first pass error = %v, want the failed withdraw reported", err)
	}
	restore()
	h.expireLeases()
	if err := store.SetMetadata(waitB, "state", waitStateReady); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	h.pass(nil)

	got := h.deliveries()
	if len(got) != 2 {
		t.Fatalf("deliveries = %d (%v), want nudge A, then nudge B once its wait fired", len(got), got)
	}
	if !strings.Contains(got[0][1], "nudge A") || strings.Contains(got[0][1], "nudge B") {
		t.Fatalf("first delivery = %q, want nudge A alone", got[0][1])
	}
	if !strings.Contains(got[1][1], "nudge B") || strings.Contains(got[1][1], "nudge A") {
		t.Fatalf("second delivery = %q, want nudge B alone", got[1][1])
	}
	if got[0][0] == "" || got[1][0] == "" || got[0][0] == got[1][0] {
		t.Fatalf("delivery keys = %q then %q, want two distinct keys: T3 would answer nudge B with the receipt of the delivery without it", got[0][0], got[1][0])
	}
	pending, inFlight, dead, err := listQueuedNudges(h.dir, "worker", time.Now())
	if err != nil || len(pending)+len(inFlight)+len(dead) != 0 {
		t.Fatalf("queue after delivery: pending=%v inFlight=%v dead=%v err=%v, want empty", pending, inFlight, dead, err)
	}
}

// A withheld nudge whose withdraw succeeded is terminal, so the rest of its
// batch is still the batch's delivery and keeps the key: a redelivery after an
// unknown outcome replays the first attempt instead of starting a second turn.
func TestTryDeliverQueuedNudgesByPollerKeepsTheKeyWhenAWithheldNudgeIsWithdrawn(t *testing.T) {
	h := newKeyedPollerHarness(t)
	store := openNudgeBeadStore(h.dir).Store
	h.enqueue("nudge A")
	_, waitB := h.enqueueWaitNudge(store, "nudge B", waitStateReady)

	h.pass(errors.New("read tcp: i/o timeout")) // {A,B} under K: unknown outcome
	if err := store.SetMetadata(waitB, "state", waitStateCanceled); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	h.pass(nil)

	got := h.deliveries()
	if len(got) != 2 {
		t.Fatalf("deliveries = %d (%v), want the ambiguous attempt and one redelivery", len(got), got)
	}
	if !strings.Contains(got[0][1], "nudge A") || !strings.Contains(got[0][1], "nudge B") {
		t.Fatalf("first delivery = %q, want the whole batch", got[0][1])
	}
	if !strings.Contains(got[1][1], "nudge A") || strings.Contains(got[1][1], "nudge B") {
		t.Fatalf("redelivery = %q, want nudge A without the withdrawn nudge B", got[1][1])
	}
	if got[0][0] == "" || got[1][0] != got[0][0] {
		t.Fatalf("redelivery key = %q, want the batch key %q", got[1][0], got[0][0])
	}
	pending, inFlight, dead, err := listQueuedNudges(h.dir, "worker", time.Now())
	if err != nil || len(pending)+len(inFlight)+len(dead) != 0 {
		t.Fatalf("queue after delivery: pending=%v inFlight=%v dead=%v err=%v, want empty", pending, inFlight, dead, err)
	}
}

// The new key is persisted before delivery: when the re-keyed delivery has an
// unknown outcome, its redelivery reuses the new key so that T3 replays it,
// while the nudge whose withdraw failed keeps the old one.
func TestTryDeliverQueuedNudgesByPollerRedeliversARekeyedBatchUnderItsNewKey(t *testing.T) {
	h := newKeyedPollerHarness(t)
	store := openNudgeBeadStore(h.dir).Store
	h.enqueue("nudge A")
	nudgeB, _ := h.enqueueWaitNudge(store, "nudge B", waitStatePending)
	restore := h.failBeadWritesOf(store, nudgeB)
	h.fake.NudgeErrors = map[string]error{"sess-worker": errors.New("read tcp: i/o timeout")}

	if _, err := tryDeliverQueuedNudgesByPoller(h.target, store, store, h.sp, 3*time.Second, h.obs); err == nil || !strings.Contains(err.Error(), "withdrawing blocked nudges") {
		t.Fatalf("first pass error = %v, want the failed withdraw reported", err)
	}
	restore()
	h.makeDue()
	h.pass(nil)

	got := h.deliveries()
	if len(got) != 2 {
		t.Fatalf("deliveries = %d (%v), want the ambiguous attempt and one redelivery", len(got), got)
	}
	for i, delivery := range got {
		if !strings.Contains(delivery[1], "nudge A") || strings.Contains(delivery[1], "nudge B") {
			t.Fatalf("delivery %d = %q, want nudge A alone", i, delivery[1])
		}
	}
	if got[0][0] == "" || got[1][0] != got[0][0] {
		t.Fatalf("redelivery key = %q, want the key of the first attempt %q", got[1][0], got[0][0])
	}
	_, inFlight, _, err := listQueuedNudges(h.dir, "worker", time.Now())
	if err != nil || len(inFlight) != 1 || inFlight[0].ID != nudgeB || inFlight[0].DeliveryBatch == "" || inFlight[0].DeliveryBatch == got[0][0] {
		t.Fatalf("in flight = %+v (err %v), want nudge B alone on the old key", inFlight, err)
	}
}

// A re-key that cannot pin every item it would deliver writes nothing and
// delivers nothing: the claims go back to pending on their old key.
func TestRekeyQueuedNudgesAfterFailedBookkeepingReleasesWhenAClaimIsGone(t *testing.T) {
	h := newKeyedPollerHarness(t)
	h.enqueue("nudge A")
	h.enqueue("nudge B")
	claimed, err := claimDueQueuedNudgeBatchForTarget(h.dir, h.target, time.Now())
	if err != nil || len(claimed) != 2 || claimed[0].DeliveryBatch == "" {
		t.Fatalf("claimed = %+v (err %v), want nudges A and B under one key", claimed, err)
	}
	key := claimed[0].DeliveryBatch
	gone := ""
	for _, item := range claimed {
		if item.Message == "nudge B" {
			gone = item.ID
		}
	}
	if gone == "" {
		t.Fatalf("claimed = %+v, want nudge B among them", claimed)
	}
	if err := ackQueuedNudges(h.dir, []string{gone}); err != nil {
		t.Fatalf("ackQueuedNudges: %v", err)
	}

	items, err := rekeyQueuedNudgesAfterFailedBookkeeping(h.dir, claimed, errors.New("withdrawing blocked nudges: connection refused"))
	if len(items) != 0 || err == nil || !strings.Contains(err.Error(), "withdrawing blocked nudges") || !strings.Contains(err.Error(), "no longer in flight") {
		t.Fatalf("rekey = %+v, %v; want nothing to deliver, and the bookkeeping and re-key failures", items, err)
	}
	pending, inFlight, _, err := listQueuedNudges(h.dir, "worker", time.Now())
	if err != nil || len(inFlight) != 0 || len(pending) != 1 || pending[0].Message != "nudge A" || pending[0].DeliveryBatch != key {
		t.Fatalf("queue: pending=%+v inFlight=%+v err=%v, want nudge A back in pending on key %q", pending, inFlight, err, key)
	}
}
