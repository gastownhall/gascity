package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestBuildDesiredState_SingletonTemplateDoesNotRealizeDependencyPoolFloorWithoutSession(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name: "db",
				Pool: &config.PoolConfig{
					Min:   0,
					Max:   3,
					Check: "printf 0",
				},
			},
			{
				Name:      "api",
				DependsOn: []string{"db"},
			},
		},
	}

	desired := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	dbSlots := 0
	for _, tp := range desired {
		if tp.TemplateName == "db" {
			dbSlots++
		}
	}
	if dbSlots != 0 {
		t.Fatalf("db desired slots = %d, want 0 without a realized dependent session", dbSlots)
	}
}

func TestBuildDesiredState_DoesNotRealizeDependencyFloorForZeroScaledDependentPool(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name: "db",
				Pool: &config.PoolConfig{
					Min:   0,
					Max:   3,
					Check: "printf 0",
				},
			},
			{
				Name: "api",
				Pool: &config.PoolConfig{
					Min:   0,
					Max:   3,
					Check: "printf 0",
				},
				DependsOn: []string{"db"},
			},
		},
	}

	desired := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	for _, tp := range desired {
		if tp.TemplateName == "db" {
			t.Fatalf("unexpected dependency-only db slot for zero-scaled dependent pool: %+v", tp)
		}
	}
}

func TestBuildDesiredState_DoesNotRealizeDependencyFloorForSuspendedDependent(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name: "db",
				Pool: &config.PoolConfig{
					Min:   0,
					Max:   3,
					Check: "printf 0",
				},
			},
			{
				Name:      "api",
				Suspended: true,
				DependsOn: []string{"db"},
			},
		},
	}

	desired := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	for _, tp := range desired {
		if tp.TemplateName == "db" {
			t.Fatalf("unexpected dependency-only db slot for suspended dependent: %+v", tp)
		}
	}
}

func TestBuildDesiredState_SingletonTemplatesDoNotRealizeTransitiveDependencyPoolFloorWithoutSession(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name: "db",
				Pool: &config.PoolConfig{
					Min:   0,
					Max:   3,
					Check: "printf 0",
				},
			},
			{
				Name: "api",
				Pool: &config.PoolConfig{
					Min:   0,
					Max:   3,
					Check: "printf 0",
				},
				DependsOn: []string{"db"},
			},
			{
				Name:      "web",
				DependsOn: []string{"api"},
			},
		},
	}

	desired := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	apiSlots := 0
	dbSlots := 0
	for _, tp := range desired {
		switch tp.TemplateName {
		case "api":
			apiSlots++
		case "db":
			dbSlots++
		}
	}
	if apiSlots != 0 {
		t.Fatalf("api desired slots = %d, want 0 without a realized root session", apiSlots)
	}
	if dbSlots != 0 {
		t.Fatalf("db desired slots = %d, want 0 without a realized root session", dbSlots)
	}
}

func TestBuildDesiredState_DiscoveredSessionRootGetsDependencyPoolFloor(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"template":     "helper",
			"session_name": "s-gc-100",
			"state":        "creating",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name: "db",
				Pool: &config.PoolConfig{
					Min:   0,
					Max:   3,
					Check: "printf 0",
				},
			},
			{
				Name:         "helper",
				Suspended:    true,
				DependsOn:    []string{"db"},
				StartCommand: "echo",
			},
		},
	}

	desired := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if _, ok := desired["s-gc-100"]; !ok {
		t.Fatalf("expected discovered helper session in desired state, got keys %v", desired)
	}
	dbSlots := 0
	for _, tp := range desired {
		if tp.TemplateName == "db" {
			dbSlots++
		}
	}
	if dbSlots != 1 {
		t.Fatalf("db desired slots = %d, want 1", dbSlots)
	}
}

func TestBuildDesiredState_ManualZeroScaledPoolSessionStaysDesiredAndKeepsDependencyFloor(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "debug api",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:api"},
		Metadata: map[string]string{
			"template":       "api",
			"session_name":   "s-gc-200",
			"state":          "creating",
			"manual_session": "true",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name: "db",
				Pool: &config.PoolConfig{
					Min:   0,
					Max:   3,
					Check: "printf 0",
				},
			},
			{
				Name:         "api",
				DependsOn:    []string{"db"},
				StartCommand: "echo",
				Pool: &config.PoolConfig{
					Min:   0,
					Max:   3,
					Check: "printf 0",
				},
			},
		},
	}

	desired := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if _, ok := desired["s-gc-200"]; !ok {
		t.Fatalf("expected manual pool session in desired state, got keys %v", desired)
	}
	dbSlots := 0
	for _, tp := range desired {
		if tp.TemplateName == "db" {
			dbSlots++
		}
	}
	if dbSlots != 1 {
		t.Fatalf("db desired slots = %d, want 1", dbSlots)
	}
}

func TestBuildDesiredState_ZeroScaledPoolSessionKeepsDependencyFloorWhileDraining(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "api-1",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:api"},
		Metadata: map[string]string{
			"template":     "api",
			"session_name": "api-1",
			"agent_name":   "api-1",
			"state":        "active",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name: "db",
				Pool: &config.PoolConfig{
					Min:   0,
					Max:   3,
					Check: "printf 0",
				},
			},
			{
				Name:      "api",
				DependsOn: []string{"db"},
				Pool: &config.PoolConfig{
					Min:   0,
					Max:   3,
					Check: "printf 0",
				},
			},
		},
	}

	desired := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if _, ok := desired["api-1"]; ok {
		t.Fatalf("did not expect zero-scaled pool bead to re-enter desired state: %+v", desired["api-1"])
	}
	dbSlots := 0
	for _, tp := range desired {
		if tp.TemplateName == "db" {
			dbSlots++
		}
	}
	if dbSlots != 1 {
		t.Fatalf("db desired slots = %d, want 1", dbSlots)
	}
}

func TestBuildDesiredState_ManualPoolSessionInSuspendedRigStaysStopped(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "payments")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "debug api",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:payments/api"},
		Metadata: map[string]string{
			"template":       "payments/api",
			"session_name":   "s-gc-300",
			"state":          "creating",
			"manual_session": "true",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Rigs: []config.Rig{{
			Name:      "payments",
			Path:      rigPath,
			Suspended: true,
		}},
		Agents: []config.Agent{
			{
				Name: "db",
				Dir:  "payments",
				Pool: &config.PoolConfig{
					Min:   0,
					Max:   3,
					Check: "printf 0",
				},
			},
			{
				Name:         "api",
				Dir:          "payments",
				DependsOn:    []string{"payments/db"},
				StartCommand: "echo",
				Pool: &config.PoolConfig{
					Min:   0,
					Max:   3,
					Check: "printf 0",
				},
			},
		},
	}

	desired := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if _, ok := desired["s-gc-300"]; ok {
		t.Fatalf("manual pool session in suspended rig should not enter desired state: %+v", desired["s-gc-300"])
	}
	for _, tp := range desired {
		if tp.TemplateName == "payments/db" {
			t.Fatalf("suspended-rig manual session should not hold dependency floor: %+v", tp)
		}
	}
}
