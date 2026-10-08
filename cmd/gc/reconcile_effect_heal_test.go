package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
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
		{"no provider", nil, false, causeLivenessUnsupported},
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
// lands while the heal reads the runtime fresh. The heal refuses superseded,
// as legacy's lifecycle fence does, and the row keeps its state and
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
	if it.Reason != decideDeadNamedHeal || s.Outcome != settledRefused || s.Cause != causeSuperseded {
		t.Fatalf("intent %q, settlement %+v, want the dead named heal refused with cause %q", it.Reason, s, causeSuperseded)
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
	ctx, cancel := context.WithCancel(context.Background())
	sp := gone()
	sp.read = cancel
	p, it := c.pass(t, sp, nil)
	if s := rowHealFreshEffect(p, it)(ctx); s.Outcome != settledFailed || s.Cause != causeDeadline {
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

func (o *sinceObserver) ObserveLivenessWithError(string, []string) (runtime.Liveness, error) {
	return runtime.Liveness{}, runtime.ErrRuntimeUnavailable
}

// Kills a read that is not fresh for the effect: the heal reads through
// LL2's fresh read, from the injected clock, taken once it holds the name
// lock. A refresh older than that time does not prove the name gone.
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
			now := func() time.Time {
				if unlock := runtimeNames.tryLock(c.w.CityPath, "s-heal"); unlock != nil {
					unlock()
					t.Error("the fresh read's time was taken before the name lock")
				}
				return t0
			}
			if s := (freshHeal{pass: p, it: it, now: now}).run(context.Background()); s.Outcome != tc.want {
				t.Fatalf("settlement %+v, want outcome %v", s, tc.want)
			}
		})
	}
}

// Kills a read through the routed leaf alone, and a heal over a route the
// composite does not know: under the auto composite, a runtime alive on the
// backend the name is not routed to still refuses the heal (the composite's
// fall-through read); before the composite's routes are seeded the heal
// refuses route-unknown.
func TestFreshHealSeesTheOtherBackend(t *testing.T) {
	for _, seeded := range []bool{true, false} {
		c := newHealCase(t, livenessGone, desireNone, "state", "creating")
		sp := auto.New(gone(), &freshObserver{Fake: runtime.NewFake(), l: runtime.Liveness{Running: true, Alive: true}})
		want := causeRouteUnknown
		if seeded {
			sp.SeedRoutes(nil)
			want = causeRuntimePresent
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
