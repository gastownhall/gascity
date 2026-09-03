package main

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

type statusProbeProvider struct {
	runtime.Provider
	delay       atomic.Int64
	running     atomic.Bool
	liveness    atomic.Value
	observeCall atomic.Int32
}

func newStatusProbeProvider() *statusProbeProvider {
	p := &statusProbeProvider{Provider: runtime.NewFake()}
	p.liveness.Store(runtime.Liveness{})
	return p
}

func (p *statusProbeProvider) IsRunning(string) bool {
	time.Sleep(time.Duration(p.delay.Load()))
	return p.running.Load()
}

func (p *statusProbeProvider) ObserveLiveness(string, []string) runtime.Liveness {
	p.observeCall.Add(1)
	return p.liveness.Load().(runtime.Liveness)
}

func TestStatusProviderTimeoutDoesNotStickAcrossCalls(t *testing.T) {
	origWarn := statusProviderTimeoutWarning
	t.Cleanup(func() { statusProviderTimeoutWarning = origWarn })
	var warnings atomic.Int32
	statusProviderTimeoutWarning = func() {
		warnings.Add(1)
	}

	base := newStatusProbeProvider()
	base.running.Store(true)
	base.delay.Store(int64(100 * time.Millisecond))
	wrapped := newBoundedStatusProvider(base, 10*time.Millisecond)

	if wrapped.IsRunning("worker") {
		t.Fatal("first IsRunning returned true, want timeout fallback false")
	}
	base.delay.Store(0)
	if !wrapped.IsRunning("worker") {
		t.Fatal("second IsRunning returned false, want fresh provider result after timeout")
	}
	if got := warnings.Load(); got != 1 {
		t.Fatalf("timeout warnings = %d, want 1", got)
	}
}

func TestStatusProviderPreservesNativeLivenessObservation(t *testing.T) {
	base := newStatusProbeProvider()
	base.liveness.Store(runtime.Liveness{Running: true, Alive: true})
	wrapped := newBoundedStatusProvider(base, 10*time.Millisecond)

	got := runtime.ObserveLiveness(wrapped, "worker", []string{"agent"})
	if !got.Running || !got.Alive {
		t.Fatalf("ObserveLiveness = %#v, want running+alive from native observer", got)
	}
	if calls := base.observeCall.Load(); calls != 1 {
		t.Fatalf("ObserveLiveness calls = %d, want 1", calls)
	}
}

func TestStatusProviderTimeoutMarksPartial(t *testing.T) {
	origWarn := statusProviderTimeoutWarning
	t.Cleanup(func() { statusProviderTimeoutWarning = origWarn })
	statusProviderTimeoutWarning = func() {}

	base := newStatusProbeProvider()
	base.running.Store(true)
	base.delay.Store(int64(100 * time.Millisecond))
	wrapped := newBoundedStatusProvider(base, 10*time.Millisecond)

	if wrapped.IsRunning("worker") {
		t.Fatal("IsRunning returned true, want timeout fallback false")
	}
	if !statusProviderPartial(wrapped) {
		t.Fatal("statusProviderPartial = false, want true after runtime probe timeout")
	}
}

// A city that widens [session] status_probe_timeout must actually widen the
// budget: a probe slower than the built-in default but inside the configured
// one has to return the provider's real answer, not the partial fallback.
func TestStatusProviderConfiguredTimeoutWidensBudget(t *testing.T) {
	base := newStatusProbeProvider()
	base.running.Store(true)
	base.delay.Store(int64(80 * time.Millisecond))

	tight := newBoundedStatusProvider(base, config.DefaultStatusProbeTimeout)
	if tight.IsRunning("worker") {
		t.Fatal("IsRunning returned true at the default budget, want the timeout fallback for an 80ms probe")
	}
	if !statusProviderPartial(tight) {
		t.Fatal("statusProviderPartial = false at the default budget, want true")
	}

	widened := newBoundedStatusProvider(base, 2*time.Second)
	if !widened.IsRunning("worker") {
		t.Fatal("IsRunning returned false at a 2s budget, want the provider result for an 80ms probe")
	}
	if statusProviderPartial(widened) {
		t.Fatal("statusProviderPartial = true at a 2s budget, want false — the probe fit inside it")
	}
}

// A non-positive configured budget opts out of the bound entirely, so even a
// probe far slower than any default must run to completion unmarked.
func TestStatusProviderNonPositiveTimeoutDisablesBound(t *testing.T) {
	base := newStatusProbeProvider()
	base.running.Store(true)
	base.delay.Store(int64(20 * time.Millisecond))

	wrapped := newBoundedStatusProvider(base, 0)
	if !wrapped.IsRunning("worker") {
		t.Fatal("IsRunning returned false with the bound disabled, want the provider result")
	}
	if statusProviderPartial(wrapped) {
		t.Fatal("statusProviderPartial = true with the bound disabled, want false")
	}
}

// Each bound provider carries its own budget, so re-binding must not silently
// reset an already-bounded provider to a different one.
func TestStatusProviderRebindKeepsOriginalTimeout(t *testing.T) {
	base := newStatusProbeProvider()
	base.running.Store(true)
	base.delay.Store(int64(80 * time.Millisecond))

	widened := newBoundedStatusProvider(base, 2*time.Second)
	rebound := newBoundedStatusProvider(widened, time.Millisecond)
	if rebound != widened {
		t.Fatal("re-binding a bounded provider returned a new provider, want the original")
	}
	if !rebound.IsRunning("worker") {
		t.Fatal("IsRunning returned false after re-binding, want the 2s budget it was bound with")
	}
}

func TestStatusProbeTimeoutForCity(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.City
		want time.Duration
	}{
		{"nil city", nil, config.DefaultStatusProbeTimeout},
		{"unset", &config.City{}, config.DefaultStatusProbeTimeout},
		{"configured", &config.City{Session: config.SessionConfig{StatusProbeTimeout: "400ms"}}, 400 * time.Millisecond},
		{"invalid falls back", &config.City{Session: config.SessionConfig{StatusProbeTimeout: "nope"}}, config.DefaultStatusProbeTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statusProbeTimeoutForCity(tt.cfg); got != tt.want {
				t.Errorf("statusProbeTimeoutForCity() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStatusObservationTimeoutForCity(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.City
		want time.Duration
	}{
		{"nil city", nil, config.DefaultStatusObservationTimeout},
		{"unset", &config.City{}, config.DefaultStatusObservationTimeout},
		{"configured", &config.City{Session: config.SessionConfig{StatusObservationTimeout: "5s"}}, 5 * time.Second},
		{"invalid falls back", &config.City{Session: config.SessionConfig{StatusObservationTimeout: "nope"}}, config.DefaultStatusObservationTimeout},
		{"non-positive disables", &config.City{Session: config.SessionConfig{StatusObservationTimeout: "0s"}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statusObservationTimeoutForCity(tt.cfg); got != tt.want {
				t.Errorf("statusObservationTimeoutForCity() = %v, want %v", got, tt.want)
			}
		})
	}
}
