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

// TestFilterInfosMatchesListFromInfosWithoutEnrich is the split oracle for
// gascity#4390's cheap-roster path: FilterInfos (the filter-only half) must
// (a) apply exactly the same persisted-state filter as ListFromInfos, and
// (b) leave every surviving Info completely unenriched — none of the runtime
// fields EnrichInfo sets (Transport, Attached, LastActive, the stale-active
// State downgrade) may appear — so EnrichInfos(FilterInfos(x)) stays provably
// equal to ListFromInfos(x) across the same state/template filter matrix.
func TestFilterInfosMatchesListFromInfosWithoutEnrich(t *testing.T) {
	at := func(minN int) time.Time {
		return time.Date(2026, 1, 2, 3, 4, minN, 0, time.UTC)
	}
	corpus := []beads.Bead{
		{
			ID: "canonical", Type: BeadType, Status: "open", Labels: []string{LabelSession},
			Metadata: map[string]string{"state": "asleep", "template": "polecat", "session_name": "canonical"}, CreatedAt: at(1),
		},
		{
			ID: "type-only", Type: BeadType, Status: "open",
			Metadata: map[string]string{"state": "active", "template": "polecat", "session_name": "type-only"}, CreatedAt: at(2),
		},
		{
			ID: "label-only", Type: "", Status: "open", Labels: []string{LabelSession},
			Metadata: map[string]string{"state": "asleep", "template": "sky", "session_name": "label-only"}, CreatedAt: at(3),
		},
		{
			ID: "non-session", Type: "task", Status: "open", Labels: []string{"work"},
			Metadata: map[string]string{"state": "active"}, CreatedAt: at(4),
		},
		{
			ID: "closed", Type: BeadType, Status: "closed", Labels: []string{LabelSession},
			Metadata: map[string]string{"state": "asleep", "template": "polecat", "session_name": "closed"}, CreatedAt: at(5),
		},
	}
	infos := make([]Info, 0, len(corpus))
	for _, b := range corpus {
		infos = append(infos, infoFromPersistedBead(b))
	}

	// A live runtime provider that would make EnrichInfo observably change an
	// active session's fields — "type-only" is deliberately left un-Start'd
	// (IsRunning=false), which is exactly what triggers EnrichInfo's
	// stale-active-downgrade (State: active -> asleep). That downgrade is the
	// most reliable unenriched/enriched tell available here: unlike
	// Attached/LastActive (which the downgrade itself skips setting once State
	// is no longer StateActive), the downgrade always fires for an active
	// session whose runtime the provider doesn't know about.
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(beads.NewMemStore(), sp)

	for _, sf := range []string{"", "asleep", "active", "all", "closed", "active,asleep"} {
		for _, tf := range []string{"", "polecat", "sky"} {
			// Two independent calls: FilterInfos is pure over its input slice
			// (never mutates infos), so calling it twice and checking each
			// result separately avoids EnrichInfos's in-place mutation of its
			// argument contaminating the unenriched-row check below.
			forUnenrichedCheck := mgr.FilterInfos(infos, sf, tf)
			for _, info := range forUnenrichedCheck {
				if info.ID == "type-only" && info.State != StateActive {
					t.Fatalf("FilterInfos(state=%q,template=%q) downgraded type-only's State to %q — FilterInfos must not run the runtime stale-active check", sf, tf, info.State)
				}
			}

			enrichedWant := mgr.ListFromInfos(infos, sf, tf)
			enrichedGot := mgr.EnrichInfos(mgr.FilterInfos(infos, sf, tf))
			if !reflect.DeepEqual(enrichedGot, enrichedWant) {
				t.Fatalf("EnrichInfos(FilterInfos(state=%q,template=%q)) diverged from ListFromInfos:\n got = %+v\nwant = %+v", sf, tf, enrichedGot, enrichedWant)
			}
		}
	}
}
