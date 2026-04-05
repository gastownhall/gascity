package api

import (
	"errors"
	"net/http"

	"github.com/gastownhall/gascity/internal/beads"
)

func (s *Server) handleConvoyList(w http.ResponseWriter, r *http.Request) {
	bp := parseBlockingParams(r)
	if bp.isBlocking() {
		waitForChange(r.Context(), s.state.EventProvider(), bp)
	}

	pp := parsePagination(r, 50)
	stores := s.state.BeadStores()
	rigNames := sortedRigNames(stores)
	var convoys []beads.Bead
	for _, rigName := range rigNames {
		store := stores[rigName]
		list, err := store.List(beads.ListQuery{Type: "convoy"})
		if err != nil {
			continue
		}
		convoys = append(convoys, list...)
	}

	if convoys == nil {
		convoys = []beads.Bead{}
	}
	if !pp.IsPaging {
		total := len(convoys)
		if pp.Limit < len(convoys) {
			convoys = convoys[:pp.Limit]
		}
		writeListJSON(w, s.latestIndex(), convoys, total)
		return
	}
	page, total, nextCursor := paginate(convoys, pp)
	if page == nil {
		page = []beads.Bead{}
	}
	writePagedJSON(w, s.latestIndex(), page, total, nextCursor)
}

func (s *Server) handleConvoyGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Formula-compiled convoy (graph workflow): delegate to the graph
	// snapshot builder which returns the full DAG with deps and status.
	if isGraphConvoyID(s, id) {
		s.handleWorkflowGet(w, r)
		return
	}

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
		if b.Type != "convoy" {
			writeError(w, http.StatusNotFound, "not_found", "bead "+id+" is not a convoy")
			return
		}

		children, err := store.List(beads.ListQuery{
			ParentID:      id,
			IncludeClosed: true,
			Sort:          beads.SortCreatedAsc,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if children == nil {
			children = []beads.Bead{}
		}

		// Compute progress.
		total := len(children)
		closed := 0
		for _, c := range children {
			if c.Status == "closed" {
				closed++
			}
		}

		writeIndexJSON(w, s.latestIndex(), map[string]any{
			"convoy":   b,
			"children": children,
			"progress": map[string]int{"total": total, "closed": closed},
		})
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "convoy "+id+" not found")
}

// isGraphConvoyID checks if the bead is a formula-compiled graph convoy
// (workflow) by looking for the gc.kind=workflow marker.
func isGraphConvoyID(s *Server, id string) bool {
	stores := s.state.BeadStores()
	for _, rigName := range sortedRigNames(stores) {
		store := stores[rigName]
		b, err := store.Get(id)
		if err != nil {
			continue
		}
		return isGraphConvoyBead(b)
	}
	return false
}

func (s *Server) handleConvoyCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Rig   string   `json:"rig"`
		Title string   `json:"title"`
		Items []string `json:"items"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if body.Title == "" {
		writeError(w, http.StatusBadRequest, "invalid", "title is required")
		return
	}

	store := s.findStore(body.Rig)
	if store == nil {
		writeError(w, http.StatusBadRequest, "invalid", "rig is required when multiple rigs are configured")
		return
	}

	// Pre-validate all items exist before creating the convoy to avoid orphans.
	for _, itemID := range body.Items {
		if _, err := store.Get(itemID); err != nil {
			writeStoreError(w, err)
			return
		}
	}

	convoy, err := store.Create(beads.Bead{
		Title: body.Title,
		Type:  "convoy",
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	// Link child items to convoy.
	for _, itemID := range body.Items {
		pid := convoy.ID
		if err := store.Update(itemID, beads.UpdateOpts{ParentID: &pid}); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "failed to link item "+itemID+": "+err.Error())
			return
		}
	}

	writeJSON(w, http.StatusCreated, convoy)
}

func (s *Server) handleConvoyAdd(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Items []string `json:"items"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

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
		if b.Type != "convoy" {
			writeError(w, http.StatusBadRequest, "invalid", "bead "+id+" is not a convoy")
			return
		}
		// Pre-validate all items exist before linking.
		for _, itemID := range body.Items {
			if _, err := store.Get(itemID); err != nil {
				writeStoreError(w, err)
				return
			}
		}
		for _, itemID := range body.Items {
			pid := id
			if err := store.Update(itemID, beads.UpdateOpts{ParentID: &pid}); err != nil {
				writeError(w, http.StatusInternalServerError, "internal", "failed to link item "+itemID+": "+err.Error())
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "convoy "+id+" not found")
}

func (s *Server) handleConvoyRemove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Items []string `json:"items"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

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
		if b.Type != "convoy" {
			writeError(w, http.StatusBadRequest, "invalid", "bead "+id+" is not a convoy")
			return
		}
		// Pre-validate all items exist and belong to this convoy.
		for _, itemID := range body.Items {
			item, err := store.Get(itemID)
			if err != nil {
				if errors.Is(err, beads.ErrNotFound) {
					writeError(w, http.StatusNotFound, "not_found", "item "+itemID+" not found")
					return
				}
				writeError(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
			if item.ParentID != id {
				writeError(w, http.StatusBadRequest, "invalid", "item "+itemID+" does not belong to convoy "+id)
				return
			}
		}
		// Unlink items by clearing their ParentID.
		empty := ""
		for _, itemID := range body.Items {
			if err := store.Update(itemID, beads.UpdateOpts{ParentID: &empty}); err != nil {
				writeError(w, http.StatusInternalServerError, "internal", "failed to unlink item "+itemID+": "+err.Error())
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "convoy "+id+" not found")
}

func (s *Server) handleConvoyCheck(w http.ResponseWriter, r *http.Request) {
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
		if b.Type != "convoy" {
			writeError(w, http.StatusBadRequest, "invalid", "bead "+id+" is not a convoy")
			return
		}

		children, err := store.List(beads.ListQuery{
			ParentID:      id,
			IncludeClosed: true,
			Sort:          beads.SortCreatedAsc,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}

		total := len(children)
		closed := 0
		for _, c := range children {
			if c.Status == "closed" {
				closed++
			}
		}

		complete := total > 0 && closed == total
		writeJSON(w, http.StatusOK, map[string]any{
			"convoy_id": id,
			"total":     total,
			"closed":    closed,
			"complete":  complete,
		})
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "convoy "+id+" not found")
}

func (s *Server) handleConvoyDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Formula-compiled convoy (graph workflow): delegate to the graph
	// delete handler which tears down the full DAG.
	if isGraphConvoyID(s, id) {
		s.handleWorkflowDelete(w, r)
		return
	}

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
		if b.Type != "convoy" {
			writeError(w, http.StatusBadRequest, "invalid", "bead "+id+" is not a convoy")
			return
		}
		if err := store.Close(id); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "convoy "+id+" not found")
}

func (s *Server) handleConvoyClose(w http.ResponseWriter, r *http.Request) {
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
		if b.Type != "convoy" {
			writeError(w, http.StatusBadRequest, "invalid", "bead "+id+" is not a convoy")
			return
		}
		if err := store.Close(id); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "closed"})
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "convoy "+id+" not found")
}
