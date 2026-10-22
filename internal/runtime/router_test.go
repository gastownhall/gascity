package runtime_test

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/runtime/hybrid"
)

// capsProvider is a backend with the given capabilities and no optional
// interfaces: embedding the Provider interface promotes only its methods.
type capsProvider struct {
	runtime.Provider
	caps runtime.ProviderCapabilities
}

func (p capsProvider) Capabilities() runtime.ProviderCapabilities { return p.caps }

func newCapsProvider(caps runtime.ProviderCapabilities) *capsProvider {
	return &capsProvider{Provider: runtime.NewFake(), caps: caps}
}

// Kills: CapabilitiesFor answering the composite's intersection, or stopping
// after one hop (auto's route to hybrid) instead of reaching the leaf.
func TestCapabilitiesForUnwrapsNestedRouters(t *testing.T) {
	local := newCapsProvider(runtime.ProviderCapabilities{CanReportAttachment: true, CanReportActivity: true})
	remote := newCapsProvider(runtime.ProviderCapabilities{CanReportActivity: true, CanStream: true})
	acp := newCapsProvider(runtime.ProviderCapabilities{CanReportActivity: true})
	h := hybrid.New(local, remote, func(name string) bool { return name == "remote-1" })
	sp := auto.New(h, acp)
	sp.SeedRoutes([]string{"acp-1"})

	for _, tc := range []struct {
		name       string
		wantLeaf   runtime.Provider
		wantLabels []string
	}{
		{name: "local-1", wantLeaf: local, wantLabels: []string{"default", "local"}},
		{name: "remote-1", wantLeaf: remote, wantLabels: []string{"default", "remote"}},
		{name: "acp-1", wantLeaf: acp, wantLabels: []string{"acp"}},
	} {
		leaf, labels, known := runtime.ResolveBackend(sp, tc.name)
		if leaf != tc.wantLeaf || !reflect.DeepEqual(labels, tc.wantLabels) || !known {
			t.Errorf("ResolveBackend(%q) = (%p, %v, %v), want (%p, %v, true)", tc.name, leaf, labels, known, tc.wantLeaf, tc.wantLabels)
		}
		caps, ok := runtime.CapabilitiesFor(sp, tc.name)
		if want := tc.wantLeaf.Capabilities(); caps != want || !ok {
			t.Errorf("CapabilitiesFor(%q) = (%+v, %v), want (%+v, true)", tc.name, caps, ok, want)
		}
	}
	if sp.Capabilities() == local.caps {
		t.Fatal("fixture is degenerate: the composite intersection equals the local leaf's capabilities")
	}
}

// Kills: Known=true through an unseeded auto, and a hop's Known ignored when an
// inner hop is known (hybrid always is).
func TestResolveBackendUnknownWhenAnyHopUnknown(t *testing.T) {
	local := runtime.NewFake()
	h := hybrid.New(local, runtime.NewFake(), func(string) bool { return false })
	sp := auto.New(h, runtime.NewFake())

	leaf, labels, known := runtime.ResolveBackend(sp, "worker")
	if leaf != local || !reflect.DeepEqual(labels, []string{"default", "local"}) || known {
		t.Fatalf("ResolveBackend(unseeded) = (%p, %v, %v), want (%p, [default local], false)", leaf, labels, known, local)
	}
	if _, ok := runtime.CapabilitiesFor(sp, "worker"); ok {
		t.Fatal("CapabilitiesFor(unseeded) ok = true, want false")
	}

	leaf, labels, known = runtime.ResolveBackend(local, "worker")
	if leaf != local || labels != nil || !known {
		t.Fatalf("ResolveBackend(leaf) = (%p, %v, %v), want the leaf itself, no labels, known", leaf, labels, known)
	}
}

// Kills: a drifted copy of the capability-to-sleep rule (gc's fallback and the
// composites share it).
func TestSleepCapabilityFromCapabilities(t *testing.T) {
	for _, tc := range []struct {
		caps runtime.ProviderCapabilities
		want runtime.SessionSleepCapability
	}{
		{runtime.ProviderCapabilities{CanReportActivity: true, CanReportAttachment: true}, runtime.SessionSleepCapabilityFull},
		{runtime.ProviderCapabilities{CanReportActivity: true}, runtime.SessionSleepCapabilityTimedOnly},
		{runtime.ProviderCapabilities{CanReportAttachment: true}, runtime.SessionSleepCapabilityDisabled},
		{runtime.ProviderCapabilities{}, runtime.SessionSleepCapabilityDisabled},
	} {
		if got := runtime.SleepCapabilityFromCapabilities(tc.caps); got != tc.want {
			t.Errorf("SleepCapabilityFromCapabilities(%+v) = %q, want %q", tc.caps, got, tc.want)
		}
	}
}

// A route is one of the composite's Backends, so the fence's per-session tier
// and the inventory's per-backend listing name a backend the same way.
// Kills: a RouteFor label or provider that drifts from Backends().
func TestRouteForReturnsOneOfBackends(t *testing.T) {
	a := auto.New(runtime.NewFake(), runtime.NewFake())
	a.SeedRoutes([]string{"acp-1"})
	h := hybrid.New(runtime.NewFake(), runtime.NewFake(), func(name string) bool { return name == "remote-1" })

	for _, tc := range []struct {
		sp interface {
			runtime.Router
			runtime.BackendsProvider
		}
		names []string
	}{
		{sp: a, names: []string{"worker", "acp-1"}},
		{sp: h, names: []string{"local-1", "remote-1"}},
	} {
		backends := tc.sp.Backends()
		for i, name := range tc.names {
			if got := tc.sp.RouteFor(name).Backend; got != backends[i] {
				t.Errorf("%T.RouteFor(%q).Backend = %+v, want Backends()[%d] = %+v", tc.sp, name, got, i, backends[i])
			}
		}
	}
}
