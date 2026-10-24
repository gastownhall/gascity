package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestClosePermitted is the shared precondition as a table (R9-C): an
// operator's suspend refuses the free-slot close alone, never a failed
// create's; a request on the row refuses no close (the commit premise
// refuses one that lands later); work refuses the work-gated closes alone.
func TestClosePermitted(t *testing.T) {
	held := sessionpkg.Info{SleepIntent: string(sessionpkg.SleepReasonUserHold)}
	soft := sessionpkg.Info{MetadataState: string(sessionpkg.StateSuspended)}
	wake := sessionpkg.Info{WakeRequest: string(sessionpkg.WakeCauseExplicit)}
	restart := sessionpkg.Info{RestartRequested: "true"}
	failed := sessionpkg.Info{MetadataState: string(sessionpkg.StateFailedCreate), SleepIntent: string(sessionpkg.SleepReasonUserHold)}
	for _, tc := range []struct {
		name string
		info sessionpkg.Info
		gate closeGate
		work bool
		want string
	}{
		{"a free seat", sessionpkg.Info{}, closeGated, false, ""},
		{"an operator suspend, free slot", held, closeFreeSlot, false, causeKeepsSlot},
		{"an operator suspend, gated", held, closeGated, false, ""},
		{"an operator suspend, releasing", held, closeReleasing, false, ""},
		{"an operator suspend, rollback", held, closeRollback, false, ""},
		{"a soft suspend, free slot", soft, closeFreeSlot, false, ""},
		{"a held failed create, free slot", failed, closeFreeSlot, false, ""},
		{"an explicit wake request", wake, closeGated, false, ""},
		{"an explicit wake request, free slot", wake, closeFreeSlot, false, ""},
		{"a restart request", restart, closeGated, false, ""},
		{"work", sessionpkg.Info{}, closeGated, true, causeHasWork},
		{"work, free slot", sessionpkg.Info{}, closeFreeSlot, true, causeHasWork},
		{"work, releasing", sessionpkg.Info{}, closeReleasing, true, ""},
		{"work, rollback", sessionpkg.Info{}, closeRollback, true, ""},
	} {
		if got := closePermitted(tc.info, tc.gate, tc.work); got != tc.want {
			t.Errorf("%s: closePermitted = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestCloseSeatAppliesThePrecondition: the free-slot close keeps an
// operator-suspended row, and every other close keeps legacy's behavior and
// closes it; a wake or restart request on the row refuses no close; a failed
// create under a suspend closes with the failed-create close.
func TestCloseSeatAppliesThePrecondition(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	held := map[string]string{"state": "asleep", "sleep_intent": "user-hold"}
	for _, tc := range []struct {
		name   string
		meta   map[string]string
		why    closeWhy
		closes bool
	}{
		{"a free-slot close of a suspended row", held, closeWhy{reason: "idle", gate: closeFreeSlot}, false},
		{"a gated close of a suspended row", held, closeWhy{reason: "orphaned", gate: closeGated}, true},
		{"a duplicate close of a suspended row", held, closeWhy{reason: "duplicate", gate: closeGated}, true},
		{"a corpse close of a suspended row", held, closeWhy{reason: "dead-runtime", gate: closeReleasing}, true},
		{"a gated close under a wake request", map[string]string{"state": "asleep", "wake_request": "explicit"}, closeWhy{reason: "orphaned", gate: closeGated}, true},
		{"a free-slot close under a restart request", map[string]string{"state": "asleep", "restart_requested": "true"}, closeWhy{reason: "idle", gate: closeFreeSlot}, true},
		{"a failed create under a suspend", map[string]string{"state": "failed-create", "sleep_intent": "user-hold"}, closeWhy{reason: "idle", gate: closeFreeSlot}, true},
		{"a free gated close", map[string]string{"state": "asleep"}, closeWhy{reason: "orphaned", gate: closeGated}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			b, err := store.Create(beads.Bead{Title: "s", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: tc.meta})
			if err != nil {
				t.Fatal(err)
			}
			info := sessionInfoFromBead(b)
			var stderr bytes.Buffer
			if got := closeSeat(store, testSeatWork("", nil, store, nil), info, tc.why, now, &stderr); got != tc.closes {
				t.Fatalf("closeSeat = %v, want %v (stderr %q)", got, tc.closes, stderr.String())
			}
			b, _ = store.Get(info.ID)
			if (b.Status == "closed") != tc.closes {
				t.Fatalf("row status %s, want closed=%v", b.Status, tc.closes)
			}
			if tc.meta["state"] == "failed-create" && b.Metadata["state"] != "failed-create" {
				t.Fatalf("a failed create closed as %q, want the failed-create close", b.Metadata["state"])
			}
		})
	}
}

// TestCloseSeatLogsARefusalOncePerEpisode: with its owner's refusal log, a
// refused close says why once, again when its cause changes; a refusal no
// one could read (the sweep's nil writer) records nothing, so the next
// caller that can print does.
func TestCloseSeatLogsARefusalOncePerEpisode(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{Title: "s", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{"state": "asleep", "sleep_intent": "user-hold"}})
	if err != nil {
		t.Fatal(err)
	}
	info := sessionInfoFromBead(b)
	var log closeRefusalLog
	sw := func() *SeatWork {
		sw := testSeatWork("", nil, store, nil)
		sw.refusals = &log
		return sw
	}
	closeSeat(store, sw(), info, closeWhy{reason: "gc_swept", gate: closeFreeSlot}, now, nil)
	var stderr bytes.Buffer
	for i := 0; i < 3; i++ {
		closeSeat(store, sw(), info, closeWhy{reason: "idle", gate: closeFreeSlot}, now, &stderr)
	}
	if n := strings.Count(stderr.String(), "refused ("+causeKeepsSlot+")"); n != 1 {
		t.Fatalf("logged the refusal %d times, want once: %q", n, stderr.String())
	}
	createWork(t, store, "open", info.ID, nil)
	closeSeat(store, sw(), sessionpkg.Info{ID: info.ID, MetadataState: "asleep"}, closeWhy{reason: "idle", gate: closeFreeSlot}, now, &stderr)
	if !strings.Contains(stderr.String(), "refused ("+causeHasWork+")") {
		t.Fatalf("a changed cause was not logged: %q", stderr.String())
	}
}
