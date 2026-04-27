package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

func appendMetadataAttachedChildren(store beads.Store, parent beads.Bead, children []beads.Bead) []beads.Bead {
	if store == nil {
		return children
	}
	seen := make(map[string]struct{}, len(children))
	for _, child := range children {
		seen[child.ID] = struct{}{}
	}
	for _, key := range []string{"molecule_id", "workflow_id"} {
		attachedID := strings.TrimSpace(parent.Metadata[key])
		if attachedID == "" {
			continue
		}
		if _, ok := seen[attachedID]; ok {
			continue
		}
		attached, err := store.Get(attachedID)
		if err != nil {
			continue
		}
		seen[attached.ID] = struct{}{}
		children = append(children, attached)
	}
	return children
}

func (s *Server) beadListAssigneeTerms(ctx context.Context, assignee string) []string {
	assignee = strings.TrimSpace(assignee)
	if assignee == "" {
		return []string{""}
	}
	terms := []string{assignee}
	store := s.state.CityBeadStore()
	if store == nil {
		return terms
	}
	id, err := s.resolveSessionTargetIDWithContext(ctx, store, assignee, apiSessionResolveOptions{})
	if err != nil || id == "" || id == assignee {
		return terms
	}
	return []string{id, assignee}
}

func (s *Server) normalizeRawBeadAssignee(ctx context.Context, assignee string) (string, error) {
	assignee = strings.TrimSpace(assignee)
	if assignee == "" {
		return "", nil
	}
	store := s.state.CityBeadStore()
	if store == nil {
		return assignee, nil
	}
	id, err := s.resolveSessionTargetIDWithContext(ctx, store, assignee, apiSessionResolveOptions{})
	if errors.Is(err, session.ErrSessionNotFound) {
		id, err = s.resolveSessionTargetIDWithContext(ctx, store, assignee, apiSessionResolveOptions{materialize: true})
	}
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return "", fmt.Errorf("assignee must resolve to a concrete open session bead ID: %q", assignee)
		}
		return "", fmt.Errorf("resolving assignee %q: %w", assignee, err)
	}
	b, err := store.Get(id)
	if err != nil {
		return "", fmt.Errorf("looking up resolved assignee session %q: %w", id, err)
	}
	if !session.IsSessionBeadOrRepairable(b) || b.Status == "closed" {
		return "", fmt.Errorf("assignee must resolve to a concrete open session bead ID: %q", assignee)
	}
	session.RepairEmptyType(store, &b)
	return b.ID, nil
}

// findStore returns the bead store for the given rig. If rig is empty, returns
// the sole store when exactly one exists (after deduplication), or nil when
// multiple distinct stores exist (caller should require explicit rig).
func (s *Server) findStore(rig string) beads.Store {
	if rig != "" {
		return s.state.BeadStore(rig)
	}
	if cityStore := s.state.CityBeadStore(); cityStore != nil {
		return cityStore
	}
	stores := s.state.BeadStores()
	names := sortedRigNames(stores)
	if len(names) == 1 {
		return stores[names[0]]
	}
	return nil
}

// beadStoresForID resolves the authoritative store for a bead ID using its
// prefix/routes mapping when possible. If there is no routed match, it falls
// back to the legacy store scan order.
func (s *Server) beadStoresForID(id string) []beads.Store {
	id = strings.TrimSpace(id)
	if store := s.resolveStoreByConfiguredIDPrefix(id); store != nil {
		return []beads.Store{store}
	}
	if prefix := beadPrefix(id); prefix != "" {
		if store := s.resolveStoreByPrefix(prefix); store != nil {
			return []beads.Store{store}
		}
	}

	stores := s.state.BeadStores()
	rigNames := sortedRigNames(stores)
	candidates := make([]beads.Store, 0, len(rigNames)+1)
	if cityStore := s.state.CityBeadStore(); cityStore != nil {
		candidates = append(candidates, cityStore)
	}
	for _, rigName := range rigNames {
		candidates = append(candidates, stores[rigName])
	}
	return candidates
}

func (s *Server) resolveStoreByConfiguredIDPrefix(id string) beads.Store {
	if id == "" {
		return nil
	}
	cfg := s.state.Config()
	if cfg == nil {
		return nil
	}

	var bestStore beads.Store
	bestLen := -1
	if prefix := strings.TrimSpace(config.EffectiveHQPrefix(cfg)); beadIDHasConfiguredPrefix(id, prefix) {
		if cityStore := s.state.CityBeadStore(); cityStore != nil {
			bestStore = cityStore
			bestLen = len(prefix)
		}
	}
	for _, rig := range cfg.Rigs {
		prefix := strings.TrimSpace(rig.EffectivePrefix())
		if !beadIDHasConfiguredPrefix(id, prefix) || len(prefix) <= bestLen {
			continue
		}
		store := s.state.BeadStore(rig.Name)
		if store == nil {
			continue
		}
		bestStore = store
		bestLen = len(prefix)
	}
	return bestStore
}

func beadIDHasConfiguredPrefix(id, prefix string) bool {
	if prefix == "" {
		return false
	}
	return id == prefix || strings.HasPrefix(id, prefix+"-")
}

// resolveStoreByPrefix finds the store that owns a bead prefix by checking
// routes.jsonl files in the city and each rig's .beads/ directory, then
// mapping the resolved store path back to the correct store.
func (s *Server) resolveStoreByPrefix(prefix string) beads.Store {
	cfg := s.state.Config()
	if cfg == nil {
		return nil
	}
	stores := s.state.BeadStores()
	cityPath := strings.TrimSpace(s.state.CityPath())

	// Build rig path → name map for reverse lookup (used by both city
	// and rig route resolution below).
	rigPathToName := make(map[string]string, len(cfg.Rigs))
	for _, rig := range cfg.Rigs {
		rp := strings.TrimSpace(rig.Path)
		if rp == "" {
			continue
		}
		if !filepath.IsAbs(rp) && cityPath != "" {
			rp = filepath.Join(cityPath, rp)
		}
		rigPathToName[filepath.Clean(rp)] = rig.Name
	}

	// Check city-level routes first.
	if cityPath != "" {
		if storePath, ok := resolveRoutePrefix(cityPath, prefix); ok {
			cleanPath := filepath.Clean(storePath)
			// Route may point to a rig directory — resolve to the rig store.
			if rigName, found := rigPathToName[cleanPath]; found {
				if store, exists := stores[rigName]; exists {
					return store
				}
			}
			// Route points to the city itself (e.g. prefix "mc" → ".").
			if cleanPath == filepath.Clean(cityPath) {
				if cityStore := s.state.CityBeadStore(); cityStore != nil {
					return cityStore
				}
			}
		}
	}

	// Search routes.jsonl in each rig's .beads/ directory.
	for _, rig := range cfg.Rigs {
		rigPath := strings.TrimSpace(rig.Path)
		if rigPath == "" {
			continue
		}
		if !filepath.IsAbs(rigPath) && cityPath != "" {
			rigPath = filepath.Join(cityPath, rigPath)
		}
		storePath, ok := resolveRoutePrefix(rigPath, prefix)
		if !ok {
			continue
		}
		// The resolved store path might point to a different rig
		// (e.g., prefix "gb" in alpha's routes maps to ../beta).
		cleanPath := filepath.Clean(storePath)
		if rigName, found := rigPathToName[cleanPath]; found {
			if store, exists := stores[rigName]; exists {
				return store
			}
		}
		// Fallback: the route pointed to the same rig.
		if store, exists := stores[rig.Name]; exists {
			return store
		}
	}
	return nil
}

// sortedRigNames returns rig names from the store map in deterministic sorted order,
// deduplicating rigs that share the same underlying store (e.g. file provider mode).
func sortedRigNames(stores map[string]beads.Store) []string {
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

// BeadGraphResponse is the response shape for GET /v0/beads/graph/{rootID}.
// Returns raw beads and deps — no status mapping, no presentation logic.
type BeadGraphResponse struct {
	Root  beads.Bead            `json:"root"`
	Beads []beads.Bead          `json:"beads"`
	Deps  []workflowDepResponse `json:"deps"`
}

func collectBeadGraph(store beads.Store, root beads.Bead) ([]beads.Bead, []workflowDepResponse, error) {
	graphBeads := make([]beads.Bead, 0, 1)
	beadIndex := make(map[string]beads.Bead)

	upsert := func(b beads.Bead) {
		if b.ID == "" {
			return
		}
		if existing, ok := beadIndex[b.ID]; ok {
			if existing.ParentID == "" && b.ParentID != "" {
				existing.ParentID = b.ParentID
				beadIndex[b.ID] = existing
				for i := range graphBeads {
					if graphBeads[i].ID == b.ID {
						graphBeads[i].ParentID = b.ParentID
						break
					}
				}
			}
			return
		}
		beadIndex[b.ID] = b
		graphBeads = append(graphBeads, b)
	}
	upsert(root)

	metadataChildren, err := store.List(beads.ListQuery{
		Metadata:      map[string]string{"gc.root_bead_id": root.ID},
		IncludeClosed: true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("listing metadata children for bead %q: %w", root.ID, err)
	}
	for _, child := range metadataChildren {
		upsert(child)
	}

	parentEdges := make([]workflowDepResponse, 0)
	seenEdges := make(map[string]bool)
	addParentEdge := func(parentID, childID string) {
		if parentID == "" || childID == "" {
			return
		}
		edge := workflowDepResponse{From: parentID, To: childID, Kind: "parent-child"}
		key := edge.From + "|" + edge.To + "|" + edge.Kind
		if seenEdges[key] {
			return
		}
		seenEdges[key] = true
		parentEdges = append(parentEdges, edge)
	}

	for i := 0; i < len(graphBeads); i++ {
		parent := graphBeads[i]
		children, err := store.List(beads.ListQuery{
			ParentID:      parent.ID,
			IncludeClosed: true,
			Sort:          beads.SortCreatedAsc,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("listing child beads for bead %q: %w", parent.ID, err)
		}
		for _, child := range children {
			if child.ParentID == "" {
				child.ParentID = parent.ID
			}
			addParentEdge(parent.ID, child.ID)
			upsert(child)
		}
	}

	return graphBeads, parentEdges, nil
}

func mergeWorkflowDeps(primary, extra []workflowDepResponse) []workflowDepResponse {
	if len(extra) == 0 {
		return primary
	}
	seen := make(map[string]bool, len(primary)+len(extra))
	for _, edge := range primary {
		seen[edge.From+"|"+edge.To+"|"+edge.Kind] = true
	}
	for _, edge := range extra {
		key := edge.From + "|" + edge.To + "|" + edge.Kind
		if seen[key] {
			continue
		}
		primary = append(primary, edge)
		seen[key] = true
	}
	return primary
}

// beadPrefix extracts the alphabetic prefix from a bead ID (e.g., "ga" from "ga-5b8i").
func beadPrefix(id string) string {
	for i, c := range id {
		if c == '-' {
			return id[:i]
		}
		if c < 'a' || c > 'z' {
			return ""
		}
	}
	return ""
}

// resolveRoutePrefix reads routes.jsonl from a rig's .beads/ directory and
// resolves the given prefix to an absolute store path.
func resolveRoutePrefix(rigPath, prefix string) (string, bool) {
	routesPath := filepath.Join(rigPath, ".beads", "routes.jsonl")
	data, err := os.ReadFile(routesPath)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var entry struct {
			Prefix string `json:"prefix"`
			Path   string `json:"path"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry.Prefix == prefix {
			resolved := entry.Path
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(rigPath, resolved)
			}
			return resolved, true
		}
	}
	return "", false
}
