package session

import (
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestListFromInfosMatchesListFullFromBeads is the oracle that lets WI-6 W2 delete
// Manager.ListFullFromBeads: the Info-fed typed listing must produce exactly the
// same enriched session set as the retired bead-fed listing across the corpus
// (including the type-only label-lost and label-only repairable beads the union
// feed surfaces) and the state/template filter matrix.
func TestListFromInfosMatchesListFullFromBeads(t *testing.T) {
	at := func(minN int) time.Time {
		return time.Date(2026, 1, 2, 3, 4, minN, 0, time.UTC)
	}
	corpus := []beads.Bead{
		{
			ID: "canonical", Type: BeadType, Status: "open", Labels: []string{LabelSession},
			Metadata: map[string]string{"state": "asleep", "template": "polecat", "session_name": "canonical"}, CreatedAt: at(1),
		},
		{ID: "type-only", Type: BeadType, Status: "open", // label lost after a crash
			Metadata: map[string]string{"state": "active", "template": "polecat", "session_name": "type-only"}, CreatedAt: at(2)},
		{ID: "label-only", Type: "", Status: "open", Labels: []string{LabelSession}, // type lost, repairable
			Metadata: map[string]string{"state": "asleep", "template": "sky", "session_name": "label-only"}, CreatedAt: at(3)},
		{
			ID: "non-session", Type: "task", Status: "open", Labels: []string{"work"},
			Metadata: map[string]string{"state": "active"}, CreatedAt: at(4),
		},
		{
			ID: "closed", Type: BeadType, Status: "closed", Labels: []string{LabelSession},
			Metadata: map[string]string{"state": "asleep", "template": "polecat", "session_name": "closed"}, CreatedAt: at(5),
		},
		{ID: "no-state", Type: BeadType, Status: "open", Labels: []string{LabelSession}, // StateNone: no "state" metadata key
			Metadata: map[string]string{"template": "polecat", "session_name": "no-state"}, CreatedAt: at(6)},
	}

	infos := make([]Info, 0, len(corpus))
	for _, b := range corpus {
		infos = append(infos, infoFromPersistedBead(b))
	}

	mgr := NewManagerWithOptions(beads.NewMemStore(), runtime.NewFake())

	// "active," is the empty-comma-member filter humaHandleCityPending uses
	// (StateActive + StateNone): it must match the no-state fixture via the empty
	// state member, exactly as the bead-form sessionMatchesFilters does.
	for _, sf := range []string{"", "asleep", "active", "all", "closed", "active,asleep", "active,"} {
		for _, tf := range []string{"", "polecat", "sky"} {
			got := mgr.ListFromInfos(infos, sf, tf)
			// want reproduces the retired ListFullFromBeads exactly: the bead-form
			// filter (IsSessionBeadOrRepairable + sessionMatchesFilters) then the
			// runtime overlay (infoFromBead == EnrichInfo(InfoFromPersistedBead)).
			want := []Info{}
			for _, b := range corpus {
				if !IsSessionBeadOrRepairable(b) {
					continue
				}
				if !sessionMatchesFilters(b, sf, tf) {
					continue
				}
				want = append(want, mgr.EnrichInfo(infoFromPersistedBead(b)))
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ListFromInfos(state=%q,template=%q) diverged from the retired bead-form listing:\n got = %+v\nwant = %+v", sf, tf, got, want)
			}
		}
	}
}
