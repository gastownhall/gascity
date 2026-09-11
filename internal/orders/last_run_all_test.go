package orders

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// LastRunAll answers for every order in one read, and must agree with the
// per-order LastRun it is there to replace. Doctor asks this question once per
// monitored cron and cooldown order, and on a bd-backed store each per-order
// read costs a subprocess pair.
func TestLastRunAllAgreesWithPerOrderLastRun(t *testing.T) {
	store := beads.NewMemStore()
	for _, spec := range []struct{ title, order string }{
		{"order:digest", "digest"},
		{"order:digest", "digest"},
		{"order:reaper", "reaper"},
	} {
		if _, err := store.Create(beads.Bead{
			Title:  spec.title,
			Status: "closed",
			Labels: []string{RunLabel(spec.order), "order-tracking"},
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}

	front := ordersStoreOver(store)
	index, complete := front.LastRunAll()
	if !complete {
		t.Fatal("LastRunAll reported an incomplete index over a healthy store")
	}
	for _, order := range []string{"digest", "reaper"} {
		want, err := front.LastRun(order)
		if err != nil {
			t.Fatalf("LastRun(%q): %v", order, err)
		}
		got, found := index[order]
		if !found {
			t.Fatalf("LastRunAll has no entry for %q", order)
		}
		if !got.Equal(want) {
			t.Fatalf("LastRunAll[%q] = %s, want %s", order, got, want)
		}
	}
	if _, found := index["never-fired"]; found {
		t.Fatal("LastRunAll invented an entry for an order that never ran")
	}
}

// A tracking bead carrying no order-run label names no order, so it is skipped
// rather than bucketed under an empty key.
func TestLastRunAllSkipsUnlabelledTrackingBeads(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{Title: "stray", Status: "open", Labels: []string{"order-tracking"}}); err != nil {
		t.Fatal(err)
	}
	if index, _ := ordersStoreOver(store).LastRunAll(); len(index) != 0 {
		t.Fatalf("LastRunAll = %v, want no entries", index)
	}
}

// A leg that fails makes the index unusable as an authority on absence: an
// empty map from a broken read must not be read as "no order has ever run".
// The caller's fallback is the per-order LastRun, so the signal has to reach it.
func TestLastRunAllReportsAnIncompleteIndex(t *testing.T) {
	broken := &rowsErrorStore{MemStore: beads.NewMemStore(), err: errors.New("store unreachable")}
	index, complete := ordersStoreOver(broken).LastRunAll()
	if complete {
		t.Fatal("LastRunAll called a failed read complete")
	}
	if len(index) != 0 {
		t.Fatalf("LastRunAll = %v, want no entries from a failed read", index)
	}
	if _, complete := LastRunAllAcross([]*Store{ordersStoreOver(beads.NewMemStore()), ordersStoreOver(broken)}); complete {
		t.Fatal("LastRunAllAcross called a federation with a failed leg complete")
	}
}

// LastRunAllAcross keeps the newest time per order across a federation, the
// same way LastRunAcross does for one order at a time.
func TestLastRunAllAcrossKeepsTheNewestPerOrder(t *testing.T) {
	older := beads.NewMemStore()
	oldRun, err := older.Create(beads.Bead{Title: "order:digest", Status: "closed", Labels: []string{RunLabel("digest"), "order-tracking"}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	newer := beads.NewMemStore()
	newRun, err := newer.Create(beads.Bead{Title: "order:digest", Status: "closed", Labels: []string{RunLabel("digest"), "order-tracking"}})
	if err != nil {
		t.Fatal(err)
	}
	if !newRun.CreatedAt.After(oldRun.CreatedAt) {
		t.Fatalf("test setup invalid: %s is not after %s", newRun.CreatedAt, oldRun.CreatedAt)
	}

	index, complete := LastRunAllAcross([]*Store{ordersStoreOver(older), nil, ordersStoreOver(newer)})
	if !complete {
		t.Fatal("LastRunAllAcross reported an incomplete index over healthy stores")
	}
	if got := index["digest"]; !got.Equal(newRun.CreatedAt) {
		t.Fatalf("LastRunAllAcross[digest] = %s, want %s", got, newRun.CreatedAt)
	}
}
