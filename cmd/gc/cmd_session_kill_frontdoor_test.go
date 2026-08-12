package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

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

// TestCmdSessionKillClearsLocalLastWokeAtNotJustDurable pins that
// cmdSessionKill's post-kill asleep sync clears last_woke_at through the
// session front door (sessFront.ApplyPatch), not a raw
// sessStore.SetMetadataBatch call, so a stale non-empty local sidecar value
// left by the migrated wake path cannot survive kill and mask the clear from
// the front door's local-overlay projection (ga-igcny0.1.2.1 Phase B finding
// 1; see info_store.go's projectWithLocalOverlay). A raw-store clear only
// writes durable metadata; the local overlay would still prefer the stale
// non-empty local value and hide the clear from crash/churn trackers.
func TestCmdSessionKillClearsLocalLastWokeAtNotJustDurable(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := shortSocketTempDir(t, "gc-kill-lwa-")
	t.Setenv("GC_CITY", cityDir)
	writeGenericNamedSessionCityTOML(t, cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	const identity = "session-a"
	bead, err := store.Create(beads.Bead{
		Title:  "named session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:worker"},
		Metadata: map[string]string{
			"alias":                      identity,
			"template":                   "worker",
			"session_name":               "s-gc-kill-lwa-test",
			"state":                      string(session.StateAsleep),
			"last_woke_at":               "2026-04-10T12:00:00Z",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: identity,
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}
	sessFront := sessionFrontDoor(store)
	if err := sessFront.SetLocalString(bead.ID, "last_woke_at", "2026-04-10T12:00:00Z"); err != nil {
		t.Fatalf("SetLocalString(last_woke_at seed): %v", err)
	}

	lis, err := startControllerSocket(
		cityDir,
		controllerHostingStandalone,
		func() {},
		nil,
		nil,
		make(chan reloadRequest),
		make(chan convergenceRequest, 1),
		make(chan struct{}, 1),
		make(chan struct{}, 1),
	)
	if err != nil {
		t.Fatalf("startControllerSocket: %v", err)
	}
	defer lis.Close()                              //nolint:errcheck
	defer os.Remove(controllerSocketPath(cityDir)) //nolint:errcheck

	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{identity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionKill = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	info, err := sessFront.Get(bead.ID)
	if err != nil {
		t.Fatalf("sessFront.Get(post-kill): %v", err)
	}
	if info.LastWokeAt != "" {
		t.Errorf("LastWokeAt = %q, want cleared (local sidecar must not mask the durable clear)", info.LastWokeAt)
	}
}
