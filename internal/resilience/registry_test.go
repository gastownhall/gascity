package resilience

import (
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
	if got != want {
		t.Fatalf("DefaultSettings() = %+v, want %+v", got, want)
	}
}
