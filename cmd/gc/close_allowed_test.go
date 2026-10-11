package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestClosePermitted is the shared precondition as a table (R9-C): an
// operator's suspend refuses every close but a rollback's and a failed
// create's; a pending request refuses a work-gated close, not a duplicate's,
// a releasing one's or a failed create's; work refuses a work-gated close and
// a duplicate's alone.
func TestClosePermitted(t *testing.T) {
	held := sessionpkg.Info{SleepIntent: string(sessionpkg.SleepReasonUserHold)}
	soft := sessionpkg.Info{MetadataState: string(sessionpkg.StateSuspended)}
	wake := sessionpkg.Info{WakeRequest: string(sessionpkg.WakeCauseExplicit)}
	restart := sessionpkg.Info{RestartRequested: "true"}
	failed := sessionpkg.Info{MetadataState: string(sessionpkg.StateFailedCreate), SleepIntent: string(sessionpkg.SleepReasonUserHold), WakeRequest: string(sessionpkg.WakeCauseExplicit)}
	for _, tc := range []struct {
		name string
		info sessionpkg.Info
		gate closeGate
		work bool
		want string
	}{
		{"a free seat", sessionpkg.Info{}, closeGated, false, ""},
		{"an operator suspend", held, closeGated, false, causeKeepsSlot},
		{"an operator suspend, releasing", held, closeReleasing, false, causeKeepsSlot},
		{"an operator suspend, duplicate", held, closeDuplicate, false, causeKeepsSlot},
		{"an operator suspend, rollback", held, closeRollback, false, ""},
		{"a soft suspend", soft, closeGated, false, ""},
		{"an explicit wake request", wake, closeGated, false, causeRequestPending},
		{"a restart request", restart, closeGated, false, causeRequestPending},
		{"a stale wake request on a duplicate", wake, closeDuplicate, false, ""},
		{"a wake request, releasing", wake, closeReleasing, false, ""},
		{"a non-explicit wake request", sessionpkg.Info{WakeRequest: "work"}, closeGated, false, ""},
		{"work", sessionpkg.Info{}, closeGated, true, causeHasWork},
		{"work on a duplicate", sessionpkg.Info{}, closeDuplicate, true, causeHasWork},
		{"work, releasing", sessionpkg.Info{}, closeReleasing, true, ""},
		{"work, rollback", sessionpkg.Info{}, closeRollback, true, ""},
		{"a held failed create with a request", failed, closeGated, false, ""},
		{"a held failed create with work", failed, closeGated, true, causeHasWork},
	} {
		if got := closePermitted(tc.info, tc.gate, tc.work); got != tc.want {
			t.Errorf("%s: closePermitted = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestCloseSeatAppliesThePrecondition: every legacy close refuses an
// operator-suspended row and leaves it open; a work-gated close refuses a
// pending wake request, a duplicate's does not; a failed create under a
// suspend still closes, as legacy's did.
func TestCloseSeatAppliesThePrecondition(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	row := func(t *testing.T, meta map[string]string) (beads.Store, sessionpkg.Info) {
		t.Helper()
		store := beads.NewMemStore()
		b, err := store.Create(beads.Bead{Title: "s", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: meta})
		if err != nil {
			t.Fatal(err)
		}
		return store, sessionInfoFromBead(b)
	}
	for _, tc := range []struct {
		name   string
		meta   map[string]string
		why    closeWhy
		closes bool
	}{
		{"a gated close of a suspended row", map[string]string{"state": "asleep", "sleep_intent": "user-hold"}, closeWhy{reason: "orphaned", gate: closeGated}, false},
		{"a corpse close of a suspended row", map[string]string{"state": "asleep", "sleep_intent": "user-hold"}, closeWhy{reason: "dead-runtime", gate: closeReleasing}, false},
		{"a gated close under a wake request", map[string]string{"state": "asleep", "wake_request": "explicit"}, closeWhy{reason: "orphaned", gate: closeGated}, false},
		{"a duplicate close under a stale wake request", map[string]string{"state": "asleep", "wake_request": "explicit"}, closeWhy{reason: "duplicate", gate: closeDuplicate}, true},
		{"a failed create under a suspend", map[string]string{"state": "failed-create", "sleep_intent": "user-hold"}, closeWhy{reason: "orphaned", gate: closeGated}, true},
		{"a free gated close", map[string]string{"state": "asleep"}, closeWhy{reason: "orphaned", gate: closeGated}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, info := row(t, tc.meta)
			var stderr bytes.Buffer
			if got := closeSeat(store, testSeatWork("", nil, store, nil), info, tc.why, now, &stderr); got != tc.closes {
				t.Fatalf("closeSeat = %v, want %v (stderr %q)", got, tc.closes, stderr.String())
			}
			b, _ := store.Get(info.ID)
			if (b.Status == "closed") != tc.closes {
				t.Fatalf("row status %s, want closed=%v", b.Status, tc.closes)
			}
			if tc.meta["state"] == "failed-create" && b.Metadata["state"] != "failed-create" {
				t.Fatalf("a failed create closed as %q, want the failed-create close", b.Metadata["state"])
			}
		})
	}
}
