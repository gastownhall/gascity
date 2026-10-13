package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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
	if err := withNudgeQueueState(h.dir, func(state *nudgeQueueState) error {
		for i := range state.InFlight {
			state.InFlight[i].LeaseUntil = time.Now().Add(-time.Second)
		}
		return nil
	}); err != nil {
		t.Fatalf("withNudgeQueueState: %v", err)
	}
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
