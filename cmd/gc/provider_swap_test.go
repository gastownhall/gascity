package main

import (
	"context"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
)

// Kills: a base carried across a changed selection name, pack runtime
// declaration, tmux socket or hybrid remote match (its runtimes would be
// served by a backend the new config does not describe); a base rebuilt when
// none of those changed (J38: every runtime stopped); an ACP leg carried
// across a changed [session.acp] config, or dropped when it is unchanged.
func TestCarriedSessionLegs(t *testing.T) {
	base, acp := runtime.NewFake(), runtime.NewFake()
	composed := sessionLegs{base: base, acp: acp}
	city := func(mut func(*config.City)) *config.City {
		c := &config.City{Session: config.SessionConfig{Socket: "s"}, Runtimes: map[string]config.DiscoveredRuntime{"box": {Command: "box-a"}}}
		if mut != nil {
			mut(c)
		}
		return c
	}
	for _, tc := range []struct {
		name             string
		old              sessionLegs
		oldName, newName string
		newCfg           *config.City
		want             sessionLegs
	}{
		{name: "ACP-only change carries both legs", old: composed, oldName: "tmux", newName: "tmux", newCfg: city(nil), want: composed},
		{name: "bare base carries its base", old: sessionLegs{base: base}, oldName: "tmux", newName: "tmux", newCfg: city(nil), want: sessionLegs{base: base}},
		{name: "selection name change", old: composed, oldName: "tmux", newName: "subprocess", newCfg: city(nil), want: sessionLegs{acp: acp}},
		{name: "pack runtime declaration change", old: composed, oldName: "box", newName: "box", newCfg: city(func(c *config.City) { c.Runtimes["box"] = config.DiscoveredRuntime{Command: "box-b"} }), want: sessionLegs{acp: acp}},
		{name: "unrelated pack runtime change", old: composed, oldName: "tmux", newName: "tmux", newCfg: city(func(c *config.City) { c.Runtimes["box"] = config.DiscoveredRuntime{Command: "box-b"} }), want: composed},
		{name: "socket change", old: composed, oldName: "tmux", newName: "tmux", newCfg: city(func(c *config.City) { c.Session.Socket = "t" }), want: sessionLegs{acp: acp}},
		{name: "remote match change", old: composed, oldName: "hybrid", newName: "hybrid", newCfg: city(func(c *config.City) { c.Session.RemoteMatch = "k8s" }), want: sessionLegs{acp: acp}},
		{name: "non-endpoint session setting", old: composed, oldName: "tmux", newName: "tmux", newCfg: city(func(c *config.City) { c.Session.SetupTimeout = "30s" }), want: composed},
		{name: "ACP config change", old: composed, oldName: "tmux", newName: "tmux", newCfg: city(func(c *config.City) { c.Session.ACP.StopGrace = "9s" }), want: sessionLegs{base: base}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := carriedSessionLegs(tc.old, city(nil), tc.newCfg, tc.oldName, tc.newName); got != tc.want {
				t.Fatalf("carried = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Kills: a swap that stops a runtime its carried leg still serves (J38), and
// one that keeps a runtime the new provider routes elsewhere, which nothing
// could stop afterwards.
func TestProviderSwapStops(t *testing.T) {
	base, base2, acp, acp2 := runtime.NewFake(), runtime.NewFake(), runtime.NewFake(), runtime.NewFake()
	composite := func(b, a runtime.Provider, acpNames ...string) runtime.Provider {
		sp := sessionauto.New(b, a)
		sp.SeedRoutes(acpNames)
		return sp
	}
	listing := func(sp runtime.Provider, names ...string) runtime.BackendListing {
		return runtime.BackendListing{Provider: sp, Names: names}
	}
	for _, tc := range []struct {
		name     string
		listings []runtime.BackendListing
		newSP    runtime.Provider
		want     []swapStop
	}{
		{
			name:     "ACP leg added over the same base",
			listings: []runtime.BackendListing{listing(base, "w1", "w2")},
			newSP:    composite(base, acp2, "r"),
		},
		{
			name:     "ACP leg added routes a running base name to ACP",
			listings: []runtime.BackendListing{listing(base, "w1", "r")},
			newSP:    composite(base, acp2, "r"),
			want:     []swapStop{{name: "r", backend: base}},
		},
		{
			name:     "ACP leg removed",
			listings: []runtime.BackendListing{listing(base, "w1"), listing(acp, "r")},
			newSP:    base,
			want:     []swapStop{{name: "r", backend: acp}},
		},
		{
			name:     "ACP leg kept, composition recomputed",
			listings: []runtime.BackendListing{listing(base, "w1"), listing(acp, "r")},
			newSP:    composite(base, acp, "r"),
		},
		{
			name:     "base replaced, ACP leg carried",
			listings: []runtime.BackendListing{listing(base, "w1", "w2"), listing(acp, "r")},
			newSP:    composite(base2, acp, "r"),
			want:     []swapStop{{name: "w1", backend: base}, {name: "w2", backend: base}},
		},
		{
			name:     "both replaced",
			listings: []runtime.BackendListing{listing(base, "w1"), listing(acp, "r")},
			newSP:    composite(base2, acp2, "r"),
			want:     []swapStop{{name: "w1", backend: base}, {name: "r", backend: acp}},
		},
		{
			name:     "bare base replaced",
			listings: []runtime.BackendListing{listing(base, "w1")},
			newSP:    base2,
			want:     []swapStop{{name: "w1", backend: base}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := providerSwapStops(tc.listings, tc.newSP); !slices.Equal(got, tc.want) {
				t.Fatalf("stops = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// swapReloadFixture is a city runtime on oldSP whose session store holds an
// active pool row for each running base name and an active row for each
// running ACP name.
type swapReloadFixture struct {
	cr       *CityRuntime
	tomlPath string
	store    *beads.MemStore
	rows     map[string]map[string]string // bead ID -> metadata before the reload
}

func newSwapReloadFixture(t *testing.T, tomlPath string, oldSP runtime.Provider, poolNames, acpNames []string) *swapReloadFixture {
	t.Helper()
	cityPath := filepath.Dir(tomlPath)
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       oldSP,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(oldSP),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	store := beads.NewMemStore()
	cs := newControllerState(context.Background(), cfg, oldSP, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = store
	cr.setControllerState(cs)
	cr.sessionDrains = newDrainTracker()
	f := &swapReloadFixture{cr: cr, tomlPath: tomlPath, store: store, rows: map[string]map[string]string{}}
	row := func(name string, extra map[string]string) {
		md := map[string]string{"session_name": name, "template": "worker", "state": "active"}
		maps.Copy(md, extra)
		b, err := store.Create(beads.Bead{Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: md})
		if err != nil {
			t.Fatalf("create row %s: %v", name, err)
		}
		f.rows[b.ID] = maps.Clone(b.Metadata)
	}
	for _, n := range poolNames {
		row(n, map[string]string{"pool_slot": "1"})
	}
	for _, n := range acpNames {
		row(n, nil)
	}
	return f
}

func (f *swapReloadFixture) reload(t *testing.T, lastProviderName string) {
	t.Helper()
	reply := f.cr.reloadConfigTraced(context.Background(), &lastProviderName, filepath.Dir(f.tomlPath), nil, reloadSourceManual)
	if reply.Outcome == reloadOutcomeFailed {
		t.Fatalf("reload failed: %+v", reply)
	}
}

// requireNoRowWrites fails when the swap wrote any session row: no suspend,
// sleep, city-stop or close (CONTRACT v5.7 P7, F2).
func (f *swapReloadFixture) requireNoRowWrites(t *testing.T) {
	t.Helper()
	for id, before := range f.rows {
		after, err := f.store.Get(id)
		if err != nil {
			t.Fatalf("get row %s: %v", id, err)
		}
		if after.Status == "closed" || !maps.Equal(after.Metadata, before) {
			t.Fatalf("provider swap wrote row %s (status %q):\nbefore %v\nafter  %v", id, after.Status, before, after.Metadata)
		}
	}
}

func startFakeSessions(t *testing.T, sp runtime.Provider, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := sp.Start(context.Background(), n, runtime.Config{}); err != nil {
			t.Fatalf("start %s: %v", n, err)
		}
	}
}

// J38: a reload that adds the ACP leg over an unchanged base stopped every
// running session and parked the pool rows suspended.
// Kills: gracefulStopAll over every listed runtime, the SuspendForShutdown
// row write, and a rebuilt base.
func TestReloadACPLegAddedKeepsBaseSessionsAndWritesNoRow(t *testing.T) {
	tomlPath := filepath.Join(t.TempDir(), "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	base, acp := runtime.NewFake(), runtime.NewFake()
	stubSessionProviderBuilds(t, map[string]runtime.Provider{"acp": acp})
	startFakeSessions(t, base, "worker-1", "worker-2")
	f := newSwapReloadFixture(t, tomlPath, base, []string{"worker-1", "worker-2"}, nil)

	writeACPAgentCityConfig(t, tomlPath, "fake")
	f.reload(t, "fake")

	autoSP, ok := f.cr.sp.(*sessionauto.Provider)
	if !ok {
		t.Fatalf("session provider after the reload = %T, want the auto composition", f.cr.sp)
	}
	if got := autoSP.RouteFor("worker-1").Provider; got != base {
		t.Fatalf("default route = %p, want the carried base %p", got, base)
	}
	for _, n := range []string{"worker-1", "worker-2"} {
		if !base.IsRunning(n) || base.CountCalls("Stop", n) != 0 || base.CountCalls("Interrupt", n) != 0 {
			t.Fatalf("%s: running=%v, Stop=%d, Interrupt=%d; want it untouched", n, base.IsRunning(n), base.CountCalls("Stop", n), base.CountCalls("Interrupt", n))
		}
	}
	f.requireNoRowWrites(t)
}

// Removing the ACP leg stops the ACP-routed runtimes, by provider Stop on the
// ACP leg, and nothing on the unchanged base.
// Kills: a removal that stops base runtimes, keeps ACP runtimes the new
// provider cannot reach, or writes a row.
func TestReloadACPLegRemovedStopsOnlyACPSessions(t *testing.T) {
	tomlPath := filepath.Join(t.TempDir(), "city.toml")
	writeACPAgentCityConfig(t, tomlPath, "fake")
	base, acp := runtime.NewFake(), runtime.NewFake()
	reviewer := agent.SessionNameFor("test-city", "reviewer", "")
	old := sessionauto.New(base, acp)
	old.RouteACP(reviewer)
	startFakeSessions(t, old, "worker-1", reviewer)
	f := newSwapReloadFixture(t, tomlPath, old, []string{"worker-1"}, []string{reviewer})

	writeCityRuntimeConfig(t, tomlPath, "fake")
	f.reload(t, "fake")

	if f.cr.sp != base {
		t.Fatalf("session provider after the reload = %T %p, want the carried bare base %p", f.cr.sp, f.cr.sp, base)
	}
	if !base.IsRunning("worker-1") || base.CountCalls("Stop", "worker-1") != 0 {
		t.Fatalf("worker-1: running=%v, Stop=%d; want it untouched", base.IsRunning("worker-1"), base.CountCalls("Stop", "worker-1"))
	}
	if acp.IsRunning(reviewer) || acp.CountCalls("Stop", reviewer) != 1 {
		t.Fatalf("%s: running=%v, Stop=%d; want one Stop on the ACP leg", reviewer, acp.IsRunning(reviewer), acp.CountCalls("Stop", reviewer))
	}
	f.requireNoRowWrites(t)
}

// Replacing the base stops the base-routed runtimes and keeps the ACP-routed
// ones, whose leg carries into the new composition.
// Kills: a base replacement that stops ACP runtimes, rebuilds the ACP leg, or
// writes a row.
func TestReloadBaseReplacedStopsOnlyBaseSessions(t *testing.T) {
	tomlPath := filepath.Join(t.TempDir(), "city.toml")
	writeACPAgentCityConfig(t, tomlPath, "fake")
	base, acp, newBase := runtime.NewFake(), runtime.NewFake(), runtime.NewFake()
	stubSessionProviderBuilds(t, map[string]runtime.Provider{"fail": newBase})
	reviewer := agent.SessionNameFor("test-city", "reviewer", "")
	old := sessionauto.New(base, acp)
	old.RouteACP(reviewer)
	startFakeSessions(t, old, "worker-1", "worker-2", reviewer)
	f := newSwapReloadFixture(t, tomlPath, old, []string{"worker-1", "worker-2"}, []string{reviewer})

	writeACPAgentCityConfig(t, tomlPath, "fail")
	f.reload(t, "fake")

	autoSP, ok := f.cr.sp.(*sessionauto.Provider)
	if !ok {
		t.Fatalf("session provider after the reload = %T, want the auto composition", f.cr.sp)
	}
	if got := autoSP.RouteFor("worker-1").Provider; got != newBase {
		t.Fatalf("default route = %p, want the new base %p", got, newBase)
	}
	if got := autoSP.RouteFor(reviewer).Provider; got != acp {
		t.Fatalf("ACP route = %p, want the carried ACP leg %p", got, acp)
	}
	for _, n := range []string{"worker-1", "worker-2"} {
		if base.IsRunning(n) || base.CountCalls("Stop", n) != 1 {
			t.Fatalf("%s: running=%v, Stop=%d; want one Stop on the old base", n, base.IsRunning(n), base.CountCalls("Stop", n))
		}
	}
	if !acp.IsRunning(reviewer) || acp.CountCalls("Stop", reviewer) != 0 {
		t.Fatalf("%s: running=%v, Stop=%d; want it untouched", reviewer, acp.IsRunning(reviewer), acp.CountCalls("Stop", reviewer))
	}
	f.requireNoRowWrites(t)
}
