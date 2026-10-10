package beads_test

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// The recent-local-write window, which keeps a reconcile from rolling back a
// local write, opens and closes on the injected clock (WithClock), not the
// real one. The fake clock starts decades before the real one, so a real read
// on either side of the window breaks one of the two checks: a real check
// against a fake stamp closes it at once, and a fake check against a real
// stamp never closes it. mc-hbgfc: the simulator's rows sat in a real-time
// window for the whole run, so no scan ever installed the backing's row (I10).
func TestCachingStoreRecentWriteWindowRunsOnInjectedClock(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	now := t0
	mem := beads.NewMemStore()
	b, err := mem.Create(beads.Bead{Title: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	cache := beads.NewCachingStoreForTest(mem, nil, beads.WithClock(func() time.Time { return now }))
	if err := cache.Prime(t.Context()); err != nil {
		t.Fatal(err)
	}
	local, outside := "local", "outside"
	if err := cache.Update(b.ID, beads.UpdateOpts{Title: &local}); err != nil {
		t.Fatal(err)
	}
	// Another writer changes the row with no event: only a scan can see it.
	if err := mem.Update(b.ID, beads.UpdateOpts{Title: &outside}); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		advance time.Duration
		want    string
	}{
		{4 * time.Second, local},   // inside the 5s window: the scan keeps the local write
		{2 * time.Second, outside}, // past it: the scan installs the backing's row
	} {
		now = now.Add(step.advance)
		cache.ReconcileNowForTest()
		got, err := cache.Get(b.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Title != step.want {
			t.Fatalf("at +%s on the injected clock: title %q, want %q", now.Sub(t0), got.Title, step.want)
		}
	}
}
