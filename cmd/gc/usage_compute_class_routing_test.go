package main

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/usage"
)

// relocatedSessionRoutes builds storageRoutes that relocate ONLY the sessions
// class, which is the shape of a city converged onto a sessions binding.
func relocatedSessionRoutes(store beads.Store) *storageRoutes {
	return &storageRoutes{
		stores:  map[coordclass.Class]beads.Store{coordclass.ClassSessions: store},
		binding: "test-sessions-binding",
	}
}

// TestEmitDueComputeFactsReadsSessionsClass proves the usage lane reads and
// writes the SESSIONS class, not the work store.
//
// Every bead this lane touches is a session bead: it Gets by session id and
// SetMetadatas the two usage markers onto that bead. On a converged split city
// the work store never held the row, so a work-store Get returns ErrNotFound,
// the loop logs and continues, and usage/billing facts silently stop being
// emitted for the whole fleet — a silent-drop, not a latency regression.
func TestEmitDueComputeFactsReadsSessionsClass(t *testing.T) {
	cityPath := t.TempDir()
	sinkPath := filepath.Join(cityPath, ".gc", "usage.jsonl")

	workStore := beads.NewMemStore()
	sessionsStore := beads.NewMemStore()

	meta := map[string]string{
		"state":            string(session.StateAsleep),
		"session_name":     "gc-demo-worker",
		"awake_started_at": "2026-08-01T00:00:00Z",
		"provider":         "codex",
	}
	bead, err := sessionsStore.Create(beads.Bead{
		Type:     session.BeadType,
		Status:   "open",
		Title:    meta["session_name"],
		Labels:   []string{session.LabelSession},
		Metadata: meta,
	})
	if err != nil {
		t.Fatalf("seed session bead in the relocated store: %v", err)
	}

	cs := &controllerState{
		cityBeadStore: workStore,
		usageSink:     usage.NewLocalSink(sinkPath),
		cityName:      "demo",
		cityPath:      cityPath,
	}
	cr := &CityRuntime{
		cs:            cs,
		cfg:           &config.City{},
		sp:            runtime.NewFake(),
		cityName:      "demo",
		cityPath:      cityPath,
		stderr:        io.Discard,
		storageRoutes: relocatedSessionRoutes(sessionsStore),
	}

	info := session.Info{
		ID:             bead.ID,
		MetadataState:  meta["state"],
		AwakeStartedAt: meta["awake_started_at"],
	}
	cr.emitDueComputeFacts(context.Background(), []session.Info{info}, false)

	// The commit marker lands on the SESSION bead, in the sessions binding.
	got, err := sessionsStore.Get(bead.ID)
	if err != nil {
		t.Fatalf("re-read the relocated session bead: %v", err)
	}
	if got.Metadata[usageComputeEmittedAtKey] == "" {
		t.Fatalf("session bead is missing %s; the usage lane read the work store, which never held it", usageComputeEmittedAtKey)
	}

	// And nothing was minted or stamped in the work store on the way.
	work, err := workStore.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatalf("list work store: %v", err)
	}
	if len(work) != 0 {
		t.Fatalf("work store holds %d bead(s) after a usage tick; the sessions class must not touch it: %v", len(work), work)
	}
}

// TestSessionsBeadStoreIsWorkStoreWhenNothingRelocates is the byte-identity
// proof for a city that relocates nothing: cr.sessionsBeadStore() hands back the
// exact store value cr.cityBeadStore() returns, so the accessor substitution is
// a no-op on every single-store city.
func TestSessionsBeadStoreIsWorkStoreWhenNothingRelocates(t *testing.T) {
	workStore := beads.NewMemStore()
	cr := &CityRuntime{
		cs:       &controllerState{cityBeadStore: workStore, cityName: "demo"},
		cfg:      &config.City{},
		cityName: "demo",
		stderr:   io.Discard,
	}
	if got := cr.sessionsBeadStore().Store; got != cr.cityBeadStore() {
		t.Fatal("default backend: cr.sessionsBeadStore().Store must be the identical value cr.cityBeadStore() returns")
	}
}
