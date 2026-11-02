package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// classFlipAPIFixture is one bead API under test, its work-store bead, and its
// binding-resident graph step. split selects a whole-split city (every
// infrastructure class served from one binding) or a single-store one.
type classFlipAPIFixture struct {
	h        http.Handler
	state    *fakeState
	work     beads.Store
	binding  beads.Store
	workID   string
	graphID  string
	bindings int
}

func newClassFlipAPIFixture(t *testing.T, split bool) classFlipAPIFixture {
	t.Helper()
	state := newFakeState(t)
	work := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "wk-1", Title: "w", Type: "task", Status: "open"}}, nil)
	state.stores["myrig"] = work
	state.cityBeadStore = beads.NewMemStore()
	f := classFlipAPIFixture{state: state, work: work, workID: "wk-1"}
	if split {
		binding := beads.NewMemStoreFrom(0, []beads.Bead{{
			ID: "gcg-1", Title: "step", Type: "task", Status: "open",
			Metadata: map[string]string{"gc.root_bead_id": "gcg-0"},
		}, {
			// A wisp whose only graph marker is a label.
			ID: "gcg-2", Title: "wisp", Type: "task", Status: "open",
			Labels: []string{"gc:wisp"},
		}}, nil)
		state.graphBeadStore = binding
		state.sessionsBeadStore = binding
		state.ordersBeadStore = binding
		state.nudgesBeadStore = binding
		f.binding = binding
		f.graphID = "gcg-1"
	}
	f.h = newTestCityHandler(t, state)
	f.bindings = len((&Server{state: state}).residencyTopology().Bindings)
	return f
}

// TestBeadAPIUpdateRefusesAClassFlipOnASplitCity is the audit's R5 finding as
// a permanent test. On a split city, PATCH and POST /update refuse every write
// that would turn a work-store bead into an infrastructure-class bead in
// place, and every write that would turn a binding resident into a work bead;
// nothing is written. On a single-store city the same writes behave exactly as
// before: the class-must-not-flip guard is silent there.
func TestBeadAPIUpdateRefusesAClassFlipOnASplitCity(t *testing.T) {
	workFlips := []struct {
		name, body string
		after      coordclass.Class
		// ownedToday marks a write refuseGenericWrite already refused before
		// this guard existed, on every city; it stays refused on both.
		ownedToday bool
	}{
		{"metadata gc.root_bead_id", `{"metadata":{"gc.root_bead_id":"gcg-1"}}`, coordclass.ClassGraph, false},
		{"metadata gc.kind=wisp", `{"metadata":{"gc.kind":"wisp"}}`, coordclass.ClassGraph, false},
		{"metadata gc.kind=workflow", `{"metadata":{"gc.kind":"workflow"}}`, coordclass.ClassGraph, false},
		{"metadata gc.formula_contract=graph.v2", `{"metadata":{"gc.formula_contract":"graph.v2"}}`, coordclass.ClassGraph, false},
		{"label wisp", `{"labels":["wisp"]}`, coordclass.ClassGraph, false},
		{"label gc:wisp", `{"labels":["gc:wisp"]}`, coordclass.ClassGraph, false},
		{"type wisp", `{"type":"wisp"}`, coordclass.ClassGraph, false},
		{"label gc:rt", `{"labels":["gc:rt"]}`, coordclass.ClassSessions, false},
		{"label gc:fact", `{"labels":["gc:fact"]}`, coordclass.ClassSessions, false},
		{"label gc:op", `{"labels":["gc:op"]}`, coordclass.ClassSessions, false},
		{"label gc:wait (owned today)", `{"labels":["gc:wait"]}`, coordclass.ClassSessions, true},
	}
	for _, split := range []bool{true, false} {
		arm := "single-store"
		if split {
			arm = "split"
		}
		for _, method := range []string{"PATCH", "POST"} {
			for _, c := range workFlips {
				t.Run(arm+"/"+method+"/"+c.name, func(t *testing.T) {
					f := newClassFlipAPIFixture(t, split)
					if split != (f.bindings == 1) {
						t.Fatalf("fixture split=%v resolved %d bindings", split, f.bindings)
					}
					rec := beadUpdate(t, f.h, f.state, method, f.workID, c.body)
					got, err := f.work.Get(f.workID)
					if err != nil {
						t.Fatal(err)
					}
					class := coordclass.Classify(got)
					if !split && !c.ownedToday {
						// No behavior change on a single-store city.
						if rec.Code != http.StatusOK || class != c.after {
							t.Fatalf("single-store %s %s = %d (class %s), want 200 and class %s; body=%s", method, c.body, rec.Code, class, c.after, rec.Body.String())
						}
						return
					}
					if rec.Code != http.StatusUnprocessableEntity {
						t.Fatalf("%s %s = %d, want 422; body=%s", method, c.body, rec.Code, rec.Body.String())
					}
					if class != coordclass.ClassWork {
						t.Fatalf("refused write still landed: the work-store bead is now %s class", class)
					}
					if split && !c.ownedToday {
						for _, want := range []string{"work-class", c.after.String() + "-class", coordclass.MintCommand(c.after), "nothing was written"} {
							if !strings.Contains(strings.ToLower(rec.Body.String()), strings.ToLower(want)) {
								t.Errorf("refusal does not mention %q: %s", want, rec.Body.String())
							}
						}
					}
				})
			}
		}
	}

	// The reverse: a binding resident written into a work bead in place.
	for _, c := range []struct{ name, id, body string }{
		{"blank the only graph marker", "gcg-1", `{"metadata":{"gc.root_bead_id":""}}`},
		// remove_labels is part of the write the guard judges (mutant M23).
		{"remove the only label marker", "gcg-2", `{"remove_labels":["gc:wisp"]}`},
	} {
		t.Run("split/binding/"+c.name, func(t *testing.T) {
			f := newClassFlipAPIFixture(t, true)
			f.graphID = c.id
			rec := beadUpdate(t, f.h, f.state, "PATCH", f.graphID, c.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("PATCH %s on a binding resident = %d, want 422; body=%s", c.body, rec.Code, rec.Body.String())
			}
			got, err := f.binding.Get(f.graphID)
			if err != nil {
				t.Fatal(err)
			}
			if class := coordclass.Classify(got); class != coordclass.ClassGraph {
				t.Fatalf("refused write still landed: the binding resident is now %s class", class)
			}
			if !strings.Contains(rec.Body.String(), coordclass.MintCommand(coordclass.ClassWork)) {
				t.Errorf("refusal does not name %q: %s", coordclass.MintCommand(coordclass.ClassWork), rec.Body.String())
			}
		})
	}

	// Writes that do not move a bead's class stay allowed on a split city.
	for _, c := range []struct {
		name, id, body string
		binding        bool
	}{
		{"work: ordinary label and metadata", "wk-1", `{"labels":["triage"],"metadata":{"gc.outcome":"pass"}}`, false},
		{"binding: ordinary metadata", "gcg-1", `{"metadata":{"gc.outcome":"pass"}}`, true},
	} {
		t.Run("split/allowed/"+c.name, func(t *testing.T) {
			f := newClassFlipAPIFixture(t, true)
			rec := beadUpdate(t, f.h, f.state, "PATCH", c.id, c.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("PATCH %s = %d, want 200; body=%s", c.body, rec.Code, rec.Body.String())
			}
		})
	}
}
