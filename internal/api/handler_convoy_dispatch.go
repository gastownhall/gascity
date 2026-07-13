package api

import (
	"errors"
	"log"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/molecule"
)

var errWorkflowNotFound = errors.New("workflow not found")

// logWorkflowSQLFallback surfaces a workflow-snapshot SQL fast-path failure
// before buildWorkflowSnapshot drops to the per-store scan. The SQL path is the
// ~190ms route; the scan is seconds and grows with rig count, so a silent
// fallback hides a real perf regression (gascity#2940). The benign
// "no SQL stores configured" outcome is left quiet so non-SQL deployments,
// whose steady state IS the scan, do not log on every workflow fetch.
func logWorkflowSQLFallback(workflowID string, err error) {
	if err == nil || errors.Is(err, errNoSQLWorkflowStores) {
		return
	}
	log.Printf("gc api: workflow %q SQL fast path failed, falling back to store scan: %v", workflowID, err)
}

// Response types (workflowSnapshotResponse, workflowBeadResponse,
// workflowDepResponse, LogicalNode, ScopeGroup) live in
// huma_types_convoys.go so every response-body struct has one
// canonical home. This file contains only the dispatch helpers that
// populate them from the bead store.

// workflowGraphStoreRefPrefix tags the dedicated graph store's workflowStoreInfo
// ref so the store-scan dedup keeps it distinct from the city store when graph is
// relocated. It is not a scope kind (graph roots carry city/rig scope metadata of
// their own) — only a store-identity tag for the snapshot scan and ref round-trip.
const workflowGraphStoreRefPrefix = "graph"

type workflowStoreInfo struct {
	ref       string
	scopeKind string
	scopeRef  string
	store     beads.Store
}

type workflowRootMatch struct {
	info workflowStoreInfo
	root beads.Bead
}

// workflowMatchKey dedups matches by (store, physical bead id) so one physical
// root is never counted twice — which would defeat selectWorkflowRootMatch's
// len(matches)==1 cross-store-uniqueness gate (gascity#2940).
type workflowMatchKey struct {
	ref string
	id  string
}

func (s *Server) buildWorkflowSnapshot(workflowID, fallbackScopeKind, fallbackScopeRef string, snapshotIndex uint64) (*workflowSnapshotResponse, error) {
	// Fast path: resolve the correct store and fetch the entire snapshot via SQL.
	snap, sqlErr := s.tryFullWorkflowSQL(workflowID, fallbackScopeKind, fallbackScopeRef, snapshotIndex)
	if sqlErr == nil {
		return snap, nil
	}
	logWorkflowSQLFallback(workflowID, sqlErr)

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
	stores := s.workflowStores()
	storesScanned := make([]string, 0, len(stores))
	seenStoreRefs := make(map[string]bool, len(stores))
	matches := make([]workflowRootMatch, 0)
	seenMatches := make(map[workflowMatchKey]bool)
	addMatch := func(info workflowStoreInfo, root beads.Bead) {
		key := workflowMatchKey{ref: info.ref, id: root.ID}
		if seenMatches[key] {
			return
		}
		seenMatches[key] = true
		matches = append(matches, workflowRootMatch{info: info, root: root})
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
		root, err := info.store.Get(workflowID)
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
			roots, err := info.store.List(beads.ListQuery{
				Metadata: map[string]string{
					beadmeta.KindMetadataKey:       beadmeta.KindWorkflow,
					beadmeta.WorkflowIDMetadataKey: workflowID,
				},
				IncludeClosed: true,
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

	return s.snapshotFromStore(match.info, match.root, fallbackScopeKind, fallbackScopeRef, cityScopeRef, storesScanned, listPartial, snapshotIndex)
}

func (s *Server) snapshotFromStore(info workflowStoreInfo, root beads.Bead, fallbackScopeKind, fallbackScopeRef, cityScopeRef string, storesScanned []string, listPartial bool, snapshotIndex uint64) (*workflowSnapshotResponse, error) {
	// Try direct SQL path — ~500x faster than N+1 bd subprocess calls.
	var (
		workflowBeads []beads.Bead
		beadIndex     map[string]beads.Bead
		depMap        map[string][]beads.Dep
		sqlErr        error
	)
	workflowBeads, beadIndex, depMap, sqlErr = s.tryWorkflowSQL(info, root.ID)
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
		store = &prefetchedDepStore{deps: depMap}
	} else {
		if prefetchedDeps, ok := prefetchedDepsForWorkflowBeads(workflowBeads); ok {
			store = &prefetchedDepStore{deps: prefetchedDeps}
		} else {
			store = info.store
		}
	}

	workflowDeps, partial := collectWorkflowDeps(store, beadIndex)
	partial = partial || listPartial
	scopeKind, scopeRef := workflowSnapshotScope(info, root, fallbackScopeKind, fallbackScopeRef, cityScopeRef)

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

func isWorkflowRoot(bead beads.Bead) bool {
	return strings.TrimSpace(bead.Metadata[beadmeta.KindMetadataKey]) == beadmeta.KindWorkflow
}

// isGraphConvoyBead reports whether a bead is a formula-compiled graph
// convoy (as opposed to a simple parent-child convoy).
func isGraphConvoyBead(b beads.Bead) bool {
	return isWorkflowRoot(b)
}

func resolvedWorkflowID(root beads.Bead) string {
	if workflowID := strings.TrimSpace(root.Metadata[beadmeta.WorkflowIDMetadataKey]); workflowID != "" {
		return workflowID
	}
	return root.ID
}

func matchesWorkflowID(root beads.Bead, workflowID string) bool {
	workflowID = strings.TrimSpace(workflowID)
	if workflowID == "" {
		return false
	}
	return root.ID == workflowID || resolvedWorkflowID(root) == workflowID
}

func selectWorkflowRootMatch(matches []workflowRootMatch, requestedScopeKind, requestedScopeRef, cityScopeRef string) (workflowRootMatch, bool) {
	if len(matches) == 0 {
		return workflowRootMatch{}, false
	}
	if requestedScopeKind == "" || requestedScopeRef == "" {
		return matches[0], true
	}

	filtered := make([]workflowRootMatch, 0, len(matches))
	for _, match := range matches {
		if workflowScopeMatches(match.info, match.root, requestedScopeKind, requestedScopeRef) {
			filtered = append(filtered, match)
		}
	}
	switch len(filtered) {
	case 0:
		// Older workflows may not stamp logical scope on the root, and city-
		// scoped workflows can still live in a rig store. Preserve the caller
		// scope only for that legacy city-on-rig case when the workflow ID is
		// unique across scanned stores.
		if len(matches) == 1 && preserveRequestedWorkflowScope(matches[0].info, matches[0].root, requestedScopeKind, requestedScopeRef, cityScopeRef) {
			return matches[0], true
		}
		return workflowRootMatch{}, false
	default:
		return filtered[0], true
	}
}

func workflowScopeMatches(info workflowStoreInfo, root beads.Bead, requestedScopeKind, requestedScopeRef string) bool {
	scopeKind, scopeRef := workflowSelectionScope(info, root)
	return scopeKind == requestedScopeKind && scopeRef == requestedScopeRef
}

func workflowSelectionScope(info workflowStoreInfo, root beads.Bead) (string, string) {
	if scopeKind, scopeRef := workflowRootScope(root); scopeKind != "" && scopeRef != "" {
		return scopeKind, scopeRef
	}
	return info.scopeKind, info.scopeRef
}

func workflowEventScope(info workflowStoreInfo, root beads.Bead, cityScopeRef string) (string, string) {
	if scopeKind, scopeRef := workflowRootScope(root); scopeKind != "" && scopeRef != "" {
		return scopeKind, scopeRef
	}
	// Event projections favor the logical city scope for legacy rig-stored
	// workflows whose roots predate explicit scope stamping. That keeps live
	// event scopes aligned with the snapshot API's preserved city-scope reads
	// for those legacy workflows, while root_store_ref still exposes the
	// physical store for callers that need it.
	if info.scopeKind == beadmeta.ScopeKindRig {
		return beadmeta.ScopeKindCity, workflowCityScopeRef(cityScopeRef)
	}
	return info.scopeKind, info.scopeRef
}

func workflowSnapshotScope(info workflowStoreInfo, root beads.Bead, requestedScopeKind, requestedScopeRef, cityScopeRef string) (string, string) {
	if scopeKind, scopeRef := workflowRootScope(root); scopeKind != "" && scopeRef != "" {
		return scopeKind, scopeRef
	}
	if preserveRequestedWorkflowScope(info, root, requestedScopeKind, requestedScopeRef, cityScopeRef) {
		return requestedScopeKind, requestedScopeRef
	}
	return info.scopeKind, info.scopeRef
}

func preserveRequestedWorkflowScope(info workflowStoreInfo, root beads.Bead, requestedScopeKind, requestedScopeRef, cityScopeRef string) bool {
	if requestedScopeKind != "city" || requestedScopeRef == "" {
		return false
	}
	if info.scopeKind != "rig" {
		return false
	}
	if strings.TrimSpace(cityScopeRef) == "" || requestedScopeRef != strings.TrimSpace(cityScopeRef) {
		return false
	}
	scopeKind, scopeRef := workflowRootScope(root)
	return scopeKind == "" || scopeRef == ""
}

func parseWorkflowRequestScope(rawScopeKind, rawScopeRef string) (string, string, string) {
	scopeKind := strings.TrimSpace(rawScopeKind)
	scopeRef := strings.TrimSpace(rawScopeRef)
	if scopeKind == "" && scopeRef == "" {
		return "", "", "scope_kind and scope_ref are required"
	}
	if scopeKind == "" || scopeRef == "" {
		return "", "", "scope_kind and scope_ref must be provided together"
	}
	switch scopeKind {
	case beadmeta.ScopeKindCity, beadmeta.ScopeKindRig:
		return scopeKind, scopeRef, ""
	default:
		return "", "", "scope_kind must be 'city' or 'rig'"
	}
}

func parseOptionalWorkflowRequestScope(rawScopeKind, rawScopeRef string) (string, string, string) {
	scopeKind := strings.TrimSpace(rawScopeKind)
	scopeRef := strings.TrimSpace(rawScopeRef)
	if scopeKind == "" && scopeRef == "" {
		return "", "", ""
	}
	return parseWorkflowRequestScope(scopeKind, scopeRef)
}

func workflowRootScope(root beads.Bead) (string, string) {
	scopeKind := strings.TrimSpace(root.Metadata[beadmeta.ScopeKindMetadataKey])
	scopeRef := strings.TrimSpace(root.Metadata[beadmeta.ScopeRefMetadataKey])
	if scopeKind == "" || scopeRef == "" {
		return "", ""
	}
	return scopeKind, scopeRef
}

// collectWorkflowDeps returns the physical bead-to-bead dependencies.
// Logical edge computation is handled by the real-world app server's presentation layer.
func collectWorkflowDeps(store beads.Store, beadIndex map[string]beads.Bead) ([]workflowDepResponse, bool) {
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

func prefetchedDepsForWorkflowBeads(workflowBeads []beads.Bead) (map[string][]beads.Dep, bool) {
	depMap := make(map[string][]beads.Dep)
	hasPrefetchedDeps := false

	for _, bead := range workflowBeads {
		if bead.Dependencies == nil {
			continue
		}
		hasPrefetchedDeps = true
		if len(bead.Dependencies) == 0 {
			depMap[bead.ID] = nil
			continue
		}
		depMap[bead.ID] = append([]beads.Dep(nil), bead.Dependencies...)
	}

	return depMap, hasPrefetchedDeps
}

// findCanonicalControl finds the earliest retry/ralph control bead that
// shares the same gc.step_id as the given control bead. This collapses
// controls across ralph iterations (e.g., iteration.1.review-own-code and
// iteration.2.review-own-code) into a single logical node. Returns "" if
// this bead is already the canonical one or no match is found.

func workflowAttempt(bead beads.Bead) *int {
	if attempt := workflowAttemptValue(bead); attempt > 0 {
		return &attempt
	}
	return nil
}

func workflowAttemptValue(bead beads.Bead) int {
	return molecule.WorkflowAttempt(bead)
}

func isTerminalWorkflowStatus(status string) bool {
	switch status {
	case "completed", "skipped", "failed":
		return true
	}
	return false
}

func statusRank(status string) int {
	switch status {
	case "active":
		return 5
	case "pending":
		return 4
	case "failed":
		return 3
	case "skipped":
		return 2
	case "completed":
		return 1
	}
	return 0
}

func containsString(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

func metadataInt(meta map[string]string, key string) int {
	if meta == nil {
		return 0
	}
	value := strings.TrimSpace(meta[key])
	if value == "" {
		return 0
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return parsed
}

func cloneStringMap(src map[string]string) map[string]string {
	if src == nil {
		return nil
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func workflowKind(bead beads.Bead) string {
	return molecule.WorkflowKind(bead)
}

func workflowStatus(bead beads.Bead) string {
	return molecule.WorkflowStatus(bead)
}

// workflowBeadResponseFromBead maps a workflow bead onto its snapshot response
// node through the molecule.WorkflowBead codec — the single mapping shared by the
// snapshot, SQL-fast-path, and event-projection build loops. The codec already
// clones the metadata map (preserving nil -> nil for the wire's "metadata":
// null), so cloneStringMap is not called here.
func workflowBeadResponseFromBead(bead beads.Bead) workflowBeadResponse {
	wb := molecule.WorkflowBeadFromBead(bead)
	var attempt *int
	if wb.Attempt > 0 {
		attempt = &wb.Attempt
	}
	return workflowBeadResponse{
		ID:            wb.ID,
		Title:         wb.Title,
		Status:        wb.Status,
		Kind:          wb.Kind,
		StepRef:       wb.StepRef,
		Attempt:       attempt,
		LogicalBeadID: wb.LogicalBeadID,
		ScopeRef:      wb.ScopeRef,
		Assignee:      wb.Assignee,
		Metadata:      wb.Metadata,
	}
}

func workflowStores(state State) []workflowStoreInfo {
	beadStores := state.BeadStores()
	stores := make([]workflowStoreInfo, 0, len(beadStores)+2)
	cityName := workflowCityScopeRef(state.CityName())
	cityStore := state.CityBeadStore()

	// Graph-first: when [beads.classes.graph] relocates graph beads to a dedicated
	// store, workflow roots/steps/wisps live there, NOT on the work store, so the
	// snapshot scan must consult it. Lead with it (graph-first) so a graph-resident
	// root is found on the first probe. Skip it on a default (non-relocated) city —
	// where GraphBeadStore() == CityBeadStore() — so the scan stays byte-identical
	// there. It carries the city scope as its fallback (graph roots stamp their own
	// scope metadata, which workflowSnapshotScope prefers) but a distinct ref so the
	// store-scan dedup never conflates it with the city store.
	if graphStore := state.GraphBeadStore().Store; graphStore != nil && graphStore != cityStore {
		stores = append(stores, workflowStoreInfo{
			ref:       workflowGraphStoreRefPrefix + ":" + cityName,
			scopeKind: beadmeta.ScopeKindCity,
			scopeRef:  cityName,
			store:     graphStore,
		})
	}

	if cityStore != nil {
		stores = append(stores, workflowStoreInfo{
			ref:       "city:" + cityName,
			scopeKind: beadmeta.ScopeKindCity,
			scopeRef:  cityName,
			store:     cityStore,
		})
	}

	for _, rigName := range sortedRigNames(beadStores) {
		if rigName == cityName {
			continue
		}
		store := state.BeadStore(rigName)
		if store == nil {
			continue
		}
		stores = append(stores, workflowStoreInfo{
			ref:       "rig:" + rigName,
			scopeKind: beadmeta.ScopeKindRig,
			scopeRef:  rigName,
			store:     store,
		})
	}

	return stores
}

func workflowStoreByRef(state State, ref string) (workflowStoreInfo, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return workflowStoreInfo{}, false
	}

	kind, scopeRef, ok := strings.Cut(ref, ":")
	if !ok {
		return workflowStoreInfo{}, false
	}
	scopeRef = strings.TrimSpace(scopeRef)
	if scopeRef == "" {
		return workflowStoreInfo{}, false
	}

	switch strings.TrimSpace(kind) {
	case beadmeta.ScopeKindCity:
		cityStore := state.CityBeadStore()
		cityName := workflowCityScopeRef(state.CityName())
		if cityStore == nil || scopeRef != cityName {
			return workflowStoreInfo{}, false
		}
		return workflowStoreInfo{
			ref:       "city:" + cityName,
			scopeKind: beadmeta.ScopeKindCity,
			scopeRef:  cityName,
			store:     cityStore,
		}, true
	case workflowGraphStoreRefPrefix:
		// Round-trip the graph-store ref minted by workflowStores when graph is
		// relocated. On a default city (graph == city) there is no graph entry, so
		// this resolves nothing and callers fall back to the store scan.
		graphStore := state.GraphBeadStore().Store
		cityName := workflowCityScopeRef(state.CityName())
		if graphStore == nil || graphStore == state.CityBeadStore() || scopeRef != cityName {
			return workflowStoreInfo{}, false
		}
		return workflowStoreInfo{
			ref:       workflowGraphStoreRefPrefix + ":" + cityName,
			scopeKind: beadmeta.ScopeKindCity,
			scopeRef:  cityName,
			store:     graphStore,
		}, true
	case beadmeta.ScopeKindRig:
		store := state.BeadStore(scopeRef)
		if store == nil {
			return workflowStoreInfo{}, false
		}
		return workflowStoreInfo{
			ref:       "rig:" + scopeRef,
			scopeKind: beadmeta.ScopeKindRig,
			scopeRef:  scopeRef,
			store:     store,
		}, true
	}
	return workflowStoreInfo{}, false
}

func (s *Server) workflowStores() []workflowStoreInfo {
	return workflowStores(s.state)
}

func workflowCityScopeRef(cityName string) string {
	cityName = strings.TrimSpace(cityName)
	if cityName == "" {
		return "city"
	}
	return cityName
}
