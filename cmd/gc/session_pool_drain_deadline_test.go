package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

var (
	errStopUnavailableForTest  = errors.New("stop failed: session unavailable")
	errCloseUnavailableForTest = errors.New("close failed: store unavailable")
)

// poolSeatName is the runtime name every fixture in this file pins: a pool
// slot's name is a pure function of its identity, and holding it open is the
// whole defect.
const poolSeatName = "worker-1-pool"

// stuckDrainedPoolSeat parks a pool-managed session bead in the drain state the
// ga-rxhu2 specimen was found in: status=open, drain never finalized, drain_at
// stamped drainAge ago, and its runtime still live. Production held this shape
// for 3d10h (gcg-5436965277275192 / beads--gc__implementation-worker-1-pool);
// an open session bead owns its session_name and a pool slot's name is a pure
// function of its identity, so the template sat at zero seats the whole window.
//
// The seat is registered in the desired set because that is the shape with no
// exit on this line: an UNdesired drained seat is eventually retired by the
// orphan drain (measured: 8 ticks), while a desired one is re-healed to
// state=awake every tick and never has its runtime stopped at all (measured:
// 15 ticks, zero provider stops). state carries the raw persisted value so both
// the fresh drain and the healed-awake ghost it decays into can be pinned.
func stuckDrainedPoolSeat(t *testing.T, env *reconcilerTestEnv, state string, drainAge time.Duration) beads.Bead {
	t.Helper()
	env.addDesired(poolSeatName, "worker", false)
	seat := env.createSessionBead(poolSeatName, "worker")
	env.setSessionMetadata(&seat, map[string]string{
		"state":                state,
		"sleep_reason":         "drained",
		"last_woke_at":         "",
		"pool_slot":            "1",
		poolManagedMetadataKey: boolMetadata(true),
		"session_origin":       "ephemeral",
		"drain_at":             env.clk.Now().Add(-drainAge).UTC().Format(time.RFC3339),
	})
	if err := env.sp.Start(context.Background(), poolSeatName, runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("start seat runtime %q: %v", poolSeatName, err)
	}
	return seat
}

func poolSeatEnv() *reconcilerTestEnv {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
	}
	return env
}

func drainDeadlineRetiredEvents(rec *events.Fake) []events.Event {
	out := make([]events.Event, 0, 1)
	for _, e := range rec.Events {
		if e.Type == events.SessionPoolSlotRetiredAtDrainDeadline {
			out = append(out, e)
		}
	}
	return out
}

// Pin 1 (RED before the fix). A pool-managed seat stuck in drain past the
// retire deadline, with no assigned work, is stopped and force-retired: the
// bead closes with the drained close reason plus greppable deadline
// provenance, and its session_name is free again.
//
// Both shapes of the same stuck seat are pinned. "drained" is how it enters;
// "awake" is what the reconciler heals it into on the very next tick, because
// ProjectLifecycle re-reads a live runtime as awake. The healed ghost is what
// the fleet actually looks like, so a bound that only recognized the literal
// drained state would miss almost every real instance.
func TestReconcileSessionBeads_DrainedPoolSlotIsRetiredAtTheDeadline(t *testing.T) {
	for _, state := range []string{"drained", "awake"} {
		t.Run("state_"+state, func(t *testing.T) {
			env := poolSeatEnv()
			rec := events.NewFake()
			env.rec = rec
			seat := stuckDrainedPoolSeat(t, env, state, poolSlotDrainRetireDeadline+time.Minute)

			env.reconcile([]beads.Bead{seat})

			got, err := env.store.Get(seat.ID)
			if err != nil {
				t.Fatalf("Get(%s): %v", seat.ID, err)
			}
			if got.Status != "closed" {
				t.Fatalf("status = %q, want closed — a pool seat stuck in drain past the deadline must be retired (metadata=%v)", got.Status, got.Metadata)
			}
			if got.Metadata["state"] != "drained" {
				t.Fatalf("state = %q, want drained", got.Metadata["state"])
			}
			if got.Metadata["close_reason"] == "" {
				t.Fatalf("close_reason is empty; want the canonical drained reason")
			}
			if got.Metadata[drainFinalizeMetadataKey] != drainFinalizeDeadline {
				t.Fatalf("%s = %q, want %q — forced retirements must be greppable",
					drainFinalizeMetadataKey, got.Metadata[drainFinalizeMetadataKey], drainFinalizeDeadline)
			}
			if env.sp.IsRunning(poolSeatName) {
				t.Fatal("runtime still running after the seat was retired; the close must never outlive its runtime")
			}

			snapshot, err := loadSessionBeadSnapshot(env.store)
			if err != nil {
				t.Fatalf("loadSessionBeadSnapshot: %v", err)
			}
			if openSessionNameTaken(snapshot, poolSeatName) {
				t.Fatal("session_name still held by an open bead; the pool still cannot mint a seat")
			}

			fired := drainDeadlineRetiredEvents(rec)
			if len(fired) != 1 {
				t.Fatalf("emitted %d %s events, want exactly 1 — the residual drain tail must stay countable",
					len(fired), events.SessionPoolSlotRetiredAtDrainDeadline)
			}
			if fired[0].SessionID != seat.ID {
				t.Fatalf("event SessionID = %q, want %q", fired[0].SessionID, seat.ID)
			}
			var payload map[string]any
			if err := json.Unmarshal(fired[0].Payload, &payload); err != nil {
				t.Fatalf("unmarshal retire payload: %v", err)
			}
			if payload["session_name"] != poolSeatName {
				t.Fatalf("payload session_name = %v, want worker-1-pool (payload=%v)", payload["session_name"], payload)
			}
			if payload["drain_at"] == "" || payload["drain_at"] == nil {
				t.Fatalf("payload drain_at missing; the age of the stuck drain is the whole diagnostic (payload=%v)", payload)
			}
			if age, _ := payload["drain_age_seconds"].(float64); age < poolSlotDrainRetireDeadline.Seconds() {
				t.Fatalf("payload drain_age_seconds = %v, want at least the deadline (payload=%v)", payload["drain_age_seconds"], payload)
			}
		})
	}
}

// The seat this bound exists for has no exit of its own on this line. Without
// the fix it survives every tick the controller will ever run: re-healed to
// awake, never stopped, its runtime name held open forever.
func TestReconcileSessionBeads_StuckDrainedPoolSlotNeverConvergesWithoutTheBound(t *testing.T) {
	env := poolSeatEnv()
	seat := stuckDrainedPoolSeat(t, env, "drained", time.Minute)

	// Inside the deadline the ordinary machinery gets every chance to converge.
	for i := 0; i < 15; i++ {
		cur, err := env.store.Get(seat.ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", seat.ID, err)
		}
		if cur.Status == "closed" {
			t.Fatalf("seat converged on its own at tick %d; this fixture no longer models the stuck population", i)
		}
		env.reconcile([]beads.Bead{cur})
		env.clk.Time = env.clk.Time.Add(time.Minute)
	}
	if !env.sp.IsRunning(poolSeatName) {
		t.Fatal("runtime stopped on its own inside the deadline; this fixture no longer models the stuck population")
	}

	// Past the deadline the bound retires it on the next tick.
	cur, err := env.store.Get(seat.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", seat.ID, err)
	}
	env.clk.Time = env.clk.Time.Add(poolSlotDrainRetireDeadline)
	env.reconcile([]beads.Bead{cur})

	got, err := env.store.Get(seat.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", seat.ID, err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed once the drain outlived the deadline (metadata=%v)", got.Status, got.Metadata)
	}
}

// Pin 2 (RED before the fix). The end-to-end convergence the operator never
// got: while the stuck bead is open the pool cannot mint the slot at all
// (its runtime name is a pure function of its identity — ga-vcjr9 — so there
// is nothing to route around), and once the deadline retires the bead the very
// next availability check succeeds.
func TestReconcileSessionBeads_RetiredDrainDeadlineSlotUnblocksPoolMinting(t *testing.T) {
	env := poolSeatEnv()
	seat := stuckDrainedPoolSeat(t, env, "drained", poolSlotDrainRetireDeadline+time.Minute)

	before, err := loadSessionBeadSnapshot(env.store)
	if err != nil {
		t.Fatalf("loadSessionBeadSnapshot(before): %v", err)
	}
	if err := ensurePoolSessionNameAvailable(env.store, env.cfg, before, poolSeatName, "worker-1"); err == nil {
		t.Fatal("precondition broken: the stuck open bead must hold its runtime name")
	}

	env.reconcile([]beads.Bead{seat})

	after, err := loadSessionBeadSnapshot(env.store)
	if err != nil {
		t.Fatalf("loadSessionBeadSnapshot(after): %v", err)
	}
	if err := ensurePoolSessionNameAvailable(env.store, env.cfg, after, poolSeatName, "worker-1"); err != nil {
		t.Fatalf("pool still cannot mint %q after the deadline retirement: %v", poolSeatName, err)
	}
}

// Pin 3 (negative). A seat holding live assigned work is never force-retired,
// and its runtime is never stopped: that population belongs to ga-ee8eo.
func TestReconcileSessionBeads_DrainDeadlineRetireRefusesWhenSeatHoldsAssignedWork(t *testing.T) {
	env := poolSeatEnv()
	seat := stuckDrainedPoolSeat(t, env, "drained", poolSlotDrainRetireDeadline+time.Minute)

	if _, err := env.store.Create(beads.Bead{
		Title:    "claimed work",
		Type:     "task",
		Status:   "in_progress",
		Assignee: seat.ID,
	}); err != nil {
		t.Fatalf("create assigned work: %v", err)
	}

	env.reconcile([]beads.Bead{seat})

	got, err := env.store.Get(seat.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", seat.ID, err)
	}
	if got.Status == "closed" {
		t.Fatalf("seat closed while it still held assigned work: metadata=%v", got.Metadata)
	}
	if !env.sp.IsRunning(poolSeatName) {
		t.Fatal("runtime stopped for a seat that still holds assigned work")
	}
}

// Pin 4 (negative). Inside the deadline nothing is forced, so a normal drain
// always finalizes through the existing drain-ack path.
func TestReconcileSessionBeads_DrainDeadlineRetireRefusesInsideTheDeadline(t *testing.T) {
	env := poolSeatEnv()
	seat := stuckDrainedPoolSeat(t, env, "drained", 2*time.Minute)

	env.reconcile([]beads.Bead{seat})

	got, err := env.store.Get(seat.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", seat.ID, err)
	}
	if got.Metadata[drainFinalizeMetadataKey] == drainFinalizeDeadline {
		t.Fatalf("seat retired by the deadline path only %v into its drain: metadata=%v", 2*time.Minute, got.Metadata)
	}
	if !env.sp.IsRunning(poolSeatName) {
		t.Fatal("runtime stopped inside the drain deadline")
	}
}

// Pin 5 (negative). A stop whose result is unknown must not authorize a close:
// closing over a surviving runtime recreates the "live agent, no owning bead"
// shape the orphan-runtime reaper was rejected for.
func TestReconcileSessionBeads_DrainDeadlineRetireRefusesWhenStopUnconfirmed(t *testing.T) {
	cases := []struct {
		name      string
		breakStop func(sp *runtime.Fake, sessionName string)
	}{
		{
			name: "stop_returns_an_error",
			breakStop: func(sp *runtime.Fake, sessionName string) {
				sp.StopErrors = map[string]error{sessionName: errStopUnavailableForTest}
			},
		},
		{
			// The dangerous shape: the stop reports success and the agent
			// survives it anyway (SIGHUP ignored, reparented, raced the grace
			// period). Trusting the return value here is how a live agent ends
			// up with no owning bead.
			name: "stop_reports_success_but_runtime_survives",
			breakStop: func(sp *runtime.Fake, sessionName string) {
				sp.StopLeavesRunning = map[string]bool{sessionName: true}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := poolSeatEnv()
			rec := events.NewFake()
			env.rec = rec
			seat := stuckDrainedPoolSeat(t, env, "drained", poolSlotDrainRetireDeadline+time.Minute)
			tc.breakStop(env.sp, poolSeatName)

			env.reconcile([]beads.Bead{seat})

			got, err := env.store.Get(seat.ID)
			if err != nil {
				t.Fatalf("Get(%s): %v", seat.ID, err)
			}
			if got.Status == "closed" {
				t.Fatalf("seat closed after an unconfirmed stop: metadata=%v", got.Metadata)
			}
			if fired := drainDeadlineRetiredEvents(rec); len(fired) != 0 {
				t.Fatalf("emitted %d retirement events for an unconfirmed stop, want 0", len(fired))
			}
		})
	}
}

// Negative — a live seat carrying a stale drain marker. drain_at is never
// cleared once stamped, so age alone is not evidence of anything: a seat whose
// drain was CANCELED (scale-back-up returns it to the desired set) keeps a
// drain_at from hours ago while working normally. It is distinguished by the
// two markers a real drain leaves behind — a wake after drain_at, and no
// surviving drain sleep_reason — and it must never be force-retired.
func TestReconcileSessionBeads_DrainDeadlineRetireSkipsLiveSeatWithStaleDrainMarker(t *testing.T) {
	env := poolSeatEnv()
	env.addDesired(poolSeatName, "worker", false)
	seat := env.createSessionBead(poolSeatName, "worker")
	env.setSessionMetadata(&seat, map[string]string{
		"state":                "active",
		"pool_slot":            "1",
		poolManagedMetadataKey: boolMetadata(true),
		"session_origin":       "ephemeral",
		"drain_at":             env.clk.Now().Add(-90 * time.Minute).UTC().Format(time.RFC3339),
		"last_woke_at":         env.clk.Now().Add(-60 * time.Minute).UTC().Format(time.RFC3339),
	})
	if err := env.sp.Start(context.Background(), poolSeatName, runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("start seat runtime: %v", err)
	}

	env.reconcile([]beads.Bead{seat})

	got, err := env.store.Get(seat.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", seat.ID, err)
	}
	if got.Status == "closed" {
		t.Fatalf("a live seat that woke after its drain was retired as stuck: metadata=%v", got.Metadata)
	}
	if !env.sp.IsRunning(poolSeatName) {
		t.Fatal("killed the runtime of a live seat that only carried a stale drain_at")
	}
}

// Pin 6 (negative). The bound is scoped to pool-managed seats. A named or
// otherwise non-pool session in the same drain state keeps its bead: its
// identity is not disposable, and nothing about it starves a pool.
func TestReconcileSessionBeads_DrainDeadlineRetireSkipsNonPoolSession(t *testing.T) {
	env := poolSeatEnv()
	seat := env.createSessionBead("worker-named", "worker")
	env.setSessionMetadata(&seat, map[string]string{
		"state":        "drained",
		"sleep_reason": "drained",
		"drain_at":     env.clk.Now().Add(-poolSlotDrainRetireDeadline - time.Minute).UTC().Format(time.RFC3339),
	})
	if err := env.sp.Start(context.Background(), "worker-named", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("start named runtime: %v", err)
	}

	env.reconcile([]beads.Bead{seat})

	got, err := env.store.Get(seat.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", seat.ID, err)
	}
	if got.Metadata[drainFinalizeMetadataKey] == drainFinalizeDeadline {
		t.Fatalf("non-pool session retired by the pool-slot deadline path: metadata=%v", got.Metadata)
	}
}

// syncWriter serializes writes so a tick that spawns async drain-ack stop
// goroutines (which outlive the reconcile invocation by design and log on their
// own schedule) can share one sink with the caller without racing it.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// reconcileAsyncSafe drives a tick whose stderr is safe to share with the
// async stop goroutines the drain-ack stop-pending path queues.
func (e *reconcilerTestEnv) reconcileAsyncSafe(sessions []beads.Bead, stderr io.Writer) {
	poolDesired := make(map[string]int)
	for _, tp := range e.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	reconcileSessionBeadsAtPath(
		context.Background(), "", sessions, e.desiredState,
		configuredSessionNames(e.cfg, "", e.store), e.cfg, e.sp, e.store, nil, nil, nil, nil,
		e.dt, poolDesired, false, nil, "", nil, e.clk, e.rec, 0, 0, io.Discard, stderr,
		e.startOptions...,
	)
}

// reconcilePartial drives the same tick as env.reconcile but declares the
// store enumeration degraded, the way the controller does when a leg came back
// partial.
func (e *reconcilerTestEnv) reconcilePartial(sessions []beads.Bead, storeQueryPartial bool, opts ...startExecutionOption) {
	poolDesired := make(map[string]int)
	for _, tp := range e.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	reconcileSessionBeadsAtPath(
		context.Background(), "", sessions, e.desiredState,
		configuredSessionNames(e.cfg, "", e.store), e.cfg, e.sp, e.store, nil, nil, nil, nil,
		e.dt, poolDesired, storeQueryPartial, nil, "", nil, e.clk, e.rec, 0, 0, &e.stdout, &e.stderr,
		append(append([]startExecutionOption{}, e.startOptions...), opts...)...,
	)
}

// A seat that still owns work claimed under its ALIAS must never be touched.
// The agent claims beads as BEADS_ACTOR, which is alias-first, and a pool
// slot's alias diverges from its session_name exactly when the runtime name
// steps aside to "<identity>-pool" — the ga-rxhu2 specimen's own shape. A probe
// that reads only {bead ID, session_name, configured_named_identity} is blind
// to the agent's own claims on precisely the configuration this bound targets,
// and this is the first path that would turn that blindness into a Kill.
func TestReconcileSessionBeads_DrainDeadlineRetireSeesWorkClaimedUnderTheAlias(t *testing.T) {
	for _, tc := range []struct {
		name     string
		assignee string
	}{
		{name: "current_alias", assignee: "worker-1"},
		{name: "prior_alias_from_history", assignee: "worker-0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := poolSeatEnv()
			seat := stuckDrainedPoolSeat(t, env, "drained", poolSlotDrainRetireDeadline+time.Minute)
			env.setSessionMetadata(&seat, map[string]string{
				"alias":         "worker-1",
				"alias_history": "worker-0",
			})
			if _, err := env.store.Create(beads.Bead{
				Title: "claimed under the alias", Type: "task",
				Status: "in_progress", Assignee: tc.assignee,
			}); err != nil {
				t.Fatalf("create alias-claimed work: %v", err)
			}

			env.reconcile([]beads.Bead{seat})

			got, err := env.store.Get(seat.ID)
			if err != nil {
				t.Fatalf("Get(%s): %v", seat.ID, err)
			}
			if got.Status == "closed" {
				t.Fatalf("closed a seat still holding work claimed as %q: metadata=%v", tc.assignee, got.Metadata)
			}
			if !env.sp.IsRunning(poolSeatName) {
				t.Fatalf("KILLED a live agent holding work claimed as %q", tc.assignee)
			}
		})
	}
}

// Degraded input never authorizes a retirement. A partial store enumeration
// cannot prove a seat is idle, and the boot tick defers session closes because
// this exact fan-out (a multi-store work probe per candidate) is what #3288
// moved off the readiness path.
func TestReconcileSessionBeads_DrainDeadlineRetireDefersOnDegradedTick(t *testing.T) {
	for _, tc := range []struct {
		name    string
		partial bool
		opts    []startExecutionOption
	}{
		{name: "store_query_partial", partial: true},
		{name: "defer_closes_on_boot", opts: []startExecutionOption{withDeferSessionClosesOnBoot()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := poolSeatEnv()
			seat := stuckDrainedPoolSeat(t, env, "drained", poolSlotDrainRetireDeadline+time.Minute)

			env.reconcilePartial([]beads.Bead{seat}, tc.partial, tc.opts...)

			got, err := env.store.Get(seat.ID)
			if err != nil {
				t.Fatalf("Get(%s): %v", seat.ID, err)
			}
			if got.Status == "closed" {
				t.Fatalf("retired a seat on a degraded tick: metadata=%v", got.Metadata)
			}
			if !env.sp.IsRunning(poolSeatName) {
				t.Fatal("stopped a runtime on a degraded tick")
			}
			if got.Metadata[drainFinalizeMetadataKey] != "" {
				t.Fatalf("%s stamped on a degraded tick: %q", drainFinalizeMetadataKey, got.Metadata[drainFinalizeMetadataKey])
			}
		})
	}
}

// Every advisory blocker the pool-slot-freeable gate promises to honor is
// honored here too. Base delivers that guarantee only because the state heal
// rewrites `state` before that gate runs; this bound reads the RAW pre-heal
// state, so it must check the blockers itself or it silently overrides them.
func TestReconcileSessionBeads_DrainDeadlineRetireHonorsAdvisoryBlockers(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta map[string]string
	}{
		{
			name: "context_churn_quarantine",
			meta: map[string]string{"quarantined_until": "", "sleep_reason": "context-churn"},
		},
		{
			name: "user_hold",
			meta: map[string]string{"held_until": "", "sleep_reason": "user-hold"},
		},
		{
			name: "wait_hold",
			meta: map[string]string{"wait_hold": "true", "sleep_intent": "wait-hold"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := poolSeatEnv()
			seat := stuckDrainedPoolSeat(t, env, "drained", poolSlotDrainRetireDeadline+time.Minute)
			future := env.clk.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
			meta := map[string]string{}
			for k, v := range tc.meta {
				if v == "" && (k == "quarantined_until" || k == "held_until") {
					v = future
				}
				meta[k] = v
			}
			env.setSessionMetadata(&seat, meta)

			env.reconcile([]beads.Bead{seat})

			got, err := env.store.Get(seat.ID)
			if err != nil {
				t.Fatalf("Get(%s): %v", seat.ID, err)
			}
			if got.Status == "closed" {
				t.Fatalf("retired a seat signaling %q: metadata=%v", tc.name, got.Metadata)
			}
			if !env.sp.IsRunning(poolSeatName) {
				t.Fatalf("stopped the runtime of a seat signaling %q", tc.name)
			}
		})
	}
}

// A retirement reclaims the seat's worktree. The deadline path preempts the
// pool-freeable close, which is the only other site that prunes, so without
// this every retired seat leaks a worktree — and the hand-killed-pane case
// (runtime already absent) is the ordinary shape here.
func TestReconcileSessionBeads_DrainDeadlineRetirePrunesTheWorktree(t *testing.T) {
	env := poolSeatEnv()
	seat := stuckDrainedPoolSeat(t, env, "drained", poolSlotDrainRetireDeadline+time.Minute)

	var pruned []string
	restore := swapWorktreePruneForTest(func(info sessionpkg.Info, _ string, _ *config.City, _ io.Writer) {
		pruned = append(pruned, info.ID)
	})
	defer restore()

	env.reconcile([]beads.Bead{seat})

	got, err := env.store.Get(seat.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", seat.ID, err)
	}
	if got.Status != "closed" {
		t.Fatalf("precondition: seat not retired (metadata=%v)", got.Metadata)
	}
	if len(pruned) != 1 || pruned[0] != seat.ID {
		t.Fatalf("worktree prune calls = %v, want exactly [%s] — a retired pool seat must reclaim its worktree", pruned, seat.ID)
	}
}

// A stop this path actually performs is a real agent stop: it must reach the
// stop counter and the session.stopped envelope, or the lifecycle timeline for
// a retired seat ends with no stop at all.
func TestReconcileSessionBeads_DrainDeadlineRetireEmitsTheStopEvent(t *testing.T) {
	env := poolSeatEnv()
	rec := events.NewFake()
	env.rec = rec
	seat := stuckDrainedPoolSeat(t, env, "drained", poolSlotDrainRetireDeadline+time.Minute)

	env.reconcile([]beads.Bead{seat})

	if got, _ := env.store.Get(seat.ID); got.Status != "closed" {
		t.Fatalf("precondition: seat not retired (metadata=%v)", got.Metadata)
	}
	stops := 0
	for _, e := range rec.Events {
		if e.Type == events.SessionStopped && e.SessionID == seat.ID {
			stops++
		}
	}
	if stops != 1 {
		t.Fatalf("emitted %d %s events for the retired seat, want 1", stops, events.SessionStopped)
	}
}

// When the close is refused the deadline provenance must not survive: nothing
// else clears it, so a later ordinary close would carry false provenance and
// the greppable marker would over-count against the event.
func TestReconcileSessionBeads_DrainDeadlineRefusedCloseLeavesNoProvenance(t *testing.T) {
	env := poolSeatEnv()
	rec := events.NewFake()
	env.rec = rec
	seat := stuckDrainedPoolSeat(t, env, "drained", poolSlotDrainRetireDeadline+time.Minute)

	// Work claimed under the alias appears only after the pre-kill probe, so
	// the retirement reaches its final fence and is refused there.
	env.store = newWorkAppearsAfterProbeStore(env.store, seat.ID)

	env.reconcile([]beads.Bead{seat})

	got, err := env.store.Get(seat.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", seat.ID, err)
	}
	if got.Status == "closed" {
		t.Fatalf("closed despite work appearing before the final fence: metadata=%v", got.Metadata)
	}
	if got.Metadata[drainFinalizeMetadataKey] != "" {
		t.Fatalf("%s = %q on a refused retirement; the marker must agree with the event",
			drainFinalizeMetadataKey, got.Metadata[drainFinalizeMetadataKey])
	}
	// The close is re-fenced downstream, but a kill cannot be taken back: the
	// work probe has to run again immediately before the kill, not only before
	// the close.
	if !env.sp.IsRunning(poolSeatName) {
		t.Fatal("killed a live agent that claimed work during the retirement window")
	}
	if fired := drainDeadlineRetiredEvents(rec); len(fired) != 0 {
		t.Fatalf("emitted %d retirement events for a refused close, want 0", len(fired))
	}
}

// workAppearsAfterProbeStore models the agent claiming a bead in the window
// between the retirement's work probe and its next fence: the first assignee
// query answers honestly (no work), and creating the claim as a side effect of
// that query means every later probe sees it.
type workAppearsAfterProbeStore struct {
	beads.Store
	sessionID string
	fired     bool
}

func newWorkAppearsAfterProbeStore(inner beads.Store, sessionID string) *workAppearsAfterProbeStore {
	return &workAppearsAfterProbeStore{Store: inner, sessionID: sessionID}
}

func (s *workAppearsAfterProbeStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if !s.fired && strings.TrimSpace(q.Assignee) != "" {
		s.fired = true
		items, err := s.Store.List(q)
		if _, createErr := s.Create(beads.Bead{
			Title: "claimed during the retirement window", Type: "task",
			Status: "in_progress", Assignee: s.sessionID,
		}); createErr != nil {
			panic("seeding raced work: " + createErr.Error())
		}
		return items, err
	}
	return s.Store.List(q)
}

// Scope pin for the population this bound does NOT own. A seat in
// state=draining is always a drain-ack stop-pending row (BeginDrainPatch is its
// sole writer and DrainAckStopPendingPatch its only non-test caller), and
// reconcileDrainAckStopPending intercepts and continues before the deadline
// gate ever sees it. That population converges through its own machinery, so
// the bound must neither need nor claim it — and when such a runtime cannot be
// killed at all, nothing may close its bead over a live agent.
func TestReconcileSessionBeads_DrainingSeatsBelongToTheStopPendingHandler(t *testing.T) {
	newDrainingSeat := func(t *testing.T, env *reconcilerTestEnv) beads.Bead {
		t.Helper()
		env.addDesired(poolSeatName, "worker", false)
		seat := env.createSessionBead(poolSeatName, "worker")
		env.setSessionMetadata(&seat, map[string]string{
			"state":                string(sessionpkg.StateDraining),
			"state_reason":         sessionpkg.DrainAckStopPendingReason,
			"last_woke_at":         "",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"session_origin":       "ephemeral",
			"drain_at":             env.clk.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339),
		})
		if err := env.sp.Start(context.Background(), poolSeatName, runtime.Config{Command: "test-cmd"}); err != nil {
			t.Fatalf("start: %v", err)
		}
		return seat
	}

	t.Run("killable_runtime_converges_without_the_bound", func(t *testing.T) {
		env := poolSeatEnv()
		rec := events.NewFake()
		env.rec = rec
		seat := newDrainingSeat(t, env)
		logs := &syncWriter{}
		for i := 0; i < 6; i++ {
			cur, err := env.store.Get(seat.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if cur.Status == "closed" {
				if cur.Metadata[drainFinalizeMetadataKey] == drainFinalizeDeadline {
					t.Fatalf("the deadline bound retired a stop-pending seat it does not own: metadata=%v", cur.Metadata)
				}
				if fired := drainDeadlineRetiredEvents(rec); len(fired) != 0 {
					t.Fatalf("deadline retirement event fired for a stop-pending seat")
				}
				return
			}
			env.reconcileAsyncSafe([]beads.Bead{cur}, logs)
			env.clk.Time = env.clk.Time.Add(time.Minute)
		}
		t.Fatal("a killable stop-pending seat never converged; its own machinery is supposed to retire it")
	})

	t.Run("immortal_runtime_is_never_closed_over", func(t *testing.T) {
		env := poolSeatEnv()
		seat := newDrainingSeat(t, env)
		env.sp.StopLeavesRunning = map[string]bool{poolSeatName: true}
		logs := &syncWriter{}
		for i := 0; i < 6; i++ {
			cur, err := env.store.Get(seat.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if cur.Status == "closed" {
				t.Fatalf("closed a bead whose agent is still running (tick %d): metadata=%v", i, cur.Metadata)
			}
			env.reconcileAsyncSafe([]beads.Bead{cur}, logs)
			env.clk.Time = env.clk.Time.Add(time.Minute)
		}
		if !env.sp.IsRunning(poolSeatName) {
			t.Fatal("fixture no longer models an immortal runtime")
		}
	})
}

// workAppearsAfterStopStore injects a claim once the runtime is gone, which
// places it exactly between the pre-kill probe and the final pre-close fence.
type workAppearsAfterStopStore struct {
	beads.Store
	sp        *runtime.Fake
	name      string
	sessionID string
	fired     bool
}

func (s *workAppearsAfterStopStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if !s.fired && strings.TrimSpace(q.Assignee) != "" && !s.sp.IsRunning(s.name) {
		s.fired = true
		if _, err := s.Create(beads.Bead{
			Title: "claimed after the stop", Type: "task",
			Status: "in_progress", Assignee: s.sessionID,
		}); err != nil {
			panic("seeding post-stop work: " + err.Error())
		}
	}
	return s.Store.List(q)
}

// A retirement refused at the FINAL fence must leave no deadline provenance
// behind. Nothing else ever clears that key, so a marker stranded on a still
// open bead would follow it into whatever close comes later and inflate the
// deadline-retirement count against an event that never fired.
func TestReconcileSessionBeads_DrainDeadlineRefusedAtFinalFenceRollsBackProvenance(t *testing.T) {
	env := poolSeatEnv()
	rec := events.NewFake()
	env.rec = rec
	seat := stuckDrainedPoolSeat(t, env, "drained", poolSlotDrainRetireDeadline+time.Minute)
	env.store = &workAppearsAfterStopStore{Store: env.store, sp: env.sp, name: poolSeatName, sessionID: seat.ID}

	env.reconcile([]beads.Bead{seat})

	got, err := env.store.Get(seat.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", seat.ID, err)
	}
	if got.Status == "closed" {
		t.Fatalf("closed despite work claimed after the stop: metadata=%v", got.Metadata)
	}
	if got.Metadata[drainFinalizeMetadataKey] != "" {
		t.Fatalf("%s = %q left behind by a refused retirement; the marker and the counted event must agree",
			drainFinalizeMetadataKey, got.Metadata[drainFinalizeMetadataKey])
	}
	if fired := drainDeadlineRetiredEvents(rec); len(fired) != 0 {
		t.Fatalf("emitted %d retirement events for a refused close, want 0", len(fired))
	}
}

// closeFailsStore lets every read through but fails the terminal close
// transaction, the one path that can strand a stamped provenance marker on a
// bead that stays open.
type closeFailsStore struct {
	beads.Store
}

func (s *closeFailsStore) Tx(label string, fn func(beads.Tx) error) error {
	if strings.HasPrefix(label, "gc: close session ") {
		return errCloseUnavailableForTest
	}
	return s.Store.Tx(label, fn)
}

// A close that fails must roll the provenance back. The marker is stamped just
// before the write, and nothing else in the system ever clears it, so leaving
// it on a still-open bead would hand false deadline provenance to whatever
// close comes later — over-counting the residual drain tail this fix exists to
// keep honest.
func TestReconcileSessionBeads_DrainDeadlineFailedCloseRollsBackProvenance(t *testing.T) {
	env := poolSeatEnv()
	rec := events.NewFake()
	env.rec = rec
	seat := stuckDrainedPoolSeat(t, env, "drained", poolSlotDrainRetireDeadline+time.Minute)
	inner := env.store
	env.store = &closeFailsStore{Store: inner}

	env.reconcile([]beads.Bead{seat})

	got, err := inner.Get(seat.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", seat.ID, err)
	}
	if got.Status == "closed" {
		t.Fatalf("precondition: the close was supposed to fail (metadata=%v)", got.Metadata)
	}
	if got.Metadata[drainFinalizeMetadataKey] != "" {
		t.Fatalf("%s = %q stranded by a failed close; nothing else ever clears it",
			drainFinalizeMetadataKey, got.Metadata[drainFinalizeMetadataKey])
	}
	if fired := drainDeadlineRetiredEvents(rec); len(fired) != 0 {
		t.Fatalf("emitted %d retirement events for a failed close, want 0", len(fired))
	}
}
