package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
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
