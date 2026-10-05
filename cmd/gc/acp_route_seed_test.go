package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
)

// acpSessionBead is an open session bead that the ACP route rule classifies
// by its transport metadata, with no agent config behind it.
func acpSessionBead(sessionName string) beads.Bead {
	return beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "opencode",
			"provider":     "opencode",
			"transport":    "acp",
			"session_name": sessionName,
		},
	}
}

// stubSessionProviderBuilds makes provider construction return built[name],
// and a fresh fake for any name built does not hold.
func stubSessionProviderBuilds(t *testing.T, built map[string]runtime.Provider) {
	t.Helper()
	old := buildSessionProviderByName
	buildSessionProviderByName = func(_ *config.City, name string, _ config.SessionConfig, _, _ string) (runtime.Provider, error) {
		if sp, ok := built[name]; ok {
			return sp, nil
		}
		return runtime.NewFake(), nil
	}
	t.Cleanup(func() { buildSessionProviderByName = old })
}

// A provider swap rebuilds the session provider. Building it from the bare
// registry dropped the auto composition, so every ACP session was then driven
// through the new default backend.
// Kills: a bare provider after the swap, and a swap that rebuilds the old
// provider name.
func TestReloadProviderSwapKeepsAutoComposition(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeACPAgentCityConfig(t, tomlPath, "fake")
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	acp, swapped := runtime.NewFake(), runtime.NewFake()
	stubSessionProviderBuilds(t, map[string]runtime.Provider{"acp": acp, "fail": swapped})

	sp := runtime.NewFake()
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)
	cr.sessionDrains = newDrainTracker()

	writeACPAgentCityConfig(t, tomlPath, "fail")
	lastProviderName := "fake"
	cr.reloadConfig(context.Background(), &lastProviderName, cityPath)

	if lastProviderName != "fail" {
		t.Fatalf("lastProviderName = %q, want fail (the swap did not happen)", lastProviderName)
	}
	autoSP, ok := cr.sp.(*sessionauto.Provider)
	if !ok {
		t.Fatalf("session provider after the swap = %T, want the auto composition", cr.sp)
	}
	acpSession := agent.SessionNameFor("test-city", "reviewer", "")
	if err := autoSP.Attach(acpSession); err == nil || !strings.Contains(err.Error(), "ACP transport") {
		t.Fatalf("Attach(%q) after the swap = %v, want the ACP transport refusal (routed to acp)", acpSession, err)
	}
	if got := autoSP.RouteFor("worker").Provider; got != swapped {
		t.Fatalf("default route after the swap = %p, want the provider built for %q (%p)", got, "fail", swapped)
	}
}

func writeACPAgentCityConfig(t *testing.T, tomlPath, provider string) {
	t.Helper()
	clearInheritedBeadsEnv(t)
	requireNoLeakedDoltAfterForPaths(t, filepath.Dir(tomlPath))
	data := []byte("[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"" + provider + "\"\n\n[[agent]]\nname = \"reviewer\"\nsession = \"acp\"\n")
	if err := os.WriteFile(tomlPath, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// Construction registers the ACP routes a restart would otherwise lose, and
// only a loaded session snapshot lets the table vouch for its default routes.
// Kills: plain RouteACP on a loaded snapshot (the fence could never trust a
// default route), and a seeded table without one.
func TestResolveSessionTransportProviderSeedsOnlyFromLoadedSnapshot(t *testing.T) {
	acp := runtime.NewFake()
	stubSessionProviderBuilds(t, map[string]runtime.Provider{"acp": acp})
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{Name: "reviewer", Session: "acp"}}}
	ctx := sessionProviderContextForCity(cfg, t.TempDir(), "fake")
	configured := agent.SessionNameFor("test-city", "reviewer", "")

	for _, tc := range []struct {
		name      string
		snapshot  *sessionBeadSnapshot
		wantKnown bool
	}{
		{name: "loaded", snapshot: newSessionBeadSnapshot([]beads.Bead{acpSessionBead("dynamic-acp")}), wantKnown: true},
		{name: "not loaded", snapshot: nil},
		{name: "load error", snapshot: newSessionBeadSnapshotWithError(io.ErrUnexpectedEOF)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp, err := resolveSessionTransportProvider(ctx, tc.snapshot)
			if err != nil {
				t.Fatalf("resolveSessionTransportProvider: %v", err)
			}
			router, ok := sp.(runtime.Router)
			if !ok {
				t.Fatalf("provider = %T, want an auto composite", sp)
			}
			if got := router.RouteFor(configured); got.Provider != acp || !got.Known {
				t.Errorf("RouteFor(%q) = %+v, want the acp backend, known", configured, got)
			}
			if got := router.RouteFor("worker"); got.Label != "default" || got.Known != tc.wantKnown {
				t.Errorf("RouteFor(worker) = %+v, want default with Known=%v", got, tc.wantKnown)
			}
		})
	}
}

// countingSeeder records SeedRoutes calls on a real auto provider.
type countingSeeder struct {
	*sessionauto.Provider
	seeds int
}

func (s *countingSeeder) SeedRoutes(names []string) {
	s.seeds++
	s.Provider.SeedRoutes(names)
}

// After a restart whose construction could not read the session beads, the
// controller's own snapshot loads rebuild the ACP routes from the beads, and a
// reload of an unchanged snapshot does not walk the config again.
// Kills: seeding only through the POOL-064 side effects (a session with an
// open bead but no desired-state entry stays on the default backend), and a
// reseed on every load.
func TestControllerSeedsACPRoutesFromSessionSnapshot(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(acpSessionBead("acp-sess")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	def, acp := runtime.NewFake(), runtime.NewFake()
	sp := &countingSeeder{Provider: sessionauto.New(def, acp)}
	cr := &CityRuntime{
		cityName:            "test-city",
		cityPath:            t.TempDir(),
		cfg:                 &config.City{Workspace: config.Workspace{Name: "test-city"}},
		sp:                  sp,
		standaloneCityStore: store,
		stderr:              io.Discard,
	}

	if got := sp.RouteFor("acp-sess"); got.Provider != def || got.Known {
		t.Fatalf("RouteFor(acp-sess) before any snapshot = %+v, want the unseeded default", got)
	}
	cr.loadSessionBeadSnapshot()
	if got := sp.RouteFor("acp-sess"); got.Provider != acp || !got.Known {
		t.Fatalf("RouteFor(acp-sess) after the snapshot = %+v, want the acp backend, known", got)
	}
	if got := sp.RouteFor("worker"); got.Provider != def || !got.Known {
		t.Fatalf("RouteFor(worker) after the snapshot = %+v, want the default backend, known", got)
	}

	cr.loadSessionBeadSnapshot()
	if sp.seeds != 1 {
		t.Fatalf("SeedRoutes calls after an unchanged snapshot = %d, want 1", sp.seeds)
	}
	if _, err := store.Create(acpSessionBead("acp-sess-2")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cr.loadTickSessionBeadSnapshot("poke")
	if got := sp.RouteFor("acp-sess-2"); sp.seeds != 2 || got.Provider != acp {
		t.Fatalf("after a changed snapshot: SeedRoutes calls = %d, RouteFor(acp-sess-2) = %+v; want 2 and the acp backend", sp.seeds, got)
	}
	cr.publishRuntimeConfig(&config.City{Workspace: config.Workspace{Name: "test-city"}}, sp, cr.dops, "rev-2")
	cr.loadSessionBeadSnapshot()
	if sp.seeds != 3 {
		t.Fatalf("SeedRoutes calls after a config publication = %d, want 3", sp.seeds)
	}
}

// sleepCapsProvider is a backend with fixed capabilities and a TimedOnly idle
// sleep capability, the shape of subprocess and acp.
type sleepCapsProvider struct {
	runtime.Provider
	caps runtime.ProviderCapabilities
}

func (p *sleepCapsProvider) Capabilities() runtime.ProviderCapabilities { return p.caps }

func (p *sleepCapsProvider) SleepCapability(string) runtime.SessionSleepCapability {
	return runtime.SessionSleepCapabilityTimedOnly
}

// In an auto(subprocess, acp) city the intersection has no activity report,
// so every ACP session read as activity-unknown although acp reports it.
// Kills: the intersection.
func TestSessionActivityReportableUsesRoutedCapabilities(t *testing.T) {
	subprocess := &sleepCapsProvider{Provider: runtime.NewFake()}
	acp := &sleepCapsProvider{Provider: runtime.NewFake(), caps: runtime.ProviderCapabilities{CanReportActivity: true}}
	sp := sessionauto.New(subprocess, acp)
	sp.SeedRoutes([]string{"acp-sess"})

	if !sessionActivityReportable(sp, "acp-sess") {
		t.Error("sessionActivityReportable(acp-sess) = false, want true: its acp backend reports activity")
	}
	if sessionActivityReportable(sp, "worker") {
		t.Error("sessionActivityReportable(worker) = true, want false: its subprocess backend reports no activity")
	}
}
