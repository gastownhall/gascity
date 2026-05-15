package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

func TestLoadSessionModelDoctorBeadsAvoidsBroadOpenWorkScan(t *testing.T) {
	store := &doctorListSpyStore{MemStore: beads.NewMemStore()}
	open, err := store.Create(beads.Bead{Title: "open work", Status: "open"})
	if err != nil {
		t.Fatalf("Create(open): %v", err)
	}
	inProgress, err := store.Create(beads.Bead{Title: "active work", Status: "in_progress"})
	if err != nil {
		t.Fatalf("Create(in_progress): %v", err)
	}
	inProgressStatus := "in_progress"
	if err := store.Update(inProgress.ID, beads.UpdateOpts{Status: &inProgressStatus}); err != nil {
		t.Fatalf("Update(in_progress): %v", err)
	}
	sessionBead, err := store.Create(beads.Bead{
		Title:  "session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Status: "closed",
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	closedStatus := "closed"
	if err := store.Update(sessionBead.ID, beads.UpdateOpts{Status: &closedStatus}); err != nil {
		t.Fatalf("Update(session closed): %v", err)
	}

	got, err := loadSessionModelDoctorBeads(store)
	if err != nil {
		t.Fatalf("loadSessionModelDoctorBeads: %v", err)
	}

	ids := make(map[string]bool, len(got))
	for _, bead := range got {
		ids[bead.ID] = true
	}
	for _, id := range []string{open.ID, inProgress.ID, sessionBead.ID} {
		if !ids[id] {
			t.Fatalf("loadSessionModelDoctorBeads missing %s; got IDs %+v", id, ids)
		}
	}
	for _, query := range store.queries {
		if query.AllowScan {
			t.Fatalf("query %+v used AllowScan; doctor should use bounded indexed selectors", query)
		}
	}
	if !store.sawStatus["open"] || !store.sawStatus["in_progress"] {
		t.Fatalf("status queries = %+v, want open and in_progress", store.sawStatus)
	}
}

type doctorListSpyStore struct {
	*beads.MemStore
	queries   []beads.ListQuery
	sawStatus map[string]bool
}

func (s *doctorListSpyStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.queries = append(s.queries, query)
	if s.sawStatus == nil {
		s.sawStatus = make(map[string]bool)
	}
	if query.Status != "" {
		s.sawStatus[query.Status] = true
	}
	return s.MemStore.List(query)
}
