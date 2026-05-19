package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func TestCityStatusNamedSessionsUseProvidedStore(t *testing.T) {
	sp := runtime.NewFake()
	dops := newFakeDrainOps()
	store := beads.NewMemStore()

	oldOpen := openCityStoreAtForStatus
	openCityStoreAtForStatus = func(string) (beads.Store, error) {
		return store, nil
	}
	t.Cleanup(func() { openCityStoreAtForStatus = oldOpen })

	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"configured_named_session":  "true",
			"configured_named_identity": "refinery",
			"configured_named_mode":     "on_demand",
		},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Agents:    []config.Agent{{Name: "refinery"}},
		NamedSessions: []config.NamedSession{{
			Template: "refinery",
		}},
	}
	var stdout, stderr bytes.Buffer
	cityPath := filepath.Join(t.TempDir(), "city")
	snapshot := collectCityStatusSnapshot(sp, cfg, cityPath, store, &stderr)
	if len(snapshot.NamedSessions) != 1 {
		t.Fatalf("named sessions = %d, want 1", len(snapshot.NamedSessions))
	}
	if snapshot.NamedSessions[0].Status != "materialized" {
		t.Fatalf("named session status = %q, want materialized", snapshot.NamedSessions[0].Status)
	}
	code := doCityStatus(sp, dops, cfg, cityPath, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Named sessions:") {
		t.Fatalf("stdout missing named sessions section, got:\n%s", out)
	}
	if !strings.Contains(out, "materialized (on_demand)") {
		t.Fatalf("stdout = %q, want materialized named session status", out)
	}
}

func TestCityStatusSnapshotNilConfigUsesCityPathName(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city")
	snapshot := collectCityStatusSnapshot(runtime.NewFake(), nil, cityPath, nil, io.Discard)
	if snapshot.CityName != "city" {
		t.Fatalf("CityName = %q, want city", snapshot.CityName)
	}
}

func TestCityStatusJSONPreservesNilAgentsWhenEmpty(t *testing.T) {
	status := cityStatusJSONFromSnapshot(cityStatusSnapshot{CityName: "city"}, StatusSummaryJSON{})
	if status.Agents != nil {
		t.Fatalf("Agents = %#v, want nil slice", status.Agents)
	}
}

type failingStatusStore struct {
	*beads.MemStore
	failID string
	err    error
}

func (s *failingStatusStore) Get(id string) (beads.Bead, error) {
	if id == s.failID {
		return beads.Bead{}, s.err
	}
	return s.MemStore.Get(id)
}

type getSpyStatusStore struct {
	*beads.MemStore
	ids []string
}

func (s *getSpyStatusStore) Get(id string) (beads.Bead, error) {
	s.ids = append(s.ids, id)
	return s.MemStore.Get(id)
}

func TestCityStatusAgentObservationDoesNotResolveRuntimeNamesThroughStore(t *testing.T) {
	sp := runtime.NewFake()
	store := &getSpyStatusStore{MemStore: beads.NewMemStore()}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Agents: []config.Agent{
			{Name: "dog", MaxActiveSessions: intPtr(2)},
		},
	}

	snapshot := collectCityStatusSnapshot(sp, cfg, "/home/user/city", store, io.Discard)
	if len(snapshot.Agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(snapshot.Agents))
	}
	if len(store.ids) != 0 {
		t.Fatalf("status observation performed bead Get calls for runtime names: %v", store.ids)
	}
}

func TestCityStatusUsesBeadBackedRuntimeNameForSingletonAgent(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "custom-mayor", runtime.Config{Command: "echo"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "mayor",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "agent:mayor"},
		Metadata: map[string]string{
			"agent_name":   "mayor",
			"template":     "mayor",
			"session_name": "custom-mayor",
			"state":        "active",
		},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Agents:    []config.Agent{{Name: "mayor", MaxActiveSessions: intPtr(1)}},
	}

	snapshot := collectCityStatusSnapshot(sp, cfg, "/home/user/city", store, io.Discard)
	if len(snapshot.Agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(snapshot.Agents))
	}
	if !snapshot.Agents[0].Agent.Running {
		t.Fatalf("singleton agent running = false, want true with bead-backed runtime name")
	}
	if got := snapshot.Agents[0].SessionName; got != "custom-mayor" {
		t.Fatalf("SessionName = %q, want %q", got, "custom-mayor")
	}
}

func TestCityStatusUsesSessionBackedObservationForSuspendedCustomRuntimeName(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "custom-mayor", runtime.Config{Command: "echo"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "mayor",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "agent:mayor"},
		Metadata: map[string]string{
			"agent_name":   "mayor",
			"template":     "mayor",
			"session_name": "custom-mayor",
			"state":        string(session.StateSuspended),
		},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Agents:    []config.Agent{{Name: "mayor", MaxActiveSessions: intPtr(1)}},
	}

	snapshot := collectCityStatusSnapshot(sp, cfg, "/home/user/city", store, io.Discard)
	if len(snapshot.Agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(snapshot.Agents))
	}
	if !snapshot.Agents[0].Agent.Running {
		t.Fatalf("running = false, want true")
	}
	if !snapshot.Agents[0].Agent.Suspended {
		t.Fatalf("suspended = false, want true from session-backed observation")
	}
}

func TestCityStatusUsesStatusSnapshotToRouteACPDrainMetadata(t *testing.T) {
	oldBuild := buildSessionProviderByName
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })

	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	buildSessionProviderByName = func(name string, _ config.SessionConfig, _, _ string) (runtime.Provider, error) {
		if name == "acp" {
			return acpSP, nil
		}
		return defaultSP, nil
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Session:   config.SessionConfig{Provider: "fake"},
		Agents:    []config.Agent{{Name: "reviewer", Session: "acp", MaxActiveSessions: intPtr(1)}},
	}
	sp := newStatusSessionProviderForCity(cfg, t.TempDir())
	if err := acpSP.Start(context.Background(), "custom-reviewer", runtime.Config{Command: "echo"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := acpSP.SetMeta("custom-reviewer", "GC_DRAIN", "123"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}

	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "reviewer",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "agent:reviewer"},
		Metadata: map[string]string{
			"agent_name":   "reviewer",
			"template":     "reviewer",
			"transport":    "acp",
			"session_name": "custom-reviewer",
			"state":        string(session.StateActive),
		},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	snapshot := collectCityStatusSnapshot(sp, cfg, "/home/user/city", store, io.Discard)
	if len(snapshot.Agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(snapshot.Agents))
	}
	if !snapshot.Agents[0].Agent.Running {
		t.Fatalf("running = false, want true")
	}

	var stdout bytes.Buffer
	renderCityStatusText(snapshot, newDrainOps(sp), &stdout)
	if !strings.Contains(stdout.String(), "running  (draining)") {
		t.Fatalf("stdout = %q, want draining status for ACP-backed custom runtime name", stdout.String())
	}
}

func TestCityStatusUsesBeadBackedRuntimeNameForPoolInstance(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "custom-dog-1", runtime.Config{Command: "echo"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "dog",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "agent:dog-1"},
		Metadata: map[string]string{
			"agent_name":   "dog-1",
			"template":     "dog",
			"session_name": "custom-dog-1",
			"pool_slot":    "1",
			"state":        "active",
		},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Agents: []config.Agent{
			{Name: "dog", MaxActiveSessions: intPtr(2)},
		},
	}

	snapshot := collectCityStatusSnapshot(sp, cfg, "/home/user/city", store, io.Discard)
	if len(snapshot.Agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(snapshot.Agents))
	}
	if got := snapshot.Agents[0].Agent.QualifiedName; got != "dog-1" {
		t.Fatalf("first QualifiedName = %q, want dog-1", got)
	}
	if !snapshot.Agents[0].Agent.Running {
		t.Fatalf("pool instance dog-1 running = false, want true with bead-backed runtime name")
	}
	if got := snapshot.Agents[0].SessionName; got != "custom-dog-1" {
		t.Fatalf("dog-1 SessionName = %q, want %q", got, "custom-dog-1")
	}
	if snapshot.Agents[1].Agent.Running {
		t.Fatalf("pool instance dog-2 running = true, want false")
	}
}

func TestCityStatusUsesBeadBackedRuntimeNameForStampedPoolSlotBead(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "custom-dog-1", runtime.Config{Command: "echo"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "dog",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "agent:frontend/dog"},
		Metadata: map[string]string{
			"agent_name":   "frontend/dog",
			"template":     "frontend/dog",
			"session_name": "custom-dog-1",
			"pool_slot":    "1",
			"state":        "active",
		},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Rigs:      []config.Rig{{Name: "frontend", Path: "/tmp/frontend"}},
		Agents: []config.Agent{
			{Name: "dog", Dir: "frontend", MaxActiveSessions: intPtr(2)},
		},
	}

	snapshot := collectCityStatusSnapshot(sp, cfg, "/home/user/city", store, io.Discard)
	if len(snapshot.Agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(snapshot.Agents))
	}
	if got := snapshot.Agents[0].Agent.QualifiedName; got != "frontend/dog-1" {
		t.Fatalf("first QualifiedName = %q, want frontend/dog-1", got)
	}
	if !snapshot.Agents[0].Agent.Running {
		t.Fatalf("pool instance frontend/dog-1 running = false, want true with stamped pool-slot bead")
	}
	if got := snapshot.Agents[0].SessionName; got != "custom-dog-1" {
		t.Fatalf("frontend/dog-1 SessionName = %q, want %q", got, "custom-dog-1")
	}
}

func TestCityStatusNamedSessionLookupErrorsAreSurfaced(t *testing.T) {
	sp := runtime.NewFake()
	dops := newFakeDrainOps()
	store := &failingStatusStore{
		MemStore: beads.NewMemStore(),
		failID:   "refinery",
		err:      errors.New("store offline"),
	}

	oldOpen := openCityStoreAtForStatus
	openCityStoreAtForStatus = func(string) (beads.Store, error) {
		return store, nil
	}
	t.Cleanup(func() { openCityStoreAtForStatus = oldOpen })

	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		NamedSessions: []config.NamedSession{{
			Template: "refinery",
		}},
	}

	var stdout, stderr bytes.Buffer
	snapshot := collectCityStatusSnapshot(sp, cfg, "/home/user/city", store, &stderr)
	if len(snapshot.NamedSessions) != 1 {
		t.Fatalf("named sessions = %d, want 1", len(snapshot.NamedSessions))
	}
	if got := snapshot.NamedSessions[0].Status; !strings.HasPrefix(got, "lookup error:") {
		t.Fatalf("snapshot named session status = %q, want lookup error", got)
	}

	code := doCityStatus(sp, dops, cfg, "/home/user/city", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "lookup error:") || !strings.Contains(out, "store offline") {
		t.Fatalf("stdout = %q, want surfaced store error", out)
	}
}
