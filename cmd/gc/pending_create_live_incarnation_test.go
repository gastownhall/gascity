package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// These tests pin the live-incarnation hold of a pending-create rollback
// (#2081): a live runtime under the row's name that carries the row's own
// GC_SESSION_ID, another GC_INSTANCE_TOKEN and a GC_RUNTIME_EPOCH other than
// the row's generation is an earlier or later incarnation of the same
// session. The rollback holds the row while that session has in-progress
// work: nothing is stopped, the row keeps its pending_create_claim, and its
// work stays assigned. Once no in-progress work is left the rollback takes
// the row as it takes any other incarnation's runtime.

// stampLiveIncarnation seeds the restarted pending create of class at
// generation 2 and stamps the box under its name as another incarnation of
// the same session at epoch.
func stampLiveIncarnation(t *testing.T, class teardownRowClass, epoch string) (*reconcilerTestEnv, beads.Bead, string) {
	t.Helper()
	env, session, name := newRestartedPendingCreate(t, class)
	env.setSessionMetadata(&session, map[string]string{"generation": "2", "instance_token": "tok-row"})
	stampRuntimeIncarnation(t, env.sp, name, session.ID, epoch)
	return env, session, name
}

// assignLiveIncarnationWork creates an in_progress work bead routed to the worker pool and
// assigned to the session name.
func assignLiveIncarnationWork(t *testing.T, store beads.Store, assignee string) beads.Bead {
	t.Helper()
	return assignRoutedWork(t, store, assignee, "in_progress")
}

// assignRoutedWork creates a work bead routed to the worker pool, assigned to
// assignee, in status.
func assignRoutedWork(t *testing.T, store beads.Store, assignee, status string) beads.Bead {
	t.Helper()
	work, err := store.Create(beads.Bead{
		Title:    "routed work",
		Assignee: assignee,
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatal(err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	return work
}

// releaseOrphanedWork runs the orphaned pool-work release over the env's open
// session rows, as the controller does after a tick.
func releaseOrphanedWork(t *testing.T, env *reconcilerTestEnv, sessionID string, work beads.Bead) beads.Bead {
	t.Helper()
	var open []sessionpkg.Info
	if row, err := env.store.Get(sessionID); err != nil {
		t.Fatal(err)
	} else if row.Status != "closed" {
		open = sessionInfosFromBeads([]beads.Bead{row})
	}
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}}
	releaseOrphanedPoolAssignments(env.store, beads.SessionStore{Store: env.store}, cfg, "", open, []beads.Bead{work}, nil, nil, nil, nil, nil, nil)
	got, err := env.store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// liveIncarnationDeferrals counts the tick's Deferred pending-create records
// attributed to a live incarnation.
func liveIncarnationDeferrals(trace *sessionReconcilerTraceCycle) int {
	n := 0
	for _, r := range trace.records {
		if r.SiteCode == TraceSiteReconcilerPendingCreate && r.ReasonCode == TraceReasonPendingCreateLiveIncarnation &&
			r.OutcomeCode == TraceOutcomeDeferred && r.Fields["attribution"] == "live_incarnation" {
			n++
		}
	}
	return n
}

// reconcileLiveIncarnationTick runs one traced legacy reconciler tick over the
// row on sp.
func reconcileLiveIncarnationTick(t *testing.T, env *reconcilerTestEnv, sp runtime.Provider, id string, trace *sessionReconcilerTraceCycle) {
	t.Helper()
	cur, err := env.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeadsTraced(
		context.Background(), env.city, []beads.Bead{cur}, env.desiredState, cfgNames, env.cfg, sp,
		env.store, nil, nil, nil, nil, env.dt, map[string]int{"worker": 1}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, trace,
		env.startOptions...,
	)
}

// assertLiveIncarnationHeld ticks the legacy reconciler twice over the row and
// checks the hold on each: the box runs unstopped, the row stays open with its
// claim, one Deferred trace per tick under a reason that arms no auto-trace,
// and the standing condition said once.
func assertLiveIncarnationHeld(t *testing.T, env *reconcilerTestEnv, sp runtime.Provider, session beads.Bead, name string) {
	t.Helper()
	if shouldAutoArmForTrace(TraceReasonPendingCreateLiveIncarnation, TraceOutcomeDeferred) {
		t.Fatalf("the hold's trace reason %q arms an auto-trace, renewed every tick the hold lasts", TraceReasonPendingCreateLiveIncarnation)
	}
	for tick := 1; tick <= 2; tick++ {
		trace := newPoolDesiredStateTestTrace("worker")
		reconcileLiveIncarnationTick(t, env, sp, session.ID, trace)
		got, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == "closed" || got.Metadata["pending_create_claim"] != "true" || got.Metadata["state"] == string(sessionpkg.StateFailedCreate) {
			t.Fatalf("tick %d: status=%q state=%q claim=%q, want the pending create held open while its live incarnation runs; stderr=%s",
				tick, got.Status, got.Metadata["state"], got.Metadata["pending_create_claim"], env.stderr.String())
		}
		if !env.sp.IsRunning(name) || countCalls(env.sp, "Stop") != 0 {
			t.Fatalf("tick %d: running=%v stops=%d, want the live incarnation left running", tick, env.sp.IsRunning(name), countCalls(env.sp, "Stop"))
		}
		if n := liveIncarnationDeferrals(trace); n != 1 {
			t.Fatalf("tick %d: live_incarnation deferral records = %d, want 1; records=%+v", tick, n, trace.records)
		}
	}
	if _, ok := env.dt.drainSkips[drainSkipKey{beadID: session.ID, kind: liveIncarnationHoldKind}]; !ok {
		t.Fatalf("standing condition %q not recorded for %s", liveIncarnationHoldKind, session.ID)
	}
	if n := strings.Count(env.stderr.String(), "(live_incarnation)"); n != 1 {
		t.Fatalf("live_incarnation lines = %d, want 1 (transition only); stderr=%s", n, env.stderr.String())
	}
}

// TestPendingCreateRollback_HoldsPoolRowsLiveIncarnation is AC-1: a
// bead-scoped pool row's expired pending create whose name holds a live
// earlier (epoch 1) or later (epoch 3) incarnation of the same session is
// held, and its in_progress work keeps its assignee.
func TestPendingCreateRollback_HoldsPoolRowsLiveIncarnation(t *testing.T) {
	for _, epoch := range []string{"1", "3"} {
		t.Run("epoch "+epoch, func(t *testing.T) {
			env, session, name := stampLiveIncarnation(t, rowPool, epoch)
			work := assignLiveIncarnationWork(t, env.store, name)

			assertLiveIncarnationHeld(t, env, env.sp, session, name)

			got := releaseOrphanedWork(t, env, session.ID, work)
			if got.Status != "in_progress" || got.Assignee != name {
				t.Fatalf("work status=%q assignee=%q, want in_progress assigned to %q", got.Status, got.Assignee, name)
			}
		})
	}
}

// TestPendingCreateRollback_HoldsNamedAndManualRowsLiveIncarnation is AC-2: a
// configured named or a manual row's expired pending create, whose name holds
// a live incarnation of the same session on a runtime that stamps identity,
// is not closed failed-create while that incarnation's work is in progress.
func TestPendingCreateRollback_HoldsNamedAndManualRowsLiveIncarnation(t *testing.T) {
	for _, class := range []teardownRowClass{rowNamed, rowManual} {
		for _, epoch := range []string{"1", "3"} {
			t.Run(string(class)+"/epoch "+epoch, func(t *testing.T) {
				env, session, name := stampLiveIncarnation(t, class, epoch)
				sp := runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: env.sp})
				assignLiveIncarnationWork(t, env.store, name)

				assertLiveIncarnationHeld(t, env, sp, session, name)
			})
		}
	}
}

// TestPendingCreateRollback_HoldsOverWorkUnderAPriorAlias is the hold read
// as a kill gate (O4, #2081): in-progress work the live incarnation holds
// under a prior alias of its session (alias_history), which the release
// identities omit, holds a pool, named or manual row as work under its name
// does, and stays with its assignee.
func TestPendingCreateRollback_HoldsOverWorkUnderAPriorAlias(t *testing.T) {
	for _, class := range []teardownRowClass{rowPool, rowNamed, rowManual} {
		for _, epoch := range []string{"1", "3"} {
			t.Run(string(class)+"/epoch "+epoch, func(t *testing.T) {
				env, session, name := stampLiveIncarnation(t, class, epoch)
				env.setSessionMetadata(&session, map[string]string{"alias_history": "worker/nux"})
				sp := runtime.Provider(env.sp)
				if class != rowPool {
					sp = runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: env.sp})
				}
				work := assignLiveIncarnationWork(t, env.store, "worker/nux")

				assertLiveIncarnationHeld(t, env, sp, session, name)

				got, err := env.store.Get(work.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.Status != "in_progress" || got.Assignee != "worker/nux" {
					t.Fatalf("work status=%q assignee=%q, want in_progress assigned to worker/nux", got.Status, got.Assignee)
				}
			})
		}
	}
}

// TestPendingCreateRollback_ConvergesOnceLiveIncarnationWorkEnds is the hold's
// exit (#2081): a row held over its live incarnation while that incarnation's
// work is in progress is rolled back by the first tick after the work ends,
// the incarnation still running and no operator acting. Open work assigned to
// the session, a continuation the incarnation cannot claim under its old
// token, does not hold it. A pool row's bead-scoped incarnation is stopped
// and that open work released to the session's next incarnation; a named or
// manual row closes without a stop, as for any other incarnation's runtime.
func TestPendingCreateRollback_ConvergesOnceLiveIncarnationWorkEnds(t *testing.T) {
	for _, class := range []teardownRowClass{rowPool, rowNamed, rowManual} {
		for _, epoch := range []string{"1", "3"} {
			t.Run(string(class)+"/epoch "+epoch, func(t *testing.T) {
				env, session, name := stampLiveIncarnation(t, class, epoch)
				sp := runtime.Provider(env.sp)
				if class != rowPool {
					sp = runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: env.sp})
				}
				work := assignLiveIncarnationWork(t, env.store, name)
				next := assignRoutedWork(t, env.store, session.ID, "open")
				assertLiveIncarnationHeld(t, env, sp, session, name)

				if err := env.store.Close(work.ID); err != nil {
					t.Fatal(err)
				}
				trace := newPoolDesiredStateTestTrace("worker")
				reconcileLiveIncarnationTick(t, env, sp, session.ID, trace)
				got, err := env.store.Get(session.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.Status != "closed" || got.Metadata["state"] != string(sessionpkg.StateFailedCreate) {
					t.Fatalf("status=%q state=%q, want the pending create rolled back once its live incarnation's work ended; stderr=%s",
						got.Status, got.Metadata["state"], env.stderr.String())
				}
				if n := liveIncarnationDeferrals(trace); n != 0 {
					t.Fatalf("live_incarnation deferral records = %d after the work ended, want 0", n)
				}
				if !strings.Contains(env.stderr.String(), "rolling back pending create "+name+": a live incarnation of this session holds no in-progress work") {
					t.Fatalf("stderr = %q, want the rollback to name the workless live incarnation", env.stderr.String())
				}
				pool := class == rowPool
				if stops := countCalls(env.sp, "Stop"); (stops > 0) != pool || env.sp.IsRunning(name) == pool {
					t.Fatalf("stops=%d running=%v, want the incarnation stopped only under the pool row's bead-scoped name", stops, env.sp.IsRunning(name))
				}
				if !pool {
					return
				}
				if released := releaseOrphanedWork(t, env, session.ID, next); released.Status != "open" || released.Assignee != "" {
					t.Fatalf("open work status=%q assignee=%q, want it released to the next incarnation", released.Status, released.Assignee)
				}
			})
		}
	}
}

// TestHoldsOverLiveIncarnation_FailsClosed pins the hold's work read: no
// in-progress work assigned to the session releases the hold, open work and
// another session's work included; in-progress work under any identity the
// session may have claimed it under (its RefuseScope: the session name, a
// prior alias, its transient pool alias), or a read that cannot answer,
// holds and says why once.
func TestHoldsOverLiveIncarnation_FailsClosed(t *testing.T) {
	base := sessionpkg.Info{ID: "gc-1", SessionNameMetadata: "worker", Generation: "2"}
	priorAlias := base
	priorAlias.AliasHistory = []string{"worker/nux"}
	slotted := base
	slotted.Template, slotted.PoolManaged, slotted.PoolSlot, slotted.Alias = "worker", true, "1", "worker-1"
	identity := pendingCreateIdentity{id: runtimeTokenMatch, token: runtimeTokenMismatch, generation: runtimeTokenMismatch, epoch: "1"}
	for _, c := range []struct {
		name     string
		info     sessionpkg.Info
		status   string // the work assigned to the session, "" for none
		assignee string // whom the work is assigned to
		noRead   bool   // no work legs at all: every answer unknown
		hold     bool
		why      string
	}{
		{name: "no work", info: base, hold: false},
		{name: "open work only", info: base, status: "open", assignee: "worker", hold: false},
		{name: "in-progress work", info: base, status: "in_progress", assignee: "worker", hold: true, why: "its work is in progress"},
		{name: "in-progress work under a prior alias", info: priorAlias, status: "in_progress", assignee: "worker/nux", hold: true, why: "its work is in progress"},
		{name: "in-progress work under its transient pool alias", info: slotted, status: "in_progress", assignee: "worker-1", hold: true, why: "its work is in progress"},
		{name: "another session's in-progress work", info: priorAlias, status: "in_progress", assignee: "worker/other", hold: false},
		{name: "work unreadable", info: base, noRead: true, hold: true, why: "its work cannot be read"},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := beads.NewMemStore()
			if c.status != "" {
				assignRoutedWork(t, store, c.assignee, c.status)
			}
			cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
			sw := testSeatWork(t.TempDir(), cfg, store, nil)
			if c.noRead {
				sw = nil
			}
			var stderr strings.Builder
			dt := newDrainTracker()
			trace := newPoolDesiredStateTestTrace("worker")
			if got := holdsOverLiveIncarnation(sw, c.info, identity, "worker", "worker", dt, &stderr, trace, time.Now()); got != c.hold {
				t.Fatalf("holdsOverLiveIncarnation = %v, want %v; stderr=%s", got, c.hold, stderr.String())
			}
			if n := liveIncarnationDeferrals(trace); (n == 1) != c.hold || n > 1 {
				t.Fatalf("live_incarnation deferral records = %d, want %d", n, map[bool]int{true: 1}[c.hold])
			}
			if c.hold && !strings.Contains(stderr.String(), c.why) {
				t.Fatalf("stderr = %q, want the hold to say %q", stderr.String(), c.why)
			}
			if !c.hold && stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want nothing said when the hold is released", stderr.String())
			}
		})
	}
}

// stampRuntimeIncarnation stamps the runtime under name as another
// incarnation of session id at epoch.
func stampRuntimeIncarnation(t *testing.T, sp *runtime.Fake, name, id, epoch string) {
	t.Helper()
	for key, value := range map[string]string{"GC_SESSION_ID": id, "GC_INSTANCE_TOKEN": "tok-other", "GC_RUNTIME_EPOCH": epoch} {
		if err := sp.SetMeta(name, key, value); err != nil {
			t.Fatal(err)
		}
	}
}

// TestExecutePreparedStartWave_LiveIncarnationDefers is AC-3's sync wave: a
// pending create whose start finds its name held by a live earlier or later
// incarnation of the same session defers, with no rollback and no stop. The
// deferral carries the colliding start error annotated with the epochs, and is
// bounded by the incarnation's liveness: once it exits, the same start failure
// is handed to the rollback again.
func TestExecutePreparedStartWave_LiveIncarnationDefers(t *testing.T) {
	for _, epoch := range []string{"1", "3"} {
		t.Run("epoch "+epoch, func(t *testing.T) {
			sp := runtime.NewFake()
			if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			stampRuntimeIncarnation(t, sp, "worker", "gc-1", epoch)
			item := preparedStart{candidate: startCandidate{
				info: sessionpkg.Info{ID: "gc-1", SessionNameMetadata: "worker", InstanceToken: "tok-1", Generation: "2", PendingCreateClaim: true},
				tp:   TemplateParams{Command: "true", SessionName: "worker", TemplateName: "worker"},
			}}

			results := executePreparedStartWaveForCity(context.Background(), []preparedStart{item}, t.TempDir(), sp, nil, nil, 10*time.Second, 1)
			if r := results[0]; r.err != nil || r.rollbackPending || r.outcome != TraceOutcomeDeferred {
				t.Fatalf("result err=%v rollbackPending=%v outcome=%q detail=%v, want a live_incarnation deferral", r.err, r.rollbackPending, r.outcome, r.unattributedErr)
			}
			want := "live_incarnation: runtime epoch " + epoch + ", row generation 2"
			if r := results[0]; !errors.Is(r.unattributedErr, runtime.ErrSessionExists) || !strings.Contains(r.unattributedErr.Error(), want) {
				t.Fatalf("unattributedErr = %v, want the colliding start error annotated with %q", r.unattributedErr, want)
			}
			if !sp.IsRunning("worker") || countCalls(sp, "Stop") != 0 {
				t.Fatalf("running=%v stops=%d, want the live incarnation left running", sp.IsRunning("worker"), countCalls(sp, "Stop"))
			}

			if err := sp.Stop("worker"); err != nil {
				t.Fatal(err)
			}
			sp.StartErrors["worker"] = errors.New("launcher exited")
			gone := fmt.Errorf("reading meta: %w", runtime.ErrSessionNotFound)
			sp.GetMetaErrors["worker"] = map[string]error{"GC_SESSION_ID": gone}
			results = executePreparedStartWaveForCity(context.Background(), []preparedStart{item}, t.TempDir(), sp, nil, nil, 10*time.Second, 1)
			if r := results[0]; r.err == nil || !r.rollbackPending {
				t.Fatalf("gone incarnation: err=%v rollbackPending=%v, want the rollback", r.err, r.rollbackPending)
			}
		})
	}
}

// TestCommitAsyncStart_SessionExistsWithLiveIncarnationDefers is AC-3's async
// commit: a session-exists result whose name holds a live earlier or later
// incarnation of the bead-scoped pool row's own session defers, with no
// rollback and no stop, and the row keeps its claim.
func TestCommitAsyncStart_SessionExistsWithLiveIncarnationDefers(t *testing.T) {
	now := time.Date(2026, 8, 15, 0, 0, 1, 0, time.UTC)
	for _, epoch := range []string{"older", "newer"} {
		t.Run(epoch, func(t *testing.T) {
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			info := createBeadScopedPoolRowWithBox(t, store, sp, now)
			name := info.SessionNameMetadata
			generation, err := strconv.Atoi(info.Generation)
			if err != nil {
				t.Fatalf("fixture precondition: generation %q: %v", info.Generation, err)
			}
			delta := 1
			if epoch == "older" {
				delta = -1
			}
			stampRuntimeIncarnation(t, sp, name, info.ID, strconv.Itoa(generation+delta))
			result := successfulPoolStartResult(info, now)
			result.err = fmt.Errorf("%w: session %q", runtime.ErrSessionExists, name)
			result.outcome = TraceOutcomeSessionExists
			result.rollbackPending = true
			result.provider = sp
			var stderr strings.Builder

			if commitAsyncStartResultWithContext(context.Background(), result, sp, store, &clock.Fake{Time: now}, events.Discard, 0, io.Discard, &stderr, nil) {
				t.Fatal("live-incarnation session-exists result reported committed")
			}
			got, err := store.Get(info.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status == "closed" || got.Metadata["pending_create_claim"] != boolMetadata(true) {
				t.Fatalf("row status=%q claim=%q, want the pending create kept; stderr=%s", got.Status, got.Metadata["pending_create_claim"], stderr.String())
			}
			if !sp.IsRunning(name) || countCalls(sp, "Stop") != 0 {
				t.Fatalf("running=%v stops=%d, want the live incarnation left running", sp.IsRunning(name), countCalls(sp, "Stop"))
			}
			if !strings.Contains(stderr.String(), "outcome="+string(TraceOutcomeDeferred)) || !strings.Contains(stderr.String(), "live_incarnation") {
				t.Fatalf("stderr = %q, want a live_incarnation deferral", stderr.String())
			}
		})
	}
}

// TestCommitAsyncStart_LiveIncarnationKeepsRateLimitAndCapacityArms is AC-3's
// screen order, as for the unknown-attribution arm: a rate-limit screen or a
// capacity refusal keeps its own commit arm even when a live incarnation of
// this session holds the name.
func TestCommitAsyncStart_LiveIncarnationKeepsRateLimitAndCapacityArms(t *testing.T) {
	now := time.Date(2026, 8, 15, 0, 0, 1, 0, time.UTC)
	for _, tc := range []struct {
		name            string
		rateLimitScreen bool
		capacityRefused bool
	}{
		{name: "rate-limit screen", rateLimitScreen: true},
		{name: "capacity refused", capacityRefused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			info := createBeadScopedPoolRowWithBox(t, store, sp, now)
			name := info.SessionNameMetadata
			generation, err := strconv.Atoi(info.Generation)
			if err != nil {
				t.Fatalf("fixture precondition: generation %q: %v", info.Generation, err)
			}
			stampRuntimeIncarnation(t, sp, name, info.ID, strconv.Itoa(generation+1))
			result := successfulPoolStartResult(info, now)
			result.err = errors.New("launcher exited")
			result.outcome = TraceOutcomeProviderError
			result.rollbackPending = true
			result.rateLimitScreen = tc.rateLimitScreen
			result.capacityRefused = tc.capacityRefused
			result.provider = sp
			var stderr strings.Builder

			commitAsyncStartResultWithContext(context.Background(), result, sp, store, &clock.Fake{Time: now}, events.Discard, 0, io.Discard, &stderr, nil)
			if strings.Contains(stderr.String(), "outcome="+string(TraceOutcomeDeferred)) {
				t.Fatalf("stderr = %q, want the %s arm, not the live-incarnation deferral", stderr.String(), tc.name)
			}
			got, err := store.Get(info.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.rateLimitScreen && got.Metadata["sleep_reason"] != string(sessionpkg.SleepReasonRateLimit):
				t.Fatalf("sleep_reason = %q, want the rate-limit hold", got.Metadata["sleep_reason"])
			case tc.capacityRefused && !strings.Contains(stderr.String(), "endpoint refused (capacity)"):
				t.Fatalf("stderr = %q, want the capacity refusal", stderr.String())
			}
		})
	}
}

// TestPendingCreateRollback_RollsBackOnceLiveIncarnationExits is AC-4: a row
// held over its live incarnation is rolled back by the next tick after that
// runtime exits, and its work is released.
func TestPendingCreateRollback_RollsBackOnceLiveIncarnationExits(t *testing.T) {
	env, session, name := stampLiveIncarnation(t, rowPool, "1")
	work := assignLiveIncarnationWork(t, env.store, name)
	assertLiveIncarnationHeld(t, env, env.sp, session, name)

	if err := env.sp.Stop(name); err != nil {
		t.Fatal(err)
	}
	reconcileLiveIncarnationTick(t, env, env.sp, session.ID, newPoolDesiredStateTestTrace("worker"))
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "closed" || got.Metadata["state"] != string(sessionpkg.StateFailedCreate) {
		t.Fatalf("status=%q state=%q, want the pending create rolled back once its live incarnation exited; stderr=%s",
			got.Status, got.Metadata["state"], env.stderr.String())
	}
	if released := releaseOrphanedWork(t, env, session.ID, work); released.Status != "open" || released.Assignee != "" {
		t.Fatalf("work status=%q assignee=%q, want it released", released.Status, released.Assignee)
	}
}

// TestPendingCreateRollback_StopsForeignSessionRuntime is AC-5: under the
// bead-scoped pool name, a live runtime that carries another session's
// GC_SESSION_ID is no incarnation of the row's: it is still stopped and the
// row still closes failed-create.
func TestPendingCreateRollback_StopsForeignSessionRuntime(t *testing.T) {
	env, session, name := stampLiveIncarnation(t, rowPool, "1")
	if err := env.sp.SetMeta(name, "GC_SESSION_ID", "gc-other"); err != nil {
		t.Fatal(err)
	}
	trace := newPoolDesiredStateTestTrace("worker")
	reconcileLiveIncarnationTick(t, env, env.sp, session.ID, trace)

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "closed" || got.Metadata["state"] != string(sessionpkg.StateFailedCreate) {
		t.Fatalf("status=%q state=%q, want the pending create rolled back; stderr=%s", got.Status, got.Metadata["state"], env.stderr.String())
	}
	if env.sp.IsRunning(name) || countCalls(env.sp, "Stop") == 0 {
		t.Fatalf("running=%v stops=%d, want the foreign runtime stopped", env.sp.IsRunning(name), countCalls(env.sp, "Stop"))
	}
	if n := liveIncarnationDeferrals(trace); n != 0 {
		t.Fatalf("live_incarnation deferral records = %d, want 0", n)
	}
}
