package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// beadListTestClock is a settable clock for a cache built WithClock; it is
// read under the cache's lock, so it must not block.
type beadListTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *beadListTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *beadListTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestBeadListDropsOutOfProcessCloseWhileCacheIsBusy pins #5239 where the
// reporter saw it: a bead closed straight in the backing store (an
// out-of-process bd close, no gc event, no ledger feed) leaves GET /beads
// within one cadence even while the cache is written every 10s. Before the
// first full reconcile was dated from the prime, each write pushed that
// reconcile back, and the closed bead was served until a restart.
func TestBeadListDropsOutOfProcessCloseWhileCacheIsBusy(t *testing.T) {
	state := newFakeState(t)
	backing := beads.NewMemStore()
	closedOutOfBand, err := backing.Create(beads.Bead{Title: "closed by bd"})
	if err != nil {
		t.Fatalf("Create(closed by bd): %v", err)
	}
	busy, err := backing.Create(beads.Bead{Title: "written by gc"})
	if err != nil {
		t.Fatalf("Create(written by gc): %v", err)
	}
	clock := &beadListTestClock{now: time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)}
	cache := beads.NewCachingStoreForTest(backing, nil, beads.WithClock(clock.Now))
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	state.stores["myrig"] = cache

	if err := backing.Close(closedOutOfBand.ID); err != nil {
		t.Fatalf("backing Close: %v", err)
	}

	h := newTestCityHandler(t, state)
	listBeads := func() []beads.Bead {
		t.Helper()
		req := httptest.NewRequest("GET", cityURL(state, "/beads?limit=1000"), nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /beads status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Items []beads.Bead `json:"items"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp.Items
	}

	// Before any reconcile the API still serves the out-of-band close as
	// open, so the bead's absence at the end is the reconcile's doing.
	staleServed := false
	for _, b := range listBeads() {
		if b.ID == closedOutOfBand.ID {
			if b.Status != "open" {
				t.Fatalf("before any reconcile GET /beads lists %s as %q, want the stale %q", b.ID, b.Status, "open")
			}
			staleServed = true
		}
	}
	if !staleServed {
		t.Fatalf("before any reconcile GET /beads does not list %s; want it served as stale %q", closedOutOfBand.ID, "open")
	}

	for step := 1; step <= 12; step++ {
		clock.Advance(10 * time.Second)
		title := fmt.Sprintf("written by gc, step %d", step)
		if err := cache.Update(busy.ID, beads.UpdateOpts{Title: &title}); err != nil {
			t.Fatalf("step %d: Update(%s): %v", step, busy.ID, err)
		}
		cache.ReconcileIfDueForTest()
	}

	items := listBeads()
	sawBusy := false
	for _, b := range items {
		if b.ID == closedOutOfBand.ID {
			t.Fatalf("GET /beads still lists %s as %q 120s after an out-of-process close", b.ID, b.Status)
		}
		if b.ID == busy.ID {
			sawBusy = true
		}
	}
	if !sawBusy {
		t.Fatalf("GET /beads items = %v; want the open bead %s listed", beadIDsOf(items), busy.ID)
	}
}
