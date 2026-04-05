package main

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

type listFailStore struct {
	beads.Store
}

func (s listFailStore) List(_ beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("list failed")
}

func TestCollectAssignedWorkBeads_IncludesReadyOpenAssignedHandoff(t *testing.T) {
	store := beads.NewMemStore()
	handoff, err := store.Create(beads.Bead{
		Title:    "merge me",
		Type:     "task",
		Status:   "open",
		Assignee: "repo/refinery",
	})
	if err != nil {
		t.Fatalf("create handoff bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "queued pool work",
		Type:   "task",
		Status: "open",
	}); err != nil {
		t.Fatalf("create queued bead: %v", err)
	}

	got, _ := collectAssignedWorkBeads(&config.City{}, store, nil, nil)
	if len(got) != 1 {
		t.Fatalf("collectAssignedWorkBeads returned %d beads, want 1: %#v", len(got), got)
	}
	if got[0].ID != handoff.ID {
		t.Fatalf("collectAssignedWorkBeads returned %q, want %q", got[0].ID, handoff.ID)
	}
	if got[0].Assignee != "repo/refinery" || got[0].Status != "open" {
		t.Fatalf("assigned handoff bead = assignee %q status %q, want repo/refinery open", got[0].Assignee, got[0].Status)
	}
}

func TestCollectAssignedWorkBeads_ExcludesBlockedOpenAssignedHandoff(t *testing.T) {
	store := beads.NewMemStore()
	blocker, err := store.Create(beads.Bead{
		Title:  "blocker",
		Type:   "task",
		Status: "open",
	})
	if err != nil {
		t.Fatalf("create blocker bead: %v", err)
	}
	handoff, err := store.Create(beads.Bead{
		Title:    "merge me later",
		Type:     "task",
		Status:   "open",
		Assignee: "repo/refinery",
	})
	if err != nil {
		t.Fatalf("create handoff bead: %v", err)
	}
	if err := store.DepAdd(handoff.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("add blocking dep: %v", err)
	}

	got, _ := collectAssignedWorkBeads(&config.City{}, store, nil, nil)
	if len(got) != 0 {
		t.Fatalf("collectAssignedWorkBeads returned %d beads, want 0: %#v", len(got), got)
	}
}

func TestCollectAssignedWorkBeads_IncludesRoutedToMetadataBeads(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	routed, err := store.Create(beads.Bead{
		Title:    "check alpha",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "seth"},
	})
	if err != nil {
		t.Fatalf("create routed bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "unrouted work",
		Type:   "task",
		Status: "open",
	}); err != nil {
		t.Fatalf("create unrouted bead: %v", err)
	}
	got, _ := collectAssignedWorkBeads(&config.City{}, store, nil, nil)
	if len(got) != 1 {
		t.Fatalf("collectAssignedWorkBeads returned %d beads, want 1", len(got))
	}
	if got[0].ID != routed.ID {
		t.Fatalf("collectAssignedWorkBeads returned %q, want %q", got[0].ID, routed.ID)
	}
}

func TestBuildDesiredState_SingletonTemplateDoesNotRealizeDependencyPoolFloorWithoutSession(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "db",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
			{
				Name:      "api",
				DependsOn: []string{"db"},
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	desired := dsResult.State
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
				Name:              "db",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
			{
				Name:              "api",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
				DependsOn: []string{"db"},
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	desired := dsResult.State
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
				Name:              "db",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
			{
				Name:      "api",
				Suspended: true,
				DependsOn: []string{"db"},
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	desired := dsResult.State
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
				Name:              "db",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
			{
				Name:              "api",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
				DependsOn: []string{"db"},
			},
			{
				Name:      "web",
				DependsOn: []string{"api"},
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	desired := dsResult.State
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
				Name:              "db",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
			{
				Name:              "helper",
				Suspended:         true,
				MaxActiveSessions: intPtr(1),
				DependsOn:         []string{"db"},
				StartCommand:      "echo",
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State
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
				Name:              "db",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
			{
				Name:              "api",
				DependsOn:         []string{"db"},
				StartCommand:      "echo",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State
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

func TestBuildDesiredState_DrainedPoolManagedSessionIsNotRediscovered(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "claude",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:claude"},
		Metadata: map[string]string{
			"template":     "claude",
			"agent_name":   "claude",
			"session_name": "s-gc-drained",
			"state":        "asleep",
			"sleep_reason": "drained",
			"pool_managed": "true",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:              "claude",
			MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5),
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State

	if _, ok := desired["s-gc-drained"]; ok {
		t.Fatalf("drained pool-managed session should not be rediscovered into desired state")
	}

	claudeSessions := 0
	for _, tp := range desired {
		if tp.TemplateName == "claude" {
			claudeSessions++
		}
	}
	if claudeSessions != 1 {
		t.Fatalf("claude desired sessions = %d, want 1", claudeSessions)
	}
}

func TestBuildDesiredState_UsesBeadNamedPoolSessionsForScaleCheckDemand(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title: "queued worker job",
		Metadata: map[string]string{
			"gc.routed_to": "worker",
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Demand is supplied by the explicit scale_check here. This test only
	// verifies that pool sessions created under demand use bead-derived names
	// and pool-managed metadata, not that routed work itself increments demand.
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "worker",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "echo 1",
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State
	if len(desired) != 1 {
		t.Fatalf("desired sessions = %d, want 1", len(desired))
	}

	var (
		sessionName string
		tp          TemplateParams
	)
	for sn, got := range desired {
		sessionName = sn
		tp = got
	}
	if tp.TemplateName != "worker" {
		t.Fatalf("TemplateName = %q, want worker", tp.TemplateName)
	}
	if !strings.HasPrefix(sessionName, "worker-") {
		t.Fatalf("session name = %q, want worker-<beadID>", sessionName)
	}
	if strings.HasSuffix(sessionName, "-1") {
		t.Fatalf("session name = %q, want bead-derived name instead of slot alias", sessionName)
	}

	sessionBeads, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("ListByLabel(%q): %v", sessionBeadLabel, err)
	}
	if len(sessionBeads) != 1 {
		t.Fatalf("session bead count = %d, want 1", len(sessionBeads))
	}
	if got := sessionBeads[0].Metadata["session_name"]; got != sessionName {
		t.Fatalf("stored session_name = %q, want %q", got, sessionName)
	}
	if got := sessionBeads[0].Metadata[poolManagedMetadataKey]; got != "true" {
		t.Fatalf("pool_managed = %q, want true", got)
	}
}

func TestBuildDesiredState_FallsBackToLegacyPoolDemandWhenListFails(t *testing.T) {
	cityPath := t.TempDir()
	memStore := beads.NewMemStore()
	store := listFailStore{Store: memStore}
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "worker",
				MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(1),
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State
	// With min=1, max=1: both the singleton path and the pool-floor path
	// may contribute a session, yielding 1 or 2 desired entries depending
	// on timing. Accept either.
	if len(desired) < 1 || len(desired) > 2 {
		t.Fatalf("desired sessions = %d, want 1 or 2", len(desired))
	}
	// At least one session should have a worker-prefixed name.
	found := false
	for sn := range desired {
		if strings.HasPrefix(sn, "worker") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no worker-prefixed session in desired: %v", desired)
	}
}

func TestBuildDesiredState_DependencyFloorDoesNotReuseRegularPoolWorkerBead(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "worker active",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"template":             "worker",
			"session_name":         "worker-existing",
			"agent_name":           "worker",
			"state":                "active",
			"pool_slot":            "1",
			poolManagedMetadataKey: "true",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"template":     "helper",
			"session_name": "helper-session",
			"state":        "creating",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "worker",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3),
			},
			{
				Name:         "helper",
				Suspended:    true,
				DependsOn:    []string{"worker"},
				StartCommand: "echo",
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State
	if _, ok := desired["worker-existing"]; ok {
		t.Fatalf("dependency floor reused regular worker bead: keys=%v", mapKeys(desired))
	}
	workerSessions := 0
	for sn, tp := range desired {
		if tp.TemplateName != "worker" {
			continue
		}
		workerSessions++
		if sn == "worker-existing" {
			t.Fatalf("dependency floor kept regular worker bead %q desired", sn)
		}
	}
	if workerSessions != 1 {
		t.Fatalf("worker desired sessions = %d, want 1; desired keys=%v", workerSessions, mapKeys(desired))
	}
}

func TestBuildDesiredState_DoesNotCreateDuplicatePoolBeadForDiscoveredSession(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "worker session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":             "worker",
			"session_name":         "worker-gc-existing",
			"manual_session":       "true",
			poolManagedMetadataKey: "true",
			"state":                "creating",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "worker",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3),
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State
	if _, ok := desired["worker-gc-existing"]; !ok {
		t.Fatalf("desired state missing discovered pool session: keys=%v", mapKeys(desired))
	}

	sessionBeads, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("ListByLabel(%q): %v", sessionBeadLabel, err)
	}
	if len(sessionBeads) != 1 {
		t.Fatalf("session bead count = %d, want 1 (no duplicate bead)", len(sessionBeads))
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
				Name:              "db",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
			{
				Name:              "api",
				DependsOn:         []string{"db"},
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State
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

func TestBuildDesiredState_PoolCheckInjectsDoltPortForRigScopedAgent(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "myrig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	// The check command outputs "2" only when BEADS_DOLT_PORT is set.
	// If the fix works, buildDesiredState prefixes the command with
	// BEADS_DOLT_PORT=9876, so the inner shell sees the variable.
	checkCmd := `sh -c 'test -n "$BEADS_DOLT_PORT" && printf 2 || printf 0'`
	cfg := &config.City{
		Rigs: []config.Rig{{
			Name:     "myrig",
			Path:     rigPath,
			DoltPort: "9876",
		}},
		Agents: []config.Agent{
			{
				Name:              "worker",
				Dir:               "myrig",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5), ScaleCheck: checkCmd,
			},
		},
	}

	desired := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	workerSlots := 0
	for _, tp := range desired.State {
		if tp.TemplateName == "myrig/worker" {
			workerSlots++
		}
	}
	if workerSlots != 2 {
		t.Fatalf("worker desired slots = %d, want 2 (BEADS_DOLT_PORT injection should make check output 2)", workerSlots)
	}
}

func TestBuildDesiredState_PoolCheckOmitsDoltPortForCityScopedAgent(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := t.TempDir()
	// Same check command but for a city-scoped agent (no rig). BEADS_DOLT_PORT
	// should NOT be injected, so the check outputs 0.
	checkCmd := `sh -c 'test -n "$BEADS_DOLT_PORT" && printf 2 || printf 0'`
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "worker",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5), ScaleCheck: checkCmd,
			},
		},
	}

	desired := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	workerSlots := 0
	for _, tp := range desired.State {
		if tp.TemplateName == "worker" {
			workerSlots++
		}
	}
	if workerSlots != 0 {
		t.Fatalf("worker desired slots = %d, want 0 (no DoltPort for city-scoped agent)", workerSlots)
	}
}

func TestBuildDesiredState_PoolCheckUsesManagedCityDoltPortWhenRigHasNoOverride(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "myrig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	ln := listenOnRandomPort(t)
	defer func() {
		if err := ln.Close(); err != nil {
			t.Fatalf("close listener: %v", err)
		}
	}()
	if err := writeDoltState(cityPath, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	checkCmd := `sh -c 'test -n "$BEADS_DOLT_PORT" && printf 2 || printf 0'`
	cfg := &config.City{
		Rigs: []config.Rig{{
			Name: "myrig",
			Path: rigPath,
		}},
		Agents: []config.Agent{
			{
				Name:              "worker",
				Dir:               "myrig",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5), ScaleCheck: checkCmd,
			},
		},
	}

	desired := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	workerSlots := 0
	for _, tp := range desired.State {
		if tp.TemplateName == "myrig/worker" {
			workerSlots++
		}
	}
	if workerSlots != 2 {
		t.Fatalf("worker desired slots = %d, want 2 (managed city dolt port should be injected for rig)", workerSlots)
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
				Name:              "db",
				Dir:               "payments",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
			{
				Name:              "api",
				Dir:               "payments",
				DependsOn:         []string{"payments/db"},
				StartCommand:      "echo",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State
	if _, ok := desired["s-gc-300"]; ok {
		t.Fatalf("manual pool session in suspended rig should not enter desired state: %+v", desired["s-gc-300"])
	}
	for _, tp := range desired {
		if tp.TemplateName == "payments/db" {
			t.Fatalf("suspended-rig manual session should not hold dependency floor: %+v", tp)
		}
	}
}

func TestSelectOrCreatePoolSessionBead_SkipsDrained(t *testing.T) {
	store := beads.NewMemStore()
	drained, err := store.Create(beads.Bead{
		Title:  "claude",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "claude",
			"agent_name":   "claude",
			"session_name": "claude-drained",
			"state":        "drained",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.add(drained)
	cfgAgent := config.Agent{Name: "claude", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, err := selectOrCreatePoolSessionBead(bp, "claude", nil, map[string]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	if result.ID == drained.ID {
		t.Fatal("should not reuse drained session bead for new-tier request")
	}
}

func TestSelectOrCreatePoolSessionBead_ReusesPreferredDrained(t *testing.T) {
	store := beads.NewMemStore()
	drained, err := store.Create(beads.Bead{
		Title:  "claude",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "claude",
			"agent_name":   "claude",
			"session_name": "claude-drained",
			"state":        "drained",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.add(drained)
	cfgAgent := config.Agent{Name: "claude", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, err := selectOrCreatePoolSessionBead(bp, "claude", &drained, map[string]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	if result.ID != drained.ID {
		t.Fatal("resume tier should reuse preferred drained session bead")
	}
}

func TestSelectOrCreateDependencyPoolSessionBead_SkipsDrained(t *testing.T) {
	store := beads.NewMemStore()
	drained, err := store.Create(beads.Bead{
		Title:  "claude",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":        "claude",
			"agent_name":      "claude",
			"session_name":    "claude-dep-drained",
			"state":           "asleep",
			"sleep_reason":    "drained",
			"dependency_only": "true",
			"pool_managed":    "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.add(drained)
	cfgAgent := config.Agent{Name: "claude", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, err := selectOrCreateDependencyPoolSessionBead(bp, &cfgAgent, "claude")
	if err != nil {
		t.Fatalf("selectOrCreateDependencyPoolSessionBead: %v", err)
	}
	if result.ID == drained.ID {
		t.Fatal("should not reuse drained dependency session bead for generic dependency demand")
	}
}

func TestSelectOrCreatePoolSessionBead_ReusesAvailableForNewTier(t *testing.T) {
	store := beads.NewMemStore()
	// Existing awake session bead without assigned work — should be reused
	// for new-tier to prevent session bead duplication across ticks.
	awake, err := store.Create(beads.Bead{
		Title:  "claude",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "claude",
			"agent_name":   "claude",
			"session_name": "claude-awake",
			"state":        "awake",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.add(awake)
	cfgAgent := config.Agent{Name: "claude", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, err := selectOrCreatePoolSessionBead(bp, "claude", nil, map[string]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	if result.ID != awake.ID {
		t.Fatal("new-tier should reuse available (non-drained) session bead")
	}
}

func TestSelectOrCreatePoolSessionBead_SkipsAsleepBeads(t *testing.T) {
	// An asleep pool session should NOT be reused for new demand.
	// The reconciler should create a fresh session instead.
	// This prevents a deadlock where an asleep bead fills a pool slot
	// but ComputeAwakeSet correctly refuses to wake it (asleep
	// ephemerals are not reused).
	store := beads.NewMemStore()
	cfgAgent := config.Agent{Name: "polecat", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}

	asleep, err := store.Create(beads.Bead{
		Title:  "polecat",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:polecat"},
		Metadata: map[string]string{
			"template":     "polecat",
			"session_name": "polecat-mc-old",
			"state":        "asleep",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot := newSessionBeadSnapshot([]beads.Bead{asleep})
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, err := selectOrCreatePoolSessionBead(bp, "polecat", nil, map[string]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	if result.ID == asleep.ID {
		t.Fatal("asleep pool session should not be reused — a fresh session should be created instead")
	}
}

func TestSelectOrCreatePoolSessionBead_ReusesActiveBeforeCreatingNew(t *testing.T) {
	// An active (awake) pool session IS reused — no fresh bead created.
	store := beads.NewMemStore()
	cfgAgent := config.Agent{Name: "polecat", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}

	active, err := store.Create(beads.Bead{
		Title:  "polecat",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:polecat"},
		Metadata: map[string]string{
			"template":     "polecat",
			"session_name": "polecat-mc-live",
			"state":        "active",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot := newSessionBeadSnapshot([]beads.Bead{active})
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, err := selectOrCreatePoolSessionBead(bp, "polecat", nil, map[string]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	if result.ID != active.ID {
		t.Fatalf("active pool session should be reused, got %s want %s", result.ID, active.ID)
	}
}

func TestSelectOrCreatePoolSessionBead_ReusesCreatingBeforeCreatingNew(t *testing.T) {
	// A creating pool session IS reused — no fresh bead created.
	store := beads.NewMemStore()
	cfgAgent := config.Agent{Name: "polecat", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}

	creating, err := store.Create(beads.Bead{
		Title:  "polecat",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:polecat"},
		Metadata: map[string]string{
			"template":     "polecat",
			"session_name": "polecat-mc-new",
			"state":        "creating",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot := newSessionBeadSnapshot([]beads.Bead{creating})
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, err := selectOrCreatePoolSessionBead(bp, "polecat", nil, map[string]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	if result.ID != creating.ID {
		t.Fatalf("creating pool session should be reused, got %s want %s", result.ID, creating.ID)
	}
}

func TestSelectOrCreatePoolSessionBead_SkipsAsleepButReusesActive(t *testing.T) {
	// With both an asleep and active bead for the same template,
	// the active one is reused and the asleep one is ignored.
	store := beads.NewMemStore()
	cfgAgent := config.Agent{Name: "polecat", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}

	asleep, err := store.Create(beads.Bead{
		Title:  "polecat",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:polecat"},
		Metadata: map[string]string{
			"template":     "polecat",
			"session_name": "polecat-mc-old",
			"state":        "asleep",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	active, err := store.Create(beads.Bead{
		Title:  "polecat",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:polecat"},
		Metadata: map[string]string{
			"template":     "polecat",
			"session_name": "polecat-mc-live",
			"state":        "active",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot := newSessionBeadSnapshot([]beads.Bead{asleep, active})
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, err := selectOrCreatePoolSessionBead(bp, "polecat", nil, map[string]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	if result.ID == asleep.ID {
		t.Fatal("should skip asleep bead")
	}
	if result.ID != active.ID {
		t.Fatalf("should reuse active bead, got %s want %s", result.ID, active.ID)
	}
}

// PR #216 — skipped for now. Cross-rig pool work visibility is a new
// feature, not a bug fix. Left as open PR for discussion about the
// gastown experience with this flow.
