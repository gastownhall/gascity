package suspensionstate

import (
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/testutil"
)

// Writers that queue on the fence while a controller holds it for a stop
// each read the state only once they hold the fence, so the second write
// starts from the first one's result instead of replacing it with the state
// both saw before the stop.
func TestConcurrentWritesDuringAHeldFenceKeepBothUpdates(t *testing.T) {
	cityDir := t.TempDir()
	if err := SetRigSuspended(fsys.OSFS{}, cityDir, "r2", boolPtr(true)); err != nil {
		t.Fatalf("seeding r2 suspended: %v", err)
	}
	release, err := Fence(cityDir)
	if err != nil {
		t.Fatalf("Fence: %v", err)
	}
	var running sync.WaitGroup
	t.Cleanup(func() {
		release()
		running.Wait()
	})
	written := make(chan error, 2)
	running.Go(func() { written <- SetRigSuspended(fsys.OSFS{}, cityDir, "r2", boolPtr(false)) })
	running.Go(func() { written <- SetRigSuspended(fsys.OSFS{}, cityDir, "r3", boolPtr(true)) })
	testutil.AwaitFlockWaiters(t, FenceFile(cityDir), 2)
	release()
	for range 2 {
		select {
		case err := <-written:
			if err != nil {
				t.Fatalf("write queued on the fence: %v", err)
			}
		case <-time.After(testutil.GoroutineRaceTimeout):
			t.Fatal("a write queued on the fence did not complete after its release")
		}
	}
	st, err := Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := ExplicitRig(st, "r2"); !ok || v {
		t.Errorf("r2 override = %v (set %v), want the explicit resume kept", v, ok)
	}
	if v, ok := ExplicitRig(st, "r3"); !ok || !v {
		t.Errorf("r3 override = %v (set %v), want the explicit suspend kept", v, ok)
	}
}
