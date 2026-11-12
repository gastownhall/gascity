package api

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storeref"
)

// The T7b.3 equivalence table, live since T7e.2: the API's WorkFederation leg
// builders (beadListFanOut, statusWorkQueries, retiredSessionSweep over
// storeref.Sweep), run beside main's frozen leg builders
// (work_federation_legs_reference_test.go) on every T7d shape, under every rig
// variant, gate on and off, each side on a fresh fixture. A run must pick the
// same legs, in the same order, under the same labels and flags, and reading
// each leg the way its reader does must touch the same physical stores in the
// same order with the same split-strict verdict and leave the same rows
// behind. Two differences are declared (Q2's status rule, and the sweep's
// federated reads), and each is pinned on its own. The handlers that read
// these legs are compared whole in work_federation_handlers_equivalence_test.go.

// t7bVariant changes a built fixture's rigs: a rig sharing another's store,
// configured rigs suspended by effective state (the API's cold set), a
// configured rig with no store.
type t7bVariant struct {
	name string
	set  func(fs *fakeState, fx t7dFixture, city string)
	// q2 marks a variant whose status legs Q2's rule changes (a cold rig a
	// store shares, or a cold configured rig named like the city).
	q2 bool
}

func t7bSuspend(fs *fakeState, names ...string) {
	for _, name := range names {
		found := false
		for i := range fs.cfg.Rigs {
			if fs.cfg.Rigs[i].Name == name {
				fs.cfg.Rigs[i].Suspended, found = true, true
			}
		}
		if !found {
			fs.cfg.Rigs = append(fs.cfg.Rigs, config.Rig{Name: name, Path: "/nonexistent/" + name, Suspended: true})
		}
	}
}

// t7bAlias adds a0, sharing beta's store and sorting first, and zz, over the
// city store and sorting last (legacy shared-file mode).
func t7bAlias(fs *fakeState, fx t7dFixture) {
	fs.stores["a0"], fs.stores["zz"] = fx.tops["beta"], fx.tops["city"]
	fs.cfg.Rigs = append(fs.cfg.Rigs, config.Rig{Name: "a0", Path: "/nonexistent/a0"}, config.Rig{Name: "zz", Path: "/nonexistent/zz"})
}

var t7bVariants = []t7bVariant{
	{"none", func(*fakeState, t7dFixture, string) {}, false},
	{"cold beta", func(fs *fakeState, _ t7dFixture, _ string) { t7bSuspend(fs, "beta") }, false},
	{"cold alpha and beta", func(fs *fakeState, _ t7dFixture, _ string) { t7bSuspend(fs, "alpha", "beta") }, false},
	{"aliased", func(fs *fakeState, fx t7dFixture, _ string) { t7bAlias(fs, fx) }, false},
	{"aliased, cold beta", func(fs *fakeState, fx t7dFixture, _ string) { t7bAlias(fs, fx); t7bSuspend(fs, "beta") }, true},
	{"aliased, cold a0", func(fs *fakeState, fx t7dFixture, _ string) { t7bAlias(fs, fx); t7bSuspend(fs, "a0") }, true},
	{"aliased, cold zz", func(fs *fakeState, fx t7dFixture, _ string) { t7bAlias(fs, fx); t7bSuspend(fs, "zz") }, true},
	{"configured rig with no store, cold", func(fs *fakeState, _ t7dFixture, _ string) { t7bSuspend(fs, "ghost") }, false},
	{"cold rig named like the city", func(fs *fakeState, _ t7dFixture, city string) { t7bSuspend(fs, city) }, true},
}

// q2StatusWorkQueries is the declared-change oracle: main's status builder
// (frozenStatusWorkQueries) with Q2's rule applied, the one rule the sweep
// and the convoy readers already follow. A cold rig the state opened drops
// from the name map before sortedRigNames dedupes it, so a store it shares
// with a warm rig or the city stays counted through that name; every such
// rig is excluded; and a cold name the state opened no rig under (a
// configured rig whose store did not open, named like the city) no longer
// drops the city's alias. Every other line is main's.
func q2StatusWorkQueries(s *Server, cacheColdRigs map[string]bool) ([]frozenWorkQuery, int) {
	stores := s.state.BeadStores()
	excluded := 0
	for _, r := range s.state.Stores().Rigs() {
		if cacheColdRigs[r.Name] {
			delete(stores, r.Name)
			excluded++
		}
	}
	rigNames := frozenSortedRigNames(stores)
	queries := make([]frozenWorkQuery, 0, len(rigNames)+1)
	if cityStore := s.state.CityBeadStore(); cityStore != nil {
		queries = append(queries, frozenWorkQuery{label: "city", store: cityStore, includeReady: true})
	}
	cityName := s.state.CityName()
	for _, rigName := range rigNames {
		queries = append(queries, frozenWorkQuery{
			label:         "rig " + rigName,
			store:         stores[rigName],
			includeStored: true,
			includeReady:  rigName != cityName,
		})
	}
	if graph := s.state.Stores().Graph().Store; graph != nil && !slices.ContainsFunc(queries, func(q frozenWorkQuery) bool { return q.store == graph }) {
		queries = append(queries, frozenWorkQuery{label: "graph", store: graph, includeReady: true})
	}
	return queries, excluded
}

// t7bFakeState is the fixture's fakeState.
func t7bFakeState(t *testing.T, fx t7dFixture) *fakeState {
	t.Helper()
	switch st := fx.state.(type) {
	case *fakeState:
		return st
	case relicReportingState:
		return st.fakeState
	}
	t.Fatalf("fixture state %T", fx.state)
	return nil
}

// t7bSeed gives every store of the fixture an open task assigned to the
// retired session, through the store the state hands out (so a cache sees it).
func t7bSeed(t *testing.T, fx t7dFixture) {
	t.Helper()
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for name := range fx.tops {
			if !yield(name) {
				return
			}
		}
	}) {
		if name == "refused" {
			continue
		}
		if _, err := fx.tops[name].Create(beads.Bead{ID: name + "-so", Title: "held", Type: "task", Assignee: "sess-old"}); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}
	*fx.log = nil
}

// t7bRows renders rows by id.
func t7bRows(rows []beads.Bead, err error) string {
	ids := make([]string, 0, len(rows))
	for _, b := range rows {
		ids = append(ids, b.ID)
	}
	sort.Strings(ids)
	if err != nil {
		return fmt.Sprintf("%v err=%v", ids, err)
	}
	return fmt.Sprint(ids)
}

// t7bCount renders a Counter's answer.
func t7bCount(c beads.Counter, ok bool) string {
	if !ok {
		return "no counter"
	}
	n, err := c.Count(context.Background(), beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.FederatedReadTier})
	return fmt.Sprintf("count %d %v", n, err)
}

var t7bListQuery = beads.ListQuery{AllowScan: true, IncludeClosed: true, Sort: beads.SortCreatedDesc, TierMode: beads.FederatedReadTier}

// t7bOp is one leg builder, frozen and live: each returns what it picked and
// read.
type t7bOp struct {
	name   string
	kind   string // "list", "status", "sweep": t7bDeclared and the log rules key on it
	frozen func(t *testing.T, fx t7dFixture) string
	live   func(t *testing.T, fx t7dFixture) string
	// oracle, when set, is what live must match: main's builder with a
	// declared change applied. frozen must then match it but on a variant
	// the change is declared for.
	oracle func(t *testing.T, fx t7dFixture) string
}

func t7bOps(city string) []t7bOp {
	var ops []t7bOp
	for _, rig := range []string{"", "alpha", "beta", city, "a0", "zz", "ghost", "missing"} {
		ops = append(ops, t7bOp{
			name: "beadList rig=" + rig,
			kind: "list",
			frozen: func(_ *testing.T, fx t7dFixture) string {
				var out []string
				for _, leg := range frozenBeadListLegs(fx.state, rig) {
					var rows []beads.Bead
					err := beads.FederatedRead(func() error {
						var listErr error
						rows, listErr = leg.store.List(t7bListQuery)
						return listErr
					})
					c, ok := beads.As[beads.Counter](leg.store)
					out = append(out, fmt.Sprintf("%s|%s|graph=%v %s %s", leg.label, fx.name(leg.store), leg.graph, t7bRows(rows, err), t7bCount(c, ok)))
				}
				return strings.Join(out, "; ")
			},
			live: func(_ *testing.T, fx t7dFixture) string {
				legs := beadListFanOut(fx.state.Stores(), fx.state.CityName(), rig)
				var out []string
				for _, leg := range legs {
					rows, err := leg.leg.Reader.List(t7bListQuery)
					c, ok := beads.ReaderAs[beads.Counter](leg.leg.Strict)
					out = append(out, fmt.Sprintf("%s|%s|graph=%v %s %s", leg.label, fx.name(leg.leg.Reader), leg.graph, t7bRows(rows, err), t7bCount(c, ok)))
				}
				return strings.Join(out, "; ")
			},
		})
	}
	ops = append(ops, t7bOp{
		name: "statusWorkCounts",
		kind: "status",
		frozen: func(_ *testing.T, fx t7dFixture) string {
			s := New(fx.state)
			queries, excluded := frozenStatusWorkQueries(s, s.loadCacheColdRigs())
			return t7bFrozenStatus(fx, queries, excluded)
		},
		oracle: func(_ *testing.T, fx t7dFixture) string {
			s := New(fx.state)
			queries, excluded := q2StatusWorkQueries(s, s.loadCacheColdRigs())
			return t7bFrozenStatus(fx, queries, excluded)
		},
		live: func(_ *testing.T, fx t7dFixture) string {
			s := New(fx.state)
			queries, excluded := statusWorkQueries(fx.state.Stores(), fx.state.CityName(), s.loadCacheColdRigs())
			out := []string{fmt.Sprintf("excluded=%d", excluded)}
			for _, q := range queries {
				_, counter := beads.ReaderAs[beads.Counter](q.leg.Strict)
				_, ctxReady := beads.ReaderAs[beads.ContextReadyReader](q.leg.Strict)
				out = append(out, t7bStatusRead(fx, q.label, q.leg.Strict, q.includeStored, q.includeReady, counter, ctxReady))
			}
			return strings.Join(out, "; ")
		},
	})
	for _, held := range []bool{false, true} {
		move, frozenMove := reassignOpenWorkAssignedToSession, frozenReassignOpenWorkAssignedToSession
		if held {
			move, frozenMove = reassignHeldOpenWorkAssignedToSession, frozenReassignHeldOpenWorkAssignedToSession
		}
		for _, old := range []string{"sess-old", "tw-worker"} {
			ops = append(ops, t7bOp{
				name: fmt.Sprintf("retiredSessionSweep held=%v %s", held, old),
				kind: "sweep",
				frozen: func(_ *testing.T, fx t7dFixture) string {
					s := New(fx.state)
					var out []string
					for _, store := range frozenRetiredSessionWorkStores(s, s.state.Stores().Sessions()) {
						out = append(out, fmt.Sprintf("%s %v", fx.name(store), frozenMove(store, old, "sess-new")))
					}
					return strings.Join(out, "; ")
				},
				live: func(t *testing.T, fx t7dFixture) string {
					s := New(fx.state)
					st := fx.state.Stores()
					plan, err := st.Plan(retiredSessionSweep(st, s.loadCacheColdRigs()))
					if err != nil {
						t.Fatal(err)
					}
					var out []string
					if _, err := storeref.Sweep(plan, func(_ storeref.TypedLeg, w beads.ResidentStore) error {
						out = append(out, fmt.Sprintf("%s %v", fx.name(w), move(w, old, "sess-new")))
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					return strings.Join(out, "; ")
				},
			})
		}
	}
	return ops
}

// t7bFrozenStatus reads main's status queries.
func t7bFrozenStatus(fx t7dFixture, queries []frozenWorkQuery, excluded int) string {
	out := []string{fmt.Sprintf("excluded=%d", excluded)}
	for _, q := range queries {
		_, counter := beads.As[beads.Counter](q.store)
		_, ctxReady := beads.As[beads.ContextReadyReader](q.store)
		out = append(out, t7bStatusRead(fx, q.label, q.store, q.includeStored, q.includeReady, counter, ctxReady))
	}
	return strings.Join(out, "; ")
}

// t7bStatusRead renders one status query and reads it as statusWorkCounts'
// helpers do: its stored counts (a Counter, else a List) and its ready
// projection (a context-aware reader, else Ready).
func t7bStatusRead(fx t7dFixture, label string, r beads.StoreReader, stored, ready, counter, ctxReady bool) string {
	out := fmt.Sprintf("%s|%s|stored=%v ready=%v", label, fx.name(r), stored, ready)
	if stored {
		out += fmt.Sprintf(" counter=%v %s", counter, t7bRows(r.List(beads.ListQuery{AllowScan: true})))
	}
	if ready {
		out += fmt.Sprintf(" ctxready=%v %s", ctxReady, t7bRows(r.Ready()))
	}
	return out
}

// t7bStripVerdicts drops each entry's split-strict verdict: the sweep's reads
// are federated legs now (the plan's gate role) where main read the raw
// stores strict, which t7bSweepVerdicts pins on its own.
func t7bStripVerdicts(log []string) []string {
	out := make([]string, len(log))
	for i, entry := range log {
		out[i] = entry[:strings.LastIndex(entry, " [")]
	}
	return out
}

// TestWorkFederationLegsMatchTheFrozenBuilders is the equivalence table.
func TestWorkFederationLegsMatchTheFrozenBuilders(t *testing.T) {
	q2Moved := map[string]int{}
	defer func() {
		for _, v := range t7bVariants {
			if v.q2 && q2Moved[v.name] == 0 && !t.Failed() {
				t.Errorf("variant %q is declared for Q2 but Q2's rule changed none of its status legs", v.name)
			}
		}
	}()
	for _, gate := range []bool{false, true} {
		t.Run(fmt.Sprintf("gate=%v", gate), func(t *testing.T) {
			if gate {
				t.Setenv(splittest.StrictEnv, "1")
			} else {
				t.Setenv(splittest.StrictEnv, "")
			}
			for _, shapeName := range slices.Sorted(func(yield func(string) bool) {
				for name := range t7dShapes {
					if !yield(name) {
						return
					}
				}
			}) {
				shape := t7dShapes[shapeName]
				t.Run(shapeName, func(t *testing.T) {
					fixture := shape.fixtures(t)
					for _, variant := range t7bVariants {
						build := func() t7dFixture {
							fx := fixture()
							variant.set(t7bFakeState(t, fx), fx, shape.cityName())
							t7bSeed(t, fx)
							return fx
						}
						for _, op := range t7bOps(shape.cityName()) {
							ffx, lfx := build(), build()
							want, got := op.frozen(t, ffx), op.live(t, lfx)
							if strings.Contains(want+got, "<unknown>") {
								t.Fatalf("%s / %s: a result names a store the fixture does not hold: %s / %s", variant.name, op.name, got, want)
							}
							if op.oracle != nil {
								ofx := build()
								declared := op.oracle(t, ofx)
								if declared != want {
									if !variant.q2 {
										t.Errorf("%s / %s: Q2's rule changes a variant it is not declared for:\n  q2     %s\n  frozen %s", variant.name, op.name, declared, want)
									}
									q2Moved[variant.name]++
								}
								want, ffx = declared, ofx
							}
							if got != want {
								t.Errorf("%s / %s:\n  live   %s\n  frozen %s", variant.name, op.name, got, want)
								continue
							}
							w, g := *ffx.log, *lfx.log
							if op.kind == "sweep" {
								t7bSweepVerdicts(t, variant.name+" / "+op.name, g)
								w, g = t7bStripVerdicts(w), t7bStripVerdicts(g)
							}
							if shape.cached {
								w, g = t7dSortOpRuns(w), t7dSortOpRuns(g)
							}
							if !slices.Equal(w, g) {
								t.Errorf("%s / %s: the live legs' reads and writes differ from the frozen ones'%s", variant.name, op.name, t7dFirstDiff(w, g))
								continue
							}
							if w, g := ffx.dump(t), lfx.dump(t); w != g {
								t.Errorf("%s / %s: the live sweep left\n%s\nthe frozen one\n%s", variant.name, op.name, g, w)
							}
						}
					}
				})
			}
		})
	}
}

// t7bSweepVerdicts pins the sweep's moved verdict: every read through a
// sweep leg is federated (the plan's gate role), never relic.
func t7bSweepVerdicts(t *testing.T, name string, log []string) {
	t.Helper()
	for _, entry := range log {
		if strings.Contains(entry, " List ") && !strings.HasSuffix(entry, "[federated]") {
			t.Errorf("%s: a sweep read was not federated: %s", name, entry)
		}
	}
}

// TestStatusWorkLegsDropColdRigsBeforeDedupe pins Q2's declared change: in
// legacy shared-file mode status keeps the store a cold rig shares with a
// warm one (main dropped it, because the cold rig sorted first and its name
// was dropped after dedupe), and counts every opened cold rig as excluded.
func TestStatusWorkLegsDropColdRigsBeforeDedupe(t *testing.T) {
	t7dGateOff(t)
	for _, tc := range []struct {
		variant      string
		frozen, live string
	}{
		{
			variant: "aliased, cold a0",
			frozen:  "excluded=1 city:city rig alpha:alpha rig test-city:city",
			live:    "excluded=1 city:city rig alpha:alpha rig beta:beta rig test-city:city",
		},
		{
			variant: "aliased, cold zz",
			frozen:  "excluded=0 city:city rig a0:beta rig alpha:alpha rig test-city:city",
			live:    "excluded=1 city:city rig a0:beta rig alpha:alpha rig test-city:city",
		},
		{
			// A configured rig named like the city whose store did not open:
			// main's cold name hid the city's alias, so the city's stored
			// counts went uncounted; it names no opened rig now.
			variant: "cold rig named like the city",
			frozen:  "excluded=1 city:city rig alpha:alpha rig beta:beta",
			live:    "excluded=0 city:city rig alpha:alpha rig beta:beta rig test-city:city",
		},
	} {
		var variant t7bVariant
		for _, v := range t7bVariants {
			if v.name == tc.variant {
				variant = v
			}
		}
		shape := t7dShapes["unsplit"]
		fx := shape.fixtures(t)()
		variant.set(t7bFakeState(t, fx), fx, shape.cityName())
		s := New(fx.state)
		cold := s.loadCacheColdRigs()
		frozen, excluded := frozenStatusWorkQueries(s, cold)
		got := []string{fmt.Sprintf("excluded=%d", excluded)}
		for _, q := range frozen {
			got = append(got, q.label+":"+fx.name(q.store))
		}
		if w := strings.Join(got, " "); w != tc.frozen {
			t.Errorf("%s: frozen %s, want %s", tc.variant, w, tc.frozen)
		}
		live, excluded := statusWorkQueries(fx.state.Stores(), fx.state.CityName(), cold)
		got = []string{fmt.Sprintf("excluded=%d", excluded)}
		for _, q := range live {
			got = append(got, q.label+":"+fx.name(q.leg.Strict))
		}
		if g := strings.Join(got, " "); g != tc.live {
			t.Errorf("%s: live %s, want %s", tc.variant, g, tc.live)
		}
	}
}
