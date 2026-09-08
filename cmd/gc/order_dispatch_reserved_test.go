package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orders"
)

type reservedDispatchExecRecorder struct {
	mu       sync.Mutex
	calls    []string
	started  chan struct{}
	startOne sync.Once
	release  <-chan struct{}
}

func (r *reservedDispatchExecRecorder) run(ctx context.Context, command, _ string, _ []string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, command)
	r.mu.Unlock()
	if r.started != nil {
		r.startOne.Do(func() { close(r.started) })
	}
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, nil
}

func (r *reservedDispatchExecRecorder) counts() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	counts := make(map[string]int, len(r.calls))
	for _, command := range r.calls {
		counts[command]++
	}
	return counts
}

func reservedExecOrder(t *testing.T, name string, noWorkGate bool) orders.Order {
	t.Helper()
	definition := `[order]
exec = "placeholder"
trigger = "cooldown"
interval = "1h"
reserved_dispatch = true
`
	if noWorkGate {
		definition += "no_work_gate = true\n"
	}
	order, err := orders.Parse([]byte(definition))
	if err != nil {
		t.Fatalf("Parse reserved order %q: %v", name, err)
	}
	order.Name = name
	order.Exec = name
	return order
}

func ordinaryExecOrder(name string) orders.Order {
	return orders.Order{
		Name:     name,
		Exec:     name,
		Trigger:  "cooldown",
		Interval: "1h",
	}
}

func countCallsWithPrefix(counts map[string]int, prefix string) int {
	total := 0
	for command, count := range counts {
		if strings.HasPrefix(command, prefix) {
			total += count
		}
	}
	return total
}

func TestOrderDispatchReservedCapacityDoesNotConsumeOrdinaryBudget(t *testing.T) {
	t.Skip("reserved-capacity dispatch pool (separate budget, cap of 3, tick rotation) is not implemented yet; tracked in ga-1ocm3f")
	store := beads.NewMemStore()
	recorder := &reservedDispatchExecRecorder{}
	ordersToDispatch := []orders.Order{
		ordinaryExecOrder("ordinary-a"),
		ordinaryExecOrder("ordinary-b"),
		ordinaryExecOrder("ordinary-c"),
		ordinaryExecOrder("ordinary-d"),
		reservedExecOrder(t, "reserved-a", false),
		reservedExecOrder(t, "reserved-b", false),
		reservedExecOrder(t, "reserved-c", false),
	}
	m := buildOrderDispatcherFromListExec(ordersToDispatch, store, nil, recorder.run, nil).(*memoryOrderDispatcher)
	m.maxDispatchesPerTick = 2

	now := time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC)
	m.dispatch(context.Background(), t.TempDir(), now)
	drainOrderDispatch(t, m)

	counts := recorder.counts()
	for _, name := range []string{"reserved-a", "reserved-b", "reserved-c"} {
		if got := counts[name]; got != 1 {
			t.Errorf("%s dispatches = %d, want 1; all reserved orders must fire beside an exhausted ordinary lane", name, got)
		}
	}
	if got := countCallsWithPrefix(counts, "ordinary-"); got != 2 {
		t.Errorf("ordinary dispatches = %d, want 2 (the configured ordinary budget)", got)
	}
	if got := countCallsWithPrefix(counts, "reserved-"); got != 3 {
		t.Errorf("reserved dispatches = %d, want 3 (the separate reserved capacity)", got)
	}
}

func TestOrderDispatchReservedCapacityIsCappedAtThree(t *testing.T) {
	t.Skip("reserved-capacity dispatch pool (separate budget, cap of 3, tick rotation) is not implemented yet; tracked in ga-1ocm3f")
	store := beads.NewMemStore()
	recorder := &reservedDispatchExecRecorder{}
	ordersToDispatch := []orders.Order{
		ordinaryExecOrder("ordinary-a"),
		reservedExecOrder(t, "reserved-a", false),
		reservedExecOrder(t, "reserved-b", false),
		reservedExecOrder(t, "reserved-c", false),
		reservedExecOrder(t, "reserved-d", false),
	}
	m := buildOrderDispatcherFromListExec(ordersToDispatch, store, nil, recorder.run, nil).(*memoryOrderDispatcher)
	m.maxDispatchesPerTick = 1

	m.dispatch(context.Background(), t.TempDir(), time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC))
	drainOrderDispatch(t, m)

	counts := recorder.counts()
	if got := countCallsWithPrefix(counts, "reserved-"); got != 3 {
		t.Errorf("reserved dispatches = %d, want exactly 3", got)
	}
	if got := countCallsWithPrefix(counts, "ordinary-"); got != 1 {
		t.Errorf("ordinary dispatches = %d, want 1 (unused ordinary work remains budget-bound)", got)
	}
}

func TestOrderDispatchReservedUnusedCapacityDoesNotIncreaseOrdinaryBudget(t *testing.T) {
	t.Skip("reserved-capacity dispatch pool (separate budget, cap of 3, tick rotation) is not implemented yet; tracked in ga-1ocm3f")
	store := beads.NewMemStore()
	recorder := &reservedDispatchExecRecorder{}
	ordersToDispatch := []orders.Order{
		ordinaryExecOrder("ordinary-a"),
		ordinaryExecOrder("ordinary-b"),
		ordinaryExecOrder("ordinary-c"),
		ordinaryExecOrder("ordinary-d"),
		reservedExecOrder(t, "reserved-a", false),
	}
	m := buildOrderDispatcherFromListExec(ordersToDispatch, store, nil, recorder.run, nil).(*memoryOrderDispatcher)
	m.maxDispatchesPerTick = 2

	m.dispatch(context.Background(), t.TempDir(), time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC))
	drainOrderDispatch(t, m)

	counts := recorder.counts()
	if got := countCallsWithPrefix(counts, "ordinary-"); got != 2 {
		t.Errorf("ordinary dispatches = %d, want 2; unused reserved capacity must not enlarge the ordinary budget", got)
	}
	if got := counts["reserved-a"]; got != 1 {
		t.Errorf("reserved-a dispatches = %d, want 1", got)
	}
}

func TestOrderDispatchReservedOverflowRotatesAcrossTicks(t *testing.T) {
	t.Skip("reserved-capacity dispatch pool (separate budget, cap of 3, tick rotation) is not implemented yet; tracked in ga-1ocm3f")
	store := beads.NewMemStore()
	recorder := &reservedDispatchExecRecorder{}
	names := []string{"reserved-a", "reserved-b", "reserved-c", "reserved-d"}
	ordersToDispatch := make([]orders.Order, 0, len(names))
	for _, name := range names {
		order := reservedExecOrder(t, name, false)
		order.Interval = "0s" // Keep every order due so progress proves rotation, not cooldown suppression.
		ordersToDispatch = append(ordersToDispatch, order)
	}
	m := buildOrderDispatcherFromListExec(ordersToDispatch, store, nil, recorder.run, nil).(*memoryOrderDispatcher)
	m.maxDispatchesPerTick = 1

	now := time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC)
	for tick := 0; tick < 2; tick++ {
		m.dispatch(context.Background(), t.TempDir(), now.Add(time.Duration(tick)*time.Second))
		drainOrderDispatch(t, m)
	}

	counts := recorder.counts()
	if got := countCallsWithPrefix(counts, "reserved-"); got != 6 {
		t.Fatalf("reserved dispatches across two ticks = %d, want 6 (three per tick)", got)
	}
	for _, name := range names {
		if got := counts[name]; got == 0 {
			t.Errorf("%s never dispatched; reserved overflow must make progress across ticks", name)
		}
	}
}

func TestOrderDispatchReservedOrdersRemainSubjectToSuspension(t *testing.T) {
	tests := []struct {
		name  string
		order orders.Order
		cfg   *config.City
	}{
		{
			name:  "city",
			order: reservedExecOrder(t, "reserved-city", false),
			cfg:   &config.City{Workspace: config.Workspace{SuspendedOnStart: true}},
		},
		{
			name: "rig",
			order: func() orders.Order {
				order := reservedExecOrder(t, "reserved-rig", false)
				order.Rig = "frozen"
				return order
			}(),
			cfg: &config.City{Rigs: []config.Rig{{Name: "frozen", Path: "/frozen", SuspendedOnStart: true}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := beads.NewMemStore()
			recorder := &reservedDispatchExecRecorder{}
			m := buildOrderDispatcherFromListExec([]orders.Order{tt.order}, store, nil, recorder.run, nil).(*memoryOrderDispatcher)
			m.cfg = tt.cfg
			cityPath := t.TempDir()
			m.cityPath = cityPath

			m.dispatch(context.Background(), cityPath, time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC))
			drainOrderDispatch(t, m)

			if got := len(recorder.counts()); got != 0 {
				t.Fatalf("reserved order dispatches while %s is suspended = %d, want 0", tt.name, got)
			}
		})
	}
}

func TestOrderDispatchReservedOrdersPreserveOpenWorkPolicy(t *testing.T) {
	tests := []struct {
		name       string
		noWorkGate bool
		wantCalls  int
	}{
		{name: "default gate blocks", wantCalls: 0},
		{name: "explicit no-work-gate opt-out still fires", noWorkGate: true, wantCalls: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const orderName = "reserved-open-work"
			store := beads.NewMemStore()
			openWork, err := store.Create(beads.Bead{
				Title:    "mol-do-work",
				Labels:   []string{"order-run:" + orderName},
				Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp},
			})
			if err != nil {
				t.Fatalf("Create open work: %v", err)
			}
			recorder := &reservedDispatchExecRecorder{}
			order := reservedExecOrder(t, orderName, tt.noWorkGate)
			m := buildOrderDispatcherFromListExec([]orders.Order{order}, store, nil, recorder.run, nil).(*memoryOrderDispatcher)

			m.dispatch(context.Background(), t.TempDir(), openWork.CreatedAt.Add(2*time.Hour))
			drainOrderDispatch(t, m)

			if got := recorder.counts()[orderName]; got != tt.wantCalls {
				t.Fatalf("reserved order dispatches = %d, want %d", got, tt.wantCalls)
			}
		})
	}
}

func TestOrderDispatchReservedOrdersRemainSubjectToTriggerEligibility(t *testing.T) {
	const orderName = "reserved-not-due"
	store := beads.NewMemStore()
	recent, err := store.Create(beads.Bead{
		Title:  "recent completed run",
		Status: "closed",
		Labels: []string{"order-run:" + orderName},
	})
	if err != nil {
		t.Fatalf("Create recent run: %v", err)
	}
	now := recent.CreatedAt.Add(time.Minute)

	recorder := &reservedDispatchExecRecorder{}
	m := buildOrderDispatcherFromListExec([]orders.Order{
		reservedExecOrder(t, orderName, false),
		ordinaryExecOrder("ordinary-due"),
	}, store, nil, recorder.run, nil).(*memoryOrderDispatcher)
	m.maxDispatchesPerTick = 1

	m.dispatch(context.Background(), t.TempDir(), now)
	drainOrderDispatch(t, m)

	counts := recorder.counts()
	if got := counts[orderName]; got != 0 {
		t.Errorf("not-due reserved order dispatches = %d, want 0", got)
	}
	if got := counts["ordinary-due"]; got != 1 {
		t.Errorf("ordinary due order dispatches = %d, want 1", got)
	}
}

func TestOrderDispatchReservedOrderDoesNotDoubleDispatchOnRepeatTick(t *testing.T) {
	t.Run("in-flight tracking bead", func(t *testing.T) {
		const orderName = "reserved-single-flight"
		store := beads.NewMemStore()
		started := make(chan struct{})
		release := make(chan struct{})
		recorder := &reservedDispatchExecRecorder{started: started, release: release}
		m := buildOrderDispatcherFromListExec([]orders.Order{reservedExecOrder(t, orderName, false)}, store, nil, recorder.run, nil).(*memoryOrderDispatcher)
		cityPath := t.TempDir()
		var releaseOne sync.Once
		releaseRun := func() { releaseOne.Do(func() { close(release) }) }
		t.Cleanup(func() {
			releaseRun()
			drainOrderDispatch(t, m)
		})

		now := time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC)
		m.dispatch(context.Background(), cityPath, now)
		awaitClose(t, started, "reserved order exec start")
		m.dispatch(context.Background(), cityPath, now.Add(time.Second))

		if got := len(trackingBeads(t, store, "order-run:"+orderName)); got != 1 {
			t.Fatalf("tracking runs across an immediate in-flight repeat tick = %d, want 1", got)
		}
		releaseRun()
		drainOrderDispatch(t, m)
		if got := recorder.counts()[orderName]; got != 1 {
			t.Fatalf("exec calls across an immediate in-flight repeat tick = %d, want 1", got)
		}
	})

	t.Run("completed cooldown", func(t *testing.T) {
		const orderName = "reserved-cooldown"
		store := beads.NewMemStore()
		recorder := &reservedDispatchExecRecorder{}
		m := buildOrderDispatcherFromListExec([]orders.Order{reservedExecOrder(t, orderName, false)}, store, nil, recorder.run, nil).(*memoryOrderDispatcher)
		cityPath := t.TempDir()

		now := time.Now()
		m.dispatch(context.Background(), cityPath, now)
		drainOrderDispatch(t, m)
		firstRuns := trackingBeads(t, store, "order-run:"+orderName)
		if len(firstRuns) != 1 {
			t.Fatalf("tracking runs after first tick = %d, want 1", len(firstRuns))
		}
		m.dispatch(context.Background(), cityPath, firstRuns[0].CreatedAt.Add(time.Second))
		drainOrderDispatch(t, m)

		if got := recorder.counts()[orderName]; got != 1 {
			t.Fatalf("exec calls across an immediate completed repeat tick = %d, want 1", got)
		}
		if got := len(trackingBeads(t, store, "order-run:"+orderName)); got != 1 {
			t.Fatalf("tracking runs across an immediate completed repeat tick = %d, want 1", got)
		}
	})
}
