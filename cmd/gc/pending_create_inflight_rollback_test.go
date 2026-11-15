package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// runtimeFirstStartProvider is a slow remote runtime (the crucible exec
// runtime on EKS): Start creates the box under the session name at once, so
// IsRunning reports it, but returns only once the box is ready. It keeps no
// per-session metadata, so GC_SESSION_ID and GC_INSTANCE_TOKEN read unset, and
// it ignores cancellation once the create is accepted.
type runtimeFirstStartProvider struct {
	*runtime.Fake
	created chan string
	release chan struct{}
}

func (p *runtimeFirstStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := p.Fake.Start(ctx, name, cfg); err != nil {
		return err
	}
	p.created <- name
	<-p.release
	return nil
}

// inFlightDeferrals counts the start-in-flight deferrals of the alive
// pending-create rollback in trace.
func inFlightDeferrals(trace *sessionReconcilerTraceCycle) int {
	n := 0
	for _, r := range trace.records {
		if r.SiteCode == TraceSiteReconcilerPendingCreate && r.ReasonCode == TraceReasonWake &&
			r.OutcomeCode == TraceOutcomeStartInFlight && r.Fields["attribution"] == "start_in_flight" {
			n++
		}
	}
	return n
}

// runSlowStart starts a pending create of "worker" on a runtimeFirstStartProvider
// over store, then runs a second reconciler tick advance later, while the
// start is still creating the box, and releases the start once that tick
// returns or blocks. meta, when given, is added to the row before the first
// tick. It returns the env, the provider, the row and the second tick's trace.
func runSlowStart(t *testing.T, store beads.Store, advance time.Duration, meta ...map[string]string) (*reconcilerTestEnv, *runtimeFirstStartProvider, beads.Bead, *sessionReconcilerTraceCycle) {
	t.Helper()
	env := newReconcilerTestEnv(t)
	if store != nil {
		env.store = store
	}
	sp := &runtimeFirstStartProvider{Fake: env.sp, created: make(chan string, 1), release: make(chan struct{})}
	released := false
	releaseStart := func() {
		if !released {
			released = true
			close(sp.release)
		}
	}
	starts := &asyncStartTracker{}
	t.Cleanup(func() {
		releaseStart()
		starts.wait(5 * time.Second)
	})
	env.startOptions = append(env.startOptions, withAsyncStartExecution(), withAsyncStartTracker(starts))
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"state":                     "creating",
		"pending_create_claim":      "true",
		"pending_create_started_at": pendingCreateStartedAtNow(env.clk.Now()),
	})
	for _, m := range meta {
		env.setSessionMetadata(&session, m)
	}

	tick := func(trace *sessionReconcilerTraceCycle) {
		got, err := env.store.Get(session.ID)
		if err != nil {
			t.Error(err)
			return
		}
		cfgNames := configuredSessionNames(env.cfg, "", env.store)
		reconcileSessionBeadsTraced(
			context.Background(), env.city, []beads.Bead{got}, env.desiredState, cfgNames, env.cfg, sp,
			env.store, nil, nil, nil, nil, env.dt, map[string]int{"worker": 1}, false, nil, "",
			nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, trace,
			env.startOptions...,
		)
	}

	tick(nil)
	select {
	case <-sp.created:
	case <-time.After(5 * time.Second):
		t.Fatalf("the start never created the box; stderr=%s", env.stderr.String())
	}
	env.clk.Advance(advance)
	// The next tick lands while the start is still creating the box. On a
	// store without the atomic close a rollback waits on the start's session
	// lock, so the start is released once the tick returns or blocks.
	trace := newPoolDesiredStateTestTrace("worker")
	ticked := make(chan struct{})
	go func() {
		defer close(ticked)
		tick(trace)
	}()
	select {
	case <-ticked:
	case <-time.After(time.Second):
	}
	releaseStart()
	select {
	case <-ticked:
	case <-time.After(5 * time.Second):
		t.Fatal("the tick did not finish once the start returned")
	}
	if !starts.wait(5 * time.Second) {
		t.Fatal("async start did not finish")
	}
	return env, sp, session, trace
}

// TestReconcileSessionBeads_InFlightStartOfUnstampedRuntimeIsNotRolledBack
// pins the AWS-cell orphan: a reconciler tick that lands while the row's own
// Start is still creating the box sees a live runtime with no identity. The
// rollback closed the row without stopping the box ("live runtime belongs to
// another session"), the late Start then found the row closed and could not
// attribute the box either, so the box ran with no row until the cell's reaper
// removed it. The in-flight start owns that window: the row stays pending and
// the start commits it. It runs on MemStore and on FileStore, the AWS cell's
// store, whose atomic close lands the rollback mid-start at once. The second
// tick lands inside the in-flight window and, ten minutes in, past it, where
// the start's live runtime lease alone holds the fence.
func TestReconcileSessionBeads_InFlightStartOfUnstampedRuntimeIsNotRolledBack(t *testing.T) {
	for _, store := range []string{"mem", "file"} {
		for _, advance := range []time.Duration{2 * time.Second, 10 * time.Minute} {
			t.Run(store+"/"+advance.String(), func(t *testing.T) {
				var st beads.Store
				if store == "file" {
					fs, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
					if err != nil {
						t.Fatal(err)
					}
					st = fs
				}
				env, sp, session, trace := runSlowStart(t, st, advance)
				got, err := env.store.Get(session.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.Status == "closed" && sp.IsRunning("worker") {
					t.Fatalf("orphan: the row closed (%s) with its runtime still running; stderr=%s", got.Metadata["close_reason"], env.stderr.String())
				}
				if got.Status == "closed" || got.Metadata["pending_create_claim"] != "" || got.Metadata["state"] != "active" {
					t.Fatalf("status=%q claim=%q state=%q, want the start committed; stderr=%s",
						got.Status, got.Metadata["pending_create_claim"], got.Metadata["state"], env.stderr.String())
				}
				if !sp.IsRunning("worker") {
					t.Fatal("the committed session's runtime is gone")
				}
				if n := inFlightDeferrals(trace); n != 1 {
					t.Fatalf("start-in-flight deferrals = %d, want 1; stderr=%s", n, env.stderr.String())
				}
			})
		}
	}
}

// TestReconcileSessionBeads_AlivePendingCreateRollbackInFlightFence pins the
// fence's edges with no start holding the runtime lease: only a runtime with
// no identity at all is spared, and only while the row's own start is inside
// its in-flight window (startup_timeout + slack, the configured timeout
// included). Positive evidence of another owner still rolls back at once, an
// unstamped runtime rolls back once the window ends, and a last_woke_at far
// in the future (a clock stepped back) holds nothing.
func TestReconcileSessionBeads_AlivePendingCreateRollbackInFlightFence(t *testing.T) {
	for _, tc := range []struct {
		name           string
		startupTimeout string
		wokeAgo        time.Duration
		meta           map[string]string
		wantRollback   bool
	}{
		{name: "unstamped, start in flight", wokeAgo: 2 * time.Second, wantRollback: false},
		{name: "unstamped, in-flight window over", wokeAgo: 30 * time.Minute, wantRollback: true},
		{name: "unstamped, 120s into a 180s startup timeout", startupTimeout: "180s", wokeAgo: 120 * time.Second, wantRollback: false},
		{name: "unstamped, 200s past a 180s startup timeout", startupTimeout: "180s", wokeAgo: 200 * time.Second, wantRollback: true},
		{name: "unstamped, woken a minute in the future", wokeAgo: -time.Minute, wantRollback: false},
		{name: "unstamped, woken ten minutes in the future", wokeAgo: -10 * time.Minute, wantRollback: true},
		{name: "unstamped, woken six hours in the future", wokeAgo: -6 * time.Hour, wantRollback: true},
		{name: "another session's ID, start in flight", wokeAgo: 2 * time.Second, meta: map[string]string{"GC_SESSION_ID": "gc-other"}, wantRollback: true},
		{name: "another incarnation's token, start in flight", wokeAgo: 2 * time.Second, meta: map[string]string{"GC_INSTANCE_TOKEN": "other-token"}, wantRollback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, session := newAlivePendingCreateEnv(t)
			env.cfg.Session.StartupTimeout = tc.startupTimeout
			env.setSessionMetadata(&session, map[string]string{
				"last_woke_at": env.clk.Now().Add(-tc.wokeAgo).UTC().Format(time.RFC3339),
			})
			for k, v := range tc.meta {
				_ = env.sp.SetMeta("worker", k, v)
			}
			trace := newPoolDesiredStateTestTrace("worker")
			reconcileTraced(env, []beads.Bead{session}, trace)
			got, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if rolledBack := got.Status == "closed"; rolledBack != tc.wantRollback {
				t.Fatalf("rolled back = %v, want %v; stderr=%s", rolledBack, tc.wantRollback, env.stderr.String())
			}
			if want := map[bool]int{true: 0, false: 1}[tc.wantRollback]; inFlightDeferrals(trace) != want {
				t.Fatalf("start_in_flight deferrals = %d, want %d", inFlightDeferrals(trace), want)
			}
			for _, r := range trace.records {
				if shouldAutoArmForTrace(r.ReasonCode, r.OutcomeCode) && !tc.wantRollback {
					t.Fatalf("a deferral armed auto-trace: %+v", r)
				}
				// Every deferral here is inside the window: the lease is not read.
				if r.OutcomeCode == TraceOutcomeStartInFlight && r.Fields["lease"] != string(ownStartInWindow) {
					t.Fatalf("deferral lease = %v, want %q", r.Fields["lease"], ownStartInWindow)
				}
			}
			if !tc.wantRollback && !strings.Contains(env.stderr.String(), "deferred: its start is in flight") {
				t.Fatalf("the deferral line does not name the in-flight start; stderr=%s", env.stderr.String())
			}
			if !env.sp.IsRunning("worker") {
				t.Fatal("the rollback stopped the runtime; a foreign or deferred runtime is never stopped here")
			}
		})
	}
}

// leaseRecord is a runtime lease record another host holds, expiring at
// expires.
func leaseRecord(expires string) map[string]string {
	return map[string]string{
		sessionpkg.RuntimeLeaseHolderKey:  "host-b/7/n",
		sessionpkg.RuntimeLeaseEpochKey:   "4",
		sessionpkg.RuntimeLeaseTTLKey:     "240",
		sessionpkg.RuntimeLeaseExpiresKey: expires,
		sessionpkg.RuntimeLeaseFlockKey:   "another-boot/1/2/0123",
	}
}

// blockLeaseLocks makes the city's runtime lease locks unreadable: the lock
// dir is a file.
func blockLeaseLocks(t *testing.T, city string) {
	t.Helper()
	locks := citylayout.SessionNameLocksDir(city)
	if err := os.RemoveAll(locks); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(locks), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(locks, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
}

const leaseWarning = "WARNING: holding pending create worker open: its runtime lease cannot be verified"

// TestReconcileSessionBeads_LeaseHoldsTheFencePastTheWindow pins the lease
// arm past the in-flight window (last_woke_at 30 minutes ago), on the record
// another host holds: only an unexpired, well-formed record holds the row.
func TestReconcileSessionBeads_LeaseHoldsTheFencePastTheWindow(t *testing.T) {
	now := time.Now().UTC() // the record is compared on the wall clock
	for _, tc := range []struct {
		name         string
		rec          map[string]string
		wantRollback bool
	}{
		{"no record", nil, true},
		{"another host, unexpired", leaseRecord(now.Add(time.Minute).Format(time.RFC3339)), false},
		{"another host, expired (a dead controller)", leaseRecord(now.Add(-time.Second).Format(time.RFC3339)), true},
		{"an expiry beyond its ttl (malformed)", leaseRecord(now.Add(10 * time.Hour).Format(time.RFC3339)), true},
		{"an unparseable expiry", leaseRecord("garbage"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, session := newAlivePendingCreateEnv(t)
			if tc.rec != nil {
				env.setSessionMetadata(&session, tc.rec)
			}
			trace := newPoolDesiredStateTestTrace("worker")
			reconcileTraced(env, []beads.Bead{session}, trace)
			got, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if rolled := got.Status == "closed"; rolled != tc.wantRollback {
				t.Fatalf("rolled back = %v, want %v; stderr=%s", rolled, tc.wantRollback, env.stderr.String())
			}
			if !tc.wantRollback {
				for _, r := range trace.records {
					if r.OutcomeCode == TraceOutcomeStartInFlight && r.Fields["lease"] != string(ownStartLeaseHeld) {
						t.Fatalf("deferral lease = %v, want %q", r.Fields["lease"], ownStartLeaseHeld)
					}
				}
				if !strings.Contains(env.stderr.String(), "deferred: its start holds the runtime lease") {
					t.Fatalf("the deferral line does not name the held lease; stderr=%s", env.stderr.String())
				}
			}
		})
	}
}

// TestReconcileSessionBeads_UnverifiableLeaseFailsClosedAndWarnsOnce pins a
// lease probe that fails: an unverifiable lease may be a live start's, so the
// row is held open (fail closed), the hold is warned once per row per
// episode with its error, and the deferral's trace records the lease as
// unverifiable. An episode that begins inside the window and continues past
// it with the probe failing warns once; so does one that begins failing.
// Once the lease reads free, the rollback applies.
func TestReconcileSessionBeads_UnverifiableLeaseFailsClosedAndWarnsOnce(t *testing.T) {
	env, session := newAlivePendingCreateEnv(t)
	env.setSessionMetadata(&session, map[string]string{
		"last_woke_at": env.clk.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339),
	})
	tick := func(trace *sessionReconcilerTraceCycle) {
		cur, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatal(err)
		}
		reconcileTraced(env, []beads.Bead{cur}, trace)
	}
	tick(newPoolDesiredStateTestTrace("worker")) // inside the window
	if strings.Contains(env.stderr.String(), leaseWarning) {
		t.Fatalf("warned inside the window, where the lease is not read; stderr=%s", env.stderr.String())
	}
	inWindow := env.stderr.Len()
	env.clk.Advance(30 * time.Minute)
	blockLeaseLocks(t, env.city)
	for i := 1; i <= 3; i++ {
		trace := newPoolDesiredStateTestTrace("worker")
		tick(trace)
		if got, _ := env.store.Get(session.ID); got.Status == "closed" {
			t.Fatalf("tick %d: rolled back while the lease could not be verified; stderr=%s", i, env.stderr.String())
		}
		if n := inFlightDeferrals(trace); n != 1 {
			t.Fatalf("tick %d: deferrals = %d, want 1", i, n)
		}
		for _, r := range trace.records {
			if r.OutcomeCode == TraceOutcomeStartInFlight && r.Fields["lease"] != string(ownStartLeaseUnverifiable) {
				t.Fatalf("tick %d: deferral lease = %v, want %q", i, r.Fields["lease"], ownStartLeaseUnverifiable)
			}
		}
	}
	if n := strings.Count(env.stderr.String(), leaseWarning); n != 1 {
		t.Fatalf("lease warnings = %d, want 1 for the episode; stderr=%s", n, env.stderr.String())
	}
	if out := env.stderr.String()[inWindow:]; strings.Contains(out, "deferred:") {
		t.Fatalf("past the window the WARNING alone says the hold, with no deferral line beside it; stderr=%s", out)
	}
	if !strings.Contains(env.stderr.String(), "creating lock dir") {
		t.Fatalf("the warning does not carry the read error; stderr=%s", env.stderr.String())
	}
	if err := os.Remove(citylayout.SessionNameLocksDir(env.city)); err != nil {
		t.Fatal(err)
	}
	tick(newPoolDesiredStateTestTrace("worker"))
	if got, _ := env.store.Get(session.ID); got.Status != "closed" {
		t.Fatalf("readable, unheld lease: status=%q, want rolled back; stderr=%s", got.Status, env.stderr.String())
	}

	// An episode that begins with the probe already failing.
	env2, session2 := newAlivePendingCreateEnv(t)
	blockLeaseLocks(t, env2.city)
	for range 3 {
		cur, _ := env2.store.Get(session2.ID)
		reconcileTraced(env2, []beads.Bead{cur}, newPoolDesiredStateTestTrace("worker"))
	}
	if got, _ := env2.store.Get(session2.ID); got.Status == "closed" {
		t.Fatalf("fresh episode: rolled back while the lease could not be verified; stderr=%s", env2.stderr.String())
	}
	if n := strings.Count(env2.stderr.String(), leaseWarning); n != 1 {
		t.Fatalf("fresh episode: lease warnings = %d, want 1; stderr=%s", n, env2.stderr.String())
	}
	if strings.Contains(env2.stderr.String(), "deferred:") {
		t.Fatalf("fresh episode: the WARNING alone says the hold, with no deferral line beside it; stderr=%s", env2.stderr.String())
	}
}

// TestStartHolderLiveErrNamesWhatItCannotProbe pins startHolderLiveErr's
// unprobeable branches: each reads held (the reaper's bool form keeps them
// held) and names its reason.
func TestStartHolderLiveErrNamesWhatItCannotProbe(t *testing.T) {
	info := sessionpkg.Info{ID: "gc-1", SessionName: "worker"}
	for _, tc := range []struct {
		name  string
		store beads.Store
		city  string
		info  sessionpkg.Info
	}{
		{"no store", nil, t.TempDir(), info},
		{"a relative city path", beads.NewMemStore(), "city", info},
		{"no session name", beads.NewMemStore(), t.TempDir(), sessionpkg.Info{ID: "gc-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			held, err := startHolderLiveErr(tc.store, tc.city, tc.info)
			if !held || err == nil {
				t.Fatalf("startHolderLiveErr = (%v, %v), want held with an error", held, err)
			}
			if !startHolderLive(tc.store, tc.city, tc.info) {
				t.Fatal("startHolderLive reads it free; want held")
			}
		})
	}
}

// Kills a hold that moves from the in-flight window to a held lease without
// saying so: the deferral line is one standing condition per reason, so the
// row's first pass past the window, held by another host's lease, prints the
// held line after the in-flight one.
func TestReconcileSessionBeads_FenceWindowToHeldLeasePrintsTheHeldLine(t *testing.T) {
	env, session := newAlivePendingCreateEnv(t)
	env.setSessionMetadata(&session, map[string]string{
		"last_woke_at": env.clk.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339),
	})
	tick := func() {
		cur, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatal(err)
		}
		reconcileTraced(env, []beads.Bead{cur}, newPoolDesiredStateTestTrace("worker"))
	}
	tick()
	if !strings.Contains(env.stderr.String(), "deferred: its start is in flight") {
		t.Fatalf("inside the window: want the in-flight line; stderr=%s", env.stderr.String())
	}
	inWindow := env.stderr.Len()
	env.clk.Advance(30 * time.Minute)
	env.setSessionMetadata(&session, leaseRecord(time.Now().UTC().Add(time.Minute).Format(time.RFC3339)))
	tick()
	if got, _ := env.store.Get(session.ID); got.Status == "closed" {
		t.Fatalf("rolled back under a held lease; stderr=%s", env.stderr.String())
	}
	if out := env.stderr.String()[inWindow:]; !strings.Contains(out, "deferred: its start holds the runtime lease") {
		t.Fatalf("past the window, held: want the held line; stderr=%s", out)
	}
}
