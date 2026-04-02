package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

func (s *Server) handleBeadList(w http.ResponseWriter, r *http.Request) {
	bp := parseBlockingParams(r)
	if bp.isBlocking() {
		waitForChange(r.Context(), s.state.EventProvider(), bp)
	}

	q := r.URL.Query()
	qStatus := q.Get("status")
	qType := q.Get("type")
	qLabel := q.Get("label")
	qAssignee := q.Get("assignee")
	qRig := q.Get("rig")
	pp := parsePagination(r, 50)

	stores := s.state.BeadStores()
	// When a specific rig is requested, query its store directly to avoid
	// dedup-related misses when multiple rigs share a store (file provider).
	var rigNames []string
	if qRig != "" {
		if _, ok := stores[qRig]; ok {
			rigNames = []string{qRig}
		}
	} else {
		rigNames = sortedRigNames(stores)
	}
	setDataSource(r, "bd_subprocess")
	var all []beads.Bead
	for _, rigName := range rigNames {
		store := stores[rigName]
		list, err := store.List()
		if err != nil {
			continue
		}
		for _, b := range list {
			if !matchBead(b, qStatus, qType, qLabel, qAssignee) {
				continue
			}
			all = append(all, b)
		}
	}

	if all == nil {
		all = []beads.Bead{}
	}
	if !pp.IsPaging {
		if pp.Limit < len(all) {
			all = all[:pp.Limit]
		}
		writeListJSON(w, s.latestIndex(), all, len(all))
		return
	}
	page, total, nextCursor := paginate(all, pp)
	if page == nil {
		page = []beads.Bead{}
	}
	writePagedJSON(w, s.latestIndex(), page, total, nextCursor)
}

func (s *Server) handleBeadReady(w http.ResponseWriter, r *http.Request) {
	bp := parseBlockingParams(r)
	if bp.isBlocking() {
		waitForChange(r.Context(), s.state.EventProvider(), bp)
	}

	stores := s.state.BeadStores()
	rigNames := sortedRigNames(stores)
	var all []beads.Bead
	for _, rigName := range rigNames {
		ready, err := stores[rigName].Ready()
		if err != nil {
			continue
		}
		all = append(all, ready...)
	}

	if all == nil {
		all = []beads.Bead{}
	}
	writeListJSON(w, s.latestIndex(), all, len(all))
}

func (s *Server) handleBeadGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, store := range s.beadStoresForID(id) {
		b, err := store.Get(id)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		writeIndexJSON(w, s.latestIndex(), b)
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "bead "+id+" not found")
}

func (s *Server) handleBeadDeps(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, store := range s.beadStoresForID(id) {
		children, err := store.Children(id)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if children == nil {
			children = []beads.Bead{}
		}
		writeIndexJSON(w, s.latestIndex(), map[string]any{"children": children})
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "bead "+id+" not found")
}

func (s *Server) handleBeadCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Rig         string   `json:"rig"`
		Title       string   `json:"title"`
		Type        string   `json:"type"`
		Priority    *int     `json:"priority"`
		Assignee    string   `json:"assignee"`
		Description string   `json:"description"`
		Labels      []string `json:"labels"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if body.Title == "" {
		writeError(w, http.StatusBadRequest, "invalid", "title is required")
		return
	}

	// Idempotency check — key is scoped by method+path to prevent cross-endpoint collisions.
	idemKey := scopedIdemKey(r, r.Header.Get("Idempotency-Key"))
	var bodyHash string
	if idemKey != "" {
		bodyHash = hashBody(body)
		if s.idem.handleIdempotent(w, idemKey, bodyHash) {
			return
		}
	}

	store := s.findStore(body.Rig)
	if store == nil {
		s.idem.unreserve(idemKey)
		writeError(w, http.StatusBadRequest, "invalid", "rig is required when multiple rigs are configured")
		return
	}

	b, err := store.Create(beads.Bead{
		Title:       body.Title,
		Type:        body.Type,
		Priority:    body.Priority,
		Assignee:    body.Assignee,
		Description: body.Description,
		Labels:      body.Labels,
	})
	if err != nil {
		s.idem.unreserve(idemKey)
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.idem.storeResponse(idemKey, bodyHash, http.StatusCreated, b)
	writeJSON(w, http.StatusCreated, b)
}

func (s *Server) handleBeadClose(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, store := range s.beadStoresForID(id) {
		if err := store.Close(id); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "closed"})
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "bead "+id+" not found")
}

func (s *Server) handleBeadUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	payload, err := decodeBodyBytes(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	var raw map[string]json.RawMessage
	if len(bytes.TrimSpace(payload)) > 0 {
		if err := json.Unmarshal(payload, &raw); err != nil {
			writeError(w, http.StatusBadRequest, "invalid", err.Error())
			return
		}
	}
	var body struct {
		Title        *string           `json:"title"`
		Status       *string           `json:"status"`
		Type         *string           `json:"type"`
		Priority     *int              `json:"priority"`
		Assignee     *string           `json:"assignee"`
		Description  *string           `json:"description"`
		Labels       []string          `json:"labels"`
		RemoveLabels []string          `json:"remove_labels"`
		Metadata     map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if rawPriority, ok := raw["priority"]; ok && bytes.Equal(bytes.TrimSpace(rawPriority), []byte("null")) {
		writeError(w, http.StatusBadRequest, "invalid", "clearing priority is not supported")
		return
	}

	opts := beads.UpdateOpts{
		Title:        body.Title,
		Status:       body.Status,
		Type:         body.Type,
		Priority:     body.Priority,
		Assignee:     body.Assignee,
		Description:  body.Description,
		Labels:       body.Labels,
		RemoveLabels: body.RemoveLabels,
	}

	for _, store := range s.beadStoresForID(id) {
		if err := store.Update(id, opts); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		// Apply metadata key-value pairs if provided.
		if len(body.Metadata) > 0 {
			if err := store.SetMetadataBatch(id, body.Metadata); err != nil {
				writeError(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "bead "+id+" not found")
}

func (s *Server) handleBeadReopen(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	status := "open"

	for _, store := range s.beadStoresForID(id) {
		b, err := store.Get(id)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if b.Status != "closed" {
			writeError(w, http.StatusConflict, "conflict", "bead "+id+" is not closed (status: "+b.Status+")")
			return
		}
		if err := store.Update(id, beads.UpdateOpts{Status: &status}); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "reopened"})
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "bead "+id+" not found")
}

func (s *Server) handleBeadAssign(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Assignee string `json:"assignee"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	for _, store := range s.beadStoresForID(id) {
		if err := store.Update(id, beads.UpdateOpts{Assignee: &body.Assignee}); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "assigned", "assignee": body.Assignee})
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "bead "+id+" not found")
}

func (s *Server) handleBeadDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, store := range s.beadStoresForID(id) {
		if err := store.Close(id); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "bead "+id+" not found")
}

// matchBead applies filters to a bead.
func matchBead(b beads.Bead, status, typ, label, assignee string) bool {
	if status != "" && b.Status != status {
		return false
	}
	if typ != "" && b.Type != typ {
		return false
	}
	if assignee != "" && b.Assignee != assignee {
		return false
	}
	if label != "" {
		found := false
		for _, l := range b.Labels {
			if l == label {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// findStore returns the bead store for the given rig. If rig is empty, returns
// the sole store when exactly one exists (after deduplication), or nil when
// multiple distinct stores exist (caller should require explicit rig).
func (s *Server) findStore(rig string) beads.Store {
	if rig != "" {
		return s.state.BeadStore(rig)
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
	if prefix := beadPrefix(strings.TrimSpace(id)); prefix != "" {
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

// beadGraphResponseJSON is the response shape for GET /v0/beads/graph/{rootID}.
// Returns raw beads and deps — no status mapping, no presentation logic.
type beadGraphResponseJSON struct {
	Root  beads.Bead            `json:"root"`
	Beads []beads.Bead          `json:"beads"`
	Deps  []workflowDepResponse `json:"deps"`
}

func (s *Server) handleBeadGraph(w http.ResponseWriter, r *http.Request) {
	rootID := r.PathValue("rootID")
	if rootID == "" {
		writeError(w, http.StatusBadRequest, "invalid", "rootID is required")
		return
	}

	stores := s.state.BeadStores()

	// Find root bead by scanning stores (bd handles prefix routing via routes.jsonl)
	var root beads.Bead
	var foundStore beads.Store
	for _, rigName := range sortedRigNames(stores) {
		store := stores[rigName]
		b, err := store.Get(rootID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		root = b
		foundStore = store
		break
	}
	if foundStore == nil {
		writeError(w, http.StatusNotFound, "not_found", "bead "+rootID+" not found")
		return
	}

	// Collect all beads in the graph: root + children where gc.root_bead_id == rootID
	all, err := foundStore.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	graphBeads := []beads.Bead{root}
	beadIndex := map[string]beads.Bead{root.ID: root}
	for _, b := range all {
		if b.ID == root.ID {
			continue
		}
		if b.Metadata != nil && b.Metadata["gc.root_bead_id"] == rootID {
			graphBeads = append(graphBeads, b)
			beadIndex[b.ID] = b
		}
	}

	// Collect deps between graph beads (reuse existing dedup logic)
	deps, _ := collectWorkflowDeps(foundStore, beadIndex)

	writeIndexJSON(w, s.latestIndex(), beadGraphResponseJSON{
		Root:  root,
		Beads: graphBeads,
		Deps:  deps,
	})
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

// decodeBody decodes JSON request body into v.
// Limits body size to 1 MiB to prevent OOM from oversized requests.
func decodeBody(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20) // 1 MiB
	return json.NewDecoder(r.Body).Decode(v)
}

func decodeBodyBytes(r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20) // 1 MiB
	return io.ReadAll(r.Body)
}
