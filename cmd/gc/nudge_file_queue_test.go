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
// a caller that holds none; WithdrawQueuedWaitNudges then touches no shadow
// (TestFileNudgeQueueZeroStoreRollbackPaysAndWithdrawOpensNoStore), which no
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

// TestFileNudgeQueueZeroStoreRollbackPaysAndWithdrawOpensNoStore pins what
// openNudgeQueue documents for the zero store. Rollback's dead letter owes its
// shadow a failed record, which Rollback pays after the queue lock like every
// verb whose commit owes one (ga-fsp5w9), opening a store of its own for it.
// WithdrawQueuedWaitNudges reaches shadows only through a caller-passed store,
// so with the zero store it opens none and leaves the withdrawn item's shadow
// open: the facades that open one for it would open it twice (the
// withdraw_deferred golden's "withdraw with no store" step). It replaces a
// package seam, so it is serial.
func TestFileNudgeQueueZeroStoreRollbackPaysAndWithdrawOpensNoStore(t *testing.T) {
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
	if opens != before+1 {
		t.Errorf("Rollback with the zero store opened %d stores, want one to pay its debt", opens-before)
	}
	if _, open, err := shadows.Find(roll.ID); err != nil || open {
		t.Errorf("shadow %s after a zero-store rollback: open=%v err=%v, want it closed", roll.ID, open, err)
	}
	before = opens
	if err := q.WithdrawQueuedWaitNudges([]string{wait.ID}); err != nil {
		t.Fatalf("WithdrawQueuedWaitNudges: %v", err)
	}
	if opens != before {
		t.Errorf("WithdrawQueuedWaitNudges with the zero store opened %d stores, want none", opens-before)
	}
	if _, open, err := shadows.Find(wait.ID); err != nil || !open {
		t.Errorf("shadow %s after a zero-store withdraw: open=%v err=%v, want it left open", wait.ID, open, err)
	}
	pending, inFlight, dead, err := q.ListForAgent("alpha", now)
	if err != nil {
		t.Fatalf("ListForAgent: %v", err)
	}
	if len(pending) != 0 || len(inFlight) != 0 || len(dead) != 1 || dead[0].ID != roll.ID {
		t.Errorf("queue after retire: pending=%d in-flight=%d dead=%v, want only n-roll dead", len(pending), len(inFlight), dead)
	}
}
