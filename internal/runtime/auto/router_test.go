package auto

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Kills: an explicit ACP route reported unknown, or routed to the default
// backend.
func TestAutoRouteForExplicitACPIsKnown(t *testing.T) {
	def, acp := runtime.NewFake(), runtime.NewFake()
	p := New(def, acp)
	p.RouteACP("acp-1")

	got := p.RouteFor("acp-1")
	if got.Provider != acp || got.Label != "acp" || !got.Known {
		t.Fatalf("RouteFor(acp-1) = %+v, want the acp backend, known", got)
	}
}

// Kills: a default route that claims to be known before the table was seeded
// from the session beads (after a restart the route may be a lost ACP route).
func TestAutoRouteForDefaultUnknownUntilSeeded(t *testing.T) {
	def, acp := runtime.NewFake(), runtime.NewFake()
	p := New(def, acp)
	p.RouteACP("acp-1")

	got := p.RouteFor("worker")
	if got.Provider != def || got.Label != "default" || got.Known {
		t.Fatalf("RouteFor(worker) before SeedRoutes = %+v, want the default backend, unknown", got)
	}
}

// Kills: SeedRoutes that does not mark the table seeded, does not add its
// routes, or drops routes registered before it (a dynamic RouteACP whose bead
// the snapshot has not seen yet).
func TestAutoSeedRoutesMarksDefaultKnown(t *testing.T) {
	def, acp := runtime.NewFake(), runtime.NewFake()
	p := New(def, acp)
	p.RouteACP("started-acp")
	p.SeedRoutes([]string{"seeded-acp"})

	for _, tc := range []struct {
		name  string
		want  runtime.Provider
		label string
	}{
		{name: "worker", want: def, label: "default"},
		{name: "seeded-acp", want: acp, label: "acp"},
		{name: "started-acp", want: acp, label: "acp"},
	} {
		got := p.RouteFor(tc.name)
		if got.Provider != tc.want || got.Label != tc.label || !got.Known {
			t.Errorf("RouteFor(%q) = %+v, want %s backend, known", tc.name, got, tc.label)
		}
	}
}

// sleeplessProvider has capabilities but no SleepCapability method, like
// herdr today.
type sleeplessProvider struct {
	runtime.Provider
	caps runtime.ProviderCapabilities
}

func (p sleeplessProvider) Capabilities() runtime.ProviderCapabilities { return p.caps }

// Kills: the Disabled fallback, which turned idle sleep off for a routed
// backend without SleepCapability even though a bare city on the same backend
// gets TimedOnly from its capabilities.
func TestAutoSleepCapabilityFallsBackToRoutedCapabilities(t *testing.T) {
	def := &sleeplessProvider{Provider: runtime.NewFake(), caps: runtime.ProviderCapabilities{CanReportActivity: true}}
	acp := &sleeplessProvider{Provider: runtime.NewFake()}
	p := New(def, acp)
	p.SeedRoutes([]string{"acp-1"})

	if got := p.SleepCapability("worker"); got != runtime.SessionSleepCapabilityTimedOnly {
		t.Errorf("SleepCapability(worker) = %q, want %q from the default backend's capabilities", got, runtime.SessionSleepCapabilityTimedOnly)
	}
	if got := p.SleepCapability("acp-1"); got != runtime.SessionSleepCapabilityDisabled {
		t.Errorf("SleepCapability(acp-1) = %q, want %q from the acp backend's capabilities", got, runtime.SessionSleepCapabilityDisabled)
	}
}
