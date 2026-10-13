package api

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/session"
)

// ownedRows are the controller-owned rows (coordclass.ControllerOwned;
// architecture B), each spelling the generic API must take for one: a session
// row, typed, crash-damaged (no type, the session label) or a typed bead
// carrying the label; a startup-health episode; a convergence root; and a
// type spelled in another case or with space around it.
var ownedRows = []struct {
	name   string
	typ    string
	labels []string
	meta   map[string]string
}{
	{"session", session.BeadType, []string{session.LabelSession}, map[string]string{"state": "asleep"}},
	{"crash-damaged session", "", []string{session.LabelSession}, nil},
	{"task carrying the session label", "task", []string{"note", session.LabelSession}, nil},
	{"session spelled ' Session '", " Session ", nil, nil},
	{"startup-health episode", session.StartupHealthEpisodeType, nil, map[string]string{session.StartupHealthSessionNameMetadataKey: "myrig--worker", session.StartupHealthQuarantinedUntilMetadataKey: "9999-01-01T00:00:00Z"}},
	{"episode spelled 'Startup-Health-Episode'", "Startup-Health-Episode", nil, nil},
	{"convergence root", "convergence", nil, nil},
}

// ownedRow creates row on store past the API (production installs no write
// hooks; withoutSessionWriteHooks), its type set after the create, which
// MemStore defaults.
func ownedRow(t *testing.T, store beads.Store, typ string, labels []string, meta map[string]string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{Title: "owned", Type: "task", Labels: labels, Metadata: meta})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(b.ID, beads.UpdateOpts{Type: &typ}); err != nil {
		t.Fatal(err)
	}
	b, _ = store.Get(b.ID)
	return b
}

// ownedRoute is one generic write naming a row id: its method, path and
// body (%s the row's id, %c a convoy's) and the errors[] location of its
// refusal.
type ownedRoute struct {
	name, method, path, body, where string
	closed                          bool // the row is closed first (a reopen)
	tracked                         bool // the row is tracked in the convoy first (a remove)
}

var ownedRoutes = []ownedRoute{
	{name: "PATCH a title", method: "PATCH", path: "/bead/%s", body: `{"title":"renamed"}`, where: "path.id"},
	{name: "POST /update a metadata key", method: "POST", path: "/bead/%s/update", body: `{"metadata":{"note":"x"}}`, where: "path.id"},
	{name: "PATCH the quarantine", method: "PATCH", path: "/bead/%s", body: `{"metadata":{"quarantined_until":"9999-01-01T00:00:00Z","startup_health_quarantined_until":"9999-01-01T00:00:00Z"}}`, where: "path.id"},
	{name: "PATCH a workflow key", method: "PATCH", path: "/bead/%s", body: `{"metadata":{"gc.root_bead_id":"x"}}`, where: "path.id"},
	{name: "PATCH status=closed", method: "PATCH", path: "/bead/%s", body: `{"status":"closed"}`, where: "path.id"},
	{name: "PATCH status=deferred", method: "PATCH", path: "/bead/%s", body: `{"status":"deferred"}`, where: "path.id"},
	{name: "PATCH status=open on a closed row", method: "PATCH", path: "/bead/%s", body: `{"status":"open"}`, where: "path.id", closed: true},
	{name: "PATCH type=task", method: "PATCH", path: "/bead/%s", body: `{"type":"task"}`, where: "path.id"},
	{name: "PATCH the session label removed", method: "PATCH", path: "/bead/%s", body: `{"remove_labels":["gc:session"]}`, where: "path.id"},
	{name: "PATCH a parent", method: "PATCH", path: "/bead/%s", body: `{"parent":"%c"}`, where: "path.id"},
	{name: "POST /close", method: "POST", path: "/bead/%s/close", body: `{"reason":"done"}`, where: "path.id"},
	{name: "DELETE", method: "DELETE", path: "/bead/%s", where: "path.id"},
	{name: "POST /reopen", method: "POST", path: "/bead/%s/reopen", body: `{}`, where: "path.id", closed: true},
	{name: "POST /assign", method: "POST", path: "/bead/%s/assign", body: `{"assignee":"someone"}`, where: "path.id"},
	{name: "POST /sling its bead", method: "POST", path: "/sling", body: `{"target":"myrig/worker","bead":"%s"}`, where: "body.bead"},
	{name: "POST /sling a formula onto it", method: "POST", path: "/sling", body: `{"target":"myrig/worker","formula":"f","attached_bead_id":"%s"}`, where: "body.attached_bead_id"},
	{name: "POST /convoys tracking it", method: "POST", path: "/convoys", body: `{"title":"c","rig":"myrig","items":["%s"]}`, where: "body.items[0]"},
	{name: "POST /convoy/{id}/add", method: "POST", path: "/convoy/%c/add", body: `{"items":["%s"]}`, where: "body.items[0]"},
	{name: "POST /convoy/{id}/remove", method: "POST", path: "/convoy/%c/remove", body: `{"items":["%s"]}`, where: "body.items[0]", tracked: true},
}

func (r ownedRoute) fill(s, id, convoy string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "%s", id), "%c", convoy)
}

// TestGenericAPINeverWritesAControllerOwnedRow (architecture B): every
// generic write naming a controller-owned row, of each owned kind and
// spelling, is refused with 422 and an errors[] entry at the field that
// named it, and writes nothing: the row, its convoy membership and the
// store's bead count are as they were.
func TestGenericAPINeverWritesAControllerOwnedRow(t *testing.T) {
	for _, row := range ownedRows {
		for _, r := range ownedRoutes {
			t.Run(row.name+"/"+r.name, func(t *testing.T) {
				withoutSessionWriteHooks(t)
				state := newFakeState(t)
				store := state.stores["myrig"]
				h := newTestCityHandler(t, state)
				b := ownedRow(t, store, row.typ, row.labels, row.meta)
				convoy, err := store.Create(beads.Bead{Title: "c", Type: "convoy"})
				if err != nil {
					t.Fatal(err)
				}
				if r.tracked {
					if err := convoycore.TrackItem(store, convoy.ID, b.ID); err != nil {
						t.Fatal(err)
					}
				}
				if r.closed {
					if err := store.Close(b.ID); err != nil {
						t.Fatal(err)
					}
				}
				before, _ := store.Get(b.ID)
				all, _ := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
				tracked, _ := convoycore.HasTrack(store, convoy.ID, b.ID)

				rec := beadRequest(t, h, r.method, cityURL(state, r.fill(r.path, b.ID, convoy.ID)), r.fill(r.body, b.ID, convoy.ID))

				got, _ := store.Get(b.ID)
				after, _ := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
				stillTracked, _ := convoycore.HasTrack(store, convoy.ID, b.ID)
				switch {
				case rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "the generic bead API never"):
					t.Fatalf("status %d (%s), want 422 naming the owner", rec.Code, rec.Body.String())
				case row.typ == "task" && !strings.Contains(rec.Body.String(), "which only bd (bd update, bd close) writes"):
					t.Fatalf("body %s: a typed work bead carrying the session label names bd as its only writer", rec.Body.String())
				case strings.Join(problemLocations(t, rec), ",") != r.where:
					t.Fatalf("errors %v, want %s", problemLocations(t, rec), r.where)
				case !reflect.DeepEqual(got, before) || len(after) != len(all) || stillTracked != tracked:
					t.Fatalf("row %+v -> %+v, beads %d -> %d, tracked %t -> %t: want nothing written", before, got, len(all), len(after), tracked, stillTracked)
				}
			})
		}
	}
}

// TestGenericAPINeverMakesAControllerOwnedRow (architecture B): a create of
// a controller-owned row, or an update that would make a work bead one, by
// type (any case, any space) or by the session label, is refused with 422
// at the type or the label, and writes nothing.
func TestGenericAPINeverMakesAControllerOwnedRow(t *testing.T) {
	for _, c := range []struct {
		name, method, body, where string
	}{
		{"create a session row", "POST", `{"title":"s","type":"session"}`, "body.type"},
		{"create a 'SESSION' row", "POST", `{"title":"s","type":"SESSION"}`, "body.type"},
		{"create a session row by its label", "POST", `{"title":"s","type":"task","labels":["note","gc:session"]}`, "body.labels[1]"},
		{"create an untyped labeled row", "POST", `{"title":"s","type":"","labels":["gc:session"]}`, "body.labels[0]"},
		{"create a row labeled ' GC:Session '", "POST", `{"title":"s","type":"task","labels":[" GC:Session "]}`, "body.labels[0]"},
		{"create a startup-health episode", "POST", `{"title":"e","type":"startup-health-episode"}`, "body.type"},
		{"create a ' Startup-Health-Episode '", "POST", `{"title":"e","type":" Startup-Health-Episode "}`, "body.type"},
		{"create a convergence root", "POST", `{"title":"c","type":"convergence"}`, "body.type"},
		{"create a 'Convergence' root", "POST", `{"title":"c","type":"Convergence"}`, "body.type"},
		{"retype a task to a session", "PATCH", `{"type":"session"}`, "body.type"},
		{"retype a task to an episode", "PATCH", `{"type":"startup-health-episode"}`, "body.type"},
		{"retype a task to ' convergence '", "PATCH", `{"type":" convergence "}`, "body.type"},
		{"label a task a session", "PATCH", `{"labels":["note","gc:session"]}`, "body.labels[1]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			state := newFakeState(t)
			store := state.stores["myrig"]
			h := newTestCityHandler(t, state)
			work, err := store.Create(beads.Bead{Title: "w", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			url := cityURL(state, "/beads")
			if c.method == "PATCH" {
				url = cityURL(state, "/bead/") + work.ID
			}
			before, _ := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
			rec := beadRequest(t, h, c.method, url, c.body)
			after, _ := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
			got, _ := store.Get(work.ID)
			switch {
			case rec.Code != http.StatusUnprocessableEntity:
				t.Fatalf("status %d (%s), want 422", rec.Code, rec.Body.String())
			case strings.Join(problemLocations(t, rec), ",") != c.where:
				t.Fatalf("errors %v, want %s", problemLocations(t, rec), c.where)
			case len(after) != len(before) || got.Type != "task" || len(got.Labels) != 0:
				t.Fatalf("beads %d -> %d, work %q %v: want nothing written", len(before), len(after), got.Type, got.Labels)
			}
		})
	}
}

// TestGenericAPIWritesAWorkBead (architecture B): the rule is the owned
// row, never a field list: a work bead takes every route, the keys a
// session's disposition or a workflow reads included, and a task carrying a
// startup-health episode's keys is created and holds no session down.
func TestGenericAPIWritesAWorkBead(t *testing.T) {
	for _, r := range ownedRoutes {
		if strings.HasPrefix(r.path, "/sling") {
			continue // a sling's own answers are its tests'; here it is refused only on an owned row
		}
		t.Run(r.name, func(t *testing.T) {
			state := newFakeState(t)
			store := state.stores["myrig"]
			h := newTestCityHandler(t, state)
			work, err := store.Create(beads.Bead{Title: "w", Type: "task", Metadata: map[string]string{"wait_hold": "true"}})
			if err != nil {
				t.Fatal(err)
			}
			convoy, err := store.Create(beads.Bead{Title: "c", Type: "convoy"})
			if err != nil {
				t.Fatal(err)
			}
			if r.tracked {
				if err := convoycore.TrackItem(store, convoy.ID, work.ID); err != nil {
					t.Fatal(err)
				}
			}
			if r.closed {
				if err := store.Close(work.ID); err != nil {
					t.Fatal(err)
				}
			}
			body := r.fill(r.body, work.ID, convoy.ID)
			if r.name == "PATCH the session label removed" {
				body = `{"remove_labels":["note"]}`
			}
			if rec := beadRequest(t, h, r.method, cityURL(state, r.fill(r.path, work.ID, convoy.ID)), body); rec.Code >= 300 {
				t.Fatalf("status %d (%s), want the work bead written", rec.Code, rec.Body.String())
			}
		})
	}
	t.Run("a task carrying the episode's keys", func(t *testing.T) {
		state := newFakeState(t)
		store := state.stores["myrig"]
		h := newTestCityHandler(t, state)
		rec := beadRequest(t, h, "POST", cityURL(state, "/beads"), `{"title":"w","type":"task","metadata":{"`+session.StartupHealthSessionNameMetadataKey+`":"myrig--worker","`+session.StartupHealthQuarantinedUntilMetadataKey+`":"9999-01-01T00:00:00Z"}}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d (%s), want 201", rec.Code, rec.Body.String())
		}
		if ep, err := session.NewStore(beads.SessionStore{Store: store}).LoadStartupHealthEpisode("myrig--worker"); err != nil || !ep.QuarantinedUntil.IsZero() {
			t.Fatalf("episode %+v (%v): a task's keys hold no session down", ep, err)
		}
	})
}

// TestWorkflowTeardownNeverClosesAControllerOwnedRow (mc-dkblm, architecture
// B): a controller-owned row carrying a workflow's keys (written past the
// API) is never a workflow root to tear down nor a member to sweep: DELETE
// /workflow/{id}?delete=true, a graph convoy's DELETE and POST
// /runs/{id}/cancel leave it as it was, and so do a real workflow's and a
// real graph convoy's descendant sweeps, which close their own root.
func TestWorkflowTeardownNeverClosesAControllerOwnedRow(t *testing.T) {
	for _, row := range ownedRows {
		for _, c := range []struct{ name, method, path string }{
			{"DELETE /workflow/{row}?delete=true", "DELETE", "/workflow/%s?delete=true"},
			{"DELETE /convoy/{row}", "DELETE", "/convoy/%s"},
			{"POST /runs/{row}/cancel", "POST", "/runs/%s/cancel"},
		} {
			t.Run(row.name+"/"+c.name, func(t *testing.T) {
				withoutSessionWriteHooks(t)
				state := newFakeState(t)
				store := state.stores["myrig"]
				h := newTestCityHandler(t, state)
				b := ownedRow(t, store, row.typ, row.labels, map[string]string{"gc.kind": "workflow", "gc.workflow_id": "wf-1"})
				before, _ := store.Get(b.ID)
				beadRequest(t, h, c.method, cityURL(state, "")+strings.Replace(c.path, "%s", b.ID, 1), "")
				if got, err := store.Get(b.ID); err != nil || !reflect.DeepEqual(got, before) {
					t.Fatalf("row %+v -> %+v (%v), want it as it was", before, got, err)
				}
			})
		}
		for _, sweep := range []struct{ name, rootType, path string }{
			{"a real workflow's sweep", "task", "/workflow/%s?delete=true"},
			{"a real graph convoy's sweep", "convoy", "/convoy/%s"},
			{"a real run's cancel", "task", "/runs/%s/cancel"},
		} {
			t.Run(row.name+"/"+sweep.name, func(t *testing.T) {
				withoutSessionWriteHooks(t)
				state := newFakeState(t)
				store := state.stores["myrig"]
				h := newTestCityHandler(t, state)
				root, err := store.Create(beads.Bead{Title: "wf", Type: sweep.rootType, Metadata: map[string]string{"gc.kind": "workflow"}})
				if err != nil {
					t.Fatal(err)
				}
				b := ownedRow(t, store, row.typ, row.labels, map[string]string{"gc.root_bead_id": root.ID})
				before, _ := store.Get(b.ID)
				method := "DELETE"
				if strings.HasPrefix(sweep.path, "/runs") {
					method = "POST"
				}
				beadRequest(t, h, method, cityURL(state, "")+strings.Replace(sweep.path, "%s", root.ID, 1), "")
				if got, err := store.Get(b.ID); err != nil || !reflect.DeepEqual(got, before) {
					t.Fatalf("row %+v -> %+v (%v), want it as it was", before, got, err)
				}
				if got, err := store.Get(root.ID); err == nil && got.Status != "closed" {
					t.Fatalf("root %s, want the workflow itself torn down", got.Status)
				}
			})
		}
	}
}
