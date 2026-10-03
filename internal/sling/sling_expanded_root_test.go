package sling

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// requireDefaultFormulaAttaches slings a source bead carrying metadata to a
// target with a default_sling_formula and requires the sling to succeed with
// a wisp attached to the source.
func requireDefaultFormulaAttaches(t *testing.T, metadata map[string]string) {
	t.Helper()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1), DefaultSlingFormula: stringPtr("code-review")}
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	source, err := deps.Store.Create(beads.Bead{Title: "source", Type: "task", Metadata: metadata})
	if err != nil {
		t.Fatalf("store.Create(source): %v", err)
	}

	result, err := DoSling(testOpts(a, source.ID), deps, deps.Store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if result.WispRootID == "" {
		t.Fatal("WispRootID is empty, want a wisp attached to the source bead")
	}
	got, err := deps.Store.Get(source.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", source.ID, err)
	}
	if got.Metadata[beadmeta.MoleculeIDMetadataKey] != result.WispRootID {
		t.Fatalf("source molecule_id = %q, want %q", got.Metadata[beadmeta.MoleculeIDMetadataKey], result.WispRootID)
	}
}

// TestDoSlingAcceptsRootOnlyWorkflowRootAsSourceBead pins that a workflow root
// whose steps were never materialized stays slingable: it carries the workflow
// kind without the expanded marker and is itself the unit of work.
func TestDoSlingAcceptsRootOnlyWorkflowRootAsSourceBead(t *testing.T) {
	requireDefaultFormulaAttaches(t, map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
	})
}

// TestDoSlingAcceptsMarkedAttemptRootAsSourceBead pins that the expanded
// marker alone does not make a bead unslingable: a retry or ralph attempt root
// carries the marker with gc.kind=task and is real work.
func TestDoSlingAcceptsMarkedAttemptRootAsSourceBead(t *testing.T) {
	requireDefaultFormulaAttaches(t, map[string]string{
		beadmeta.KindMetadataKey:             beadmeta.KindTask,
		beadmeta.WorkflowExpandedMetadataKey: "true",
	})
}

// TestDoSlingPlainRoutesCookedExpandedWorkflowRoot pins that the refusal is
// about wrapping a formula around the root, not about the root itself. A
// cooked expanded workflow root slung to a target with no default formula is
// a plain bead route, and it goes through as before: routed to the target,
// with nothing attached.
func TestDoSlingPlainRoutesCookedExpandedWorkflowRoot(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(2)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	root, err := deps.Store.Create(beads.Bead{
		Title:  "workflow",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:             beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey:  beadmeta.FormulaContractGraphV2,
			beadmeta.WorkflowExpandedMetadataKey: "true",
		},
	})
	if err != nil {
		t.Fatalf("store.Create(root): %v", err)
	}

	result, err := DoSling(testOpts(a, root.ID), deps, deps.Store)
	if err != nil {
		t.Fatalf("DoSling: %v, want a plain route of the cooked root", err)
	}
	if result.Method != "bead" {
		t.Fatalf("Method = %q, want bead", result.Method)
	}
	if result.WispRootID != "" {
		t.Fatalf("WispRootID = %q, want no formula attached on a plain route", result.WispRootID)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %#v, want the one route call", runner.calls)
	}
}

// TestDoSlingRefusesFormulaBackedRouteOfExpandedWorkflowRoot pins the typed
// refusal on both formula-backed routes, the target's default_sling_formula
// and an explicit --on, each plain, with --force and under dry-run. Every case
// returns an *ExpandedWorkflowRootError naming the root and leaves the store,
// the runner and the router untouched.
func TestDoSlingRefusesFormulaBackedRouteOfExpandedWorkflowRoot(t *testing.T) {
	routes := []struct {
		name      string
		target    config.Agent
		onFormula string
	}{
		{
			name:   "default formula",
			target: config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1), DefaultSlingFormula: stringPtr("code-review")},
		},
		{
			name:      "explicit on",
			target:    config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)},
			onFormula: "code-review",
		},
	}
	modes := []struct {
		name   string
		force  bool
		dryRun bool
	}{
		{name: "plain"},
		{name: "force", force: true},
		{name: "dry run", dryRun: true},
	}
	for _, route := range routes {
		for _, mode := range modes {
			t.Run(route.name+"/"+mode.name, func(t *testing.T) {
				runner := newFakeRunner()
				router := &fakeBeadRouter{}
				cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
				deps := testDeps(cfg, runtime.NewFake(), runner.run)
				deps.Router = router
				root, err := deps.Store.Create(beads.Bead{
					Title:  "workflow",
					Type:   "task",
					Status: "open",
					Metadata: map[string]string{
						beadmeta.KindMetadataKey:             beadmeta.KindWorkflow,
						beadmeta.FormulaContractMetadataKey:  beadmeta.FormulaContractGraphV2,
						beadmeta.WorkflowExpandedMetadataKey: "true",
						beadmeta.RoutedToMetadataKey:         "mayor",
					},
				})
				if err != nil {
					t.Fatalf("store.Create(root): %v", err)
				}
				if _, err := deps.Store.Create(beads.Bead{
					Title:    "step",
					Type:     "task",
					Status:   "in_progress",
					Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
				}); err != nil {
					t.Fatalf("store.Create(step): %v", err)
				}
				before, err := deps.Store.Get(root.ID)
				if err != nil {
					t.Fatalf("store.Get(%s): %v", root.ID, err)
				}

				opts := testOpts(route.target, root.ID)
				opts.OnFormula = route.onFormula
				opts.Force = mode.force
				opts.DryRun = mode.dryRun
				_, err = DoSling(opts, deps, deps.Store)
				var refused *ExpandedWorkflowRootError
				if !errors.As(err, &refused) {
					t.Fatalf("DoSling error = %T %[1]v, want ExpandedWorkflowRootError", err)
				}
				if refused.BeadID != root.ID {
					t.Fatalf("ExpandedWorkflowRootError.BeadID = %q, want %q", refused.BeadID, root.ID)
				}

				requireOnlySeedBeads(t, deps.Store, 2)
				after, err := deps.Store.Get(root.ID)
				if err != nil {
					t.Fatalf("store.Get(%s): %v", root.ID, err)
				}
				if after.Status != before.Status {
					t.Fatalf("root status = %q, want %q", after.Status, before.Status)
				}
				if after.Assignee != before.Assignee {
					t.Fatalf("root assignee = %q, want %q", after.Assignee, before.Assignee)
				}
				if !reflect.DeepEqual(after.Metadata, before.Metadata) {
					t.Fatalf("root metadata = %#v, want %#v", after.Metadata, before.Metadata)
				}
				if len(runner.calls) != 0 {
					t.Fatalf("runner calls = %#v, want none", runner.calls)
				}
				if len(router.routed) != 0 {
					t.Fatalf("router calls = %#v, want none", router.routed)
				}
			})
		}
	}
}
