package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// killPokeSessionIdentity is the configured named-session identity every
// kill-poke fixture city declares.
const killPokeSessionIdentity = "session-a"

// newKillPokeSession stands up a city, store, and fake runtime for an awake
// canonical configured named session, returning the store and the session
// bead. The fake provider is wired through buildSessionProviderByName so
// cmdSessionKill resolves a real handle and reaches the asleep-sync + handoff
// tail.
func newKillPokeSession(t *testing.T, mode string) (beads.Store, beads.Bead, string) {
	t.Helper()
	const identity = killPokeSessionIdentity

	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := shortSocketTempDir(t, "gc-kill-poke-")
	t.Setenv("GC_CITY", cityDir)
	writeGenericNamedSessionCityTOML(t, cityDir)
	configPath := filepath.Join(cityDir, "city.toml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(city.toml): %v", err)
	}
	data = append(data, []byte("mode = \""+mode+"\"\n")...)
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	cfg, err := loadCityConfig(cityDir, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	spec, ok := findNamedSessionSpec(cfg, cfg.EffectiveCityName(), identity)
	if !ok {
		t.Fatalf("findNamedSessionSpec(%q) = not found", identity)
	}
	if spec.Mode != mode {
		t.Fatalf("named session mode = %q, want %q", spec.Mode, mode)
	}
	sessionName := spec.SessionName

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
			"template":                   spec.Agent.QualifiedName(),
			"agent_name":                 spec.Agent.QualifiedName(),
			"session_name":               sessionName,
			"state":                      "awake",
			"session_origin":             "named",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: identity,
			namedSessionModeMetadata:     mode,
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

// TestCmdSessionKill_AlwaysNamedPersistsWakeBeforeExactHandoff proves an
// always-named kill durably records both the killed sleep transition and an
// explicit wake before handing the exact canonical bead ID to the keyed start
// controller.
func TestCmdSessionKill_AlwaysNamedPersistsWakeBeforeExactHandoff(t *testing.T) {
	store, bead, cityDir := newKillPokeSession(t, "always")

	calls := 0
	var gotCityPath, gotSessionID string
	var metadataAtHandoff map[string]string
	old := sessionKillExactStartController
	sessionKillExactStartController = func(cityPath, sessionID string) error {
		calls++
		gotCityPath = cityPath
		gotSessionID = sessionID
		if b, getErr := store.Get(sessionID); getErr == nil {
			metadataAtHandoff = b.Metadata
		}
		return nil
	}
	t.Cleanup(func() { sessionKillExactStartController = old })

	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{killPokeSessionIdentity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionKill = %d, want 0; stderr=%s", code, stderr.String())
	}

	if calls != 1 {
		t.Fatalf("poke called %d times, want exactly 1", calls)
	}
	if gotCityPath != cityDir {
		t.Errorf("handoff cityPath = %q, want %q", gotCityPath, cityDir)
	}
	if gotSessionID != bead.ID {
		t.Errorf("handoff session ID = %q, want canonical bead %q", gotSessionID, bead.ID)
	}
	if got := metadataAtHandoff["state"]; got != string(sessionpkg.StateAsleep) {
		t.Errorf("state at handoff = %q, want %q", got, sessionpkg.StateAsleep)
	}
	if got := metadataAtHandoff["sleep_reason"]; got != "killed" {
		t.Errorf("sleep_reason at handoff = %q, want killed", got)
	}
	if got := metadataAtHandoff["wake_request"]; got != string(sessionpkg.WakeCauseExplicit) {
		t.Errorf("wake_request at handoff = %q, want %q", got, sessionpkg.WakeCauseExplicit)
	}
	if metadataAtHandoff["wake_requested_at"] == "" {
		t.Error("wake_requested_at at handoff is empty")
	}
	if metadataAtHandoff["synced_at"] == "" {
		t.Error("synced_at at handoff is empty")
	}
}

// TestCmdSessionKill_OnDemandNamedStaysAsleepWithoutExactHandoff proves an
// on-demand configured session retains the killed sleep transition without a
// durable wake request or exact-key start hint.
func TestCmdSessionKill_OnDemandNamedStaysAsleepWithoutExactHandoff(t *testing.T) {
	store, bead, _ := newKillPokeSession(t, "on_demand")

	exactCalls := 0
	oldExact := sessionKillExactStartController
	sessionKillExactStartController = func(string, string) error {
		exactCalls++
		return nil
	}
	t.Cleanup(func() { sessionKillExactStartController = oldExact })
	genericCalls := 0
	oldGeneric := sessionKillPokeController
	sessionKillPokeController = func(string) error {
		genericCalls++
		return nil
	}
	t.Cleanup(func() { sessionKillPokeController = oldGeneric })

	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{killPokeSessionIdentity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionKill = %d, want 0; stderr=%s", code, stderr.String())
	}
	if exactCalls != 0 {
		t.Fatalf("exact handoff called %d times, want 0", exactCalls)
	}
	if genericCalls != 1 {
		t.Fatalf("generic poke called %d times, want 1", genericCalls)
	}
	updated, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get(%q): %v", bead.ID, err)
	}
	if got := updated.Metadata["state"]; got != string(sessionpkg.StateAsleep) {
		t.Errorf("state = %q, want %q", got, sessionpkg.StateAsleep)
	}
	if got := updated.Metadata["sleep_reason"]; got != "killed" {
		t.Errorf("sleep_reason = %q, want killed", got)
	}
	if got := updated.Metadata["wake_request"]; got != "" {
		t.Errorf("wake_request = %q, want empty", got)
	}
	if got := updated.Metadata["wake_requested_at"]; got != "" {
		t.Errorf("wake_requested_at = %q, want empty", got)
	}
}

// TestCmdSessionKill_PinnedOnDemandNamedHandsOffExactKeyWithoutWakeMarker
// proves a killed pinned on-demand configured session persists asleep with the
// killed reason and its retained pin, hands exactly one canonical bead ID to
// the keyed start controller with zero generic pokes, and synthesizes no wake
// request: the pin is the only wake authority the restart may use.
func TestCmdSessionKill_PinnedOnDemandNamedHandsOffExactKeyWithoutWakeMarker(t *testing.T) {
	store, bead, cityDir := newKillPokeSession(t, "on_demand")
	if err := store.SetMetadata(bead.ID, "pin_awake", "true"); err != nil {
		t.Fatalf("pin on-demand named session: %v", err)
	}

	exactCalls := 0
	var gotCityPath, gotSessionID string
	var metadataAtHandoff map[string]string
	oldExact := sessionKillExactStartController
	sessionKillExactStartController = func(cityPath, sessionID string) error {
		exactCalls++
		gotCityPath = cityPath
		gotSessionID = sessionID
		if b, getErr := store.Get(sessionID); getErr == nil {
			metadataAtHandoff = b.Metadata
		}
		return nil
	}
	t.Cleanup(func() { sessionKillExactStartController = oldExact })
	genericCalls := 0
	oldGeneric := sessionKillPokeController
	sessionKillPokeController = func(string) error {
		genericCalls++
		return nil
	}
	t.Cleanup(func() { sessionKillPokeController = oldGeneric })

	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{killPokeSessionIdentity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionKill = %d, want 0; stderr=%s", code, stderr.String())
	}
	if exactCalls != 1 || genericCalls != 0 {
		t.Fatalf("handoff calls = exact %d generic %d, want exactly one exact key and no generic poke", exactCalls, genericCalls)
	}
	if gotCityPath != cityDir || gotSessionID != bead.ID {
		t.Fatalf("handoff = (%q, %q), want canonical bead %q in city %q", gotCityPath, gotSessionID, bead.ID, cityDir)
	}
	if got := metadataAtHandoff["state"]; got != string(sessionpkg.StateAsleep) {
		t.Errorf("state at handoff = %q, want %q", got, sessionpkg.StateAsleep)
	}
	if got := metadataAtHandoff["sleep_reason"]; got != "killed" {
		t.Errorf("sleep_reason at handoff = %q, want killed", got)
	}
	if got := metadataAtHandoff["pin_awake"]; got != "true" {
		t.Errorf("pin_awake at handoff = %q, want the retained pin", got)
	}
	if got := metadataAtHandoff["wake_request"]; got != "" {
		t.Errorf("wake_request at handoff = %q, want the pin to remain the sole wake authority", got)
	}
	if got := metadataAtHandoff["wake_requested_at"]; got != "" {
		t.Errorf("wake_requested_at at handoff = %q, want empty", got)
	}
	if metadataAtHandoff["synced_at"] == "" {
		t.Error("synced_at at handoff is empty")
	}
}

// TestCmdSessionKill_ExactHandoffFailureIsNonFatal pins the best-effort
// contract: an exact start-handoff failure must not fail the kill after the
// durable sleep and wake intent have been persisted.
func TestCmdSessionKill_ExactHandoffFailureIsNonFatal(t *testing.T) {
	_, _, _ = newKillPokeSession(t, "always")

	old := sessionKillExactStartController
	sessionKillExactStartController = func(string, string) error { return errors.New("dial failed") }
	t.Cleanup(func() { sessionKillExactStartController = old })

	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{killPokeSessionIdentity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionKill = %d, want 0 (handoff failure is best-effort); stderr=%s", code, stderr.String())
	}
}

// TestCmdSessionKill_PokesControllerAfterSleep pins #3812: a successful
// `gc session kill` on a shape with no exact-key handoff must poke the
// controller so the reconciler observes the killed state promptly instead of
// waiting a full patrol interval. The poke fires exactly once, with the
// resolved cityPath, and only AFTER the bead has been synced asleep.
//
// Restored from origin/main under ga-f7v2ft.183. 84ba8cfa47 renamed this test
// in place onto the exact-handoff seam it added, which left the generic-poke
// branch (cmd_session.go's `else if sessionKillPokeController(...)`) live but
// uncovered for both the ordering and the argument.
func TestCmdSessionKill_PokesControllerAfterSleep(t *testing.T) {
	store, bead, cityDir := newKillPokeSession(t, "on_demand")

	exactCalls := 0
	oldExact := sessionKillExactStartController
	sessionKillExactStartController = func(string, string) error {
		exactCalls++
		return nil
	}
	t.Cleanup(func() { sessionKillExactStartController = oldExact })

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
	if code := cmdSessionKill([]string{killPokeSessionIdentity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionKill = %d, want 0; stderr=%s", code, stderr.String())
	}

	if exactCalls != 0 {
		t.Fatalf("exact start handoff called %d times, want 0 for the generic-poke shape", exactCalls)
	}
	if calls != 1 {
		t.Fatalf("poke called %d times, want exactly 1", calls)
	}
	if gotCityPath != cityDir {
		t.Errorf("poke cityPath = %q, want %q", gotCityPath, cityDir)
	}
	if stateAtPoke != string(sessionpkg.StateAsleep) {
		t.Errorf("state at poke time = %q, want %q (the poke must run after the SleepPatch write)", stateAtPoke, sessionpkg.StateAsleep)
	}
}

// TestCmdSessionKill_PokeFailureIsNonFatal pins the best-effort contract: a
// generic poke failure (no controller running, say) must not fail the kill.
// The session state is already synced asleep, so the reconciler observes it on
// its normal convergence pass whether or not the poke landed.
//
// Restored from origin/main under ga-f7v2ft.183. The lane's
// TestCmdSessionKill_ExactHandoffFailureIsNonFatal stubs the OTHER seam, so
// nothing drove sessionKillPokeController to an error.
func TestCmdSessionKill_PokeFailureIsNonFatal(t *testing.T) {
	_, _, _ = newKillPokeSession(t, "on_demand")

	oldExact := sessionKillExactStartController
	sessionKillExactStartController = func(string, string) error {
		t.Error("exact start handoff must not run for the generic-poke shape")
		return nil
	}
	t.Cleanup(func() { sessionKillExactStartController = oldExact })

	old := sessionKillPokeController
	sessionKillPokeController = func(string) error { return errors.New("dial failed") }
	t.Cleanup(func() { sessionKillPokeController = old })

	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{killPokeSessionIdentity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionKill = %d, want 0 (poke failure is best-effort); stderr=%s", code, stderr.String())
	}
}

// killOrderProvider observes the durable row at the instant the runtime is torn
// down — the only place the awake+dead window can be proven closed.
type killOrderProvider struct {
	runtime.Provider
	onStop func()
}

func (p *killOrderProvider) Stop(name string) error {
	p.onStop()
	return p.Provider.Stop(name)
}

// TestCmdSessionKill_RecordsSleepIntentBeforeTearingDownTheRuntime pins
// ga-rk41a: a reconciler tick that lands between the teardown and the asleep
// sync sees a row claiming a live runtime whose runtime is already dead, and
// acts on it twice over — it restarts the session the operator just killed and
// closes the row as dead-runtime.
func TestCmdSessionKill_RecordsSleepIntentBeforeTearingDownTheRuntime(t *testing.T) {
	store, bead, _ := newKillPokeSession(t, "always")

	oldExact := sessionKillExactStartController
	sessionKillExactStartController = func(string, string) error { return nil }
	t.Cleanup(func() { sessionKillExactStartController = oldExact })
	oldPoke := sessionKillPokeController
	sessionKillPokeController = func(string) error { return nil }
	t.Cleanup(func() { sessionKillPokeController = oldPoke })

	stopCalls := 0
	var metadataAtStop map[string]string
	oldBuild := buildSessionProviderByName
	buildSessionProviderByName = func(cfg *config.City, name string, sc config.SessionConfig, cityName, cityPath string) (runtime.Provider, error) {
		p, err := oldBuild(cfg, name, sc, cityName, cityPath)
		if err != nil {
			return nil, err
		}
		return &killOrderProvider{Provider: p, onStop: func() {
			stopCalls++
			if b, getErr := store.Get(bead.ID); getErr == nil {
				metadataAtStop = b.Metadata
			}
		}}, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })

	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{killPokeSessionIdentity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionKill = %d, want 0; stderr=%s", code, stderr.String())
	}
	if stopCalls != 1 {
		t.Fatalf("runtime teardown calls = %d, want exactly 1; stderr=%s", stopCalls, stderr.String())
	}
	if got := metadataAtStop["state"]; got != string(sessionpkg.StateAsleep) {
		t.Errorf("state at runtime teardown = %q, want %q", got, sessionpkg.StateAsleep)
	}
	if got := metadataAtStop["sleep_reason"]; got != "killed" {
		t.Errorf("sleep_reason at runtime teardown = %q, want killed", got)
	}
	// The wake half must still be published after the teardown: a wake request
	// on a row whose runtime is alive invites a duplicate incarnation.
	if got := metadataAtStop["wake_request"]; got != "" {
		t.Errorf("wake_request at runtime teardown = %q, want empty", got)
	}
	final, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get(%q): %v", bead.ID, err)
	}
	if got := final.Metadata["wake_request"]; got != string(sessionpkg.WakeCauseExplicit) {
		t.Errorf("wake_request after kill = %q, want %q", got, sessionpkg.WakeCauseExplicit)
	}
}
