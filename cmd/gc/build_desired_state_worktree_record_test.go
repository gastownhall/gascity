package main

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func TestPoolTriggerWorkDirNonPackUsesConfiguredBase(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "fixture"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			WorkDir:           ".gc/workspaces/{{.AgentBase}}",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(1),
		}},
	}
	bp := newAgentBuildParams("fixture", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), beads.NewMemStore(), &bytes.Buffer{})
	want := filepath.Join(bp.cityPath, ".gc", "workspaces", "worker")

	got := poolTriggerWorkDir(bp, &cfg.Agents[0], "worker", SessionRequest{
		WorkBeadID:    "ga-123",
		WorkBeadTitle: "ordinary repository work",
	})
	if got != want {
		t.Fatalf("ordinary non-pack work dir = %q, want configured base %q", got, want)
	}
}

func TestBindPoolSessionTriggerBeadUsesExplicitWorkspace(t *testing.T) {
	const (
		workBead  = "dip-42"
		workspace = "dip-42-implement-compound-work-item"
	)

	cfg := &config.City{
		Workspace: config.Workspace{Name: "dip"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			WorkDir:           ".gc/workspaces/{{.AgentBase}}",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(1),
		}},
	}
	var stderr bytes.Buffer
	store := beads.NewMemStore()
	bp := newAgentBuildParams("dip", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)

	base := filepath.Join(bp.cityPath, ".gc", "workspaces", "worker")
	launcherCreated := filepath.Join(base, workspace)

	created, err := store.Create(beads.Bead{ID: "sess-1", Type: "session"})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	info, err := sessionFrontDoor(store).Get(created.ID)
	if err != nil {
		t.Fatalf("get session info: %v", err)
	}
	bound, err := bindPoolSessionTriggerBead(bp, &cfg.Agents[0], "worker", info, SessionRequest{
		Tier:          "new",
		WorkBeadID:    workBead,
		WorkWorkspace: workspace,
	})
	if err != nil {
		t.Fatalf("first bind: %v", err)
	}
	recorded := bound.WorkDirCanonical
	if recorded != launcherCreated {
		t.Fatalf("first-bind work_dir = %q, want launcher-created %q", recorded, launcherCreated)
	}

	reBound, err := bindPoolSessionTriggerBead(bp, &cfg.Agents[0], "worker", bound, SessionRequest{
		Tier:          "wake-known-identity",
		WorkBeadID:    workBead,
		WorkWorkspace: workspace,
	})
	if err != nil {
		t.Fatalf("re-bind: %v", err)
	}

	got := reBound.WorkDirCanonical
	if got != launcherCreated {
		t.Fatalf("re-bind work_dir = %q, want explicit workspace %q", got, launcherCreated)
	}
	if reBound.WorkDir != launcherCreated {
		t.Fatalf("re-bind legacy work_dir = %q, want %q", reBound.WorkDir, launcherCreated)
	}

	// The store copy must agree with the returned bead.
	persisted, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got := persisted.Metadata[beadmeta.WorkDirMetadataKey]; got != launcherCreated {
		t.Fatalf("persisted work_dir = %q, want %q", got, launcherCreated)
	}
	if got := persisted.Metadata[beadmeta.LegacyWorkDirMetadataKey]; got != launcherCreated {
		t.Fatalf("persisted legacy work_dir = %q, want %q", got, launcherCreated)
	}
}

// TestAssignedActivePoolResumePreservesConcreteWorkDir verifies that rebinding
// an already-running worker cannot rewrite the recorded cwd out from under the
// live process, even when the currently-processing marker has not caught up.
func TestAssignedActivePoolResumePreservesConcreteWorkDir(t *testing.T) {
	const (
		assignedWorkID  = "fi-kar"
		assignedTitle   = "Implement owned work"
		transientWorkID = "fi-43h"
	)

	cfg := &config.City{
		Workspace: config.Workspace{Name: "fixture"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			WorkDir:           ".gc/workspaces/{{.AgentBase}}",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(1),
		}},
	}
	var stderr bytes.Buffer
	store := beads.NewMemStore()
	bp := newAgentBuildParams("fixture", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	base := filepath.Join(bp.cityPath, ".gc", "workspaces", "worker")
	transientWorkDir := filepath.Join(base, "fi-43h-implement-owned-work")

	created, err := store.Create(beads.Bead{
		Title:  "worker active session",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":                        "worker",
			"agent_name":                      "worker-1",
			"alias":                           "worker-1",
			"session_name":                    "worker-active",
			"state":                           "awake",
			"pool_slot":                       "1",
			poolManagedMetadataKey:            boolMetadata(true),
			beadmeta.TriggerBeadIDMetadataKey: transientWorkID,
			beadmeta.WorkDirMetadataKey:       transientWorkDir,
			beadmeta.LegacyWorkDirMetadataKey: transientWorkDir,
			// currently_processing_bead_id intentionally absent: in the
			// preserved failure it arrived after this reconcile completed.
		},
	})
	if err != nil {
		t.Fatalf("create active session: %v", err)
	}
	info, err := sessionFrontDoor(store).Get(created.ID)
	if err != nil {
		t.Fatalf("get active session: %v", err)
	}
	priority := 1
	assigned := beads.Bead{
		ID:       assignedWorkID,
		Title:    assignedTitle,
		Status:   "in_progress",
		Assignee: created.ID,
		Priority: &priority,
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: "worker",
		},
	}

	states := ComputePoolDesiredStates(cfg, []beads.Bead{assigned}, []session.Info{info}, nil)
	if len(states) != 1 || len(states[0].Requests) != 1 {
		t.Fatalf("desired states = %#v, want one resume request", states)
	}
	request := states[0].Requests[0]
	if request.Tier != "resume" || request.SessionBeadID != created.ID || request.WorkBeadID != assignedWorkID {
		t.Fatalf("request = %#v, want concrete resume of %s for %s", request, created.ID, assignedWorkID)
	}
	if got := request.WorkBeadTitle; got != assignedTitle {
		t.Errorf("resume WorkBeadTitle = %q, want %q", got, assignedTitle)
	}

	rebound, err := bindPoolSessionTriggerBead(bp, &cfg.Agents[0], "worker", info, request)
	if err != nil {
		t.Fatalf("bind assigned work: %v", err)
	}
	if got := rebound.WorkDirCanonical; got != transientWorkDir {
		t.Errorf("active resume gc.work_dir = %q, want live process cwd %q", got, transientWorkDir)
	}
	if got := rebound.WorkDir; got != transientWorkDir {
		t.Errorf("active resume work_dir = %q, want live process cwd %q", got, transientWorkDir)
	}
}

func TestComputePoolTriggerBindingPatchAsleepResumeUsesConfiguredBase(t *testing.T) {
	legacyWorkDir := filepath.Join("legacy", "fi-old-title")
	configuredBase := filepath.Join("integration", "worker")
	info := session.Info{
		ID:               "sess-1",
		State:            session.StateAsleep,
		TriggerBeadID:    "fi-old",
		WorkDirCanonical: legacyWorkDir,
		WorkDir:          legacyWorkDir,
	}
	request := SessionRequest{
		Tier:          "resume",
		SessionBeadID: "sess-1",
		WorkBeadID:    "fi-new",
	}

	patch := computePoolTriggerBindingPatch(info, request, configuredBase)
	if got := patch[beadmeta.WorkDirMetadataKey]; got != configuredBase {
		t.Errorf("asleep resume gc.work_dir patch = %q, want configured base %q", got, configuredBase)
	}
	if got := patch[beadmeta.LegacyWorkDirMetadataKey]; got != configuredBase {
		t.Errorf("asleep resume work_dir patch = %q, want configured base %q", got, configuredBase)
	}
}

// TestWakeKnownIdentityRequestCarriesWorkBeadTitle pins the complete trigger
// identity supplied to a replacement session, independent of workdir routing.
func TestWakeKnownIdentityRequestCarriesWorkBeadTitle(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{
		Name:              "worker",
		StartCommand:      "true",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(1),
	}}}
	priority := 1
	assigned := beads.Bead{
		ID:       "fi-kar",
		Title:    "  Implement owned work  ",
		Status:   "in_progress",
		Assignee: "worker",
		Priority: &priority,
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: "worker",
		},
	}

	states := ComputePoolDesiredStates(cfg, []beads.Bead{assigned}, nil, nil)
	if len(states) != 1 || len(states[0].Requests) != 1 {
		t.Fatalf("desired states = %#v, want one wake-known-identity request", states)
	}
	request := states[0].Requests[0]
	if request.Tier != "wake-known-identity" || request.WorkBeadID != assigned.ID {
		t.Fatalf("request = %#v, want wake-known-identity for %s", request, assigned.ID)
	}
	if got, want := request.WorkBeadTitle, "Implement owned work"; got != want {
		t.Errorf("wake-known-identity WorkBeadTitle = %q, want %q", got, want)
	}
}
