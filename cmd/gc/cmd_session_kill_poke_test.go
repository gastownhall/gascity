package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// newKillPokeSession stands up a city, store, and fake runtime for an awake
// named session, returning the store and the session bead. The fake provider
// is wired through buildSessionProviderByName so cmdSessionKill resolves a real
// handle and reaches the asleep-sync + poke tail.
func newKillPokeSession(t *testing.T, identity, sessionName string) (beads.Store, beads.Bead, string) {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := shortSocketTempDir(t, "gc-kill-poke-")
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
	bead, err := store.Create(beads.Bead{
		Title:  "named session",
		Type:   sessionpkg.BeadType,
		Labels: []string{sessionpkg.LabelSession, "template:worker"},
		Metadata: map[string]string{
			"alias":                      identity,
			"template":                   "worker",
			"agent_name":                 "gascity/gc.worker",
			"session_name":               sessionName,
			"state":                      "awake",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: identity,
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}
	if err := fakeProvider.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("fakeProvider.Start: %v", err)
	}
	if err := fakeProvider.SetMeta(sessionName, "GC_SESSION_ID", bead.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	return store, bead, cityDir
}

// TestCmdSessionKill_PokesControllerAfterSleep pins #3812: a successful
// `gc session kill` must poke the controller so the reconciler observes the
// killed state promptly instead of waiting a full patrol interval. The poke
// must fire exactly once, with the resolved cityPath, and only AFTER the bead
// has been synced asleep (so the reconciler observes the killed state when it
// converges).
func TestCmdSessionKill_PokesControllerAfterSleep(t *testing.T) {
	const identity = "session-a"
	const sessionName = "s-gc-kill-poke"
	store, bead, cityDir := newKillPokeSession(t, identity, sessionName)

	calls := 0
	var gotCityPath, stateAtPoke string
	old := sessionKillPokeController
	sessionKillPokeController = func(cityPath string) error {
		calls++
		gotCityPath = cityPath
		if b, gErr := store.Get(bead.ID); gErr == nil {
			stateAtPoke = b.Metadata["state"]
		}
		return nil
	}
	t.Cleanup(func() { sessionKillPokeController = old })

	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{identity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionKill = %d, want 0; stderr=%s", code, stderr.String())
	}

	if calls != 1 {
		t.Fatalf("poke called %d times, want exactly 1", calls)
	}
	if gotCityPath != cityDir {
		t.Errorf("poke cityPath = %q, want %q", gotCityPath, cityDir)
	}
	if stateAtPoke != string(sessionpkg.StateAsleep) {
		t.Errorf("state at poke time = %q, want %q (poke must run after the SleepPatch write)", stateAtPoke, sessionpkg.StateAsleep)
	}
}

// TestCmdSessionKill_PokeFailureIsNonFatal pins the best-effort contract: a
// poke failure (e.g. no controller running) must not fail the kill — the
// session state has already been synced asleep, so the reconciler observes it
// on its normal convergence pass regardless of whether the poke landed.
func TestCmdSessionKill_PokeFailureIsNonFatal(t *testing.T) {
	const identity = "session-a"
	const sessionName = "s-gc-kill-poke-fail"
	_, _, _ = newKillPokeSession(t, identity, sessionName)

	old := sessionKillPokeController
	sessionKillPokeController = func(string) error { return errors.New("dial failed") }
	t.Cleanup(func() { sessionKillPokeController = old })

	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{identity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionKill = %d, want 0 (poke failure is best-effort); stderr=%s", code, stderr.String())
	}
}
