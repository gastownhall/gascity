package configedit_test

import (
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
	"github.com/gastownhall/gascity/internal/testutil"
)

// A rig suspend or resume that waits on the suspension fence, held by a
// controller stopping a scope's store, does not hold the Editor's lock while it
// waits, so every other config edit goes through in the meantime.
func TestRigSuspensionQueuedOnTheFenceDoesNotHoldUpOtherEdits(t *testing.T) {
	for _, tc := range []struct {
		name      string
		write     func(*configedit.Editor, string) error
		suspended bool
	}{
		{name: "SuspendRig", write: (*configedit.Editor).SuspendRig, suspended: true},
		{name: "ResumeRig", write: (*configedit.Editor).ResumeRig, suspended: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTOML(t, dir, cityWithRig())
			ed := configedit.NewEditor(fsys.OSFS{}, path)
			release, err := suspensionstate.Fence(dir)
			if err != nil {
				t.Fatalf("Fence: %v", err)
			}
			var running sync.WaitGroup
			t.Cleanup(func() {
				release()
				running.Wait()
			})

			written := make(chan error, 1)
			running.Go(func() { written <- tc.write(ed, "my-rig") })
			testutil.AwaitFlockWaiters(t, suspensionstate.FenceFile(dir), 1)
			edited := make(chan error, 1)
			running.Go(func() { edited <- ed.Edit(func(*config.City) error { return nil }) })
			select {
			case err := <-edited:
				if err != nil {
					t.Fatalf("Edit: %v", err)
				}
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatalf("a config edit waited on a %s queued on the suspension fence", tc.name)
			}

			release()
			select {
			case err := <-written:
				if err != nil {
					t.Fatalf("%s: %v", tc.name, err)
				}
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatalf("%s did not finish after the fence was released", tc.name)
			}
			st, err := suspensionstate.Load(fsys.OSFS{}, dir)
			if err != nil {
				t.Fatal(err)
			}
			if v, ok := suspensionstate.ExplicitRig(st, "my-rig"); !ok || v != tc.suspended {
				t.Errorf("my-rig override = %v (set %v), want %v", v, ok, tc.suspended)
			}
		})
	}
}
