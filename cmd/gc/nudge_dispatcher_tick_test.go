package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestControllerTickDeliversACPNudgesInLegacyDispatcherMode (mc-z0og9;
// staging J34 B fails both reconciler modes). Kills a controller that leaves
// an ACP session's queued nudge undelivered in the default (legacy)
// dispatcher mode, where no poller can deliver it: one patrol tick of the
// legacy reconciler, and one of v2, each deliver it as a runtime prompt.
func TestControllerTickDeliversACPNudgesInLegacyDispatcherMode(t *testing.T) {
	for _, mode := range []string{"legacy", "v2"} {
		t.Run(mode, func(t *testing.T) {
			cr, store := newPhaseFixtureRuntime(t, false, mode == "v2")
			if mode == "v2" {
				armMaintenanceLanes(t, cr, store)
				attachTestV2(t, cr)
			}
			if cr.cfg.Daemon.NudgeDispatcherMode() != "legacy" {
				t.Fatalf("fixture dispatcher mode %q, want the legacy default", cr.cfg.Daemon.NudgeDispatcherMode())
			}
			rows, err := store.Store.List(beads.ListQuery{Label: sessionBeadLabel})
			if err != nil || len(rows) != 1 {
				t.Fatalf("fixture session rows %v (%v), want one", rows, err)
			}
			if err := store.Store.SetMetadata(rows[0].ID, "transport", "acp"); err != nil {
				t.Fatal(err)
			}
			if err := enqueueQueuedNudge(cr.cityPath, newQueuedNudge("worker", "j34 tick nudge", time.Now().Add(-time.Minute))); err != nil {
				t.Fatalf("enqueueQueuedNudge: %v", err)
			}
			sp := cr.sp.(*runtime.Fake)
			sp.SetActivity("worker", time.Now().Add(-10*time.Second))
			runFixtureTick(cr, "patrol")
			for _, call := range sp.Calls {
				if call.Method == "Nudge" && strings.Contains(call.Message, "j34 tick nudge") {
					return
				}
			}
			t.Fatalf("%s tick: no prompt carried the queued nudge; calls %v", mode, sp.Calls)
		})
	}
}

// Kills a legacy-mode gate that counts only pending items (#7816 review):
// a queue whose only item is in flight with an expired lease (a delivery a
// crashed pass claimed) still runs the controller's pass, which delivers it
// to the ACP seat, in both reconciler modes.
func TestControllerTickRecoversAnInFlightOnlyACPQueue(t *testing.T) {
	for _, mode := range []string{"legacy", "v2"} {
		t.Run(mode, func(t *testing.T) {
			cr, store := newPhaseFixtureRuntime(t, false, mode == "v2")
			if mode == "v2" {
				armMaintenanceLanes(t, cr, store)
				attachTestV2(t, cr)
			}
			rows, err := store.Store.List(beads.ListQuery{Label: sessionBeadLabel})
			if err != nil || len(rows) != 1 {
				t.Fatalf("fixture session rows %v (%v), want one", rows, err)
			}
			if err := store.Store.SetMetadata(rows[0].ID, "transport", "acp"); err != nil {
				t.Fatal(err)
			}
			if err := enqueueQueuedNudge(cr.cityPath, newQueuedNudge("worker", "j34 in-flight nudge", time.Now().Add(-time.Minute))); err != nil {
				t.Fatalf("enqueueQueuedNudge: %v", err)
			}
			state, err := nudgequeue.LoadState(cr.cityPath)
			if err != nil || len(state.Pending) != 1 {
				t.Fatalf("queue %+v (%v), want one pending item", state, err)
			}
			item := state.Pending[0]
			item.ClaimedAt, item.LeaseUntil = time.Now().Add(-10*time.Minute), time.Now().Add(-time.Minute)
			state.Pending, state.InFlight = nil, []nudgequeue.Item{item}
			data, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(nudgequeue.StatePath(cr.cityPath), data, 0o644); err != nil {
				t.Fatal(err)
			}
			sp := cr.sp.(*runtime.Fake)
			sp.SetActivity("worker", time.Now().Add(-10*time.Second))
			runFixtureTick(cr, "patrol")
			for _, call := range sp.Calls {
				if call.Method == "Nudge" && strings.Contains(call.Message, "j34 in-flight nudge") {
					return
				}
			}
			t.Fatalf("%s tick: the in-flight-only queue was not delivered; calls %v", mode, sp.Calls)
		})
	}
}
