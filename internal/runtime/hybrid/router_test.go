package hybrid

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Kills: a route that disagrees with the isRemote predicate, a wrong label, or
// Known=false for a route that is a pure function of the name.
func TestHybridRouteForIsDeterministic(t *testing.T) {
	local, remote := runtime.NewFake(), runtime.NewFake()
	p := New(local, remote, isRemote)

	for _, tc := range []struct {
		name  string
		want  runtime.Provider
		label string
	}{
		{name: "local-agent", want: local, label: "local"},
		{name: "remote-agent-1", want: remote, label: "remote"},
	} {
		got := p.RouteFor(tc.name)
		if got.Provider != tc.want || got.Label != tc.label || !got.Known {
			t.Errorf("RouteFor(%q) = %+v, want %s backend, known", tc.name, got, tc.label)
		}
	}
}

// Kills: the Disabled fallback for a routed backend without SleepCapability
// (see the auto twin).
func TestHybridSleepCapabilityFallsBackToRoutedCapabilities(t *testing.T) {
	remote := &struct{ runtime.Provider }{Provider: runtime.NewFake()}
	p := New(runtime.NewFake(), remote, isRemote)

	if got := p.SleepCapability("remote-agent-1"); got != runtime.SessionSleepCapabilityFull {
		t.Fatalf("SleepCapability(remote-agent-1) = %q, want %q from the remote backend's capabilities", got, runtime.SessionSleepCapabilityFull)
	}
}
