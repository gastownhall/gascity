package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// restartSessionAtNewEpoch drives the controller's restart path for a session:
// a pending continuation reset (what restart_requested becomes) consumed by
// preWakeCommit, which bumps continuation_epoch, then the new incarnation
// awake. It returns the new epoch.
func restartSessionAtNewEpoch(t *testing.T, store beads.Store, sessionID string) string {
	t.Helper()
	sessFront := sessionFrontDoor(store)
	if err := sessFront.ApplyPatch(sessionID, sessionpkg.MetadataPatch{"continuation_reset_pending": "true"}); err != nil {
		t.Fatalf("ApplyPatch(continuation_reset_pending): %v", err)
	}
	info, _, err := sessFront.GetPersistedResponse(sessionID)
	if err != nil {
		t.Fatalf("GetPersistedResponse: %v", err)
	}
	before := info.ContinuationEpoch
	if _, _, _, err := preWakeCommit(info, sessFront, &clock.Fake{Time: time.Now().UTC()}); err != nil {
		t.Fatalf("preWakeCommit: %v", err)
	}
	// The new incarnation starts and comes up awake, as the Mayor's bead reads
	// after a restart (state=awake, state_reason=creation_complete).
	if err := sessFront.ApplyPatch(sessionID, sessionpkg.MetadataPatch{"state": string(sessionpkg.StateAwake), "state_reason": "creation_complete"}); err != nil {
		t.Fatalf("ApplyPatch(state): %v", err)
	}
	restarted, err := store.Get(sessionID)
	if err != nil {
		t.Fatalf("store.Get(session): %v", err)
	}
	after := restarted.Metadata["continuation_epoch"]
	if after == "" || after == before {
		t.Fatalf("continuation_epoch after restart = %q, want a new epoch after %q", after, before)
	}
	return after
}

// TestNamedSessionRestartKeepsAndDeliversItsWait registers a wait for a
// configured named session, restarts the session at a new continuation epoch,
// closes the dependency and requires the wait to be delivered to the restarted
// session and closed, instead of being canceled as continuation-stale.
func TestNamedSessionRestartKeepsAndDeliversItsWait(t *testing.T) {
	setWaitTestFileBeads(t)
	dir := t.TempDir()
	prevPoller := startNudgePoller
	startNudgePoller = func(string, string, string) error { return nil }
	t.Cleanup(func() { startNudgePoller = prevPoller })

	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)
	info, err := mgr.CreateSession(context.Background(), sessionpkg.CreateOptions{Template: "mayor", Title: "Mayor", Command: "codex", WorkDir: dir, Provider: "codex", Hints: runtime.Config{WorkDir: dir}, ExtraMeta: map[string]string{
		"configured_named_session":  "true",
		"configured_named_identity": "mayor",
		"configured_named_mode":     "always",
	}})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := mgr.Start(context.Background(), testOperatorIn(t, dir), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	dep, err := store.Create(beads.Bead{Title: "review", Type: "task"})
	if err != nil {
		t.Fatalf("create dependency: %v", err)
	}
	sessFront := sessionFrontDoor(store.Store)
	registered, err := sessFront.CreateWait(sessionpkg.WaitSpec{
		SessionID: info.ID,
		Kind:      "deps",
		DepIDs:    []string{dep.ID},
		DepMode:   "all",
		Note:      "Check the review.",
		Now:       time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("CreateWait: %v", err)
	}
	oldEpoch := registered.RegisteredEpoch
	if oldEpoch == "" {
		t.Fatal("registered_epoch is empty")
	}

	newEpoch := restartSessionAtNewEpoch(t, store.Store, info.ID)
	if _, err := prepareWaitWakeState(store.Store, time.Now().UTC()); err != nil {
		t.Fatalf("prepareWaitWakeState(after restart): %v", err)
	}
	rearmed, err := store.Get(registered.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if rearmed.Status == "closed" || rearmed.Metadata["state"] != waitStatePending {
		t.Fatalf("wait after restart: status=%q state=%q last_error=%q, want an open pending wait",
			rearmed.Status, rearmed.Metadata["state"], rearmed.Metadata["last_error"])
	}
	if rearmed.Metadata["registered_epoch"] != newEpoch || rearmed.Metadata["rearmed_from_epoch"] != oldEpoch {
		t.Fatalf("wait epochs: registered=%q rearmed_from=%q, want %q from %q",
			rearmed.Metadata["registered_epoch"], rearmed.Metadata["rearmed_from_epoch"], newEpoch, oldEpoch)
	}

	if err := store.Close(dep.ID); err != nil {
		t.Fatalf("close dependency: %v", err)
	}
	readyWaitSet, err := prepareWaitWakeState(store.Store, time.Now().UTC())
	if err != nil {
		t.Fatalf("prepareWaitWakeState(after close): %v", err)
	}
	if !readyWaitSet[info.ID] {
		t.Fatalf("readyWaitSet = %#v, want session %s ready", readyWaitSet, info.ID)
	}
	ready, err := store.Get(registered.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	nudgeID := waitNudgeID(sessionpkg.WaitInfoFromBead(ready))
	if err := dispatchReadyWaitNudges(dir, store.Store, fake, time.Now().Add(-1*time.Minute).UTC()); err != nil {
		t.Fatalf("dispatchReadyWaitNudges: %v", err)
	}
	if _, ok, err := nudgeFrontDoor(store).Find(nudgeID); err != nil || !ok {
		t.Fatalf("Find(%s) = (%v, %v), want the queued nudge for the new epoch", nudgeID, ok, err)
	}

	idleSince := time.Now().Add(-10 * time.Second)
	fake.Activity = map[string]time.Time{info.SessionName: idleSince}
	target := nudgeTarget{
		cityPath:          dir,
		agent:             config.Agent{Name: "mayor"},
		sessionID:         info.ID,
		continuationEpoch: newEpoch,
		resolved:          &config.ResolvedProvider{Name: "codex"},
		sessionName:       info.SessionName,
	}
	obs := worker.LiveObservation{Running: true, Alive: true, LastActivity: &idleSince}
	delivered, err := tryDeliverQueuedNudgesByPoller(target, store.Store, store.Store, fake, 3*time.Second, obs)
	if err != nil {
		t.Fatalf("tryDeliverQueuedNudgesByPoller: %v", err)
	}
	if !delivered {
		t.Fatal("delivered = false, want the wait's nudge delivered to the restarted session")
	}
	if _, err := prepareWaitWakeState(store.Store, time.Now().UTC()); err != nil {
		t.Fatalf("prepareWaitWakeState(after delivery): %v", err)
	}
	closed, err := store.Get(registered.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if closed.Status != "closed" || closed.Metadata["state"] != waitStateClosed || closed.Metadata["nudge_id"] != nudgeID {
		t.Fatalf("wait after delivery: status=%q state=%q nudge_id=%q, want closed by %s",
			closed.Status, closed.Metadata["state"], closed.Metadata["nudge_id"], nudgeID)
	}
}

// TestPrepareWaitWakeState_RearmsReadyWaitOfRestartedNamedSession covers a
// wait that was already ready when the session restarted: its nudge was fenced
// to the old epoch, so it is re-armed with the nudge id cleared and the next
// dispatch queues a nudge for the new epoch.
func TestPrepareWaitWakeState_RearmsReadyWaitOfRestartedNamedSession(t *testing.T) {
	setWaitTestFileBeads(t)
	dir := t.TempDir()
	store, err := openCityStoreAt(dir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":              "mayor",
			"agent_name":                "mayor",
			"configured_named_session":  "true",
			"configured_named_identity": "mayor",
			"configured_named_mode":     "always",
			"wake_mode":                 "resume",
			"continuation_epoch":        "2",
			"state":                     string(sessionpkg.StateAwake),
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	waitBead, err := store.Create(beads.Bead{
		Type:        waitBeadType,
		Labels:      []string{waitBeadLabel, "session:" + sessionBead.ID},
		Description: "Continue after review closes.",
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "mayor",
			"kind":             "deps",
			"state":            waitStateReady,
			"dep_ids":          "gc-1",
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
			"nudge_id":         "wait-old-1-1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}

	readyWaitSet, err := prepareWaitWakeState(store, time.Now().UTC())
	if err != nil {
		t.Fatalf("prepareWaitWakeState: %v", err)
	}
	if !readyWaitSet[sessionBead.ID] {
		t.Fatalf("readyWaitSet = %#v, want session %s ready", readyWaitSet, sessionBead.ID)
	}
	rearmed, err := store.Get(waitBead.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if rearmed.Status == "closed" || rearmed.Metadata["state"] != waitStateReady {
		t.Fatalf("wait: status=%q state=%q last_error=%q, want an open ready wait",
			rearmed.Status, rearmed.Metadata["state"], rearmed.Metadata["last_error"])
	}
	if rearmed.Metadata["registered_epoch"] != "2" || rearmed.Metadata["nudge_id"] != "" {
		t.Fatalf("wait: registered_epoch=%q nudge_id=%q, want 2 and cleared",
			rearmed.Metadata["registered_epoch"], rearmed.Metadata["nudge_id"])
	}

	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "mayor", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := dispatchReadyWaitNudges(dir, store, sp, time.Now().UTC()); err != nil {
		t.Fatalf("dispatchReadyWaitNudges: %v", err)
	}
	pending, _, _, err := listQueuedNudges(dir, "mayor", time.Now().UTC())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending nudges = %d, want 1", len(pending))
	}
	if want := waitNudgeID(sessionpkg.WaitInfoFromBead(rearmed)); pending[0].ID != want || pending[0].ContinuationEpoch != "2" {
		t.Fatalf("queued nudge id=%q epoch=%q, want %q at epoch 2", pending[0].ID, pending[0].ContinuationEpoch, want)
	}
}

// TestPrepareWaitWakeState_CancelsStaleEpochWaitOfReplacedContinuation keeps
// the continuation-stale cancel for the sessions it exists for: a session that
// is not a configured named session, and a named session with
// wake_mode=fresh that starts a new continuation every cycle by design.
func TestPrepareWaitWakeState_CancelsStaleEpochWaitOfReplacedContinuation(t *testing.T) {
	for name, extra := range map[string]map[string]string{
		"unnamed session": {"wake_mode": "resume"},
		"named fresh-wake session": {
			"configured_named_session":  "true",
			"configured_named_identity": "witness",
			"configured_named_mode":     "always",
			"wake_mode":                 "fresh",
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			meta := map[string]string{
				"session_name":       "worker",
				"agent_name":         "worker",
				"continuation_epoch": "2",
				"state":              string(sessionpkg.StateAwake),
			}
			for key, value := range extra {
				meta[key] = value
			}
			sessionBead, err := store.Create(beads.Bead{Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: meta})
			if err != nil {
				t.Fatalf("create session bead: %v", err)
			}
			waitBead, err := store.Create(beads.Bead{
				Type:   waitBeadType,
				Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
				Metadata: map[string]string{
					"session_id":       sessionBead.ID,
					"session_name":     "worker",
					"kind":             "deps",
					"state":            waitStatePending,
					"dep_ids":          "gc-1",
					"dep_mode":         "all",
					"registered_epoch": "1",
				},
			})
			if err != nil {
				t.Fatalf("create wait bead: %v", err)
			}
			if _, err := prepareWaitWakeState(store, time.Now().UTC()); err != nil {
				t.Fatalf("prepareWaitWakeState: %v", err)
			}
			updated, err := store.Get(waitBead.ID)
			if err != nil {
				t.Fatalf("store.Get(wait): %v", err)
			}
			if updated.Status != "closed" || updated.Metadata["state"] != waitStateCanceled || updated.Metadata["last_error"] != "continuation-stale" {
				t.Fatalf("wait: status=%q state=%q last_error=%q, want closed canceled continuation-stale",
					updated.Status, updated.Metadata["state"], updated.Metadata["last_error"])
			}
		})
	}
}

// TestPrepareWaitWakeState_ClosesDeliveredReadyWaitOfRestartedNamedSession
// covers a ready wait whose reminder the old continuation already received:
// it closes as delivered instead of being re-armed and sent a second time.
func TestPrepareWaitWakeState_ClosesDeliveredReadyWaitOfRestartedNamedSession(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":              "mayor",
			"agent_name":                "mayor",
			"configured_named_session":  "true",
			"configured_named_identity": "mayor",
			"configured_named_mode":     "always",
			"continuation_epoch":        "2",
			"state":                     string(sessionpkg.StateAwake),
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	waitBead, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "mayor",
			"kind":             "deps",
			"state":            waitStateReady,
			"dep_ids":          "gc-1",
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	nudgeID := waitNudgeID(sessionpkg.WaitInfoFromBead(waitBead))
	nudge, err := store.Create(beads.Bead{
		Type:   nudgeBeadType,
		Title:  "nudge:" + nudgeID,
		Labels: []string{nudgeBeadLabel, "nudge:" + nudgeID},
		Metadata: map[string]string{
			"nudge_id": nudgeID,
			"state":    "injected",
		},
	})
	if err != nil {
		t.Fatalf("create nudge bead: %v", err)
	}
	if err := store.Close(nudge.ID); err != nil {
		t.Fatalf("close nudge bead: %v", err)
	}

	if _, err := prepareWaitWakeState(store, time.Now().UTC()); err != nil {
		t.Fatalf("prepareWaitWakeState: %v", err)
	}
	updated, err := store.Get(waitBead.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if updated.Status != "closed" || updated.Metadata["state"] != waitStateClosed || updated.Metadata["nudge_id"] != nudgeID {
		t.Fatalf("wait: status=%q state=%q nudge_id=%q, want closed by %s",
			updated.Status, updated.Metadata["state"], updated.Metadata["nudge_id"], nudgeID)
	}
	if updated.Metadata["registered_epoch"] != "1" {
		t.Fatalf("registered_epoch = %q, want 1 (not re-armed)", updated.Metadata["registered_epoch"])
	}
}
