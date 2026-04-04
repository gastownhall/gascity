package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

var errWorkflowNotFound = errors.New("workflow not found")

type workflowSnapshotResponse struct {
	WorkflowID        string                 `json:"workflow_id"`
	RootBeadID        string                 `json:"root_bead_id"`
	RootStoreRef      string                 `json:"root_store_ref"`
	ScopeKind         string                 `json:"scope_kind"`
	ScopeRef          string                 `json:"scope_ref"`
	Beads             []workflowBeadResponse `json:"beads"`
	Deps              []workflowDepResponse  `json:"deps"`
	LogicalNodes      []logicalNodeResponse  `json:"logical_nodes"`
	LogicalEdges      []workflowDepResponse  `json:"logical_edges"`
	ScopeGroups       []scopeGroupResponse   `json:"scope_groups"`
	Partial           bool                   `json:"partial"`
	ResolvedRootStore string                 `json:"resolved_root_store"`
	StoresScanned     []string               `json:"stores_scanned"`
	SnapshotVersion   uint64                 `json:"snapshot_version"`
	SnapshotEventSeq  *uint64                `json:"snapshot_event_seq,omitempty"`
}

type workflowBeadResponse struct {
	ID            string            `json:"id"`
	Title         string            `json:"title"`
	Status        string            `json:"status"`
	Kind          string            `json:"kind"`
	StepRef       string            `json:"step_ref,omitempty"`
	Attempt       *int              `json:"attempt,omitempty"`
	LogicalBeadID string            `json:"logical_bead_id,omitempty"`
	ScopeRef      string            `json:"scope_ref,omitempty"`
	Assignee      string            `json:"assignee,omitempty"`
	Metadata      map[string]string `json:"metadata"`
}

type workflowDepResponse struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind,omitempty"`
}

// Presentation types (logical_nodes, logical_edges, scope_groups) are
// computed by the MC server's workflow_presentation module. GC exports
// empty arrays — these types exist only for JSON serialization.
type (
	logicalNodeResponse = json.RawMessage
	scopeGroupResponse  = json.RawMessage
)

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

func (s *Server) handleWorkflowGet(w http.ResponseWriter, r *http.Request) {
	workflowID := strings.TrimSpace(r.PathValue("workflow_id"))
	if workflowID == "" {
		workflowID = strings.TrimSpace(r.PathValue("id"))
	}
	if workflowID == "" {
		writeError(w, http.StatusBadRequest, "invalid", "convoy id is required")
		return
	}

	q := r.URL.Query()
	scopeKind, scopeRef, scopeErr := parseOptionalWorkflowRequestScope(q.Get("scope_kind"), q.Get("scope_ref"))
	if scopeErr != "" {
		writeError(w, http.StatusBadRequest, "invalid", scopeErr)
		return
	}
	index := s.latestIndex()

	snapshot, err := s.buildWorkflowSnapshot(workflowID, scopeKind, scopeRef, index)
	if err != nil {
		if errors.Is(err, errWorkflowNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "workflow "+workflowID+" not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "workflow snapshot failed")
		return
	}

	writeIndexJSON(w, index, snapshot)
}

func (s *Server) handleWorkflowDelete(w http.ResponseWriter, r *http.Request) {
	workflowID := strings.TrimSpace(r.PathValue("workflow_id"))
	if workflowID == "" {
		workflowID = strings.TrimSpace(r.PathValue("id"))
	}
	if workflowID == "" {
		writeError(w, http.StatusBadRequest, "invalid", "convoy id is required")
		return
	}

	q := r.URL.Query()
	scopeKind := strings.TrimSpace(q.Get("scope_kind"))
	scopeRef := strings.TrimSpace(q.Get("scope_ref"))
	deleteFromStore := q.Get("delete") == "true"

	stores := s.workflowStores()

	closed := 0
	deleted := 0
	found := false

	for _, info := range stores {
		if info.store == nil {
			continue
		}
		// Skip stores that don't match the requested scope.
		if scopeKind != "" && info.scopeKind != scopeKind {
			continue
		}
		if scopeRef != "" && info.scopeRef != scopeRef {
			continue
		}

		all, err := info.store.ListOpen()
		if err != nil {
			continue
		}

		var ids []string
		for _, b := range all {
			if b.ID == workflowID || b.Metadata["gc.root_bead_id"] == workflowID {
				ids = append(ids, b.ID)
			}
		}
		if len(ids) == 0 {
			continue
		}
		found = true

		// Phase 1: Batch close all open beads.
		n, _ := info.store.CloseAll(ids, map[string]string{"gc.outcome": "skipped"})
		closed += n

		// Phase 2: Delete if requested.
		if deleteFromStore {
			for _, id := range ids {
				// Remove deps before delete.
				if deps, err := info.store.DepList(id, "down"); err == nil {
					for _, dep := range deps {
						_ = info.store.DepRemove(id, dep.DependsOnID)
					}
				}
				if deps, err := info.store.DepList(id, "up"); err == nil {
					for _, dep := range deps {
						_ = info.store.DepRemove(dep.IssueID, id)
					}
				}
				deleted++
			}
		}
	}

	if !found {
		writeError(w, http.StatusNotFound, "not_found", "workflow "+workflowID+" not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"workflow_id": workflowID,
		"closed":      closed,
		"deleted":     deleted,
	})
}

func (s *Server) buildWorkflowSnapshot(workflowID, fallbackScopeKind, fallbackScopeRef string, snapshotIndex uint64) (*workflowSnapshotResponse, error) {
	// Fast path: resolve the correct store and fetch the entire snapshot via SQL.
	if snap, err := s.tryFullWorkflowSQL(workflowID, fallbackScopeKind, fallbackScopeRef, snapshotIndex); err == nil {
		return snap, nil
	}

	// Slow path: bd subprocess N+1
	stores := s.workflowStores()
	storesScanned := make([]string, 0, len(stores))
	seenStoreRefs := make(map[string]bool, len(stores))
	matches := make([]workflowRootMatch, 0)
	listPartial := false
	var firstListErr error
	cityScopeRef := ""

	for _, info := range stores {
		if info.store == nil {
			continue
		}
		if cityScopeRef == "" && info.scopeKind == "city" {
			cityScopeRef = info.scopeRef
		}
		if !seenStoreRefs[info.ref] {
			storesScanned = append(storesScanned, info.ref)
			seenStoreRefs[info.ref] = true
		}

		all, err := info.store.ListOpen()
		if err != nil {
			if firstListErr == nil {
				firstListErr = err
			}
			listPartial = true
			continue
		}
		for _, bead := range all {
			if !isWorkflowRoot(bead) || !matchesWorkflowID(bead, workflowID) {
				continue
			}
			matches = append(matches, workflowRootMatch{info: info, root: bead})
		}
	}

	match, ok := selectWorkflowRootMatch(matches, fallbackScopeKind, fallbackScopeRef, cityScopeRef)
	if !ok {
		if firstListErr != nil {
			return nil, firstListErr
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
		all, err := info.store.ListOpen()
		if err != nil {
			return nil, err
		}

		workflowBeads = make([]beads.Bead, 0, len(all))
		for _, bead := range all {
			if bead.ID == root.ID || bead.Metadata["gc.root_bead_id"] == root.ID {
				workflowBeads = append(workflowBeads, bead)
			}
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
		beadResponses = append(beadResponses, workflowBeadResponse{
			ID:            bead.ID,
			Title:         bead.Title,
			Status:        workflowStatus(bead),
			Kind:          workflowKind(bead),
			StepRef:       strings.TrimSpace(bead.Metadata["gc.step_ref"]),
			Attempt:       workflowAttempt(bead),
			LogicalBeadID: strings.TrimSpace(bead.Metadata["gc.logical_bead_id"]),
			ScopeRef:      strings.TrimSpace(bead.Metadata["gc.scope_ref"]),
			Assignee:      strings.TrimSpace(bead.Assignee),
			Metadata:      cloneStringMap(bead.Metadata),
		})
	}

	snapshot := &workflowSnapshotResponse{
		WorkflowID:        resolvedWorkflowID(root),
		RootBeadID:        root.ID,
		RootStoreRef:      info.ref,
		ScopeKind:         scopeKind,
		ScopeRef:          scopeRef,
		Beads:             beadResponses,
		Deps:              workflowDeps,
		LogicalNodes:      []logicalNodeResponse{},
		LogicalEdges:      []workflowDepResponse{},
		ScopeGroups:       []scopeGroupResponse{},
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
	return strings.TrimSpace(bead.Metadata["gc.kind"]) == "workflow"
}

// isGraphConvoyBead reports whether a bead is a formula-compiled graph
// convoy (as opposed to a simple parent-child convoy).
func isGraphConvoyBead(b beads.Bead) bool {
	return isWorkflowRoot(b)
}

func resolvedWorkflowID(root beads.Bead) string {
	if workflowID := strings.TrimSpace(root.Metadata["gc.workflow_id"]); workflowID != "" {
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
	if info.scopeKind == "rig" {
		return "city", workflowCityScopeRef(cityScopeRef)
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
	case "city", "rig":
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
	scopeKind := strings.TrimSpace(root.Metadata["gc.scope_kind"])
	scopeRef := strings.TrimSpace(root.Metadata["gc.scope_ref"])
	if scopeKind == "" || scopeRef == "" {
		return "", ""
	}
	return scopeKind, scopeRef
}

// collectWorkflowDeps returns the physical bead-to-bead dependencies.
// Logical edge computation is handled by the MC server's presentation layer.
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
	raw := strings.TrimSpace(bead.Metadata["gc.attempt"])
	if raw == "" {
		return 0
	}
	v, _ := strconv.Atoi(raw)
	return v
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
	if bead.Metadata != nil {
		if kind := strings.TrimSpace(bead.Metadata["gc.kind"]); kind != "" {
			return kind
		}
	}
	return strings.TrimSpace(bead.Type)
}

func workflowStatus(bead beads.Bead) string {
	outcome := strings.TrimSpace(bead.Metadata["gc.outcome"])
	hasAssignment := strings.TrimSpace(bead.Assignee) != ""
	switch strings.TrimSpace(bead.Status) {
	case "closed":
		switch outcome {
		case "fail":
			return "failed"
		case "skipped":
			return "skipped"
		}
		return "completed"
	case "in_progress":
		if hasAssignment {
			return "active"
		}
		return "pending"
	case "open":
		return "pending"
	default:
		switch outcome {
		case "fail":
			return "failed"
		case "skipped":
			return "skipped"
		}
		return strings.TrimSpace(bead.Status)
	}
}

func workflowStores(state State) []workflowStoreInfo {
	beadStores := state.BeadStores()
	stores := make([]workflowStoreInfo, 0, len(beadStores)+1)
	cityName := workflowCityScopeRef(state.CityName())

	if cityStore := state.CityBeadStore(); cityStore != nil {
		stores = append(stores, workflowStoreInfo{
			ref:       "city:" + cityName,
			scopeKind: "city",
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
			scopeKind: "rig",
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
	case "city":
		cityStore := state.CityBeadStore()
		cityName := workflowCityScopeRef(state.CityName())
		if cityStore == nil || scopeRef != cityName {
			return workflowStoreInfo{}, false
		}
		return workflowStoreInfo{
			ref:       "city:" + cityName,
			scopeKind: "city",
			scopeRef:  cityName,
			store:     cityStore,
		}, true
	case "rig":
		store := state.BeadStore(scopeRef)
		if store == nil {
			return workflowStoreInfo{}, false
		}
		return workflowStoreInfo{
			ref:       "rig:" + scopeRef,
			scopeKind: "rig",
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
