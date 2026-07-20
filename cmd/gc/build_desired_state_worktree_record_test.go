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

// TestBindPoolSessionTriggerBead_PreservesLauncherWorktreePath is the
// regression guard for sc-s5mfl3: the recorded gc.work_dir must match the
// worktree the launcher actually created, including the work-bead title slug
// suffix (e.g. "dip-<bead>-implement-compound-work-item").
//
// The launcher derives the worktree name from the work bead's id+title slug
// when the session is first created ("new" tier, title known). On a subsequent
// reconcile the same session may be re-bound to the same trigger bead via a
// partial or legacy request that does not carry the title. The
// pre-fix code unconditionally re-derived the work_dir from the incomplete
// request, producing the suffix-less "dip-<bead>" form and clobbering the
// launcher-created path that was already recorded. The build-artifact-valid
// gate then chdirs into a path that never existed.
//
// The fix makes the recorded launcher path the single source of truth: for an
// unchanged trigger bead, the already-recorded work_dir is preserved rather
// than re-derived from a request that may be missing the title.
func TestBindPoolSessionTriggerBead_PreservesLauncherWorktreePath(t *testing.T) {
	const (
		workBead = "dip-42"
		title    = "implement compound work item"
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

	// The base workspace root the launcher joins the trigger slug under.
	base := filepath.Join(bp.cityPath, ".gc", "workspaces", "worker")
	// What the launcher actually creates the first time, title in hand.
	launcherCreated := filepath.Join(base, "dip-42-implement-compound-work-item")

	// 1. First bind: "new" tier carries the title. This mirrors the launcher's
	//    own path derivation, so the recorded work_dir == the created worktree.
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
		WorkBeadTitle: title,
	})
	if err != nil {
		t.Fatalf("first bind: %v", err)
	}
	recorded := bound.WorkDirCanonical
	if recorded != launcherCreated {
		t.Fatalf("first-bind work_dir = %q, want launcher-created %q", recorded, launcherCreated)
	}

	// 2. Re-bind on the next reconcile with a deliberately partial request for
	//    the SAME trigger bead. The recorded launcher path must survive even
	//    when a legacy or external caller omits WorkBeadTitle.
	reBound, err := bindPoolSessionTriggerBead(bp, &cfg.Agents[0], "worker", bound, SessionRequest{
		Tier:       "wake-known-identity",
		WorkBeadID: workBead,
		// WorkBeadTitle intentionally empty.
	})
	if err != nil {
		t.Fatalf("re-bind: %v", err)
	}

	got := reBound.WorkDirCanonical
	if got != launcherCreated {
		t.Fatalf("re-bind dropped the title suffix:\n  recorded = %q\n  created  = %q\nrecorded path never existed", got, launcherCreated)
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

// TestAssignedPoolResumeRebindCarriesTitleForLauncherWorktree reproduces the
// build-basic failure from nightly run 29385443130. The pool session was first
// launched for fi-kar in its title-qualified worktree, then transiently bound
// to fi-43h while concurrent cold-pool demand was being reconciled. The
// provider stayed in the original fi-kar worktree. When fi-kar became assigned
// back to the session, currently_processing_bead_id had not been stamped yet,
// so the resume path had to re-derive the launcher's exact cwd from the work
// bead identity carried by its SessionRequest.
func TestAssignedPoolResumeRebindCarriesTitleForLauncherWorktree(t *testing.T) {
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
	launcherWorkDir := filepath.Join(base, "fi-kar-implement-owned-work")
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
	if got := rebound.WorkDirCanonical; got != launcherWorkDir {
		t.Errorf("resume gc.work_dir = %q, want launcher cwd %q", got, launcherWorkDir)
	}
	if got := rebound.WorkDir; got != launcherWorkDir {
		t.Errorf("resume work_dir = %q, want launcher cwd %q", got, launcherWorkDir)
	}
}

// TestWakeKnownIdentityRequestCarriesWorkBeadTitle pins the other resume-like
// request producer. A closed or lost pool session must launch the replacement
// into the same title-qualified worktree shape as an ordinary cold-pool create.
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
