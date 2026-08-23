package main

import (
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestControlDispatcherRigContextForStoreRefUsesCanonicalScope(t *testing.T) {
	for _, tc := range []struct {
		name string
		ref  string
		want string
	}{
		{name: "legacy city", ref: "", want: ""},
		{name: "city alias", ref: "city", want: ""},
		{name: "city", ref: "city:test-city", want: ""},
		{name: "relocated class", ref: "class:gmnos", want: ""},
		{name: "rig", ref: "rig:fixture", want: "fixture"},
		{name: "legacy bare rig", ref: "fixture", want: "fixture"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := controlDispatcherRigContextForStoreRef(tc.ref); got != tc.want {
				t.Fatalf("controlDispatcherRigContextForStoreRef(%q) = %q, want %q", tc.ref, got, tc.want)
			}
		})
	}
}

func TestBuildDesiredStateClassControlWorkWakesExactlyOneCityDispatcher(t *testing.T) {
	cityPath := t.TempDir()
	cityStore := beads.NewMemStore()
	classStore := beads.NewMemStore()
	seedSplitRoutes(t, cityPath, classStore)

	control, err := classStore.Create(beads.Bead{
		Title:  "Finalize graph workflow",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:         beadmeta.KindWorkflowFinalize,
			beadmeta.RoutedToMetadataKey:     "core.control-dispatcher",
			beadmeta.RootStoreRefMetadataKey: "class:gmnos",
		},
	})
	if err != nil {
		t.Fatalf("create class control: %v", err)
	}

	maxActive := 1
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              config.ControlDispatcherAgentName,
			BindingName:       "core",
			StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: &maxActive,
		}},
	}

	result := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), cityStore,
		nil, newSessionBeadSnapshot(nil), nil, io.Discard,
	)

	stored, err := classStore.Get(control.ID)
	if err != nil {
		t.Fatalf("get class control: %v", err)
	}
	if got := stored.Metadata[beadmeta.RoutedToMetadataKey]; got != "core.control-dispatcher" {
		t.Fatalf("class control route = %q, want canonical city route preserved", got)
	}
	if got := result.ScaleCheckCounts["core.control-dispatcher"]; got != 1 {
		t.Fatalf("city dispatcher demand = %d, want exactly one", got)
	}
	if got := result.ScaleCheckCounts["fixture/core.control-dispatcher"]; got != 0 {
		t.Fatalf("rig dispatcher demand = %d, want zero for class-resident control work", got)
	}
	if len(result.State) != 1 {
		t.Fatalf("desired state = %v, want exactly one ephemeral city dispatcher", mapKeys(result.State))
	}
	for _, desired := range result.State {
		if desired.TemplateName != "core.control-dispatcher" {
			t.Fatalf("desired dispatcher template = %q, want core.control-dispatcher", desired.TemplateName)
		}
	}
}
