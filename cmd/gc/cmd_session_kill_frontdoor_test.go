package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// TestCmdSessionKill_ForeignAndMissingRejectedAtResolutionWithoutWrite pins the
// observable contract around the WI-7 W-flip of cmdSessionKill's session read
// (raw sessStore.Get + codec → sessionFrontDoor(sessStore).Get → Info).
//
// The front-door Get is STRICTER than the old raw Get: it rejects a
// present-but-non-session bead (ErrSessionNotFound) and wraps absence. The flip
// preserves best-effort kill by construction — an Info-read error only leaves
// identity empty and proceeds; it adds no early return before handle.Kill.
//
// Crucially, the infoErr != nil branch is UNREACHABLE end-to-end via
// cmdSessionKill: resolveSessionIDWithConfig runs first and rejects any target
// that is not a session bead (same IsSessionBeadOrRepairable predicate the
// front-door Get uses), and even if a target slipped past resolution,
// workerHandleForSessionWithConfig reads the same store and fails identically
// before handle.Kill. So a foreign / missing target exits 1 at resolution — it
// never reaches the Get or the kill. This test locks that reachable contract,
// and in particular that a present FOREIGN bead is left completely UNWRITTEN
// (no session sleep metadata is stamped onto a non-session bead) — the
// design-sanctioned property of routing the read through the session front door.
//
// (Two mutation experiments confirm the branch analysis: adding
// `if infoErr != nil { return 1 }` after the Get keeps the whole TestCmdSessionKill
// suite green — the branch is dead end-to-end; while breaking the front-door
// identity read (namedSessionIdentityInfo(info)) fails
// TestCmdSessionKill_ClearsCircuitBreaker — the reachable healthy path IS pinned.)
func TestCmdSessionKill_ForeignAndMissingRejectedAtResolutionWithoutWrite(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := shortSocketTempDir(t, "gc-kill-frontdoor-")
	t.Setenv("GC_CITY", cityDir)
	writeGenericNamedSessionCityTOML(t, cityDir)

	fakeProvider := runtime.NewFake()
	oldBuild := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return fakeProvider, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}

	// A present, FOREIGN bead: not a session bead (type task, no gc:session label).
	// Wire a fake runtime under its would-be session name so that IF the kill flow
	// ever advanced past resolution it COULD reach a live handle — making the
	// "rejected at resolution, nothing written" assertion meaningful.
	foreign, err := store.Create(beads.Bead{
		Title:    "foreign",
		Type:     "task",
		Metadata: map[string]string{"session_name": "s-foreign", "state": "awake"},
	})
	if err != nil {
		t.Fatalf("store.Create(foreign): %v", err)
	}
	if err := fakeProvider.Start(context.Background(), "s-foreign", runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("fakeProvider.Start: %v", err)
	}
	if err := fakeProvider.SetMeta("s-foreign", "GC_SESSION_ID", foreign.ID); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}

	t.Run("foreign bead rejected at resolution, left unwritten", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := cmdSessionKill([]string{foreign.ID}, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("cmdSessionKill(foreign) = %d, want 1 (rejected at resolution); stderr=%s", code, stderr.String())
		}
		got, err := store.Get(foreign.ID)
		if err != nil {
			t.Fatalf("re-Get(foreign): %v", err)
		}
		// The foreign bead must be untouched: no session sleep metadata stamped on
		// a non-session bead. state stays its original "awake"; the kill's asleep
		// sync (SleepPatch: state/sleep_reason/synced_at) never fires.
		if got.Metadata["state"] != "awake" {
			t.Errorf("foreign bead state = %q, want unchanged \"awake\" (no SleepPatch on a non-session bead)", got.Metadata["state"])
		}
		if got.Metadata["synced_at"] != "" {
			t.Errorf("foreign bead synced_at = %q, want empty (no asleep sync written)", got.Metadata["synced_at"])
		}
		if got.Metadata["sleep_reason"] != "" {
			t.Errorf("foreign bead sleep_reason = %q, want empty (no asleep sync written)", got.Metadata["sleep_reason"])
		}
	})

	t.Run("missing id rejected at resolution", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := cmdSessionKill([]string{"ga-does-not-exist"}, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("cmdSessionKill(missing) = %d, want 1 (session not found); stderr=%s", code, stderr.String())
		}
	})
}

// TestSyncKilledSessionAsleepClearsLocalLastWokeAtNotJustDurable pins that
// syncKilledSessionAsleep — cmdSessionKill's post-kill asleep sync, extracted
// so it can be exercised directly — clears last_woke_at through the session
// front door (sessFront.ApplyPatch), not a raw sessStore.SetMetadataBatch
// call, so a stale non-empty local sidecar value left by the migrated wake
// path cannot survive kill and mask the clear from the front door's
// local-overlay projection (ga-igcny0.1.2.1 Phase B finding 1; see
// info_store.go's projectWithLocalOverlay). A raw-store clear only writes
// durable metadata; the local overlay would still prefer the stale
// non-empty local value and hide the clear from crash/churn trackers.
//
// This drives the helper directly against a single shared store instance
// rather than going through the full cmdSessionKill CLI wrapper: for the
// file/MemStore provider, local sidecar state (SetLocalString/GetLocalString)
// is pure in-process memory with no cross-instance persistence, so a
// CLI-level test — which would need cmdSessionKill to open its own store
// internally via openCityStore — could never observe a local seed written
// through a separately-opened test store instance. Testing the extracted
// write against one store used for both seed and assertion is the only way
// to pin this regression for this provider.
func TestSyncKilledSessionAsleepClearsLocalLastWokeAtNotJustDurable(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "named session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "s-sync-killed-lwa-test",
			"state":        string(session.StateAsleep),
			"last_woke_at": "2026-04-10T12:00:00Z",
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}

	sessFront := sessionFrontDoor(store)
	if err := sessFront.SetLocalString(bead.ID, "last_woke_at", "2026-04-10T12:00:00Z"); err != nil {
		t.Fatalf("SetLocalString(last_woke_at seed): %v", err)
	}

	if err := syncKilledSessionAsleep(sessFront, bead.ID, time.Now().UTC()); err != nil {
		t.Fatalf("syncKilledSessionAsleep: %v", err)
	}

	info, err := sessFront.Get(bead.ID)
	if err != nil {
		t.Fatalf("sessFront.Get(post-sync): %v", err)
	}
	if info.LastWokeAt != "" {
		t.Errorf("LastWokeAt = %q, want cleared (local sidecar must not mask the durable clear)", info.LastWokeAt)
	}

	raw, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get(post-sync, raw durable): %v", err)
	}
	if raw.Metadata["last_woke_at"] != "" {
		t.Errorf("durable last_woke_at = %q, want cleared", raw.Metadata["last_woke_at"])
	}
}
