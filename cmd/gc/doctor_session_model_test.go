package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
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

	got, err := loadSessionModelDoctorBeads(store, store)
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

// TestLoadSessionModelDoctorBeadsReadsSessionUnionFromSessionStore pins the
// two-class split: under a [beads.classes.sessions] relocation the session beads
// live in a different store from the work beads. Reading both legs from the work
// store returned an empty session union and the check reported "session ownership
// is consistent" on a city whose session model was broken.
func TestLoadSessionModelDoctorBeadsReadsSessionUnionFromSessionStore(t *testing.T) {
	workStore := beads.NewMemStore()
	sessStore := beads.NewMemStore()

	work, err := workStore.Create(beads.Bead{Title: "open work", Status: "open"})
	if err != nil {
		t.Fatalf("Create(open work): %v", err)
	}
	sessionBead, err := sessStore.Create(beads.Bead{
		Title:  "session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Status: "open",
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	got, err := loadSessionModelDoctorBeads(workStore, sessStore)
	if err != nil {
		t.Fatalf("loadSessionModelDoctorBeads: %v", err)
	}
	ids := make(map[string]bool, len(got))
	for _, bead := range got {
		ids[bead.ID] = true
	}
	if !ids[sessionBead.ID] {
		t.Fatalf("session bead %s missing from relocated-sessions read; got IDs %+v", sessionBead.ID, ids)
	}
	if !ids[work.ID] {
		t.Fatalf("work bead %s missing; got IDs %+v", work.ID, ids)
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

func TestSessionModelDoctorSeparatesMailAndResolvesAliases(t *testing.T) {
	store := beads.NewMemStore()
	create := func(b beads.Bead) beads.Bead {
		t.Helper()
		got, err := store.Create(b)
		if err != nil {
			t.Fatalf("Create(%s): %v", b.Title, err)
		}
		return got
	}
	active := create(beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{
		"alias": "gc-packs/seat.current", "alias_history": "gc-packs/seat.former", "session_name": "s-active",
	}})
	closed := create(beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}})
	if err := store.Close(closed.ID); err != nil {
		t.Fatal(err)
	}
	retired := create(beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{"state": "archived", "continuity_eligible": "false"}})
	for _, b := range []beads.Bead{
		{Type: "message", Title: "historical mail", Assignee: closed.ID, Metadata: map[string]string{"gc.routed_to": "gone"}},
		{Type: "message", Title: "unread mail", Assignee: "gc-missing1"},
		{Type: "task", Title: "valid ID", Assignee: active.ID},
		{Type: "task", Title: "current alias", Assignee: "gc-packs/seat.current"},
		{Type: "task", Title: "old alias", Assignee: "gc-packs/seat.former"},
		{Type: "task", Title: "closed owner", Assignee: closed.ID},
		{Type: "task", Title: "retired owner", Assignee: retired.ID},
		{Type: "task", Title: "missing owner", Assignee: "ga-abc12"},
	} {
		create(b)
	}
	check := &sessionModelDoctorCheck{cfg: &config.City{}, newStore: func(string) (beads.Store, error) { return store, nil }}
	result := check.Run(&doctor.CheckContext{})
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{"historical-alias-owner", "closed-bead-owner", "retired-bead-owner", "missing-bead-owner: "} {
		if !strings.Contains(details, want) {
			t.Errorf("missing %q: %s", want, details)
		}
	}
	if len(result.Details) != 4 {
		t.Errorf("got %d findings, want 4: %s", len(result.Details), details)
	}
}

func TestSessionModelDoctorIDShapes(t *testing.T) {
	for _, tc := range []struct {
		token string
		want  bool
	}{
		{"gc-packs/seat.current", false},
		{"gc-packs/seat.former", false},
		{"gc-abc12", true},
		{"bd-123", true},
		{"mc-a23", true},
		{"ga-abc12", true},
		{"fa-wisp-7rqp9u9", true},
	} {
		if got := looksLikeSessionBeadID(tc.token); got != tc.want {
			t.Errorf("looksLikeSessionBeadID(%q) = %v, want %v", tc.token, got, tc.want)
		}
	}
}

// Each class must fail the diagnostic rather than silently producing a partial census.
type sessionModelFailListStore struct{ beads.Store }

func (sessionModelFailListStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("store unavailable")
}

func TestLoadSessionModelDoctorBeadsReportsClassFailures(t *testing.T) {
	good := beads.NewMemStore()
	bad := sessionModelFailListStore{Store: good}
	for _, tc := range []struct {
		name           string
		work, sessions beads.Store
		want           string
	}{
		{"work", bad, good, "open work: store unavailable"},
		{"sessions", good, bad, "session beads: store unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadSessionModelDoctorBeads(tc.work, tc.sessions)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
