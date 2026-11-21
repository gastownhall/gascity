package main

import (
	"bytes"
	"io"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/graphroute"
	"github.com/gastownhall/gascity/internal/session"
)

// continuationSessionFixture holds a concrete session continuation (#8436):
// the session bead lives in the city's sessions store and names the frontend
// rig, while the workflow store holds a decoy session bead with the same id
// that names the backend rig. Decorating the continuation must read the
// authoritative bead, so every rig-scoped route lands on frontend.
type continuationSessionFixture struct {
	cfg      *config.City
	sessions *beads.MemStore
	workflow *beads.MemStore
	id       string
	source   beads.Bead
}

func newContinuationSessionFixture(t *testing.T) continuationSessionFixture {
	t.Helper()
	sessions, workflow := beads.NewMemStore(), beads.NewMemStore()
	sess, err := sessions.Create(beads.Bead{
		Title:    "authoritative session",
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"alias": "frontend/sky", "session_name": "frontend-sky"},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	decoy, err := workflow.Create(beads.Bead{
		Title:    "decoy",
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"alias": "backend/decoy", "session_name": "backend-decoy"},
	})
	if err != nil {
		t.Fatalf("Create(decoy): %v", err)
	}
	if decoy.ID != sess.ID {
		t.Fatalf("decoy id %q != session id %q; the fixture needs a colliding id", decoy.ID, sess.ID)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{FormulaV2: boolPtr(true)},
		Rigs:      []config.Rig{{Name: "frontend", Path: "frontend"}, {Name: "backend", Path: "backend"}},
	}
	config.InjectImplicitAgents(cfg)
	addTestControlDispatcherAgents(cfg, "", "frontend", "backend")
	return continuationSessionFixture{
		cfg:      cfg,
		sessions: sessions,
		workflow: workflow,
		id:       sess.ID,
		source: beads.Bead{
			ID:       "gc-source",
			Title:    "Source",
			Metadata: map[string]string{graphroute.GraphExecutionRouteMetaKey: sess.ID},
		},
	}
}

func (f continuationSessionFixture) assertWorkflowUntouched(t *testing.T) {
	t.Helper()
	got, err := f.workflow.Get(f.id)
	if err != nil {
		t.Fatalf("decoy vanished from the workflow store: %v", err)
	}
	want := map[string]string{"alias": "backend/decoy", "session_name": "backend-decoy"}
	if !maps.Equal(got.Metadata, want) || got.Status == "closed" {
		t.Errorf("workflow decoy = status %q metadata %v, want it untouched", got.Status, got.Metadata)
	}
}

// TestDecorateDynamicFragmentRecipeReadsConcreteSessionFromSessionStore is
// AC-3 for dynamic fragments: a fragment attached under a concrete-session
// continuation binds that session and takes its rig context from the
// sessions-store bead, never from a workflow-store decoy with the same id.
func TestDecorateDynamicFragmentRecipeReadsConcreteSessionFromSessionStore(t *testing.T) {
	f := newContinuationSessionFixture(t)
	fragment := &formula.FragmentRecipe{
		Name: "expansion-review",
		Steps: []formula.RecipeStep{
			{ID: "expansion-review.review", Title: "Review"},
			{ID: "expansion-review.check", Title: "Check", Metadata: map[string]string{"gc.kind": "check"}},
		},
		Deps: []formula.RecipeDep{{StepID: "expansion-review.check", DependsOnID: "expansion-review.review", Type: "blocks"}},
	}
	if err := decorateDynamicFragmentRecipe(fragment, f.source, beadstest.Sessions(f.sessions), f.cfg.Workspace.Name, t.TempDir(), f.cfg); err != nil {
		t.Fatalf("decorateDynamicFragmentRecipe: %v", err)
	}
	steps := map[string]formula.RecipeStep{}
	for _, step := range fragment.Steps {
		steps[step.ID] = step
	}
	if got := steps["expansion-review.review"].Assignee; got != f.id {
		t.Errorf("review assignee = %q, want the concrete session %s", got, f.id)
	}
	if got := steps["expansion-review.check"].Metadata["gc.routed_to"]; got != "frontend/control-dispatcher" {
		t.Errorf("check gc.routed_to = %q, want frontend/control-dispatcher from the sessions-store bead's rig", got)
	}
	if got := steps["expansion-review.check"].Metadata[graphroute.GraphExecutionRigContextMetaKey]; got != "frontend" {
		t.Errorf("check execution rig context = %q, want frontend", got)
	}
	f.assertWorkflowUntouched(t)
}

// TestDecorateDrainItemRecipeReadsConcreteSessionFromSessionStore is AC-3 for
// drain items: an item recipe routed by a concrete-session continuation binds
// that session and its rig context from the sessions-store bead, never from a
// workflow-store decoy with the same id.
func TestDecorateDrainItemRecipeReadsConcreteSessionFromSessionStore(t *testing.T) {
	f := newContinuationSessionFixture(t)
	recipe := &formula.Recipe{
		Name: "item",
		Steps: []formula.RecipeStep{
			{ID: "item", IsRoot: true, Type: "task", Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"}},
			{ID: "item.work", Title: "Work", Type: "task"},
			{ID: "item.check", Title: "Check", Type: "task", Metadata: map[string]string{"gc.kind": "check"}},
		},
	}
	if err := decorateDrainItemRecipe(recipe, f.source, beadstest.Sessions(f.sessions), "rig:frontend", f.cfg.Workspace.Name, t.TempDir(), f.cfg); err != nil {
		t.Fatalf("decorateDrainItemRecipe: %v", err)
	}
	if got := recipe.StepByID("item.work").Assignee; got != f.id {
		t.Errorf("item.work assignee = %q, want the concrete session %s", got, f.id)
	}
	check := recipe.StepByID("item.check")
	if got := check.Metadata[graphroute.GraphExecutionRigContextMetaKey]; got != "frontend" {
		t.Errorf("item.check execution rig context = %q, want frontend from the sessions-store bead", got)
	}
	if got := check.Metadata["gc.routed_to"]; got != "frontend/control-dispatcher" {
		t.Errorf("item.check gc.routed_to = %q, want frontend/control-dispatcher", got)
	}
	f.assertWorkflowUntouched(t)
}

// TestControlDispatcherFanoutBindsConcreteSessionFromTheCitySessionStore is
// AC-3 end to end: a rig-scoped fanout whose source continues a concrete
// session spawns its fragment bound to that session, read from the city's
// sessions store. The rig workflow store holds no session row, and the id it
// reuses names an unrelated workflow bead.
func TestControlDispatcherFanoutBindsConcreteSessionFromTheCitySessionStore(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city config: %v", err)
	}
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "expand.formula.toml"), []byte(`
formula = "expand"
type = "expansion"
version = 2
contract = "graph.v2"

[vars.reviewer]
required = true

[[template]]
id = "{target}.review"
title = "Review {reviewer}"
`), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}
	sessions, workflow := beads.NewMemStore(), beads.NewMemStore()
	sess, err := sessions.Create(beads.Bead{
		Title:    "authoritative session",
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"alias": "frontend/sky", "session_name": "frontend-sky"},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	root, source, control := createFanoutControl(t, workflow)
	if root.ID != sess.ID {
		t.Fatalf("workflow root id %q != session id %q; the fixture needs a colliding id", root.ID, sess.ID)
	}
	if err := workflow.SetMetadata(source.ID, graphroute.GraphExecutionRouteMetaKey, sess.ID); err != nil {
		t.Fatalf("route source to the session: %v", err)
	}
	prevOpen := slingOpenCityStore
	slingOpenCityStore = func(path string) (beads.Store, error) {
		if path != cityPath {
			t.Fatalf("slingOpenCityStore(%q), want %q", path, cityPath)
		}
		return sessions, nil
	}
	t.Cleanup(func() { slingOpenCityStore = prevOpen })

	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Daemon:        config.DaemonConfig{FormulaV2: boolPtr(true)},
		Rigs:          []config.Rig{{Name: "frontend", Path: rigPath}},
		FormulaLayers: config.FormulaLayers{City: []string{formulaDir}},
	}
	config.InjectImplicitAgents(cfg)
	addTestControlDispatcherAgents(cfg, "", "frontend")
	var stderr bytes.Buffer
	if err := runControlDispatcherWithStoreAndConfig(cityPath, rigPath, workflow, control.ID, cfg, io.Discard, &stderr); err != nil {
		t.Fatalf("runControlDispatcherWithStoreAndConfig: %v (stderr: %s)", err, stderr.String())
	}
	all, err := workflow.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	var reviews []beads.Bead
	for _, b := range all {
		if b.Title == "Review reviewer" {
			reviews = append(reviews, b)
		}
	}
	if len(reviews) != 1 {
		t.Fatalf("fanout spawned %d review step(s), want 1 (stderr: %s)", len(reviews), stderr.String())
	}
	if got := reviews[0].Assignee; got != sess.ID {
		t.Errorf("review assignee = %q, want the concrete session %s", got, sess.ID)
	}
	if got := reviews[0].Metadata[beadmeta.SessionIDMetadataKey]; got != sess.ID {
		t.Errorf("review %s = %q, want %s", beadmeta.SessionIDMetadataKey, got, sess.ID)
	}
	if n := countSessionBeads(t, workflow); n != 0 {
		t.Errorf("the rig workflow store holds %d session bead(s), want none", n)
	}
}
