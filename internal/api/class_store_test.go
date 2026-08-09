package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// writeRoutesJSONL writes a routes.jsonl into scopeDir/.beads/, creating the
// directory. Lines are already-encoded JSON objects.
func writeRoutesJSONL(t *testing.T, scopeDir string, lines ...string) {
	t.Helper()
	beadsDir := filepath.Join(scopeDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", beadsDir, err)
	}
	var body string
	for _, line := range lines {
		body += line + "\n"
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile(%s/routes.jsonl): %v", beadsDir, err)
	}
}

// TestBeadStoresForIDDefaultBackendIsCityLed pins the single-store invariant:
// when the graph class is NOT relocated (GraphBeadStore() == CityBeadStore()),
// the class-prefix arm never fires, so the unrouted by-id candidate set leads
// with the city store ahead of the per-rig work stores — byte-identical to the
// pre-seam ordering.
func TestBeadStoresForIDDefaultBackendIsCityLed(t *testing.T) {
	st := newFakeState(t)
	city := beads.NewMemStore()
	st.cityBeadStore = city
	// Drop the rig store so the city store is the only by-id candidate.
	st.stores = map[string]beads.Store{}
	st.cfg.Rigs = nil
	s := New(st)

	got := s.beadStoresForID("gcg-1")
	if len(got) != 1 {
		t.Fatalf("beadStoresForID returned %d stores, want 1 (city-led, no graph arm); got %v", len(got), got)
	}
	if got[0] != city {
		t.Errorf("beadStoresForID[0] = %p, want CityBeadStore %p", got[0], city)
	}
}

// TestBeadStoresForIDClassAwareGraphArm pins the relocated-graph behavior: with a
// DISTINCT dedicated graph store, a graph-class id (reserved prefix "gcg") that is
// not reachable via a rig/HQ prefix resolves to [graph, work] — graph-first — so
// the by-id Get-then-mutate handler loop pins the graph store on the first probe.
// On a single-store city (graph == city) the arm is skipped, so this path stays
// byte-identical there (covered by TestBeadStoresForIDDefaultBackendIsCityLed).
func TestBeadStoresForIDClassAwareGraphArm(t *testing.T) {
	work := beads.NewMemStore()
	graph := beads.NewMemStore()

	st := newFakeState(t)
	st.cityBeadStore = work   // plain work store
	st.graphBeadStore = graph // dedicated, distinct graph store
	st.stores = nil
	st.cfg.Rigs = nil

	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatalf("ReservedClassPrefix(graph) returned ok=false; expected a reserved prefix")
	}
	s := New(st)

	got := s.beadStoresForID(prefix + "-1")
	if len(got) != 2 || got[0] != s.state.GraphBeadStore().Store || got[1] != s.state.CityBeadStore() {
		t.Fatalf("beadStoresForID(%s-1) = %v (len %d), want [graph, work]", prefix, got, len(got))
	}
}

// relocatedGraphRouteState builds a relocated-graph city with one rig, and
// returns the state plus the graph, city and rig stores. The graph store lives
// at <city>/.gc/infra, which is NOT a registered rig path.
func relocatedGraphRouteState(t *testing.T) (*fakeState, beads.Store, beads.Store, beads.Store) {
	t.Helper()
	st := newFakeState(t)
	city := beads.NewMemStore()
	graph := beads.NewMemStore()
	rig := beads.NewMemStore()
	st.cityBeadStore = city
	st.graphBeadStore = graph
	st.stores = map[string]beads.Store{"myrig": rig}
	st.cfg.Rigs = []config.Rig{{Name: "myrig", Path: filepath.Join(st.cityPath, "rigs", "myrig")}}
	return st, graph, city, rig
}

// TestBeadStoresForIDClassArmBeatsRigRouteCapture pins the ordering the split
// city depends on: a rig routes.jsonl entry for the reserved graph prefix
// resolves to the relocated graph directory, which is not a rig, so the rig
// must NOT answer for it — the class arm must.
func TestBeadStoresForIDClassArmBeatsRigRouteCapture(t *testing.T) {
	st, graph, city, rig := relocatedGraphRouteState(t)
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatalf("ReservedClassPrefix(graph) returned ok=false; expected a reserved prefix")
	}
	// The rig's routes.jsonl routes the graph class OUT of the rig, to the
	// relocated graph store directory.
	writeRoutesJSONL(t, st.cfg.Rigs[0].Path,
		`{"prefix":"mr","path":"."}`,
		`{"prefix":"`+prefix+`","path":"../../.gc/infra"}`,
	)

	s := New(st)
	got := s.beadStoresForID(prefix + "-1")
	if len(got) != 2 || got[0] != graph || got[1] != city {
		t.Fatalf("beadStoresForID(%s-1) = %v (len %d), want [graph, city]; rig store is %p", prefix, got, len(got), rig)
	}
}

// TestBeadStoresForIDClassArmBeatsCityRouteCapture pins the same ordering for a
// city-scope route: a routes.jsonl entry that resolves the reserved graph prefix
// back to the city directory must not hand graph-class ids to the city work
// store — the class arm owns reserved prefixes.
func TestBeadStoresForIDClassArmBeatsCityRouteCapture(t *testing.T) {
	st, graph, city, _ := relocatedGraphRouteState(t)
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatalf("ReservedClassPrefix(graph) returned ok=false; expected a reserved prefix")
	}
	writeRoutesJSONL(t, st.cityPath, `{"prefix":"`+prefix+`","path":"."}`)

	s := New(st)
	got := s.beadStoresForID(prefix + "-1")
	if len(got) != 2 || got[0] != graph || got[1] != city {
		t.Fatalf("beadStoresForID(%s-1) = %v (len %d), want [graph, city]; graph is %p, city is %p", prefix, got, len(got), graph, city)
	}
}

// TestBeadStoresForIDClassArmBeatsShadowingRigPrefix pins the ordering against
// the other way a work store can name a reserved prefix: a rig configured with
// the class prefix itself. config.ReservedPrefixWarnings allows that today but
// documents the class store as the owner once relocation is active, so on a
// relocated city the class arm — not the shadowing rig — answers FIRST.
//
// The shadowing rig store still has to be IN the list, behind the class store.
// The prefix is warned-and-allowed, not rejected (config.ValidateRigs lets it
// through), so that rig's beads are real; a list that dropped it made every one
// of them unreachable by id the moment graph relocated. That was carried minor
// (a) from PR #5128's council, and this is where it is closed.
func TestBeadStoresForIDClassArmBeatsShadowingRigPrefix(t *testing.T) {
	st, graph, city, rig := relocatedGraphRouteState(t)
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatalf("ReservedClassPrefix(graph) returned ok=false; expected a reserved prefix")
	}
	st.cfg.Rigs[0].Prefix = prefix

	s := New(st)
	got := s.beadStoresForID(prefix + "-1")
	if len(got) != 3 || got[0] != graph || got[1] != rig || got[2] != city {
		t.Fatalf("beadStoresForID(%s-1) = %v (len %d), want [graph, rig, city]; graph is %p, rig is %p, city is %p", prefix, got, len(got), graph, rig, city)
	}
}

// TestBeadStoresForIDClassArmKeepsLongerRigPrefix closes carried minor (b): an
// id under a LONGER configured prefix that starts with the reserved one is
// inside the class namespace by the exact-or-hyphen rule, so the class arm
// fires — and used to return [graph, city], silently losing the rig store that
// declares the longer prefix and actually mints the id.
//
// The rig sorts ahead of the city store because it is the more specific
// declared owner, and behind the class store, which is never captured.
func TestBeadStoresForIDClassArmKeepsLongerRigPrefix(t *testing.T) {
	st, graph, city, rig := relocatedGraphRouteState(t)
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatalf("ReservedClassPrefix(graph) returned ok=false; expected a reserved prefix")
	}
	longer := prefix + "-alpha"
	st.cfg.Rigs[0].Prefix = longer

	s := New(st)
	got := s.beadStoresForID(longer + "-1")
	if len(got) != 3 || got[0] != graph || got[1] != rig || got[2] != city {
		t.Fatalf("beadStoresForID(%s-1) = %v (len %d), want [graph, rig, city]; graph is %p, rig is %p, city is %p", longer, got, len(got), graph, rig, city)
	}
}

// TestBeadGetResolvesAShadowingRigID is the mutation proof behind the two
// carried minors: a bead that exists ONLY in a rig whose configured prefix sits
// inside the relocated class namespace must still resolve through the handler
// the by-id candidate list feeds. Before the fix the rig store was not a
// candidate at all, so this read 404'd.
func TestBeadGetResolvesAShadowingRigID(t *testing.T) {
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatalf("ReservedClassPrefix(graph) returned ok=false; expected a reserved prefix")
	}
	for _, rigPrefix := range []string{prefix, prefix + "-alpha"} {
		t.Run(rigPrefix, func(t *testing.T) {
			st, graph, city, rig := relocatedGraphRouteState(t)
			st.cfg.Rigs[0].Prefix = rigPrefix
			memRig, isMem := rig.(*beads.MemStore)
			if !isMem {
				t.Fatalf("fixture rig store is %T, want *beads.MemStore so the test can pin its minted prefix", rig)
			}
			memRig.IDPrefix = rigPrefix
			created, err := memRig.Create(beads.Bead{Title: "rig work bead in the class namespace"})
			if err != nil {
				t.Fatalf("seeding the rig store: %v", err)
			}
			for name, other := range map[string]beads.Store{"graph": graph, "city": city} {
				if _, err := other.Get(created.ID); err == nil {
					t.Fatalf("the %s store also holds %s; the fixture proves nothing", name, created.ID)
				}
			}

			out, err := New(st).humaHandleBeadGet(context.Background(), &BeadGetInput{ID: created.ID})
			if err != nil {
				t.Fatalf("GET bead %s: %v — a shadowing rig's bead is unreachable by id on a relocated city", created.ID, err)
			}
			if out.Body.ID != created.ID {
				t.Fatalf("resolved bead id = %q, want %q", out.Body.ID, created.ID)
			}
		})
	}
}

// TestBeadStoresForIDShadowingRigPrefixStillWinsOnDefaultCity pins the other
// side of that rule: with no relocation (GraphBeadStore() == CityBeadStore())
// the class arm never fires, so a rig configured with the reserved prefix keeps
// owning those ids exactly as it does today.
func TestBeadStoresForIDShadowingRigPrefixStillWinsOnDefaultCity(t *testing.T) {
	st := newFakeState(t)
	city := beads.NewMemStore()
	rig := beads.NewMemStore()
	st.cityBeadStore = city
	st.stores = map[string]beads.Store{"myrig": rig}
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatalf("ReservedClassPrefix(graph) returned ok=false; expected a reserved prefix")
	}
	st.cfg.Rigs = []config.Rig{{Name: "myrig", Path: filepath.Join(st.cityPath, "rigs", "myrig"), Prefix: prefix}}

	s := New(st)
	got := s.beadStoresForID(prefix + "-1")
	if len(got) != 1 || got[0] != rig {
		t.Fatalf("beadStoresForID(%s-1) = %v (len %d), want only the shadowing rig store %p", prefix, got, len(got), rig)
	}
}

// TestBeadGetResolvesARelocatedGraphID is the evidence behind the one command
// beads.RelocatedClassRefusal tells an operator to run.
//
// That refusal fires when a bd-ledger read names a relocated class's id
// namespace, and it has to name a read that DOES resolve such an id. It used to
// name `gc bd show` / `gc bd dep tree`, which are raw bd passthroughs against
// the same blind ledger — following the advice reproduced the bug being
// reported. The verb it names now, `gc beads show <id>`, routes through this
// handler (GET /v0/city/{cityName}/bead/{id}), so this test drives the handler
// end to end against a bead that exists ONLY in the relocated graph store. If it
// ever stops resolving, the refusal is giving bad advice again.
func TestBeadGetResolvesARelocatedGraphID(t *testing.T) {
	work := beads.NewMemStore()
	graph := beads.NewMemStore()

	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatalf("ReservedClassPrefix(graph) returned ok=false; expected a reserved prefix")
	}
	graph.IDPrefix = prefix
	created, err := graph.Create(beads.Bead{Title: "molecule root"})
	if err != nil {
		t.Fatalf("seeding the graph store: %v", err)
	}
	relocatedID := created.ID
	if _, err := work.Get(relocatedID); err == nil {
		t.Fatalf("the work store holds %s; the fixture proves nothing", relocatedID)
	}

	st := newFakeState(t)
	st.cityBeadStore = work
	st.graphBeadStore = graph
	st.stores = nil
	st.cfg.Rigs = nil

	out, err := New(st).humaHandleBeadGet(context.Background(), &BeadGetInput{ID: relocatedID})
	if err != nil {
		t.Fatalf("GET /v0/city/{cityName}/bead/%s: %v — the verb the refusal recommends cannot resolve a relocated id", relocatedID, err)
	}
	if out.Body.ID != relocatedID {
		t.Fatalf("resolved bead id = %q, want %q", out.Body.ID, relocatedID)
	}
}
