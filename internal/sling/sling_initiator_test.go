package sling

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// This file pins SlingOpts.Initiator: when set, sling records it as
// gc.initiator on every root it instantiates and on the bead it routes, and
// the routed bead carries it BEFORE routing runs, so no worker can claim the
// bead ahead of the stamp. Both routing shapes are covered: the typed Router
// and the shell sling_query runner.

const testInitiator = "alice"

// initiatorAtRouteRouter is a typed Router that reads gc.initiator from the
// store at the moment Route is called, then stamps gc.routed_to the way the
// built-in routers do.
type initiatorAtRouteRouter struct {
	fallback beads.Store
	seen     map[string]string
}

func (r *initiatorAtRouteRouter) Route(_ context.Context, req RouteRequest) error {
	store := req.Store
	if store == nil {
		store = r.fallback
	}
	b, err := store.Get(req.BeadID)
	if err != nil {
		return err
	}
	if r.seen == nil {
		r.seen = map[string]string{}
	}
	r.seen[req.BeadID] = b.Metadata[beadmeta.InitiatorMetadataKey]
	return store.SetMetadata(req.BeadID, beadmeta.RoutedToMetadataKey, req.Target)
}

// initiatorAtRouteRunner is a shell sling_query runner that reads
// gc.initiator for every bead named in a routing command at the moment the
// command runs.
type initiatorAtRouteRunner struct {
	store beads.Store
	ids   []string
	seen  map[string]string
}

func (r *initiatorAtRouteRunner) run(_, command string, _ map[string]string) (string, error) {
	if r.seen == nil {
		r.seen = map[string]string{}
	}
	for _, id := range r.ids {
		if !strings.Contains(command, id) {
			continue
		}
		b, err := r.store.Get(id)
		if err != nil {
			return "", err
		}
		r.seen[id] = b.Metadata[beadmeta.InitiatorMetadataKey]
	}
	return "", nil
}

func requireInitiator(t *testing.T, store beads.Store, id string) {
	t.Helper()
	const want = testInitiator
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if got := b.Metadata[beadmeta.InitiatorMetadataKey]; got != want {
		t.Fatalf("%s %s = %q, want %q", id, beadmeta.InitiatorMetadataKey, got, want)
	}
}

func slingWorker() config.Agent {
	return config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
}

func TestSlingStampsInitiatorOnPlainBeadBeforeTypedRoute(t *testing.T) {
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), newFakeRunner().run)
	deps.Store = seededStore("BL-1")
	router := &initiatorAtRouteRouter{fallback: deps.Store}
	deps.Router = router

	opts := testOpts(slingWorker(), "BL-1")
	opts.Initiator = testInitiator
	if _, err := DoSling(opts, deps, deps.Store); err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if got := router.seen["BL-1"]; got != testInitiator {
		t.Fatalf("Route saw %s = %q, want %q (stamp must precede routing)", beadmeta.InitiatorMetadataKey, got, testInitiator)
	}
	requireInitiator(t, deps.Store, "BL-1")
}

func TestSlingStampsInitiatorOnPlainBeadBeforeShellRoute(t *testing.T) {
	store := seededStore("BL-1")
	runner := &initiatorAtRouteRunner{store: store, ids: []string{"BL-1"}}
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), runner.run)
	deps.Store = store

	opts := testOpts(slingWorker(), "BL-1")
	opts.Initiator = testInitiator
	if _, err := DoSling(opts, deps, deps.Store); err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if got, ok := runner.seen["BL-1"]; !ok || got != testInitiator {
		t.Fatalf("sling_query saw %s = %q (ran=%v), want %q", beadmeta.InitiatorMetadataKey, got, ok, testInitiator)
	}
}

func TestSlingWithoutInitiatorStampsNothing(t *testing.T) {
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), newFakeRunner().run)
	deps.Store = seededStore("BL-1")
	deps.Router = &initiatorAtRouteRouter{fallback: deps.Store}

	if _, err := DoSling(testOpts(slingWorker(), "BL-1"), deps, deps.Store); err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	b, err := deps.Store.Get("BL-1")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := b.Metadata[beadmeta.InitiatorMetadataKey]; ok {
		t.Fatalf("bead carries %s=%q without an initiator", beadmeta.InitiatorMetadataKey, got)
	}
}

// A standalone v1 formula: the poured root carries the initiator, its step
// beads do not (they resolve to the root through the run chain).
func TestSlingStandaloneFormulaStampsInitiatorOnRoot(t *testing.T) {
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, nil, newFakeRunner().run)
	router := &initiatorAtRouteRouter{fallback: deps.Store}
	deps.Router = router

	result, err := DoSling(SlingOpts{Target: slingWorker(), BeadOrFormula: "code-review", IsFormula: true, Initiator: testInitiator}, deps, nil)
	if err != nil {
		t.Fatalf("DoSling --formula: %v", err)
	}
	requireInitiator(t, deps.Store, result.BeadID)
	if got := router.seen[result.BeadID]; got != testInitiator {
		t.Fatalf("Route saw root %s = %q, want %q", beadmeta.InitiatorMetadataKey, got, testInitiator)
	}
	all, err := deps.Store.List(beads.ListQuery{AllowScan: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range all {
		if b.ID == result.BeadID {
			continue
		}
		if got, ok := b.Metadata[beadmeta.InitiatorMetadataKey]; ok {
			t.Fatalf("non-root bead %s carries %s=%q; only the root is stamped", b.ID, beadmeta.InitiatorMetadataKey, got)
		}
	}
}

// A standalone root-only wisp is graph class: on a city that relocates graph
// it is minted in the graph store, and the stamp must land there.
func TestSlingStandaloneWispStampsInitiatorInRelocatedGraphStore(t *testing.T) {
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		FormulaLayers: config.FormulaLayers{City: []string{splitAttachFormulaDir(t)}},
	}
	deps, work, graph := splitSlingDeps(t, cfg)
	deps.Router = &initiatorAtRouteRouter{fallback: work}

	result, err := DoSling(SlingOpts{Target: slingWorker(), BeadOrFormula: splitAttachVaporA, IsFormula: true, Initiator: testInitiator}, deps, nil)
	if err != nil {
		t.Fatalf("DoSling --formula: %v", err)
	}
	requireInitiator(t, graph, result.BeadID)
}

// --on: the routed source bead is stamped before routing, and the attached
// wisp root is stamped at creation, in whichever store minted it.
func TestSlingOnFormulaStampsInitiatorOnSourceAndWispRoot(t *testing.T) {
	for _, formula := range []string{splitAttachVaporA, splitAttachPouredA} {
		t.Run(formula, func(t *testing.T) {
			forEachSlingTopology(t, func(t *testing.T, env splitAttachEnv) {
				router := &initiatorAtRouteRouter{fallback: env.work}
				env.deps.Router = router
				src := env.source(t, "")

				res, err := DoSling(SlingOpts{Target: slingWorker(), BeadOrFormula: src.ID, OnFormula: formula, Initiator: testInitiator}, env.deps, env.deps.Store)
				if err != nil {
					t.Fatalf("DoSling --on: %v", err)
				}
				if res.WispRootID == "" {
					t.Fatal("WispRootID empty, want an attached wisp")
				}
				if got := router.seen[src.ID]; got != testInitiator {
					t.Fatalf("Route saw source %s = %q, want %q", beadmeta.InitiatorMetadataKey, got, testInitiator)
				}
				requireInitiator(t, env.work, src.ID)
				rootStore := env.work
				if _, err := rootStore.Get(res.WispRootID); err != nil {
					rootStore = env.graph
				}
				requireInitiator(t, rootStore, res.WispRootID)
			})
		})
	}
}

func writeInitiatorGraphV2Formula(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "graph-work.toml"), []byte(`
formula = "graph-work"
version = 2
contract = "graph.v2"

[[steps]]
id = "step"
title = "Do work"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// graph.v2 roots are relocated into the graph store on a split city; the
// workflow root carries the initiator, there and on a single-store city.
func TestSlingGraphV2StampsInitiatorOnWorkflowRoot(t *testing.T) {
	for _, split := range []bool{false, true} {
		name := "single-store"
		if split {
			name = "split"
		}
		t.Run(name, func(t *testing.T) {
			cfg := graphV2SlingTestConfig(t, writeInitiatorGraphV2Formula(t))
			deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
			graph := deps.Store
			if split {
				deps, _, graph = splitSlingDeps(t, cfg)
			}
			deps.CityPath = t.TempDir()
			src, err := deps.Store.Create(beads.Bead{Title: "work", Type: "task", Status: "open"})
			if err != nil {
				t.Fatal(err)
			}

			res, err := DoSling(SlingOpts{Target: slingWorker(), BeadOrFormula: src.ID, OnFormula: "graph-work", Initiator: testInitiator}, deps, deps.Store)
			if err != nil {
				t.Fatalf("DoSling --on graph.v2: %v", err)
			}
			if res.WorkflowID == "" {
				t.Fatal("WorkflowID empty, want a graph.v2 root")
			}
			requireInitiator(t, graph, res.WorkflowID)

			standalone, err := DoSling(SlingOpts{Target: slingWorker(), BeadOrFormula: "graph-work", IsFormula: true, Initiator: testInitiator}, deps, nil)
			if err != nil {
				t.Fatalf("DoSling --formula graph.v2: %v", err)
			}
			rootID := standalone.WorkflowID
			if rootID == "" {
				rootID = standalone.BeadID
			}
			requireInitiator(t, graph, rootID)
		})
	}
}

func newInitiatorConvoy(t *testing.T, store beads.Store) (convoyID string, childIDs []string) {
	t.Helper()
	convoy, err := store.Create(beads.Bead{Title: "convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	for i := 0; i < 2; i++ {
		child, err := store.Create(beads.Bead{Title: "child", Type: "task", Status: "open"})
		if err != nil {
			t.Fatalf("create child: %v", err)
		}
		if err := store.DepAdd(convoy.ID, child.ID, "tracks"); err != nil {
			t.Fatalf("track child: %v", err)
		}
		childIDs = append(childIDs, child.ID)
	}
	return convoy.ID, childIDs
}

func TestSlingBatchStampsInitiatorOnChildrenBeforeTypedRoute(t *testing.T) {
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), newFakeRunner().run)
	router := &initiatorAtRouteRouter{fallback: deps.Store}
	deps.Router = router
	convoyID, children := newInitiatorConvoy(t, deps.Store)

	result, err := DoSlingBatch(SlingOpts{Target: slingWorker(), BeadOrFormula: convoyID, Initiator: testInitiator}, deps, deps.Store)
	if err != nil {
		t.Fatalf("DoSlingBatch: %v", err)
	}
	if result.Routed != len(children) {
		t.Fatalf("Routed = %d, want %d", result.Routed, len(children))
	}
	for _, id := range children {
		if got := router.seen[id]; got != testInitiator {
			t.Fatalf("Route saw child %s %s = %q, want %q", id, beadmeta.InitiatorMetadataKey, got, testInitiator)
		}
	}
}

func TestSlingBatchStampsInitiatorOnChildrenBeforeShellRoute(t *testing.T) {
	store := beads.NewMemStore()
	convoyID, children := newInitiatorConvoy(t, store)
	runner := &initiatorAtRouteRunner{store: store, ids: children}
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), runner.run)
	deps.Store = store

	result, err := DoSlingBatch(SlingOpts{Target: slingWorker(), BeadOrFormula: convoyID, Initiator: testInitiator}, deps, store)
	if err != nil {
		t.Fatalf("DoSlingBatch: %v", err)
	}
	if result.Routed != len(children) {
		t.Fatalf("Routed = %d, want %d", result.Routed, len(children))
	}
	for _, id := range children {
		if got, ok := runner.seen[id]; !ok || got != testInitiator {
			t.Fatalf("sling_query saw child %s %s = %q (ran=%v), want %q", id, beadmeta.InitiatorMetadataKey, got, ok, testInitiator)
		}
	}
}

// Convoy children slung --on a formula: each child's attached wisp root is
// stamped at creation, and each child before it is routed.
func TestSlingBatchOnFormulaStampsInitiatorOnChildWispRoots(t *testing.T) {
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), newFakeRunner().run)
	router := &initiatorAtRouteRouter{fallback: deps.Store}
	deps.Router = router
	convoyID, children := newInitiatorConvoy(t, deps.Store)

	result, err := DoSlingBatch(SlingOpts{Target: slingWorker(), BeadOrFormula: convoyID, OnFormula: "code-review", Initiator: testInitiator}, deps, deps.Store)
	if err != nil {
		t.Fatalf("DoSlingBatch --on: %v", err)
	}
	if result.Routed != len(children) {
		t.Fatalf("Routed = %d, want %d", result.Routed, len(children))
	}
	for _, child := range result.Children {
		if child.WispRootID == "" {
			t.Fatalf("child %s has no attached wisp root", child.BeadID)
		}
		requireInitiator(t, deps.Store, child.WispRootID)
		if got := router.seen[child.BeadID]; got != testInitiator {
			t.Fatalf("Route saw child %s %s = %q, want %q", child.BeadID, beadmeta.InitiatorMetadataKey, got, testInitiator)
		}
	}
}
