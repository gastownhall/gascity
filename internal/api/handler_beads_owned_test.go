package api

import (
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/session"
)

// ownedRows are the controller-owned rows (coordclass.ControllerOwned;
// architecture B), each spelling the generic API must take for one: a session
// row, typed, crash-damaged (no type, the session label) or a typed bead
// carrying the label; a startup-health episode; a convergence root; an
// operation-journal op or fact row (type op); and a type spelled in another
// case or with space around it.
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
	{"operation-journal op row", "op", []string{"gc:op", "gc:op-conv:gc-7"}, map[string]string{"gc.op.status": "submitted", "gc.op.open": "gc-7"}},
	{"operation-journal fact row", "op", []string{"gc:fact", "gc:fact-conv:gc-7"}, nil},
	{"op row spelled ' OP '", " OP ", []string{"gc:op"}, nil},
}

// coordinationRows are the coordination-owned rows (coordclass.
// CoordinationOwned; architecture B v2, mc-v8g4p) in each spelling the
// generic API must take for one: a mail message, a wait by gc:wait and by its
// typed session:<id> shape, a nudge shadow and a queued nudge, an order's
// tracking and run labels, and an extmsg row. Teardowns do not skip them (an
// order's wisp root carries its run label), so only the write tables do.
var coordinationRows = []struct {
	name   string
	typ    string
	labels []string
	meta   map[string]string
}{
	{"mail message", "message", nil, map[string]string{"from": "mayor"}},
	{"message spelled ' Message '", " Message ", nil, nil},
	{"wait by gc:wait", "gate", []string{"gc:wait", "session:s-1"}, map[string]string{"session_id": "s-1", "state": "pending"}},
	{"wait by its session label", "gate", []string{"session:s-1"}, map[string]string{"session_id": "s-1"}},
	{"nudge shadow", "chore", []string{"gc:nudge"}, nil},
	{"queued nudge", "task", []string{"GC:Nudge-Queue", "agent:myrig/worker"}, nil},
	{"order tracking", "task", []string{"order-tracking", "order-run:digest"}, nil},
	{"order run label alone", "task", []string{"order-run:digest"}, nil},
	{"extmsg binding", "task", []string{"gc:extmsg-binding"}, nil},
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
	for _, row := range append(slices.Clone(ownedRows), coordinationRows...) {
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
				case row.typ == "task" && slices.Contains(row.labels, session.LabelSession) && !strings.Contains(rec.Body.String(), "which only bd (bd update, bd close) writes"):
					t.Fatalf("body %s: a typed work bead carrying the session label names bd as its only writer", rec.Body.String())
				case strings.EqualFold(strings.TrimSpace(row.typ), "op") && !strings.Contains(rec.Body.String(), "operation-journal row, which only the operation journal"):
					t.Fatalf("body %s: an op row names the operation journal as its only writer", rec.Body.String())
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
		{"create an op row", "POST", `{"title":"o","type":"op","labels":["gc:op"]}`, "body.type"},
		{"retype a task to ' Op '", "PATCH", `{"type":" Op "}`, "body.type"},
		{"retype a task to a session", "PATCH", `{"type":"session"}`, "body.type"},
		{"retype a task to an episode", "PATCH", `{"type":"startup-health-episode"}`, "body.type"},
		{"retype a task to ' convergence '", "PATCH", `{"type":" convergence "}`, "body.type"},
		{"label a task a session", "PATCH", `{"labels":["note","gc:session"]}`, "body.labels[1]"},
		{"create a mail message", "POST", `{"title":"m","type":"message","metadata":{"from":"mayor"}}`, "body.type"},
		{"create a ' MESSAGE '", "POST", `{"title":"m","type":" MESSAGE "}`, "body.type"},
		{"create a wait by gc:wait", "POST", `{"title":"w","type":"task","labels":["gc:wait"]}`, "body.labels[0]"},
		{"create a wait by its session label", "POST", `{"title":"w","type":"gate","labels":["note","session:s-1"],"metadata":{"session_id":"s-1"}}`, "body.labels[1]"},
		{"create a nudge shadow", "POST", `{"title":"n","type":"chore","labels":["gc:nudge"]}`, "body.labels[0]"},
		{"create a queued nudge", "POST", `{"title":"n","type":"task","labels":["agent:myrig/worker"," gc:nudge-queue "]}`, "body.labels[1]"},
		{"create an order run record", "POST", `{"title":"o","type":"task","labels":["Order-Run:digest"]}`, "body.labels[0]"},
		{"create order tracking", "POST", `{"title":"o","type":"task","labels":["order-tracking"]}`, "body.labels[0]"},
		{"create an extmsg row", "POST", `{"title":"x","type":"task","labels":["gc:extmsg-delivery"]}`, "body.labels[0]"},
		{"retype a task to a message", "PATCH", `{"type":"message"}`, "body.type"},
		{"label a task an order run", "PATCH", `{"labels":["order-run:digest"]}`, "body.labels[0]"},
		{"label a task a wait", "PATCH", `{"labels":["gc:wait"]}`, "body.labels[0]"},
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

// TestGenericAPINeverMakesAWaitBySessionID (architecture B v2): a gate
// carrying a session:<id> label becomes a wait when its session_id is set
// (session.IsWaitBead), so that update is refused at body.metadata.session_id
// and writes nothing; a gate step, a task with a session label and a gate
// naming another session take the generic API.
func TestGenericAPINeverMakesAWaitBySessionID(t *testing.T) {
	for _, c := range []struct {
		name, typ string
		labels    []string
		body      string
		want      int
	}{
		{"a gate given its session_id", "gate", []string{"session:s-1"}, `{"metadata":{"session_id":"s-1"}}`, http.StatusUnprocessableEntity},
		{"a gate step", "gate", nil, `{"metadata":{"note":"x"}}`, http.StatusOK},
		{"a task with a session label given a session_id", "task", []string{"session:s-1"}, `{"metadata":{"session_id":"s-1"}}`, http.StatusOK},
		{"a gate given another session's id", "gate", []string{"session:s-2"}, `{"metadata":{"session_id":"s-1"}}`, http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			state := newFakeState(t)
			store := state.stores["myrig"]
			h := newTestCityHandler(t, state)
			b := ownedRow(t, store, c.typ, c.labels, nil)
			rec := beadRequest(t, h, "PATCH", cityURL(state, "/bead/")+b.ID, c.body)
			got, _ := store.Get(b.ID)
			if rec.Code != c.want {
				t.Fatalf("status %d (%s), want %d", rec.Code, rec.Body.String(), c.want)
			}
			if c.want == http.StatusUnprocessableEntity && (strings.Join(problemLocations(t, rec), ",") != "body.metadata.session_id" || got.Metadata["session_id"] != "") {
				t.Fatalf("errors %v, metadata %v: want body.metadata.session_id and nothing written", problemLocations(t, rec), got.Metadata)
			}
		})
	}
}

// TestGenericAPIClosesAnOrderRunRoot (v5.10f, the #7766 review): an order's
// run root, a wisp or molecule root carrying its order-run label, takes the
// generic close (POST /close, DELETE), as the dashboard's Beads page closes
// it; every other generic write to it stays refused and points at bd or the
// workflow's teardown. Its tracking bead, a plain task carrying the run
// label, and a root also carrying another owned mark (a queued nudge's, the
// session label) take no close.
func TestGenericAPIClosesAnOrderRunRoot(t *testing.T) {
	for _, c := range []struct{ name, method, path, body string }{
		{"POST /close", "POST", "/close", `{}`},
		{"DELETE", "DELETE", "", ""},
	} {
		for _, root := range []struct {
			typ  string
			meta map[string]string
		}{{"molecule", nil}, {"wisp", nil}, {"task", map[string]string{"gc.kind": "wisp"}}, {"task", map[string]string{"gc.kind": "workflow"}}} {
			state := newFakeState(t)
			store := state.stores["myrig"]
			h := newTestCityHandler(t, state)
			b := ownedRow(t, store, root.typ, []string{"order-run:digest"}, root.meta)
			if rec := beadRequest(t, h, c.method, cityURL(state, "/bead/")+b.ID+c.path, c.body); rec.Code >= 300 {
				t.Fatalf("%s on a %s run root: %d %s, want it closed", c.name, root.typ, rec.Code, rec.Body.String())
			}
			if got, _ := store.Get(b.ID); got.Status != "closed" {
				t.Fatalf("%s on a %s run root: status %s, want closed", c.name, root.typ, got.Status)
			}
		}
		for _, other := range []struct {
			name   string
			typ    string
			labels []string
		}{{"tracking bead", "task", []string{"order-run:digest", "order-tracking"}}, {"plain task", "task", []string{"order-run:digest"}}, {"root that is also a queued nudge", "molecule", []string{"order-run:digest", "gc:nudge"}}, {"root carrying the session label", "molecule", []string{"order-run:digest", "gc:session"}}} {
			state := newFakeState(t)
			store := state.stores["myrig"]
			h := newTestCityHandler(t, state)
			b := ownedRow(t, store, other.typ, other.labels, nil)
			if rec := beadRequest(t, h, c.method, cityURL(state, "/bead/")+b.ID+c.path, c.body); rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("%s on a %s: %d %s, want 422", c.name, other.name, rec.Code, rec.Body.String())
			}
		}
	}
	for _, c := range []struct{ name, method, path, body string }{
		{"PATCH status=closed", "PATCH", "", `{"status":"closed"}`},
		{"PATCH a label", "PATCH", "", `{"labels":["note"]}`},
		{"POST /assign", "POST", "/assign", `{"assignee":"someone"}`},
		{"POST /reopen", "POST", "/reopen", `{}`},
	} {
		state := newFakeState(t)
		store := state.stores["myrig"]
		h := newTestCityHandler(t, state)
		b := ownedRow(t, store, "molecule", []string{"order-run:digest"}, nil)
		if c.name == "POST /reopen" {
			if err := store.Close(b.ID); err != nil {
				t.Fatal(err)
			}
		}
		rec := beadRequest(t, h, c.method, cityURL(state, "/bead/")+b.ID+c.path, c.body)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "workflow's teardown") {
			t.Fatalf("%s on a run root: %d %s, want 422 pointing at bd or the workflow's teardown", c.name, rec.Code, rec.Body.String())
		}
	}
}

// TestGenericAPIRefusalNamesTheOwner (the #7766 review, M30-M31): each
// coordination row's refusal names its kind and its owner, so a caller is
// sent to the route that writes it.
func TestGenericAPIRefusalNamesTheOwner(t *testing.T) {
	for _, c := range []struct {
		typ    string
		labels []string
		meta   map[string]string
		kind   string
		owner  string
	}{
		{"message", nil, nil, "a mail message", "/mail"},
		{"gate", []string{"gc:wait"}, nil, "a session wait", "gc session wait"},
		{"gate", []string{"session:s-1"}, map[string]string{"session_id": "s-1"}, "a session wait", "gc session wait"},
		{"chore", []string{"gc:nudge"}, nil, "a queued nudge", "the nudge queue"},
		{"task", []string{"gc:nudge-queue"}, nil, "a queued nudge", "the nudge queue"},
		{"task", []string{"order-tracking", "order-run:digest"}, nil, "an order run record", "workflow's teardown"},
		{"task", []string{"gc:extmsg-binding"}, nil, "an external-messaging row", "/extmsg/"},
	} {
		state := newFakeState(t)
		store := state.stores["myrig"]
		h := newTestCityHandler(t, state)
		b := ownedRow(t, store, c.typ, c.labels, c.meta)
		rec := beadRequest(t, h, "PATCH", cityURL(state, "/bead/")+b.ID, `{"title":"renamed"}`)
		body := rec.Body.String()
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "is "+c.kind+",") || !strings.Contains(body, c.owner) {
			t.Fatalf("%s %v: %d %s, want 422 naming %q and %q", c.typ, c.labels, rec.Code, body, c.kind, c.owner)
		}
	}
}
