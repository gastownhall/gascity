package main

import (
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storebinding"
	"github.com/gastownhall/gascity/internal/storebinding/storebindingtest"
)

// TestFileNudgeQueueConformance runs the Nudges class suite against the file
// queue openNudgeQueue builds, under the two legacy behaviors it declares.
// Each assertion gets a fresh city whose shadow store is one MemStore, both
// the store a caller passes and the one every self-opening verb (enqueue,
// claim, list, ack, release, record-failure, Maintain) opens. The second leg
// passes the zero store, so those verbs open their own, as the facades do for
// a caller that holds none; Rollback and WithdrawQueuedWaitNudges then touch no
// shadow (TestFileNudgeQueueZeroStoreRollbackAndWithdrawOpenNoStore), which no
// case of the suite observes.
// It replaces a package seam, so it is serial.
func TestFileNudgeQueueConformance(t *testing.T) {
	var mu sync.Mutex
	stores := map[string]*beads.MemStore{}
	prev := openOwnedNudgeBeadStore
	openOwnedNudgeBeadStore = func(cityPath string) (beads.NudgesStore, beads.Store) {
		mu.Lock()
		defer mu.Unlock()
		store := stores[cityPath]
		if store == nil {
			return beads.NudgesStore{}, nil
		}
		return beads.NudgesStore{Store: store}, store
	}
	t.Cleanup(func() { openOwnedNudgeBeadStore = prev })

	for _, leg := range []struct {
		name      string
		passStore bool
	}{
		{name: "with the caller's store", passStore: true},
		{name: "with the zero store", passStore: false},
	} {
		t.Run(leg.name, func(t *testing.T) {
			storebindingtest.RunNudgeFrontDoorTests(storebindingtest.Wrap(t), storebindingtest.NudgesSuite{
				NewFrontDoors: func(tb storebindingtest.TB) storebinding.NudgeFrontDoors {
					city := tb.TempDir()
					store := beads.NewMemStore()
					mu.Lock()
					stores[city] = store
					mu.Unlock()
					nudges := beads.NudgesStore{Store: store}
					passed := beads.NudgesStore{}
					if leg.passStore {
						passed = nudges
					}
					return storebinding.NudgeFrontDoors{Queue: openNudgeQueue(city, passed).Queue, Shadows: nudgeShadows(nudges)}
				},
				Capability: storebinding.ClassCapability{Available: true},
				Policy:     storebindingtest.NudgeQueuePolicy{VerbsMaintain: true, ForgetsRetiredIDs: true},
			})
		})
	}
}

// TestFileNudgeQueueZeroStoreRollbackAndWithdrawOpenNoStore pins what
// openNudgeQueue documents for the zero store: Rollback and
// WithdrawQueuedWaitNudges reach shadows only through a caller-passed store, so
// with the zero store they open none and leave the retired item's shadow open.
// Every deployed caller passes the store it holds; the facades that open one
// for them would open it twice if these verbs opened their own (the
// withdraw_deferred golden's "withdraw with no store" step). It replaces a
// package seam, so it is serial.
func TestFileNudgeQueueZeroStoreRollbackAndWithdrawOpenNoStore(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	city := t.TempDir()
	mem := beads.NewMemStore()
	opens := 0
	prev := openOwnedNudgeBeadStore
	openOwnedNudgeBeadStore = func(string) (beads.NudgesStore, beads.Store) {
		opens++
		return beads.NudgesStore{Store: mem}, mem
	}
	t.Cleanup(func() { openOwnedNudgeBeadStore = prev })

	now := time.Now().UTC()
	roll := newQueuedNudgeWithOptions("alpha", "m", "session", now, queuedNudgeOptions{ID: "n-roll"})
	wait := newQueuedNudgeWithOptions("alpha", "m", "wait", now, queuedNudgeOptions{ID: "wait-w1-0-1"})
	q := openNudgeQueue(city, beads.NudgesStore{}).Queue
	for _, item := range []queuedNudge{roll, wait} {
		if err := q.Enqueue(item); err != nil {
			t.Fatalf("Enqueue(%s): %v", item.ID, err)
		}
	}
	shadows := nudgeShadows(beads.NudgesStore{Store: mem})
	for _, id := range []string{roll.ID, wait.ID} {
		if _, open, err := shadows.Find(id); err != nil || !open {
			t.Fatalf("shadow %s after Enqueue: open=%v err=%v, want an open shadow", id, open, err)
		}
	}
	before := opens
	if err := q.Rollback(roll, "managed wake failed"); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if err := q.WithdrawQueuedWaitNudges([]string{wait.ID}); err != nil {
		t.Fatalf("WithdrawQueuedWaitNudges: %v", err)
	}
	if opens != before {
		t.Errorf("Rollback and WithdrawQueuedWaitNudges with the zero store opened %d stores, want none", opens-before)
	}
	for _, id := range []string{roll.ID, wait.ID} {
		if _, open, err := shadows.Find(id); err != nil || !open {
			t.Errorf("shadow %s after a zero-store retire: open=%v err=%v, want it left open", id, open, err)
		}
	}
	pending, inFlight, dead, err := q.ListForAgent("alpha", now)
	if err != nil {
		t.Fatalf("ListForAgent: %v", err)
	}
	if len(pending) != 0 || len(inFlight) != 0 || len(dead) != 1 || dead[0].ID != roll.ID {
		t.Errorf("queue after retire: pending=%d in-flight=%d dead=%v, want only n-roll dead", len(pending), len(inFlight), dead)
	}
}
