package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// inc003Env builds a live, active session bead that is not in the desired
// state. With configured=false the template is absent from config and the drain
// reason is "orphaned"; with configured=true the template is configured but
// undesired and the reason is "suspended". wokeAt returns the raw last_woke_at
// for the env's clock.
func inc003Env(t *testing.T, configured bool, wokeAt func(now time.Time) string) (*reconcilerTestEnv, beads.Bead) {
	t.Helper()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	if configured {
		env.cfg = &config.City{
			Agents:        []config.Agent{{Name: "worker"}},
			NamedSessions: []config.NamedSession{{Template: "worker"}},
		}
	}
	name := "orphan"
	if configured {
		name = "worker"
	}
	if err := env.sp.Start(context.Background(), name, runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	session := env.createSessionBead(name, name)
	env.setSessionMetadata(&session, map[string]string{"state": "active", "last_woke_at": wokeAt(env.clk.Now())})
	return env, session
}

func wokeAgo(d time.Duration) func(now time.Time) string {
	return func(now time.Time) string { return now.Add(-d).UTC().Format(time.RFC3339) }
}

func (e *reconcilerTestEnv) reconcileINC003Traced(trace *sessionReconcilerTraceCycle, sessions ...beads.Bead) {
	cfgNames := configuredSessionNames(e.cfg, "", e.store)
	reconcileSessionBeadsTraced(
		context.Background(), "", sessions, e.desiredState, cfgNames, e.cfg, e.sp,
		e.store, nil, nil, nil, nil, e.dt, map[string]int{}, false, nil, "",
		nil, e.clk, e.rec, 0, 0, &e.stdout, &e.stderr, trace,
		e.startOptions...,
	)
}

func inc003GraceRecord(t *testing.T, trace *sessionReconcilerTraceCycle, name string) SessionReconcilerTraceRecord {
	t.Helper()
	for _, r := range trace.records {
		if r.SiteCode == TraceSiteReconcilerOrphaned && r.SessionName == name && r.ReasonCode == TraceReasonUndesiredWakeGrace {
			return r
		}
	}
	t.Fatalf("no %s/%s trace record for %q; records: %+v", TraceSiteReconcilerOrphaned, TraceReasonUndesiredWakeGrace, name, trace.records)
	return SessionReconcilerTraceRecord{}
}

// A live undesired session woken 30s ago is kept: no drain begins, the runtime
// stays up, and the deferral is traced and logged. Without the grace this is
// the INC-003 storm (drain the fresh seat, ack, kill, re-wake; ~34 kills/min).
func TestReconcileSessionBeads_INC003_UndesiredWokeUnder5mKept(t *testing.T) {
	env, session := inc003Env(t, false, wokeAgo(30*time.Second))
	wokeAt := session.Metadata["last_woke_at"]
	trace := newPoolDesiredStateTestTrace("orphan")

	env.reconcileINC003Traced(trace, session)

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("drain began inside the undesired-wake grace: reason=%q", ds.reason)
	}
	if !env.sp.IsRunning("orphan") {
		t.Fatal("runtime stopped inside the undesired-wake grace")
	}
	r := inc003GraceRecord(t, trace, "orphan")
	if r.OutcomeCode != TraceOutcomeDeferred {
		t.Errorf("outcome = %q, want %q", r.OutcomeCode, TraceOutcomeDeferred)
	}
	for key, want := range map[string]any{
		"drain_reason":   "orphaned",
		"last_woke_at":   wokeAt,
		"grace_s":        300,
		"provider_alive": true,
	} {
		if got := r.Fields[key]; got != want {
			t.Errorf("trace field %s = %#v, want %#v", key, got, want)
		}
	}
	if !strings.Contains(env.stdout.String(), "Skipping orphaned drain for 'orphan': woken within the 5m0s undesired-wake grace") {
		t.Errorf("stdout missing grace skip line:\n%s", env.stdout.String())
	}
}

// The grace is level-triggered and bounded: the same row is kept one second
// before the grace ends and drained on the first tick at the boundary.
func TestReconcileSessionBeads_INC003_DrainsAfterGrace(t *testing.T) {
	env, session := inc003Env(t, false, wokeAgo(0))
	woke := env.clk.Now()

	env.clk.Time = woke.Add(wakeUndesiredGrace - time.Second)
	env.reconcile([]beads.Bead{session})
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("drain began 1s before the grace ended: reason=%q", ds.reason)
	}

	env.clk.Time = woke.Add(wakeUndesiredGrace)
	env.reconcile([]beads.Bead{session})
	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatal("expected the orphaned drain once the grace elapsed")
	}
	if ds.reason != "orphaned" {
		t.Errorf("drain reason = %q, want %q", ds.reason, "orphaned")
	}
}

// An empty or unparseable last_woke_at is no evidence of a recent wake, so the
// row drains on the first tick as it did before INC-003.
func TestReconcileSessionBeads_INC003_UnparseableLastWokeDrains(t *testing.T) {
	for _, tc := range []struct{ name, lastWokeAt string }{
		{"empty", ""},
		{"garbage", "not-a-time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, session := inc003Env(t, false, func(time.Time) string { return tc.lastWokeAt })

			env.reconcile([]beads.Bead{session})

			ds := env.dt.get(session.ID)
			if ds == nil {
				t.Fatalf("last_woke_at=%q must not earn the grace; expected an orphaned drain", tc.lastWokeAt)
			}
			if ds.reason != "orphaned" {
				t.Errorf("drain reason = %q, want %q", ds.reason, "orphaned")
			}
		})
	}
}

// CONTRACT v4 §3.3 arm 18 / C4.9 covers every undesired drain, not only
// orphans: a configured-but-undesired ("suspended") row woken inside the grace
// is kept too.
func TestReconcileSessionBeads_INC003_SuspendedReasonAlsoDeferred(t *testing.T) {
	env, session := inc003Env(t, true, wokeAgo(time.Minute))
	trace := newPoolDesiredStateTestTrace("worker")

	env.reconcileINC003Traced(trace, session)

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("suspended drain began inside the undesired-wake grace: reason=%q", ds.reason)
	}
	if got := inc003GraceRecord(t, trace, "worker").Fields["drain_reason"]; got != "suspended" {
		t.Errorf("trace drain_reason = %#v, want %q", got, "suspended")
	}
}

// Clock skew must never pin a row. A last_woke_at modestly ahead of now (the
// wake can be stamped by another process) is inside the grace, but one
// wakeUndesiredGrace or more ahead gets no grace and drains as before.
func TestReconcileSessionBeads_INC003_FarFutureLastWokeDrains(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ahead     time.Duration
		wantDrain bool
	}{
		{"modest-skew-kept", wakeUndesiredGrace - time.Second, false},
		{"at-grace-drains", wakeUndesiredGrace, true},
		{"far-future-drains", 24 * time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, session := inc003Env(t, false, wokeAgo(-tc.ahead))

			env.reconcile([]beads.Bead{session})

			ds := env.dt.get(session.ID)
			if tc.wantDrain && (ds == nil || ds.reason != "orphaned") {
				t.Fatalf("last_woke_at %s ahead: drain = %+v, want an orphaned drain", tc.ahead, ds)
			}
			if !tc.wantDrain && ds != nil {
				t.Fatalf("last_woke_at %s ahead: drain began (reason=%q), want it kept inside the grace", tc.ahead, ds.reason)
			}
		})
	}
}

// A drain already tracked keeps running inside the grace, and the grace is not
// logged or traced for it: beginSessionDrainInfo would no-op, so a grace record
// would claim a deferral that is not happening.
func TestReconcileSessionBeads_INC003_TrackedDrainNotReportedAsGrace(t *testing.T) {
	env, session := inc003Env(t, false, wokeAgo(30*time.Second))
	env.dt.set(session.ID, &drainState{
		startedAt:  env.clk.Now(),
		deadline:   env.clk.Now().Add(defaultDrainTimeout),
		reason:     "orphaned",
		generation: 1,
	})
	trace := newPoolDesiredStateTestTrace("orphan")

	env.reconcileINC003Traced(trace, session)

	if ds := env.dt.get(session.ID); ds == nil || ds.reason != "orphaned" {
		t.Fatalf("tracked drain = %+v, want the orphaned drain left in flight", ds)
	}
	for _, r := range trace.records {
		if r.ReasonCode == TraceReasonUndesiredWakeGrace {
			t.Fatalf("grace traced for a row whose drain is already tracked: %+v", r)
		}
	}
	if strings.Contains(env.stdout.String(), "undesired-wake grace") {
		t.Fatalf("grace logged for a row whose drain is already tracked:\n%s", env.stdout.String())
	}
}

// wakeGracePreservesUndesiredRow is the exact rule v2's arm 18 reuses.
func TestWakeGracePreservesUndesiredRow(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	for _, tc := range []struct {
		name, lastWokeAt string
		want             bool
	}{
		{"just-woken", at(0), true},
		{"inside", at(-wakeUndesiredGrace + time.Second), true},
		{"at-grace", at(-wakeUndesiredGrace), false},
		{"modest-future", at(wakeUndesiredGrace - time.Second), true},
		{"future-at-grace", at(wakeUndesiredGrace), false},
		{"empty", "", false},
		{"unparseable", "yesterday", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := sessionpkg.Info{LastWokeAt: tc.lastWokeAt}
			if got := wakeGracePreservesUndesiredRow(info, now); got != tc.want {
				t.Fatalf("wakeGracePreservesUndesiredRow(last_woke_at=%q) = %v, want %v", tc.lastWokeAt, got, tc.want)
			}
		})
	}
}
