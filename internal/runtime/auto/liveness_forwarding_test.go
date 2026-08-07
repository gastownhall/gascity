package auto

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// livenessObserverStub is a Fake that also reports a fixed LivenessObserver
// verdict, so a test can prove auto preserves the routed backend's native
// liveness fast-path rather than collapsing to the generic IsRunning+ProcessAlive
// fold.
type livenessObserverStub struct {
	*runtime.Fake
	obs runtime.Liveness
}

func (s *livenessObserverStub) ObserveLiveness(string, []string) runtime.Liveness { return s.obs }

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
