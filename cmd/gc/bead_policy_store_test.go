package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

type captureCreateStore struct {
	beads.Store
	created  []beads.Bead
	storages []beads.StorageClass
}

func (s *captureCreateStore) Create(b beads.Bead) (beads.Bead, error) {
	s.created = append(s.created, b)
	s.storages = append(s.storages, beads.StorageDefault)
	return s.Store.Create(b)
}

func (s *captureCreateStore) CreateWithStorage(b beads.Bead, storage beads.StorageClass) (beads.Bead, error) {
	s.created = append(s.created, b)
	s.storages = append(s.storages, storage)
	return s.Store.Create(b)
}

type captureGraphStore struct {
	beads.Store
	plan    *beads.GraphApplyPlan
	storage beads.StorageClass
}

func underlyingPolicyStoreForTest(store beads.Store) beads.Store {
	base, _, _ := unwrapBeadPolicyStore(store)
	return base
}

func TestBeadPolicyStorePreservesConditionalAssignmentReleaser(t *testing.T) {
	backing := beads.NewMemStore()
	wrapped := wrapStoreWithBeadPolicies(backing, nil)
	releaser, ok := wrapped.(beads.ConditionalAssignmentReleaser)
	if !ok {
		t.Fatalf("wrapped store implements ConditionalAssignmentReleaser = false")
	}
	bead, err := wrapped.Create(beads.Bead{Title: "work", Assignee: "worker-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	status := "in_progress"
	if err := wrapped.Update(bead.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update status: %v", err)
	}

	released, err := releaser.ReleaseIfCurrent(bead.ID, "worker-1")
	if err != nil {
		t.Fatalf("ReleaseIfCurrent: %v", err)
	}
	if !released {
		t.Fatal("ReleaseIfCurrent released = false, want true")
	}
	got, err := wrapped.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("released bead = %+v, want open and unassigned", got)
	}
}

func (s *captureGraphStore) ApplyGraphPlan(_ context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	next := *plan
	s.plan = &next
	ids := make(map[string]string, len(plan.Nodes))
	for _, node := range plan.Nodes {
		ids[node.Key] = "bd-" + node.Key
	}
	return &beads.GraphApplyResult{IDs: ids}, nil
}

func (s *captureGraphStore) ApplyGraphPlanWithStorage(_ context.Context, plan *beads.GraphApplyPlan, storage beads.StorageClass) (*beads.GraphApplyResult, error) {
	s.storage = storage
	return s.ApplyGraphPlan(context.Background(), plan)
}

func TestBeadPolicyStoreAppliesDefaultStorageForAllowlistedCreates(t *testing.T) {
	backing := &captureCreateStore{Store: beads.NewMemStore()}
	store := wrapStoreWithBeadPolicies(backing, &config.City{})

	cases := []struct {
		name string
		bead beads.Bead
		want string
	}{
		{
			name: "session",
			bead: beads.Bead{Title: "session", Type: session.BeadType, Labels: []string{session.LabelSession}},
			want: beadStorageNoHistory,
		},
		{
			name: "wait",
			bead: beads.Bead{Title: "wait", Type: session.WaitBeadType, Labels: []string{session.WaitBeadLabel}},
			want: beadStorageNoHistory,
		},
		{
			name: "nudge",
			bead: beads.Bead{Title: "nudge", Type: nudgeBeadType, Labels: []string{nudgeBeadLabel}},
			want: beadStorageNoHistory,
		},
		{
			name: "order tracking",
			bead: beads.Bead{Title: "order:daily", Labels: []string{"order-run:daily", labelOrderTracking}},
			want: beadStorageNoHistory,
		},
		{
			name: "workflow root",
			bead: beads.Bead{Title: "workflow", Metadata: map[string]string{
				"gc.kind":             "workflow",
				"gc.formula_contract": "graph.v2",
			}},
			want: beadStorageHistory,
		},
		{
			name: "workflow child",
			bead: beads.Bead{Title: "workflow child", Metadata: map[string]string{
				"gc.root_bead_id": "bd-root",
			}},
			want: beadStorageHistory,
		},
		{
			name: "wisp root",
			bead: beads.Bead{Title: "wisp", Metadata: map[string]string{"gc.kind": "wisp"}},
			want: beadStorageHistory,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			backing.created = nil
			backing.storages = nil
			if _, err := store.Create(tt.bead); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if len(backing.created) != 1 {
				t.Fatalf("captured creates = %d, want 1", len(backing.created))
			}
			assertStorageClass(t, backing.storages[0], tt.want)
		})
	}
}

func TestBeadPolicyStoreBD105OptInUsesFastDefaultStorage(t *testing.T) {
	backing := &captureCreateStore{Store: beads.NewMemStore()}
	store := wrapStoreWithBeadPolicies(backing, &config.City{
		Beads: config.BeadsConfig{BDCompatibility: config.BeadsBDCompatibility105},
	})

	cases := []struct {
		name string
		bead beads.Bead
		want string
	}{
		{
			name: "wisp root",
			bead: beads.Bead{Title: "wisp", Metadata: map[string]string{"gc.kind": "wisp"}},
			want: beadStorageEphemeral,
		},
		{
			name: "workflow graph work",
			bead: beads.Bead{Title: "workflow", Metadata: map[string]string{
				"gc.kind":             "workflow",
				"gc.formula_contract": "graph.v2",
			}},
			want: beadStorageNoHistory,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			backing.created = nil
			backing.storages = nil
			if _, err := store.Create(tt.bead); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if len(backing.storages) != 1 {
				t.Fatalf("captured storages = %d, want 1", len(backing.storages))
			}
			assertStorageClass(t, backing.storages[0], tt.want)
		})
	}
}

func TestBeadPolicyStoreLeavesOrdinaryWorkInHistory(t *testing.T) {
	backing := &captureCreateStore{Store: beads.NewMemStore()}
	store := wrapStoreWithBeadPolicies(backing, &config.City{})

	if _, err := store.Create(beads.Bead{
		Title: "source work",
		Type:  "task",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(backing.storages) != 1 {
		t.Fatalf("captured storages = %d, want 1", len(backing.storages))
	}
	if backing.storages[0] != beads.StorageDefault {
		t.Fatalf("ordinary work storage = %q, want default history path", backing.storages[0])
	}
}

func TestBeadPolicyStoreStorageOverrides(t *testing.T) {
	backing := &captureCreateStore{Store: beads.NewMemStore()}
	store := wrapStoreWithBeadPolicies(backing, &config.City{
		Beads: config.BeadsConfig{Policies: map[string]config.BeadPolicyConfig{
			beadPolicySession:       {Storage: beadStorageHistory},
			beadPolicyOrderTracking: {Storage: beadStorageHistory},
			beadPolicyWorkflow:      {Storage: beadStorageHistory},
			beadPolicyWisp:          {Storage: beadStorageHistory},
		}},
	})

	if _, err := store.Create(beads.Bead{
		Title:     "session",
		Type:      session.BeadType,
		Labels:    []string{session.LabelSession},
		Ephemeral: true,
	}); err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	assertStorageClass(t, backing.storages[0], beadStorageHistory)

	if _, err := store.Create(beads.Bead{
		Title:  "order:daily",
		Labels: []string{"order-run:daily", labelOrderTracking},
	}); err != nil {
		t.Fatalf("Create(order tracking): %v", err)
	}
	assertStorageClass(t, backing.storages[1], beadStorageHistory)

	if _, err := store.Create(beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
	}); err != nil {
		t.Fatalf("Create(workflow): %v", err)
	}
	assertStorageClass(t, backing.storages[2], beadStorageHistory)

	if _, err := store.Create(beads.Bead{
		Title:    "wisp",
		Metadata: map[string]string{"gc.kind": "wisp"},
	}); err != nil {
		t.Fatalf("Create(wisp): %v", err)
	}
	assertStorageClass(t, backing.storages[3], beadStorageHistory)
}

func TestBeadPolicyStoreAppliesWispRootStorageToSequentialChildren(t *testing.T) {
	backing := &captureCreateStore{Store: beads.NewMemStore()}
	store := wrapStoreWithBeadPolicies(backing, &config.City{})

	root, err := store.Create(beads.Bead{Title: "root", Metadata: map[string]string{"gc.kind": "wisp"}})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:    "child",
		Metadata: map[string]string{"gc.root_bead_id": root.ID},
	}); err != nil {
		t.Fatalf("Create(child): %v", err)
	}

	if len(backing.created) != 2 {
		t.Fatalf("captured creates = %d, want 2", len(backing.created))
	}
	assertStorageClass(t, backing.storages[0], beadStorageHistory)
	assertStorageClass(t, backing.storages[1], beadStorageHistory)
}

func TestBeadPolicyGraphStoreAppliesWispStorageToGraphPlan(t *testing.T) {
	backing := &captureGraphStore{Store: beads.NewMemStore()}
	store := wrapStoreWithBeadPolicies(backing, &config.City{})
	applier, ok := store.(beads.GraphApplyStore)
	if !ok {
		t.Fatal("wrapped graph store does not implement GraphApplyStore")
	}

	_, err := applier.ApplyGraphPlan(context.Background(), &beads.GraphApplyPlan{
		Nodes: []beads.GraphApplyNode{
			{Key: "root", Title: "Root", Metadata: map[string]string{"gc.kind": "wisp"}},
			{Key: "child", Title: "Child", MetadataRefs: map[string]string{"gc.root_bead_id": "root"}},
		},
	})
	if err != nil {
		t.Fatalf("ApplyGraphPlan: %v", err)
	}
	if backing.plan == nil {
		t.Fatal("graph plan was not captured")
	}
	assertStorageClass(t, backing.storage, beadStorageHistory)
}

func TestBeadPolicyGraphStoreAppliesWorkflowStorageToGraphPlan(t *testing.T) {
	backing := &captureGraphStore{Store: beads.NewMemStore()}
	store := wrapStoreWithBeadPolicies(backing, &config.City{})
	applier, ok := store.(beads.GraphApplyStore)
	if !ok {
		t.Fatal("wrapped graph store does not implement GraphApplyStore")
	}

	_, err := applier.ApplyGraphPlan(context.Background(), &beads.GraphApplyPlan{
		Nodes: []beads.GraphApplyNode{
			{Key: "root", Title: "Root", Metadata: map[string]string{
				"gc.kind":             "workflow",
				"gc.formula_contract": "graph.v2",
			}},
			{Key: "child", Title: "Child", MetadataRefs: map[string]string{"gc.root_bead_id": "root"}},
		},
	})
	if err != nil {
		t.Fatalf("ApplyGraphPlan: %v", err)
	}
	if backing.plan == nil {
		t.Fatal("graph plan was not captured")
	}
	assertStorageClass(t, backing.storage, beadStorageHistory)
}

func TestBeadPolicyGraphStoreAppliesWorkflowStorageToFragmentGraphPlan(t *testing.T) {
	backing := &captureGraphStore{Store: beads.NewMemStore()}
	store := wrapStoreWithBeadPolicies(backing, &config.City{})
	applier, ok := store.(beads.GraphApplyStore)
	if !ok {
		t.Fatal("wrapped graph store does not implement GraphApplyStore")
	}

	_, err := applier.ApplyGraphPlan(context.Background(), &beads.GraphApplyPlan{
		Nodes: []beads.GraphApplyNode{
			{Key: "child", Title: "Child", Metadata: map[string]string{"gc.root_bead_id": "bd-root"}},
		},
	})
	if err != nil {
		t.Fatalf("ApplyGraphPlan: %v", err)
	}
	if backing.plan == nil {
		t.Fatal("graph plan was not captured")
	}
	assertStorageClass(t, backing.storage, beadStorageHistory)
}

func TestBeadPolicyGraphStoreBD105OptInUsesFastDefaultStorage(t *testing.T) {
	tests := []struct {
		name string
		plan *beads.GraphApplyPlan
		want string
	}{
		{
			name: "wisp graph",
			plan: &beads.GraphApplyPlan{
				Nodes: []beads.GraphApplyNode{
					{Key: "root", Title: "Root", Metadata: map[string]string{"gc.kind": "wisp"}},
					{Key: "child", Title: "Child", MetadataRefs: map[string]string{"gc.root_bead_id": "root"}},
				},
			},
			want: beadStorageEphemeral,
		},
		{
			name: "workflow graph",
			plan: &beads.GraphApplyPlan{
				Nodes: []beads.GraphApplyNode{
					{Key: "root", Title: "Root", Metadata: map[string]string{
						"gc.kind":             "workflow",
						"gc.formula_contract": "graph.v2",
					}},
					{Key: "child", Title: "Child", MetadataRefs: map[string]string{"gc.root_bead_id": "root"}},
				},
			},
			want: beadStorageNoHistory,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backing := &captureGraphStore{Store: beads.NewMemStore()}
			store := wrapStoreWithBeadPolicies(backing, &config.City{
				Beads: config.BeadsConfig{BDCompatibility: config.BeadsBDCompatibility105},
			})
			applier, ok := store.(beads.GraphApplyStore)
			if !ok {
				t.Fatal("wrapped graph store does not implement GraphApplyStore")
			}

			_, err := applier.ApplyGraphPlan(context.Background(), tt.plan)
			if err != nil {
				t.Fatalf("ApplyGraphPlan: %v", err)
			}
			if backing.plan == nil {
				t.Fatal("graph plan was not captured")
			}
			assertStorageClass(t, backing.storage, tt.want)
		})
	}
}

func TestBeadPolicyGraphStorePreservesGraphApplyThroughCachingStore(t *testing.T) {
	backing := &captureGraphStore{Store: beads.NewMemStore()}
	cache := beads.NewCachingStoreForTest(backing, nil)
	store := wrapStoreWithBeadPolicies(cache, &config.City{
		Beads: config.BeadsConfig{BDCompatibility: config.BeadsBDCompatibility105},
	})
	applier, ok := store.(beads.GraphApplyStore)
	if !ok {
		t.Fatal("policy-wrapped cached graph store does not implement GraphApplyStore")
	}

	_, err := applier.ApplyGraphPlan(context.Background(), &beads.GraphApplyPlan{
		Nodes: []beads.GraphApplyNode{
			{Key: "root", Title: "Root", Metadata: map[string]string{"gc.kind": "wisp"}},
			{Key: "child", Title: "Child", MetadataRefs: map[string]string{"gc.root_bead_id": "root"}},
		},
	})
	if err != nil {
		t.Fatalf("ApplyGraphPlan: %v", err)
	}
	if backing.plan == nil {
		t.Fatal("graph plan was not captured")
	}
	assertStorageClass(t, backing.storage, beadStorageEphemeral)
}

func TestBeadPolicyGraphStoreRejectsNoHistoryWispOverride(t *testing.T) {
	backing := &captureGraphStore{Store: beads.NewMemStore()}
	store := wrapStoreWithBeadPolicies(backing, &config.City{
		Beads: config.BeadsConfig{Policies: map[string]config.BeadPolicyConfig{
			beadPolicyWisp: {Storage: beadStorageNoHistory},
		}},
	})
	applier, ok := store.(beads.GraphApplyStore)
	if !ok {
		t.Fatal("wrapped graph store does not implement GraphApplyStore")
	}

	_, err := applier.ApplyGraphPlan(context.Background(), &beads.GraphApplyPlan{
		Nodes: []beads.GraphApplyNode{
			{Key: "root", Title: "Root", Metadata: map[string]string{"gc.kind": "wisp"}},
			{Key: "child", Title: "Child", MetadataRefs: map[string]string{"gc.root_bead_id": "root"}},
		},
	})
	if err != nil {
		t.Fatalf("ApplyGraphPlan: %v", err)
	}
	assertStorageClass(t, backing.storage, beadStorageHistory)
}

func TestBeadPolicyStoreIgnoresUnsafeStorageOverrides(t *testing.T) {
	tests := []struct {
		name       string
		policyName string
		storage    string
		bead       beads.Bead
		want       string
	}{
		{
			name:       "wisp cannot be forced no-history under bd 1.0.4",
			policyName: beadPolicyWisp,
			storage:    beadStorageNoHistory,
			bead:       beads.Bead{Title: "wisp", Metadata: map[string]string{"gc.kind": "wisp"}},
			want:       beadStorageHistory,
		},
		{
			name:       "wisp cannot be forced ephemeral under bd 1.0.4",
			policyName: beadPolicyWisp,
			storage:    beadStorageEphemeral,
			bead:       beads.Bead{Title: "wisp", Metadata: map[string]string{"gc.kind": "wisp"}},
			want:       beadStorageHistory,
		},
		{
			name:       "session cannot be forced ephemeral",
			policyName: beadPolicySession,
			storage:    beadStorageEphemeral,
			bead:       beads.Bead{Title: "session", Type: session.BeadType, Labels: []string{session.LabelSession}},
			want:       beadStorageNoHistory,
		},
		{
			name:       "order tracking cannot be forced ephemeral",
			policyName: beadPolicyOrderTracking,
			storage:    beadStorageEphemeral,
			bead:       beads.Bead{Title: "order:daily", Labels: []string{"order-run:daily", labelOrderTracking}},
			want:       beadStorageNoHistory,
		},
		{
			name:       "workflow cannot be forced no-history under bd 1.0.4",
			policyName: beadPolicyWorkflow,
			storage:    beadStorageNoHistory,
			bead: beads.Bead{Title: "workflow", Metadata: map[string]string{
				"gc.kind":             "workflow",
				"gc.formula_contract": "graph.v2",
			}},
			want: beadStorageHistory,
		},
		{
			name:       "workflow cannot be forced ephemeral",
			policyName: beadPolicyWorkflow,
			storage:    beadStorageEphemeral,
			bead: beads.Bead{Title: "workflow", Metadata: map[string]string{
				"gc.kind":             "workflow",
				"gc.formula_contract": "graph.v2",
			}},
			want: beadStorageHistory,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backing := &captureCreateStore{Store: beads.NewMemStore()}
			store := wrapStoreWithBeadPolicies(backing, &config.City{
				Beads: config.BeadsConfig{Policies: map[string]config.BeadPolicyConfig{
					tt.policyName: {Storage: tt.storage},
				}},
			})

			if _, err := store.Create(tt.bead); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if len(backing.storages) != 1 {
				t.Fatalf("captured storage count = %d, want 1", len(backing.storages))
			}
			assertStorageClass(t, backing.storages[0], tt.want)
		})
	}
}

func TestPolicyReadPathsIncludeHistoryAndNoHistoryRows(t *testing.T) {
	runner := func(_ string, name string, args ...string) ([]byte, error) {
		cmd := name + " " + strings.Join(args, " ")
		switch cmd {
		case "bd list --json --type=session --include-infra --include-gates --limit 0",
			"bd list --json --label=gc:session --include-infra --include-gates --limit 0":
			return []byte(`[
				{"id":"bd-old-session","title":"old session","status":"open","issue_type":"session","created_at":"2026-05-01T00:00:00Z","labels":["gc:session"],"metadata":{"session_name":"old"}},
				{"id":"bd-new-session","title":"new session","status":"open","issue_type":"session","created_at":"2026-05-01T00:00:01Z","labels":["gc:session"],"metadata":{"session_name":"new"},"no_history":true}
			]`), nil
		case "bd list --json --label=gc:wait --include-infra --include-gates --limit 0",
			"bd list --json --label=gc:wait --include-infra --include-gates --limit 1001":
			return []byte(`[
				{"id":"bd-old-wait","title":"old wait","status":"open","issue_type":"gate","created_at":"2026-05-01T00:00:00Z","labels":["gc:wait"],"metadata":{"session_id":"s1"}},
				{"id":"bd-new-wait","title":"new wait","status":"open","issue_type":"gate","created_at":"2026-05-01T00:00:01Z","labels":["gc:wait"],"metadata":{"session_id":"s2"},"no_history":true}
			]`), nil
		default:
			return nil, fmt.Errorf("unexpected command: %s", cmd)
		}
	}
	store := beads.NewBdStore("/city", runner)

	sessions, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %+v, want history and no-history rows", sessions)
	}
	if !sessions[1].NoHistory {
		t.Fatalf("new session row = %+v, want no_history parsed", sessions[1])
	}

	waits, err := loadWaitBeads(store)
	if err != nil {
		t.Fatalf("loadWaitBeads: %v", err)
	}
	if len(waits) != 2 {
		t.Fatalf("waits = %+v, want history and no-history rows", waits)
	}
	foundNoHistoryWait := false
	for _, wait := range waits {
		if wait.ID == "bd-new-wait" {
			foundNoHistoryWait = wait.NoHistory
			break
		}
	}
	if !foundNoHistoryWait {
		t.Fatalf("waits = %+v, want bd-new-wait with no_history parsed", waits)
	}
}

func TestOpenStoreResultAtForCityWrapsSQLiteWithBeadPolicies(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "sqlite-policy-city"
prefix = "ga"

[beads]
provider = "sqlite"
bd_compatibility = "bd-1.0.5"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := openStoreResultAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreResultAtForCity: %v", err)
	}
	base, _, wrapped := unwrapBeadPolicyStore(result.Store)
	if !wrapped {
		t.Fatalf("openStoreResultAtForCity(sqlite) returned %T, want bead policy wrapper", result.Store)
	}
	defer func() {
		if c, ok := base.(interface{ CloseStore() error }); ok {
			c.CloseStore() //nolint:errcheck
		}
	}()

	created, err := result.Store.Create(beads.Bead{
		Title: "workflow root",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	if err != nil {
		t.Fatalf("Create(workflow): %v", err)
	}
	if !created.NoHistory {
		t.Fatalf("created workflow = %+v, want policy-applied no_history storage", created)
	}

	ready, err := result.Store.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	for _, bead := range ready {
		if bead.ID == created.ID {
			return
		}
	}
	t.Fatalf("Ready() = %+v, missing policy-created SQLite workflow %s", ready, created.ID)
}

func assertStorageClass(t *testing.T, got beads.StorageClass, want string) {
	t.Helper()
	var wantClass beads.StorageClass
	switch want {
	case beadStorageHistory:
		wantClass = beads.StorageHistory
	case beadStorageEphemeral:
		wantClass = beads.StorageEphemeral
	case beadStorageNoHistory:
		wantClass = beads.StorageNoHistory
	default:
		t.Fatalf("unknown expected storage %q", want)
	}
	if got != wantClass {
		t.Fatalf("storage = %q, want %q", got, wantClass)
	}
}
