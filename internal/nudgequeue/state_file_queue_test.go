package nudgequeue

import (
	"os"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func deferredTestItem(id string, deliverAfter time.Time) Item {
	return Item{
		ID: id, Agent: "alpha", SessionID: "s-1", ContinuationEpoch: "2", Source: "session",
		Message: "later", CreatedAt: deliverAfter, DeliverAfter: deliverAfter, ExpiresAt: deliverAfter.Add(time.Hour),
	}
}

// EnqueueDeferred writes the bytes the deferred-submit append always wrote: the
// item appended to pending and the buckets sorted, under the queue lock.
func TestStateFileQueueEnqueueDeferredWritesTheAppendBytes(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	earlier := deferredTestItem("nudge-early", now)
	later := deferredTestItem("nudge-late", now.Add(time.Minute))

	viaQueue := t.TempDir()
	queue := StateFileQueue{CityPath: viaQueue}
	for _, item := range []Item{later, earlier} {
		if err := queue.EnqueueDeferred(item); err != nil {
			t.Fatalf("EnqueueDeferred(%s): %v", item.ID, err)
		}
	}

	direct := t.TempDir()
	for _, item := range []Item{later, earlier} {
		if err := WithState(direct, func(state *State) error {
			state.Pending = append(state.Pending, item)
			SortState(state)
			return nil
		}); err != nil {
			t.Fatalf("WithState: %v", err)
		}
	}

	got, err := os.ReadFile(StatePath(viaQueue))
	if err != nil {
		t.Fatalf("reading the queue's state.json: %v", err)
	}
	want, err := os.ReadFile(StatePath(direct))
	if err != nil {
		t.Fatalf("reading the direct state.json: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("state.json =\n%s\nwant\n%s", got, want)
	}
	state, err := LoadState(viaQueue)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Pending) != 2 || state.Pending[0].ID != "nudge-early" {
		t.Fatalf("pending = %+v, want nudge-early then nudge-late", state.Pending)
	}
}

// The deferred append keeps no idempotence: the same id twice is two entries.
func TestStateFileQueueEnqueueDeferredDoesNotDeduplicate(t *testing.T) {
	city := t.TempDir()
	queue := StateFileQueue{CityPath: city}
	item := deferredTestItem("nudge-twice", time.Now().UTC())
	for range 2 {
		if err := queue.EnqueueDeferred(item); err != nil {
			t.Fatalf("EnqueueDeferred: %v", err)
		}
	}
	state, err := LoadState(city)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Pending) != 2 {
		t.Fatalf("pending = %d entries, want 2", len(state.Pending))
	}
}

// WithdrawQueuedWaitNudges is WithdrawWaitNudges over the queue's shadow store:
// with one, the shadow is closed as wait-canceled before the entry is removed;
// without one, the entry is removed alone.
func TestStateFileQueueWithdrawQueuedWaitNudges(t *testing.T) {
	for _, withShadows := range []bool{true, false} {
		name := "without shadows"
		if withShadows {
			name = "with shadows"
		}
		t.Run(name, func(t *testing.T) {
			city := t.TempDir()
			store := beads.NewMemStore()
			item := deferredTestItem("wait-w1-0-1", time.Now().UTC())
			beadID, _, err := NewStore(beads.NudgesStore{Store: store}).Save(item)
			if err != nil {
				t.Fatalf("Save: %v", err)
			}
			item.BeadID = beadID
			if err := (StateFileQueue{CityPath: city}).EnqueueDeferred(item); err != nil {
				t.Fatalf("EnqueueDeferred: %v", err)
			}
			queue := StateFileQueue{CityPath: city}
			if withShadows {
				queue.Shadows = store
			}
			if err := queue.WithdrawQueuedWaitNudges([]string{item.ID}); err != nil {
				t.Fatalf("WithdrawQueuedWaitNudges: %v", err)
			}
			state, err := LoadState(city)
			if err != nil {
				t.Fatalf("LoadState: %v", err)
			}
			if len(state.Pending) != 0 {
				t.Fatalf("pending = %+v, want the wait nudge withdrawn", state.Pending)
			}
			shadow, err := store.Get(beadID)
			if err != nil {
				t.Fatalf("Get shadow: %v", err)
			}
			if closed := shadow.Status == "closed"; closed != withShadows {
				t.Fatalf("shadow status = %q, want closed only when the queue holds the shadow store", shadow.Status)
			}
		})
	}
}
