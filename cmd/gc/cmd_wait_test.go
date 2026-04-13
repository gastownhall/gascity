package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

type waitErrorStore struct {
	*beads.MemStore
}

func (s waitErrorStore) ListByLabel(label string, limit int, _ ...beads.QueryOpt) ([]beads.Bead, error) {
	if label == waitBeadLabel {
		return nil, errors.New("wait list failed")
	}
	return s.MemStore.ListByLabel(label, limit)
}

func (s waitErrorStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == waitBeadLabel {
		return nil, errors.New("wait list failed")
	}
	return s.MemStore.List(query)
}

func TestPrepareWaitWakeState_MarksDepsReady(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
			"provider":           "codex",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	dep, err := store.Create(beads.Bead{Title: "dep"})
	if err != nil {
		t.Fatalf("create dep bead: %v", err)
	}
	if err := store.Close(dep.ID); err != nil {
		t.Fatalf("close dep bead: %v", err)
	}
	waitBead, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStatePending,
			"dep_ids":          dep.ID,
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}

	readyWaitSet, err := prepareWaitWakeState(store, nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("prepareWaitWakeState: %v", err)
	}
	if !readyWaitSet[sessionBead.ID] {
		t.Fatalf("readyWaitSet missing session %s", sessionBead.ID)
	}
	updated, err := store.Get(waitBead.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if got := updated.Metadata["state"]; got != waitStateReady {
		t.Fatalf("wait state = %q, want %q", got, waitStateReady)
	}
	if updated.Metadata["ready_at"] == "" {
		t.Fatal("ready_at was not recorded")
	}
}

func TestPrepareWaitWakeState_FailsMissingDependencyWait(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
			"wait_hold":          "true",
			"sleep_reason":       "wait-hold",
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
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStatePending,
			"dep_ids":          "gc-missing",
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}

	readyWaitSet, err := prepareWaitWakeState(store, nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("prepareWaitWakeState: %v", err)
	}
	if readyWaitSet[sessionBead.ID] {
		t.Fatalf("readyWaitSet unexpectedly contains session %s", sessionBead.ID)
	}

	updatedWait, err := store.Get(waitBead.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if got := updatedWait.Metadata["state"]; got != waitStateFailed {
		t.Fatalf("wait state = %q, want %q", got, waitStateFailed)
	}
	if updatedWait.Status != "closed" {
		t.Fatalf("wait status = %q, want closed", updatedWait.Status)
	}
	if updatedWait.Metadata["failed_at"] == "" {
		t.Fatal("failed_at was not recorded")
	}
	if updatedWait.Metadata["last_error"] == "" {
		t.Fatal("last_error was not recorded")
	}

	updatedSession, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("store.Get(session): %v", err)
	}
	if updatedSession.Metadata["wait_hold"] != "" {
		t.Fatalf("wait_hold = %q, want cleared", updatedSession.Metadata["wait_hold"])
	}
	if updatedSession.Metadata["sleep_reason"] != "" {
		t.Fatalf("sleep_reason = %q, want cleared", updatedSession.Metadata["sleep_reason"])
	}
}

func TestPrepareWaitWakeState_FinalizesFromNudge(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
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
			"session_name":     "worker",
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
	nudgeID := waitNudgeID(waitBead)
	nudge, err := store.Create(beads.Bead{
		Type:   nudgeBeadType,
		Title:  "nudge:" + nudgeID,
		Labels: []string{nudgeBeadLabel, "nudge:" + nudgeID},
		Metadata: map[string]string{
			"nudge_id":           nudgeID,
			"state":              "injected",
			"commit_boundary":    "provider-nudge-return",
			"terminal_reason":    "",
			"continuation_epoch": "1",
		},
	})
	if err != nil {
		t.Fatalf("create nudge bead: %v", err)
	}
	if err := store.Close(nudge.ID); err != nil {
		t.Fatalf("close nudge bead: %v", err)
	}

	readyWaitSet, err := prepareWaitWakeState(store, nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("prepareWaitWakeState: %v", err)
	}
	if readyWaitSet[sessionBead.ID] {
		t.Fatalf("session %s should not remain in ready set after terminal nudge", sessionBead.ID)
	}
	updated, err := store.Get(waitBead.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if got := updated.Metadata["state"]; got != waitStateClosed {
		t.Fatalf("wait state = %q, want %q", got, waitStateClosed)
	}
	if updated.Status != "closed" {
		t.Fatalf("wait status = %q, want closed", updated.Status)
	}
}

func TestDepsWaitReady_IgnoresEmptyDependencyEntries(t *testing.T) {
	store := beads.NewMemStore()
	dep, err := store.Create(beads.Bead{Title: "dep"})
	if err != nil {
		t.Fatalf("create dep bead: %v", err)
	}
	if err := store.Close(dep.ID); err != nil {
		t.Fatalf("close dep bead: %v", err)
	}

	ready := depsWaitReady(store, beads.Bead{
		Metadata: map[string]string{
			"dep_ids":  dep.ID + ", ,",
			"dep_mode": "all",
		},
	})
	if !ready {
		t.Fatal("depsWaitReady = false, want true with only one real closed dependency")
	}
}

func TestDepsWaitReadyDetailed_ResolvesRigBeads(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	// Create a dep bead in the rig store (not the city store).
	dep, err := rigStore.Create(beads.Bead{Title: "rig-task"})
	if err != nil {
		t.Fatalf("create dep bead in rig store: %v", err)
	}
	if err := rigStore.Close(dep.ID); err != nil {
		t.Fatalf("close dep bead: %v", err)
	}

	get := multiStoreGetter(cityStore, map[string]beads.Store{"my-rig": rigStore})

	ready, err := depsWaitReadyDetailed(get, beads.Bead{
		Metadata: map[string]string{
			"dep_ids":  dep.ID,
			"dep_mode": "all",
		},
	})
	if err != nil {
		t.Fatalf("depsWaitReadyDetailed: %v", err)
	}
	if !ready {
		t.Fatal("depsWaitReadyDetailed = false, want true for closed rig bead")
	}
}

func TestDepsWaitReadyDetailed_RigBeadNotClosedStaysPending(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	// Create a dep bead in the rig store but do NOT close it.
	dep, err := rigStore.Create(beads.Bead{Title: "rig-task-pending"})
	if err != nil {
		t.Fatalf("create dep bead in rig store: %v", err)
	}

	get := multiStoreGetter(cityStore, map[string]beads.Store{"my-rig": rigStore})

	ready, err := depsWaitReadyDetailed(get, beads.Bead{
		Metadata: map[string]string{
			"dep_ids":  dep.ID,
			"dep_mode": "all",
		},
	})
	if err != nil {
		t.Fatalf("depsWaitReadyDetailed: %v", err)
	}
	if ready {
		t.Fatal("depsWaitReadyDetailed = true, want false for open rig bead")
	}
}

func TestDepsWaitReadyDetailed_MissingFromAllStores(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	get := multiStoreGetter(cityStore, map[string]beads.Store{"my-rig": rigStore})

	ready, err := depsWaitReadyDetailed(get, beads.Bead{
		Metadata: map[string]string{
			"dep_ids":  "gc-nonexistent",
			"dep_mode": "all",
		},
	})
	if err == nil {
		t.Fatal("expected error for bead missing from all stores")
	}
	if ready {
		t.Fatal("depsWaitReadyDetailed = true, want false for missing bead")
	}
}

func TestPrepareWaitWakeState_ResolvesCrossRigDeps(t *testing.T) {
	cityStore := beads.NewMemStore()
	// Offset rig store sequence to avoid ID collisions with the city store.
	rigStore := beads.NewMemStoreFrom(100, nil, nil)

	// Create session bead in city store.
	sessionBead, err := cityStore.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	// Create dependency bead in rig store and close it.
	dep, err := rigStore.Create(beads.Bead{Title: "rig-work"})
	if err != nil {
		t.Fatalf("create dep bead in rig store: %v", err)
	}
	if err := rigStore.Close(dep.ID); err != nil {
		t.Fatalf("close dep bead: %v", err)
	}

	// Create wait bead in city store referencing the rig dep.
	_, err = cityStore.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStatePending,
			"dep_ids":          dep.ID,
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}

	rigStores := map[string]beads.Store{"my-rig": rigStore}
	readyWaitSet, err := prepareWaitWakeState(cityStore, rigStores, time.Now().UTC())
	if err != nil {
		t.Fatalf("prepareWaitWakeState: %v", err)
	}
	if !readyWaitSet[sessionBead.ID] {
		t.Fatalf("readyWaitSet does not contain session %s — cross-rig dep not resolved", sessionBead.ID)
	}
}

func TestMultiStoreGetter_CityStoreFirst(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	// Create bead in city store.
	cityBead, err := cityStore.Create(beads.Bead{Title: "city-bead"})
	if err != nil {
		t.Fatalf("create city bead: %v", err)
	}

	get := multiStoreGetter(cityStore, map[string]beads.Store{"my-rig": rigStore})
	b, err := get(cityBead.ID)
	if err != nil {
		t.Fatalf("multiStoreGetter: %v", err)
	}
	if b.Title != "city-bead" {
		t.Fatalf("got title %q, want %q", b.Title, "city-bead")
	}
}

// brokenStore wraps a MemStore but returns a non-ErrNotFound error on Get.
type brokenStore struct {
	*beads.MemStore
}

func (s brokenStore) Get(_ string) (beads.Bead, error) {
	return beads.Bead{}, errors.New("store unavailable")
}

func TestMultiStoreGetter_PropagatesCityStoreError(t *testing.T) {
	city := brokenStore{beads.NewMemStore()}
	rig := beads.NewMemStore()

	get := multiStoreGetter(city, map[string]beads.Store{"r": rig})
	_, err := get("gc-1")
	if err == nil {
		t.Fatal("expected error from broken city store")
	}
	if !strings.Contains(err.Error(), "city store") {
		t.Fatalf("error should mention city store, got: %v", err)
	}
}

func TestMultiStoreGetter_PropagatesRigStoreError(t *testing.T) {
	city := beads.NewMemStore()
	rig := brokenStore{beads.NewMemStore()}

	get := multiStoreGetter(city, map[string]beads.Store{"r": rig})
	_, err := get("gc-nonexistent")
	if err == nil {
		t.Fatal("expected error from broken rig store")
	}
	if !strings.Contains(err.Error(), "rig store") {
		t.Fatalf("error should mention rig store, got: %v", err)
	}
}

func TestNextWaitDeliveryAttempt_IncrementsAfterTerminalNudge(t *testing.T) {
	store := beads.NewMemStore()
	wait, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel},
		Metadata: map[string]string{
			"state":            waitStateFailed,
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	nudgeID := waitNudgeID(wait)
	nudge, err := store.Create(beads.Bead{
		Type:   nudgeBeadType,
		Title:  "nudge:" + nudgeID,
		Labels: []string{nudgeBeadLabel, "nudge:" + nudgeID},
		Metadata: map[string]string{
			"nudge_id": nudgeID,
			"state":    "failed",
		},
	})
	if err != nil {
		t.Fatalf("create nudge bead: %v", err)
	}
	if err := store.Close(nudge.ID); err != nil {
		t.Fatalf("close nudge bead: %v", err)
	}

	next, err := nextWaitDeliveryAttempt(store, wait)
	if err != nil {
		t.Fatalf("nextWaitDeliveryAttempt: %v", err)
	}
	if next != "2" {
		t.Fatalf("nextWaitDeliveryAttempt = %q, want 2", next)
	}
}

func TestRetryClosedWait_CreatesReplacement(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"continuation_epoch": "2",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	wait, err := store.Create(beads.Bead{
		Type:        waitBeadType,
		Title:       "wait:worker",
		Description: "Retry me.",
		Labels:      []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStateFailed,
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	nudgeID := waitNudgeID(wait)
	nudge, err := store.Create(beads.Bead{
		Type:   nudgeBeadType,
		Title:  "nudge:" + nudgeID,
		Labels: []string{nudgeBeadLabel, "nudge:" + nudgeID},
		Metadata: map[string]string{
			"nudge_id": nudgeID,
			"state":    "failed",
		},
	})
	if err != nil {
		t.Fatalf("create nudge bead: %v", err)
	}
	if err := store.Close(nudge.ID); err != nil {
		t.Fatalf("close nudge bead: %v", err)
	}
	if err := store.Close(wait.ID); err != nil {
		t.Fatalf("close wait bead: %v", err)
	}

	retried, err := retryClosedWait(store, wait, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("retryClosedWait: %v", err)
	}
	if retried.ID == wait.ID {
		t.Fatal("retryClosedWait reused original wait ID")
	}
	if retried.Type != waitBeadType {
		t.Fatalf("retried type = %q, want %q", retried.Type, waitBeadType)
	}
	if retried.Metadata["state"] != waitStateReady {
		t.Fatalf("retried state = %q, want %q", retried.Metadata["state"], waitStateReady)
	}
	if retried.Metadata["delivery_attempt"] != "2" {
		t.Fatalf("retried attempt = %q, want 2", retried.Metadata["delivery_attempt"])
	}
	if retried.Metadata["registered_epoch"] != "2" {
		t.Fatalf("retried registered_epoch = %q, want 2", retried.Metadata["registered_epoch"])
	}
	if retried.Metadata["retried_from_wait"] != wait.ID {
		t.Fatalf("retried_from_wait = %q, want %q", retried.Metadata["retried_from_wait"], wait.ID)
	}
	if retried.Status == "closed" {
		t.Fatalf("retried wait status = %q, want open", retried.Status)
	}
}

func TestRetryClosedWait_DropsInternalMetadata(t *testing.T) {
	store := beads.NewMemStore()
	wait, err := store.Create(beads.Bead{
		Type:        waitBeadType,
		Title:       "wait:worker",
		Description: "Retry me.",
		Labels:      []string{waitBeadLabel},
		Metadata: map[string]string{
			"session_id":         "gc-session",
			"session_name":       "worker",
			"kind":               "deps",
			"state":              waitStateFailed,
			"dep_ids":            "gc-1",
			"dep_mode":           "all",
			"registered_epoch":   "1",
			"delivery_attempt":   "1",
			"created_by_session": "gc-origin",
			"nudge_id":           "wait-gc-1-1-1",
			"last_error":         "boom",
			"synced_at":          "2026-03-16T10:00:00Z",
			"future_internal":    "should-not-carry",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	if err := store.Close(wait.ID); err != nil {
		t.Fatalf("close wait bead: %v", err)
	}

	retried, err := retryClosedWait(store, wait, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("retryClosedWait: %v", err)
	}
	if retried.Metadata["dep_ids"] != "gc-1" {
		t.Fatalf("dep_ids = %q, want gc-1", retried.Metadata["dep_ids"])
	}
	if retried.Metadata["created_by_session"] != "gc-origin" {
		t.Fatalf("created_by_session = %q, want gc-origin", retried.Metadata["created_by_session"])
	}
	if retried.Metadata["nudge_id"] != "" {
		t.Fatalf("nudge_id = %q, want cleared", retried.Metadata["nudge_id"])
	}
	if retried.Metadata["last_error"] != "" {
		t.Fatalf("last_error = %q, want cleared", retried.Metadata["last_error"])
	}
	if retried.Metadata["synced_at"] != "" {
		t.Fatalf("synced_at = %q, want omitted", retried.Metadata["synced_at"])
	}
	if retried.Metadata["future_internal"] != "" {
		t.Fatalf("future_internal = %q, want omitted", retried.Metadata["future_internal"])
	}
}

func TestRetryClosedWait_PreservesNonDepsMetadata(t *testing.T) {
	store := beads.NewMemStore()
	wait, err := store.Create(beads.Bead{
		Type:        waitBeadType,
		Title:       "wait:worker",
		Description: "Retry me.",
		Labels:      []string{waitBeadLabel},
		Metadata: map[string]string{
			"session_id":       "gc-session",
			"session_name":     "worker",
			"kind":             "probe",
			"state":            waitStateFailed,
			"registered_epoch": "1",
			"delivery_attempt": "1",
			"probe_name":       "github-pr-approval",
			"probe_target":     "owner/repo#123",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	if err := store.Close(wait.ID); err != nil {
		t.Fatalf("close wait bead: %v", err)
	}

	retried, err := retryClosedWait(store, wait, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("retryClosedWait: %v", err)
	}
	if retried.Metadata["kind"] != "probe" {
		t.Fatalf("kind = %q, want probe", retried.Metadata["kind"])
	}
	if retried.Metadata["probe_name"] != "github-pr-approval" {
		t.Fatalf("probe_name = %q, want github-pr-approval", retried.Metadata["probe_name"])
	}
	if retried.Metadata["probe_target"] != "owner/repo#123" {
		t.Fatalf("probe_target = %q, want owner/repo#123", retried.Metadata["probe_target"])
	}
}

func TestDispatchReadyWaitNudges_EnqueuesDeterministicNudge(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store, err := openCityStoreAt(dir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
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
			"session_name":     "worker",
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
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := dispatchReadyWaitNudges(dir, store, sp, time.Now().UTC()); err != nil {
		t.Fatalf("dispatchReadyWaitNudges: %v", err)
	}
	pending, inFlight, dead, err := listQueuedNudges(dir, "worker", time.Now().UTC())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("pending=%d inFlight=%d dead=%d, want 1/0/0", len(pending), len(inFlight), len(dead))
	}
	wantID := waitNudgeID(waitBead)
	if pending[0].ID != wantID {
		t.Fatalf("queued nudge id = %q, want %q", pending[0].ID, wantID)
	}
	if pending[0].SessionID != sessionBead.ID {
		t.Fatalf("queued nudge session_id = %q, want %q", pending[0].SessionID, sessionBead.ID)
	}
	if pending[0].Reference == nil || pending[0].Reference.ID != waitBead.ID {
		t.Fatalf("queued nudge reference = %#v, want wait bead %s", pending[0].Reference, waitBead.ID)
	}
	if pending[0].BeadID == "" {
		t.Fatal("queued nudge bead_id is empty")
	}
	refreshedStore, err := openCityStoreAt(dir)
	if err != nil {
		t.Fatalf("openCityStoreAt(refresh): %v", err)
	}
	if _, err := refreshedStore.Get(pending[0].BeadID); err != nil {
		t.Fatalf("refreshedStore.Get(%s): %v", pending[0].BeadID, err)
	}
}

func TestDispatchReadyWaitNudges_StartsCodexPoller(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store, err := openCityStoreAt(dir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
			"provider":           "codex",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStateReady,
			"dep_ids":          "gc-1",
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	}); err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	called := false
	prev := startNudgePoller
	startNudgePoller = func(cityPath, agentName, sessionName string) error {
		called = true
		if cityPath != dir || agentName != "worker" || sessionName != "worker" {
			t.Fatalf("unexpected poller args city=%q agent=%q session=%q", cityPath, agentName, sessionName)
		}
		return nil
	}
	t.Cleanup(func() { startNudgePoller = prev })

	if err := dispatchReadyWaitNudges(dir, store, sp, time.Now().UTC()); err != nil {
		t.Fatalf("dispatchReadyWaitNudges: %v", err)
	}
	if !called {
		t.Fatal("startNudgePoller was not called")
	}
}

func TestWithdrawQueuedWaitNudges_RemovesQueuedNudge(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	item := newQueuedNudgeWithOptions("worker", "Wait satisfied.", "wait", time.Now().Add(-time.Minute), queuedNudgeOptions{
		ID:        "wait-gc-1-1-1",
		Reference: &nudgeReference{Kind: "bead", ID: "gc-1"},
	})
	if err := enqueueQueuedNudge(dir, item); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	if err := withdrawQueuedWaitNudges(dir, []string{item.ID}); err != nil {
		t.Fatalf("withdrawQueuedWaitNudges: %v", err)
	}

	pending, inFlight, dead, err := listQueuedNudges(dir, "worker", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 0 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("pending=%d inFlight=%d dead=%d, want all zero", len(pending), len(inFlight), len(dead))
	}

	store, err := openCityStoreAt(dir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	nudge, ok, err := findAnyQueuedNudgeBead(store, item.ID)
	if err != nil {
		t.Fatalf("findAnyQueuedNudgeBead: %v", err)
	}
	if !ok {
		t.Fatal("findAnyQueuedNudgeBead returned not found")
	}
	if nudge.Status != "closed" {
		t.Fatalf("nudge status = %q, want closed", nudge.Status)
	}
	if nudge.Metadata["terminal_reason"] != "wait-canceled" {
		t.Fatalf("terminal_reason = %q, want wait-canceled", nudge.Metadata["terminal_reason"])
	}
}

func TestCancelWaitsForSession(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	waitBead, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id": sessionBead.ID,
			"state":      waitStatePending,
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}

	if err := cancelWaitsForSession(store, sessionBead.ID); err != nil {
		t.Fatalf("cancelWaitsForSession: %v", err)
	}
	updated, err := store.Get(waitBead.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if got := updated.Metadata["state"]; got != waitStateCanceled {
		t.Fatalf("wait state = %q, want %q", got, waitStateCanceled)
	}
	if updated.Status != "closed" {
		t.Fatalf("wait status = %q, want closed", updated.Status)
	}
}

func TestLoadSessionWaitBeads_IncludesLegacyWaitType(t *testing.T) {
	store := beads.NewMemStore()
	sessionID := "gc-session"
	if _, err := store.Create(beads.Bead{
		Type:   sessionpkg.LegacyWaitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionID},
		Metadata: map[string]string{
			"session_id": sessionID,
			"state":      waitStatePending,
		},
	}); err != nil {
		t.Fatalf("create legacy wait bead: %v", err)
	}

	waits, err := loadSessionWaitBeads(store, sessionID)
	if err != nil {
		t.Fatalf("loadSessionWaitBeads: %v", err)
	}
	if len(waits) != 1 {
		t.Fatalf("loadSessionWaitBeads returned %d waits, want 1", len(waits))
	}
	if waits[0].Type != sessionpkg.LegacyWaitBeadType {
		t.Fatalf("wait type = %q, want legacy %q", waits[0].Type, sessionpkg.LegacyWaitBeadType)
	}
}

func TestClearSessionWaitHoldIfIdle_PropagatesWaitLoadError(t *testing.T) {
	store := waitErrorStore{MemStore: beads.NewMemStore()}
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"wait_hold":    "true",
			"sleep_intent": "wait-hold",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	if err := clearSessionWaitHoldIfIdle(store, sessionBead.ID); err == nil {
		t.Fatal("expected clearSessionWaitHoldIfIdle to return load error")
	}

	updated, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("store.Get(session): %v", err)
	}
	if updated.Metadata["wait_hold"] != "true" {
		t.Fatalf("wait_hold = %q, want true", updated.Metadata["wait_hold"])
	}
	if updated.Metadata["sleep_intent"] != "wait-hold" {
		t.Fatalf("sleep_intent = %q, want wait-hold", updated.Metadata["sleep_intent"])
	}
}

func TestCmdSessionWait_DoesNotMaterializeTemplateTarget(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	prevCityFlag := cityFlag
	cityFlag = ""
	t.Cleanup(func() {
		cityFlag = prevCityFlag
	})

	cityPath := t.TempDir()
	cityToml := `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", cityPath)

	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	dep, err := store.Create(beads.Bead{Title: "dep"})
	if err != nil {
		t.Fatalf("create dep bead: %v", err)
	}
	if err := store.Close(dep.ID); err != nil {
		t.Fatalf("close dep bead: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdSessionWait([]string{"worker"}, []string{dep.ID}, false, "block", false, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("cmdSessionWait() = 0, want failure; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	sessions, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("ListByLabel(session): %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("session bead count = %d, want 0", len(sessions))
	}
}
