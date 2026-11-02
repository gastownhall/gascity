package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// A wedged session is retried every reconcile tick. Without a per-key
// in-flight latch each tick would spawn one more wedged scan; while the
// earlier scan is still out, a retry must answer at once that the scan is
// outstanding instead of starting another.
func TestKillExistingOrphansRepeatedStartsOfWedgedSessionScanOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedOrphanScan()
		sp.blockLeading.Store(1)
		defer sp.unblock()
		mgr := newTestManager(t, beads.NewMemStore(), sp)

		first := orphanWatchdog(t, func() error { return mgr.killExistingOrphans(context.Background(), "sid-once") })
		if !errors.Is(first, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("first cleanup = %v, want unavailable at the scan budget", first)
		}

		before := time.Now()
		second := orphanWatchdog(t, func() error { return mgr.killExistingOrphans(context.Background(), "sid-once") })

		if !errors.Is(second, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("second cleanup = %v, want unavailable: the wedged scan is still outstanding", second)
		}
		if elapsed := time.Since(before); elapsed >= orphanScanBudgetValue {
			t.Fatalf("second cleanup waited %v, want an immediate answer while the first scan is outstanding", elapsed)
		}
		if got := sp.calls.Load(); got != 1 {
			t.Fatalf("scans = %d over two attempts, want the one wedged scan", got)
		}
	})
}

// The latch is keyed by the scan, not by the Manager: a retried start builds
// a fresh Manager, and its scan must still see the wedged one outstanding
// rather than start a second.
func TestKillExistingOrphansLatchHoldsAcrossManagerRecreation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedOrphanScan()
		sp.blockLeading.Store(1)
		defer sp.unblock()
		mgr := newTestManager(t, beads.NewMemStore(), sp)

		first := orphanWatchdog(t, func() error { return mgr.killExistingOrphans(context.Background(), "sid-once") })
		if !errors.Is(first, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("first cleanup = %v, want unavailable at the scan budget", first)
		}

		sp2 := newScriptedOrphanScan()
		mgr2 := NewManagerWithOptions(beads.NewMemStore(), sp2, WithCityPath(mgr.cityPath))

		err := mgr2.killExistingOrphans(context.Background(), "sid-once")
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("recreated manager's cleanup = %v, want unavailable: the wedged scan is outstanding across managers", err)
		}
		if got := sp2.calls.Load(); got != 0 {
			t.Fatalf("recreated manager scanned %d times, want 0 while the earlier scan holds the key", got)
		}
	})
}

// One wedged session's scan must not hold another's: the latch is keyed by
// city and session, so scans of other sessions and other cities answer
// normally.
func TestKillExistingOrphansKeysLatchByCityAndSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedOrphanScan()
		sp.blockLeading.Store(1)
		defer sp.unblock()
		city := t.TempDir()
		mgr := NewManagerWithOptions(beads.NewMemStore(), sp, WithCityPath(city))

		first := orphanWatchdog(t, func() error { return mgr.killExistingOrphans(context.Background(), "s1") })
		if !errors.Is(first, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("wedged session: %v, want unavailable at the scan budget", first)
		}

		other := newScriptedOrphanScan()
		mgr2 := NewManagerWithOptions(beads.NewMemStore(), other, WithCityPath(city))
		if err := mgr2.killExistingOrphans(context.Background(), "s2"); err != nil {
			t.Fatalf("other session of the same city = %v, want nil: one wedged session must not block another", err)
		}
		if got := other.calls.Load(); got != 1 {
			t.Fatalf("other session scanned %d times, want 1", got)
		}
		mgr3 := NewManagerWithOptions(beads.NewMemStore(), newScriptedOrphanScan(), WithCityPath(t.TempDir()))
		if err := mgr3.killExistingOrphans(context.Background(), "s1"); err != nil {
			t.Fatalf("other city = %v, want nil: one wedged city must not block another", err)
		}
	})
}

// City paths that differ only as paths (a trailing separator) name the same
// city, so they latch the same scan.
func TestKillExistingOrphansLatchKeysNormalizeEquivalentCityPaths(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedOrphanScan()
		sp.blockLeading.Store(1)
		defer sp.unblock()
		mgr := newTestManager(t, beads.NewMemStore(), sp)

		first := orphanWatchdog(t, func() error { return mgr.killExistingOrphans(context.Background(), "sid-once") })
		if !errors.Is(first, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("first cleanup = %v, want unavailable at the scan budget", first)
		}

		sp2 := newScriptedOrphanScan()
		mgr2 := NewManagerWithOptions(beads.NewMemStore(), sp2, WithCityPath(mgr.cityPath+string(filepath.Separator)))

		err := mgr2.killExistingOrphans(context.Background(), "sid-once")
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("cleanup from an equivalent city path = %v, want unavailable: equivalent city paths must latch the same key", err)
		}
		if got := sp2.calls.Load(); got != 0 {
			t.Fatalf("equivalent path scanned %d times, want 0", got)
		}
	})
}
