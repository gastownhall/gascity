package api

// Frozen reference copies (typed-stores T7d, plan principle 3) of the API's
// workflow-root readers as they stood on main before they read storeref's
// Workflow legs: the workflow store list and ref resolution, isGraphConvoyID,
// the convoy event stream's subject and root lookups, the workflow snapshot's
// store scan, both workflow deletes, the run cancel, the workflow run
// projections, and the order feed and orders-binding append that now take
// their stores from the workflow legs. The equivalence table
// (workflow_legs_equivalence_test.go) runs each beside the live code.
//
// Copied with every helper in this package that reads or writes a store, or
// that T7d edits or deletes (sortedRigNames, the relic-free and graph-store
// helpers of rig_graph_legs.go, the order history detail handler (whose
// store list now comes from the workflow legs), collectWorkflowDeps and its
// prefetchedDepStore, findWorkflowRoots, cancelWorkflowRoot,
// listActiveWorkflowProjectionBeads). Everything else they call is live code
// T7d leaves byte-identical, of two kinds:
//
//   - pure bead helpers, which read no store: isWorkflowRoot,
//     matchesWorkflowID, selectWorkflowRootMatch and the scope helpers,
//     workflowBeadResponseFromBead, prefetchedDepsForWorkflowBeads, the
//     projection and feed formatters, inProgressAssignees;
//   - readers and writers that act only on the store the copy hands them:
//     sourceworkflow (WorkflowTree, CloseWorkflowSubtreeAsExcept), molecule
//     (TeardownTailExclusion), orders.Store (ListTracking, LatestOpenRun) and
//     orderHistoryBeadAcrossStoreInfos. The copy chooses that store, and the
//     table logs every read and write they make on it.
//
// Neither kind can hide a change of which store a read or write touched.
//
// T7d.2 retyped these copies, and changed nothing they do: workflowStoreInfo
// became t7dFrozenInfo (production dropped its store field), a frozen root
// match keeps its frozen entry beside the live match selectWorkflowRootMatch
// takes, the live pure scope helpers take info.live(), state.BeadStore became
// t7b2FrozenBeadStore (production dropped it from State), rigGraphRelicReporter
// became t7b2FrozenRigGraphRelicReporter (T7d.2 deleted it), and the two order
// helpers T7d.2 converted (orderTrackingUpdatedAt,
// orderHistoryBeadAcrossStoreInfos) became their frozen copies from main
// 8d2fb3120c (order_history_handlers_reference_test.go), the code these
// copies called when they were taken.
//
// The SQL fast paths are left out: no fixture here has a Dolt server, so the
// live code takes the store scan too (the table asserts it).
//
// The copies keep main's code, read markers included: markers in test files
// are neither linted (storeclass) nor ledgered (reviewedMarkers).
//
// The copies reach two api split-strict keys nothing else does: the frozen
// order detail and order feed read through the order readers T7d leaves
// marked, from a test, so their caller is "-"
// (internal/api/testdata/split_strict_baseline.tsv:
// api.orderHistoryBeadAcrossStoreInfos get relic:work orders -, and
// orders.(*Store).ListTracking list relic:work orders -). Deleting this file
// (with the T7f conversion of those readers, or once T7j retires the markers)
// leaves both rows unreached: regenerate the api inputs in the same change.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// errFrozenNoSQL stands in for the SQL fast path the copies leave out.
var errFrozenNoSQL = errors.New("frozen reference: no SQL fast path")

// frozenPrefetchedDepStore is main's prefetchedDepStore.
type frozenPrefetchedDepStore struct {
	beads.Store // embed nil Store — only DepList is called
	deps        map[string][]beads.Dep
}

func (s *frozenPrefetchedDepStore) DepList(id, direction string) ([]beads.Dep, error) {
	if direction == "down" {
		return s.deps[id], nil
	}
	// "up" direction — reverse lookup
	var result []beads.Dep
	for _, deps := range s.deps {
		for _, d := range deps {
			if d.DependsOnID == id {
				result = append(result, d)
			}
		}
	}
	return result, nil
}

func frozenWorkflowStores(state State) []t7dFrozenInfo {
	return frozenWorkflowStoresWith(state, false)
}

// frozenWorkflowStoresIncludingRetiredRigs is frozenWorkflowStores with every rig leg,
// relic-free or not, for a reader that may be looking for rig WORK (an event
// subject), which a relic-free rig still holds.
func frozenWorkflowStoresIncludingRetiredRigs(state State) []t7dFrozenInfo {
	return frozenWorkflowStoresWith(state, true)
}

func frozenWorkflowStoresWith(state State, includeRetiredRigs bool) []t7dFrozenInfo {
	beadStores := state.BeadStores()
	stores := make([]t7dFrozenInfo, 0, len(beadStores)+2)
	cityName := workflowCityScopeRef(state.CityName())
	cityStore := state.CityBeadStore()

	// Graph-first: when [beads.classes.graph] relocates graph beads to a dedicated
	// store, workflow roots/steps/wisps live there, NOT on the work store, so the
	// snapshot scan must consult it. Lead with it (graph-first) so a graph-resident
	// root is found on the first probe. Skip it on a default (non-relocated) city —
	// where Stores().Graph() wraps CityBeadStore() — so the scan stays byte-identical
	// there. It carries the city scope as its fallback (graph roots stamp their own
	// scope metadata, which workflowSnapshotScope prefers) but a distinct ref so the
	// store-scan dedup never conflates it with the city store.
	if graphStore := state.Stores().Graph().Store; graphStore != nil && graphStore != cityStore {
		stores = append(stores, t7dFrozenInfo{
			ref:       workflowGraphStoreRefPrefix + ":" + cityName,
			scopeKind: beadmeta.ScopeKindCity,
			scopeRef:  cityName,
			store:     graphStore,
		})
	}

	if cityStore != nil {
		stores = append(stores, t7dFrozenInfo{
			ref:       "city:" + cityName,
			scopeKind: beadmeta.ScopeKindCity,
			scopeRef:  cityName,
			store:     cityStore,
		})
	}

	for _, rigName := range frozenSortedRigNames(beadStores) {
		if rigName == cityName {
			continue
		}
		store := t7b2FrozenBeadStore(state, rigName)
		if store == nil {
			continue
		}
		// On a split city a rig's graph rows live in the binding leading
		// this list; the rig's own store is read only while it may still
		// hold pre-S4 rows. Once the rig is imported or proven clean (the
		// S4 relic-free predicate) its store holds only work, and no
		// workflow reader here reads work.
		if !includeRetiredRigs && frozenRigGraphLegRetired(state, rigName) {
			continue
		}
		stores = append(stores, t7dFrozenInfo{
			ref:       "rig:" + rigName,
			scopeKind: beadmeta.ScopeKindRig,
			scopeRef:  rigName,
			store:     store,
		})
	}

	return stores
}

func frozenWorkflowStoreByRef(state State, ref string) (t7dFrozenInfo, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return t7dFrozenInfo{}, false
	}

	kind, scopeRef, ok := strings.Cut(ref, ":")
	if !ok {
		return t7dFrozenInfo{}, false
	}
	scopeRef = strings.TrimSpace(scopeRef)
	if scopeRef == "" {
		return t7dFrozenInfo{}, false
	}

	switch strings.TrimSpace(kind) {
	case beadmeta.ScopeKindCity:
		cityStore := state.CityBeadStore()
		cityName := workflowCityScopeRef(state.CityName())
		if cityStore == nil || scopeRef != cityName {
			return t7dFrozenInfo{}, false
		}
		return t7dFrozenInfo{
			ref:       "city:" + cityName,
			scopeKind: beadmeta.ScopeKindCity,
			scopeRef:  cityName,
			store:     cityStore,
		}, true
	case workflowGraphStoreRefPrefix:
		// Round-trip the graph-store ref minted by frozenWorkflowStores when graph is
		// relocated. On a default city (graph == city) there is no graph entry, so
		// this resolves nothing and callers fall back to the store scan.
		graphStore := state.Stores().Graph().Store
		cityName := workflowCityScopeRef(state.CityName())
		if graphStore == nil || graphStore == state.CityBeadStore() || scopeRef != cityName {
			return t7dFrozenInfo{}, false
		}
		return t7dFrozenInfo{
			ref:       workflowGraphStoreRefPrefix + ":" + cityName,
			scopeKind: beadmeta.ScopeKindCity,
			scopeRef:  cityName,
			store:     graphStore,
		}, true
	case ordersClassStoreRefPrefix:
		// Round-trip the orders-class ref minted by frozenAppendOrdersClassStoreInfo: the
		// order history list hands it to a client as the store_ref for every
		// binding-resident tracking bead, and the detail endpoint takes it back. On
		// a default city (orders == city) there is no orders entry to name, so this
		// resolves nothing and callers fall back to the store scan.
		ordersStore := state.Stores().Orders().Store
		cityName := workflowCityScopeRef(state.CityName())
		if ordersStore == nil || ordersStore == state.CityBeadStore() || scopeRef != cityName {
			return t7dFrozenInfo{}, false
		}
		return t7dFrozenInfo{
			ref:       ordersClassStoreRefPrefix + ":" + cityName,
			scopeKind: beadmeta.ScopeKindCity,
			scopeRef:  cityName,
			store:     ordersStore,
		}, true
	case beadmeta.ScopeKindRig:
		store := t7b2FrozenBeadStore(state, scopeRef)
		if store == nil {
			return t7dFrozenInfo{}, false
		}
		return t7dFrozenInfo{
			ref:       "rig:" + scopeRef,
			scopeKind: beadmeta.ScopeKindRig,
			scopeRef:  scopeRef,
			store:     store,
		}, true
	}
	return t7dFrozenInfo{}, false
}

// frozenSortedRigNames returns rig names from the store map in deterministic sorted order,
// deduplicating rigs that share the same underlying store (e.g. file provider mode).
func frozenSortedRigNames(stores map[string]beads.Store) []string {
	names := make([]string, 0, len(stores))
	for name := range stores {
		names = append(names, name)
	}
	sort.Strings(names)
	// Deduplicate by store identity — when multiple rigs share the same
	// store instance (file provider), only keep the first rig name to
	// prevent duplicate results in aggregate queries.
	seen := make(map[beads.Store]bool, len(names))
	deduped := names[:0]
	for _, name := range names {
		s := stores[name]
		if seen[s] {
			continue
		}
		seen[s] = true
		deduped = append(deduped, name)
	}
	return deduped
}

// frozenRigGraphLegRetired reports whether graph readers may skip rig's own store.
// It is true only on a split city whose state proves the rig relic-free; a
// single-store city never retires a rig leg, because there the rig store is
// where the rig's graph rows live.
func frozenRigGraphLegRetired(state State, rig string) bool {
	reporter, ok := state.(t7b2FrozenRigGraphRelicReporter)
	if !ok || rig == "" {
		return false
	}
	return reporter.RigGraphRelicFree(rig)
}

// frozenWorkflowRigLegRetired is frozenRigGraphLegRetired for a workflow store entry: only
// a rig entry can be retired.
func frozenWorkflowRigLegRetired(state State, info t7dFrozenInfo) bool {
	if info.scopeKind != beadmeta.ScopeKindRig {
		return false
	}
	return frozenRigGraphLegRetired(state, info.scopeRef)
}

// frozenWorkflowGraphStoreInfo returns the graph binding's workflow store entry on a
// city that relocates the graph class, and false on every other city (where
// the graph store IS the city store and no separate probe is needed).
func frozenWorkflowGraphStoreInfo(state State) (t7dFrozenInfo, bool) {
	graphStore := state.Stores().Graph().Store
	if graphStore == nil || graphStore == state.CityBeadStore() {
		return t7dFrozenInfo{}, false
	}
	cityName := workflowCityScopeRef(state.CityName())
	return t7dFrozenInfo{
		ref:       workflowGraphStoreRefPrefix + ":" + cityName,
		scopeKind: beadmeta.ScopeKindCity,
		scopeRef:  cityName,
		store:     graphStore,
	}, true
}

// frozenIsGraphConvoyID checks if the bead is a formula-compiled graph convoy
// (workflow) by looking for the gc.kind=workflow marker. It consults the
// dedicated graph store first (graph-first) so a relocated graph convoy id is
// recognized — its bead lives only in the graph store, never on a rig store. On a
// default city the graph store equals the work store, so this leg is byte-identical.
func frozenIsGraphConvoyID(s *Server, id string) bool {
	if graphStore := s.state.Stores().Graph().Store; graphStore != nil && graphStore != s.state.CityBeadStore() {
		// Graph-first leg: a plain (work) convoy id misses here and the rig
		// legs below answer it.
		if b, err := beads.FederatedGet(graphStore, id); err == nil {
			return isGraphConvoyBead(b)
		}
	}
	stores := s.state.BeadStores()
	for _, rigName := range frozenSortedRigNames(stores) {
		// On a split city a rig's graph convoys live in the binding asked
		// above (the S4 placement rule). A rig store is asked only while it
		// may still hold pre-S4 graph rows; a relic-free rig holds only work,
		// whose convoys are never graph convoys.
		if frozenRigGraphLegRetired(s.state, rigName) {
			continue
		}
		store := stores[rigName]
		b, err := frozenRigLegConvoyGet(store, id)
		if err != nil {
			continue
		}
		return isGraphConvoyBead(b)
	}
	return false
}

// frozenRigLegConvoyGet is frozenIsGraphConvoyID's rig leg: a relic read (S4 debt) of a
// graph convoy a rig ledger holds only from before S4a, until the rig is
// imported; a work convoy is that rig's own row.
func frozenRigLegConvoyGet(store beads.Store, id string) (beads.Bead, error) {
	return beads.RelicGet(store, id)
}

func frozenWorkflowEventBeadFromSubject(state State, subjectID string) (beads.Bead, bool) {
	subjectID = strings.TrimSpace(subjectID)
	if subjectID == "" {
		return beads.Bead{}, false
	}

	matches := make([]beads.Bead, 0, 2)
	// Every rig leg, retired or not: the subject may be rig WORK, which a
	// relic-free rig still holds (S4b retires its leg only for graph rows).
	for _, info := range frozenWorkflowStoresIncludingRetiredRigs(state) {
		if info.store == nil {
			continue
		}
		// Relic leg (S4 debt) on a scope store: a city or rig ledger holds these
		// rows only as pre-split copies, so beads.RelicRead counts the read as
		// debt; on the class binding the same read is legitimate.
		// A residence probe over every frozenWorkflowStores leg; only the owner hits.
		bead, err := beads.RelicGet(info.store, subjectID)
		if err == nil {
			matches = append(matches, bead)
		}
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	return beads.Bead{}, false
}

func frozenWorkflowEventRoot(state State, bead beads.Bead) (t7dFrozenInfo, beads.Bead, bool) {
	rootID := strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey])
	if rootID == "" && workflowKind(bead) == "workflow" {
		rootID = bead.ID
	}
	if rootID == "" {
		return t7dFrozenInfo{}, beads.Bead{}, false
	}

	// gc.root_store_ref is the run's SCOPE, never its location (S4 §2.1). On a
	// split city every new root lives in the graph binding whatever its scope,
	// so the binding is probed FIRST: a rig-scoped root there must not be
	// shadowed by the rig store the metadata names, and an id held by both (an
	// import's copy before its clear) resolves to the binding's row, as every
	// by-id read does. The store the metadata names is then probed as the
	// relic leg — a pre-S4 rig tree — unless that rig is proven relic-free.
	if info, ok := frozenWorkflowGraphStoreInfo(state); ok {
		if root, ok := frozenWorkflowRootInStore(info.store, rootID); ok {
			return info, root, true
		}
	}
	if info, ok := frozenWorkflowStoreByRef(state, bead.Metadata[beadmeta.RootStoreRefMetadataKey]); ok && info.store != nil &&
		!frozenWorkflowRigLegRetired(state, info) {
		root, ok := frozenWorkflowRootInStore(info.store, rootID)
		if ok {
			return info, root, true
		}
	}

	matches := make([]t7dFrozenRootMatch, 0, 2)
	for _, info := range frozenWorkflowStores(state) {
		if info.store == nil || frozenWorkflowRigLegRetired(state, info) {
			continue
		}
		// Relic leg (S4 debt) on a scope store: a city or rig ledger holds these
		// rows only as pre-split copies, so beads.RelicRead counts the read as
		// debt; on the class binding the same read is legitimate.
		// A residence probe over every frozenWorkflowStores leg; only the owner
		// hits. (The root_store_ref hint above names one store and is not.)
		var root beads.Bead
		var ok bool
		_ = beads.RelicRead(func() error {
			root, ok = frozenWorkflowRootInStore(info.store, rootID)
			return nil
		})
		if ok {
			matches = append(matches, t7dFrozenRootMatch{info: info, root: root})
		}
	}
	if len(matches) == 1 {
		return matches[0].info, matches[0].root, true
	}
	return t7dFrozenInfo{}, beads.Bead{}, false
}

func frozenWorkflowRootInStore(store beads.Store, rootID string) (beads.Bead, bool) {
	root, err := store.Get(rootID)
	if err != nil || !isWorkflowRoot(root) {
		return beads.Bead{}, false
	}
	return root, true
}

func frozenBuildWorkflowSnapshot(s *Server, workflowID, fallbackScopeKind, fallbackScopeRef string, snapshotIndex uint64) (*workflowSnapshotResponse, error) {
	// Frozen without the SQL fast path: no fixture here has a Dolt server,
	// and the table asserts the live fast path refuses each one.

	// Slow path: no SQL fast path, so resolve the root by consulting the bead
	// stores directly. The root for a workflow lives in exactly one store, and
	// there are two ways to find it: a point Get by bead id (the common
	// graph.v2 case, root.ID == workflowID) and a gc.workflow_id metadata List
	// (the logical-id case). The List is the expensive metadata scan, so sweep
	// every store with the cheap Get first and only fall back to the List sweep
	// when no Get matched — dropping up to one metadata scan per store on the
	// common path. Both phases still consult every store, so cross-store
	// uniqueness (selectWorkflowRootMatch's len(matches)==1 gate) is unchanged
	// (gascity#2940).
	stores := frozenWorkflowStores(s.state)
	storesScanned := make([]string, 0, len(stores))
	seenStoreRefs := make(map[string]bool, len(stores))
	matches := make([]workflowRootMatch, 0)
	seenMatches := make(map[workflowMatchKey]bool)
	frozenOf := make(map[workflowMatchKey]t7dFrozenInfo)
	addMatch := func(info t7dFrozenInfo, root beads.Bead) {
		key := workflowMatchKey{ref: info.ref, id: root.ID}
		if seenMatches[key] {
			return
		}
		seenMatches[key] = true
		frozenOf[key] = info
		matches = append(matches, workflowRootMatch{info: info.live(), root: root})
	}
	// cityScopeRef feeds the legacy city-on-rig preserve path. Derive it from
	// the city name directly (as the SQL path does) instead of harvesting it
	// mid-scan, so it is correct even when the city store yields no match.
	cityScopeRef := workflowCityScopeRef(s.state.CityName())
	var firstGetErr error

	for _, info := range stores {
		if info.store == nil {
			continue
		}
		if !seenStoreRefs[info.ref] {
			storesScanned = append(storesScanned, info.ref)
			seenStoreRefs[info.ref] = true
		}
		// Relic leg (S4 debt) on a scope store: a city or rig ledger holds these
		// rows only as pre-split copies, so beads.RelicRead counts the read as
		// debt; on the class binding the same read is legitimate.
		// One leg of the frozenWorkflowStores federation (graph, city, rigs): a
		// miss here is covered by the leg that owns the root.
		root, err := beads.RelicGet(info.store, workflowID)
		if err != nil {
			if firstGetErr == nil && !errors.Is(err, beads.ErrNotFound) {
				firstGetErr = err
			}
			continue
		}
		if isWorkflowRoot(root) && matchesWorkflowID(root, workflowID) {
			addMatch(info, root)
		}
	}

	// Fall back to the gc.workflow_id metadata sweep only when no store owned
	// the id directly — the logical-workflow-id case Get cannot resolve. A
	// Phase-1 hit is authoritative: Get is keyed by bead id, so a hit means
	// root.ID == workflowID, and a physical bead id is owned by exactly one
	// store. A second root elsewhere could only also match by stamping
	// gc.workflow_id == workflowID, i.e. claiming another root's *physical* id
	// as its own *logical* id — which the id-space separation (sling-prefixed
	// physical ids vs formula-stamped logical ids) does not allow. So once a
	// store owns the id directly, the metadata sweep cannot add a legitimate
	// competing match, and skipping it is safe.
	listPartial := false
	if len(matches) == 0 {
		for _, info := range stores {
			if info.store == nil {
				continue
			}
			var roots []beads.Bead
			// Relic leg (S4 debt) on a scope store: a city or rig ledger holds these
			// rows only as pre-split copies, so beads.RelicRead counts the read as
			// debt; on the class binding the same read is legitimate.
			// One leg of the frozenWorkflowStores federation; see the Get above.
			err := beads.RelicRead(func() error {
				var listErr error
				roots, listErr = info.store.List(beads.ListQuery{
					Metadata: map[string]string{
						beadmeta.KindMetadataKey:       beadmeta.KindWorkflow,
						beadmeta.WorkflowIDMetadataKey: workflowID,
					},
					IncludeClosed: true,
				})
				return listErr
			})
			if err != nil {
				listPartial = true
				continue
			}
			for _, bead := range roots {
				if !matchesWorkflowID(bead, workflowID) {
					continue
				}
				addMatch(info, bead)
			}
		}
	}

	match, ok := selectWorkflowRootMatch(matches, fallbackScopeKind, fallbackScopeRef, cityScopeRef)
	if !ok {
		if firstGetErr != nil {
			return nil, firstGetErr
		}
		return nil, errWorkflowNotFound
	}

	return frozenSnapshotFromStore(s, frozenOf[workflowMatchKey{ref: match.info.ref, id: match.root.ID}], match.root, fallbackScopeKind, fallbackScopeRef, cityScopeRef, storesScanned, listPartial, snapshotIndex)
}

// frozenSnapshotFromStore builds a workflow snapshot for one store. Both of its
// branches implement beads.MembershipDirectRootID — the root plus every bead
// carrying gc.root_bead_id — the SQL fast path as a WHERE clause
// (workflowSQLQueryWorkflowBeads) and the fallback inline below. Neither is a
// dependency walk, deliberately: a gc.kind=spec sidecar has no edges, so a dep
// walk would drop it and hand the dashboard a shorter, plausible step list.
//
// The fallback is spelled out here instead of calling beads.DirectMembers
// because it reads through info.store's own handle. beads.DirectMembers reads
// through the LIVE handle, which on a caching store bypasses the cache — a
// correctness win for the dispatcher writing the molecule, an unwanted cost on
// this dashboard read path. The RULE is shared; the read handle is not.
//
// KNOWN DIVERGENCE (ga-212sl): the fallback lists at the zero-value
// TierMode, so it is TierIssues and drops every ephemeral row, while the SQL
// fast path above queries the wisps table as well. A wisp molecule's beads are
// all ephemeral, so the two branches of this one function return different
// sets for one — and which branch runs depends only on whether the Dolt server
// was reachable, which is exactly the coupling workflowSQLQueryWorkflowBeads's
// doc says must not exist. Fixing it moves this read onto beads.DirectMembers
// (or onto TierBoth), which changes what this endpoint returns for wisp
// molecules; that is its own slice.
func frozenSnapshotFromStore(_ *Server, info t7dFrozenInfo, root beads.Bead, fallbackScopeKind, fallbackScopeRef, cityScopeRef string, storesScanned []string, listPartial bool, snapshotIndex uint64) (*workflowSnapshotResponse, error) {
	// Try direct SQL path — ~500x faster than N+1 bd subprocess calls.
	var (
		workflowBeads []beads.Bead
		beadIndex     map[string]beads.Bead
		depMap        map[string][]beads.Dep
		sqlErr        error
	)
	sqlErr = errFrozenNoSQL // the SQL path is left out; see frozenBuildWorkflowSnapshot
	usedSQL := sqlErr == nil && len(workflowBeads) > 0

	if !usedSQL {
		// Fall back to bd subprocess path.
		all, err := info.store.List(beads.ListQuery{
			Metadata:      map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
			IncludeClosed: true,
		})
		if err != nil {
			return nil, err
		}

		workflowBeads = make([]beads.Bead, 0, len(all)+1)
		seen := make(map[string]struct{}, len(all)+1)
		addBead := func(bead beads.Bead) {
			if bead.ID == "" {
				return
			}
			if _, ok := seen[bead.ID]; ok {
				return
			}
			seen[bead.ID] = struct{}{}
			workflowBeads = append(workflowBeads, bead)
		}
		if freshRoot, err := info.store.Get(root.ID); err == nil {
			addBead(freshRoot)
		}
		for _, bead := range all {
			addBead(bead)
		}

		beadIndex = make(map[string]beads.Bead, len(workflowBeads))
		for _, bead := range workflowBeads {
			beadIndex[bead.ID] = bead
		}
	}

	if len(workflowBeads) == 0 {
		return nil, errWorkflowNotFound
	}

	// Update root from the fetched data (SQL path may have richer data)
	if updated, ok := beadIndex[root.ID]; ok {
		root = updated
	}

	var store beads.Store
	if usedSQL {
		store = &frozenPrefetchedDepStore{deps: depMap}
	} else {
		if prefetchedDeps, ok := prefetchedDepsForWorkflowBeads(workflowBeads); ok {
			store = &frozenPrefetchedDepStore{deps: prefetchedDeps}
		} else {
			store = info.store
		}
	}

	workflowDeps, partial := frozenCollectWorkflowDeps(store, beadIndex)
	partial = partial || listPartial
	scopeKind, scopeRef := workflowSnapshotScope(info.live(), root, fallbackScopeKind, fallbackScopeRef, cityScopeRef)

	beadResponses := make([]workflowBeadResponse, 0, len(workflowBeads))
	for _, bead := range workflowBeads {
		beadResponses = append(beadResponses, workflowBeadResponseFromBead(bead))
	}

	snapshot := &workflowSnapshotResponse{
		WorkflowID:        resolvedWorkflowID(root),
		RootBeadID:        root.ID,
		RootStoreRef:      info.ref,
		ScopeKind:         scopeKind,
		ScopeRef:          scopeRef,
		Beads:             beadResponses,
		Deps:              workflowDeps,
		LogicalNodes:      []LogicalNode{},
		LogicalEdges:      []workflowDepResponse{},
		ScopeGroups:       []ScopeGroup{},
		Partial:           partial,
		ResolvedRootStore: info.ref,
		StoresScanned:     storesScanned,
		SnapshotVersion:   snapshotIndex,
	}
	if snapshotIndex > 0 {
		snapshot.SnapshotEventSeq = &snapshotIndex
	}
	return snapshot, nil
}

// frozenCollectWorkflowDeps returns the physical bead-to-bead dependencies.
// Logical edge computation is handled by the real-world app server's presentation layer.
func frozenCollectWorkflowDeps(store beads.Store, beadIndex map[string]beads.Bead) ([]workflowDepResponse, bool) {
	workflowDeps := make([]workflowDepResponse, 0)
	seen := map[string]bool{}
	partial := false

	for beadID := range beadIndex {
		deps, err := store.DepList(beadID, "down")
		if err != nil {
			partial = true
			continue
		}
		for _, dep := range deps {
			if _, ok := beadIndex[dep.DependsOnID]; !ok {
				continue
			}
			edge := workflowDepResponse{
				From: dep.DependsOnID,
				To:   dep.IssueID,
				Kind: dep.Type,
			}
			key := edge.From + "|" + edge.To + "|" + edge.Kind
			if !seen[key] {
				workflowDeps = append(workflowDeps, edge)
				seen[key] = true
			}
		}
	}

	return workflowDeps, partial
}

// frozenHumaDeleteWorkflow handles workflow convoy deletion through the Huma handler.
func frozenHumaDeleteWorkflow(s *Server, workflowID string) (*OKResponse, error) {
	stores := frozenWorkflowStores(s.state)
	found := false

	for _, info := range stores {
		if info.store == nil {
			continue
		}

		// open is the subset the close may stamp: the tree is listed with
		// IncludeClosed, and a store that batches the close without reading each
		// status first writes the skip onto every id it is handed, rewriting the
		// outcome of a step that already finished.
		var open []string
		seen := make(map[string]struct{}, 4)
		rootIDs := make([]string, 0, 2)
		rootSeen := make(map[string]struct{}, 2)
		addBead := func(b beads.Bead) {
			if b.ID == "" || coordclass.ControllerOwned(b) { // never a controller-owned row (mc-dkblm, architecture B)
				return
			}
			if _, ok := seen[b.ID]; ok {
				return
			}
			seen[b.ID] = struct{}{}
			if b.Status != "closed" {
				open = append(open, b.ID)
			}
		}
		addRoot := func(root beads.Bead) {
			if !isWorkflowRoot(root) || !matchesWorkflowID(root, workflowID) {
				return
			}
			if _, ok := rootSeen[root.ID]; ok {
				return
			}
			rootSeen[root.ID] = struct{}{}
			rootIDs = append(rootIDs, root.ID)
			addBead(root)
		}
		// Relic leg (S4 debt): a scope store holds graph roots and their
		// steps only as rows written before the split; beads.RelicGet and
		// beads.RelicRead count each read as debt, as frozenHumaHandleWorkflowDelete
		// does.
		if root, err := beads.RelicGet(info.store, workflowID); err == nil {
			addRoot(root)
		}
		var roots []beads.Bead
		if err := beads.RelicRead(func() error {
			var listErr error
			roots, listErr = info.store.List(beads.ListQuery{
				Metadata: map[string]string{
					beadmeta.KindMetadataKey:       beadmeta.KindWorkflow,
					beadmeta.WorkflowIDMetadataKey: workflowID,
				},
				IncludeClosed: true,
			})
			return listErr
		}); err == nil {
			for _, root := range roots {
				addRoot(root)
			}
		}
		for _, rootID := range rootIDs {
			var all []beads.Bead
			if err := beads.RelicRead(func() error {
				var listErr error
				all, listErr = info.store.List(beads.ListQuery{
					Metadata:      map[string]string{beadmeta.RootBeadIDMetadataKey: rootID},
					IncludeClosed: true,
				})
				return listErr
			}); err != nil {
				continue
			}
			for _, b := range all {
				addBead(b)
			}
		}
		if len(seen) == 0 {
			continue
		}
		found = true
		if len(open) == 0 {
			continue
		}
		info.store.CloseAll(open, map[string]string{ //nolint:errcheck
			beadmeta.OutcomeMetadataKey: beadmeta.OutcomeSkipped,
			"close_reason":              sourceworkflow.WorkflowSkippedCloseReason,
		})
	}

	if !found {
		return nil, apierr.WorkflowNotFound.Msg("workflow " + workflowID + " not found")
	}

	resp := &OKResponse{}
	resp.Body.Status = "deleted"
	return resp, nil
}

// frozenHumaHandleWorkflowDelete is the Huma-typed handler for DELETE /v0/workflow/{workflow_id}.
// Backward-compatible alias for the convoy/workflow delete endpoint.
func frozenHumaHandleWorkflowDelete(_ context.Context, s *Server, input *WorkflowDeleteInput) (*struct {
	Body workflowDeleteResponse
}, error,
) {
	workflowID := strings.TrimSpace(input.WorkflowID)
	if workflowID == "" {
		return nil, apierr.InvalidRequest.Msg("convoy id is required")
	}

	scopeKind := strings.TrimSpace(input.ScopeKind)
	scopeRef := strings.TrimSpace(input.ScopeRef)
	deleteFromStore := input.Delete

	stores := frozenWorkflowStores(s.state)

	closed := 0
	deleted := 0
	found := false
	var pa partialAggregator

	for _, info := range stores {
		if info.store == nil {
			continue
		}
		if scopeKind != "" && info.scopeKind != scopeKind {
			continue
		}
		if scopeRef != "" && info.scopeRef != scopeRef {
			continue
		}

		// ids is the whole tree, which Phase 2 deletes. open is the subset Phase
		// 1 may stamp: the tree is listed with IncludeClosed, and a store that
		// batches the close without reading each status first writes the skip
		// onto every id it is handed, rewriting the outcome of a step that
		// already finished.
		var ids, open []string
		seen := make(map[string]struct{}, 4)
		rootIDs := make([]string, 0, 2)
		rootSeen := make(map[string]struct{}, 2)
		addBead := func(b beads.Bead) {
			if b.ID == "" || coordclass.ControllerOwned(b) { // never a controller-owned row (mc-dkblm, architecture B)
				return
			}
			if _, ok := seen[b.ID]; ok {
				return
			}
			seen[b.ID] = struct{}{}
			ids = append(ids, b.ID)
			if b.Status != "closed" {
				open = append(open, b.ID)
			}
		}
		addRoot := func(root beads.Bead) {
			if !isWorkflowRoot(root) || !matchesWorkflowID(root, workflowID) {
				return
			}
			if _, ok := rootSeen[root.ID]; ok {
				return
			}
			rootSeen[root.ID] = struct{}{}
			rootIDs = append(rootIDs, root.ID)
			addBead(root)
		}
		// Relic leg (S4 debt) on a scope store: a city or rig ledger holds these
		// rows only as pre-split copies, so beads.RelicRead counts the read as
		// debt; on the class binding the same read is legitimate.
		// One leg of the frozenWorkflowStores federation (graph, city, rigs): a
		// miss on either root probe is covered by the leg that owns the root.
		if root, err := beads.RelicGet(info.store, workflowID); err == nil {
			addRoot(root)
		} else if !errors.Is(err, beads.ErrNotFound) {
			pa.record("store "+info.scopeRef+" get root", err)
		}
		var roots []beads.Bead
		if err := beads.RelicRead(func() error {
			var listErr error
			roots, listErr = info.store.List(beads.ListQuery{
				Metadata: map[string]string{
					beadmeta.KindMetadataKey:       beadmeta.KindWorkflow,
					beadmeta.WorkflowIDMetadataKey: workflowID,
				},
				IncludeClosed: true,
			})
			return listErr
		}); err == nil {
			for _, root := range roots {
				addRoot(root)
			}
		} else {
			pa.record("store "+info.scopeRef+" list roots", err)
		}
		for _, rootID := range rootIDs {
			var all []beads.Bead
			if err := beads.RelicRead(func() error {
				var listErr error
				all, listErr = info.store.List(beads.ListQuery{
					Metadata:      map[string]string{beadmeta.RootBeadIDMetadataKey: rootID},
					IncludeClosed: true,
				})
				return listErr
			}); err != nil {
				pa.record("store "+info.scopeRef+" list descendants", err)
				continue
			}
			for _, b := range all {
				addBead(b)
			}
		}
		if len(ids) == 0 {
			continue
		}
		found = true

		// Phase 1: Batch close all open beads.
		if len(open) > 0 {
			n, closeErr := info.store.CloseAll(open, map[string]string{
				beadmeta.OutcomeMetadataKey: beadmeta.OutcomeSkipped,
				"close_reason":              sourceworkflow.WorkflowSkippedCloseReason,
			})
			closed += n
			if closeErr != nil {
				pa.record("store "+info.scopeRef+" close", closeErr)
			}
		}

		// Phase 2: Delete if requested.
		if deleteFromStore {
			for _, id := range ids {
				if deps, err := info.store.DepList(id, "down"); err == nil {
					for _, dep := range deps {
						if err := info.store.DepRemove(id, dep.DependsOnID); err != nil {
							pa.record("store "+info.scopeRef+" dep-remove "+id+"→"+dep.DependsOnID, err)
						}
					}
				} else {
					pa.record("store "+info.scopeRef+" dep-list down "+id, err)
				}
				if deps, err := info.store.DepList(id, "up"); err == nil {
					for _, dep := range deps {
						if err := info.store.DepRemove(dep.IssueID, id); err != nil {
							pa.record("store "+info.scopeRef+" dep-remove "+dep.IssueID+"→"+id, err)
						}
					}
				} else {
					pa.record("store "+info.scopeRef+" dep-list up "+id, err)
				}
				if err := info.store.Delete(id); err != nil {
					pa.record("store "+info.scopeRef+" delete "+id, err)
					continue
				}
				deleted++
			}
		}
	}

	if !found {
		return nil, apierr.WorkflowNotFound.Msg("workflow " + workflowID + " not found")
	}

	return &struct {
		Body workflowDeleteResponse
	}{Body: workflowDeleteResponse{
		WorkflowID:    workflowID,
		Closed:        closed,
		Deleted:       deleted,
		Partial:       pa.partial(),
		PartialErrors: pa.messages(),
	}}, nil
}

type frozenRunCancelTarget struct {
	store     beads.Store
	root      beads.Bead
	workflows []string
}

// frozenCancelRun winds down the run rooted at runID across every workflow store,
// closing each open root and its still-OPEN member beads with a canceled
// outcome, in three steps ordered so the run's work stops as early as it can:
//
//  1. Mark each open run root canceling: gc.cancel_requested, one write per
//     root and none when a previous attempt already marked it. From here on
//     the claim gate (molecule.WorkflowStopReason) refuses every open step of
//     the run, including the steps of drain item workflows whose own roots
//     are still open, since their ancestry reaches the marked root; the
//     dispatcher's control gate closes the run's controls; the workflow
//     finalizer settles as canceled; and the run projects "canceling".
//  2. Read the run's workflow tree (sourceworkflow.WorkflowTree: the run and
//     every drain item workflow nested under it at any depth, one member read
//     per workflow) and collect the assignees of the in-progress steps the
//     close will sweep, then hand them to interrupt, which must not block: the
//     interrupts run alongside the closes. The tree is read after the mark, so
//     a claim that landed before the mark is seen here and one after it is
//     refused by the gate.
//  3. Close each open workflow, nested drain items deepest first and the run
//     root last, reusing the workflow teardown close ordering (descendants
//     before the root, blockers before blocked) so a strict store accepts the
//     batch. Each root's close stamps the cancel marker on it too, so a nested
//     item's own controls stop as canceled.
//
// Any store read/write failure is returned so the caller reports a retryable
// 5xx rather than a phantom success. Every state a failure (or a crash) can
// leave is one a retry completes: the run root stays open until its own close,
// the last write, so the retry finds the run again instead of 409ing; if the
// mark landed the run is already stopped meanwhile; already-closed members and
// nested roots are left as they are, and the retry interrupts whoever still
// holds an in-progress step. Already-terminal runs (and already-closed members)
// are left untouched: closing a completed member would rewrite its recorded
// outcome. Each workflow's teardown tail (molecule.TeardownTailExclusion) is
// spared: it runs AFTER its root reaches a terminal state by contract, and its
// pass condition may read ROOT_OUTCOME, which this cancel's own close stamps;
// force-closing it unexecuted would strand the resources it releases.
func frozenCancelRun(s *Server, runID string, interrupt func(holders []string)) (cancelRunResult, error) {
	var res cancelRunResult
	var targets []frozenRunCancelTarget
	for _, info := range frozenWorkflowStores(s.state) {
		if info.store == nil {
			continue
		}
		// Relic leg (S4 debt) on a scope store: a city or rig ledger holds these
		// rows only as pre-split copies, so beads.RelicRead counts the read as
		// debt; on the class binding the same read is legitimate.
		// One leg of the frozenWorkflowStores federation (graph, city, rigs): the
		// root probes miss on every leg but the one that owns the run.
		var roots []beads.Bead
		err := beads.RelicRead(func() error {
			var findErr error
			roots, findErr = frozenFindWorkflowRoots(info.store, runID)
			return findErr
		})
		if err != nil {
			return res, err
		}
		for _, root := range roots {
			res.found = true
			if isClosedStatus(root.Status) {
				continue // already terminal — nothing to wind down
			}
			targets = append(targets, frozenRunCancelTarget{store: info.store, root: root})
		}
	}
	if len(targets) == 0 {
		res.terminal = res.found
		return res, nil
	}

	for _, target := range targets {
		if strings.TrimSpace(target.root.Metadata[beadmeta.CancelRequestedMetadataKey]) != "" {
			continue // marked by an earlier attempt
		}
		if err := target.store.SetMetadata(target.root.ID, beadmeta.CancelRequestedMetadataKey, "true"); err != nil {
			return res, err
		}
	}

	var holders []string
	for i := range targets {
		held, err := frozenPlanRunCancel(&targets[i])
		if err != nil {
			return res, err
		}
		holders = append(holders, held...)
	}
	if len(holders) > 0 && interrupt != nil {
		interrupt(holders)
	}

	for _, target := range targets {
		for _, rootID := range target.workflows {
			// target.store may hold a graph or a v1 work molecule: a relabel until the T7 Molecule intent (typed-store T5a).
			n, err := frozenCancelWorkflowRoot(beads.GraphMolecule(beads.GraphStore{Store: target.store}), rootID)
			res.closed += n
			if err != nil {
				return res, err
			}
		}
		res.status = RunStatusCanceled
	}
	return res, nil
}

// frozenCancelWorkflowRoot closes one workflow root and its open members as
// canceled, stamping the cancel-intent marker on the root alone and sparing its
// teardown tail. A nested drain item root is canceled exactly like the run
// root: it is a workflow root in its own right, so its own controls read
// gc.cancel_requested off it and its own teardown tail still runs.
func frozenCancelWorkflowRoot(store beads.MoleculeTarget, rootID string) (int, error) {
	exclude, err := molecule.TeardownTailExclusion(store, rootID)
	if err != nil {
		return 0, err
	}
	return sourceworkflow.CloseWorkflowSubtreeAsExcept(
		store,
		rootID,
		beadmeta.OutcomeCanceled,
		runCanceledCloseReason,
		map[string]string{beadmeta.CancelRequestedMetadataKey: "true"},
		exclude,
	)
}

// frozenPlanRunCancel reads target's workflow tree, records the open workflows the
// cancel closes (deepest first, the run root last), and returns the assignees
// of the in-progress members those closes will sweep, in first-seen order and
// possibly repeated. A nested item workflow already closed is neither closed
// again nor searched for holders. A spared teardown step is not swept, so its
// holder is not returned; the teardown tail is judged from the members the
// tree walk already read (every member of a graph workflow carries its
// gc.root_bead_id) rather than from a further subtree read, which the close
// makes for itself, so that no read beyond the walk delays the interrupts.
func frozenPlanRunCancel(target *frozenRunCancelTarget) ([]string, error) {
	// target.store may hold a graph or a v1 work molecule: a relabel until the T7 Molecule intent (typed-store T5a).
	tree, err := sourceworkflow.WorkflowTree(beads.GraphMolecule(beads.GraphStore{Store: target.store}), target.root.ID)
	if err != nil {
		return nil, err
	}
	var holders []string
	for _, node := range tree {
		if node.Root.ID == "" || isClosedStatus(node.Root.Status) {
			continue
		}
		exclude := molecule.TeardownTailExclusionFromMembers(node.Members)
		holders = append(holders, inProgressAssignees(node.Members, exclude)...)
		target.workflows = append(target.workflows, node.Root.ID)
	}
	return holders, nil
}

// frozenFindWorkflowRoots returns the graph-workflow roots in store that match runID
// (Get by id + List by kind=workflow/workflow_id). Root membership uses
// sourceworkflow.IsWorkflowRoot (gc.kind=workflow OR gc.formula_contract=graph.v2)
// so a graph.v2-only root — one the Run resource lists but that lacks the
// gc.kind=workflow marker — is still cancellable, not a false 404. A non-NotFound
// store error is returned so the caller reports a 5xx.
func frozenFindWorkflowRoots(store beads.Store, runID string) ([]beads.Bead, error) {
	var roots []beads.Bead
	seen := map[string]bool{}
	add := func(root beads.Bead) {
		if !sourceworkflow.IsWorkflowRoot(root) || !matchesWorkflowID(root, runID) {
			return
		}
		if seen[root.ID] {
			return
		}
		seen[root.ID] = true
		roots = append(roots, root)
	}
	if root, err := store.Get(runID); err == nil {
		add(root)
	} else if !errors.Is(err, beads.ErrNotFound) {
		return nil, err
	}
	list, err := store.List(beads.ListQuery{
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindWorkflow,
			beadmeta.WorkflowIDMetadataKey: runID,
		},
		IncludeClosed: true,
	})
	if err != nil {
		return nil, err
	}
	for _, r := range list {
		add(r)
	}
	return roots, nil
}

func frozenBuildWorkflowRunProjections(state State, requestedScopeKind, requestedScopeRef, formulaNameFilter string) (workflowRunProjectionResult, error) {
	stores := frozenWorkflowStores(state)
	projections := make([]workflowRunProjection, 0)
	partialErrors := make([]string, 0)
	cityScopeRef := workflowCityScopeRef(state.CityName())
	includeAllForCity := requestedScopeKind == beadmeta.ScopeKindCity && requestedScopeRef == cityScopeRef
	var requestedScopeErr error

	for _, info := range stores {
		if info.store == nil {
			continue
		}
		openBeads, err := frozenListActiveWorkflowProjectionBeads(info.store)
		if err != nil {
			if requestedScopeErr == nil && info.scopeKind == requestedScopeKind && info.scopeRef == requestedScopeRef {
				requestedScopeErr = err
			}
			if includeAllForCity {
				msg := info.ref + " store unavailable"
				log.Printf("api: workflow run projection list failed for %s: %v", info.ref, err)
				partialErrors = append(partialErrors, msg)
			}
			continue
		}

		openChildrenByRoot := make(map[string][]beads.Bead)
		for _, bead := range openBeads {
			rootID := strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey])
			if rootID == "" {
				continue
			}
			openChildrenByRoot[rootID] = append(openChildrenByRoot[rootID], bead)
		}

		// Relic leg (S4 debt) on a scope store: a city or rig ledger holds these
		// rows only as pre-split copies, so beads.RelicRead counts the read as
		// debt; on the class binding the same read is legitimate.
		// One leg of the frozenWorkflowStores federation (graph, city, rigs): each
		// leg lists only the roots it holds.
		var roots []beads.Bead
		err = beads.RelicRead(func() error {
			var listErr error
			roots, listErr = info.store.List(beads.ListQuery{
				Metadata: map[string]string{
					beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
					beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
				},
				IncludeClosed: true,
			})
			return listErr
		})
		if err != nil {
			log.Printf("api: workflow run projection closed-root list failed for %s: %v", info.ref, err)
			roots = nil
			for _, bead := range openBeads {
				if isWorkflowRoot(bead) && strings.TrimSpace(bead.Metadata[beadmeta.FormulaContractMetadataKey]) == beadmeta.FormulaContractGraphV2 {
					roots = append(roots, bead)
				}
			}
			if includeAllForCity {
				partialErrors = append(partialErrors, info.ref+" workflow history incomplete")
			}
		}

		for _, bead := range roots {
			if !isWorkflowRoot(bead) {
				continue
			}
			if formulaNameFilter != "" && workflowFormulaName(bead) != formulaNameFilter {
				continue
			}

			scopeKind, scopeRef := workflowProjectionScope(info.live(), bead, cityScopeRef, requestedScopeKind, requestedScopeRef)
			if !includeAllForCity && (scopeKind != requestedScopeKind || scopeRef != requestedScopeRef) {
				continue
			}

			runBeads := append([]beads.Bead{bead}, openChildrenByRoot[bead.ID]...)
			children, childErr := info.store.List(beads.ListQuery{
				Metadata:      map[string]string{beadmeta.RootBeadIDMetadataKey: bead.ID},
				IncludeClosed: true,
			})
			if childErr != nil {
				log.Printf("api: workflow run projection child list failed for %s root %s: %v", info.ref, bead.ID, childErr)
				partialErrors = append(partialErrors, bead.ID+" workflow history incomplete")
			} else {
				seen := make(map[string]bool, len(runBeads))
				for _, existing := range runBeads {
					seen[existing.ID] = true
				}
				for _, child := range children {
					if seen[child.ID] {
						continue
					}
					runBeads = append(runBeads, child)
				}
			}
			projection := workflowRunProjection{
				WorkflowID:     resolvedWorkflowID(bead),
				FormulaName:    workflowFormulaName(bead),
				Title:          workflowProjectionTitle(bead),
				Status:         normalizeMonitorStatus(aggregateWorkflowRunStatus(bead, runBeads)),
				Target:         workflowProjectionTarget(bead),
				StartedAt:      bead.CreatedAt,
				UpdatedAt:      workflowProjectionUpdatedAt(runBeads),
				ScopeKind:      scopeKind,
				ScopeRef:       scopeRef,
				RootBeadID:     bead.ID,
				RootStoreRef:   info.ref,
				AttachedBeadID: strings.TrimSpace(bead.Metadata[beadmeta.SourceBeadIDMetadataKey]),
			}
			projections = append(projections, projection)
		}
	}

	if len(projections) == 0 && requestedScopeErr != nil && !includeAllForCity {
		return workflowRunProjectionResult{}, requestedScopeErr
	}

	sortWorkflowRunProjections(projections)

	return workflowRunProjectionResult{
		Items:         projections,
		Partial:       len(partialErrors) > 0,
		PartialErrors: partialErrors,
	}, nil
}

// frozenBuildWorkflowRunProjectionsRootOnly builds workflow run projections using
// only root beads and their open children.  It intentionally skips per-root
// closed-child lookups for speed, so status and UpdatedAt may lag behind
// the full projection path.  Use this for monitor/feed views where freshness
// matters more than precision.
func frozenBuildWorkflowRunProjectionsRootOnly(state State, requestedScopeKind, requestedScopeRef string) (workflowRunProjectionResult, error) {
	stores := frozenWorkflowStores(state)
	projections := make([]workflowRunProjection, 0)
	partialErrors := make([]string, 0)
	cityScopeRef := workflowCityScopeRef(state.CityName())
	includeAllForCity := requestedScopeKind == beadmeta.ScopeKindCity && requestedScopeRef == cityScopeRef
	var requestedScopeErr error

	for _, info := range stores {
		if info.store == nil {
			continue
		}
		openBeads, err := frozenListActiveWorkflowProjectionBeads(info.store)
		if err != nil {
			if requestedScopeErr == nil && info.scopeKind == requestedScopeKind && info.scopeRef == requestedScopeRef {
				requestedScopeErr = err
			}
			if includeAllForCity {
				msg := info.ref + " store unavailable"
				log.Printf("api: workflow root projection list failed for %s: %v", info.ref, err)
				partialErrors = append(partialErrors, msg)
			}
			continue
		}

		openChildrenByRoot := make(map[string][]beads.Bead)
		for _, bead := range openBeads {
			rootID := strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey])
			if rootID == "" {
				continue
			}
			openChildrenByRoot[rootID] = append(openChildrenByRoot[rootID], bead)
		}

		// Relic leg (S4 debt) on a scope store: a city or rig ledger holds these
		// rows only as pre-split copies, so beads.RelicRead counts the read as
		// debt; on the class binding the same read is legitimate.
		// One leg of the frozenWorkflowStores federation (graph, city, rigs): each
		// leg lists only the roots it holds.
		var roots []beads.Bead
		err = beads.RelicRead(func() error {
			var listErr error
			roots, listErr = info.store.List(beads.ListQuery{
				Metadata: map[string]string{
					beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
					beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
				},
				IncludeClosed: true,
				// The root-only projection reads only id/status/created/metadata,
				// never labels, so skip the per-root label hydration on this
				// closed-history scan — same rows and order, less work per root
				// (gascity#3253). Bounding this scan further is not contract-safe
				// because the feed orders by computed status rank, not created_at.
				SkipLabels: true,
			})
			return listErr
		})
		if err != nil {
			if requestedScopeErr == nil && info.scopeKind == requestedScopeKind && info.scopeRef == requestedScopeRef {
				requestedScopeErr = err
			}
			log.Printf("api: workflow root projection closed-root list failed for %s: %v", info.ref, err)
			roots = nil
			for _, bead := range openBeads {
				if isWorkflowRoot(bead) && strings.TrimSpace(bead.Metadata[beadmeta.FormulaContractMetadataKey]) == beadmeta.FormulaContractGraphV2 {
					roots = append(roots, bead)
				}
			}
			partialErrors = append(partialErrors, info.ref+" workflow history incomplete")
		}

		for _, root := range roots {
			if !isWorkflowRoot(root) {
				continue
			}

			scopeKind, scopeRef := workflowProjectionScope(info.live(), root, cityScopeRef, requestedScopeKind, requestedScopeRef)
			if !includeAllForCity && (scopeKind != requestedScopeKind || scopeRef != requestedScopeRef) {
				continue
			}

			runBeads := append([]beads.Bead{root}, openChildrenByRoot[root.ID]...)
			projections = append(projections, workflowRunProjection{
				WorkflowID:     resolvedWorkflowID(root),
				FormulaName:    workflowFormulaName(root),
				Title:          workflowProjectionTitle(root),
				Status:         normalizeMonitorStatus(aggregateWorkflowRunStatus(root, runBeads)),
				Target:         workflowProjectionTarget(root),
				StartedAt:      root.CreatedAt,
				UpdatedAt:      workflowProjectionUpdatedAt(runBeads),
				ScopeKind:      scopeKind,
				ScopeRef:       scopeRef,
				RootBeadID:     root.ID,
				RootStoreRef:   info.ref,
				AttachedBeadID: strings.TrimSpace(root.Metadata[beadmeta.SourceBeadIDMetadataKey]),
			})
		}
	}

	if len(projections) == 0 && requestedScopeErr != nil && !includeAllForCity {
		return workflowRunProjectionResult{}, requestedScopeErr
	}

	sortWorkflowRunProjections(projections)

	return workflowRunProjectionResult{
		Items:         projections,
		Partial:       len(partialErrors) > 0,
		PartialErrors: partialErrors,
	}, nil
}

func frozenListActiveWorkflowProjectionBeads(store beads.Store) ([]beads.Bead, error) {
	// One Live, status-scoped read per active status, unioned by ID.
	//
	// The old raw scan could not gate status at all (gc-4zb): mapBdStatus folds
	// bd's blocked/deferred/review/testing into Gas City's three statuses, so a
	// scanned blocked root arrives with Status "open" and is indistinguishable
	// from ready work. Filtering the snapshot on b.Status keeps every one of
	// them for the same reason. Only the backing store filters on the raw
	// status, by passing --status to bd, and only a Live query reaches it — a
	// cached read matches on the collapsed status.
	//
	// This matters because the workflow-root spawn path selects on gc.routed_to
	// without re-checking status: a blocked root that still carries a route is
	// spawned against and burns a polecat slot on a no-op drain (gc-nz5i).
	seen := make(map[string]struct{})
	var active []beads.Bead
	for _, status := range activeWorkflowProjectionStatuses {
		items, err := store.List(beads.ListQuery{Status: status, AllowScan: true, Live: true})
		if err != nil {
			return nil, fmt.Errorf("listing %s workflow projection beads: %w", status, err)
		}
		for _, b := range items {
			if _, dup := seen[b.ID]; dup {
				continue
			}
			seen[b.ID] = struct{}{}
			active = append(active, b)
		}
	}
	return active, nil
}

func frozenBuildOrderRunFeedItems(state State, requestedScopeKind, requestedScopeRef string, limit int) (orderRunFeedResult, error) {
	// The feed lists order-tracking beads, which are orders class and live in
	// the orders binding on a split city. frozenWorkflowStores leads with the GRAPH
	// binding (its own callers scan workflow roots), so on a city that relocates
	// only graph the tracking beads would still be missed; the orders leg is
	// appended here and deduplicated, so a city that serves both classes from
	// one binding — the shape this build supports — reads it once.
	stores := frozenAppendOrdersClassStoreInfo(frozenWorkflowStores(state), state, workflowCityScopeRef(state.CityName()))
	allOrders := state.OrdersAll()
	orderByScopedName := make(map[string]orders.Order, len(allOrders))
	for _, order := range allOrders {
		orderByScopedName[order.ScopedName()] = order
	}

	cityScopeRef := workflowCityScopeRef(state.CityName())
	includeAllForCity := requestedScopeKind == beadmeta.ScopeKindCity && requestedScopeRef == cityScopeRef
	items := make([]monitorFeedItemResponse, 0)
	partialErrors := make([]string, 0)
	var requestedScopeErr error
	for _, info := range stores {
		if info.store == nil {
			continue
		}
		front := orders.NewStore(beads.OrdersStore{Store: info.store})
		// Relic leg (S4 debt) on a scope store: a city or rig ledger holds these
		// rows only as pre-split copies, so beads.RelicRead counts the read as
		// debt; on the class binding the same read is legitimate.
		// One leg of the order-feed federation (graph, orders binding, and
		// the city and rig scopes for pre-cutover history).
		var runs []orders.OrderRun
		err := beads.RelicRead(func() error {
			var listErr error
			runs, listErr = front.ListTracking(limit)
			return listErr
		})
		if err != nil {
			if requestedScopeErr == nil && info.scopeKind == requestedScopeKind && info.scopeRef == requestedScopeRef {
				requestedScopeErr = err
			}
			if includeAllForCity {
				msg := info.ref + " store unavailable"
				log.Printf("api: order feed list failed for %s: %v", info.ref, err)
				partialErrors = append(partialErrors, msg)
			}
			continue
		}

		// Scoped to this store's front: LatestOpenRun is a per-store lookup, so
		// a cache keyed only by run.Scoped must not be reused across stores.
		// Runs sharing a scoped name within this store's result (repeat runs of
		// the same order) cost one backing lookup instead of one per run.
		latestOpenCache := make(map[string]latestOpenRunLookup, len(runs))
		for _, run := range runs {
			scopeKind, scopeRef := orderTrackingScope(run.Scoped, cityScopeRef)
			if !includeAllForCity && (scopeKind != requestedScopeKind || scopeRef != requestedScopeRef) {
				continue
			}

			updatedAt := t7d2FrozenOrderTrackingUpdatedAt(front, run, latestOpenCache)
			orderDef, ok := orderByScopedName[run.Scoped]
			title := orderTrackingTitle(run.Scoped, orderDef, ok)
			target := orderTrackingTarget(orderDef, ok, run)
			itemType := orderTrackingType(orderDef, ok, run)
			item := monitorFeedItemResponse{
				ID:                 "order:" + info.ref + ":" + run.ID,
				Type:               itemType,
				Status:             normalizeMonitorStatus(run.State()),
				Title:              title,
				ScopeKind:          scopeKind,
				ScopeRef:           scopeRef,
				Target:             target,
				StartedAt:          run.CreatedAt.Format(time.RFC3339Nano),
				UpdatedAt:          updatedAt.Format(time.RFC3339Nano),
				BeadID:             run.ID,
				StoreRef:           info.ref,
				DetailAvailable:    ok && orderDef.IsExec(),
				RunDetailAvailable: ok && orderDef.IsExec(),
			}
			items = append(items, item)
		}
	}

	if requestedScopeErr != nil && !includeAllForCity {
		return orderRunFeedResult{}, requestedScopeErr
	}

	return orderRunFeedResult{
		Items:         items,
		Partial:       len(partialErrors) > 0,
		PartialErrors: partialErrors,
	}, nil
}

// frozenAppendOrdersClassStoreInfo adds the orders-class binding to an order read's
// store list, unless the list already reads it.
//
// The controller creates every order-tracking bead in this store, so a read that
// skips it reports a split city's orders as never having run — `gc order check`
// through the API sees no last run and calls a just-fired order due, and
// `gc order history` returns nothing. The scope stores stay in the list because a
// converged city holds the pre-cutover history in them.
//
// Skipped whenever the binding is one of the stores already present, which is
// every city that relocates nothing: Stores().Orders() wraps CityBeadStore()
// there, so the read stays byte-identical rather than scanning one database
// twice.
func frozenAppendOrdersClassStoreInfo(infos []t7dFrozenInfo, state State, cityName string) []t7dFrozenInfo {
	ordersStore := state.Stores().Orders().Store
	if ordersStore == nil {
		return infos
	}
	for _, info := range infos {
		if info.store == ordersStore {
			return infos
		}
	}
	return append(infos, t7dFrozenInfo{
		ref:       ordersClassStoreRefPrefix + ":" + cityName,
		scopeKind: beadmeta.ScopeKindCity,
		scopeRef:  cityName,
		store:     ordersStore,
	})
}

// humaHandleOrderHistoryDetail is the Huma-typed handler for GET /v0/order/history/{bead_id}.
func frozenHumaHandleOrderHistoryDetail(_ context.Context, s *Server, input *OrderHistoryDetailInput) (*struct {
	Body orderHistoryDetailResponse
}, error,
) {
	// The bead this reads is an order-tracking bead, which on a split city lives in
	// the orders binding. workflowStores leads with the GRAPH binding, so without
	// the orders leg this only finds one because the shape this build serves
	// happens to serve both classes from one store — the co-residency the feed
	// already refuses to rely on.
	storeInfos := frozenAppendOrdersClassStoreInfo(frozenWorkflowStores(s.state), s.state, workflowCityScopeRef(s.state.CityName()))
	if input.StoreRef != "" {
		info, ok := frozenWorkflowStoreByRef(s.state, input.StoreRef)
		if !ok {
			return nil, apierr.ScopeNotFound.Msg("store_ref does not resolve to a known scope")
		}
		storeInfos = []t7dFrozenInfo{info}
	}
	result, err := t7d2FrozenOrderHistoryBeadAcrossStoreInfos(storeInfos, input.BeadID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil, apierr.BeadNotFound.Msg("bead not found")
		}
		if errors.Is(err, errNoOrderStores) {
			return nil, apierr.ServiceUnavailable.Msg(err.Error())
		}
		return nil, apierr.Internal.Msg(err.Error())
	}
	b := result.bead

	output := convergence.GateOutputFromMetadata(b.Metadata).CombinedOutput()

	return &struct {
		Body orderHistoryDetailResponse
	}{Body: orderHistoryDetailResponse{
		BeadID:    b.ID,
		StoreRef:  result.storeRef,
		CreatedAt: b.CreatedAt.Format(time.RFC3339Nano),
		Labels:    b.Labels,
		Output:    output,
	}}, nil
}
