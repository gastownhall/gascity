package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/runtime/hybrid"
)

// These tests pin the pending-create teardown of a manual session's runtime
// on a runtime that cannot vouch for identity (releaseRowOwnedRuntime's
// manual arm, manualRowTearsDownUnattributedRuntime). The shape they share is
// a restarted controller: the row is a claimed pending create whose own start
// is gone (last_woke_at 30 minutes old, no lease holder), and a box runs
// under its name with no GC_SESSION_ID or GC_INSTANCE_TOKEN, as a remote
// runtime's box does when its identity is not readable from creation on. The
// rollback reads that box as another session's and closes the row; for a
// manual row nothing addresses the box again.

// teardownRowClass names the row shapes the teardown tells apart.
type teardownRowClass string

const (
	rowManual       teardownRowClass = "manual"
	rowManualLegacy teardownRowClass = "manual-legacy-marker"
	rowNamed        teardownRowClass = "named"
	rowSingleton    teardownRowClass = "singleton"
	rowUnsetOrigin  teardownRowClass = "unset-origin"
	rowPool         teardownRowClass = "pool-bead-scoped"
)

// teardownRowMeta is the metadata each class adds to the fixture row.
var teardownRowMeta = map[teardownRowClass]map[string]string{
	rowManual:       {"session_origin": "manual", "session_name_explicit": "true"},
	rowManualLegacy: {"manual_session": "true"},
	rowNamed:        {"session_origin": "named", "configured_named_identity": "worker", "configured_named_session": "true"},
	rowSingleton:    {"session_origin": "ephemeral"},
	rowUnsetOrigin:  {},
	rowPool:         {"pool_managed": "true"},
}

// newRestartedPendingCreate seeds a claimed pending create of class whose
// start is gone, with a box running under its name and no identity on it. It
// returns the env, the row and its runtime name.
func newRestartedPendingCreate(t *testing.T, class teardownRowClass) (*reconcilerTestEnv, beads.Bead, string) {
	t.Helper()
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	session := env.createSessionBead("worker", "worker")
	name := "worker"
	if class == rowPool {
		name = PoolSessionName("worker", session.ID)
	}
	leasedAt := env.clk.Now().Add(-30 * time.Minute)
	meta := map[string]string{
		"session_name":              name,
		"agent_name":                name,
		"state":                     "creating",
		"pending_create_claim":      "true",
		"pending_create_started_at": pendingCreateStartedAtNow(leasedAt),
		"last_woke_at":              leasedAt.UTC().Format(time.RFC3339),
	}
	for k, v := range teardownRowMeta[class] {
		meta[k] = v
	}
	env.setSessionMetadata(&session, meta)
	env.addDesired(name, "worker", true)
	return env, session, name
}

// reconcileTeardownTick runs one reconciler tick over the row on sp.
func reconcileTeardownTick(t *testing.T, env *reconcilerTestEnv, sp runtime.Provider, id string) beads.Bead {
	t.Helper()
	cur, err := env.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeadsTraced(
		context.Background(), env.city, []beads.Bead{cur}, env.desiredState, cfgNames, env.cfg, sp,
		env.store, nil, nil, nil, nil, env.dt, map[string]int{"worker": 1}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, newPoolDesiredStateTestTrace("worker"),
		env.startOptions...,
	)
	got, err := env.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// teardownRuntimeVariant builds the provider a case runs on over the env's
// fake, and says whether the fix's teardown applies to a manual row on it.
type teardownRuntimeVariant struct {
	name  string
	build func(f *runtime.Fake, name string) runtime.Provider
	// tearsDown: a manual row's unattributed box is stopped before close.
	tearsDown bool
	// holds: the row is held open (the runtime serving the name is unknown).
	holds bool
}

func profileVariant(p runtime.Profile, tearsDown bool) teardownRuntimeVariant {
	return teardownRuntimeVariant{
		name: p.String(),
		build: func(f *runtime.Fake, _ string) runtime.Provider {
			return runtime.NewFakeProfile(p, runtime.FullFake{Fake: f})
		},
		tearsDown: tearsDown,
	}
}

func teardownRuntimeVariants() []teardownRuntimeVariant {
	leg := func(p runtime.Profile, f *runtime.Fake) runtime.Provider {
		return runtime.NewFakeProfile(p, runtime.FullFake{Fake: f})
	}
	return []teardownRuntimeVariant{
		profileVariant(runtime.ProfileTmux, false),
		profileVariant(runtime.ProfileACP, false),
		profileVariant(runtime.ProfileSubprocess, false),
		profileVariant(runtime.ProfileHerdr, true),
		profileVariant(runtime.ProfileK8s, true),
		profileVariant(runtime.ProfileExec, true),
		profileVariant(runtime.ProfileSSH, true),
		profileVariant(runtime.ProfileT3Bridge, true),
		{
			name: "auto(exec,acp)/seeded",
			build: func(f *runtime.Fake, _ string) runtime.Provider {
				sp := auto.New(leg(runtime.ProfileExec, f), leg(runtime.ProfileACP, f))
				sp.SeedRoutes(nil)
				return sp
			},
			tearsDown: true,
		},
		{
			name: "auto(exec,acp)/acp-routed",
			build: func(f *runtime.Fake, name string) runtime.Provider {
				sp := auto.New(leg(runtime.ProfileExec, f), leg(runtime.ProfileACP, f))
				sp.SeedRoutes([]string{name})
				return sp
			},
		},
		{
			name: "auto(exec,acp)/unseeded",
			build: func(f *runtime.Fake, _ string) runtime.Provider {
				return auto.New(leg(runtime.ProfileExec, f), leg(runtime.ProfileACP, f))
			},
			holds: true,
		},
		{
			name: "auto(tmux,acp)/unseeded",
			build: func(f *runtime.Fake, _ string) runtime.Provider {
				return auto.New(leg(runtime.ProfileTmux, f), leg(runtime.ProfileACP, f))
			},
		},
		{
			name: "hybrid(tmux,k8s)/remote",
			build: func(f *runtime.Fake, _ string) runtime.Provider {
				return hybrid.New(leg(runtime.ProfileTmux, f), leg(runtime.ProfileK8s, f), func(string) bool { return true })
			},
			tearsDown: true,
		},
		{
			name: "hybrid(tmux,k8s)/local",
			build: func(f *runtime.Fake, _ string) runtime.Provider {
				return hybrid.New(leg(runtime.ProfileTmux, f), leg(runtime.ProfileK8s, f), func(string) bool { return false })
			},
		},
	}
}

// TestPendingCreateRollback_TearsDownManualRowsUnattributedRuntime: a
// restarted controller rolls back a manual session's pending create whose
// unattributed box runs on a runtime that cannot vouch for identity. It stops
// the box once and then closes the row. Every other cell keeps its previous
// outcome: a manual row on tmux, acp or subprocess, or one routed there, is
// closed with no stop (an unstamped box there is another owner's); named,
// singleton and unset-origin rows are closed with no stop (their name is
// re-targeted and the box found again); a bead-scoped pool row is torn down
// on every runtime, as before. A manual row whose name an unseeded composite
// cannot route to a leaf that vouches for identity is held.
//
// Kills: no leaf gate; no manual-origin check; route-unknown read as known.
func TestPendingCreateRollback_TearsDownManualRowsUnattributedRuntime(t *testing.T) {
	classes := []teardownRowClass{rowManual, rowManualLegacy, rowNamed, rowSingleton, rowUnsetOrigin, rowPool}
	for _, v := range teardownRuntimeVariants() {
		for _, class := range classes {
			t.Run(v.name+"/"+string(class), func(t *testing.T) {
				env, session, name := newRestartedPendingCreate(t, class)
				sp := v.build(env.sp, name)
				got := reconcileTeardownTick(t, env, sp, session.ID)
				running := env.sp.IsRunning(name)
				stops := countCalls(env.sp, "Stop")
				manual := class == rowManual || class == rowManualLegacy
				switch {
				case manual && v.holds:
					if got.Status == "closed" || got.Metadata["pending_create_claim"] != "true" || stops != 0 || !running {
						t.Fatalf("status=%s claim=%q stops=%d running=%v, want the row held open with its box untouched while its route is unknown; stderr=%s",
							got.Status, got.Metadata["pending_create_claim"], stops, running, env.stderr.String())
					}
				case manual && v.tearsDown, class == rowPool:
					if got.Status != "closed" || stops != 1 || running {
						t.Fatalf("status=%s stops=%d running=%v, want the box stopped once and then the row closed; stderr=%s",
							got.Status, stops, running, env.stderr.String())
					}
				default:
					if got.Status != "closed" || stops != 0 || !running {
						t.Fatalf("status=%s stops=%d running=%v, want the row closed with no stop, as before; stderr=%s",
							got.Status, stops, running, env.stderr.String())
					}
				}
			})
		}
	}
}

// blindLivenessProvider is a minimal remote runtime that cannot report
// liveness (an exec pack with no is-running): every box reads not running.
// It implements no optional interface, so it cannot vouch for identity.
type blindLivenessProvider struct{ runtime.Provider }

func (blindLivenessProvider) IsRunning(string) bool { return false }

func execProfile(f *runtime.Fake) runtime.Provider {
	return runtime.NewFakeProfile(runtime.ProfileExec, runtime.FullFake{Fake: f})
}

// TestPendingCreateRollback_ManualTeardownEdges pins the teardown's edges for
// a manual row on an exec runtime over three ticks: row state, stop count and
// the hold's log lines (one per episode).
//
// Kills: no positive-foreign exception; unknown attribution treated as
// stoppable; close before the stop is confirmed; stop outside the flock;
// per-tick hold logging; a hold never released after a successful stop.
func TestPendingCreateRollback_ManualTeardownEdges(t *testing.T) {
	for _, tc := range []struct {
		name string
		// setup arranges the case and returns the provider to tick on, and
		// for a held case the heal that clears the fault after three ticks;
		// the fourth tick must then tear down and close.
		setup     func(t *testing.T, env *reconcilerTestEnv) (runtime.Provider, func())
		wantHeld  bool
		wantStops int
		holdLine  string
	}{
		{
			name: "another session's ID",
			setup: func(_ *testing.T, env *reconcilerTestEnv) (runtime.Provider, func()) {
				_ = env.sp.SetMeta("worker", "GC_SESSION_ID", "gc-other")
				return execProfile(env.sp), nil
			},
		},
		{
			name: "identity unreadable",
			setup: func(_ *testing.T, env *reconcilerTestEnv) (runtime.Provider, func()) {
				failIdentityReads(env.sp, "worker", "GC_SESSION_ID", "GC_INSTANCE_TOKEN")
				return blindLivenessProvider{execProfile(env.sp)}, func() { env.sp.GetMetaErrors["worker"] = nil }
			},
			wantHeld:  true,
			wantStops: 0,
			holdLine:  "the identity of its runtime",
		},
		{
			name: "stop fails",
			setup: func(_ *testing.T, env *reconcilerTestEnv) (runtime.Provider, func()) {
				env.sp.StopErrors["worker"] = errors.New("backend unreachable")
				return execProfile(env.sp), func() { delete(env.sp.StopErrors, "worker") }
			},
			wantHeld:  true,
			wantStops: 3,
			holdLine:  "tearing down its runtime",
		},
		{
			name: "stop returns but the box survives",
			setup: func(_ *testing.T, env *reconcilerTestEnv) (runtime.Provider, func()) {
				env.sp.StopLeavesRunning["worker"] = true
				return execProfile(env.sp), func() { delete(env.sp.StopLeavesRunning, "worker") }
			},
			wantHeld:  true,
			wantStops: 3,
			holdLine:  "survived its stop",
		},
		{
			name: "name's flock busy",
			setup: func(t *testing.T, env *reconcilerTestEnv) (runtime.Provider, func()) {
				_, release, err := tryRuntimeLease(nil, env.city, "worker", "", 0)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(release)
				return blindLivenessProvider{execProfile(env.sp)}, release
			},
			wantHeld:  true,
			wantStops: 0,
			holdLine:  "cannot be torn down yet",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, session, name := newRestartedPendingCreate(t, rowManual)
			sp, heal := tc.setup(t, env)
			var got beads.Bead
			for tick := 1; tick <= 3; tick++ {
				got = reconcileTeardownTick(t, env, sp, session.ID)
				env.clk.Advance(30 * time.Second)
				if !tc.wantHeld {
					break
				}
				if got.Status == "closed" || got.Metadata["pending_create_claim"] != "true" {
					t.Fatalf("tick %d: status=%s claim=%q, want the row held open; stderr=%s", tick, got.Status, got.Metadata["pending_create_claim"], env.stderr.String())
				}
			}
			stops := countCalls(env.sp, "Stop")
			if !tc.wantHeld {
				if got.Status != "closed" || stops != 0 || !env.sp.IsRunning(name) {
					t.Fatalf("status=%s stops=%d running=%v, want the row closed and another session's box left alone; stderr=%s",
						got.Status, stops, env.sp.IsRunning(name), env.stderr.String())
				}
				return
			}
			if stops != tc.wantStops || !env.sp.IsRunning(name) {
				t.Fatalf("stops=%d running=%v, want %d stops and the box still there while held; stderr=%s", stops, env.sp.IsRunning(name), tc.wantStops, env.stderr.String())
			}
			if n := strings.Count(env.stderr.String(), tc.holdLine); n != 1 {
				t.Fatalf("hold line %q printed %d times over 3 ticks, want once per episode; stderr=%s", tc.holdLine, n, env.stderr.String())
			}
			heal()
			got = reconcileTeardownTick(t, env, sp, session.ID)
			if got.Status != "closed" || env.sp.IsRunning(name) {
				t.Fatalf("after the fault cleared: status=%s running=%v, want the box torn down and the row closed; stderr=%s",
					got.Status, env.sp.IsRunning(name), env.stderr.String())
			}
		})
	}
}

// TestPendingCreateRollback_BlindRunningTearsDownAtLeaseExpiry: a runtime that
// cannot report liveness reads the manual row's box not running, so the
// lease-expired rollback (not the alive one) closes the row. It tears the box
// down first.
//
// Kills: no teardown at the lease-expired site.
func TestPendingCreateRollback_BlindRunningTearsDownAtLeaseExpiry(t *testing.T) {
	env, session, name := newRestartedPendingCreate(t, rowManual)
	got := reconcileTeardownTick(t, env, blindLivenessProvider{execProfile(env.sp)}, session.ID)
	if got.Status != "closed" || countCalls(env.sp, "Stop") != 1 || env.sp.IsRunning(name) {
		t.Fatalf("status=%s stops=%d running=%v, want the box stopped and the row closed; stderr=%s",
			got.Status, countCalls(env.sp, "Stop"), env.sp.IsRunning(name), env.stderr.String())
	}
	if !strings.Contains(env.stderr.String(), "lease expired and no live runtime") {
		t.Fatalf("the row was not closed by the lease-expired rollback; stderr=%s", env.stderr.String())
	}
}

// TestPendingCreateRollback_ManualFenceStillHolds: a manual row's own slow
// start on a runtime with no identity is not torn down while it runs. The
// tick that lands mid-start defers to it (the in-flight fence), and the start
// commits the row over its box.
//
// Kills: a stop while the row's own start is live.
func TestPendingCreateRollback_ManualFenceStillHolds(t *testing.T) {
	for _, advance := range []time.Duration{2 * time.Second, 10 * time.Minute} {
		t.Run(advance.String(), func(t *testing.T) {
			env, sp, session, _ := runSlowStart(t, nil, advance, teardownRowMeta[rowManual])
			got, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if n := countCalls(env.sp, "Stop"); n != 0 {
				t.Fatalf("Stop called %d times during the row's own start; stderr=%s", n, env.stderr.String())
			}
			if got.Status == "closed" || got.Metadata["state"] != "active" || !sp.IsRunning("worker") {
				t.Fatalf("status=%s state=%q running=%v, want the start committed over its box; stderr=%s",
					got.Status, got.Metadata["state"], sp.IsRunning("worker"), env.stderr.String())
			}
		})
	}
}

// slowStartProvider is a slow remote runtime over a profiled fake: Start
// creates the box at once with no identity on it, then returns only once
// released. A tmux variant keeps the batched environment read, so it vouches
// for identity as tmux does.
type slowStartProvider struct {
	runtime.Provider
	created chan string
	release chan struct{}
}

func (p *slowStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := p.Provider.Start(ctx, name, cfg); err != nil {
		return err
	}
	p.created <- name
	<-p.release
	return nil
}

type slowStartTmuxProvider struct {
	*slowStartProvider
	runtime.EnvironmentBatchProvider
}

// TestStaleAsyncStart_StopsManualRowsUnattributedRuntime: a manual row is
// closed while its start is creating the box. The start's commit finds the
// row gone, and its stale-start cleanup stops the box it created on a runtime
// that cannot vouch for identity. It leaves the box alone on tmux (an
// unstamped box there is another owner's) and for a named row (its name is
// re-targeted).
//
// Kills: no stale-start override.
func TestStaleAsyncStart_StopsManualRowsUnattributedRuntime(t *testing.T) {
	for _, tc := range []struct {
		name     string
		class    teardownRowClass
		profile  runtime.Profile
		wantStop bool
	}{
		{name: "exec/manual", class: rowManual, profile: runtime.ProfileExec, wantStop: true},
		{name: "k8s/manual", class: rowManual, profile: runtime.ProfileK8s, wantStop: true},
		{name: "tmux/manual", class: rowManual, profile: runtime.ProfileTmux},
		{name: "exec/named", class: rowNamed, profile: runtime.ProfileExec},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv(t)
			profiled := runtime.NewFakeProfile(tc.profile, runtime.FullFake{Fake: env.sp})
			slow := &slowStartProvider{Provider: profiled, created: make(chan string, 1), release: make(chan struct{})}
			var sp runtime.Provider = slow
			if batch, ok := profiled.(runtime.EnvironmentBatchProvider); ok {
				sp = slowStartTmuxProvider{slowStartProvider: slow, EnvironmentBatchProvider: batch}
			}
			starts := &asyncStartTracker{}
			released := false
			t.Cleanup(func() {
				if !released {
					close(slow.release)
				}
				starts.wait(5 * time.Second)
			})
			env.startOptions = append(env.startOptions, withAsyncStartExecution(), withAsyncStartTracker(starts))
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
			env.addDesired("worker", "worker", false)
			session := env.createSessionBead("worker", "worker")
			meta := map[string]string{
				"state":                     "creating",
				"pending_create_claim":      "true",
				"pending_create_started_at": pendingCreateStartedAtNow(env.clk.Now()),
			}
			for k, v := range teardownRowMeta[tc.class] {
				meta[k] = v
			}
			env.setSessionMetadata(&session, meta)
			reconcileTeardownTick(t, env, sp, session.ID)
			select {
			case <-slow.created:
			case <-time.After(5 * time.Second):
				t.Fatalf("the start never created the box; stderr=%s", env.stderr.String())
			}
			if err := env.store.Close(session.ID); err != nil {
				t.Fatal(err)
			}
			released = true
			close(slow.release)
			if !starts.wait(5 * time.Second) {
				t.Fatal("async start did not finish")
			}
			stopped := countCalls(env.sp, "Stop") > 0 && !env.sp.IsRunning("worker")
			if stopped != tc.wantStop {
				t.Fatalf("box stopped = %v, want %v; stderr=%s", stopped, tc.wantStop, env.stderr.String())
			}
		})
	}
}

// TestStopStaleAsyncStartRuntime_ManualRowNeedsTheStartsLease: the stale-start
// cleanup counts a manual row's unattributed box as the start's own only while
// that start still holds its runtime lease. Once the lease is gone another
// starter may hold the name, so the box is left alone.
//
// Kills: a stale-start override that ignores the lease.
func TestStopStaleAsyncStartRuntime_ManualRowNeedsTheStartsLease(t *testing.T) {
	for _, tc := range []struct {
		name     string
		released bool
		wantStop bool
	}{
		{name: "lease held", wantStop: true},
		{name: "lease released", released: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, session, name := newRestartedPendingCreate(t, rowManual)
			lease, release, err := tryRuntimeLease(nil, env.city, name, "", 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(release)
			if tc.released {
				release()
			}
			result := startResult{prepared: preparedStart{candidate: startCandidate{
				info:  env.sessionInfo(session.ID),
				tp:    TemplateParams{SessionName: name, TemplateName: "worker", Command: "test-cmd"},
				lease: lease,
			}}}
			stopStaleAsyncStartRuntime(result, execProfile(env.sp), io.Discard)
			if stopped := !env.sp.IsRunning(name); stopped != tc.wantStop {
				t.Fatalf("box stopped = %v, want %v", stopped, tc.wantStop)
			}
		})
	}
}

// boxThenFailProvider creates the box and then fails the start, leaving the
// box behind (a provider whose own cleanup did not run).
type boxThenFailProvider struct{ runtime.Provider }

func (p boxThenFailProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := p.Provider.Start(ctx, name, cfg); err != nil {
		return err
	}
	return errors.New("readiness probe failed")
}

// TestStartFailure_ManualRowTearsDownBeforeRollback: a manual row's start
// creates its box and then fails. The failed start's rollback stops the box
// before it closes the row; while the stop fails the row stays a pending
// create, and the lease-expired rollback retries once the stop works.
func TestStartFailure_ManualRowTearsDownBeforeRollback(t *testing.T) {
	for _, stopFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop works", true: "stop fails"}[stopFails], func(t *testing.T) {
			env := newReconcilerTestEnv(t)
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
			env.addDesired("worker", "worker", false)
			session := env.createSessionBead("worker", "worker")
			meta := map[string]string{
				"state":                     "creating",
				"pending_create_claim":      "true",
				"pending_create_started_at": pendingCreateStartedAtNow(env.clk.Now()),
			}
			for k, v := range teardownRowMeta[rowManual] {
				meta[k] = v
			}
			env.setSessionMetadata(&session, meta)
			if stopFails {
				env.sp.StopErrors["worker"] = errors.New("backend unreachable")
			}
			sp := boxThenFailProvider{execProfile(env.sp)}
			got := reconcileTeardownTick(t, env, sp, session.ID)
			if !stopFails {
				if got.Status != "closed" || env.sp.IsRunning("worker") {
					t.Fatalf("status=%s running=%v, want the failed start's box stopped and the row closed; stderr=%s",
						got.Status, env.sp.IsRunning("worker"), env.stderr.String())
				}
				return
			}
			if got.Status == "closed" || got.Metadata["pending_create_claim"] != "true" || !env.sp.IsRunning("worker") {
				t.Fatalf("status=%s claim=%q running=%v, want the row kept a pending create while its box cannot be stopped; stderr=%s",
					got.Status, got.Metadata["pending_create_claim"], env.sp.IsRunning("worker"), env.stderr.String())
			}
		})
	}
}

// TestManualRowTearsDownUnattributedRuntime_RowShapes pins which row shapes
// the teardown applies to on a runtime that cannot vouch for identity.
//
// Kills: applying to a configured named session; dropping the manual-origin
// check; acting on a row with no runtime name.
func TestManualRowTearsDownUnattributedRuntime_RowShapes(t *testing.T) {
	exec := execProfile(runtime.NewFake())
	tmux := runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()})
	for _, tc := range []struct {
		name string
		meta map[string]string
		want bool
	}{
		{name: "manual, explicit adhoc name", meta: map[string]string{"session_origin": "manual", "session_name": "worker-adhoc-0123456789"}, want: true},
		{name: "manual, bead-derived name", meta: map[string]string{"session_origin": "manual", "session_name": "s-gc-1"}, want: true},
		{name: "legacy manual marker", meta: map[string]string{"manual_session": "true", "session_name": "worker"}, want: true},
		{name: "manual, no runtime name", meta: map[string]string{"session_origin": "manual"}},
		{name: "manual, configured named", meta: map[string]string{"session_origin": "manual", "configured_named_session": "true", "session_name": "worker"}},
		{name: "named", meta: map[string]string{"session_origin": "named", "configured_named_session": "true", "session_name": "worker"}},
		{name: "singleton", meta: map[string]string{"session_origin": "ephemeral", "session_name": "worker"}},
		{name: "unset origin", meta: map[string]string{"session_name": "worker"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := beads.Bead{ID: "gc-1", Type: sessionBeadType, Metadata: tc.meta}
			info := sessionInfoFromBead(b)
			if applies, known := manualRowTearsDownUnattributedRuntime(info, exec); applies != tc.want || !known {
				t.Fatalf("on exec: applies=%v known=%v, want %v true", applies, known, tc.want)
			}
			if applies, _ := manualRowTearsDownUnattributedRuntime(info, tmux); applies {
				t.Fatal("on tmux: applies, want never: tmux vouches for identity")
			}
		})
	}
}
