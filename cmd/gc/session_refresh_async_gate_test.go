package main

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// TestPrepareStartCandidateRejectsNonSessionBead is the SYNC-path sibling of
// TestRefreshAsyncStartRejectsNonSessionBead. prepareStartCandidateForCity's in-lock
// re-Get now goes through the session front door (GetPersistedResponse), which
// rejects a mid-start bead that lost BOTH its type and its gc:session label
// (IsSessionBeadOrRepairable == false) with ErrSessionNotFound — where the raw
// store.Get it replaced would have returned the bead and proceeded. The sync prepare
// path must surface that front-door error and NOT build a prepared start (no launch)
// for a rejected bead.
func TestPrepareStartCandidateRejectsNonSessionBead(t *testing.T) {
	store := beads.NewMemStore()
	// A bead that lost its session identity: non-session type, no gc:session label.
	corrupt, err := store.Create(beads.Bead{
		Title:    "orphan",
		Type:     "task",
		Metadata: map[string]string{"state": "creating", "session_name": "worker-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate := startCandidate{
		info: session.Info{ID: corrupt.ID},
		tp:   TemplateParams{TemplateName: "worker"},
	}
	prepared, err := prepareStartCandidate(candidate, &config.City{}, store, clock.Real{})
	if err == nil {
		t.Fatal("prepareStartCandidate err=nil for a bead that failed the front-door session gate; want the loading-session rejection, no launch")
	}
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Errorf("err = %v, want ErrSessionNotFound (the front-door gate, not a raw store error)", err)
	}
	if prepared != nil {
		t.Errorf("prepared=%+v; the sync prepare path must not build a prepared start for a rejected bead", prepared)
	}
}

// TestRefreshAsyncStartRejectsNonSessionBead pins the documented WI-6 W5 delta
// (Risk 1): refreshAsyncStartResult's gate read now goes through the session front
// door (sessFront.Get), which rejects a mid-start bead that lost BOTH its type and
// its gc:session label (IsSessionBeadOrRepairable == false). Such a bead takes the
// refresh-failed path (ok=false, releaseInFlight=true → lease released, retry next
// tick) instead of committing. The raw store.Get the front door replaces would have
// returned the bead and proceeded to the staleness checks; this is the accepted,
// vanishingly-rare behavioral difference. A valid session bead still proceeds and
// gets its typed twin (candidate.info) populated from the fresh read.
func TestRefreshAsyncStartRejectsNonSessionBead(t *testing.T) {
	t.Run("non-session bead → refresh-failed", func(t *testing.T) {
		store := beads.NewMemStore()
		// A bead that lost its session identity: non-session type, no gc:session
		// label. It fails IsSessionBeadOrRepairable, so the front-door Get rejects it.
		corrupt, err := store.Create(beads.Bead{
			Title: "orphan",
			Type:  "task",
			Metadata: map[string]string{
				"state": "creating",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		result := startResult{
			prepared: preparedStart{
				candidate: startCandidate{
					info: session.Info{ID: corrupt.ID},
					tp:   TemplateParams{TemplateName: "worker"},
				},
			},
			outcome: "success",
		}
		_, ok, cleanupRuntime, releaseInFlight := refreshAsyncStartResult(result, store, ioDiscard{})
		if ok {
			t.Fatal("refreshAsyncStartResult ok=true for a bead that failed the front-door session gate; want refresh-failed")
		}
		if cleanupRuntime {
			t.Error("cleanupRuntime=true; the refresh-failed (front-door reject) path must not request runtime cleanup")
		}
		if !releaseInFlight {
			t.Error("releaseInFlight=false; the refresh-failed path must release the in-flight lease so the next tick retries")
		}
	})

	t.Run("valid session bead → proceeds + twin populated", func(t *testing.T) {
		store := beads.NewMemStore()
		bead, err := store.Create(beads.Bead{
			Title:  "worker",
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"session_name":   "worker-1",
				"state":          "creating",
				"instance_token": "tok-1",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		preparedBead := bead
		result := startResult{
			prepared: preparedStart{
				candidate: startCandidate{
					info: sessiontest.SeedBead(t, preparedBead),
					tp:   TemplateParams{TemplateName: "worker"},
				},
			},
			outcome: "success",
		}
		refreshed, ok, _, _ := refreshAsyncStartResult(result, store, ioDiscard{})
		if !ok {
			t.Fatal("refreshAsyncStartResult ok=false for a valid session bead; want proceed")
		}
		if refreshed.prepared.candidate.info.ID != bead.ID {
			t.Errorf("candidate.info.ID = %q, want %q (twin not refreshed from the front-door read)", refreshed.prepared.candidate.info.ID, bead.ID)
		}
		if refreshed.prepared.candidate.info.InstanceToken != "tok-1" {
			t.Errorf("candidate.info.InstanceToken = %q, want tok-1 (twin not coherent with the fresh bead)", refreshed.prepared.candidate.info.InstanceToken)
		}
	})
}
