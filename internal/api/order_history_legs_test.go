package api

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/storeref"
)

// t7d2Refs renders entries' refs in order.
func t7d2Refs(infos []workflowStoreInfo) string {
	refs := make([]string, 0, len(infos))
	for _, info := range infos {
		refs = append(refs, info.ref)
	}
	return strings.Join(refs, " ")
}

// TestOrderReadsReadOneEpoch: each order request reads Stores() once, so its
// legs and its cache gate never come from two rig sets. The epoch is swapped
// after the first read.
func TestOrderReadsReadOneEpoch(t *testing.T) {
	t7dGateOff(t)
	city, binding, alpha := beads.NewMemStore(), beads.NewMemStore(), beads.NewMemStore()
	epoch := func(rigs map[string]beads.Store) *fakeState {
		f := newFakeState(t)
		f.cityName = t7dCity
		f.cityBeadStore = city
		f.graphBeadStore, f.ordersBeadStore = binding, binding
		f.stores = rigs
		f.autos = []orders.Order{{Name: "nightly", Rig: "alpha", Trigger: "cooldown", Interval: "1h", Formula: "f"}}
		return f
	}
	before := epoch(map[string]beads.Store{"alpha": alpha})
	after := epoch(map[string]beads.Store{})
	swap := func() *t7dEpochSwapState {
		return &t7dEpochSwapState{fakeState: before, first: before.Stores(), later: after.Stores()}
	}
	ctx := context.Background()

	state := swap()
	if _, err := New(state).humaHandleOrderHistory(ctx, &OrderHistoryInput{ScopedName: "nightly:rig:alpha"}); err != nil || state.calls != 1 {
		t.Errorf("order history: %v after %d Stores() reads, want one", err, state.calls)
	}
	state = swap()
	if New(state).buildOrderCheckBody(true); state.calls != 1 {
		t.Errorf("order check: %d Stores() reads, want one", state.calls)
	}
	state = swap()
	if _, err := New(state).humaHandleOrderHistoryDetail(ctx, &OrderHistoryDetailInput{BeadID: "missing"}); state.calls != 1 {
		t.Errorf("order detail: %v after %d Stores() reads, want one", err, state.calls)
	}
	state = swap()
	if _, err := New(state).humaHandleOrderHistoryDetail(ctx, &OrderHistoryDetailInput{BeadID: "missing", StoreRef: "orders:" + t7dCity}); state.calls != 1 {
		t.Errorf("order detail by orders: ref: %v after %d Stores() reads, want one", err, state.calls)
	}
	state = swap()
	if _, err := buildOrderRunFeedItems(state, "city", t7dCity, 0); err != nil || state.calls != 1 {
		t.Errorf("order feed: %v after %d Stores() reads, want one", err, state.calls)
	}
}

// TestOrderHistoryOrdersRefResolvesASharedGraphBinding: where one binding
// serves graph and orders, the every-order list names it graph: (its workflow
// leg), and orders:<city> still resolves to it, under its own ref.
func TestOrderHistoryOrdersRefResolvesASharedGraphBinding(t *testing.T) {
	t7dGateOff(t)
	state := newFakeState(t)
	state.cityName = t7dCity
	state.cityBeadStore = beads.NewMemStore()
	binding := beads.NewMemStore()
	state.graphBeadStore, state.ordersBeadStore = binding, binding
	state.stores, state.cfg.Rigs = nil, nil

	if got, want := t7d2Refs(everyOrderLegs(state.Stores(), state.CityName())), "graph:"+t7dCity+" city:"+t7dCity; got != want {
		t.Errorf("every-order legs = %s, want %s", got, want)
	}
	info, ok := workflowStoreByRef(state, "orders:"+t7dCity)
	if !ok || info.ref != "orders:"+t7dCity || !beads.SameStore(info.leg.Reader, binding) {
		t.Errorf("orders:%s = %v, %q; want the shared binding under orders:", t7dCity, ok, info.ref)
	}
	if _, ok := workflowStoreByRef(state, "orders:elsewhere"); ok {
		t.Error("orders:elsewhere resolved")
	}
}

// TestOrderHistoryReadsAnUnopenedRigOrderFromTheCityAndBinding: an order whose
// rig the epoch did not open still reads the city and the orders binding.
func TestOrderHistoryReadsAnUnopenedRigOrderFromTheCityAndBinding(t *testing.T) {
	t7dGateOff(t)
	state := newFakeState(t)
	state.cityName = t7dCity
	state.cityBeadStore = beads.NewMemStore()
	state.ordersBeadStore, state.graphBeadStore = beads.NewMemStore(), beads.NewMemStore()
	state.stores = map[string]beads.Store{"alpha": beads.NewMemStore()}
	infos, err := orderLegsFor(state.Stores(), state.CityName(), "ghost")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := t7d2Refs(infos), "city:"+t7dCity+" orders:"+t7dCity; got != want {
		t.Errorf("legs = %s, want %s", got, want)
	}
}

// TestOrderHistoryReadsARigNamedLikeTheCity: an order of a rig named like the
// city reads that rig, first; the every-order list skips it, as the workflow
// legs do.
func TestOrderHistoryReadsARigNamedLikeTheCity(t *testing.T) {
	t7dGateOff(t)
	state := newFakeState(t)
	state.cityName = t7dCity
	state.cityBeadStore = beads.NewMemStore()
	state.stores = map[string]beads.Store{t7dCity: beads.NewMemStore()}
	state.cfg.Rigs = []config.Rig{{Name: t7dCity, Path: "/nonexistent/tx"}}
	infos, err := orderLegsFor(state.Stores(), state.CityName(), t7dCity)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := t7d2Refs(infos), "rig:"+t7dCity+" city:"+t7dCity; got != want {
		t.Errorf("the order's legs = %s, want %s", got, want)
	}
	if got := t7d2Refs(everyOrderLegs(state.Stores(), state.CityName())); strings.Contains(got, "rig:"+t7dCity) {
		t.Errorf("the every-order legs = %s, want no rig named like the city", got)
	}
}

// TestOrderHistoryListsARigOverTheCityStoreOnce pins the declared change at
// the handler: a rig over the city store (legacy shared-file mode) lists its
// history once, under rig:<name>; main listed every row twice, under rig: and
// city:.
func TestOrderHistoryListsARigOverTheCityStoreOnce(t *testing.T) {
	t7dGateOff(t)
	state := newFakeState(t)
	state.cityName = t7dCity
	city := beads.NewMemStore()
	state.cityBeadStore = city
	state.stores = map[string]beads.Store{"zz": city}
	state.cfg.Rigs = []config.Rig{{Name: "zz", Path: "/nonexistent/zz"}}
	if _, err := city.Create(beads.Bead{Title: "order:nightly:rig:zz", Labels: []string{"order-run:nightly:rig:zz", "order-tracking"}}); err != nil {
		t.Fatal(err)
	}
	in := &OrderHistoryInput{ScopedName: "nightly:rig:zz"}
	frozen, err := t7d2FrozenHumaHandleOrderHistory(context.Background(), New(state), in)
	if err != nil || len(frozen.Body.Entries) != 2 {
		t.Fatalf("fixture: main listed %d entries (%v), want the row twice", len(frozen.Body.Entries), err)
	}
	live, err := New(state).humaHandleOrderHistory(context.Background(), in)
	if err != nil || len(live.Body.Entries) != 1 || live.Body.Entries[0].StoreRef != "rig:zz" {
		t.Errorf("history = %+v (%v), want the row once, under rig:zz", live, err)
	}
}

// roleCachedLister is a store whose cache-first list records the leg role on
// its stack, as the split-strict gate would see it.
type roleCachedLister struct {
	beads.Store
	roles []beads.LegRole
}

func (s *roleCachedLister) CachedList(q beads.ListQuery) ([]beads.Bead, bool) {
	pcs := make([]uintptr, 64)
	s.roles = append(s.roles, beads.LegRoleOnStack(pcs[:runtime.Callers(1, pcs)]))
	rows, err := s.List(q)
	return rows, err == nil
}

// TestOrderHistoryCachedFirstReadsInTheLegRole: the history list's cache-first
// read goes through the leg's own role-carrying lister, so a relic leg's
// cached read is relic debt and a binding's is federated, as its plain read
// is. A cached read never reaches a backing store, so only this pin sees it.
func TestOrderHistoryCachedFirstReadsInTheLegRole(t *testing.T) {
	for _, tc := range []struct {
		name string
		mint func(beads.Store) beads.FederatedReader
		want beads.LegRole
	}{
		{"relic", beadstest.RelicLeg, beads.RelicLegRole},
		{"federated", beadstest.FederatedLeg, beads.FederatedLegRole},
	} {
		store := &roleCachedLister{Store: beads.NewMemStore()}
		info := workflowStoreInfo{ref: "city:c", leg: storeref.TypedLeg{Reader: tc.mint(store), Strict: beadstest.StrictLeg(store)}}
		if _, err := orderHistoryBeadsAcrossStoreInfosCachedFirst([]workflowStoreInfo{info}, "nightly", 5, time.Time{}); err != nil {
			t.Fatal(err)
		}
		if len(store.roles) != 1 || store.roles[0] != tc.want {
			t.Errorf("%s leg: cache-first reads ran in roles %v, want one %v", tc.name, store.roles, tc.want)
		}
	}
}
