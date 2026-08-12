package sling

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// This file pins the store the source-workflow REPLACEMENT machinery reads and
// mutates graph.v2 roots through.
//
// A workflow root is GRAPH class and is BORN through deps.graphStore()
// (InstantiateSlingFormula, doStartGraphWorkflow). Every read of that same root
// has to use the same store: on a city that relocates graph, reading it through
// deps.Store asks the WORK ledger about a bead it has never held, and every one
// of these paths then fails silently rather than loudly — a replacement finds
// nothing to replace and launches a second live root beside the first, a
// rollback closes an empty subtree and leaves the abandoned launch open and
// claim-attracting, and a workflow-id lookup reports a live root as missing.
//
// deps.graphStore() collapses to deps.Store whenever GraphStore is unset, so
// every single-store caller is byte-identical.

// splitSlingDeps returns deps whose graph class is served by a separate,
// prefix-disjoint store — the shape a split city has. The work leaf mints
// "gc-<n>" and the class leaf "gcg-<n>", so a root read from the wrong leg is
// not merely a different row, it is an id the leg cannot mint.
func splitSlingDeps(t *testing.T, cfg *config.City) (SlingDeps, beads.Store, beads.Store) {
	t.Helper()
	work, graph := splittest.NewSplitStores(t)
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps.Store = work
	deps.GraphStore = graph
	return deps, work, graph
}

// newGraphResidentWorkflowRoot writes a workflow root and one member into the
// GRAPH store, the way InstantiateSlingFormula does on a split city.
func newGraphResidentWorkflowRoot(t *testing.T, graph beads.Store, sourceBeadID, sourceStoreRef string) (root, member beads.Bead) {
	t.Helper()
	root, err := graph.Create(beads.Bead{
		Title: "workflow root",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:                 "workflow",
			beadmeta.FormulaContractMetadataKey:      "graph.v2",
			beadmeta.SourceBeadIDMetadataKey:         sourceBeadID,
			sourceworkflow.SourceStoreRefMetadataKey: sourceStoreRef,
		},
	})
	if err != nil {
		t.Fatalf("creating the graph-resident root: %v", err)
	}
	if !sourceworkflow.IsWorkflowRoot(root) {
		t.Fatalf("fixture root %s is not recognized as a workflow root; the invariant would pass vacuously", root.ID)
	}
	member, err = graph.Create(beads.Bead{
		Title: "workflow step",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: root.ID,
		},
	})
	if err != nil {
		t.Fatalf("creating the graph-resident member: %v", err)
	}
	return root, member
}

// TestSourceWorkflowRootByIDReadsTheGraphStore covers the single-store fallback
// in sourceWorkflowRootByID, taken by every caller that wires no
// SourceWorkflowStores enumerator (the embedded dispatch paths).
//
// The subject is a workflow ROOT. deps.Store holds the SOURCE bead; the root
// lives in the graph store, and a lookup that reads the source store reports a
// live workflow as absent — which the replacement machinery reads as "there is
// nothing to replace".
func TestSourceWorkflowRootByIDReadsTheGraphStore(t *testing.T) {
	deps, work, graph := splitSlingDeps(t, &config.City{Workspace: config.Workspace{Name: "test"}})
	source, err := work.Create(beads.Bead{Title: "source bead", Type: "task"})
	if err != nil {
		t.Fatalf("creating the source bead: %v", err)
	}
	root, _ := newGraphResidentWorkflowRoot(t, graph, source.ID, deps.StoreRef)
	if deps.SourceWorkflowStores != nil {
		t.Fatal("this invariant is about the single-store fallback; testDeps must not wire an enumerator")
	}

	got, ok, reason, err := sourceWorkflowRootByID(deps, source.ID, root.ID, deps.StoreRef)
	if err != nil {
		t.Fatalf("sourceWorkflowRootByID: %v", err)
	}
	if !ok {
		t.Fatalf("sourceWorkflowRootByID(%s) reported the live graph-resident root as absent (reason %q); the fallback read the work ledger, which never held it", root.ID, reason)
	}
	if got.root.ID != root.ID {
		t.Errorf("resolved root %q, want %q", got.root.ID, root.ID)
	}
}

// TestPendingGraphWorkflowLaunchRollbackClosesTheGraphSubtree covers the
// rollback of a pending source-workflow launch. The subtree it closes is the
// root this launch just materialized through the graph store; closing it through
// deps.Store finds nothing, reports success, and leaves the abandoned workflow
// open — a live root with no source, which the dispatcher keeps trying to run.
func TestPendingGraphWorkflowLaunchRollbackClosesTheGraphSubtree(t *testing.T) {
	deps, _, graph := splitSlingDeps(t, &config.City{Workspace: config.Workspace{Name: "test"}})
	root, member := newGraphResidentWorkflowRoot(t, graph, "gc-source", "city:test-city")

	launch := pendingGraphWorkflowLaunch(root.ID, "", config.Agent{Name: "mayor"}, "formula", "graph-work", deps)
	if err := launch.rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	closedRoot, err := graph.Get(root.ID)
	if err != nil {
		t.Fatalf("reading the root back: %v", err)
	}
	if closedRoot.Status != "closed" {
		t.Errorf("root %s status = %q after rollback, want closed — the rollback closed a subtree in a store that does not hold it", root.ID, closedRoot.Status)
	}
	closedMember, err := graph.Get(member.ID)
	if err != nil {
		t.Fatalf("reading the member back: %v", err)
	}
	if closedMember.Status != "closed" {
		t.Errorf("member %s status = %q after rollback, want closed", member.ID, closedMember.Status)
	}
}

// TestGraphV2ReplacementSnapshotAndRollbackUseTheGraphStore covers the --force
// replacement pair: the snapshot that captures the root being replaced, and the
// rollback that restores it when the replacement's launch fails.
//
// Both are called with one store expression inside attachFormulaToBead, and both
// have the same subject — a graph.v2 root keyed by gc.graphv2_root_key, which
// only ever exists in the graph store. A snapshot taken from the work ledger is
// empty, so --force silently replaces nothing and the rollback has nothing to
// restore.
func TestGraphV2ReplacementSnapshotAndRollbackUseTheGraphStore(t *testing.T) {
	formulaDir := t.TempDir()
	writeGraphV2ConvoyFormula(t, formulaDir)
	cfg := graphV2SlingTestConfig(t, formulaDir)
	deps, work, graph := splitSlingDeps(t, cfg)
	deps.CityPath = t.TempDir()

	convoy, err := work.Create(beads.Bead{Title: "input", Type: "convoy"})
	if err != nil {
		t.Fatalf("creating the input convoy: %v", err)
	}
	vars := map[string]string{"convoy_id": convoy.ID}

	replaced, err := InstantiateSlingFormula(t.Context(), "graph-work", []string{formulaDir}, molecule.Options{Vars: vars}, "", "default", "", config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}, deps)
	if err != nil {
		t.Fatalf("materializing the root to be replaced: %v", err)
	}
	if _, err := graph.Get(replaced.RootID); err != nil {
		t.Fatalf("the first root is not graph resident (%v); the fixture cannot distinguish the two stores", err)
	}

	snapshot, err := snapshotGraphV2ReplacementRoot(deps.graphStore(), "graph-work", vars, "default", "", true)
	if err != nil {
		t.Fatalf("snapshotGraphV2ReplacementRoot: %v", err)
	}
	if snapshot.rootID != replaced.RootID {
		t.Fatalf("snapshot captured root %q, want the live graph-resident root %q — a snapshot taken from the work ledger is empty, and --force then replaces nothing", snapshot.rootID, replaced.RootID)
	}

	// The rollback half: closing the live root and restoring it from the
	// snapshot has to happen in the same store the snapshot came from.
	closed := "closed"
	if err := graph.Update(replaced.RootID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("closing the replaced root: %v", err)
	}
	if err := rollbackGraphV2ReplacementLaunch(deps.graphStore(), "", snapshot); err != nil {
		t.Fatalf("rollbackGraphV2ReplacementLaunch: %v", err)
	}
	restored, err := graph.Get(replaced.RootID)
	if err != nil {
		t.Fatalf("reading the restored root: %v", err)
	}
	if restored.Status == "closed" {
		t.Errorf("root %s is still closed after rollback; the restore ran against a store that does not hold it", replaced.RootID)
	}
}

// TestGraphRootStoreCollapsesWhenGraphIsNotRelocated is the single-store
// compatibility row for all four sites: with GraphStore unset, deps.graphStore()
// returns the EXACT store value deps.Store carries, so every one of these paths
// reads the store it always did — and the optional-capability assertions the
// molecule create relies on keep holding on the same value.
func TestGraphRootStoreCollapsesWhenGraphIsNotRelocated(t *testing.T) {
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), newFakeRunner().run)
	if deps.GraphStore != nil {
		t.Fatal("testDeps wired a GraphStore; this row is about the collapse")
	}
	if deps.graphStore() != deps.Store {
		t.Errorf("graphStore() returned %p, want the identical work store %p", deps.graphStore(), deps.Store)
	}
}

// rootKeyQuerySpy counts the gc.graphv2_root_key lookups a store answers. That
// query is the replacement machinery's own fingerprint — closeFailedGraphV2RootsByKey
// and snapshotGraphV2ReplacementRoot both run it — so counting it names WHICH
// STORE the call site reached for, which a test that calls the helper directly
// cannot do.
type rootKeyQuerySpy struct {
	beads.Store
	rootKeyQueries int
}

func (s *rootKeyQuerySpy) ListByMetadata(match map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	if _, ok := match[beadmeta.Graphv2RootKeyMetadataKey]; ok {
		s.rootKeyQueries++
	}
	return s.Store.ListByMetadata(match, limit, opts...)
}

// TestForcedGraphV2ReplacementQueriesTheGraphStoreNotTheWorkLedger is the
// CALL-SITE assertion for the snapshot/rollback pair in attachFormulaToBead.
//
// A forced launch looks up the root it is replacing by gc.graphv2_root_key.
// Only the graph store ever holds such a root, so the work ledger must never be
// asked: a lookup there returns empty, --force replaces nothing, and a second
// live root is launched beside the first.
func TestForcedGraphV2ReplacementQueriesTheGraphStoreNotTheWorkLedger(t *testing.T) {
	formulaDir := t.TempDir()
	writeGraphV2ConvoyFormula(t, formulaDir)
	cfg := graphV2SlingTestConfig(t, formulaDir)
	deps, work, graph := splitSlingDeps(t, cfg)
	deps.CityPath = t.TempDir()
	workSpy := &rootKeyQuerySpy{Store: work}
	graphSpy := &rootKeyQuerySpy{Store: graph}
	deps.Store = workSpy
	deps.GraphStore = graphSpy

	source, err := work.Create(beads.Bead{Title: "source bead", Type: "task"})
	if err != nil {
		t.Fatalf("creating the source bead: %v", err)
	}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	if _, err := DoSling(SlingOpts{Target: a, BeadOrFormula: source.ID, OnFormula: "graph-work", Force: true}, deps, deps.Store); err != nil {
		t.Fatalf("forced sling: %v", err)
	}

	if graphSpy.rootKeyQueries == 0 {
		t.Errorf("the forced launch never asked the graph store for a gc.graphv2_root_key root; the replacement lookup ran somewhere else")
	}
	if workSpy.rootKeyQueries != 0 {
		t.Errorf("the forced launch asked the WORK ledger for a gc.graphv2_root_key root %d time(s); only the graph store holds one, so that lookup is always empty and --force silently replaces nothing", workSpy.rootKeyQueries)
	}
}

// promoteFailingStore fails the in_progress promotion for every root except the
// one named, so a SECOND forced launch fails at doStartGraphWorkflow — the only
// path that reaches rollbackGraphV2ReplacementLaunch.
type promoteFailingStore struct {
	beads.Store
	spare string
	armed bool
}

func (s *promoteFailingStore) Update(id string, opts beads.UpdateOpts) error {
	if s.armed && id != s.spare && opts.Status != nil && *opts.Status == "in_progress" {
		return errRefusedPromotion
	}
	return s.Store.Update(id, opts)
}

var errRefusedPromotion = errorString("refusing the workflow promotion")

type errorString string

func (e errorString) Error() string { return string(e) }

// TestForcedGraphV2ReplacementRollbackRestoresInTheGraphStore is the CALL-SITE
// assertion for the rollback half of the --force replacement pair.
//
// A forced launch closes the root it replaces and materializes a new one. When
// the new one fails to start, the rollback has to close the replacement AND
// restore the root it displaced — in the store both of them live in. Running the
// restore against the work ledger silently succeeds against nothing, and the
// city is left with the previous workflow closed and the replacement never
// started: a run that vanished.
func TestForcedGraphV2ReplacementRollbackRestoresInTheGraphStore(t *testing.T) {
	formulaDir := t.TempDir()
	writeGraphV2ConvoyFormula(t, formulaDir)
	cfg := graphV2SlingTestConfig(t, formulaDir)
	deps, work, graph := splitSlingDeps(t, cfg)
	deps.CityPath = t.TempDir()
	promoteGuard := &promoteFailingStore{Store: graph}
	deps.GraphStore = promoteGuard

	convoy, err := work.Create(beads.Bead{Title: "input", Type: "convoy"})
	if err != nil {
		t.Fatalf("creating the input convoy: %v", err)
	}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	first, err := DoSling(SlingOpts{Target: a, BeadOrFormula: convoy.ID, OnFormula: "graph-work", Force: true}, deps, deps.Store)
	if err != nil {
		t.Fatalf("first forced sling: %v", err)
	}
	if first.WorkflowID == "" {
		t.Fatal("the first forced sling started no workflow; there is nothing for a replacement to displace")
	}
	promoteGuard.spare = first.WorkflowID
	promoteGuard.armed = true

	if _, err := DoSling(SlingOpts{Target: a, BeadOrFormula: convoy.ID, OnFormula: "graph-work", Force: true}, deps, deps.Store); err == nil {
		t.Fatal("the second forced sling succeeded; the fixture must fail its start to reach the rollback")
	}

	restored, err := graph.Get(first.WorkflowID)
	if err != nil {
		t.Fatalf("reading the displaced root back: %v", err)
	}
	if restored.Status == "closed" {
		t.Errorf("displaced root %s is still closed after the failed replacement rolled back; the restore ran against a store that does not hold it, so the city lost both workflows", first.WorkflowID)
	}
}
