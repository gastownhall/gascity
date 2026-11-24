package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worktree"
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

// A parked seat waking for its pre-assigned work resumes its conversation,
// whose transcript is filed under the dir it ran in, so its asleep resume
// keeps the recorded work dir as a live continuation's does (PR #8110 review
// 5592555088). The live rule's guards hold: another row's resume, a
// fresh-wake row, a request that is not a resume, or no parked verdict
// re-derives the dir.
func TestComputePoolTriggerBindingPatchParkedResumeKeepsRecordedWorkDir(t *testing.T) {
	recorded := filepath.Join("worktrees", "fi-old-title")
	derived := filepath.Join("integration", "worker")
	info := session.Info{
		ID:               "sess-1",
		State:            session.StateAsleep,
		TriggerBeadID:    "fi-old",
		WorkDirCanonical: recorded,
		WorkDir:          recorded,
	}
	parked := SessionRequest{Tier: "resume", SessionBeadID: "sess-1", WorkBeadID: "fi-new", ParkedContinuation: true}

	patch := computePoolTriggerBindingPatch(info, parked, derived)
	if got := patch[beadmeta.TriggerBeadIDMetadataKey]; got != "fi-new" {
		t.Errorf("parked resume trigger patch = %q, want its pin fi-new", got)
	}
	for _, key := range []string{beadmeta.WorkDirMetadataKey, beadmeta.LegacyWorkDirMetadataKey} {
		if got, ok := patch[key]; ok {
			t.Errorf("parked resume %s patch = %q, want the recorded %q kept", key, got, recorded)
		}
	}
	for name, mutate := range map[string]func(*session.Info, *SessionRequest){
		"another row's resume":   func(_ *session.Info, r *SessionRequest) { r.SessionBeadID = "sess-2" },
		"a fresh-wake row":       func(i *session.Info, _ *SessionRequest) { i.WakeMode = "fresh" },
		"a request not a resume": func(_ *session.Info, r *SessionRequest) { r.Tier = "wake-known-identity" },
		"no parked verdict":      func(_ *session.Info, r *SessionRequest) { r.ParkedContinuation = false },
	} {
		i, r := info, parked
		mutate(&i, &r)
		if got := computePoolTriggerBindingPatch(i, r, derived)[beadmeta.WorkDirMetadataKey]; got != derived {
			t.Errorf("%s: gc.work_dir patch = %q, want the derived %q", name, got, derived)
		}
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

// wakeKnownEvidenceBead provisions a real managed worktree for beadID the way
// `gc worktree ensure` does, publishes its complete ownership evidence on an
// in_progress bead routed to template and assigned to assignee, and returns
// the bead and its verified path. With incomplete evidence (one of the nine
// ownership keys missing) the published evidence is the shape
// worktreeSpecForBead fails closed on.
func wakeKnownEvidenceBead(t *testing.T, beadID, template, assignee string, incomplete bool) (beads.Bead, string) {
	t.Helper()
	repo, base := worktreeTestRepo(t)
	root := t.TempDir()
	path := filepath.Join(root, beadID)
	spec := worktree.Spec{
		RepoDir: repo, Root: root, Path: path, Branch: "wt/task/" + beadID, Base: base,
		BeadID: beadID, StoreRef: "rig:r1", Creator: "gc-sling", Owner: "gc-sling",
		Generation: "1", Lifecycle: worktree.LifecycleActive,
	}
	report, err := worktree.Ensure(spec)
	if err != nil {
		t.Fatalf("worktree.Ensure: %v", err)
	}
	b := workBead(beadID, template, assignee, "in_progress", 1)
	meta := map[string]string{
		beadmeta.RootStoreRefMetadataKey:       "rig:r1",
		beadmeta.WorkDirMetadataKey:            path,
		beadmeta.WorktreeRepoMetadataKey:       repo,
		beadmeta.WorktreeRootMetadataKey:       root,
		beadmeta.WorkBranchMetadataKey:         spec.Branch,
		beadmeta.WorktreeBaseRefMetadataKey:    base,
		beadmeta.WorktreeBaseSHAMetadataKey:    report.Provenance.BaseSHA,
		beadmeta.WorktreeCreatorMetadataKey:    spec.Creator,
		beadmeta.WorktreeOwnerMetadataKey:      spec.Owner,
		beadmeta.WorktreeGenerationMetadataKey: spec.Generation,
	}
	if !incomplete {
		meta[beadmeta.WorktreeLifecycleMetadataKey] = spec.Lifecycle
	}
	for k, v := range meta {
		b.Metadata[k] = v
	}
	return b, path
}

// TestWakeKnownCleanCreateStartsInVerifiedWorktree pins the realize half of
// gastownhall/gascity#5753 for the wake-known-identity tier: the clean create
// planned for a bead that names a verified managed worktree starts there, not
// in the pool's own directory, and a bead whose evidence is incomplete is
// skipped rather than bound to a fallback directory.
func TestWakeKnownCleanCreateStartsInVerifiedWorktree(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{
		Name: "worker", StartCommand: "true",
		MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3),
	}}}
	bead, path := wakeKnownEvidenceBead(t, "gc-b", "worker", "worker", false)

	states := ComputePoolDesiredStates(cfg, []beads.Bead{bead}, nil, nil)
	if len(states) != 1 || len(states[0].Requests) != 1 {
		t.Fatalf("desired states = %#v, want one wake-known-identity request", states)
	}
	request := states[0].Requests[0]
	if request.Tier != "wake-known-identity" || request.SessionBeadID != "" {
		t.Fatalf("request = %#v, want a clean create (wake-known-identity, no session)", request)
	}
	rows, _, stderr := realizeFreshRequest(t, request)
	if len(rows) != 1 {
		t.Fatalf("realization created %d sessions; stderr=%s", len(rows), stderr)
	}
	if got := rows[0].TriggerBeadID; got != "gc-b" {
		t.Fatalf("created session trigger = %q, want gc-b", got)
	}
	if got := preparedStartSessionWorkDir(rows[0]); got != path {
		t.Fatalf("created session work dir = %q, want the verified worktree %q, not the pool directory", got, path)
	}

	// Incomplete evidence: the request is still emitted (with the failure
	// carried, not resolved), and realization skips the item rather than
	// creating a seat in the pool's own directory.
	broken, _ := wakeKnownEvidenceBead(t, "gc-c", "worker", "worker", true)
	states = ComputePoolDesiredStates(cfg, []beads.Bead{broken}, nil, nil)
	if len(states) != 1 || len(states[0].Requests) != 1 {
		t.Fatalf("desired states = %#v, want the request still emitted", states)
	}
	request = states[0].Requests[0]
	if request.WorktreeError == "" || request.WorktreeSpec != nil {
		t.Fatalf("request = %#v, want a nil spec and a non-empty WorktreeError", request)
	}
	rows, _, stderr = realizeFreshRequest(t, request)
	if len(rows) != 0 {
		t.Fatalf("incomplete evidence created %d sessions; want the item skipped", len(rows))
	}
	if !strings.Contains(stderr, "(skipping)") {
		t.Fatalf("stderr = %q, want the (skipping) diagnostic", stderr)
	}
}
