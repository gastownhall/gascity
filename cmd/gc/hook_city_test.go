package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// countHookCityStoreOpens counts the city-store opens behind the hook's session
// front door.
func countHookCityStoreOpens(t *testing.T) *atomic.Int64 {
	t.Helper()
	var opens atomic.Int64
	prev := hookCityStoreOpener
	hookCityStoreOpener = func(cityPath string, cfg *config.City) (beads.Store, error) {
		opens.Add(1)
		return prev(cityPath, cfg)
	}
	t.Cleanup(func() { hookCityStoreOpener = prev })
	return &opens
}

// TestHookClaimOpensTheSessionStoreOnce pins that the session fence and the
// drain-pending probe read through ONE open of the city store. Each used to open
// its own (~0.5 s apiece on maintainer-city with the config reload), and the
// work-dir lookup and the current-claim stamp of a claim that finds work opened
// two more.
func TestHookClaimOpensTheSessionStoreOnce(t *testing.T) {
	hookClaimNoWorkCity(t)
	opens := countHookCityStoreOpens(t)
	runHookClaimNoWork(t)
	if got := opens.Load(); got != 1 {
		t.Fatalf("gc hook --claim opened the city store %d times for its session reads, want 1", got)
	}
}

// TestHookClaimWithWorkOpensTheSessionStoreOnce pins the claim that FINDS work:
// the drain-pending probe, the session work-dir lookup and the current-claim
// stamp all run, and between them the city store behind the session front door
// is opened once. Each used to resolve the city, reload its config and open the
// store on its own (~0.5 s apiece on maintainer-city).
func TestHookClaimWithWorkOpensTheSessionStoreOnce(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	const (
		beadID    = "ga-ready1"
		sessionID = "gcg-session-557fc1017792caa9a01355325b212416"
	)
	legacyAssignedBeadHookCity(t, beadID, sessionID, "open", true)
	t.Setenv("GC_TEMPLATE", "builder")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", sessionID)
	t.Setenv("BEADS_ACTOR", sessionID)
	t.Setenv("GC_SESSION_NAME", "builder-1")
	t.Setenv("GC_SESSION_ID", sessionID)
	opens := countHookCityStoreOpens(t)

	var stdout, stderr bytes.Buffer
	code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON (code %d): %v\nraw: %s\nstderr: %s", code, err, stdout.String(), stderr.String())
	}
	if result.Action != "work" || result.BeadID != beadID {
		t.Fatalf("result = %+v (code %d), want the claim of %s; stderr: %s", result, code, beadID, stderr.String())
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("a claim with work opened the city store %d times for its session reads and writes, want 1; stderr: %s", got, stderr.String())
	}
}

// TestHookCitySeamsShareOneSessionFrontDoor pins the claim's session seams to
// the invocation's one session front door: the drain-pending probe, the session
// work-dir lookup and the current-claim stamp each answer through it, and only
// the first of them opens the store.
func TestHookCitySeamsShareOneSessionFrontDoor(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeFenceTestCity(t)
	sessionID := newFenceSessionBead(t, cityDir, session.StateActive, "live-token")
	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}
	opens := countHookCityStoreOpens(t)
	ops := newHookCity(cityDir, cfg).claimOps()

	if pending, err := ops.DrainPending(sessionID); err != nil || pending {
		t.Fatalf("DrainPending = %v, %v; want false for an active session", pending, err)
	}
	_ = ops.ResolveSessionWorkDir(sessionID)
	if err := ops.StampSessionClaim(sessionID, "gc-claimed"); err != nil {
		t.Fatalf("StampSessionClaim: %v", err)
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("three session seams opened the city store %d times, want 1", got)
	}
	front, err := cliSessionFrontDoor(mustOpenCityStore(t, cityDir), cfg, cityDir).Get(sessionID)
	if err != nil {
		t.Fatalf("reread session: %v", err)
	}
	if got := front.CurrentClaimBeadID; got != "gc-claimed" {
		t.Fatalf("session current claim = %q, want the stamp written through the shared front door", got)
	}
}

// TestHookCityEventsGoToTheInvocationsCity pins that a claim's events are
// recorded to the city the invocation resolved, without resolving it again: the
// recorder used to re-run the whole city resolution for every event.
func TestHookCityEventsGoToTheInvocationsCity(t *testing.T) {
	clearGCEnv(t)
	cityDir := writeFenceTestCity(t)
	elsewhere := t.TempDir()
	t.Chdir(elsewhere) // nothing to resolve from here
	ops := newHookCity(cityDir, &config.City{}).claimOps()

	ops.EmitClaimRejected("gc-1", "holder", "claimer")
	ops.EmitClaimReleased(hookClaimReleaseRecord{BeadID: "gc-2", Assignee: "claimer", Reason: "test"})
	ops.EmitClaimFailed("gc-3", hookClaimOptions{Assignee: "claimer", Env: []string{"GC_TEMPLATE=pool", "GC_SESSION_ID=gcg-session-1"}}, errors.New("claim failed"))

	data, err := os.ReadFile(filepath.Join(cityDir, ".gc", "events.jsonl"))
	if err != nil {
		t.Fatalf("read the invocation city's event log: %v", err)
	}
	for _, want := range []string{"gc-1", "gc-2", `"type":"bead.claim_failed"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("event log %s does not record %s:\n%s", cityDir, want, data)
		}
	}
}

func mustOpenCityStore(t *testing.T, cityDir string) beads.Store {
	t.Helper()
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	return store
}

// TestHookCityWorkStartedSeamsShareTheFrontDoor pins the work_started seams
// (PHASE1-2 W1) to the invocation's one session front door and city: the read,
// the started write and its removal open the city store once between them,
// the store ref is read off the loaded config, and work.started_unrecorded
// goes to the invocation's event log, without resolving the city again.
func TestHookCityWorkStartedSeamsShareTheFrontDoor(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeFenceTestCity(t)
	sessionID := newFenceSessionBead(t, cityDir, session.StateActive, "live-token")
	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}
	t.Chdir(t.TempDir()) // nothing to resolve from here
	opens := countHookCityStoreOpens(t)
	ops := newHookCity(cityDir, cfg).claimOps()

	if raw, err := ops.ReadWorkStarted(sessionID); err != nil || raw != "" {
		t.Fatalf("ReadWorkStarted = %q, %v; want an empty cell", raw, err)
	}
	key := hookStartedKey{Store: ops.StoreRef(cityDir), Bead: "gc-claimed"}
	_, _ = ops.RecordWorkStarted(sessionID, hookStartedFence{Token: "live-token"}, key, "claim") // the file store may not fence: either way, no second open
	_ = ops.ForgetWorkStarted(sessionID, key)
	if got := opens.Load(); got != 1 {
		t.Fatalf("the work_started seams opened the city store %d times, want 1", got)
	}
	if key.Store != "city" {
		t.Fatalf("store ref of the city dir = %q, want city", key.Store)
	}
	ops.EmitWorkStartedUnrecorded(sessionID, "worker", "gc-claimed", errors.New("test cause"))
	data, err := os.ReadFile(filepath.Join(cityDir, ".gc", "events.jsonl"))
	if err != nil || !strings.Contains(string(data), "work.started_unrecorded") {
		t.Fatalf("event log %s, %v: want work.started_unrecorded recorded in the invocation's city", data, err)
	}
}

// Kills a session-trigger seam that reads anything but the invocation's own
// session row: it returns the row's gc.trigger_bead_id and
// gc.trigger_bead_store_ref stamps, and a row it cannot read is an error,
// never an empty stamp (gastownhall/gascity#5738).
func TestHookCitySessionTriggerReadsTheRowStamp(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeFenceTestCity(t)
	sessionID := newFenceSessionBead(t, cityDir, session.StateActive, "live-token")
	if err := openCitySessionStoreForTest(t, cityDir).SetMetadata(sessionID, beadmeta.TriggerBeadIDMetadataKey, "wb-1"); err != nil {
		t.Fatalf("stamp session row: %v", err)
	}
	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}
	t.Chdir(t.TempDir()) // nothing to resolve from here
	ops := newHookCity(cityDir, cfg).claimOps()

	got, err := ops.ReadSessionTrigger(sessionID)
	if err != nil || got.ID != "wb-1" {
		t.Fatalf("ReadSessionTrigger = %+v, %v; want the row stamp wb-1", got, err)
	}
	if got.StoreRef != "" {
		t.Fatalf("ReadSessionTrigger StoreRef = %q; want empty: the row carries no store stamp", got.StoreRef)
	}
	if got, err := ops.ReadSessionTrigger("gc-no-such-session"); err == nil {
		t.Fatalf("ReadSessionTrigger of a missing row = %+v, nil; want an error", got)
	}
}

// gastownhall/gascity#5858, through the production hookCity wiring: a
// demand-spawned rig-scoped seat whose session row stamps the trigger in the
// city store, where the row is still open and claimable while the seat's own
// leg would call it closed. The drain result is the plain no_work one, the
// event names the counting store's open copy, and the worker's stderr names the
// row and the store.
func TestHookCityDivergenceReadsTheTriggerInTheStoreTheControllerCountedIt(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeFenceTestCityAgents(t, `
[[rigs]]
name = "fed-canary"
path = "rigs/fed-canary"

[[agent]]
name = "worker"
dir = "fed-canary"
`)
	sessionID := newFenceSessionBead(t, cityDir, session.StateActive, "live-token")
	store := openCitySessionStoreForTest(t, cityDir)
	if err := store.SetMetadata(sessionID, beadmeta.TriggerBeadIDMetadataKey, "canary-8a1"); err != nil {
		t.Fatalf("stamp trigger id: %v", err)
	}
	if err := store.SetMetadata(sessionID, beadmeta.TriggerBeadStoreRefMetadataKey, "city"); err != nil {
		t.Fatalf("stamp trigger store ref: %v", err)
	}
	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}
	t.Chdir(t.TempDir()) // nothing to resolve from here

	// The demand-trigger resolver builds its leg through the fan-out's env
	// seam; the classification reads by id, so the env contents are not under
	// test here — pin the leg's dir, not a real bd context.
	prevQueryEnv := hookStoreQueryEnv
	hookStoreQueryEnv = func(_ string, _ *config.City, a *config.Agent) (map[string]string, error) {
		return map[string]string{"GC_STORE": "leg-" + a.Dir}, nil
	}
	t.Cleanup(func() { hookStoreQueryEnv = prevQueryEnv })

	rec := events.NewFake()
	prevRecorder := demandDivergenceRecorder
	demandDivergenceRecorder = func(io.Writer) events.Recorder { return rec }
	t.Cleanup(func() { demandDivergenceRecorder = prevRecorder })

	countedInCity := beads.Bead{
		ID: "canary-8a1", Status: "open", Type: "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "fed-canary/worker"},
	}
	closedElsewhere := countedInCity
	closedElsewhere.Status = "closed"
	ops := newHookCity(cityDir, cfg).claimOps()
	ops.ReadWorkMeta = func(_ context.Context, dir string, _ []string, beadID, _ string) (beads.Bead, error) {
		if beadID != countedInCity.ID {
			return beads.Bead{}, fmt.Errorf("read %s: not the trigger", beadID)
		}
		if dir == cityDir {
			return countedInCity, nil
		}
		return closedElsewhere, nil
	}
	ops.ConfirmBlocked = func(context.Context, string, []string, string, string) (bool, error) {
		return false, nil
	}

	opts := hookClaimOptions{
		Assignee:     "gc__worker-1",
		RouteTargets: []string{"fed-canary/worker"},
		Env: []string{
			"GC_SESSION_ID=" + sessionID,
			"GC_SESSION_NAME=gc__worker-1",
			"GC_TEMPLATE=fed-canary/worker",
			"GC_SPAWN_ORIGIN=demand",
		},
		DrainAck: false,
		JSON:     true,
	}
	var stdout, stderr bytes.Buffer
	code := writeHookClaimNoWork(opts, ops, false, filepath.Join(cityDir, "rigs", "fed-canary"), &stdout, &stderr)

	if code != 1 {
		t.Fatalf("drain exit = %d, want 1 (an unacknowledged drain); stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"action":"drain"`) || !strings.Contains(stdout.String(), `"reason":"no_work"`) {
		t.Fatalf("stdout = %q, want the plain no_work drain result", stdout.String())
	}
	if len(rec.Events) != 1 {
		t.Fatalf("events = %+v, want exactly 1", rec.Events)
	}
	var payload events.SessionDemandClaimDivergencePayload
	if err := json.Unmarshal(rec.Events[0].Payload, &payload); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	if payload.TriggerBeadID != "canary-8a1" || payload.TriggerStatusAtDrain != "open" || payload.Classification != events.DemandClaimDivergence {
		t.Fatalf("payload = %+v, want divergence over the open counting-store copy of canary-8a1", payload)
	}
	if !strings.Contains(stderr.String(), "canary-8a1") || !strings.Contains(stderr.String(), "city") {
		t.Fatalf("stderr = %q, want the row and the counting store named", stderr.String())
	}
}

// AC-6 for gastownhall/gascity#5858: sessionTriggerBeadVia, unit-level. Both
// demand stamps — the trigger bead id and the store it was counted in — come
// off the ONE row read the front door already makes, trimmed, with an empty
// store ref for an id-only row, a zero trigger for an unstamped one, and an
// error for a row the seat cannot read (never a silent empty stamp that would
// pass for "no trigger recorded").
func TestSessionTriggerBeadViaReadsBothStampsOffOneRow(t *testing.T) {
	newFront := func(id, triggerID, storeRef string) sessionFrontGetter {
		meta := map[string]string{"state": string(session.StateActive)}
		if triggerID != "" {
			meta[beadmeta.TriggerBeadIDMetadataKey] = triggerID
		}
		if storeRef != "" {
			meta[beadmeta.TriggerBeadStoreRefMetadataKey] = storeRef
		}
		bead := beads.Bead{
			ID: id, Type: session.BeadType, Status: "open", Title: "session",
			Labels: []string{session.LabelSession}, Metadata: meta,
		}
		store := session.NewStore(beads.SessionStore{Store: beads.NewMemStoreFrom(1, []beads.Bead{bead}, nil)})
		return func() (*session.Store, error) { return store, nil }
	}

	t.Run("both stamps off one row, trimmed", func(t *testing.T) {
		got, err := sessionTriggerBeadVia(newFront("mc-sess1", " wb-1 ", " city:test-city "), " mc-sess1 ")
		if err != nil {
			t.Fatalf("sessionTriggerBeadVia: %v", err)
		}
		if got != (demandTrigger{ID: "wb-1", StoreRef: "city:test-city"}) {
			t.Fatalf("got %+v, want both stamps trimmed off the one row", got)
		}
	})
	t.Run("an id-only row carries no store stamp", func(t *testing.T) {
		got, err := sessionTriggerBeadVia(newFront("mc-sess1", "wb-1", ""), "mc-sess1")
		if err != nil {
			t.Fatalf("sessionTriggerBeadVia: %v", err)
		}
		if got.ID != "wb-1" || got.StoreRef != "" {
			t.Fatalf("got %+v, want the id and an empty store ref", got)
		}
	})
	t.Run("an unstamped row reads as zero, not an error", func(t *testing.T) {
		got, err := sessionTriggerBeadVia(newFront("mc-sess1", "", ""), "mc-sess1")
		if err != nil {
			t.Fatalf("sessionTriggerBeadVia: %v", err)
		}
		if got != (demandTrigger{}) {
			t.Fatalf("got %+v, want a zero trigger for an unstamped row", got)
		}
	})
	t.Run("a row the seat cannot read is an error, never a silent empty stamp", func(t *testing.T) {
		got, err := sessionTriggerBeadVia(func() (*session.Store, error) {
			return nil, errors.New("front door failed")
		}, "mc-sess1")
		if err == nil {
			t.Fatalf("got %+v, nil; want an error", got)
		}
	})
	t.Run("a missing row is an error", func(t *testing.T) {
		got, err := sessionTriggerBeadVia(newFront("mc-sess1", "wb-1", "city"), "mc-sess2")
		if err == nil {
			t.Fatalf("got %+v, nil; want an error for a row that does not exist", got)
		}
	})
}
