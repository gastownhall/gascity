package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

func TestQueuedNudgeDeliveryKey(t *testing.T) {
	a := queuedNudge{ID: "nudge-a"}
	b := queuedNudge{ID: "nudge-b"}
	c := queuedNudge{ID: "nudge-c"}
	key := queuedNudgeDeliveryKey([]queuedNudge{a, b})
	if key == "" {
		t.Fatal("delivery key is empty")
	}
	if got := queuedNudgeDeliveryKey([]queuedNudge{b, a}); got != key {
		t.Fatalf("key of the same batch in another order = %q, want %q", got, key)
	}
	if got := queuedNudgeDeliveryKey([]queuedNudge{a, b, c}); got == key {
		t.Fatalf("a different batch shares the key %q", got)
	}
	bumped := a
	bumped.DeliveryGeneration = 1
	if got := queuedNudgeDeliveryKey([]queuedNudge{bumped, b}); got == key {
		t.Fatalf("a batch redelivered after a rejection kept the key %q", got)
	}
}

func TestFailedQueuedNudgeBumpsTheDeliveryGenerationOnlyOnRejection(t *testing.T) {
	now := time.Now()
	ambiguous, _ := failedQueuedNudge(queuedNudge{ID: "n", ExpiresAt: now.Add(time.Hour)}, errors.New("i/o timeout"), now)
	if ambiguous.DeliveryGeneration != 0 {
		t.Fatalf("generation after an ambiguous failure = %d, want 0: the redelivery must replay the first attempt", ambiguous.DeliveryGeneration)
	}
	rejected, _ := failedQueuedNudge(queuedNudge{ID: "n", ExpiresAt: now.Add(time.Hour)}, fmt.Errorf("sending: %w: no", runtime.ErrNudgeRejected), now)
	if rejected.DeliveryGeneration != 1 {
		t.Fatalf("generation after a rejection = %d, want 1", rejected.DeliveryGeneration)
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
