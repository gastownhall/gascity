package resilience

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestRegistryReturnsSameBreakerForSameKey(t *testing.T) {
	reg := NewRegistry(Settings{Enabled: true})
	a := reg.Breaker("/city/rigs/vr", OpClassBd)
	b := reg.Breaker("/city/rigs/vr", OpClassBd)
	if a != b {
		t.Fatal("Breaker() returned distinct instances for the same (scope, opClass)")
	}
}

func TestRegistryIsolatesScopes(t *testing.T) {
	reg := NewRegistry(Settings{Enabled: true, ConsecutiveFailures: 1})
	a := reg.Breaker("/city/rigs/vr", OpClassBd)
	b := reg.Breaker("/city/rigs/hq", OpClassBd)
	a.RecordFailure()
	if got := a.State(); got != StateOpen {
		t.Fatalf("scope-a State() = %v, want %v", got, StateOpen)
	}
	if got := b.State(); got != StateClosed {
		t.Fatalf("scope-b State() = %v, want %v (one scope's trip must not poison another)", got, StateClosed)
	}
}

func TestRegistryIsolatesOpClasses(t *testing.T) {
	reg := NewRegistry(Settings{Enabled: true, ConsecutiveFailures: 1})
	a := reg.Breaker("/city", OpClassBd)
	b := reg.Breaker("/city", "sql")
	a.RecordFailure()
	if got := b.State(); got != StateClosed {
		t.Fatalf("other opClass State() = %v, want %v", got, StateClosed)
	}
}

func TestRegistryOnStateChangeReceivesTransitions(t *testing.T) {
	reg := NewRegistry(Settings{Enabled: true, ConsecutiveFailures: 1})
	var mu sync.Mutex
	var got []Transition
	reg.SetOnStateChange(func(tr Transition) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, tr)
	})
	reg.Breaker("/city", OpClassBd).RecordFailure()
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("got %d transitions, want 1", len(got))
	}
	if got[0].Scope != "/city" || got[0].OpClass != OpClassBd || got[0].To != StateOpen {
		t.Fatalf("transition = %+v, want /city/bd -> open", got[0])
	}
}

func TestRegistryOnStateChangeAppliesToExistingBreakers(t *testing.T) {
	reg := NewRegistry(Settings{Enabled: true, ConsecutiveFailures: 1})
	b := reg.Breaker("/city", OpClassBd)
	var mu sync.Mutex
	fired := 0
	reg.SetOnStateChange(func(Transition) {
		mu.Lock()
		defer mu.Unlock()
		fired++
	})
	b.RecordFailure()
	mu.Lock()
	defer mu.Unlock()
	if fired != 1 {
		t.Fatalf("callback fired %d times, want 1 (must reach breakers created before SetOnStateChange)", fired)
	}
}

func TestRegistryStatesSnapshot(t *testing.T) {
	reg := NewRegistry(Settings{Enabled: true, ConsecutiveFailures: 1})
	reg.Breaker("/city", OpClassBd).RecordFailure()
	reg.Breaker("/city/rigs/vr", OpClassBd)
	states := reg.States()
	if len(states) != 2 {
		t.Fatalf("States() has %d entries, want 2", len(states))
	}
	if got := states[Key{Scope: "/city", OpClass: OpClassBd}]; got != StateOpen {
		t.Errorf("city state = %v, want %v", got, StateOpen)
	}
	if got := states[Key{Scope: "/city/rigs/vr", OpClass: OpClassBd}]; got != StateClosed {
		t.Errorf("rig state = %v, want %v", got, StateClosed)
	}
}

func TestRegistryConcurrentBreakerAccess(_ *testing.T) {
	reg := NewRegistry(Settings{Enabled: true})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			scopes := []string{"/a", "/b", "/c"}
			for j := 0; j < 100; j++ {
				b := reg.Breaker(scopes[(n+j)%len(scopes)], OpClassBd)
				if (n+j)%2 == 0 {
					b.RecordFailure()
				} else {
					b.RecordSuccess()
				}
				_ = reg.States()
			}
		}(i)
	}
	wg.Wait()
}

func TestRegistryDefaultSettings(t *testing.T) {
	got := DefaultSettings()
	want := Settings{
		Enabled:             true,
		ConsecutiveFailures: 3,
		OpenBase:            time.Second,
		OpenMax:             60 * time.Second,
		HalfOpenInterval:    15 * time.Second,
	}
	// Settings holds a func (Now), so compare with DeepEqual; nil funcs are equal.
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultSettings() = %+v, want %+v", got, want)
	}
}

func TestRegistry_StatusesReportsTripsCapAndDeadline(t *testing.T) {
	clock := newTestClock()
	settings := capacitySettings()
	settings.Now = clock.Now
	reg := NewRegistry(settings)
	open := reg.Breaker("provider:a", OpClassSessionStart)
	reg.Breaker("provider:b", OpClassSessionStart)
	open.jitter = maxJitter

	open.RecordFailure()
	clock.Advance(5 * time.Second)
	if !open.Allow() {
		t.Fatal("Allow() = false past the deadline, want the probe")
	}
	probeAt := clock.Now()
	open.RecordFailure()

	statuses := reg.Statuses()
	if len(statuses) != 2 {
		t.Fatalf("Statuses() returned %d entries, want 2", len(statuses))
	}
	got := statuses[Key{Scope: "provider:a", OpClass: OpClassSessionStart}]
	want := Status{
		State:       StateOpen,
		Failures:    1,
		Trips:       2,
		Deadline:    probeAt.Add(10 * time.Second),
		BackoffCap:  10 * time.Second,
		LastProbeAt: probeAt,
	}
	if got != want {
		t.Fatalf("Statuses()[provider:a] = %+v, want %+v", got, want)
	}
	if closed := statuses[Key{Scope: "provider:b", OpClass: OpClassSessionStart}]; closed != (Status{State: StateClosed}) {
		t.Fatalf("Statuses()[provider:b] = %+v, want a zero closed status", closed)
	}
}

func TestRegistry_RemoveForgetsBreaker(t *testing.T) {
	reg := NewRegistry(Settings{Enabled: true, ConsecutiveFailures: 1})
	reg.Breaker("gone", OpClassSessionStart).RecordFailure()

	reg.Remove("gone", OpClassSessionStart)

	if len(reg.Statuses()) != 0 {
		t.Fatalf("Statuses() = %v, want the removed breaker gone", reg.Statuses())
	}
	if got := reg.Breaker("gone", OpClassSessionStart).State(); got != StateClosed {
		t.Fatalf("recreated breaker state = %v, want a fresh closed breaker", got)
	}
}

func TestRegistry_SetJitterForTestPinsBackoff(t *testing.T) {
	clock := newTestClock()
	reg := NewRegistry(Settings{Enabled: true, ConsecutiveFailures: 1, OpenBase: time.Second, Now: clock.Now})
	existing := reg.Breaker("existing", OpClassBd)
	reg.SetJitterForTest(maxJitter)
	created := reg.Breaker("created", OpClassBd)

	for _, b := range []*Breaker{existing, created} {
		b.RecordFailure()
		if got, want := b.Status().Deadline, clock.Now().Add(time.Second); !got.Equal(want) {
			t.Fatalf("deadline = %v, want %v with pinned jitter", got, want)
		}
	}
}
