package main

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func TestControllerStateSessionStartSnapshotCapturesOneGeneration(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "snapshot-city"}}
	provider := runtime.NewFake()
	store := beads.NewMemStore()
	cs := &controllerState{
		cfg:                         cfg,
		sp:                          provider,
		cityBeadStore:               store,
		cityName:                    "snapshot-city",
		cityPath:                    "/snapshot-city",
		sessionStartGeneration:      17,
		sessionStartStoreGeneration: 17,
	}

	snapshot, err := cs.sessionStartSnapshot()
	if err != nil {
		t.Fatalf("sessionStartSnapshot: %v", err)
	}
	if snapshot.Generation != 17 {
		t.Fatalf("generation = %d, want 17", snapshot.Generation)
	}
	if snapshot.Config != cfg || snapshot.Provider != provider || snapshot.Store != store {
		t.Fatalf("snapshot mixed controller state: %+v", snapshot)
	}
	if snapshot.CityName != "snapshot-city" || snapshot.CityPath != "/snapshot-city" {
		t.Fatalf("snapshot identity = %q %q, want snapshot-city /snapshot-city", snapshot.CityName, snapshot.CityPath)
	}
}

func TestControllerStateSessionStartSnapshotFailsClosedWhenUnavailable(t *testing.T) {
	tests := []struct {
		name string
		cs   *controllerState
	}{
		{name: "nil state"},
		{name: "nil config", cs: &controllerState{sp: runtime.NewFake(), cityBeadStore: beads.NewMemStore()}},
		{name: "nil provider", cs: &controllerState{cfg: &config.City{}, cityBeadStore: beads.NewMemStore()}},
		{name: "nil store", cs: &controllerState{cfg: &config.City{}, sp: runtime.NewFake()}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.cs.sessionStartSnapshot(); err == nil {
				t.Fatal("sessionStartSnapshot error = nil")
			}
		})
	}
}

func TestControllerStateConfigOnlyUpdateDoesNotBlessIncoherentSessionStore(t *testing.T) {
	store := beads.NewMemStore()
	cs := &controllerState{
		cfg:                         &config.City{},
		sp:                          runtime.NewFake(),
		cityBeadStore:               store,
		sessionStartGeneration:      9,
		sessionStartStoreGeneration: 8,
	}

	cs.updateConfigAndProviderOnly(&config.City{}, runtime.NewFake())

	if _, err := cs.sessionStartSnapshot(); err == nil {
		t.Fatal("config-only update made a previously incoherent session store available")
	}
}

func TestControllerStateSessionStartSnapshotRejectsPendingConfigMutation(t *testing.T) {
	cs := &controllerState{
		cfg:                         &config.City{},
		sp:                          runtime.NewFake(),
		cityBeadStore:               beads.NewMemStore(),
		sessionStartGeneration:      3,
		sessionStartStoreGeneration: 3,
	}
	cs.markConfigMutationPending("next-revision")

	if _, _, err := cs.acquireSessionStartSnapshot(); err == nil {
		t.Fatal("exact session start acquired a snapshot while runtime config application was pending")
	}

	cs.clearConfigMutationPending()
	snapshot, release, err := cs.acquireSessionStartSnapshot()
	if err != nil {
		t.Fatalf("acquireSessionStartSnapshot after matching runtime application: %v", err)
	}
	release()
	if snapshot.Generation != 3 {
		t.Fatalf("generation = %d, want 3", snapshot.Generation)
	}
}

func TestControllerStatePendingConfigUpdateWaitsForExactStartLease(t *testing.T) {
	stubSessionStartCityStoreOpen(t)
	cacheCtx, cancelCache := context.WithCancel(context.Background())
	t.Cleanup(cancelCache)
	oldCfg := &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	newCfg := &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true", DependsOn: []string{"database"}}}}
	cs := &controllerState{
		cfg:                         oldCfg,
		sp:                          runtime.NewFake(),
		cityBeadStore:               beads.NewMemStore(),
		eventProv:                   events.NewFake(),
		cacheCtx:                    cacheCtx,
		sessionStartGeneration:      7,
		sessionStartStoreGeneration: 7,
	}

	_, release, err := cs.acquireSessionStartSnapshot()
	if err != nil {
		t.Fatalf("acquire old exact-start lease: %v", err)
	}
	updated := make(chan struct{})
	go func() {
		cs.updateWithPendingConfigMutation(newCfg, cs.sp, "next-revision")
		close(updated)
	}()
	awaitCond(t, func() bool {
		cs.sessionStartLeaseMu.Lock()
		defer cs.sessionStartLeaseMu.Unlock()
		return cs.sessionStartSwapPending
	}, "pending config update to enter the exact-start generation fence")

	if cs.configMutationPending.Load() {
		t.Fatal("config mutation enabled legacy fallback before the old exact-start lease drained")
	}
	select {
	case <-updated:
		t.Fatal("config update completed before the old exact-start lease released")
	default:
	}

	release()
	awaitClose(t, updated, "pending config update")
	if !cs.configMutationPending.Load() {
		t.Fatal("config update exposed new controller state without marking runtime application pending")
	}
}

func stubSessionStartCityStoreOpen(t *testing.T) {
	t.Helper()
	previous := newControllerStateOpenCityStore
	newControllerStateOpenCityStore = func(string, gate.Mode) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{Store: beads.NewMemStore()}, nil
	}
	t.Cleanup(func() {
		newControllerStateOpenCityStore = previous
	})
}

func TestControllerStateSessionStartLeaseFencesGenerationSwap(t *testing.T) {
	oldCfg := &config.City{Workspace: config.Workspace{Name: "old"}}
	newCfg := &config.City{Workspace: config.Workspace{Name: "new"}}
	oldProvider := runtime.NewFake()
	newProvider := runtime.NewFake()
	store := beads.NewMemStore()
	cs := &controllerState{
		cfg:                         oldCfg,
		sp:                          oldProvider,
		cityBeadStore:               store,
		sessionStartGeneration:      4,
		sessionStartStoreGeneration: 4,
	}

	snapshot, release, err := cs.acquireSessionStartSnapshot()
	if err != nil {
		t.Fatalf("acquireSessionStartSnapshot: %v", err)
	}
	if snapshot.Generation != 4 || snapshot.Config != oldCfg || snapshot.Provider != oldProvider {
		t.Fatalf("leased snapshot = %+v, want old generation 4", snapshot)
	}

	updated := make(chan struct{})
	go func() {
		cs.updateConfigAndProviderOnly(newCfg, newProvider)
		close(updated)
	}()
	awaitCond(t, func() bool {
		cs.sessionStartLeaseMu.Lock()
		defer cs.sessionStartLeaseMu.Unlock()
		return cs.sessionStartSwapPending
	}, "session-start generation swap to wait for lease")

	if _, _, err := cs.acquireSessionStartSnapshot(); err == nil {
		t.Fatal("new lease entered while a generation swap was pending")
	}
	select {
	case <-updated:
		t.Fatal("generation swap completed before the old lease released")
	default:
	}

	release()
	awaitClose(t, updated, "session-start generation swap")
	current, currentRelease, err := cs.acquireSessionStartSnapshot()
	if err != nil {
		t.Fatalf("acquire current session-start snapshot: %v", err)
	}
	defer currentRelease()
	if current.Generation != 5 || current.Config != newCfg || current.Provider != newProvider {
		t.Fatalf("current snapshot = %+v, want new generation 5", current)
	}
}

func TestControllerStateSessionEventAdmissionFiltersByDecodedBead(t *testing.T) {
	cs := &controllerState{}
	admitted := make(chan string, 4)
	if err := cs.installSessionStartEventAdmission(func(id string) {
		admitted <- id
	}); err != nil {
		t.Fatalf("installSessionStartEventAdmission: %v", err)
	}
	t.Cleanup(cs.stopSessionStartEventAdmission)

	tests := []struct {
		name string
		evt  events.Event
		want string
	}{
		{
			name: "proper session",
			evt: beadEventForSessionStartTest(t, events.BeadCreated, beads.Bead{
				ID:   "gcs-proper1",
				Type: session.BeadType,
			}),
			want: "gcs-proper1",
		},
		{
			name: "repairable session",
			evt: beadEventForSessionStartTest(t, events.BeadUpdated, beads.Bead{
				ID:     "gcs-repair1",
				Labels: []string{session.LabelSession},
			}),
			want: "gcs-repair1",
		},
		{
			name: "ordinary work",
			evt: beadEventForSessionStartTest(t, events.BeadUpdated, beads.Bead{
				ID:   "ga-work1",
				Type: "task",
			}),
		},
		{
			name: "wrong type despite session label",
			evt: beadEventForSessionStartTest(t, events.BeadUpdated, beads.Bead{
				ID:     "ga-work2",
				Type:   "task",
				Labels: []string{session.LabelSession},
			}),
		},
		{
			name: "malformed payload",
			evt:  events.Event{Type: events.BeadUpdated, Payload: []byte("not-json")},
		},
		{
			name: "non bead event",
			evt: beadEventForSessionStartTest(t, events.ControllerStarted, beads.Bead{
				ID:   "gcs-wrong-event1",
				Type: session.BeadType,
			}),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cs.admitSessionStartEvent(test.evt)
			select {
			case got := <-admitted:
				if test.want == "" {
					t.Fatalf("unexpected admission %q", got)
				}
				if got != test.want {
					t.Fatalf("admission = %q, want %q", got, test.want)
				}
			default:
				if test.want != "" {
					t.Fatalf("missing admission %q", test.want)
				}
			}
		})
	}
}

func TestControllerStateStoppingSessionEventAdmissionDrainsCallback(t *testing.T) {
	cs := &controllerState{}
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	if err := cs.installSessionStartEventAdmission(func(string) {
		calls.Add(1)
		close(entered)
		<-release
	}); err != nil {
		t.Fatalf("installSessionStartEventAdmission: %v", err)
	}

	eventDone := make(chan struct{})
	go func() {
		cs.admitSessionStartEvent(beadEventForSessionStartTest(t, events.BeadUpdated, beads.Bead{
			ID:   "gcs-drain1",
			Type: session.BeadType,
		}))
		close(eventDone)
	}()
	awaitClose(t, entered, "session-event admission callback")

	stopDone := make(chan struct{})
	go func() {
		cs.stopSessionStartEventAdmission()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		t.Fatal("stop returned before the admitted callback drained")
	default:
	}
	close(release)
	awaitClose(t, eventDone, "session-event callback completion")
	awaitClose(t, stopDone, "session-event admission stop")

	cs.admitSessionStartEvent(beadEventForSessionStartTest(t, events.BeadUpdated, beads.Bead{
		ID:   "gcs-after-stop1",
		Type: session.BeadType,
	}))
	if got := calls.Load(); got != 1 {
		t.Fatalf("callback calls = %d, want 1 after admission stopped", got)
	}

	if err := cs.installSessionStartEventAdmission(func(string) {}); err != nil {
		t.Fatalf("reinstallSessionStartEventAdmission after completed drain: %v", err)
	}
	cs.stopSessionStartEventAdmission()
}

func beadEventForSessionStartTest(t *testing.T, eventType string, bead beads.Bead) events.Event {
	t.Helper()
	payload, err := json.Marshal(bead)
	if err != nil {
		t.Fatalf("marshal bead event: %v", err)
	}
	return events.Event{Type: eventType, Subject: bead.ID, Payload: payload}
}
