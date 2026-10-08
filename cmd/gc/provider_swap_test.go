package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
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

// Kills: a swap that stops a runtime its carried leg still serves (J38), one
// that keeps a runtime the new provider routes elsewhere, which nothing could
// stop afterwards, and one that guesses a route the new provider does not know.
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
		wantErr  bool
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
		{
			name:     "a name listed on both legs is stopped on each",
			listings: []runtime.BackendListing{listing(base, "x"), listing(acp, "x")},
			newSP:    composite(base2, acp2),
			want:     []swapStop{{name: "x", backend: base}, {name: "x", backend: acp}},
		},
		{
			name:     "unknown default route on an unseeded table",
			listings: []runtime.BackendListing{listing(base, "w1"), listing(acp, "r")},
			newSP:    sessionauto.New(base2, acp),
			wantErr:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := providerSwapStops(tc.listings, tc.newSP)
			if (err != nil) != tc.wantErr || !slices.Equal(got, tc.want) {
				t.Fatalf("stops = %+v, err = %v; want %+v, error %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

// swapListStore is a MemStore whose list reads fail while failList is set.
type swapListStore struct {
	*beads.MemStore
	failList bool
}

var errSwapListStore = errors.New("store list unavailable")

func (s *swapListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.failList {
		return nil, errSwapListStore
	}
	return s.MemStore.List(q)
}

func (s *swapListStore) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	if s.failList {
		return nil, errSwapListStore
	}
	return s.MemStore.ListByLabel(label, limit, opts...)
}

// swapStopFailProvider is a fake whose Stop fails while fail is set.
type swapStopFailProvider struct {
	*runtime.Fake
	fail bool
}

func (p *swapStopFailProvider) Stop(name string) error {
	if !p.fail {
		return p.Fake.Stop(name)
	}
	return errors.New("stop refused")
}

// swapReloadFixture is a city runtime on oldSP whose session store holds an
// active pool row for each running base name and an active row for each
// running ACP name.
type swapReloadFixture struct {
	cr       *CityRuntime
	oldSP    runtime.Provider
	tomlPath string
	store    *swapListStore
	rec      *events.Fake
	stdout   bytes.Buffer
	rows     map[string]map[string]string // bead ID -> metadata before the reload
}

func newSwapReloadFixture(t *testing.T, tomlPath string, oldSP runtime.Provider, poolNames, acpNames []string) *swapReloadFixture {
	t.Helper()
	cityPath := filepath.Dir(tomlPath)
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	f := &swapReloadFixture{oldSP: oldSP, tomlPath: tomlPath, rec: events.NewFake(), rows: map[string]map[string]string{}}
	f.cr = newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       oldSP,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(oldSP),
		Rec:    f.rec,
		Stdout: &f.stdout,
		Stderr: io.Discard,
	})
	f.store = &swapListStore{MemStore: beads.NewMemStore()}
	cs := newControllerState(context.Background(), cfg, oldSP, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = f.store
	f.cr.setControllerState(cs)
	f.cr.sessionDrains = newDrainTracker()
	row := func(name string, extra map[string]string) {
		md := map[string]string{"session_name": name, "template": "worker", "state": "active"}
		maps.Copy(md, extra)
		b, err := f.store.Create(beads.Bead{Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: md})
		if err != nil {
			t.Fatalf("create row %s: %v", name, err)
		}
		f.rows[b.ID] = maps.Clone(b.Metadata)
	}
	for i, n := range poolNames {
		row(n, map[string]string{"pool_slot": strconv.Itoa(i + 1)})
	}
	// Runs before the runtime's shutdown cleanup, whose graceful stop would
	// otherwise wait out its timeout over the fakes' live sessions.
	t.Cleanup(func() {
		for _, sp := range []runtime.Provider{oldSP, f.cr.sp} {
			names, _ := sp.ListRunning("")
			for _, n := range names {
				_ = sp.Stop(n)
			}
		}
	})
	for _, n := range acpNames {
		row(n, nil)
	}
	return f
}

// reload applies the rewritten config to a runtime that runs the "fake"
// selection name.
func (f *swapReloadFixture) reload(t *testing.T) reloadControlReply {
	t.Helper()
	lastProviderName := "fake"
	return f.cr.reloadConfigTraced(context.Background(), &lastProviderName, filepath.Dir(f.tomlPath), nil, reloadSourceManual)
}

func (f *swapReloadFixture) mustReload(t *testing.T) {
	t.Helper()
	if reply := f.reload(t); reply.Outcome == reloadOutcomeFailed {
		t.Fatalf("reload failed: %+v", reply)
	}
}

// requireAborted fails unless the reload failed with errSub, kept the old
// provider, and stopped nothing.
func (f *swapReloadFixture) requireAborted(t *testing.T, reply reloadControlReply, errSub string, legs ...*runtime.Fake) {
	t.Helper()
	if reply.Outcome != reloadOutcomeFailed || !strings.Contains(reply.Error, errSub) {
		t.Fatalf("reload reply = %+v, want a failure containing %q", reply, errSub)
	}
	if f.cr.sp != f.oldSP || strings.Contains(f.stdout.String(), "Session provider swapped") {
		t.Fatalf("aborted swap published the new provider (%T)", f.cr.sp)
	}
	for _, leg := range legs {
		for _, c := range leg.SnapshotCalls() {
			if c.Method == "Stop" || c.Method == "Interrupt" {
				t.Fatalf("aborted swap called %s(%s)", c.Method, c.Name)
			}
		}
	}
	f.requireNoRowWrites(t)
}

// requireNoRowWrites fails when the swap wrote any session row: no suspend,
// sleep, city-stop or close (CONTRACT P7, F2).
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

// stoppedEvents returns the session.stopped events, keyed by session ID.
func (f *swapReloadFixture) stoppedEvents() map[string]events.Event {
	got := map[string]events.Event{}
	for _, e := range f.rec.Events {
		if e.Type == events.SessionStopped {
			got[e.SessionID] = e
		}
	}
	return got
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
	f.mustReload(t)

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
	if got := f.stoppedEvents(); len(got) != 0 {
		t.Fatalf("session.stopped events = %v, want none", got)
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
	f.mustReload(t)

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
// ones, whose leg carries into the new composition. Each stop records legacy's
// session.stopped event.
// Kills: a base replacement that stops ACP runtimes, rebuilds the ACP leg,
// writes a row, or records no (or a differently shaped) stop event.
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
	ids := map[string]string{}
	for id, md := range f.rows {
		ids[md["session_name"]] = id
	}

	writeACPAgentCityConfig(t, tomlPath, "fail")
	f.mustReload(t)

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
	got := f.stoppedEvents()
	if len(got) != 2 {
		t.Fatalf("session.stopped events = %v, want one per stopped worker", got)
	}
	for _, n := range []string{"worker-1", "worker-2"} {
		e, ok := got[ids[n]]
		if !ok {
			t.Fatalf("no session.stopped event for %s (%s): %v", n, ids[n], got)
		}
		var payload map[string]any
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("payload %s: %v", e.Payload, err)
		}
		want := map[string]any{"session_id": ids[n], "template": "worker", "reason": "stopped"}
		if e.Actor != "gc" || e.Subject != n || !maps.Equal(payload, want) {
			t.Fatalf("event for %s = %+v payload %v, want actor gc, session %s, payload %v", n, e, payload, ids[n], want)
		}
	}
	f.requireNoRowWrites(t)
}

// A session snapshot that does not load after the wait leaves the new
// provider's routes unknown, so the swap aborts as a listing failure does.
// Kills: stopping on routes seeded from a stale or missing snapshot (here the
// base is replaced and the ACP leg carried, so a guessed default route would
// stop the ACP runtime too, or keep it on a provider that cannot reach it).
func TestReloadProviderSwapAbortsWhenSessionSnapshotFails(t *testing.T) {
	tomlPath := filepath.Join(t.TempDir(), "city.toml")
	writeACPAgentCityConfig(t, tomlPath, "fake")
	base, acp, newBase := runtime.NewFake(), runtime.NewFake(), runtime.NewFake()
	stubSessionProviderBuilds(t, map[string]runtime.Provider{"fail": newBase})
	reviewer := agent.SessionNameFor("test-city", "reviewer", "")
	old := sessionauto.New(base, acp)
	old.RouteACP(reviewer)
	startFakeSessions(t, old, "worker-1", reviewer)
	f := newSwapReloadFixture(t, tomlPath, old, []string{"worker-1"}, []string{reviewer})

	writeACPAgentCityConfig(t, tomlPath, "fail")
	f.store.failList = true
	reply := f.reload(t)
	f.store.failList = false

	f.requireAborted(t, reply, "session beads unreadable during provider swap", base, acp)
}

// Any leg's listing error aborts the swap before a stop.
// Kills: a listing that checks only the merged or the default leg's error.
func TestReloadProviderSwapAbortsOnACPLegListingError(t *testing.T) {
	tomlPath := filepath.Join(t.TempDir(), "city.toml")
	writeACPAgentCityConfig(t, tomlPath, "fake")
	base, newBase := runtime.NewFake(), runtime.NewFake()
	acp := &listOnlyProvider{Fake: runtime.NewFake(), err: errors.New("acp leg unavailable")}
	stubSessionProviderBuilds(t, map[string]runtime.Provider{"fail": newBase})
	startFakeSessions(t, base, "worker-1")
	f := newSwapReloadFixture(t, tomlPath, sessionauto.New(base, acp), []string{"worker-1"}, nil)

	writeACPAgentCityConfig(t, tomlPath, "fail")
	f.requireAborted(t, f.reload(t), "acp leg unavailable", base, acp.Fake)
	if !base.IsRunning("worker-1") {
		t.Fatal("worker-1 stopped by an aborted swap")
	}
}

// A failed stop aborts the reload before the new provider is published, so
// the old one still reaches the runtime it could not stop.
// Kills: publishing over a failed stop (the runtime is then unreachable) and
// a session.stopped event for a stop that failed.
func TestReloadProviderSwapAbortsOnFailedStop(t *testing.T) {
	tomlPath := filepath.Join(t.TempDir(), "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	base := &swapStopFailProvider{Fake: runtime.NewFake(), fail: true}
	stubSessionProviderBuilds(t, map[string]runtime.Provider{"fail": runtime.NewFake()})
	startFakeSessions(t, base, "worker-1")
	f := newSwapReloadFixture(t, tomlPath, base, []string{"worker-1"}, nil)

	writeCityRuntimeConfig(t, tomlPath, "fail")
	reply := f.reload(t)

	if reply.Outcome != reloadOutcomeFailed || !strings.Contains(reply.Error, "stopping worker-1 failed") {
		t.Fatalf("reload reply = %+v, want the failed stop to abort it", reply)
	}
	if f.cr.sp != f.oldSP || !base.IsRunning("worker-1") {
		t.Fatalf("provider = %T, worker-1 running = %v; want the old provider kept over the live runtime", f.cr.sp, base.IsRunning("worker-1"))
	}
	if got := f.stoppedEvents(); len(got) != 0 {
		t.Fatalf("session.stopped events = %v, want none for a failed stop", got)
	}
	f.requireNoRowWrites(t)
	base.fail = false // let the fixture's cleanup stop it
}

// Legacy waits for its launched async start goroutines before the swap
// lists, so the listing cannot miss a runtime a start is still creating. A
// start still running past startup_timeout + slack, or a ctx done during the
// wait (a controller stop), aborts the swap. A reservation that never
// launched a goroutine (a panic before the launch) is not waited for.
// Kills: a legacy swap that waits on nothing, one that ignores a timed-out
// wait or a done ctx, and a count of reservations instead of goroutines.
func TestBeforeProviderSwapWaitsLegacyAsyncStarts(t *testing.T) {
	cfg := &config.City{Session: config.SessionConfig{StartupTimeout: "1s"}}
	t.Run("leaked reservation", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			cr := &CityRuntime{}
			if _, ok := cr.asyncStarts.start(); !ok {
				t.Fatal("start refused")
			}
			if _, err := cr.beforeProviderSwap(context.Background(), cfg); err != nil {
				t.Fatalf("swap waited on a reservation that launched nothing: %v", err)
			}
		})
	})
	t.Run("start outlives the wait", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			cr := &CityRuntime{}
			release := make(chan struct{})
			cr.asyncStarts.goStart(func() { <-release })
			if _, err := cr.beforeProviderSwap(context.Background(), cfg); err == nil {
				t.Fatal("swap went ahead over a start still running past startup_timeout + slack")
			}
			close(release)
		})
	})
	t.Run("start finishes within the wait", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			cr := &CityRuntime{}
			cr.asyncStarts.goStart(func() { <-time.After(5 * time.Second) })
			if _, err := cr.beforeProviderSwap(context.Background(), cfg); err != nil {
				t.Fatalf("swap aborted over a start that finished within the wait: %v", err)
			}
		})
	})
	t.Run("panicking start", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			cr := &CityRuntime{}
			cr.asyncStarts.goStart(func() {
				defer func() { _ = recover() }()
				panic("start blew up")
			})
			synctest.Wait()
			if _, err := cr.beforeProviderSwap(context.Background(), cfg); err != nil {
				t.Fatalf("a panicked start left the count raised: %v", err)
			}
		})
	})
	t.Run("ctx done mid-wait", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			cr := &CityRuntime{}
			release := make(chan struct{})
			cr.asyncStarts.goStart(func() { <-release })
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(time.Second, cancel)
			if _, err := cr.beforeProviderSwap(ctx, cfg); !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want the controller stop to abort the swap", err)
			}
			close(release)
		})
	})
	t.Run("v2 ctx done", func(t *testing.T) {
		cr := &CityRuntime{v2: newDefaultPlanner(io.Discard)}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		resume, err := cr.beforeProviderSwap(ctx, cfg)
		resume()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want a done ctx to abort the v2 swap", err)
		}
	})
}

// A controller stop during the swap wait aborts the reload: nothing is
// stopped and the new provider is not published.
// Kills: a swap that goes on to stop runtimes and publish after ctx is done.
func TestReloadProviderSwapAbortsWhenContextDone(t *testing.T) {
	tomlPath := filepath.Join(t.TempDir(), "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	base := runtime.NewFake()
	stubSessionProviderBuilds(t, map[string]runtime.Provider{"fail": runtime.NewFake()})
	startFakeSessions(t, base, "worker-1")
	f := newSwapReloadFixture(t, tomlPath, base, []string{"worker-1"}, nil)

	writeCityRuntimeConfig(t, tomlPath, "fail")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lastProviderName := "fake"
	reply := f.cr.reloadConfigTraced(ctx, &lastProviderName, filepath.Dir(tomlPath), nil, reloadSourceManual)
	f.requireAborted(t, reply, context.Canceled.Error(), base)
	if !base.IsRunning("worker-1") || lastProviderName != "fake" {
		t.Fatalf("worker-1 running = %v, provider name %q; want both kept", base.IsRunning("worker-1"), lastProviderName)
	}
}

// standaloneSwapRuntime is a standalone (no API) city runtime on base whose
// city bead store failed to open at boot.
func standaloneSwapRuntime(t *testing.T, tomlPath string, base runtime.Provider) *CityRuntime {
	t.Helper()
	dir := filepath.Dir(tomlPath)
	result, err := tryReloadConfig(tomlPath, "test-city", dir)
	if err != nil {
		t.Fatal(err)
	}
	cr := &CityRuntime{
		cityPath: dir, cityName: "test-city", configName: "test-city", tomlPath: tomlPath,
		configRev: result.Revision, cfg: result.Cfg, sp: base, dops: newDrainOps(base),
		rec: events.Discard, stdout: io.Discard, stderr: io.Discard, logPrefix: "gc test",
	}
	t.Cleanup(cr.stopConfigWatcher)
	return cr
}

// The reviewer's scenario: a standalone city whose bead store failed at
// boot, with a tmux session running, reloads to add its first ACP agent. The
// reload reopens the store before the swap, so the swap reads the session
// beads, keeps the session, and publishes; a store that still will not open
// aborts the swap with its own reason instead of wedging on an unknown route,
// and leaves the rig stores of the config still running.
// Kills: the store refresh left after publication (every such reload aborts
// forever), a no-store abort that reads like any unknown route, rig stores
// rebuilt for a config the swap then does not publish, and rig stores not
// rebuilt once it does.
func TestReloadStandaloneSwapReopensStoreBeforeTheSwap(t *testing.T) {
	for _, tc := range []struct {
		name      string
		storeOpen bool
	}{{name: "store opens", storeOpen: true}, {name: "store still unavailable"}} {
		t.Run(tc.name, func(t *testing.T) {
			tomlPath := filepath.Join(t.TempDir(), "city.toml")
			writeCityRuntimeConfig(t, tomlPath, "fake")
			base := runtime.NewFake()
			stubSessionProviderBuilds(t, map[string]runtime.Provider{"acp": runtime.NewFake()})
			startFakeSessions(t, base, "worker-1")
			cr := standaloneSwapRuntime(t, tomlPath, base)
			if !tc.storeOpen {
				old := reloadOpenCityStore
				reloadOpenCityStore = func(string) (beads.Store, error) { return nil, errSwapListStore }
				t.Cleanup(func() { reloadOpenCityStore = old })
			}

			// A rig-store map the swap must leave alone until publication.
			oldRigs := map[string]beads.Store{"old-rig": beads.NewMemStore()}
			cr.setStandaloneStores(nil, oldRigs)

			writeACPAgentCityConfig(t, tomlPath, "fake")
			lastProviderName := "fake"
			reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, filepath.Dir(tomlPath), nil, reloadSourceManual)
			_, keptOldRigs := cr.rigBeadStores()["old-rig"]

			if !base.IsRunning("worker-1") || base.CountCalls("Stop", "worker-1") != 0 {
				t.Fatalf("worker-1: running=%v, Stop=%d; want it untouched", base.IsRunning("worker-1"), base.CountCalls("Stop", "worker-1"))
			}
			if !tc.storeOpen {
				if reply.Outcome != reloadOutcomeFailed || !strings.Contains(reply.Error, "no session bead store") || cr.sp != base {
					t.Fatalf("reply = %+v, provider %T; want the no-store abort with the old provider kept", reply, cr.sp)
				}
				if !keptOldRigs {
					t.Fatalf("aborted swap rebuilt the rig stores for the unpublished config: %v", cr.rigBeadStores())
				}
				return
			}
			if reply.Outcome == reloadOutcomeFailed || cr.cityBeadStore() == nil {
				t.Fatalf("reply = %+v, store %v; want the reopened store and an applied swap", reply, cr.cityBeadStore())
			}
			if keptOldRigs {
				t.Fatalf("rig stores not rebuilt for the published config: %v", cr.rigBeadStores())
			}
			if autoSP, ok := cr.sp.(*sessionauto.Provider); !ok || autoSP.RouteFor("worker-1").Provider != base {
				t.Fatalf("provider after the swap = %T, want the auto composition over the carried base", cr.sp)
			}
		})
	}
}

// The accepted next-pass consequence (CONTRACT P7, §12.2 row 25): a legacy
// pass over an active row whose runtime the swap stopped reads it as a runtime
// death. The heal parks it asleep with runtime-missing (freeable) and resets
// its conversation, and the churn classifier charges one strike.
// Kills: a swap that leaves the row in a shape legacy cannot free or restart,
// and an unpinned change to that consequence.
func TestLegacyPassAfterProviderSwapStopReadsRuntimeDeath(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"state":               "active",
		"last_woke_at":        env.clk.Now().Add(-90 * time.Second).UTC().Format(time.RFC3339),
		"session_key":         "old-key",
		"started_config_hash": "old-hash",
	})

	stops, err := providerSwapStops([]runtime.BackendListing{{Provider: env.sp, Names: []string{"worker"}}}, runtime.NewFake())
	if err != nil || len(stops) != 1 {
		t.Fatalf("stops = %+v, err = %v; want worker", stops, err)
	}
	if err := stopProviderSwapRuntimes(stops, env.cfg, env.store, events.Discard, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if after, _ := env.store.Get(session.ID); !maps.Equal(after.Metadata, session.Metadata) {
		t.Fatalf("the swap stop wrote the row:\nbefore %v\nafter  %v", session.Metadata, after.Metadata)
	}

	env.reconcile([]beads.Bead{session})

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"state":        "asleep",
		"sleep_reason": string(sessionpkg.SleepReasonRuntimeMissing),
		"churn_count":  "1",
		"session_key":  "",
	} {
		if got.Metadata[k] != want {
			t.Fatalf("after the pass %s = %q, want %q (metadata %v)", k, got.Metadata[k], want, got.Metadata)
		}
	}
}

// The new provider's routes come from the session beads as they stand after
// the swap waited out in-flight starts: an ACP session a start created during
// the wait keeps its ACP route and its runtime.
// Kills: routes seeded only from the snapshot read before the wait, which
// default-routes the new session and stops its runtime on the carried leg.
func TestReloadProviderSwapRoutesSessionsStartedDuringTheWait(t *testing.T) {
	tomlPath := filepath.Join(t.TempDir(), "city.toml")
	writeACPAgentCityConfig(t, tomlPath, "fake")
	base, acp, newBase := runtime.NewFake(), runtime.NewFake(), runtime.NewFake()
	old := sessionauto.New(base, acp)
	f := newSwapReloadFixture(t, tomlPath, old, nil, nil)
	finished := make(chan struct{})
	old2 := buildSessionProviderByName
	t.Cleanup(func() { buildSessionProviderByName = old2 })
	buildSessionProviderByName = func(_ *config.City, name string, _ config.SessionConfig, _, _ string) (runtime.Provider, error) {
		if name == "fail" {
			// The new provider is built before the wait; the in-flight start
			// lands its row and runtime only after that.
			f.cr.asyncStarts.goStart(func() {
				defer close(finished)
				b := acpSessionBead("dyn-acp")
				if _, err := f.store.Create(b); err != nil {
					t.Errorf("create row: %v", err)
				}
				old.RouteACP("dyn-acp")
				if err := old.Start(context.Background(), "dyn-acp", runtime.Config{}); err != nil {
					t.Errorf("start: %v", err)
				}
			})
			return newBase, nil
		}
		return runtime.NewFake(), nil
	}

	writeACPAgentCityConfig(t, tomlPath, "fail")
	f.mustReload(t)
	<-finished

	if !acp.IsRunning("dyn-acp") || acp.CountCalls("Stop", "dyn-acp") != 0 {
		t.Fatalf("dyn-acp: running=%v, Stop=%d; want it kept on the carried ACP leg", acp.IsRunning("dyn-acp"), acp.CountCalls("Stop", "dyn-acp"))
	}
	if got := sessionRouteFor(f.cr.sp, "dyn-acp"); got.Provider != acp {
		t.Fatalf("dyn-acp route = %+v, want the ACP leg", got)
	}
}

// Each (leg, name) is its own target: a name listed on both legs is stopped
// on each, by that leg's Stop.
// Kills: grouping targets by name (one leg's runtime survives) and stopping
// through any leg but the one that listed the runtime.
func TestStopProviderSwapRuntimesStopsEachLeg(t *testing.T) {
	base, acp := runtime.NewFake(), runtime.NewFake()
	startFakeSessions(t, base, "x", "w")
	startFakeSessions(t, acp, "x")
	stops := []swapStop{{name: "x", backend: base}, {name: "w", backend: base}, {name: "x", backend: acp}}
	if err := stopProviderSwapRuntimes(stops, nil, nil, events.Discard, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		leg  *runtime.Fake
		name string
	}{{base, "x"}, {base, "w"}, {acp, "x"}} {
		if c.leg.IsRunning(c.name) || c.leg.CountCalls("Stop", c.name) != 1 {
			t.Fatalf("%s: running=%v, Stop=%d; want one Stop on its own leg", c.name, c.leg.IsRunning(c.name), c.leg.CountCalls("Stop", c.name))
		}
	}
}

// An enqueued async start's goroutine is counted from before it launches
// until it returns, so a provider swap waits for it.
// Kills: async starts launched outside the tracker's count.
func TestEnqueuedAsyncStartIsCountedUntilItReturns(t *testing.T) {
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
		store, _, _, item := postStartBeadItem(t, clk, workDir)
		var tracker asyncStartTracker
		gate := make(chan struct{})
		enqueuePreparedStartWaveForCity(
			context.Background(),
			[]asyncPreparedStart{{item: item, release: func() { <-gate }, tracker: &tracker}},
			"", runtime.NewFake(), store, nil, clk, events.NewFake(), postStartTimeout, 1, io.Discard, io.Discard, nil, nil,
			immediateStartStabilityWaiter, immediateSessionStaleKeyDetectionWaiter, nil,
		)
		synctest.Wait()
		if err := tracker.waitLaunchedStarts(context.Background(), 0); err == nil {
			t.Fatal("a running async start goroutine was not counted")
		}
		close(gate)
		synctest.Wait()
		if err := tracker.waitLaunchedStarts(context.Background(), 0); err != nil {
			t.Fatalf("a returned async start goroutine is still counted: %v", err)
		}
	})
}
