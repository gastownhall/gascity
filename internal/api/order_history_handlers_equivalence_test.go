package api

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/storeref"
)

// The T7d.2 equivalence table: every function T7d.2 converts, run beside its
// frozen copy from main 8d2fb3120c (order_history_handlers_reference_test.go)
// on every T7d shape under every T7e rig variant, gate on and off, each side
// on a fresh fixture. A run must give the same result, touch the same
// physical stores in the same order, each read with the same split-strict
// verdict but for the verdicts the order-history plan moves on purpose
// (t7d2MovedVerdicts), and leave the same rows behind.
//
// Declared, each pinned on its own:
//
//   - verdicts (t7b2-design.md Q7): main read every order leg under
//     beads.RelicRead; a leg now reads in its plan role, so the orders and
//     graph bindings, every leg of a city whose graph is not relocated, and a
//     retired rig a store_ref names read federated; a relocated city's scope
//     legs stay relic. The reads main made unmarked (the fresh event cursor,
//     the feed's latest-open lookup, the run cancel) stay strict, exactly;
//   - a rig over the city store is read once (TestOrderHistoryListsARigOverTheCityStoreOnce);
//     the aliased variant names no order on that rig here, and the leg table
//     (order_history_legs_equivalence_test.go) counts the case.
//
// Only two fields are canonicalized: the seconds a cooldown reason renders
// (time passes between the two runs) and the history's cache age.

// t7d2Orders are the fixture's orders: a city cooldown order, rig orders on
// alpha, beta, the rig named like the city and a configured rig with no
// store, and an event order whose cursor reads every leg.
func t7d2Orders(city string) []orders.Order {
	return []orders.Order{
		{Name: "nightly", Trigger: "cooldown", Interval: "1ns", Formula: "f"},
		{Name: "nightly", Rig: "alpha", Trigger: "cooldown", Interval: "1000h", Formula: "f"},
		{Name: "nightly", Rig: "beta", Trigger: "cooldown", Interval: "1ns", Exec: "true"},
		{Name: "nightly", Rig: city, Trigger: "cooldown", Interval: "1ns", Formula: "f"},
		{Name: "nightly", Rig: "ghost", Trigger: "cooldown", Interval: "1ns", Formula: "f"},
		{Name: "watch", Trigger: "event", On: "bead.closed", Formula: "f"},
	}
}

// t7d2Seed adds order history to a shape's templates: tracking beads for
// every order in the stores that hold them before and after the split,
// event-order runs carrying event seqs on several legs, and a plain bead
// carrying a run label (no run record).
func t7d2Seed(t *testing.T, city string) func(map[string]*beads.MemStore) {
	return func(templates map[string]*beads.MemStore) {
		track := func(store, id, scoped string, extra ...string) {
			m := templates[store]
			if m == nil {
				return
			}
			t7dBead(t, m, beads.Bead{
				ID: m.IDPrefix + "-" + id, Title: "order:" + scoped, Type: "task", Status: "closed",
				Labels: append([]string{"order-run:" + scoped, "order-tracking"}, extra...),
			})
		}
		home := "city"
		if templates["binding"] != nil {
			home = "binding"
		}
		track(home, "ra1", "nightly:rig:alpha")
		track("alpha", "ra0", "nightly:rig:alpha")
		track("beta", "rb0", "nightly:rig:beta")
		track(home, "rn1", "nightly:rig:"+city)
		track("named", "rn0", "nightly:rig:"+city)
		track("city", "w0", "watch", "seq:7")
		track(home, "w1", "watch", "seq:12")
		track("orders", "w2", "watch", "seq:20")
		track("alpha", "w3", "watch", "seq:3")
		if m := templates["alpha"]; m != nil {
			t7dBead(t, m, beads.Bead{ID: "al-plain", Title: "found by nightly", Type: "task", Labels: []string{"order-run:nightly", "seq:99"}})
		}
	}
}

var (
	t7d2Elapsed   = regexp.MustCompile(`elapsed [0-9hms.µn]+`)
	t7d2Remaining = regexp.MustCompile(`cooldown: [0-9hms.µn]+ remaining`)
)

// t7d2Canonical renders a result with the cooldown seconds canonicalized.
func t7d2Canonical(v any, err error) string {
	out := t7dJSON(v, err)
	out = t7d2Elapsed.ReplaceAllString(out, "elapsed <d>")
	return t7d2Remaining.ReplaceAllString(out, "cooldown: <d> remaining")
}

// t7d2History is a history response without its cache age.
func t7d2History(out *OrderHistoryListOutput, err error) string {
	if out != nil {
		c := *out
		c.CacheAgeS, c.CacheRefresh = 0, CacheRefresh{}
		out = &c
	}
	return t7d2Canonical(out, err)
}

// t7d2MovedVerdicts is the declared verdict oracle: a store an order-history
// or workflow plan reads through a federated leg may read federated where
// main's marker read it relic; a store that one plan reads relic and another
// federated (the gate's two views of one store) may read either. A strict
// read is never rewritten.
func t7d2MovedVerdicts(fx t7dFixture, frozen, live []string) ([]string, []string) {
	st := fx.state.Stores()
	reads := storeref.WorkflowReadsSuspendedRigs
	intents := []storeref.Intent{
		storeref.OrderHistory{Kind: storeref.OrderHistoryAll, SuspendedRigs: reads},
		storeref.Workflow{IncludeRetiredRigs: true, SuspendedRigs: reads},
	}
	for _, o := range t7d2Orders(fx.state.CityName()) {
		scope := storeref.WorkRef
		if o.Rig != "" {
			scope = storeref.RigRef(o.Rig)
		}
		intents = append(intents, storeref.OrderHistory{Kind: storeref.OrderHistoryOne, Scope: scope, SuspendedRigs: reads})
	}
	federated, relic := map[string]bool{}, map[string]bool{}
	for _, in := range intents {
		plan, err := st.Plan(in)
		if err != nil {
			continue
		}
		for _, l := range plan.Legs {
			name, _, _ := strings.Cut(fx.name(l.Leg.Store), "/")
			switch l.Gate {
			case beads.FederatedLegRole:
				federated[name] = true
			case beads.RelicLegRole:
				relic[name] = true
			}
		}
	}
	norm := func(log []string, rewrite bool) []string {
		out := make([]string, len(log))
		for i, entry := range log {
			name, _, _ := strings.Cut(entry, " ")
			switch {
			case federated[name] && relic[name]:
				entry = strings.Replace(strings.Replace(entry, "[relic]", "[relic|federated]", 1), "[federated]", "[relic|federated]", 1)
			case federated[name] && rewrite:
				entry = strings.Replace(entry, "[relic]", "[federated]", 1)
			}
			out[i] = entry
		}
		return out
	}
	return norm(frozen, true), norm(live, false)
}

// t7d2Op is one converted function, frozen and live.
type t7d2Op struct {
	name         string
	frozen, live func(fx t7dFixture) string
	writes       bool
}

func t7d2Ops(city string) []t7d2Op {
	ctx := context.Background()
	var ops []t7d2Op
	for _, fresh := range []bool{false, true} {
		ops = append(ops, t7d2Op{
			name: fmt.Sprintf("buildOrderCheckBody fresh=%v", fresh),
			frozen: func(fx t7dFixture) string {
				return t7d2Canonical(t7d2FrozenBuildOrderCheckBody(New(fx.state), fresh), nil)
			},
			live: func(fx t7dFixture) string { return t7d2Canonical(New(fx.state).buildOrderCheckBody(fresh), nil) },
		})
	}
	for _, o := range t7d2Orders(city) {
		for _, in := range []OrderHistoryInput{
			{ScopedName: o.ScopedName()},
			{ScopedName: o.ScopedName(), Limit: 1},
			{ScopedName: o.ScopedName(), Before: "2000-01-01T00:00:00Z"},
		} {
			ops = append(ops, t7d2Op{
				name: fmt.Sprintf("humaHandleOrderHistory %s limit=%d before=%q", in.ScopedName, in.Limit, in.Before),
				frozen: func(fx t7dFixture) string {
					return t7d2History(t7d2FrozenHumaHandleOrderHistory(ctx, New(fx.state), &in))
				},
				live: func(fx t7dFixture) string { return t7d2History(New(fx.state).humaHandleOrderHistory(ctx, &in)) },
			})
		}
	}
	// An order name no order defines: the scoped name's rig still picks the legs.
	unknown := OrderHistoryInput{ScopedName: "gone:rig:alpha"}
	ops = append(ops, t7d2Op{
		name: "humaHandleOrderHistory gone:rig:alpha",
		frozen: func(fx t7dFixture) string {
			return t7d2History(t7d2FrozenHumaHandleOrderHistory(ctx, New(fx.state), &unknown))
		},
		live: func(fx t7dFixture) string { return t7d2History(New(fx.state).humaHandleOrderHistory(ctx, &unknown)) },
	})
	for _, id := range []string{"tc-or1", "gcg-or1", "al-or1", "or-or1", "al-ra0", "gcg-w1", "tc-w0", "or-w2", "tx-rn0", "missing"} {
		for _, ref := range []string{"", "city:" + city, "rig:alpha", "rig:" + city, "orders:" + city, "graph:" + city, "orders:elsewhere"} {
			in := OrderHistoryDetailInput{BeadID: id, StoreRef: ref}
			ops = append(ops, t7d2Op{
				name: "humaHandleOrderHistoryDetail " + id + " ref=" + ref,
				frozen: func(fx t7dFixture) string {
					return t7d2Canonical(t7d2FrozenHumaHandleOrderHistoryDetail(ctx, New(fx.state), &in))
				},
				live: func(fx t7dFixture) string { return t7d2Canonical(New(fx.state).humaHandleOrderHistoryDetail(ctx, &in)) },
			})
		}
	}
	for _, scope := range [][2]string{{"city", city}, {"rig", "alpha"}, {"rig", "beta"}, {"rig", city}} {
		for _, limit := range []int{0, 2} {
			ops = append(ops, t7d2Op{
				name: fmt.Sprintf("buildOrderRunFeedItems %s:%s limit=%d", scope[0], scope[1], limit),
				frozen: func(fx t7dFixture) string {
					return t7d2Canonical(t7d2FrozenBuildOrderRunFeedItems(fx.state, scope[0], scope[1], limit))
				},
				live: func(fx t7dFixture) string {
					return t7d2Canonical(buildOrderRunFeedItems(fx.state, scope[0], scope[1], limit))
				},
			})
		}
	}
	for _, runID := range []string{"run-1", "run-9", "run-5", "run-7", "run-3", "run-tw", "co-1", "missing"} {
		ops = append(ops, t7d2Op{
			name:   "cancelRun " + runID,
			writes: true,
			frozen: func(fx t7dFixture) string { return t7d2Canonical(t7d2FrozenCancelRun(New(fx.state), runID, nil)) },
			live:   func(fx t7dFixture) string { return t7d2Canonical(New(fx.state).cancelRun(runID, nil)) },
		})
	}
	return ops
}

// TestOrderHistoryHandlersMatchTheFrozenReaders is the equivalence table.
func TestOrderHistoryHandlersMatchTheFrozenReaders(t *testing.T) {
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
					fixtures := shape.fixturesWith(t, t7d2Seed(t, shape.cityName()))
					for _, variantName := range slices.Sorted(func(yield func(string) bool) {
						for name := range t7eVariants {
							if !yield(name) {
								return
							}
						}
					}) {
						variant := t7eVariants[variantName]
						build := func() t7dFixture {
							fx := fixtures()
							fs := t7eFakeState(t, fx.state)
							if variant != nil {
								variant(t, fx, fs)
							}
							fs.autos = t7d2Orders(shape.cityName())
							ep := events.NewFake()
							for i := 0; i < 30; i++ {
								ep.Record(events.Event{Type: "bead.closed", Actor: "t"})
							}
							fs.eventProv = ep
							*fx.log = nil
							return fx
						}
						for _, op := range t7d2Ops(shape.cityName()) {
							name := variantName + " / " + op.name
							ffx, lfx := build(), build()
							want, got := op.frozen(ffx), op.live(lfx)
							if strings.Contains(want+got, "<unknown>") {
								t.Fatalf("%s: a result names a store the fixture does not hold: %s / %s", name, got, want)
							}
							if got != want {
								t.Errorf("%s:\n  live   %s\n  frozen %s", name, got, want)
								continue
							}
							w, g := t7dSortDepRuns(*ffx.log), t7dSortDepRuns(*lfx.log)
							w, g = t7d2MovedVerdicts(ffx, w, g)
							switch {
							case shape.cached && op.writes:
								w, g = t7dOpSet(w), t7dOpSet(g)
							case shape.cached:
								w, g = t7dSortOpRuns(w), t7dSortOpRuns(g)
							}
							if !slices.Equal(w, g) {
								t.Errorf("%s: the live reads and writes differ from the frozen ones'%s", name, t7dFirstDiff(w, g))
								continue
							}
							if w, g := ffx.dump(t), lfx.dump(t); w != g {
								t.Errorf("%s: the live run left\n%s\nthe frozen one\n%s", name, g, w)
							}
						}
					}
				})
			}
		})
	}
}
