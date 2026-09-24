package api

// Where POST /v0/beads puts the bead it creates.
//
// The by-id door of this surface has taken residency through the resolver since
// the ga-axin6 lane; the CREATE door never did. It picked its store with
// findStore(rig) — "the rig you named, else the city work ledger" — and then
// created whatever it was handed there, including a bead whose class a
// converged city moved off that ledger. That is not a read miss you can retry:
// it is a bead minted into the wrong store, in the work ledger's own id
// namespace, which `gc storage status` counts as a stranded write and which
// every later class-routed read looks for in the binding and does not find.
//
// The rows below are the two halves of the placement rule. A relocated class
// goes to the binding, whatever rig-less body asked for it; a city with nothing
// relocated chooses exactly the store it chose before, for every shape, because
// a placement plan over an unsplit topology names the work store and the
// resolver is the identity there.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// createBody is the POST /v0/beads request body, spelled as a struct so a
// field rename in BeadCreateInput.Body is a compile error here rather than a
// silently-ignored JSON key.
type createBody struct {
	Rig      string            `json:"rig,omitempty"`
	Title    string            `json:"title"`
	Type     string            `json:"type,omitempty"`
	Labels   []string          `json:"labels,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// bead returns the bead the classifier sees for this body — the same fields
// coordclass.Classify reads — so a row can state its intended class and have
// the test verify it rather than assert it in a comment.
func (b createBody) bead() beads.Bead {
	return beads.Bead{Title: b.Title, Type: b.Type, Labels: b.Labels, Metadata: b.Metadata}
}

// infraCreateBodies is one create body per infrastructure class, each shaped
// the way the class's real beads are shaped. Every entry's class is asserted
// against coordclass.Classify before it is used, so a classifier change breaks
// the fixture instead of quietly turning a row into a work-class no-op.
func infraCreateBodies() map[coordclass.Class]createBody {
	return map[coordclass.Class]createBody{
		coordclass.ClassGraph:     {Title: "a wisp", Type: "task", Labels: []string{"gc:wisp"}},
		coordclass.ClassMessaging: {Title: "mail", Type: "message"},
		coordclass.ClassSessions:  {Title: "a session", Type: "session"},
		coordclass.ClassOrders:    {Title: "an order run", Type: "task", Labels: []string{"order-tracking"}},
		coordclass.ClassNudges:    {Title: "a nudge", Type: "task", Labels: []string{"gc:nudge"}},
	}
}

// newWholeSplitCreateState is a converged city as this plane observes one: an
// HQ work store, a rig work store, and ONE binding serving every relocated
// class. The four class accessors point at the same store because this build
// serves the whole split or none of it, which is also what lets
// completeObservedClasses round the binding up to the messaging class the API
// has no accessor for.
func newWholeSplitCreateState(t *testing.T) (*fakeState, beads.Store) {
	t.Helper()
	fs := newFakeState(t)
	fs.cityBeadStore = splittest.NewWorkStore(t, "hq")
	fs.stores["myrig"] = splittest.NewWorkStore(t, "ra")
	binding := splittest.NewClassStore(t, config.BeadClassGraph)
	fs.graphBeadStore = binding
	fs.sessionsBeadStore = binding
	fs.ordersBeadStore = binding
	fs.nudgesBeadStore = binding
	return fs, binding
}

// newUnsplitCreateState is the same city with nothing relocated: every class
// accessor falls back to the city store, so the topology carries no binding.
func newUnsplitCreateState(t *testing.T) *fakeState {
	t.Helper()
	fs := newFakeState(t)
	fs.cityBeadStore = splittest.NewWorkStore(t, "hq")
	fs.stores["myrig"] = splittest.NewWorkStore(t, "ra")
	return fs
}

// postBead issues POST /v0/beads and returns the raw recorder, so a row can
// assert on a refusal status as easily as on a created bead.
//
// The handler is a parameter rather than a per-call newTestCityHandler because
// every call to that helper builds a fresh SupervisorMux, and therefore a fresh
// per-city Server with its own idempotency cache — two posts through two
// handlers can never replay each other.
func postBead(t *testing.T, h http.Handler, fs *fakeState, body createBody, idemKey string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal create body: %v", err)
	}
	req := newPostRequest(cityURL(fs, "/beads"), bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// createBead posts and decodes, failing on any non-201.
func createBead(t *testing.T, h http.Handler, fs *fakeState, body createBody, idemKey string) beads.Bead {
	t.Helper()
	rec := postBead(t, h, fs, body, idemKey)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /beads %+v status = %d, want 201 (body=%q)", body, rec.Code, rec.Body.String())
	}
	var created beads.Bead
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response %q: %v", rec.Body.String(), err)
	}
	if created.ID == "" {
		t.Fatalf("create response %q carries no bead id", rec.Body.String())
	}
	return created
}

func storeHolds(t *testing.T, store beads.Store, id string) bool {
	t.Helper()
	_, err := store.Get(id)
	return err == nil
}

// storeCount is the bead count of a leg, used by the refusal rows to prove the
// refused create wrote nothing anywhere.
func storeCount(t *testing.T, store beads.Store) int {
	t.Helper()
	list, err := store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return len(list)
}

// TestBeadCreateRoutesEveryInfraClassToTheBinding is the defect. Red on the
// pre-fix tree for all five classes, e.g.:
//
//	--- FAIL: .../graph
//	    created bead hq-1 is not in the class binding; a graph create on a
//	    converged city belongs there
//	    created bead hq-1 landed in the CITY WORK LEDGER — a stranded write in
//	    the store this city moved graph off
//
// The id is the tell: hq-1 is the work ledger's own prefix, so the bead is not
// merely in the wrong store, it carries an id minted from the wrong namespace
// and cannot be relocated by copying it.
func TestBeadCreateRoutesEveryInfraClassToTheBinding(t *testing.T) {
	for class, body := range infraCreateBodies() {
		t.Run(class.String(), func(t *testing.T) {
			if got := coordclass.Classify(body.bead()); got != class {
				t.Fatalf("fixture body %+v classifies as %s, want %s", body, got, class)
			}
			fs, binding := newWholeSplitCreateState(t)

			created := createBead(t, newTestCityHandler(t, fs), fs, body, "")

			if !storeHolds(t, binding, created.ID) {
				t.Errorf("created bead %s is not in the class binding; a %s create on a converged city belongs there", created.ID, class)
			}
			if storeHolds(t, fs.cityBeadStore, created.ID) {
				t.Errorf("created bead %s landed in the CITY WORK LEDGER — a stranded write in the store this city moved %s off", created.ID, class)
			}
		})
	}
}

// TestBeadCreateKeepsWorkClassOnTheWorkLedger is the other half: relocating a
// class must not relocate the work beads that share the city. A fix that routed
// every create at the binding would pass the row above and destroy this one.
func TestBeadCreateKeepsWorkClassOnTheWorkLedger(t *testing.T) {
	fs, binding := newWholeSplitCreateState(t)
	body := createBody{Title: "ordinary work", Type: "task"}
	if got := coordclass.Classify(body.bead()); got != coordclass.ClassWork {
		t.Fatalf("fixture body classifies as %s, want work", got)
	}

	created := createBead(t, newTestCityHandler(t, fs), fs, body, "")

	if !storeHolds(t, fs.cityBeadStore, created.ID) {
		t.Errorf("work-class bead %s is not in the city work ledger", created.ID)
	}
	if storeHolds(t, binding, created.ID) {
		t.Errorf("work-class bead %s landed in the class binding; only relocated classes are placed there", created.ID)
	}
}

// TestBeadCreateOnAnUnsplitCityIsUnchanged pins the identity case: with nothing
// relocated the placement plan names the work store, so every shape — infra or
// not, rig-named or not — chooses exactly the store findStore chose before.
func TestBeadCreateOnAnUnsplitCityIsUnchanged(t *testing.T) {
	bodies := infraCreateBodies()
	bodies[coordclass.ClassWork] = createBody{Title: "ordinary work", Type: "task"}
	for class, body := range bodies {
		t.Run(class.String(), func(t *testing.T) {
			fs := newUnsplitCreateState(t)
			h := newTestCityHandler(t, fs)
			created := createBead(t, h, fs, body, "")
			if !storeHolds(t, fs.cityBeadStore, created.ID) {
				t.Errorf("bead %s is not in the city work ledger; an unsplit city has no binding and must be byte-identical", created.ID)
			}

			rigged := body
			rigged.Rig = "myrig"
			riggedBead := createBead(t, h, fs, rigged, "")
			if !storeHolds(t, fs.stores["myrig"], riggedBead.ID) {
				t.Errorf("bead %s is not in the named rig's store; an explicit rig still chooses the rig on an unsplit city", riggedBead.ID)
			}
		})
	}
}

// TestBeadCreateRefusesAnInfraClassWithAnExplicitRig states the one refusal the
// seam adds.
//
// A class binding is CITY-keyed: there is one per city and no per-rig binding to
// route to. So a caller asking for a relocated class in a named rig has asked
// for something that does not exist, and both silent answers are wrong —
// honoring the rig re-creates the stranded write, ignoring it returns a bead
// that is not where the caller was told it would be.
func TestBeadCreateRefusesAnInfraClassWithAnExplicitRig(t *testing.T) {
	for class, body := range infraCreateBodies() {
		t.Run(class.String(), func(t *testing.T) {
			fs, binding := newWholeSplitCreateState(t)
			body.Rig = "myrig"

			rec := postBead(t, newTestCityHandler(t, fs), fs, body, "")

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%q)", rec.Code, rec.Body.String())
			}
			if n := storeCount(t, binding); n != 0 {
				t.Errorf("the refused create still wrote %d bead(s) to the binding", n)
			}
			if n := storeCount(t, fs.stores["myrig"]); n != 0 {
				t.Errorf("the refused create still wrote %d bead(s) to the rig store", n)
			}
		})
	}
}

// TestBeadCreateIdempotencyReplayHoldsAcrossPlacement pins that the seam lives
// INSIDE the idempotency closure. A replay must return the first bead and must
// not create a second one — and it must do so for a placed create, where the
// store the record was written through is not the store findStore names.
func TestBeadCreateIdempotencyReplayHoldsAcrossPlacement(t *testing.T) {
	fs, binding := newWholeSplitCreateState(t)
	body := infraCreateBodies()[coordclass.ClassGraph]
	h := newTestCityHandler(t, fs)

	first := createBead(t, h, fs, body, "key-1")
	replay := createBead(t, h, fs, body, "key-1")

	if first.ID != replay.ID {
		t.Fatalf("replay returned %s, want the first bead %s", replay.ID, first.ID)
	}
	if n := storeCount(t, binding); n != 1 {
		t.Fatalf("the binding holds %d bead(s) after an idempotent replay, want 1", n)
	}
}

// TestIdemRecordsAreWorkClass freezes the fragility the S5 audit found at
// rigidem.go:351: the durable rig-create idempotency record is a "task" bead
// carrying gc-idem labels, and it is work class only because coordclass has no
// arm for that family. It is created through the rig's own store, deliberately,
// so if an arm is ever added this row fails first — before the record starts
// being minted into a binding that no lookup on that path reads.
func TestIdemRecordsAreWorkClass(t *testing.T) {
	record := beads.Bead{
		Type:   "task",
		Title:  "idem: rig-create req-1",
		Labels: []string{idemLabel, idemLabelRigCreate},
		Metadata: beads.StringMap{
			metaIdemKind:      idemKindRigCreate,
			metaIdemCity:      "test-city",
			metaIdemRequestID: "req-1",
		},
	}
	if got := coordclass.Classify(record); got != coordclass.ClassWork {
		t.Fatalf("a gc-idem record classifies as %s, want work — createIdemRecord writes it through the rig store, and every lookup on that path reads only there", got)
	}
}
