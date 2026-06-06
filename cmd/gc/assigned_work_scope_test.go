package main

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestFilterAssignedWorkBeadsForSessionWakeKeepsOnlyReachableAssigneeSources(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name: "worker",
			Dir:  "riga",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Dir:      "riga",
			Mode:     "on_demand",
		}},
	}
	sessions := []beads.Bead{{
		ID:     "session-1",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":                  "riga/worker",
			"session_name":              "worker-session",
			"configured_named_identity": "riga/worker",
		},
	}}
	work := []beads.Bead{
		{ID: "city-named", Status: "open", Assignee: "riga/worker"},
		{ID: "rig-named", Status: "open", Assignee: "riga/worker"},
		{ID: "city-session", Status: "in_progress", Assignee: "session-1"},
		{ID: "rig-session", Status: "in_progress", Assignee: "session-1"},
	}
	storeRefs := []string{"", "riga", "", "riga"}

	got := filterAssignedWorkBeadsForSessionWake(cfg, cityPath, sessions, work, storeRefs)

	if len(got) != 2 {
		t.Fatalf("filtered work length = %d, want 2: %#v", len(got), got)
	}
	if got[0].ID != "rig-named" || got[1].ID != "rig-session" {
		t.Fatalf("filtered work IDs = [%s %s], want [rig-named rig-session]", got[0].ID, got[1].ID)
	}
}

func TestFilterAssignedWorkBeadsForSessionWakeCityScopedAgentIsCrossStoreEligible(t *testing.T) {
	// vp-kvp: a city-scoped singleton legitimately serves per-rig routed work.
	// Its assigned work may live in ANY store, so reachability must federate
	// across stores — gating it to its own configured rig is the cross-store
	// dead-drop this fixes. Rig-scoped agents stay single-store (unchanged).
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name:  "auditor",
			Scope: "city",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "auditor",
			Scope:    "city",
			Mode:     "on_demand",
		}},
	}
	identity := cfg.NamedSessions[0].QualifiedName()
	work := []beads.Bead{
		{ID: "city-work", Status: "open", Assignee: identity},
		{ID: "rig-work", Status: "open", Assignee: identity},
	}
	storeRefs := []string{"", "riga"} // city store + rig store

	got := filterAssignedWorkBeadsForSessionWake(cfg, cityPath, nil, work, storeRefs)

	if len(got) != 2 {
		t.Fatalf("city-scoped %q must be reachable from BOTH stores; got %d: %#v", identity, len(got), got)
	}
}

func TestFilterAssignedWorkBeadsForPoolDemandKeepsDirectAssigneeAfterTemplateFallback(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{
			Name: "worker",
		}},
	}
	sessions := []beads.Bead{{
		ID:     "session-1",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":     "worker",
			"session_name": "worker-session",
		},
	}}
	work := []beads.Bead{{
		ID:       "direct-assigned",
		Status:   "in_progress",
		Assignee: "session-1",
		Metadata: map[string]string{},
	}}

	got := filterAssignedWorkBeadsForPoolDemand(cfg, "", sessions, work, []string{""})

	if len(got) != 1 || got[0].ID != "direct-assigned" {
		t.Fatalf("filtered work = %#v, want direct-assigned work preserved through template fallback", got)
	}
}

func TestFilterAssignedWorkBeadsForPoolDemandKeepsLegacyWorkflowRunTarget(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{
			Name: "worker",
		}},
	}
	work := []beads.Bead{{
		ID:       "legacy-workflow-root",
		Status:   "in_progress",
		Assignee: "worker-dead",
		Metadata: map[string]string{
			"gc.kind":       "workflow",
			"gc.run_target": "worker",
		},
	}}

	got := filterAssignedWorkBeadsForPoolDemand(cfg, "", nil, work, []string{""})

	if len(got) != 1 || got[0].ID != "legacy-workflow-root" {
		t.Fatalf("filtered work = %#v, want legacy workflow root preserved through run_target fallback", got)
	}
}

func TestFilterAssignedWorkBeadsForPoolDemandDropsDirectAssigneeFromUnreachableStore(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name: "worker",
		}},
	}
	sessions := []beads.Bead{{
		ID:     "session-1",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":     "worker",
			"session_name": "worker-session",
		},
	}}
	work := []beads.Bead{{
		ID:       "rig-direct-assigned",
		Status:   "in_progress",
		Assignee: "session-1",
		Metadata: map[string]string{},
	}}

	got := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, sessions, work, []string{"riga"})

	if len(got) != 0 {
		t.Fatalf("filtered work = %#v, want unreachable rig-store direct assignment dropped", got)
	}
}

func TestSessionHasOpenAssignedWorkUsesOnlyReachableStore(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name: "worker",
			Dir:  "riga",
		}},
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	session := beads.Bead{
		ID:     "session-1",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"template":     "riga/worker",
			"session_name": "worker-session",
		},
	}
	if _, err := cityStore.Create(beads.Bead{
		ID:       "city-work",
		Type:     "task",
		Status:   "open",
		Assignee: session.ID,
	}); err != nil {
		t.Fatalf("Create city work: %v", err)
	}

	has, err := sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, cityStore, map[string]beads.Store{"riga": rigStore}, session)
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForReachableStore: %v", err)
	}
	if has {
		t.Fatal("city-store assigned work should not count for a rig-scoped session")
	}

	if _, err := rigStore.Create(beads.Bead{
		ID:       "rig-work",
		Type:     "task",
		Status:   "open",
		Assignee: session.ID,
	}); err != nil {
		t.Fatalf("Create rig work: %v", err)
	}
	has, err = sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, cityStore, map[string]beads.Store{"riga": rigStore}, session)
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForReachableStore: %v", err)
	}
	if !has {
		t.Fatal("rig-store assigned work should count for a rig-scoped session")
	}
}

func TestSessionHasOpenAssignedWorkMatchesConfiguredNamedSessionRuntimeFallback(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:        "worker",
			BindingName: "pack",
		}},
		NamedSessions: []config.NamedSession{{
			Template:    "worker",
			BindingName: "pack",
			Mode:        "on_demand",
		}},
	}
	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "pack.worker")
	store := beads.NewMemStore()
	session := beads.Bead{
		ID:     "session-1",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"template":                   "pack.worker",
			"session_name":               sessionName,
			namedSessionMetadataKey:      "true",
			namedSessionModeMetadata:     "on_demand",
			namedSessionIdentityMetadata: "",
		},
	}
	if _, err := store.Create(beads.Bead{
		ID:       "named-work",
		Type:     "task",
		Status:   "open",
		Assignee: "pack.worker",
	}); err != nil {
		t.Fatalf("Create named work: %v", err)
	}

	has, err := sessionHasOpenAssignedWorkForReachableStore("", cfg, store, nil, session)
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForReachableStore: %v", err)
	}
	if !has {
		t.Fatal("configured named-session runtime-name fallback assignment should count as open assigned work")
	}
}

func TestSessionAssignmentIdentifiersForConfigConfiguredNamedSessionFallbackIsConservative(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:        "worker",
			BindingName: "pack",
		}},
		NamedSessions: []config.NamedSession{{
			Template:    "worker",
			BindingName: "pack",
			Mode:        "on_demand",
		}},
	}
	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "pack.worker")

	tests := []struct {
		name    string
		session beads.Bead
	}{
		{
			name: "identity metadata already present",
			session: beads.Bead{
				ID: "session-with-identity",
				Metadata: map[string]string{
					"template":                   "pack.worker",
					"session_name":               sessionName,
					namedSessionMetadataKey:      "true",
					namedSessionIdentityMetadata: "pack.other",
				},
			},
		},
		{
			name: "template mismatch",
			session: beads.Bead{
				ID: "session-template-mismatch",
				Metadata: map[string]string{
					"template":                   "pack.other",
					"session_name":               sessionName,
					namedSessionMetadataKey:      "true",
					namedSessionIdentityMetadata: "",
				},
			},
		},
		{
			name: "runtime name mismatch",
			session: beads.Bead{
				ID: "session-runtime-mismatch",
				Metadata: map[string]string{
					"template":                   "pack.worker",
					"session_name":               "different-session",
					namedSessionMetadataKey:      "true",
					namedSessionIdentityMetadata: "",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, identifier := range sessionAssignmentIdentifiersForConfig(tt.session, cfg) {
				if identifier == "pack.worker" {
					t.Fatalf("identifiers include configured identity %q for conservative mismatch case: %v", identifier, sessionAssignmentIdentifiersForConfig(tt.session, cfg))
				}
			}
		})
	}
}

func TestAgentReachesWorkflowStore(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "alpha")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "alpha", Path: rigPath}},
	}
	hqAgent := &config.Agent{Name: "mayor"}
	rigAgent := &config.Agent{Name: "polecat", Dir: "alpha"}

	cases := []struct {
		name     string
		storeRef string
		agent    *config.Agent
		want     bool
	}{
		{name: "hq agent reaches city store", storeRef: "city:test-city", agent: hqAgent, want: true},
		{name: "hq agent cannot reach rig store", storeRef: "rig:alpha", agent: hqAgent, want: false},
		{name: "rig agent reaches own rig store", storeRef: "rig:alpha", agent: rigAgent, want: true},
		{name: "rig agent cannot reach city store", storeRef: "city:test-city", agent: rigAgent, want: false},
		{name: "rig agent cannot reach a different rig", storeRef: "rig:beta", agent: rigAgent, want: false},
		{name: "empty storeRef is unreachable for rig agent", storeRef: "", agent: rigAgent, want: false},
		{name: "empty storeRef is unreachable for hq agent", storeRef: "", agent: hqAgent, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentutil.AgentReachesWorkflowStore(tc.storeRef, tc.agent, cityPath, cfg); got != tc.want {
				t.Fatalf("AgentReachesWorkflowStore(%q, %q) = %v, want %v", tc.storeRef, tc.agent.Name, got, tc.want)
			}
		})
	}

	if !agentutil.AgentReachesWorkflowStore("city:test-city", nil, cityPath, cfg) {
		t.Fatal("nil agent should permissively reach any store")
	}
	if !agentutil.AgentReachesWorkflowStore("rig:alpha", rigAgent, cityPath, nil) {
		t.Fatal("nil cfg should permissively reach any store")
	}
}

func TestAgentReachableStoreLabel(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "alpha")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "alpha", Path: rigPath}},
	}
	hqAgent := &config.Agent{Name: "mayor"}
	rigAgent := &config.Agent{Name: "polecat", Dir: "alpha"}

	if got := agentutil.AgentReachableStoreLabel(hqAgent, cityPath, "test-city", cfg); got != "city:test-city" {
		t.Errorf("hq agent label = %q, want city:test-city", got)
	}
	if got := agentutil.AgentReachableStoreLabel(rigAgent, cityPath, "test-city", cfg); got != "rig:alpha" {
		t.Errorf("rig agent label = %q, want rig:alpha", got)
	}
	if got := agentutil.AgentReachableStoreLabel(hqAgent, cityPath, "", cfg); got != "city:city" {
		t.Errorf("hq agent label with empty cityName = %q, want city:city", got)
	}
	if got := agentutil.AgentReachableStoreLabel(nil, cityPath, "test-city", cfg); got != "" {
		t.Errorf("nil agent label = %q, want empty", got)
	}
	if got := agentutil.AgentReachableStoreLabel(hqAgent, cityPath, "test-city", nil); got != "" {
		t.Errorf("nil cfg label = %q, want empty", got)
	}
}

func TestSessionHasOpenAssignedWorkIncludesReachableAssignedWisp(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name: "worker",
			Dir:  "riga",
		}},
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	session := beads.Bead{
		ID:     "session-1",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"template":     "riga/worker",
			"session_name": "worker-session",
		},
	}
	wisp, err := rigStore.Create(beads.Bead{
		ID:        "rig-wisp-work",
		Title:     "active workflow step",
		Type:      "task",
		Status:    "in_progress",
		Assignee:  session.Metadata["session_name"],
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create rig wisp work: %v", err)
	}
	inProgress := "in_progress"
	if err := rigStore.Update(wisp.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark rig wisp in progress: %v", err)
	}

	has, err := sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, cityStore, map[string]beads.Store{"riga": rigStore}, session)
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForReachableStore: %v", err)
	}
	if !has {
		t.Fatal("reachable assigned wisp work should count before closing a session")
	}
}

func TestFirstOpenAssignedWorkBeadIncludesAssignedWisp(t *testing.T) {
	store := beads.NewMemStore()
	wisp, err := store.Create(beads.Bead{
		Title:     "active workflow step",
		Type:      "task",
		Assignee:  "worker-session",
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create wisp work: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(wisp.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark wisp in progress: %v", err)
	}

	got, found, err := firstOpenAssignedWorkBeadInStoreByIdentifiers(store, []string{"worker-session"})
	if err != nil {
		t.Fatalf("firstOpenAssignedWorkBeadInStoreByIdentifiers: %v", err)
	}
	if !found {
		t.Fatal("assigned wisp work should be found for session diagnostics")
	}
	if got.ID != wisp.ID {
		t.Fatalf("first assigned work ID = %q, want %q", got.ID, wisp.ID)
	}
}

func TestResolveTaskWorkDirIncludesAssignedWisp(t *testing.T) {
	workDir := t.TempDir()
	store := beads.NewMemStore()
	wisp, err := store.Create(beads.Bead{
		Title:     "active workflow step",
		Type:      "task",
		Assignee:  "worker-session",
		Metadata:  map[string]string{"work_dir": workDir},
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create wisp work: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(wisp.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark wisp in progress: %v", err)
	}

	if got := resolveTaskWorkDir(store, "worker-session"); got != workDir {
		t.Fatalf("resolveTaskWorkDir = %q, want assigned wisp work_dir %q", got, workDir)
	}
}
