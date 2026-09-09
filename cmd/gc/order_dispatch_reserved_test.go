package main

import (
	"context"
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
