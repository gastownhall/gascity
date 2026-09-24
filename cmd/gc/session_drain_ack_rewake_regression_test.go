package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Regression guards for "drained seats never re-wake" (maintainer city,
// 2026-09-22/23; seat-claim defect 4).
//
// On the mc deploy line an agent drain-ack whose keyed re-authorization failed
// (refusal=lease_invalid/work_not_closed, lease_invalid/not_pool_managed, or an
// incomplete liveness observation) left the session row parked in
// state=draining/state_reason=drain-ack-stop-pending. The keyed lane parked it
// forever, its deadline "release" only requested a legacy pass, and the legacy
// finalizer was excluded from the row (the keyed exclusion fails open only for
// POOL-managed rows after 30m). Nobody finalized the row, so it held the
// pool's only slot / the named seat's identity while demand waited, until an
// operator ran `gc session close`.
//
// Main has no keyed reconciler: the stop-pending finalizer
// (finalizeDrainAckStopPendingSessions) owns every stop-pending row and the
// drain-ack decision needs no authorization lease. These tests drive the same
// tick ordering CityRuntime.tick uses (stop-pending finalize -> desired state
// -> session sync -> reconcile) and pin the outcome the city needs to run
// hands-off: a drained seat with pending demand restarts (named seat) or is
// replaced (pool seat) with no operator action, whatever became of the
// acknowledgement's trigger work.

// drainAckRewakeTick runs one controller tick in CityRuntime.tick order.
func drainAckRewakeTick(
	t *testing.T,
	cityPath string,
	cfg *config.City,
	sp *runtime.Fake,
	store beads.Store,
	dops drainOps,
	dt *drainTracker,
	tracker *asyncStartTracker,
	clk clock.Clock,
	out io.Writer,
) {
	t.Helper()
	before, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loadSessionBeads(before finalize): %v", err)
	}
	finalizeDrainAckStopPendingSessions(
		cityPath, cfg, sp, beads.SessionStore{Store: store}, nil, sessionInfosFromBeads(before),
		dops, dt, tracker, clk, events.Discard, out,
	)
	ds := buildDesiredState(cfg.EffectiveCityName(), cityPath, clk.Now().UTC(), cfg, sp, store, out)
	cfgNames := configuredSessionNames(cfg, cfg.EffectiveCityName(), store)
	syncSessionBeads(cityPath, store, ds.State, sp, cfgNames, cfg, clk, out, true)
	sessions, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	poolDesired := PoolDesiredCounts(ComputePoolDesiredStates(cfg, ds.AssignedWorkBeads, sessionInfosFromBeads(sessions), ds.ScaleCheckCounts))
	if poolDesired == nil {
		poolDesired = make(map[string]int)
	}
	mergeNamedSessionDemand(poolDesired, ds.NamedSessionDemand, cfg)
	snap := newSessionBeadSnapshot(sessions)
	reconcileSessionBeadsAtPathWithNamedDemand(
		context.Background(), cityPath, snap.OpenForReconcile(), snap, ds.State, cfgNames, cfg, sp,
		store, dops, ds.AssignedWorkBeads, nil, nil, dt, nil, poolDesired,
		ds.NamedSessionDemand, ds.NamedSessionRoutedDemand, ds.StoreQueryPartial, ds.WorkSet, cfg.EffectiveCityName(),
		nil, clk, events.Discard, 0, 0, out, out,
		withAsyncDrainAckStopTracker(tracker),
	)
}

// settleAsyncDrainAckStops gives the detached drain-ack stop goroutines a
// bounded window to land before the next tick observes liveness.
func settleAsyncDrainAckStops(sp *runtime.Fake, name string) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && sp.IsRunning(name) {
		time.Sleep(10 * time.Millisecond)
	}
}

func dumpDrainAckRewakeState(t *testing.T, store beads.Store, sp *runtime.Fake, tick int) {
	t.Helper()
	all, err := store.List(beads.ListQuery{Type: sessionBeadType, IncludeClosed: true})
	if err != nil {
		t.Logf("tick %d: listing sessions: %v", tick, err)
		return
	}
	for _, b := range all {
		t.Logf("tick %d: %s status=%s state=%s reason=%s name=%s running=%v",
			tick, b.ID, b.Status, b.Metadata["state"], b.Metadata["state_reason"],
			b.Metadata["session_name"], sp.IsRunning(b.Metadata["session_name"]))
	}
}

// TestDrainAckStopPendingPoolSeatWithOpenTriggerIsReplaced pins the pool half:
// a max=1 (canonical, single-slot) pool seat whose agent drain-acked while its
// trigger work is still OPEN — the shape the deploy line refused as
// lease_invalid/work_not_closed — must release the only slot and a replacement
// must start for the still-ready routed work.
func TestDrainAckStopPendingPoolSeatWithOpenTriggerIsReplaced(t *testing.T) {
	cases := []struct {
		name string
		// parked: the row is already parked in drain-ack-stop-pending with a
		// dead runtime and an agent-stamped acknowledgement (the state the
		// deploy-line incident rows sat in). Otherwise the seat is live and
		// acks this tick.
		parked bool
	}{
		{name: "live_seat_acks_with_open_trigger"},
		{name: "parked_stop_pending_dead_runtime", parked: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			cityPath := t.TempDir()
			writeCityTOML(t, cityPath, "trace-town", "worker")
			cfg := &config.City{
				Workspace: config.Workspace{Name: "trace-town"},
				Session:   config.SessionConfig{Provider: "fake"},
				Agents: []config.Agent{{
					Name:              "worker",
					Dir:               "repo",
					StartCommand:      "true",
					MinActiveSessions: intPtr(0),
					MaxActiveSessions: intPtr(1),
				}},
			}
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			trigger := createRoutedReadyBeadForReplacement(t, store, "repo/worker", "trigger work still open")

			seat := createCanonicalPoolSession(t, store, &cfg.Agents[0], now, 1)
			setPoolSessionActive(t, store, seat.ID)
			if err := store.SetMetadata(seat.ID, "gc.trigger_bead_id", trigger.ID); err != nil {
				t.Fatalf("stamp trigger: %v", err)
			}
			seat, err := store.Get(seat.ID)
			if err != nil {
				t.Fatalf("reload seat: %v", err)
			}
			name := seat.Metadata["session_name"]
			token := seat.Metadata["instance_token"]
			dops := newDrainOps(sp)
			if tc.parked {
				if err := store.SetMetadataBatch(seat.ID, map[string]string{
					"state":                              "draining",
					"state_reason":                       "drain-ack-stop-pending",
					"drain_at":                           now.Add(-2 * time.Hour).Format(time.RFC3339),
					"drain_ack_source":                   "agent",
					"drain_ack_requester_session_id":     seat.ID,
					"drain_ack_requester_instance_token": token,
				}); err != nil {
					t.Fatalf("park seat: %v", err)
				}
			} else {
				if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
					t.Fatalf("start seat runtime: %v", err)
				}
				if err := dops.setDrainAck(name); err != nil {
					t.Fatalf("setDrainAck: %v", err)
				}
			}

			dt := newDrainTracker()
			tracker := &asyncStartTracker{}
			clk := &clock.Fake{Time: now}
			var out strings.Builder
			replacementRan := false
			originalClosed := false
			for tick := 0; tick < 6 && (!replacementRan || !originalClosed); tick++ {
				drainAckRewakeTick(t, cityPath, cfg, sp, store, dops, dt, tracker, clk, &out)
				if tick == 0 && !tc.parked {
					settleAsyncDrainAckStops(sp, name)
				}
				dumpDrainAckRewakeState(t, store, sp, tick)
				if got, err := store.Get(seat.ID); err == nil && got.Status == "closed" {
					originalClosed = true
				}
				open, err := loadSessionBeads(store)
				if err != nil {
					t.Fatalf("loadSessionBeads: %v", err)
				}
				for _, b := range open {
					if b.ID != seat.ID && b.Metadata["template"] == "repo/worker" && sp.IsRunning(b.Metadata["session_name"]) {
						replacementRan = true
					}
				}
				clk.Advance(30 * time.Second)
			}
			if !originalClosed {
				t.Errorf("drain-acked pool seat %s never released its slot (bead still open)\n%s", seat.ID, out.String())
			}
			if !replacementRan {
				t.Fatalf("no replacement pool seat started for the still-open routed trigger %s: the drained seat blocked the pool's only slot\n%s", trigger.ID, out.String())
			}
		})
	}
}

// TestDrainAckStopPendingNamedSeatRewakesOnDemand pins the named half — the
// olivia incident: an on_demand named seat (NOT pool-managed, so the deploy
// line refused it as lease_invalid/not_pool_managed and never handed it back)
// parked in drain-ack-stop-pending with a stale agent acknowledgement whose
// trigger is gone, while assignee-direct work waits for it. The finalizer must
// settle the stop and the SAME seat must wake for its work.
func TestDrainAckStopPendingNamedSeatRewakesOnDemand(t *testing.T) {
	for _, live := range []bool{false, true} {
		name := "runtime_dead"
		if live {
			name = "runtime_live"
		}
		t.Run(name, func(t *testing.T) {
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Agents: []config.Agent{{
					Name:              "mayor",
					StartCommand:      "true",
					MaxActiveSessions: intPtr(1),
					WorkQuery:         "printf ''",
				}},
				NamedSessions: []config.NamedSession{{Template: "mayor", Mode: "on_demand"}},
			}
			sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "mayor")
			cityPath := t.TempDir()
			store := beads.NewMemStore()
			now := time.Now().UTC()
			clk := &clock.Fake{Time: now}
			sp := runtime.NewFake()
			if _, err := store.Create(beads.Bead{Title: "work for the named seat", Type: "task", Status: "open", Assignee: "mayor"}); err != nil {
				t.Fatalf("create demand: %v", err)
			}
			seat, err := store.Create(beads.Bead{
				Title:  sessionName,
				Type:   sessionBeadType,
				Labels: []string{sessionBeadLabel},
				Metadata: map[string]string{
					"session_name":                       sessionName,
					"alias":                              "mayor",
					"template":                           "mayor",
					"state":                              "draining",
					"state_reason":                       "drain-ack-stop-pending",
					"drain_at":                           now.Add(-2 * time.Hour).Format(time.RFC3339),
					"generation":                         "2",
					"instance_token":                     "tok-1",
					"continuation_epoch":                 "1",
					"drain_ack_source":                   "agent",
					"drain_ack_requester_instance_token": "tok-1",
					"gc.trigger_bead_id":                 "trigger-deleted-after-scope-finalized",
					namedSessionMetadataKey:              "true",
					namedSessionIdentityMetadata:         "mayor",
					namedSessionModeMetadata:             "on_demand",
				},
			})
			if err != nil {
				t.Fatalf("create parked seat: %v", err)
			}
			if err := store.SetMetadata(seat.ID, "drain_ack_requester_session_id", seat.ID); err != nil {
				t.Fatalf("stamp requester: %v", err)
			}
			dops := newDrainOps(sp)
			if live {
				// The acknowledging pane is still up: the agent acked and kept
				// sitting at its prompt.
				if err := sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
					t.Fatalf("start seat runtime: %v", err)
				}
				_ = sp.SetMeta(sessionName, "GC_SESSION_ID", seat.ID)
				_ = sp.SetMeta(sessionName, "GC_INSTANCE_TOKEN", "tok-1")
				if err := dops.setDrainAck(sessionName); err != nil {
					t.Fatalf("setDrainAck: %v", err)
				}
			}

			dt := newDrainTracker()
			tracker := &asyncStartTracker{}
			var out strings.Builder
			startsBefore := 0
			for _, call := range sp.SnapshotCalls() {
				if call.Method == "Start" && call.Name == sessionName {
					startsBefore++
				}
			}
			rewoke := false
			for tick := 0; tick < 6 && !rewoke; tick++ {
				drainAckRewakeTick(t, cityPath, cfg, sp, store, dops, dt, tracker, clk, &out)
				if tick == 0 && live {
					settleAsyncDrainAckStops(sp, sessionName)
				}
				dumpDrainAckRewakeState(t, store, sp, tick)
				starts := 0
				for _, call := range sp.SnapshotCalls() {
					if call.Method == "Start" && call.Name == sessionName {
						starts++
					}
				}
				got, err := store.Get(seat.ID)
				if err != nil {
					t.Fatalf("reload seat: %v", err)
				}
				rewoke = starts > startsBefore && sp.IsRunning(sessionName) && got.Status != "closed" &&
					got.Metadata["state_reason"] != "drain-ack-stop-pending"
				clk.Advance(30 * time.Second)
			}
			if !rewoke {
				t.Fatalf("named seat %s parked in drain-ack-stop-pending never re-woke for its assigned work\n%s", seat.ID, out.String())
			}
			open, err := loadSessionBeads(store)
			if err != nil {
				t.Fatalf("loadSessionBeads: %v", err)
			}
			if len(open) != 1 || open[0].ID != seat.ID {
				ids := make([]string, 0, len(open))
				for _, b := range open {
					ids = append(ids, b.ID)
				}
				t.Fatalf("open session beads = %v, want exactly the original named seat %s", ids, seat.ID)
			}
		})
	}
}
