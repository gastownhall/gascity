package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestWaitRegisteredDuringPendingResetSurvivesStart: a deps wait registered
// against a session after a fresh restart was requested for it, but before
// that start runs, belongs to the conversation the start begins. The start's
// epoch rotation must not cancel it as continuation-stale.
func TestWaitRegisteredDuringPendingResetSurvivesStart(t *testing.T) {
	for _, tc := range []struct {
		name              string
		depClosesInWindow bool
	}{
		{name: "dep-closes-after-start"},
		{name: "dep-closes-before-start", depClosesInWindow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			sf := sessionFrontDoor(store)
			now := time.Date(2026, 10, 24, 16, 7, 0, 0, time.UTC)

			dep, err := store.Create(beads.Bead{Title: "upstream step", Type: "task"})
			if err != nil {
				t.Fatalf("create dep: %v", err)
			}
			sess, err := store.Create(beads.Bead{
				Title:  "worker",
				Type:   sessionBeadType,
				Labels: []string{sessionBeadLabel},
				Metadata: map[string]string{
					"session_name":       "worker",
					"agent_name":         "worker",
					"continuation_epoch": "2",
					"state":              string(sessionpkg.StateActive),
				},
			})
			if err != nil {
				t.Fatalf("create session: %v", err)
			}

			// Manager.RequestFreshRestart markers.
			if err := sf.ApplyPatch(sess.ID, sessionpkg.MetadataPatch{
				"restart_requested":          "true",
				"continuation_reset_pending": "true",
			}); err != nil {
				t.Fatalf("request reset: %v", err)
			}
			w, err := sf.CreateWait(sessionpkg.WaitSpec{
				SessionID:        sess.ID,
				Kind:             "deps",
				DepIDs:           []string{dep.ID},
				DepMode:          "all",
				Note:             "pick up the next step",
				CreatedBySession: "coordinator",
				Now:              now.Add(25 * time.Second),
			})
			if err != nil {
				t.Fatalf("CreateWait: %v", err)
			}
			if tc.depClosesInWindow {
				if err := store.Close(dep.ID); err != nil {
					t.Fatalf("close dep: %v", err)
				}
			}
			if _, err := prepareWaitWakeState(store, now.Add(time.Minute)); err != nil {
				t.Fatalf("prepareWaitWakeState (window): %v", err)
			}

			// The start consumes the reset: epoch 2 -> 3, marker cleared
			// (reconcile_effect_launch.go).
			start := now.Add(7 * time.Minute)
			patch := sessionpkg.PreWakePatch(sessionpkg.PreWakePatchInput{
				Generation:        3,
				InstanceToken:     "tok-3",
				ContinuationEpoch: 3,
				Now:               start,
			})
			patch["restart_requested"] = ""
			patch["continuation_reset_pending"] = ""
			if err := sf.ApplyPatch(sess.ID, patch); err != nil {
				t.Fatalf("ApplyPatch(pre-wake): %v", err)
			}
			if err := sf.ApplyPatch(sess.ID, sessionpkg.MetadataPatch{"state": string(sessionpkg.StateActive)}); err != nil {
				t.Fatalf("ApplyPatch(active): %v", err)
			}
			if !tc.depClosesInWindow {
				if err := store.Close(dep.ID); err != nil {
					t.Fatalf("close dep: %v", err)
				}
			}

			if _, err := prepareWaitWakeState(store, start.Add(time.Minute)); err != nil {
				t.Fatalf("prepareWaitWakeState (after start): %v", err)
			}
			raw, err := store.Get(w.ID)
			if err != nil {
				t.Fatalf("Get wait: %v", err)
			}
			if raw.Status == "closed" && raw.Metadata["state"] == waitStateCanceled {
				t.Fatalf("wait registered during the pending reset (registered_epoch=%q) was canceled %q at the start that reset begins",
					raw.Metadata["registered_epoch"], raw.Metadata["last_error"])
			}
			if got := raw.Metadata["state"]; got != waitStateReady {
				t.Fatalf("wait state after start with dep closed = %q, want ready", got)
			}
		})
	}
}
