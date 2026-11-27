package runtime_test

import (
	"reflect"
	"testing"
	"time"

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

// cachedLeaf answers every liveness read from its cache and has no fresh read
// (herdr, exec).
type cachedLeaf struct {
	runtime.Provider
	cached runtime.Liveness
}

func (l cachedLeaf) ObserveLivenessWithError(string, []string) (runtime.Liveness, error) {
	return l.cached, nil
}

// freshLeaf also reads past its cache (tmux, k8s). since is the boundary its
// last fresh read was asked for.
type freshLeaf struct {
	cachedLeaf
	fresh runtime.Liveness
	since time.Time
}

func (l *freshLeaf) ObserveLivenessSince(_ string, _ []string, since time.Time) (runtime.Liveness, error) {
	l.since = since
	return l.fresh, nil
}

// guessingRouter routes every name to leaf without vouching for the route, as
// auto does before SeedRoutes, and claims no fresh read of its own. Its own
// read is its cached one.
type guessingRouter struct {
	cachedLeaf
	leaf runtime.Provider
}

func (r guessingRouter) RouteFor(string) runtime.Route {
	return runtime.Route{Backend: runtime.Backend{Label: "guess", Provider: r.leaf}}
}

// A composite that claims no fresh read (hybrid) answers from its leaves'
// cached reads. After another process kills a session, that cache still reads
// it alive. So ObserveLivenessSince reads the leaf serving the name, when the
// route is known and that leaf can read fresh. A leaf without the fresh read,
// or a route the composite cannot vouch for, keeps the composite's own read.
// Kills: the fresh read of a hybrid-routed session (the heal confirmer, the
// nudge fence, the ownerless reap) answered from the leaf's cache, or a
// guessed route trusted.
func TestObserveLivenessSinceReadsTheFreshLeafBehindARouter(t *testing.T) {
	since := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	stale := runtime.Liveness{Running: true, Alive: true}
	dead := runtime.Liveness{Running: true}
	cached := cachedLeaf{Provider: runtime.NewFake(), cached: stale}
	isRemote := func(name string) bool { return name == "remote-1" }
	for _, tc := range []struct {
		desc  string
		build func(fresh *freshLeaf) runtime.Provider
		name  string
		want  runtime.Liveness
		asked bool
	}{
		{
			desc:  "hybrid reads the leaf serving the name fresh",
			build: func(fresh *freshLeaf) runtime.Provider { return hybrid.New(cached, fresh, isRemote) },
			name:  "remote-1",
			want:  dead,
			asked: true,
		},
		{
			desc:  "auto's hybrid backend reads its leaf fresh",
			build: func(fresh *freshLeaf) runtime.Provider { return auto.New(hybrid.New(cached, fresh, isRemote), cached) },
			name:  "remote-1",
			want:  dead,
			asked: true,
		},
		{
			desc:  "a leaf without the fresh read keeps the composite's read",
			build: func(fresh *freshLeaf) runtime.Provider { return hybrid.New(cached, fresh, isRemote) },
			name:  "local-1",
			want:  stale,
		},
		{
			desc:  "a guessed route keeps the composite's read",
			build: func(fresh *freshLeaf) runtime.Provider { return guessingRouter{cachedLeaf: cached, leaf: fresh} },
			name:  "worker",
			want:  stale,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			fresh := &freshLeaf{cachedLeaf: cached, fresh: dead}
			got, err := runtime.ObserveLivenessSince(tc.build(fresh), tc.name, nil, since)
			if err != nil || got != tc.want {
				t.Errorf("ObserveLivenessSince(%q) = (%+v, %v), want %+v", tc.name, got, err, tc.want)
			}
			if asked := !fresh.since.IsZero(); asked != tc.asked || asked && !fresh.since.Equal(since) {
				t.Errorf("fresh leaf asked = %v (since %v), want asked = %v (since %v)", asked, fresh.since, tc.asked, since)
			}
		})
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
