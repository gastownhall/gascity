package auto

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// livenessObserverStub is a Fake that also reports a fixed LivenessObserver
// verdict, so a test can prove auto preserves the routed backend's native
// liveness fast-path rather than collapsing to the generic IsRunning+ProcessAlive
// fold.
type livenessObserverStub struct {
	*runtime.Fake
	obs           runtime.Liveness
	invalidations []string
}

func (s *livenessObserverStub) ObserveLiveness(string, []string) runtime.Liveness { return s.obs }

func (s *livenessObserverStub) InvalidateLiveness(name string) {
	s.invalidations = append(s.invalidations, name)
}

type errorBearingLivenessObserverStub struct {
	*runtime.Fake
	obs runtime.Liveness
	err error
}

func (s *errorBearingLivenessObserverStub) ObserveLivenessWithError(string, []string) (runtime.Liveness, error) {
	return s.obs, s.err
}

// TestProvider_ForwardsObserveLivenessToRoutedBackend guards the herdr
// singleton-liveness fix against the auto wrapper. When a LivenessObserver
// backend (herdr) is wrapped in an auto router — which happens whenever a
// herdr-default city also routes some sessions to ACP — auto.ObserveLiveness
// must still reach that backend's fast-path. Otherwise the reconciler falls
// back to the fragile process-table walk and the singleton restart-loop
// returns for mixed herdr/ACP cities.
func TestProvider_ForwardsObserveLivenessToRoutedBackend(t *testing.T) {
	def := &livenessObserverStub{Fake: runtime.NewFake(), obs: runtime.Liveness{Running: true, Alive: true}}
	acp := &livenessObserverStub{Fake: runtime.NewFake(), obs: runtime.Liveness{Running: true, Alive: false}}
	p := New(def, acp)
	p.RouteACP("acpsess")

	if got := p.ObserveLiveness("plain", []string{"claude"}); got != def.obs {
		t.Errorf("default route ObserveLiveness = %+v; want %+v (backend fast-path lost)", got, def.obs)
	}
	if got := p.ObserveLiveness("acpsess", []string{"claude"}); got != acp.obs {
		t.Errorf("acp route ObserveLiveness = %+v; want %+v", got, acp.obs)
	}
}

// TestProvider_ObserveLivenessFallsThroughOnStaleRoute proves the stale-route
// recovery that mirrors IsRunning: when the in-memory route table has no entry
// for a session (e.g. after a controller restart clears it), a session that is
// actually live on the ACP backend must not be misread as dead just because the
// default backend — where the missing route sends it — reports not-running.
func TestProvider_ObserveLivenessFallsThroughOnStaleRoute(t *testing.T) {
	def := &livenessObserverStub{Fake: runtime.NewFake(), obs: runtime.Liveness{Running: false}}
	acp := &livenessObserverStub{Fake: runtime.NewFake(), obs: runtime.Liveness{Running: true, Alive: true}}
	p := New(def, acp)
	// No RouteACP entry: routing is stale, so route() sends "acpsess" to def.

	if got := p.ObserveLiveness("acpsess", []string{"claude"}); got != acp.obs {
		t.Errorf("stale-route ObserveLiveness = %+v; want %+v (fallthrough to ACP backend lost)", got, acp.obs)
	}
}

func TestProvider_InvalidatesLivenessOnBothBackends(t *testing.T) {
	def := &livenessObserverStub{Fake: runtime.NewFake()}
	acp := &livenessObserverStub{Fake: runtime.NewFake()}
	p := New(def, acp)

	p.InvalidateLiveness("stale-route")

	if got := def.invalidations; len(got) != 1 || got[0] != "stale-route" {
		t.Fatalf("default invalidations = %#v, want exact name once", got)
	}
	if got := acp.invalidations; len(got) != 1 || got[0] != "stale-route" {
		t.Fatalf("ACP invalidations = %#v, want exact name once", got)
	}
}

func TestProvider_ForwardsLivenessObservationErrorFromRoutedBackend(t *testing.T) {
	wantErr := errors.New("snapshot unavailable")
	def := &errorBearingLivenessObserverStub{Fake: runtime.NewFake(), err: wantErr}
	acp := &errorBearingLivenessObserverStub{Fake: runtime.NewFake(), err: wantErr}
	p := New(def, acp)
	p.RouteACP("acpsess")

	for _, name := range []string{"plain", "acpsess"} {
		got, err := p.ObserveLivenessWithError(name, nil)
		if !errors.Is(err, wantErr) {
			t.Errorf("ObserveLivenessWithError(%q) error = %v, want %v", name, err, wantErr)
		}
		if got != (runtime.Liveness{}) {
			t.Errorf("ObserveLivenessWithError(%q) = %+v, want zero while routed result is unknown", name, got)
		}
	}
}

// TestAutoForwardsIsAttachedWithError proves auto forwards the error-bearing
// attachment probe to the routed backend. Without the forward, the error is
// lost behind the bool IsAttached and a probe failure reads "not attached".
func TestAutoForwardsIsAttachedWithError(t *testing.T) {
	def, acp := runtime.NewFake(), runtime.NewFake()
	defErr := fmt.Errorf("default probe: %w", runtime.ErrRuntimeUnavailable)
	acpErr := fmt.Errorf("acp probe: %w", runtime.ErrRuntimeUnavailable)
	def.AttachedErrors["plain"] = defErr
	acp.AttachedErrors["acpsess"] = acpErr
	def.SetAttached("attached", true)
	p := New(def, acp)
	p.RouteACP("acpsess")

	for _, tc := range []struct {
		name    string
		want    bool
		wantErr error
	}{
		{"plain", false, defErr},
		{"acpsess", false, acpErr},
		{"attached", true, nil},
	} {
		got, err := runtime.IsAttachedWithError(p, tc.name)
		if got != tc.want || !errors.Is(err, tc.wantErr) {
			t.Errorf("IsAttachedWithError(%q) = (%v, %v), want (%v, %v)", tc.name, got, err, tc.want, tc.wantErr)
		}
	}
}
