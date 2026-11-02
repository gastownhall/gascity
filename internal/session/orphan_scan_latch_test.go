package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// abandonOrphanScan runs a pre-start cleanup whose context is canceled while
// its scan is blocked, so the cleanup gives up and the scan stays outstanding.
func abandonOrphanScan(t *testing.T, mgr *Manager, sessionID string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := killOrphansAsync(ctx, mgr, sessionID)
	synctest.Wait()
	cancel()
	synctest.Wait()
	requireUnanswered(t, done, context.Canceled)
}

// requireRefusedAsOutstanding fails unless a cleanup returns at once, without
// a wait, refused because the key's earlier scan is still outstanding.
func requireRefusedAsOutstanding(t *testing.T, mgr *Manager, sessionID string) {
	t.Helper()
	done := killOrphansAsync(context.Background(), mgr, sessionID)
	synctest.Wait()
	requireUnanswered(t, done, runtime.ErrBoundedCallOutstanding)
}

func TestKillExistingOrphansRetryThroughTheSameManagerStartsNoSecondScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const sessionID = "sid-retry-same"
		city := t.TempDir()
		sp := newBlockingScanFake(t, strandedOrphan(city, sessionID))
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(city))

		abandonOrphanScan(t, mgr, sessionID)
		requireRefusedAsOutstanding(t, mgr, sessionID)
		if got := sp.scanCount(); got != 1 {
			t.Errorf("scans started = %d, want only the outstanding one", got)
		}
		releaseScan(t, sp)
	})
}

func TestKillExistingOrphansRetryThroughANewManagerStartsNoSecondScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const sessionID = "sid-retry-new"
		city := t.TempDir()
		store := beads.NewMemStore()
		sp := newBlockingScanFake(t, strandedOrphan(city, sessionID))

		abandonOrphanScan(t, newTestManager(t, store, sp, WithCityPath(city)), sessionID)
		requireRefusedAsOutstanding(t, newTestManager(t, store, sp, WithCityPath(city)), sessionID)
		if got := sp.scanCount(); got != 1 {
			t.Errorf("scans started = %d, want only the outstanding one", got)
		}
		releaseScan(t, sp)
	})
}

// A city named by another spelling of its path (surrounding space, a
// trailing separator, a symlink) is the same city, so its scan is the same
// outstanding scan.
func TestKillExistingOrphansEquivalentCityPathsShareTheOutstandingScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const sessionID = "sid-equivalent"
		city := t.TempDir()
		link := filepath.Join(t.TempDir(), "city-link")
		if err := os.Symlink(city, link); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
		store := beads.NewMemStore()
		sp := newBlockingScanFake(t, strandedOrphan(city, sessionID))

		abandonOrphanScan(t, newTestManager(t, store, sp, WithCityPath(city)), sessionID)
		for _, spelling := range []string{" " + city + " ", city + string(filepath.Separator), link} {
			requireRefusedAsOutstanding(t, newTestManager(t, store, sp, WithCityPath(spelling)), sessionID)
		}
		if got := sp.scanCount(); got != 1 {
			t.Errorf("scans started = %d, want only the outstanding one", got)
		}
		releaseScan(t, sp)
	})
}

// An outstanding scan holds only its own city and session: another session
// in the city, and the same session in another city, each scan for itself.
func TestKillExistingOrphansOtherCitiesAndSessionsScanIndependently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const sessionID, otherSessionID = "sid-independent", "sid-independent-other"
		city, otherCity := t.TempDir(), t.TempDir()
		store := beads.NewMemStore()
		sp := newBlockingScanFake(t, strandedOrphan(city, sessionID))
		mgr := newTestManager(t, store, sp, WithCityPath(city))

		abandonOrphanScan(t, mgr, sessionID)
		abandonOrphanScan(t, mgr, otherSessionID)
		abandonOrphanScan(t, newTestManager(t, store, sp, WithCityPath(otherCity)), sessionID)
		if got := sp.scanCount(); got != 3 {
			t.Errorf("scans started = %d, want one for each city and session", got)
		}
		releaseScan(t, sp)
	})
}
