package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"

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
	stores := s.state.BeadStores()

	for _, rigName := range sortedRigNames(stores) {
		store := stores[rigName]
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
	stores := s.state.BeadStores()

	for _, rigName := range sortedRigNames(stores) {
		store := stores[rigName]
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
	stores := s.state.BeadStores()

	for _, rigName := range sortedRigNames(stores) {
		store := stores[rigName]
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
	var body struct {
		Title        *string           `json:"title"`
		Assignee     *string           `json:"assignee"`
		Description  *string           `json:"description"`
		Labels       []string          `json:"labels"`
		RemoveLabels []string          `json:"remove_labels"`
		Metadata     map[string]string `json:"metadata"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	stores := s.state.BeadStores()
	opts := beads.UpdateOpts{
		Title:        body.Title,
		Assignee:     body.Assignee,
		Description:  body.Description,
		Labels:       body.Labels,
		RemoveLabels: body.RemoveLabels,
	}

	for _, rigName := range sortedRigNames(stores) {
		store := stores[rigName]
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
	stores := s.state.BeadStores()
	status := "open"

	for _, rigName := range sortedRigNames(stores) {
		store := stores[rigName]
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

	stores := s.state.BeadStores()
	for _, rigName := range sortedRigNames(stores) {
		store := stores[rigName]
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
	stores := s.state.BeadStores()

	for _, rigName := range sortedRigNames(stores) {
		store := stores[rigName]
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

// decodeBody decodes JSON request body into v.
// Limits body size to 1 MiB to prevent OOM from oversized requests.
func decodeBody(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20) // 1 MiB
	return json.NewDecoder(r.Body).Decode(v)
}
