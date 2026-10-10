package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestNewCityDirRefusesNoCity: a CityDir is made only from an absolute path
// naming an existing directory, so no lifecycle verb can be handed "" or a
// relative or computed path as its city.
func TestNewCityDirRefusesNoCity(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", "  ", ".", "city", "./city", file, filepath.Join(dir, "missing")} {
		if d, err := NewCityDir(path); err == nil || !d.IsZero() {
			t.Errorf("NewCityDir(%q) = %q, %v; want an error and no city", path, d.Path(), err)
		}
	}
	d, err := NewCityDir(" " + dir + "/ ")
	if err != nil || d.IsZero() || d.Path() != dir {
		t.Fatalf("NewCityDir(%q) = %q, %v; want %q", dir, d.Path(), err, dir)
	}
	if zero := (CityDir{}); !zero.IsZero() || zero.Path() != "" {
		t.Fatalf("the zero CityDir names %q", zero.Path())
	}
}

// TestActorLeaseModes: the lease mode is derived from the actor's kind, and a
// kind that names no actor is refused.
func TestActorLeaseModes(t *testing.T) {
	for _, c := range []struct {
		kind     ActorKind
		mode     leaseMode
		consumes bool
	}{
		{ActorOperator, leaseWait, true},
		{ActorAgent, leaseWait, false},
		{ActorBackground, leaseWait, false},
		{ActorController, leaseTry, false},
		{ActorSweep, leaseNone, false},
	} {
		by := Actor{Kind: c.kind}
		if mode, err := by.leaseMode(); mode != c.mode || err != nil {
			t.Errorf("kind %d: lease mode %d, %v; want %d", c.kind, mode, err, c.mode)
		}
		if by.consumesHold() != c.consumes {
			t.Errorf("kind %d: consumesHold = %v, want %v", c.kind, by.consumesHold(), c.consumes)
		}
	}
	for _, kind := range []ActorKind{0, ActorSweep + 1, -1} {
		if _, err := (Actor{Kind: kind}).leaseMode(); !errors.Is(err, ErrNoActor) {
			t.Errorf("kind %d: %v, want ErrNoActor", kind, err)
		}
	}
}

// TestEachActorTakesItsLeaseMode runs a Manager kill and a runtime-name lease
// for each actor against a lease another holder keeps: an operator, agent or
// background caller waits, then gets ErrSessionStarting; the controller gets
// the bare ErrRuntimeLeaseBusy at once; a stop sweep stops past it.
func TestEachActorTakesItsLeaseMode(t *testing.T) {
	for _, c := range []struct {
		kind    ActorKind
		waits   bool
		stopped bool
	}{
		{ActorOperator, true, false},
		{ActorAgent, true, false},
		{ActorBackground, true, false},
		{ActorController, false, false},
		{ActorSweep, false, true},
	} {
		m := newManagerLeaseFixture(t)
		if err := m.start(m.operator(nil)); err != nil {
			t.Fatal(err)
		}
		held := m.hold(t)
		began := time.Now()
		err := m.mgr.Kill(context.Background(), m.as(c.kind), m.info.ID)
		took := time.Since(began)
		switch {
		case c.stopped:
			if err != nil || len(m.sp.stops) != 1 {
				t.Errorf("kind %d: kill under a held lease = %v (stops %q), want stopped past it", c.kind, err, m.sp.stops)
			}
		case c.waits:
			if !errors.Is(err, ErrSessionStarting) || took < 400*time.Millisecond || len(m.sp.stops) != 0 {
				t.Errorf("kind %d: kill under a held lease = %v after %v (stops %q), want ErrSessionStarting after the wait", c.kind, err, took, m.sp.stops)
			}
		default:
			if !errors.Is(err, ErrRuntimeLeaseBusy) || errors.Is(err, ErrSessionStarting) || took > 300*time.Millisecond || len(m.sp.stops) != 0 {
				t.Errorf("kind %d: kill under a held lease = %v after %v (stops %q), want ErrRuntimeLeaseBusy at once", c.kind, err, took, m.sp.stops)
			}
		}
		release, err := LeaseRuntimeName(context.Background(), m.as(c.kind), m.info.SessionName)
		release()
		switch {
		case c.stopped:
			if err != nil {
				t.Errorf("kind %d: the sweep's name lease = %v, want none taken", c.kind, err)
			}
		case c.waits:
			if !errors.Is(err, ErrSessionStarting) {
				t.Errorf("kind %d: name lease under a held flock = %v, want ErrSessionStarting", c.kind, err)
			}
		default:
			if !errors.Is(err, ErrRuntimeLeaseBusy) || errors.Is(err, ErrSessionStarting) {
				t.Errorf("kind %d: name lease under a held flock = %v, want ErrRuntimeLeaseBusy", c.kind, err)
			}
		}
		held.Release()
	}
}

// TestLifecycleVerbsRefuseAnActorWithoutCityOrKind: with the refusal expected,
// every lifecycle verb that would start or stop a runtime refuses an Actor
// with no City (ErrRuntimeLeaseNoCity), and every one refuses an Actor with no
// kind (ErrNoActor), before any provider call.
func TestLifecycleVerbsRefuseAnActorWithoutCityOrKind(t *testing.T) {
	ExpectNoCityRefusalsForTest(t)
	type verb func(m managerLeaseFixture, by Actor) error
	hints := func(m managerLeaseFixture) runtime.Config { return runtime.Config{WorkDir: m.info.WorkDir} }
	starts := map[string]verb{
		"Start": func(m managerLeaseFixture, by Actor) error { return m.start(by) },
		"StartRuntimeOnly": func(m managerLeaseFixture, by Actor) error {
			return m.mgr.StartRuntimeOnly(context.Background(), by, m.info.ID, BuildResumeCommand(m.info), hints(m))
		},
		"Attach": func(m managerLeaseFixture, by Actor) error {
			return m.mgr.Attach(context.Background(), by, m.info.ID, BuildResumeCommand(m.info), hints(m))
		},
		"Send": func(m managerLeaseFixture, by Actor) error {
			_, err := m.mgr.Send(context.Background(), by, m.info.ID, "hi", BuildResumeCommand(m.info), hints(m))
			return err
		},
		"SendImmediate": func(m managerLeaseFixture, by Actor) error {
			_, err := m.mgr.SendImmediate(context.Background(), by, m.info.ID, "hi", BuildResumeCommand(m.info), hints(m))
			return err
		},
		"Submit": func(m managerLeaseFixture, by Actor) error {
			_, err := m.mgr.Submit(context.Background(), by, m.info.ID, "hi", BuildResumeCommand(m.info), hints(m), SubmitIntentDefault)
			return err
		},
		"TryWaitIdleNudge": func(m managerLeaseFixture, by Actor) error {
			_, err := m.mgr.TryWaitIdleNudge(context.Background(), by, m.info.ID, "test", "hi", BuildResumeCommand(m.info), hints(m))
			return err
		},
	}
	stops := map[string]verb{
		"Kill": func(m managerLeaseFixture, by Actor) error { return m.mgr.Kill(context.Background(), by, m.info.ID) },
		"Suspend": func(m managerLeaseFixture, by Actor) error {
			return m.mgr.Suspend(context.Background(), by, m.info.ID, false)
		},
		"Suspend soft": func(m managerLeaseFixture, by Actor) error {
			return m.mgr.Suspend(context.Background(), by, m.info.ID, true)
		},
		"Close": func(m managerLeaseFixture, by Actor) error { return m.mgr.Close(context.Background(), by, m.info.ID) },
	}
	for _, set := range []struct {
		verbs   map[string]verb
		running bool
	}{{starts, false}, {stops, true}} {
		for name, do := range set.verbs {
			for _, c := range []struct {
				by   func(m managerLeaseFixture) Actor
				want error
			}{
				{func(managerLeaseFixture) Actor { return Actor{Kind: ActorOperator} }, ErrRuntimeLeaseNoCity},
				{func(m managerLeaseFixture) Actor { return Actor{City: m.as(ActorOperator).City} }, ErrNoActor},
			} {
				m := newManagerLeaseFixture(t)
				if set.running {
					if err := m.start(m.operator(nil)); err != nil {
						t.Fatal(err)
					}
				}
				startsBefore, stopsBefore := len(m.sp.starts), len(m.sp.stops)
				want := c.want
				if name == "StartRuntimeOnly" && errors.Is(want, ErrRuntimeLeaseNoCity) {
					want = ErrNoCallerLease // it takes no lease of its own, so it needs its caller's
				}
				if err := do(m, c.by(m)); !errors.Is(err, want) {
					t.Errorf("%s: %v, want %v", name, err, want)
				}
				if len(m.sp.starts) != startsBefore || len(m.sp.stops) != stopsBefore {
					t.Errorf("%s: the refused call reached the provider (starts %q, stops %q)", name, m.sp.starts, m.sp.stops)
				}
			}
		}
	}
}

// TestQueueForEachActor: whether an actor may start or resume a dormant row
// is its kind's. Only an operator resumes a held row; a background caller
// never starts a dormant one itself; any other resumes only an unheld one.
func TestQueueForEachActor(t *testing.T) {
	held := map[string]string{"state": string(StateAsleep), "held_until": "2099-01-01T00:00:00Z", "sleep_intent": "user-hold"}
	unheld := map[string]string{"state": string(StateAsleep)}
	mgr := NewManagerWithOptions(nil, runtime.NewFake())
	for _, c := range []struct {
		kind               ActorKind
		queueHeld, queueUn bool
	}{
		{ActorOperator, false, false},
		{ActorAgent, true, false},
		{ActorController, true, false},
		{ActorBackground, true, true},
	} {
		if got := mgr.queueFor(held, "s-down", Actor{Kind: c.kind}); got != c.queueHeld {
			t.Errorf("kind %d, held: queue = %v, want %v", c.kind, got, c.queueHeld)
		}
		if got := mgr.queueFor(unheld, "s-down", Actor{Kind: c.kind}); got != c.queueUn {
			t.Errorf("kind %d, unheld: queue = %v, want %v", c.kind, got, c.queueUn)
		}
	}
}

// TestLifecycleVerbsRefuseAKindlessActorWithoutALease: a verb that takes no
// lease (a send into a live runtime, a suspend of a suspended row) still
// refuses an Actor with no kind.
func TestLifecycleVerbsRefuseAKindlessActorWithoutALease(t *testing.T) {
	m := newManagerLeaseFixture(t)
	if err := m.start(m.operator(nil)); err != nil {
		t.Fatal(err)
	}
	kindless := Actor{City: m.as(ActorOperator).City}
	if _, err := m.mgr.Send(context.Background(), kindless, m.info.ID, "hi", BuildResumeCommand(m.info), runtime.Config{WorkDir: m.info.WorkDir}); !errors.Is(err, ErrNoActor) {
		t.Fatalf("Send into a live runtime by no actor = %v, want ErrNoActor", err)
	}
	if n := m.sp.CountCalls("Nudge", m.info.SessionName) + m.sp.CountCalls("SendKeys", m.info.SessionName); n != 0 {
		t.Fatalf("the refused send delivered %d calls", n)
	}
	if err := m.mgr.Suspend(context.Background(), m.operator(nil), m.info.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := m.mgr.Suspend(context.Background(), kindless, m.info.ID, false); !errors.Is(err, ErrNoActor) {
		t.Fatalf("Suspend of a suspended row by no actor = %v, want ErrNoActor", err)
	}
}
