package sling

import (
	"context"
	"errors"
	"fmt"
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

// Dispatch is the entry point gc sling and POST /sling share. Its plain-route
// branch hands off through ExpandConvoy, which rebuilds SlingOpts from
// RouteOpts; the initiator must survive that hop for a plain bead, a
// --no-formula sling to a target with a default formula, and a plain convoy,
// under both the typed Router and the shell sling_query runner.
func TestDispatchPlainRouteStampsInitiatorBeforeRouting(t *testing.T) {
	withDefault := slingWorker()
	withDefault.DefaultSlingFormula = stringPtr("code-review")

	cases := []struct {
		name      string
		target    config.Agent
		noFormula bool
		convoy    bool
	}{
		{name: "plain bead", target: slingWorker()},
		{name: "no formula", target: withDefault, noFormula: true},
		{name: "plain convoy", target: slingWorker(), convoy: true},
	}
	for _, tc := range cases {
		for _, routing := range []string{"typed", "shell"} {
			t.Run(tc.name+"/"+routing, func(t *testing.T) {
				store := beads.NewMemStore()
				var ids []string
				beadID := ""
				if tc.convoy {
					beadID, ids = newInitiatorConvoy(t, store)
				} else {
					b, err := store.Create(beads.Bead{Title: "work", Type: "task", Status: "open"})
					if err != nil {
						t.Fatal(err)
					}
					beadID, ids = b.ID, []string{b.ID}
				}
				shell := &initiatorAtRouteRunner{store: store, ids: ids}
				deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), shell.run)
				deps.Store = store
				typed := &initiatorAtRouteRouter{fallback: store}
				if routing == "typed" {
					deps.Router = typed
				}
				s, err := New(deps)
				if err != nil {
					t.Fatal(err)
				}

				opts := SlingOpts{Target: tc.target, BeadOrFormula: beadID, NoFormula: tc.noFormula, Initiator: testInitiator}
				if _, err := s.Dispatch(context.Background(), opts, store); err != nil {
					t.Fatalf("Dispatch: %v", err)
				}
				seen := shell.seen
				if routing == "typed" {
					seen = typed.seen
				}
				for _, id := range ids {
					if got, ok := seen[id]; !ok || got != testInitiator {
						t.Fatalf("%s routing saw %s %s = %q (routed=%v), want %q", routing, id, beadmeta.InitiatorMetadataKey, got, ok, testInitiator)
					}
					requireInitiator(t, store, id)
				}
			})
		}
	}
}

// The exported intent entry points carry the initiator through their option
// structs, so none of them silently drops it.
func TestIntentEntryPointsCarryInitiator(t *testing.T) {
	newSling := func(t *testing.T, store beads.Store) (*Sling, *initiatorAtRouteRouter) {
		t.Helper()
		deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), newFakeRunner().run)
		deps.Store = store
		router := &initiatorAtRouteRouter{fallback: store}
		deps.Router = router
		s, err := New(deps)
		if err != nil {
			t.Fatal(err)
		}
		return s, router
	}
	ctx := context.Background()

	t.Run("RouteBead", func(t *testing.T) {
		store := seededStore("BL-1")
		s, router := newSling(t, store)
		if _, err := s.RouteBead(ctx, "BL-1", slingWorker(), RouteOpts{Initiator: testInitiator}); err != nil {
			t.Fatalf("RouteBead: %v", err)
		}
		if got := router.seen["BL-1"]; got != testInitiator {
			t.Fatalf("Route saw %s = %q, want %q", beadmeta.InitiatorMetadataKey, got, testInitiator)
		}
	})
	t.Run("ExpandConvoy", func(t *testing.T) {
		store := beads.NewMemStore()
		convoyID, children := newInitiatorConvoy(t, store)
		s, router := newSling(t, store)
		if _, err := s.ExpandConvoy(ctx, convoyID, slingWorker(), RouteOpts{Initiator: testInitiator}, store); err != nil {
			t.Fatalf("ExpandConvoy: %v", err)
		}
		for _, id := range children {
			if got := router.seen[id]; got != testInitiator {
				t.Fatalf("Route saw child %s %s = %q, want %q", id, beadmeta.InitiatorMetadataKey, got, testInitiator)
			}
		}
	})
	t.Run("LaunchFormula", func(t *testing.T) {
		store := beads.NewMemStore()
		s, _ := newSling(t, store)
		res, err := s.LaunchFormula(ctx, "code-review", slingWorker(), FormulaOpts{Initiator: testInitiator})
		if err != nil {
			t.Fatalf("LaunchFormula: %v", err)
		}
		requireInitiator(t, store, res.BeadID)
	})
	t.Run("AttachFormula", func(t *testing.T) {
		store := seededStore("BL-1")
		s, router := newSling(t, store)
		res, err := s.AttachFormula(ctx, "code-review", "BL-1", slingWorker(), FormulaOpts{Initiator: testInitiator})
		if err != nil {
			t.Fatalf("AttachFormula: %v", err)
		}
		if got := router.seen["BL-1"]; got != testInitiator {
			t.Fatalf("Route saw %s = %q, want %q", beadmeta.InitiatorMetadataKey, got, testInitiator)
		}
		requireInitiator(t, store, res.WispRootID)
	})
}

// routeCallRouter records every Route call without reading the store, so a
// test can prove routing never ran even when the store cannot be read.
type routeCallRouter struct {
	calls []string
}

func (r *routeCallRouter) Route(_ context.Context, req RouteRequest) error {
	r.calls = append(r.calls, req.BeadID)
	return nil
}

// initiatorStampErrStore fails every gc.initiator write with err and passes
// every other call through.
type initiatorStampErrStore struct {
	beads.Store
	err error
}

func (s *initiatorStampErrStore) SetMetadata(id, key, value string) error {
	if key == beadmeta.InitiatorMetadataKey {
		return s.err
	}
	return s.Store.SetMetadata(id, key, value)
}

// initiatorStampCountStore counts the gc.initiator writes made through it.
type initiatorStampCountStore struct {
	beads.Store
	stamps int
}

func (s *initiatorStampCountStore) SetMetadata(id, key, value string) error {
	if key == beadmeta.InitiatorMetadataKey {
		s.stamps++
	}
	return s.Store.SetMetadata(id, key, value)
}

// newRoutingProbe returns deps on store that route through a typed Router
// (routing "typed") or the shell sling_query runner (routing "shell"), and a
// func listing every routing call made so far: a bead id or a command.
func newRoutingProbe(routing string, store beads.Store) (SlingDeps, func() []string) {
	runner := newFakeRunner()
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), runner.run)
	deps.Store = store
	if routing == "typed" {
		router := &routeCallRouter{}
		deps.Router = router
		return deps, func() []string { return router.calls }
	}
	return deps, func() []string { return runner.calls }
}

// routedBead reports whether any routing call names id.
func routedBead(calls []string, id string) bool {
	for _, call := range calls {
		if strings.Contains(call, id) {
			return true
		}
	}
	return false
}

// A failed read of the bead about to be routed stops the sling before either
// router runs: the stamp cannot be confirmed, and a routed bead is claimable.
// Force skips every preflight read, so DoSling reaches the routing read
// itself; Force tolerates only a missing bead, not a failed read.
func TestSlingRefusesToRouteWhenInitiatorReadFails(t *testing.T) {
	readErr := errors.New("transient read failure")
	for _, routing := range []string{"typed", "shell"} {
		t.Run(routing, func(t *testing.T) {
			deps, routed := newRoutingProbe(routing, &getErrStore{Store: seededStore("BL-1"), err: readErr})
			opts := testOpts(slingWorker(), "BL-1")
			opts.Initiator = testInitiator
			opts.Force = true

			_, err := DoSling(opts, deps, deps.Store)
			if !errors.Is(err, readErr) {
				t.Fatalf("DoSling err = %v, want the read failure", err)
			}
			if routedBead(routed(), "BL-1") {
				t.Fatalf("routed BL-1 after the read failed: %q", routed())
			}
		})
	}
}

// A forced sling of a bead its store does not hold routes best-effort by
// design. With an initiator pending it still routes, and reports that the
// stamp was not recorded along with the store's reason.
func TestForcedSlingOfMissingBeadReportsUnrecordedInitiator(t *testing.T) {
	for _, routing := range []string{"typed", "shell"} {
		t.Run(routing, func(t *testing.T) {
			deps, routed := newRoutingProbe(routing, beads.NewMemStore())
			opts := testOpts(slingWorker(), "BL-9")
			opts.Initiator = testInitiator
			opts.Force = true

			result, err := DoSling(opts, deps, deps.Store)
			if err != nil {
				t.Fatalf("DoSling: %v", err)
			}
			if !routedBead(routed(), "BL-9") {
				t.Fatal("BL-9 not routed; a forced sling routes a missing bead best-effort")
			}
			var warning string
			for _, msg := range result.MetadataErrors {
				if strings.Contains(msg, "initiator not recorded") {
					warning = msg
				}
			}
			if !strings.Contains(warning, "BL-9") || !strings.Contains(warning, beads.ErrNotFound.Error()) {
				t.Fatalf("MetadataErrors = %q, want an initiator-not-recorded warning naming BL-9 with the store's reason", result.MetadataErrors)
			}
		})
	}
}

// A failed gc.initiator write stops the sling before either router runs,
// forced or not.
func TestSlingRefusesToRouteWhenInitiatorStampFails(t *testing.T) {
	stampErr := errors.New("transient write failure")
	for _, routing := range []string{"typed", "shell"} {
		for _, force := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/force=%t", routing, force), func(t *testing.T) {
				deps, routed := newRoutingProbe(routing, &initiatorStampErrStore{Store: seededStore("BL-1"), err: stampErr})
				opts := testOpts(slingWorker(), "BL-1")
				opts.Initiator = testInitiator
				opts.Force = force

				_, err := DoSling(opts, deps, deps.Store)
				if !errors.Is(err, stampErr) {
					t.Fatalf("DoSling err = %v, want the stamp failure", err)
				}
				if routedBead(routed(), "BL-1") {
					t.Fatalf("routed BL-1 after its stamp failed: %q", routed())
				}
			})
		}
	}
}

// DoSlingBatch: a convoy child whose gc.initiator write fails is reported as
// failed and is never routed.
func TestSlingBatchRefusesToRouteChildWhoseInitiatorStampFails(t *testing.T) {
	stampErr := errors.New("transient write failure")
	for _, routing := range []string{"typed", "shell"} {
		t.Run(routing, func(t *testing.T) {
			mem := beads.NewMemStore()
			convoyID, children := newInitiatorConvoy(t, mem)
			deps, routed := newRoutingProbe(routing, &initiatorStampErrStore{Store: mem, err: stampErr})

			result, err := DoSlingBatch(SlingOpts{Target: slingWorker(), BeadOrFormula: convoyID, Initiator: testInitiator}, deps, mem)
			if !errors.Is(err, stampErr) {
				t.Fatalf("DoSlingBatch err = %v, want the stamp failure", err)
			}
			if result.Routed != 0 || result.Failed != len(children) {
				t.Fatalf("Routed = %d, Failed = %d, want 0 and %d", result.Routed, result.Failed, len(children))
			}
			for _, id := range children {
				if routedBead(routed(), id) {
					t.Fatalf("routed child %s after its stamp failed: %q", id, routed())
				}
			}
		})
	}
}

// The stamp is written only when the bead lacks this initiator: a root minted
// with RootMetadata already carries it, while a bead carrying another
// launcher's initiator is re-stamped before routing.
func TestSlingStampsInitiatorOnlyWhenBeadLacksIt(t *testing.T) {
	t.Run("minted root", func(t *testing.T) {
		store := &initiatorStampCountStore{Store: beads.NewMemStore()}
		deps, _ := newRoutingProbe("typed", store)

		result, err := DoSling(SlingOpts{Target: slingWorker(), BeadOrFormula: "code-review", IsFormula: true, Initiator: testInitiator}, deps, nil)
		if err != nil {
			t.Fatalf("DoSling --formula: %v", err)
		}
		requireInitiator(t, store, result.BeadID)
		if store.stamps != 0 {
			t.Fatalf("%s written %d times after the root was minted with it, want 0", beadmeta.InitiatorMetadataKey, store.stamps)
		}
	})
	t.Run("another launcher", func(t *testing.T) {
		store := seededStore("BL-1")
		if err := store.SetMetadata("BL-1", beadmeta.InitiatorMetadataKey, "bob"); err != nil {
			t.Fatal(err)
		}
		deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), newFakeRunner().run)
		deps.Store = store
		router := &initiatorAtRouteRouter{fallback: store}
		deps.Router = router

		opts := testOpts(slingWorker(), "BL-1")
		opts.Initiator = testInitiator
		if _, err := DoSling(opts, deps, deps.Store); err != nil {
			t.Fatalf("DoSling: %v", err)
		}
		if got := router.seen["BL-1"]; got != testInitiator {
			t.Fatalf("Route saw %s = %q, want %q", beadmeta.InitiatorMetadataKey, got, testInitiator)
		}
	})
}
