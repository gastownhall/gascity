package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
// session row: it returns the row's gc.trigger_bead_id stamp, and a row it
// cannot read is an error, never an empty stamp (gastownhall/gascity#5738).
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

	if got, err := ops.ReadSessionTrigger(sessionID); err != nil || got != (demandTrigger{ID: "wb-1"}) {
		t.Fatalf("ReadSessionTrigger = %+v, %v; want the row stamp wb-1 and no store ref", got, err)
	}
	if got, err := ops.ReadSessionTrigger("gc-no-such-session"); err == nil {
		t.Fatalf("ReadSessionTrigger of a missing row = %+v, nil; want an error", got)
	}
}

// Kills a session-trigger seam that drops the store the controller counted the
// trigger in: a row stamped with both gc.trigger_bead_id and
// gc.trigger_bead_store_ref hands back both, from that one row read
// (gastownhall/gascity#5858).
func TestHookCitySessionTriggerCarriesTheCountingStoreRef(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeFenceTestCity(t)
	sessionID := newFenceSessionBead(t, cityDir, session.StateActive, "live-token")
	sessions := openCitySessionStoreForTest(t, cityDir)
	if err := sessions.SetMetadata(sessionID, beadmeta.TriggerBeadIDMetadataKey, "canary-8a1"); err != nil {
		t.Fatalf("stamp trigger id: %v", err)
	}
	if err := sessions.SetMetadata(sessionID, beadmeta.TriggerBeadStoreRefMetadataKey, " city "); err != nil {
		t.Fatalf("stamp trigger store ref: %v", err)
	}
	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}
	t.Chdir(t.TempDir()) // nothing to resolve from here
	opens := countHookCityStoreOpens(t)
	ops := newHookCity(cityDir, cfg).claimOps()

	got, err := ops.ReadSessionTrigger(sessionID)
	if err != nil || got != (demandTrigger{ID: "canary-8a1", StoreRef: "city"}) {
		t.Fatalf("ReadSessionTrigger = %+v, %v; want canary-8a1 counted in city", got, err)
	}
	if n := opens.Load(); n != 1 {
		t.Fatalf("the trigger read opened the city store %d times, want 1", n)
	}
}

// gastownhall/gascity#5858 through the production hook-city wiring: the
// controller counted canary-8a1 in the CITY store (open, unassigned,
// route-matching, unblocked), but the rig-scoped seat drained in its rig leg,
// where the same id is closed. The seat's session row stamps where the row was
// counted, and the divergence diagnostics must read it there: the verdict is
// divergence, named on stderr with the store, and the drain result is the
// plain no_work drain a seat without the diagnostics writes.
func TestHookCityDivergenceReadsTheTriggerInTheStoreTheControllerCountedIt(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeFenceTestCityAgents(t, `[[rigs]]
name = "fed-canary"
path = "rigs/fed-canary"

[[agent]]
name = "worker"
dir = "fed-canary"
`)
	sessionID := newFenceSessionBead(t, cityDir, session.StateActive, "live-token")
	sessions := openCitySessionStoreForTest(t, cityDir)
	if err := sessions.SetMetadata(sessionID, beadmeta.TriggerBeadIDMetadataKey, "canary-8a1"); err != nil {
		t.Fatalf("stamp trigger id: %v", err)
	}
	if err := sessions.SetMetadata(sessionID, beadmeta.TriggerBeadStoreRefMetadataKey, "city"); err != nil {
		t.Fatalf("stamp trigger store ref: %v", err)
	}
	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}
	rec := events.NewFake()
	prev := demandDivergenceRecorder
	demandDivergenceRecorder = func(io.Writer) events.Recorder { return rec }
	t.Cleanup(func() { demandDivergenceRecorder = prev })

	ops := newHookCity(cityDir, cfg).claimOps()
	var reads []string
	ops.ReadWorkMeta = func(_ context.Context, dir string, _ []string, beadID, _ string) (beads.Bead, error) {
		reads = append(reads, dir)
		bead := beads.Bead{
			ID: beadID, Status: "closed", Type: "task",
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "fed-canary/worker"},
		}
		if filepath.Clean(dir) == filepath.Clean(cityDir) {
			bead.Status = "open"
		}
		return bead, nil
	}
	ops.ConfirmBlocked = func(context.Context, string, []string, string, string) (bool, error) {
		return false, nil
	}
	seatDir := filepath.Join(cityDir, "rigs", "fed-canary")
	options := func(env ...string) hookClaimOptions {
		return hookClaimOptions{
			Assignee:           sessionID,
			IdentityCandidates: []string{sessionID},
			RouteTargets:       []string{"fed-canary/worker"},
			Env:                append([]string{"GC_SESSION_ID=" + sessionID, "GC_TEMPLATE=fed-canary/worker"}, env...),
		}
	}

	var baseOut, baseErr bytes.Buffer
	baseCode := writeHookClaimNoWork(options(), ops, false, seatDir, &baseOut, &baseErr)
	var stdout, stderr bytes.Buffer
	code := writeHookClaimNoWork(options("GC_SPAWN_ORIGIN=demand"), ops, false, seatDir, &stdout, &stderr)

	if code != baseCode || stdout.String() != baseOut.String() {
		t.Fatalf("drain = %d %q, want the plain no_work drain %d %q", code, stdout.String(), baseCode, baseOut.String())
	}
	if len(rec.Events) != 1 {
		t.Fatalf("events = %+v, want exactly 1", rec.Events)
	}
	var payload events.SessionDemandClaimDivergencePayload
	if err := json.Unmarshal(rec.Events[0].Payload, &payload); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	if payload.TriggerBeadID != "canary-8a1" || payload.TriggerStatusAtDrain != "open" || payload.Classification != events.DemandClaimDivergence {
		t.Fatalf("payload = %+v (trigger reads in %q), want a divergence over canary-8a1, open in the city store it was counted in", payload, reads)
	}
	if line := stderr.String(); !strings.Contains(line, "canary-8a1") || !strings.Contains(line, "city") {
		t.Fatalf("stderr = %q, want the divergence line naming canary-8a1 and the city store", line)
	}
}
