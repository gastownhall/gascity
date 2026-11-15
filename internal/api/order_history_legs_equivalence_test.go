package api

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/storeref"
)

// The T7b.2 equivalence table: storeref's OrderHistory legs, labeled the way
// the API names its order-history stores (orderHistoryLegs and
// orderHistoryOrdersRef, test-local in T7b.2 and production since T7d.2),
// beside main's frozen order-history builders
// (order_history_legs_reference_test.go), on every T7d shape under every T7e
// rig variant, gate on and off, each side on a fresh fixture. A run must pick
// the same stores in the same order under the same refs and scopes, and one
// read of each leg (an order's run label, the history list's query) must
// reach the same physical stores in the same order.
//
// Declared, each counted and pinned on its own:
//
//   - a rig over the city store (the aliased variant's zz) is read once:
//     main listed that store twice, under rig:zz and city:, and the plan
//     reads a store once, under the first leg's ref (owner decision,
//     2026-11-13);
//   - verdicts: main read every leg under beads.RelicRead; a leg now reads
//     in its plan role, so the orders and graph bindings and every leg of a
//     city whose graph is not relocated read federated, and only a relocated
//     city's scope legs stay relic (t7b2-design.md Q7).
//
// The handlers that read these legs are compared whole in
// order_history_handlers_equivalence_test.go.

// t7b2HistoryQuery is the history list's read of one leg.
var t7b2HistoryQuery = beads.ListQuery{Label: "order-run:nightly", Limit: 20, IncludeClosed: true, Sort: beads.SortCreatedDesc, TierMode: beads.TierBoth}

// t7b2Frozen renders main's list and reads each entry as the history list
// did: under beads.RelicRead, through its store.
func t7b2Frozen(fx t7dFixture, infos []t7b2FrozenOrderInfo, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	var out []string
	for _, info := range infos {
		var rows []beads.Bead
		readErr := beads.RelicRead(func() error {
			var listErr error
			rows, listErr = info.store.List(t7b2HistoryQuery)
			return listErr
		})
		out = append(out, fmt.Sprintf("%s|%s|%s|%s %s", info.ref, info.scopeKind, info.scopeRef, fx.name(info.store), t7b2Rows(rows, readErr)))
	}
	return strings.Join(out, "; ")
}

// t7b2Live renders the labeled legs and reads each through its reader.
func t7b2Live(fx t7dFixture, legs []workflowStoreInfo, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	var out []string
	for _, l := range legs {
		rows, readErr := l.leg.Reader.List(t7b2HistoryQuery)
		out = append(out, fmt.Sprintf("%s|%s|%s|%s %s", l.ref, l.scopeKind, l.scopeRef, fx.name(l.leg.Reader), t7b2Rows(rows, readErr)))
	}
	return strings.Join(out, "; ")
}

func t7b2Rows(rows []beads.Bead, err error) string {
	ids := make([]string, 0, len(rows))
	for _, b := range rows {
		ids = append(ids, b.ID)
	}
	if err != nil {
		return fmt.Sprintf("%v err=%v", ids, err)
	}
	return fmt.Sprint(ids)
}

// t7b2DropRepeats is the declared once-per-store oracle: main's list with
// every entry whose store an earlier entry already reads dropped.
func t7b2DropRepeats(infos []t7b2FrozenOrderInfo) []t7b2FrozenOrderInfo {
	var out []t7b2FrozenOrderInfo
	for _, info := range infos {
		if slices.ContainsFunc(out, func(have t7b2FrozenOrderInfo) bool { return beads.SameStore(have.store, info.store) }) {
			continue
		}
		out = append(out, info)
	}
	return out
}

// t7b2Op is one leg builder, frozen and live.
type t7b2Op struct {
	name   string
	frozen func(fx t7dFixture) ([]t7b2FrozenOrderInfo, error)
	live   func(fx t7dFixture) ([]workflowStoreInfo, error)
}

func t7b2Ops(city string) []t7b2Op {
	reads := storeref.WorkflowReadsSuspendedRigs
	var ops []t7b2Op
	for _, rig := range []string{"", "alpha", "beta", city, "a0", "zz", "ghost", "nope"} {
		scope := storeref.WorkRef
		if rig != "" {
			scope = storeref.RigRef(rig)
		}
		ops = append(ops, t7b2Op{
			name: fmt.Sprintf("one order, rig %q", rig),
			frozen: func(fx t7dFixture) ([]t7b2FrozenOrderInfo, error) {
				return t7b2FrozenOrderStoreInfosForState(fx.state, orders.Order{Name: "nightly", Rig: rig})
			},
			live: func(fx t7dFixture) ([]workflowStoreInfo, error) {
				legs, err := orderHistoryLegs(fx.state.Stores(), fx.state.CityName(), storeref.OrderHistory{Kind: storeref.OrderHistoryOne, Scope: scope, SuspendedRigs: reads})
				if err != nil {
					return nil, errNoOrderStores
				}
				return legs, nil
			},
		})
	}
	ops = append(ops, t7b2Op{
		name:   "every order",
		frozen: func(fx t7dFixture) ([]t7b2FrozenOrderInfo, error) { return t7b2FrozenEveryOrderInfos(fx.state), nil },
		live: func(fx t7dFixture) ([]workflowStoreInfo, error) {
			legs, err := orderHistoryLegs(fx.state.Stores(), fx.state.CityName(), storeref.OrderHistory{Kind: storeref.OrderHistoryAll, SuspendedRigs: reads})
			if err != nil {
				return nil, nil // main's list was empty, never an error
			}
			return legs, nil
		},
	})
	for _, scopeRef := range []string{city, "elsewhere"} {
		ops = append(ops, t7b2Op{
			name: "orders:" + scopeRef,
			frozen: func(fx t7dFixture) ([]t7b2FrozenOrderInfo, error) {
				info, ok := t7b2FrozenOrdersRefInfo(fx.state, scopeRef)
				if !ok {
					return nil, nil
				}
				return []t7b2FrozenOrderInfo{info}, nil
			},
			live: func(fx t7dFixture) ([]workflowStoreInfo, error) {
				l, ok := orderHistoryOrdersRef(fx.state.Stores(), fx.state.CityName(), scopeRef)
				if !ok {
					return nil, nil
				}
				return []workflowStoreInfo{l}, nil
			},
		})
	}
	return ops
}

// t7b2Verdicts splits a log into its entries without verdicts and the
// verdicts themselves.
func t7b2Verdicts(log []string) (entries, verdicts []string) {
	for _, e := range log {
		i := strings.LastIndex(e, " [")
		entries, verdicts = append(entries, e[:i]), append(verdicts, e[i+2:len(e)-1])
	}
	return entries, verdicts
}

// TestOrderHistoryLegsMatchTheFrozenBuilders is the equivalence table.
func TestOrderHistoryLegsMatchTheFrozenBuilders(t *testing.T) {
	declared := 0
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
				fixtures := shape.fixtures(t)
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
						if variant != nil {
							variant(t, fx, t7eFakeState(t, fx.state))
						}
						*fx.log = nil
						return fx
					}
					for _, op := range t7b2Ops(shape.cityName()) {
						name := fmt.Sprintf("%s / %s / %s", shapeName, variantName, op.name)
						ffx, lfx := build(), build()
						frozenInfos, frozenErr := op.frozen(ffx)
						if dropped := t7b2DropRepeats(frozenInfos); len(dropped) != len(frozenInfos) {
							if variantName != "aliased rigs" || op.name != `one order, rig "zz"` {
								t.Errorf("%s: main read a store twice outside the declared rig-over-the-city case", name)
							}
							frozenInfos = dropped
							declared++
						}
						want := t7b2Frozen(ffx, frozenInfos, frozenErr)
						liveLegs, liveErr := op.live(lfx)
						got := t7b2Live(lfx, liveLegs, liveErr)
						if strings.Contains(want+got, "<unknown>") {
							t.Fatalf("%s: a result names a store the fixture does not hold:\n  live   %s\n  frozen %s", name, got, want)
						}
						if got != want {
							t.Errorf("%s:\n  live   %s\n  frozen %s", name, got, want)
							continue
						}
						w, wv := t7b2Verdicts(*ffx.log)
						g, gv := t7b2Verdicts(*lfx.log)
						if shape.cached {
							w, g = t7dSortOpRuns(w), t7dSortOpRuns(g)
						}
						if !slices.Equal(w, g) {
							t.Errorf("%s: the live legs' reads differ from the frozen ones'%s", name, t7dFirstDiff(w, g))
							continue
						}
						for _, v := range wv {
							if v != "relic" {
								t.Errorf("%s: a frozen read was %s, not under its relic marker", name, v)
							}
						}
						t7b2CheckLiveVerdicts(t, name, lfx, liveLegs, gv)
					}
				}
			}
		})
	}
	if declared == 0 && !t.Failed() {
		t.Error("the rig-over-the-city store is declared but no run read a store twice")
	}
}

// t7b2CheckLiveVerdicts pins the declared verdicts: each leg's one read ran
// in its plan's gate role, relic exactly on a relocated city's scope legs.
func t7b2CheckLiveVerdicts(t *testing.T, name string, fx t7dFixture, legs []workflowStoreInfo, verdicts []string) {
	t.Helper()
	relocated := !beads.SameStore(fx.state.Stores().Graph(), fx.state.Stores().Work())
	if len(verdicts) != len(legs) && !strings.Contains(name, "cached") {
		t.Errorf("%s: %d legs made %d reads", name, len(legs), len(verdicts))
		return
	}
	want := make([]string, 0, len(legs))
	for _, l := range legs {
		v := "federated"
		if relocated && l.leg.Role == storeref.RoleRelic {
			v = "relic"
		}
		if l.leg.Role == storeref.RoleRelic && !relocated {
			t.Errorf("%s: %s is a relic leg on a city whose graph is not relocated", name, l.ref)
		}
		want = append(want, v)
	}
	if len(verdicts) == len(want) && !slices.Equal(verdicts, want) {
		t.Errorf("%s: live verdicts %v, want %v", name, verdicts, want)
	}
}

// TestOrderHistoryReadsARigOverTheCityStoreOnce pins the declared change: a
// rig order whose rig shares the city store lists that store once, under the
// rig's ref (main listed it under rig: and city:, so each row came back
// twice).
func TestOrderHistoryReadsARigOverTheCityStoreOnce(t *testing.T) {
	t7dGateOff(t)
	fx := t7dShapes["unsplit"].fixtures(t)()
	fs := t7eFakeState(t, fx.state)
	t7eVariants["aliased rigs"](t, fx, fs)
	frozen, err := t7b2FrozenOrderStoreInfosForState(fx.state, orders.Order{Name: "nightly", Rig: "zz"})
	if err != nil {
		t.Fatal(err)
	}
	if len(frozen) != 2 || !beads.SameStore(frozen[0].store, frozen[1].store) {
		t.Fatalf("fixture: main's list is %d entries; want rig:zz and city: over one store", len(frozen))
	}
	legs, err := orderHistoryLegs(fx.state.Stores(), fx.state.CityName(), storeref.OrderHistory{Kind: storeref.OrderHistoryOne, Scope: storeref.RigRef("zz"), SuspendedRigs: storeref.WorkflowReadsSuspendedRigs})
	if err != nil {
		t.Fatal(err)
	}
	if len(legs) != 1 || legs[0].ref != "rig:zz" || !beads.SameStore(legs[0].leg.Reader, frozen[0].store) {
		t.Errorf("legs = %s, want rig:zz alone over the city store", t7b2Live(fx, legs, nil))
	}
}
