package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/session"
)

// The fresh heal's effect tests (CONTRACT v5 A6, R2; C5d's open race and
// its max review, M1-M3).

// nameFree fails the test unless city's lock on name is free.
func nameFree(t *testing.T, city, name string) {
	t.Helper()
	unlock := runtimeNames.tryLock(city, name)
	if unlock == nil {
		t.Fatalf("the runtime name lock on %q is still held", name)
	}
	unlock()
}

// Kills the heal orphaning a live runtime: the inventory reads the name
// gone, but a start still holds the name, or settled deferred with its
// runtime up. The heal refuses while the name is locked, on an incomplete or
// unsupported read, and over anything present (a live agent, a live pane
// whose agent reads dead, which a booting agent can, or a corpse A12 has not
// classified), writing nothing. Over a gone runtime it writes under the
// lock. Every path leaves the lock free.
func TestFreshHealNeverOrphansALiveRuntime(t *testing.T) {
	unavailable := errors.Join(runtime.ErrRuntimeUnavailable, errors.New("probe"))
	for _, tc := range []struct {
		name   string
		sp     runtime.Provider
		locked bool
		cause  string
	}{
		{"name held by a start", gone(), true, causeNameBusy},
		{"alive", &freshObserver{Fake: runtime.NewFake(), l: runtime.Liveness{Running: true, Alive: true}}, false, causeRuntimePresent},
		{"live pane, agent dead", &freshObserver{Fake: runtime.NewFake(), l: runtime.Liveness{Running: true}}, false, causeRuntimePresent},
		{"corpse", &freshObserver{Fake: runtime.NewFake(), l: runtime.Liveness{Corpse: true}}, false, causeRuntimePresent},
		{"incomplete", &freshObserver{Fake: runtime.NewFake(), err: unavailable}, false, causeLivenessUnknown},
		{"no error-bearing read", runtime.NewFake(), false, causeLivenessUnsupported},
		{"no provider", nil, false, causeRouteUnknown},
		{"gone", gone(), false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newHealCase(t, livenessGone, desireNone, "state", "creating")
			if tc.locked {
				unlock := runtimeNames.tryLock(c.w.CityPath, "s-heal")
				defer func() {
					if runtimeNames.tryLock(c.w.CityPath, "s-heal") != nil {
						t.Error("the refused heal released a lock it does not hold")
					}
					unlock()
				}()
			} else {
				defer nameFree(t, c.w.CityPath, "s-heal")
			}
			lockedAtCAS := false
			adversary := &interleavedStore{Store: c.store, id: c.k.ID, between: func() {
				if unlock := runtimeNames.tryLock(c.w.CityPath, "s-heal"); unlock != nil {
					unlock()
				} else {
					lockedAtCAS = true
				}
			}}
			_, s := c.run(t, tc.sp, adversary)
			if tc.cause != "" {
				if s.Outcome != settledRefused || s.Cause != tc.cause {
					t.Fatalf("settlement %+v, want refused with cause %q", s, tc.cause)
				}
				if m := c.meta(t); m["state"] != "creating" {
					t.Fatalf("state %q, want creating: the heal wrote over a runtime it did not prove gone", m["state"])
				}
				return
			}
			if s.Outcome != settledLanded || c.meta(t)["state"] != "asleep" {
				t.Fatalf("settlement %+v, state %q, want the heal landed", s, c.meta(t)["state"])
			}
			if !lockedAtCAS {
				t.Fatal("the heal's CAS ran without the runtime name lock")
			}
		})
	}
}

// Kills M1, the review's repro: an explicit wake (wake_request, which moves
// neither the incarnation nor the decision the pass's allocation made) that
// lands while the heal reads the runtime fresh. The heal's premise refuses
// it, as legacy's lifecycle fence does, and the row keeps its state and
// continuation.
func TestFreshHealRefusesAWakeThatLandsDuringTheRead(t *testing.T) {
	named := []string{"configured_named_session", "true", "configured_named_identity", "chat", "configured_named_mode", "on_demand"}
	c := newHealCase(t, livenessGone, desireNone, append([]string{"state", "active", "session_key", "k-1"}, named...)...)
	sp := gone()
	sp.read = func() {
		if err := c.store.SetMetadataBatch(c.k.ID, map[string]string{"wake_request": "explicit", "wake_requested_at": rowAt(0)}); err != nil {
			t.Error(err)
		}
	}
	it, s := c.run(t, sp, nil)
	if it.Reason != decideDeadRuntimeHeal || s.Outcome != settledRefused || s.Cause != causePremise {
		t.Fatalf("intent %q, settlement %+v, want the dead-runtime heal refused with cause %q", it.Reason, s, causePremise)
	}
	if m := c.meta(t); m["state"] != "active" || m["session_key"] != "k-1" {
		t.Fatalf("row %v, want it active with its continuation", m)
	}
}

// Kills a deadline during the fresh read read as a refusal: the effect
// settles failed with cause deadline, writing nothing, and frees the lock.
func TestFreshHealDeadlineDuringTheReadSettlesFailed(t *testing.T) {
	c := newHealCase(t, livenessGone, desireNone, "state", "creating")
	defer nameFree(t, c.w.CityPath, "s-heal")
	ctx, cancel := context.WithCancelCause(context.Background())
	sp := gone()
	sp.read = func() { cancel(context.DeadlineExceeded) }
	p, it := c.pass(t, sp, nil)
	if s := runTx(ctx, p, it, effectSpecs[it.Kind], nil); s.Outcome != settledFailed || s.Cause != causeDeadline {
		t.Fatalf("settlement %+v, want failed with cause %q", s, causeDeadline)
	}
	if got := c.meta(t)["state"]; got != "creating" {
		t.Fatalf("state %q, want creating", got)
	}
}

// sinceObserver answers l to a fresh read only for a refresh begun at or
// after notBefore (LL2's FreshLivenessObserver); its snapshot read never
// answers.
type sinceObserver struct {
	*runtime.Fake
	notBefore time.Time
	l         runtime.Liveness
}

func (o *sinceObserver) ObserveLivenessSince(_ string, _ []string, since time.Time) (runtime.Liveness, error) {
	if since.Before(o.notBefore) {
		return runtime.Liveness{}, runtime.ErrRuntimeUnavailable
	}
	return o.l, nil
}

// checkedClock is a planner clock that runs check at each Now.
type checkedClock struct {
	plannerClock
	check func()
}

func (c checkedClock) Now() time.Time { c.check(); return c.plannerClock.Now() }

func (o *sinceObserver) ObserveLivenessWithError(string, []string) (runtime.Liveness, error) {
	return runtime.Liveness{}, runtime.ErrRuntimeUnavailable
}

// Kills a read that is not fresh for the effect: the heal reads through
// LL2's fresh read, since the pass's clock read once the transaction holds
// the name lock. A refresh older than that time does not prove the name
// gone.
func TestFreshHealReadIsFreshForTheEffect(t *testing.T) {
	t0 := gatherNow.Add(time.Hour)
	for _, tc := range []struct {
		name      string
		notBefore time.Time
		want      settleOutcome
	}{
		{"refresh begun at the effect's time", t0, settledLanded},
		{"only a later refresh would answer", t0.Add(time.Second), settledRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newHealCase(t, livenessGone, desireNone, "state", "creating")
			p, it := c.pass(t, &sinceObserver{Fake: runtime.NewFake(), notBefore: tc.notBefore}, nil)
			p.Clock = checkedClock{newFakePlannerClock(t0), func() {
				if unlock := runtimeNames.tryLock(c.w.CityPath, "s-heal"); unlock != nil {
					unlock()
					t.Error("the fresh read's time was taken before the name lock")
				}
			}}
			if s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil); s.Outcome != tc.want {
				t.Fatalf("settlement %+v, want outcome %v", s, tc.want)
			}
		})
	}
}

// Kills a read through the routed leaf alone, and a heal over a route the
// composite does not know: under the auto composite, a runtime alive on the
// backend the name is not routed to still refuses the heal (the
// fall-through hop reads it, so the name is not proven absent); before the
// composite's routes are seeded the heal refuses route-unknown.
func TestFreshHealSeesTheOtherBackend(t *testing.T) {
	for _, seeded := range []bool{true, false} {
		c := newHealCase(t, livenessGone, desireNone, "state", "creating")
		sp := auto.New(gone(), &freshObserver{Fake: runtime.NewFake(), l: runtime.Liveness{Running: true, Alive: true}})
		want := causeRouteUnknown
		if seeded {
			sp.SeedRoutes(nil)
			want = causeLivenessUnknown
		}
		if _, s := c.run(t, sp, nil); s.Outcome != settledRefused || s.Cause != want {
			t.Fatalf("seeded %v: settlement %+v, want refused with cause %q", seeded, s, want)
		}
	}
}

// Kills repeated heal refusals kept silent: an A6 heal refused until its
// backoff reaches the cap alerts once, not before and not again.
func TestHealRefusalsAlertAtTheCap(t *testing.T) {
	f := newObserveFixture(t)
	for i := 1; i <= healRefusalsBeforeAlert+2; i++ {
		f.clk.Advance(10 * time.Minute) // past the last backoff
		f.p.settlements.post(settlement{Key: rowKeyOf("gc-1"), Kind: intentRowHealFresh, Outcome: settledRefused, Cause: causeRuntimePresent})
		f.p.drainSettlements(f.clk.Now())
		want := 0
		if i >= healRefusalsBeforeAlert {
			want = 1
		}
		if got := len(f.alerts(t)); got != want {
			t.Fatalf("after %d refusals: %d alerts, want %d", i, got, want)
		}
	}
}

// Kills the awake heal mixing backends: under the auto composite, an own
// Current runtime alive on the backend the name is not routed to is not
// read through the routed leaf, so the heal refuses (a stale route only
// refuses, the C4c2 re-review ruling); on the routed backend it lands.
func TestAwakeHealReadsTheRoutedLeafOnly(t *testing.T) {
	for _, routed := range []bool{true, false} {
		c := ownRuntimeCase(t, "tok-3", "state", "asleep", "sleep_reason", "idle")
		own := &freshObserver{
			Fake: runtime.NewFake(), l: runtime.Liveness{Running: true, Alive: true},
			env: map[string]string{"GC_SESSION_ID": c.k.ID, "GC_INSTANCE_TOKEN": "tok-3"},
		}
		sp := auto.New(gone(), own)
		if routed {
			sp.SeedRoutes([]string{"s-heal"})
		} else {
			sp.SeedRoutes(nil)
		}
		_, s := c.run(t, sp, nil)
		if routed != (s.Outcome == settledLanded) || !routed && s.Cause != causeRuntimeNotOwn {
			t.Fatalf("routed %v: settlement %+v", routed, s)
		}
	}
}

// lockedObserver answers alive once up is set; up is written under the row's
// session mutation lock, as Manager.Start/Submit start a runtime under it.
type lockedObserver struct {
	*runtime.Fake
	mu sync.Mutex
	up bool
}

func (o *lockedObserver) ObserveLivenessSince(name string, pn []string, _ time.Time) (runtime.Liveness, error) {
	return o.ObserveLivenessWithError(name, pn)
}

func (o *lockedObserver) ObserveLivenessWithError(string, []string) (runtime.Liveness, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.up {
		return runtime.Liveness{Running: true, Alive: true}, nil
	}
	return runtime.Liveness{}, nil
}

// Kills the fresh read taken outside the row's session mutation lock (the
// re-review's pin for M1(a)): an in-process Manager start (Submit/Send ->
// ensureRunning) holds that lock across the provider Start and writes
// nothing on an active row. The heal waits for the lock, reads the runtime
// it brought up, and refuses.
func TestFreshHealReadsUnderTheSessionMutationLock(t *testing.T) {
	named := []string{"configured_named_session", "true", "configured_named_identity", "chat", "configured_named_mode", "on_demand"}
	c := newHealCase(t, livenessGone, desireNone, append([]string{"state", "active", "session_key", "k-1"}, named...)...)
	sp := &lockedObserver{Fake: runtime.NewFake()}
	p, it := c.pass(t, sp, nil)
	held, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = session.WithSessionMutationLock(c.k.ID, func() error {
			close(held)
			<-release
			sp.mu.Lock()
			sp.up = true // the Manager's provider Start lands, no row write
			sp.mu.Unlock()
			return nil
		})
	}()
	<-held
	done := make(chan settlement, 1)
	go func() { done <- runTx(context.Background(), p, it, effectSpecs[it.Kind], nil) }()
	select {
	case s := <-done:
		t.Fatalf("the heal settled %+v while an in-process start held the row's lock", s)
	case <-time.After(50 * time.Millisecond): // the heal reaches the lock (or, read outside it, reads gone)
	}
	close(release)
	s := <-done
	if s.Outcome != settledRefused || s.Cause != causeRuntimePresent {
		t.Fatalf("settlement %+v, row state %q, want refused %q", s, c.meta(t)["state"], causeRuntimePresent)
	}
}

// Kills the awake heal trusting a sidecar that outlived its runtime (acp,
// subprocess), or one read across two runtimes: the runtime reads alive,
// the identity read finds the row's token, and by the bracketing re-read
// the runtime is gone, or another object runs under the name. The heal
// refuses and the row stays asleep.
func TestAwakeHealRechecksPresenceAfterTheIdentityRead(t *testing.T) {
	for name, after := range map[string]runtime.Liveness{
		"exited":   {},
		"replaced": {Running: true, Alive: true, ObjectID: "$2"},
	} {
		c := ownRuntimeCase(t, "tok-3", "state", "asleep", "sleep_reason", "idle")
		sp := &freshObserver{
			Fake: runtime.NewFake(), l: runtime.Liveness{Running: true, Alive: true, ObjectID: "$1"},
			env: map[string]string{"GC_SESSION_ID": c.k.ID, "GC_INSTANCE_TOKEN": "tok-3"},
		}
		reads := 0
		sp.read = func() {
			if reads++; reads == 2 {
				sp.l = after
			}
		}
		_, s := c.run(t, sp, nil)
		if s.Outcome != settledRefused || s.Cause != causeRuntimeNotOwn || c.meta(t)["state"] != "asleep" {
			t.Fatalf("%s: settlement %+v, state %q, want refused %q and the row asleep", name, s, c.meta(t)["state"], causeRuntimeNotOwn)
		}
	}
}

// Kills the heal deciding on a cached row (the transaction's backing read):
// over a caching leg, a wake request written behind the cache after the
// pass refuses the heal on its premise, on its first attempt's row, read
// from the backing (a cached read would spend an attempt on a lost CAS).
func TestFreshHealDecidesOnTheBackingRow(t *testing.T) {
	c := newHealCase(t, livenessGone, desireNone, "state", "creating")
	sp, reads := gone(), 0
	sp.read = func() { reads++ }
	cache := beads.NewCachingStoreForTest(simBacking{c.store}, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.before = func() {
		if err := c.store.SetMetadataBatch(c.k.ID, map[string]string{"wake_request": "explicit", "wake_requested_at": rowAt(0)}); err != nil {
			t.Error(err)
		}
	}
	if _, s := c.run(t, sp, cache); s.Outcome != settledRefused || s.Cause != causePremise || reads != 1 || c.meta(t)["state"] != "creating" {
		t.Fatalf("settlement %+v after %d attempts, state %q, want refused %q on the first", s, reads, c.meta(t)["state"], causePremise)
	}
}

// Kills the heal writing the pass's patch without deciding again on the
// fresh row: a suspend (sleep_intent, which no lifecycle fact carries) or a
// wait hold landing after the pass makes the row operator-dormant, so the
// awake heal refuses redecided and the row stays asleep.
func TestAwakeHealDecidesAgainOnTheFreshRow(t *testing.T) {
	for _, kv := range [][]string{{"sleep_intent", "user-hold"}, {"wait_hold", "op"}} {
		c := ownRuntimeCase(t, "tok-3", "state", "asleep", "sleep_reason", "idle")
		c.before = func() {
			if err := c.store.SetMetadataBatch(c.k.ID, map[string]string{kv[0]: kv[1]}); err != nil {
				t.Error(err)
			}
		}
		own := &freshObserver{
			Fake: runtime.NewFake(), l: runtime.Liveness{Running: true, Alive: true},
			env: map[string]string{"GC_SESSION_ID": c.k.ID, "GC_INSTANCE_TOKEN": "tok-3"},
		}
		if _, s := c.run(t, own, nil); s.Outcome != settledRefused || s.Cause != causeRedecided || c.meta(t)["state"] != "asleep" {
			t.Fatalf("%s since the pass: settlement %+v, state %q, want refused %q", kv[0], s, c.meta(t)["state"], causeRedecided)
		}
	}
}

// Kills a fresh heal that drops the A6 items folded into it (C5d2's one CAS
// per row, CONTRACT v5.8e): the dead-runtime heal of a row whose detached_at
// marker is set lands the heal and the marker's clear in its one CAS, the
// merged patch decided again on the fresh row.
func TestFreshHealLandsItsFoldedItemsInOneCAS(t *testing.T) {
	c := newHealCase(t, livenessGone, desireWake, "state", "active", "session_key", "k-1", "detached_at", rowAt(-time.Hour))
	it, s := c.run(t, gone(), nil)
	if it.Kind != intentRowHealFresh || s.Outcome != settledLanded {
		t.Fatalf("intent (%q, %q), settlement %+v, want the fresh heal landed", it.Kind, it.Reason, s)
	}
	if m := c.meta(t); m["state"] != "asleep" || m["detached_at"] != "" || m["session_key"] != "" {
		t.Fatalf("row %v, want asleep, its continuation reset and its marker cleared in the one CAS", m)
	}
}

// Kills a fold written from the pass's decision rather than decided again on
// the fresh row (review pins): a detached_at marker cleared by another
// writer before the effect is not written back stale, and one stamped since
// the pass is cleared in the heal's one CAS.
func TestFreshHealDecidesItsFoldOnTheFreshRow(t *testing.T) {
	for _, c := range []struct {
		name          string
		marker, write string
	}{
		{"marker cleared since the pass", rowAt(-time.Hour), ""},
		{"marker stamped since the pass", "", rowAt(-time.Minute)},
	} {
		meta := []string{"state", "active", "session_key", "k-1"}
		if c.marker != "" {
			meta = append(meta, "detached_at", c.marker)
		}
		hc := newHealCase(t, livenessGone, desireWake, meta...)
		hc.before = func() {
			if err := hc.store.SetMetadataBatch(hc.k.ID, map[string]string{"detached_at": c.write}); err != nil {
				t.Error(err)
			}
		}
		it, s := hc.run(t, gone(), nil)
		if it.Kind != intentRowHealFresh || s.Outcome != settledLanded {
			t.Fatalf("%s: intent (%q, %q), settlement %+v, want the fresh heal landed", c.name, it.Kind, it.Reason, s)
		}
		if m := hc.meta(t); m["state"] != "asleep" || m["detached_at"] != "" || m["session_key"] != "" {
			t.Fatalf("%s: row %v, want asleep, its continuation reset and no marker, in the one CAS", c.name, m)
		}
	}
}
