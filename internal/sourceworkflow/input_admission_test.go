package sourceworkflow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

func TestInputAdmissionCurrentRelations(t *testing.T) {
	for _, c := range []struct {
		name          string
		split, cached bool
	}{
		{"unified", false, false},
		{"split", true, false},
		{"cached unified", false, true},
		{"cached split", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			work := beads.NewMemStore()
			work.HonorExplicitIDs = true
			graph := work
			if c.split {
				graph = beads.NewMemStore()
				graph.HonorExplicitIDs = true
			}
			create := func(s beads.Store, b beads.Bead) beads.Bead {
				t.Helper()
				got, err := s.Create(b)
				if err != nil {
					t.Fatal(err)
				}
				return got
			}
			dep := func(s beads.Store, from, to, kind string) {
				t.Helper()
				if err := s.DepAdd(from, to, kind); err != nil {
					t.Fatal(err)
				}
			}
			input := create(work, beads.Bead{ID: "work-input", Title: "input"})
			conv := create(work, beads.Bead{ID: "work-convoy", Title: "inputs", Type: "convoy"})
			dep(work, conv.ID, input.ID, "tracks")
			root := create(graph, beads.Bead{ID: "graph-root", Title: "workflow", Metadata: map[string]string{"gc.kind": "workflow", "gc.input_convoy_id": conv.ID}})
			step := create(graph, beads.Bead{ID: "graph-step", Title: "early step", Metadata: map[string]string{"gc.root_bead_id": root.ID}})
			var workRead, graphRead beads.Store = work, graph
			if c.cached {
				cache := func(store beads.Store) *beads.CachingStore {
					t.Helper()
					cached := beads.NewCachingStoreForTest(store, nil)
					if err := cached.Prime(context.Background()); err != nil {
						t.Fatal(err)
					}
					return cached
				}
				workRead = cache(work)
				graphRead = workRead
				if c.split {
					graphRead = cache(graph)
				}
			}
			stores := InputStores{Graph: beadstest.Graph(graphRead), Work: []beads.WorkStore{beadstest.Work(workRead)}}
			check := func(want bool) {
				t.Helper()
				got, err := stores.Admission(context.Background(), step)
				if err != nil || got.Ready != want {
					t.Fatalf("Admission = %+v, %v; want ready=%v", got, err, want)
				}
			}
			check(true) // An open input need not close before its report runs.
			gate := create(work, beads.Bead{ID: "work-final-gate", Title: "unrelated final gate", Status: "blocked"})
			check(true) // No parent/sibling scan: an unrelated gate cannot hold planning.
			dep(work, input.ID, gate.ID, "blocks")
			check(false) // Added after materialization.
			if err := work.DepRemove(input.ID, gate.ID); err != nil {
				t.Fatal(err)
			}
			check(true)
			second := create(work, beads.Bead{ID: "work-second", Title: "second", Labels: []string{"hold:external"}})
			dep(work, conv.ID, second.ID, "tracks")
			check(false) // Every current nonterminal member is required.
			if err := work.DepRemove(conv.ID, second.ID); err != nil {
				t.Fatal(err)
			}
			check(true)
			dep(work, conv.ID, second.ID, "tracks")
			if err := work.Close(second.ID); err != nil {
				t.Fatal(err)
			}
			check(true) // Terminal members use convoy terminal semantics.
			dep(work, conv.ID, gate.ID, "blocks")
			check(false) // The convoy's own prerequisites are also required.
			replacement := create(work, beads.Bead{ID: "work-replacement", Title: "replacement inputs", Type: "convoy"})
			if err := graph.SetMetadata(root.ID, "gc.input_convoy_id", replacement.ID); err != nil {
				t.Fatal(err)
			}
			check(true) // The root's current input relation must also bypass caches.
		})
	}
}

func TestInputAdmissionLifecycleAndTerminalConvoy(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "convoy", Type: "convoy", Status: "closed"},
		{ID: "input", Type: "task", Status: "open", Labels: []string{"hold:external"}},
		{ID: "root", Type: "molecule", Status: "open", Metadata: map[string]string{"gc.input_convoy_id": "convoy"}},
		{ID: "step", Type: "task", Status: "open", Metadata: map[string]string{"gc.root_bead_id": "root"}},
	}, []beads.Dep{{IssueID: "convoy", DependsOnID: "input", Type: "tracks"}})
	stores := InputStores{Graph: beadstest.Graph(store), Work: []beads.WorkStore{beadstest.Work(store)}}
	step, err := store.Get("step")
	if err != nil {
		t.Fatal(err)
	}
	got, err := stores.Admission(context.Background(), step)
	if err != nil || got.Ready {
		t.Fatalf("closed convoy hid its held member: %+v, %v", got, err)
	}
	for _, kind := range []string{"workflow-finalize", "teardown", "terminal root", "cancel intent"} {
		t.Run(kind, func(t *testing.T) {
			candidate := beads.Bead{ID: "cleanup", Metadata: map[string]string{"gc.root_bead_id": "root"}}
			switch kind {
			case "workflow-finalize":
				candidate.Metadata["gc.kind"] = kind
			case "teardown":
				candidate.Metadata["gc.scope_role"] = kind
			case "terminal root":
				if err := store.Close("root"); err != nil {
					t.Fatal(err)
				}
			case "cancel intent":
				if err := store.Reopen("root"); err != nil {
					t.Fatal(err)
				}
				if err := store.SetMetadata("root", "gc.cancel_requested", "true"); err != nil {
					t.Fatal(err)
				}
			}
			// No work handle: lifecycle admission cannot depend on reading the
			// input, even while its owner is unavailable.
			got, err := (InputStores{Graph: stores.Graph, OpenWork: func(context.Context) ([]beads.WorkStore, error) {
				t.Fatal("lifecycle opened work stores")
				return nil, errors.New("unavailable")
			}}).Admission(context.Background(), candidate)
			if err != nil || !got.Ready {
				t.Fatalf("lifecycle blocked: %+v, %v", got, err)
			}
		})
	}
}

func TestInputAdmissionParkedAndUnknown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input beads.Bead
	}{
		{"blocked", beads.Bead{Status: "blocked"}},
		{"normalized blocked", beads.Bead{SourceStatus: "blocked"}},
		{"deferred", beads.Bead{Status: "deferred"}},
		{"normalized deferred", beads.Bead{IndefinitelyDeferred: true}},
		{"future deferral", beads.Bead{DeferUntil: timeRef(time.Now().Add(time.Hour))}},
		{"external hold", beads.Bead{Labels: []string{"hold:external"}}},
		{"operator hold", beads.Bead{Labels: []string{"hold:mayor"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			tc.input.ID, tc.input.Title = "input", "input"
			input, err := store.Create(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if tc.input.Status != "" {
				if err := store.Update(input.ID, beads.UpdateOpts{Status: &tc.input.Status}); err != nil {
					t.Fatal(err)
				}
			}
			conv, err := store.Create(beads.Bead{ID: "convoy", Type: "convoy", Title: "inputs"})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.DepAdd(conv.ID, input.ID, "tracks"); err != nil {
				t.Fatal(err)
			}
			root, err := store.Create(beads.Bead{ID: "root", Title: "workflow", Metadata: map[string]string{"gc.input_convoy_id": conv.ID}})
			if err != nil {
				t.Fatal(err)
			}
			stores := InputStores{Graph: beadstest.Graph(store), Work: []beads.WorkStore{beadstest.Work(store)}}
			got, err := stores.Admission(context.Background(), root)
			if err != nil || got.Ready || got.Reason == "" {
				t.Fatalf("Admission = %+v, %v", got, err)
			}
		})
	}
}

func timeRef(t time.Time) *time.Time { return &t }

type admissionFaultStore struct {
	beads.Store
	failID   string
	failDeps bool
}

func (s admissionFaultStore) Get(id string) (beads.Bead, error) {
	if id == s.failID {
		return beads.Bead{}, errors.New("fixture read failure")
	}
	return s.Store.Get(id)
}

func (s admissionFaultStore) DepList(id, direction string) ([]beads.Dep, error) {
	if s.failDeps {
		return nil, errors.New("fixture relation failure")
	}
	return s.Store.DepList(id, direction)
}

func TestInputAdmissionRelationFailures(t *testing.T) {
	for _, failure := range []string{"missing convoy", "missing member", "read", "relation", "cycle"} {
		t.Run(failure, func(t *testing.T) {
			mem := beads.NewMemStore()
			conv, err := mem.Create(beads.Bead{ID: "convoy", Type: "convoy", Title: "inputs"})
			if err != nil {
				t.Fatal(err)
			}
			root, err := mem.Create(beads.Bead{ID: "root", Title: "workflow", Metadata: map[string]string{"gc.input_convoy_id": conv.ID}})
			if err != nil {
				t.Fatal(err)
			}
			fault := admissionFaultStore{Store: mem}
			switch failure {
			case "missing convoy":
				if err := mem.SetMetadata(root.ID, "gc.input_convoy_id", "absent"); err != nil {
					t.Fatal(err)
				}
			case "missing member":
				if err := mem.DepAdd(conv.ID, "absent", "tracks"); err != nil {
					t.Fatal(err)
				}
			case "read":
				fault.failID = conv.ID
			case "relation":
				fault.failDeps = true
			case "cycle":
				if err := mem.DepAdd(conv.ID, conv.ID, "tracks"); err != nil {
					t.Fatal(err)
				}
			}
			got, err := (InputStores{Graph: beadstest.Graph(mem), Work: []beads.WorkStore{beadstest.Work(fault)}}).Admission(context.Background(), root)
			if err == nil || got.Ready {
				t.Fatalf("Admission = %+v, %v; want explicit error", got, err)
			}
			if (failure == "read" || failure == "relation") && !strings.Contains(err.Error(), "fixture") {
				t.Fatal(err)
			}
		})
	}
}

func TestInputAdmissionStructuralExpansion(t *testing.T) {
	for _, kind := range []string{"drain", "fanout", "check", ""} {
		t.Run(kind, func(t *testing.T) {
			work := beads.NewMemStoreFrom(0, []beads.Bead{
				{ID: "convoy", Type: "convoy", Status: "open"},
				{ID: "a", Status: "open"},
				{ID: "b", Status: "open", Labels: []string{"hold:external"}},
			}, []beads.Dep{
				{IssueID: "convoy", DependsOnID: "a", Type: "tracks"},
				{IssueID: "convoy", DependsOnID: "b", Type: "tracks"},
				{IssueID: "b", DependsOnID: "a", Type: "blocks"},
			})
			graph := beads.NewMemStoreFrom(0, []beads.Bead{
				{ID: "root", Status: "open", Metadata: map[string]string{"gc.input_convoy_id": "convoy"}},
				{ID: "step", Status: "open", Metadata: map[string]string{"gc.root_bead_id": "root", "gc.kind": kind}},
			}, nil)
			stores := InputStores{Graph: beadstest.Graph(graph), Work: []beads.WorkStore{beadstest.Work(work)}}
			step, _ := graph.Get("step")
			structural := kind == "drain" || kind == "fanout"
			check := func(ready bool, wantErr bool) {
				t.Helper()
				got, err := stores.Admission(context.Background(), step)
				if got.Ready != ready || (err != nil) != wantErr {
					t.Fatalf("Admission = %+v, %v; want ready=%v error=%v", got, err, ready, wantErr)
				}
			}
			check(structural, false)
			for _, id := range []string{"convoy", "root", "step"} {
				target := work
				if id != "convoy" {
					target = graph
				}
				if err := target.Update(id, beads.UpdateOpts{Labels: []string{"hold:external"}}); err != nil {
					t.Fatal(err)
				}
				step, _ = graph.Get("step")
				check(false, false)
				if err := target.Update(id, beads.UpdateOpts{RemoveLabels: []string{"hold:external"}}); err != nil {
					t.Fatal(err)
				}
				step, _ = graph.Get("step")
			}
			if err := work.DepAdd("convoy", "a", "blocks"); err != nil {
				t.Fatal(err)
			}
			check(false, false)
			if err := work.DepRemove("convoy", "a"); err != nil {
				t.Fatal(err)
			}
			if err := work.DepAdd("convoy", "missing", "tracks"); err != nil {
				t.Fatal(err)
			}
			check(false, true)
			if err := work.DepRemove("convoy", "missing"); err != nil {
				t.Fatal(err)
			}
			if err := work.DepAdd("a", "b", "blocks"); err != nil {
				t.Fatal(err)
			}
			check(false, true)
		})
	}
}

// Only operation states that cannot expand or advance work bypass input reads.
// Root/control parks still fence graph-only settlement under an active root.
func TestInputAdmissionSettlementBoundary(t *testing.T) {
	for _, tc := range []struct {
		kind, key, state string
		settlement       bool
	}{
		{"drain", "gc.drain_state", "succeeded", true},
		{"drain", "gc.drain_state", "failed", true},
		{"fanout", "gc.fanout_state", "spawned", true},
		{"drain", "gc.drain_state", "expanded", false},
		{"drain", "gc.drain_state", "completing", false},
		{"drain", "gc.drain_state", "expanding", false},
		{"fanout", "gc.fanout_state", "spawning", false},
	} {
		t.Run(tc.kind+"/"+tc.state, func(t *testing.T) {
			graph := beads.NewMemStoreFrom(0, []beads.Bead{
				{ID: "root", Status: "open", Metadata: map[string]string{"gc.input_convoy_id": "absent"}},
				{ID: "control", Status: "open", Metadata: map[string]string{"gc.root_bead_id": "root", "gc.kind": tc.kind, tc.key: tc.state, "gc.outcome": "pass", "gc.drain_context": "shared"}},
			}, nil)
			opens := 0
			stores := InputStores{Graph: beadstest.Graph(graph), OpenWork: func(context.Context) ([]beads.WorkStore, error) { opens++; return nil, errors.New("work unavailable") }}
			control, _ := graph.Get("control")
			got, err := stores.Admission(context.Background(), control)
			if tc.settlement {
				if err != nil || !got.Ready || opens != 0 {
					t.Fatalf("settlement: %+v, %v, opens=%d", got, err, opens)
				}
				for _, id := range []string{"root", "control"} {
					if err := graph.Update(id, beads.UpdateOpts{Labels: []string{"hold:external"}}); err != nil {
						t.Fatal(err)
					}
					control, _ = graph.Get("control")
					got, err = stores.Admission(context.Background(), control)
					if err != nil || got.Ready || opens != 0 {
						t.Fatalf("held %s: %+v, %v, opens=%d", id, got, err, opens)
					}
					if err := graph.Update(id, beads.UpdateOpts{RemoveLabels: []string{"hold:external"}}); err != nil {
						t.Fatal(err)
					}
				}
			} else if err == nil || got.Ready || opens != 1 {
				t.Fatalf("expansion bypassed input read: %+v, %v, opens=%d", got, err, opens)
			}
		})
	}
}
