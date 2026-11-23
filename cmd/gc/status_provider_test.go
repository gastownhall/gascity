package main

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

type statusProbeProvider struct {
	runtime.Provider
	delay       atomic.Int64
	running     atomic.Bool
	liveness    atomic.Value
	observeErr  error
	observeGate <-chan struct{}
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

func (p *statusProbeProvider) ObserveLivenessWithError(string, []string) (runtime.Liveness, error) {
	p.observeCall.Add(1)
	if p.observeGate != nil {
		<-p.observeGate
	}
	return p.liveness.Load().(runtime.Liveness), p.observeErr
}

func TestStatusProviderTimeoutDoesNotStickAcrossCalls(t *testing.T) {
	origTimeout := statusProviderCallTimeout
	origWarn := statusProviderTimeoutWarning
	t.Cleanup(func() {
		statusProviderCallTimeout = origTimeout
		statusProviderTimeoutWarning = origWarn
	})
	statusProviderCallTimeout = 10 * time.Millisecond
	var warnings atomic.Int32
	statusProviderTimeoutWarning = func() {
		warnings.Add(1)
	}

	base := newStatusProbeProvider()
	base.running.Store(true)
	base.delay.Store(int64(100 * time.Millisecond))
	wrapped := newBoundedStatusProvider(base)

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
	wrapped := newBoundedStatusProvider(base)

	got := runtime.ObserveLiveness(wrapped, "worker", []string{"agent"})
	if !got.Running || !got.Alive {
		t.Fatalf("ObserveLiveness = %#v, want running+alive from native observer", got)
	}
	if calls := base.observeCall.Load(); calls != 1 {
		t.Fatalf("ObserveLiveness calls = %d, want 1", calls)
	}
}

func TestStatusProviderLivenessTimeoutPreservesObservationUncertainty(t *testing.T) {
	origTimeout := statusProviderCallTimeout
	origWarn := statusProviderTimeoutWarning
	t.Cleanup(func() {
		statusProviderCallTimeout = origTimeout
		statusProviderTimeoutWarning = origWarn
	})
	statusProviderCallTimeout = 10 * time.Millisecond
	statusProviderTimeoutWarning = func() {}

	base := newStatusProbeProvider()
	base.liveness.Store(runtime.Liveness{Running: true, Alive: true})
	gate := make(chan struct{})
	base.observeGate = gate
	t.Cleanup(func() { close(gate) })
	wrapped := newBoundedStatusProvider(base)

	got, err := runtime.ObserveLivenessWithError(wrapped, "worker", nil)
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("ObserveLivenessWithError error = %v, want runtime unavailable", err)
	}
	if got != (runtime.Liveness{}) {
		t.Fatalf("ObserveLivenessWithError = %+v, want zero while timed-out observation is unknown", got)
	}
	if !statusProviderPartial(wrapped) {
		t.Fatal("statusProviderPartial = false, want true after liveness timeout")
	}
}

func TestStatusProviderTimeoutMarksPartial(t *testing.T) {
	origTimeout := statusProviderCallTimeout
	origWarn := statusProviderTimeoutWarning
	t.Cleanup(func() {
		statusProviderCallTimeout = origTimeout
		statusProviderTimeoutWarning = origWarn
	})
	statusProviderCallTimeout = 10 * time.Millisecond
	statusProviderTimeoutWarning = func() {}

	base := newStatusProbeProvider()
	base.running.Store(true)
	base.delay.Store(int64(100 * time.Millisecond))
	wrapped := newBoundedStatusProvider(base)

	if wrapped.IsRunning("worker") {
		t.Fatal("IsRunning returned true, want timeout fallback false")
	}
	if !statusProviderPartial(wrapped) {
		t.Fatal("statusProviderPartial = false, want true after runtime probe timeout")
	}
}

// attachProbeProvider answers IsAttachedWithError from fixed values, held at
// gate when it is set.
type attachProbeProvider struct {
	runtime.Provider
	attached bool
	err      error
	gate     <-chan struct{}
}

func (p *attachProbeProvider) IsAttachedWithError(string) (bool, error) {
	if p.gate != nil {
		<-p.gate
	}
	return p.attached, p.err
}

// Both cmd/gc wrappers serve display paths only (gc status, gc session list),
// but they still forward the capability so a reader that asks for the error
// sees the base probe's failure instead of the bool IsAttached's "detached".
func TestCmdProviderWrappersForwardAttachProbeError(t *testing.T) {
	origTimeout := statusProviderCallTimeout
	t.Cleanup(func() { statusProviderCallTimeout = origTimeout })
	// Unbounded, so the statusProvider row forwards the probe instead of
	// racing its timeout.
	statusProviderCallTimeout = 0

	probeErr := fmt.Errorf("attach probe timed out: %w", runtime.ErrRuntimeUnavailable)
	base := &attachProbeProvider{Provider: runtime.NewFake(), err: probeErr}
	for _, tc := range []struct {
		name string
		sp   runtime.Provider
	}{
		{name: "statusProvider", sp: newBoundedStatusProvider(base)},
		{name: "attachmentCachingProvider", sp: &attachmentCachingProvider{Provider: base, cache: map[string]bool{"other": true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attached, err := runtime.IsAttachedWithError(tc.sp, "worker")
			if attached || !errors.Is(err, probeErr) {
				t.Fatalf("IsAttachedWithError = (%v, %v), want (false, %v)", attached, err, probeErr)
			}
		})
	}
}

func TestStatusProviderAttachTimeoutIsUnavailable(t *testing.T) {
	origTimeout := statusProviderCallTimeout
	origWarn := statusProviderTimeoutWarning
	t.Cleanup(func() {
		statusProviderCallTimeout = origTimeout
		statusProviderTimeoutWarning = origWarn
	})
	statusProviderCallTimeout = 10 * time.Millisecond
	statusProviderTimeoutWarning = func() {}

	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	wrapped := newBoundedStatusProvider(&attachProbeProvider{Provider: runtime.NewFake(), attached: true, gate: gate})

	attached, err := runtime.IsAttachedWithError(wrapped, "worker")
	if attached || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("IsAttachedWithError = (%v, %v), want (false, runtime unavailable) on timeout", attached, err)
	}
	if !statusProviderPartial(wrapped) {
		t.Fatal("statusProviderPartial = false, want true after attachment timeout")
	}
}
