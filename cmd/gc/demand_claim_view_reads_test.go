package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// claimViewRow is a row routed to template, in status.
func claimViewRow(id, template, status string) beads.Bead {
	return beads.Bead{ID: id, Title: id, Type: "task", Status: status, Metadata: map[string]string{beadmeta.RoutedToMetadataKey: template}}
}

// claimViewCity is a city with one rig pool, gascity/worker
// (warmCrossStoreCfg), whose default probe reads the city store through a
// cross-store leg. The city holds a genuine city-only row, gc-city-1, and the
// stale replica of gc-stale, which the rig closed. Both stores are caches
// over logged backings; the rig is the pool's claim view.
type claimViewCity struct {
	cityPath   string
	cfg        *config.City
	city       *beads.CachingStore
	rig        *beads.CachingStore
	rigBacking *demandBacking
}

func newClaimViewCity(t *testing.T) claimViewCity {
	t.Helper()
	return newClaimViewCityWith(t, false)
}

// newClaimViewCityWith is newClaimViewCity with the city leg exact or
// lane-fed, and rigRows in the rig besides the closed gc-stale.
func newClaimViewCityWith(t *testing.T, exactCity bool, rigRows ...beads.Bead) claimViewCity {
	t.Helper()
	cityPath := t.TempDir()
	city, _ := newDemandCache(t, exactCity,
		claimViewRow("gc-city-1", warmWorkerTemplate, "open"),
		claimViewRow("gc-stale", warmWorkerTemplate, "open"))
	rig, rigBacking := newDemandCache(t, false, append([]beads.Bead{claimViewRow("gc-stale", warmWorkerTemplate, "closed")}, rigRows...)...)
	return claimViewCity{cityPath: cityPath, cfg: warmCrossStoreCfg(t, cityPath), city: city, rig: rig, rigBacking: rigBacking}
}

func (c claimViewCity) rigStores() map[string]beads.Store {
	return map[string]beads.Store{"gascity": c.rig}
}

func (c claimViewCity) gatherEnv() demandGatherEnv {
	return demandGatherEnv{CityPath: c.cityPath, Cfg: c.cfg, CityStore: c.city, RigStores: c.rigStores()}
}

// laneEnv is the external-reads lane's env for c, built as k1Env builds it.
func (c claimViewCity) laneEnv() externalReadsEnv {
	targets := buildDemandTargets("", c.cityPath, c.cfg, c.city, c.rigStores(), nil, nil, noProbeEnv, io.Discard)
	return externalReadsEnv{
		CityPath: c.cityPath, Cfg: c.cfg, CityStore: c.city, RigStores: c.rigStores(),
		ProbeStores: demandProbeStores(targets), ClaimViews: demandClaimViewSets(targets.defaultScaleTargets),
	}
}

// recording is a recording of c at now: each store's live reads, and the
// claim view's source when view is set.
func (c claimViewCity) recording(now time.Time, view *sourceResult) *externalReadsRecording {
	cityRows := []beads.Bead{claimViewRow("gc-city-1", warmWorkerTemplate, "open"), claimViewRow("gc-stale", warmWorkerTemplate, "open")}
	sources := map[sourceKey]sourceResult{
		keyOf(sourceDemand, c.city): {EndedAt: now, sourcePayload: sourcePayload{Leg: legRecording{RawOpen: cityRows, ReadyAll: cityRows}}},
		keyOf(sourceDemand, c.rig):  {EndedAt: now},
	}
	if view != nil {
		sources[claimViewSourceKey("rig:gascity")] = *view
	}
	return &externalReadsRecording{Seq: 1, FreshFor: time.Minute, Sources: sources}
}

// gather runs the v2 demand gather over rec with the rig's backing armed, and
// returns the view and the rig backing reads it made.
func (c claimViewCity) gather(t *testing.T, now time.Time, rec *externalReadsRecording) (demandView, []string) {
	t.Helper()
	before := len(c.rigBacking.readLog())
	c.rigBacking.armed.Store(true)
	defer c.rigBacking.armed.Store(false)
	v, err := gatherDemand(c.gatherEnv(), newV2DemandReads(now, rec))
	if err != nil {
		t.Fatalf("gatherDemand: %v", err)
	}
	return v, c.rigBacking.readLog()[before:]
}

// TestV2GatherServesClaimViewsFromTheLaneRecording (#9446 review): the claim
// view a rig pool's city rows are asked of is the rig store, and asking it is
// a Get the v2 pass must not make (CONTRACT v5 R4). The external-reads lane
// asks it off the pass, and the gather counts the city-only row, leaves out
// the stale replica, and reads no rig backing.
func TestV2GatherServesClaimViewsFromTheLaneRecording(t *testing.T) {
	c := newClaimViewCity(t)
	lane, _ := newTestBackstopLane(c.laneEnv())
	lane.steps = nil
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	passAndJoin(context.Background(), at(awaitEveryRead(lane), now))

	rec := lane.recording()
	got, ok := rec.source(claimViewSourceKey("rig:gascity"))
	if !ok || got.Err != nil {
		t.Fatalf("claim view source recorded=%t err=%v, want the rig view recorded", ok, got.Err)
	}
	want := map[string]demandClaimViewVerdict{"gc-city-1": {Claimable: true}, "gc-stale": {}}
	if len(got.ClaimView) != len(want) || got.ClaimView["gc-city-1"] != want["gc-city-1"] || got.ClaimView["gc-stale"] != want["gc-stale"] {
		t.Fatalf("recorded verdicts = %+v, want %+v", got.ClaimView, want)
	}

	v, rigReads := c.gather(t, now, rec)
	if len(rigReads) > 0 {
		t.Errorf("the v2 gather read the rig's backing: %q", rigReads)
	}
	if ids := v.Collected.DefaultDemand[warmWorkerTemplate].WorkBeadIDs; !slices.Equal(ids, []string{"gc-city-1"}) {
		t.Errorf("default demand names %v, want only the city-only gc-city-1", ids)
	}
	if v.Collected.DefaultPartials[warmWorkerTemplate] {
		t.Errorf("default demand read partial over a fresh recording: %v", v.Collected.DefaultPartials)
	}
}

// TestV2ClaimViewsReadPartialOnlyWhenTheRecordingCannotAnswer: a claim view
// the recording does not hold, holds stale, or holds failed leaves the row's
// claim unknown, so the row is not counted and the pool reads partial (retain,
// block creates). A fresh, error-free recording with no verdict for the row
// has not asked the view about it yet: the row is claimable, as a pass with no
// claim view counts it (#9446 review, iteration 2). None of them sends the
// pass to the rig's backing.
func TestV2ClaimViewsReadPartialOnlyWhenTheRecordingCannotAnswer(t *testing.T) {
	c := newClaimViewCity(t)
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	verdicts := map[string]demandClaimViewVerdict{"gc-city-1": {Claimable: true}, "gc-stale": {}}
	viewDown := errors.New("rig store down")
	for _, tc := range []struct {
		name    string
		view    *sourceResult
		want    []string
		partial bool
		cause   error
	}{
		{name: "fresh verdicts", view: &sourceResult{EndedAt: now, sourcePayload: sourcePayload{ClaimView: verdicts}}, want: []string{"gc-city-1"}},
		{name: "row not asked yet", view: &sourceResult{EndedAt: now, sourcePayload: sourcePayload{ClaimView: map[string]demandClaimViewVerdict{"gc-stale": {}}}}, want: []string{"gc-city-1"}},
		{name: "view not recorded", partial: true, cause: errDemandRecordingMissing},
		{name: "view recorded stale", view: &sourceResult{EndedAt: now.Add(-2 * time.Minute), sourcePayload: sourcePayload{ClaimView: verdicts}}, partial: true, cause: errDemandRecordingStale},
		{name: "view read failed", view: &sourceResult{EndedAt: now, Err: viewDown}, partial: true, cause: viewDown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, rigReads := c.gather(t, now, c.recording(now, tc.view))
			if len(rigReads) > 0 {
				t.Errorf("the v2 gather read the rig's backing: %q", rigReads)
			}
			if ids := v.Collected.DefaultDemand[warmWorkerTemplate].WorkBeadIDs; !slices.Equal(ids, tc.want) {
				t.Errorf("default demand names %v, want %v", ids, tc.want)
			}
			if got := v.Collected.DefaultPartials[warmWorkerTemplate]; got != tc.partial {
				t.Errorf("pool partial = %t, want %t", got, tc.partial)
			}
			verdict := v2ClaimView(c.recording(now, tc.view), now, "rig:gascity", "gc-city-1")
			if tc.cause == nil {
				if !verdict.Claimable || verdict.Err != nil {
					t.Errorf("v2ClaimView(gc-city-1) = %+v, want claimable with no error", verdict)
				}
				return
			}
			if verdict.Claimable || !errors.Is(verdict.Err, tc.cause) {
				t.Errorf("v2ClaimView(gc-city-1) = %+v, want unclaimable with %v", verdict, tc.cause)
			}
		})
	}
}

// TestV2RigPoolKeepsCreatingForARowItsClaimViewHasNotSeen (#9446 review,
// iteration 2): the rows a v2 pass counts and the claim-view verdicts it
// serves are two reads, so a routed city row can reach the pass before the
// lane has asked the rig's claim view about it. An exact city leg serves the
// row from its cache at once; a lane-fed one publishes it while the view's
// slower read is still running. The view's fresh, error-free verdicts must
// not turn the pool partial for it, which would block every create of the
// pool, its own store's work included, on a healthy city: the row counts, and
// the lane's next recording judges it.
func TestV2RigPoolKeepsCreatingForARowItsClaimViewHasNotSeen(t *testing.T) {
	for _, tc := range []struct {
		name      string
		exactCity bool
		// viewStall, when set, slows every read of the rig in the lane's
		// second pass, so its claim-view read outlasts the source deadline
		// and the pass publishes the view's first-pass verdicts.
		viewStall time.Duration
	}{
		{name: "exact city leg read in the pass", exactCity: true},
		{name: "claim view read outlasts the source deadline", viewStall: 2 * externalReadsSourceDeadline},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newClaimViewCityWith(t, tc.exactCity, claimViewRow("gc-rig-1", warmWorkerTemplate, "open"))
				lane, _ := newTestBackstopLane(c.laneEnv())
				lane.steps = nil
				ctx := context.Background()
				passAndJoin(ctx, lane)

				arrived, err := c.city.Create(beads.Bead{Title: "routed after the lane's pass", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: warmWorkerTemplate}})
				if err != nil {
					t.Fatalf("create the arriving city row: %v", err)
				}
				if tc.viewStall > 0 {
					c.rigBacking.stall.Store(int64(tc.viewStall))
					lane.pass(ctx)
					c.rigBacking.stall.Store(0)
				}
				rec := lane.recording()
				view, ok := rec.source(claimViewSourceKey("rig:gascity"))
				if !ok || view.Err != nil || len(view.ClaimView) == 0 {
					t.Fatalf("fixture: claim view recorded=%t err=%v verdicts=%+v, want the first pass's fresh verdicts", ok, view.Err, view.ClaimView)
				}
				if _, asked := view.ClaimView[arrived.ID]; asked {
					t.Fatalf("fixture: the recording already holds a verdict for %s, so the pass reads no row the view has not seen", arrived.ID)
				}
				if tc.viewStall > 0 && view.InFlightSince.IsZero() {
					t.Fatal("fixture: the claim view's second read did not outlast the source deadline")
				}

				v, err := gatherDemand(c.gatherEnv(), newV2DemandReads(time.Now(), rec))
				if err != nil {
					t.Fatalf("gatherDemand: %v", err)
				}
				if merged := mergeCollectedDemand(c.cfg, v.Collected); merged.PoolScaleCheckPartial[warmWorkerTemplate] {
					t.Errorf("the pool reads partial, so it creates no seat, over a fresh claim view that has not seen %s", arrived.ID)
				}
				ids := slices.Clone(v.Collected.DefaultDemand[warmWorkerTemplate].WorkBeadIDs)
				slices.Sort(ids)
				want := []string{arrived.ID, "gc-city-1", "gc-rig-1"}
				slices.Sort(want)
				if !slices.Equal(ids, want) {
					t.Errorf("default demand names %v, want %v: the rig's own row, the city-only row and the arriving row, not the stale replica", ids, want)
				}
				_ = lane.join(ctx)
			})
		})
	}
}

// TestBackstopLaneRecordsEachClaimViewOnItsOwn: each claim view, exact or
// not, is a source of its own, holding verdicts for the rows its own pools are
// served and no other, and the views share one live Ready of the city leg per
// pass.
func TestBackstopLaneRecordsEachClaimViewOnItsOwn(t *testing.T) {
	cityPath := t.TempDir()
	pool := func(rig string) config.Agent {
		return config.Agent{Name: "worker", Dir: rig, StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3)}
	}
	cfg := &config.City{Workspace: config.Workspace{Name: "gc"}}
	for _, rig := range []string{"alpha", "beta", "gamma"} {
		path := filepath.Join(cityPath, rig)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		cfg.Rigs = append(cfg.Rigs, config.Rig{Name: rig, Path: path})
		cfg.Agents = append(cfg.Agents, pool(rig))
	}
	city, cityBacking := newDemandCache(t, false,
		claimViewRow("gc-a", "alpha/worker", "open"),
		claimViewRow("gc-b", "beta/worker", "open"),
		claimViewRow("gc-g", "gamma/worker", "open"),
		claimViewRow("gc-x", "elsewhere/worker", "open"))
	held := claimViewRow("gc-b", "beta/worker", "open")
	held.Assignee = "beta/worker-1"
	alpha, _ := newDemandCache(t, false)
	beta, _ := newDemandCache(t, false, held)
	gamma, _ := newDemandCache(t, true)
	rigs := map[string]beads.Store{"alpha": alpha, "beta": beta, "gamma": gamma}
	targets := buildDemandTargets("", cityPath, cfg, city, rigs, nil, nil, noProbeEnv, io.Discard)
	env := externalReadsEnv{
		CityPath: cityPath, Cfg: cfg, CityStore: city, RigStores: rigs,
		ProbeStores: demandProbeStores(targets), ClaimViews: demandClaimViewSets(targets.defaultScaleTargets),
	}
	if len(env.ClaimViews) != 3 {
		t.Fatalf("claim view sets = %d, want one per rig pool's view", len(env.ClaimViews))
	}
	lane, _ := newTestBackstopLane(env)
	lane.steps = nil
	cityBacking.armed.Store(true)
	passAndJoin(context.Background(), at(awaitEveryRead(lane), time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)))

	rec := lane.recording()
	for _, tc := range []struct {
		name string
		want map[string]demandClaimViewVerdict
	}{
		{name: "alpha", want: map[string]demandClaimViewVerdict{"gc-a": {Claimable: true}}},
		{name: "beta", want: map[string]demandClaimViewVerdict{"gc-b": {}}},
		{name: "gamma", want: map[string]demandClaimViewVerdict{"gc-g": {Claimable: true}}},
	} {
		got, ok := rec.source(claimViewSourceKey("rig:" + tc.name))
		if !ok || got.Err != nil || len(got.ClaimView) != len(tc.want) {
			t.Errorf("%s view recorded=%t err=%v verdicts=%+v, want %+v", tc.name, ok, got.Err, got.ClaimView, tc.want)
			continue
		}
		for id, want := range tc.want {
			if got.ClaimView[id] != want {
				t.Errorf("%s view verdict for %s = %+v, want %+v", tc.name, id, got.ClaimView[id], want)
			}
		}
	}
	var readies int
	for _, op := range cityBacking.readLog() {
		if strings.HasPrefix(op, "Ready ") {
			readies++
		}
	}
	if readies != 2 {
		t.Errorf("the pass read the city's Ready %d times, want 2: one for its demand leg, one shared by its three claim views", readies)
	}
}

// TestGatherK1EnvCarriesClaimViews: the lane's env names each rig pool's
// claim view with the cross-store legs it answers for, so the lane asks it off
// the pass. Without them the lane records no verdict and v2 reads every such
// pool partial for good.
func TestGatherK1EnvCarriesClaimViews(t *testing.T) {
	f := newGatherFixture(t, poolRow("gc-1", "worker", 1, "active"))
	cfg := *f.cur.Load().Cfg
	rigPath := filepath.Join(f.env.CityPath, "rig-a")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.Rigs = []config.Rig{{Name: "rig-a", Path: rigPath}}
	cfg.Agents = append(slices.Clone(cfg.Agents), config.Agent{Name: "rigworker", Dir: "rig-a", StartCommand: "true", MaxActiveSessions: intPtr(2)})
	f.cur.Store(&reconcileEnv{Gen: 1, Cfg: &cfg, SP: f.sp})
	rig := beads.NewMemStore()
	f.env.RigStores = func() map[string]beads.Store { return map[string]beads.Store{"rig-a": rig} }

	env, err := f.env.externalReadsEnv()
	if err != nil {
		t.Fatalf("k1 env: %v", err)
	}
	if len(env.ClaimViews) != 1 {
		t.Fatalf("claim views = %+v, want the rig pool's one", env.ClaimViews)
	}
	set := env.ClaimViews[0]
	if set.view.reader != beads.StoreReader(rig) || set.view.key != "rig:rig-a" {
		t.Errorf("claim view = (%v, %q), want the rig store, rig:rig-a", set.view.reader, set.view.key)
	}
	if len(set.legs) != 1 || set.legs[0].reader != beads.StoreReader(f.cache) {
		t.Fatalf("claim view legs = %+v, want the city store alone", set.legs)
	}
	if _, ok := set.legs[0].templates["rig-a/rigworker"]; !ok || len(set.legs[0].templates) != 1 {
		t.Errorf("city leg templates = %v, want rig-a/rigworker", set.legs[0].templates)
	}
}

// TestLegacyDemandClaimViewsGiveUpAtTheirDeadline (#9446 review): the legacy
// pass asks its claim views live, so one view that stops answering must not
// hold the pass. Every view is asked at once and the pass waits at most
// demandClaimViewDeadline: the stalled view's rows are left uncounted and
// their pool partial, and the other view's answers count. A pass reads each
// view once, all of its rows in one read, so a view that stops answering
// holds at most one read per pass.
func TestLegacyDemandClaimViewsGiveUpAtTheirDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := &config.City{
			Workspace: config.Workspace{Name: "gc"},
			Rigs:      []config.Rig{{Name: "alpha", Path: "/tmp/alpha"}, {Name: "beta", Path: "/tmp/beta"}},
			Agents: []config.Agent{
				{Name: "worker", Dir: "alpha", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3)},
				{Name: "worker", Dir: "beta", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3)},
			},
		}
		city := beads.NewMemStore()
		city.HonorExplicitIDs = true
		mustCreateRow(t, city, claimViewRow("gc-a", "alpha/worker", "open"), "open")
		mustCreateRow(t, city, claimViewRow("gc-a2", "alpha/worker", "open"), "open")
		mustCreateRow(t, city, claimViewRow("gc-b", "beta/worker", "open"), "open")
		stalled := &hangingByIDStore{Store: beads.NewMemStore(), release: make(chan struct{})}
		stalledView := newClaimViewProbeStore(stalled)
		answering := newClaimViewProbeStore(beads.NewMemStore())
		var targets []defaultScaleCheckTarget
		for _, own := range []defaultScaleCheckTarget{
			{template: "alpha/worker", store: stalledView, storeKey: "rig:alpha"},
			{template: "beta/worker", store: answering, storeKey: "rig:beta"},
		} {
			targets = append(targets, own)
			targets = append(targets, crossStoreDemandLegs([]defaultScaleCheckTarget{{template: own.template, store: city, storeKey: "city"}}, own)...)
		}

		start := time.Now()
		counts, demand, partial, errs := defaultScaleCheckCountsAndDemand(cfg, targets)
		if waited := time.Since(start); waited != demandClaimViewDeadline {
			t.Errorf("the pass returned after %s, want the claim views' deadline %s", waited, demandClaimViewDeadline)
		}
		if counts["alpha/worker"] != 0 || !partial["alpha/worker"] {
			t.Errorf("alpha/worker demand = %d partial = %t, want 0 and partial: its view never answered", counts["alpha/worker"], partial["alpha/worker"])
		}
		if !slices.ContainsFunc(errs, func(err error) bool { return errors.Is(err, errDemandClaimViewDeadline) }) {
			t.Errorf("errs = %v, want one naming the claim views' deadline", errs)
		}
		if ids := demand["beta/worker"].WorkBeadIDs; !slices.Equal(ids, []string{"gc-b"}) || partial["beta/worker"] {
			t.Errorf("beta/worker demand = %v partial = %t, want gc-b, complete: its view answered in time", ids, partial["beta/worker"])
		}
		if got := answering.probes("gc-b"); got != 1 {
			t.Errorf("the answering view was asked about gc-b %d times, want 1", got)
		}
		close(stalled.release)
		synctest.Wait()
		if reads, a, a2 := stalledView.readsByID(), stalledView.probes("gc-a"), stalledView.probes("gc-a2"); reads != 1 || a != 1 || a2 != 1 {
			t.Errorf("the stalled view was read %d times by id, asking gc-a %d and gc-a2 %d times; want one read asking each once", reads, a, a2)
		}
	})
}

// TestLegacyDemandClaimViewsAnswerABacklogInOneRead (#9446 review,
// iterations 3 and 4): a view asked one row at a time cost the legacy pass a
// round trip per cross-store row against a fixed deadline. At bd's cost for a
// city-only id (about 1.5 s: bd show, then the wisp query), the seventh row
// missed demandClaimViewDeadline, so a healthy rig pool read partial and got
// no fresh seat on any tick. One read for all of a pass's rows still grew with
// the backlog, a bd show per 200 ids: 1,400 city-only rows took eight reads,
// 12 s, and the pool read partial on every pass. A pass now asks only about
// the rows inside the claim window, in one read by id: behind a view that
// takes 1.5 s a read, the pool's own row and every city-only row count, the
// stale replica in the window does not, and the pass is complete after one
// round trip that asked no row behind the window.
func TestLegacyDemandClaimViewsAnswerABacklogInOneRead(t *testing.T) {
	for _, backlog := range []int{20, 1400} {
		t.Run(fmt.Sprintf("%d city-only rows", backlog), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const roundTrip = 1500 * time.Millisecond
				rigMem := beads.NewMemStore()
				rigMem.HonorExplicitIDs = true
				cityMem := beads.NewMemStore()
				cityMem.HonorExplicitIDs = true
				mustCreateRow(t, rigMem, claimViewRow("rig-own", agreementTemplate, "open"), "open")
				stale := claimViewRow("canary-x", agreementTemplate, "closed")
				stale.Assignee = "other-session"
				mustCreateRow(t, rigMem, stale, "closed")
				mustCreateRow(t, cityMem, claimViewRow("canary-x", agreementTemplate, "open"), "open")
				want := []string{"rig-own"}
				for n := range backlog {
					id := fmt.Sprintf("city-%04d", n)
					mustCreateRow(t, cityMem, claimViewRow(id, agreementTemplate, "open"), "open")
					want = append(want, id)
				}
				last := want[len(want)-1]
				slices.Sort(want)
				rig := newClaimViewProbeStore(rigMem)
				rig.latency = roundTrip
				own := defaultScaleCheckTarget{template: agreementTemplate, store: rig, storeKey: "rig:rig"}
				targets := append([]defaultScaleCheckTarget{own}, crossStoreDemandLegs([]defaultScaleCheckTarget{{template: agreementTemplate, store: cityMem, storeKey: "city"}}, own)...)

				start := time.Now()
				counts, demand, partial, errs := defaultScaleCheckCountsAndDemand(agreementConfig(), targets)
				waited := time.Since(start)
				if len(errs) != 0 || partial[agreementTemplate] {
					t.Fatalf("after %s: errs = %v partial = %t, want a complete pass", waited, errs, partial[agreementTemplate])
				}
				ids := slices.Sorted(slices.Values(demand[agreementTemplate].WorkBeadIDs))
				if counts[agreementTemplate] != len(want) || !slices.Equal(ids, want) {
					t.Errorf("demand = %d rows, want %d: the pool's own row and every city-only row, not the stale replica", counts[agreementTemplate], len(want))
				}
				if reads := rig.readsByID(); reads != 1 || waited != roundTrip {
					t.Errorf("the view was read %d times by id and the pass took %s, want one read of %s", reads, waited, roundTrip)
				}
				if widest, asked := rig.widestRead(), rig.probes("canary-x"); widest > demandClaimViewWindow || asked != 1 {
					t.Errorf("the view's read asked %d ids, the stale replica %d times; want at most the claim window's %d, the replica once", widest, asked, demandClaimViewWindow)
				}
				if asked := rig.probes(last); asked != 0 {
					t.Errorf("the view was asked about %s, ranked behind the claim window, %d times; want 0", last, asked)
				}
			})
		})
	}
}

// TestDemandClaimViewsAskOnlyTheClaimWindow (#9446 review, iteration 4): a
// pass asks a claim view only about the rows inside a template's claim window,
// the rows a seat's claim tier reads. A cross-store row ranked behind the
// window counts as a row with no claim view does, even a stale replica, and
// is asked about once claims advance the window to it, when the view shadows
// the replica.
func TestDemandClaimViewsAskOnlyTheClaimWindow(t *testing.T) {
	rigMem := beads.NewMemStore()
	rigMem.HonorExplicitIDs = true
	cityMem := beads.NewMemStore()
	cityMem.HonorExplicitIDs = true
	var ahead []string
	for n := range demandClaimViewWindow {
		id := fmt.Sprintf("gc-%02d", n)
		mustCreateRow(t, cityMem, claimViewRow(id, agreementTemplate, "open"), "open")
		ahead = append(ahead, id)
	}
	// The stale replica ranks first behind the window: the rig closed its copy.
	mustCreateRow(t, cityMem, claimViewRow("gc-stale", agreementTemplate, "open"), "open")
	mustCreateRow(t, rigMem, claimViewRow("gc-stale", agreementTemplate, "closed"), "closed")
	rig := newClaimViewProbeStore(rigMem)
	own := defaultScaleCheckTarget{template: agreementTemplate, store: rig, storeKey: "rig:rig"}
	targets := crossStoreDemandLegs([]defaultScaleCheckTarget{{template: agreementTemplate, store: cityMem, storeKey: "city"}}, own)
	pass := func() []string {
		t.Helper()
		counts, demand, partial, errs := defaultScaleCheckCountsAndDemand(agreementConfig(), targets)
		if len(errs) != 0 || partial[agreementTemplate] {
			t.Fatalf("errs = %v partial = %t, want a complete pass", errs, partial[agreementTemplate])
		}
		ids := slices.Sorted(slices.Values(demand[agreementTemplate].WorkBeadIDs))
		if counts[agreementTemplate] != len(ids) {
			t.Fatalf("counts = %d, but demand names %d rows", counts[agreementTemplate], len(ids))
		}
		return ids
	}

	want := slices.Sorted(slices.Values(append(slices.Clone(ahead), "gc-stale")))
	if ids := pass(); !slices.Equal(ids, want) {
		t.Errorf("demand = %v, want %v: the window's rows, and the replica behind it as a row with no claim view", ids, want)
	}
	if asked := rig.probes("gc-stale"); asked != 0 {
		t.Errorf("the view was asked about gc-stale, behind the claim window, %d times; want 0", asked)
	}
	for _, id := range ahead {
		if asked := rig.probes(id); asked != 1 {
			t.Errorf("the view was asked about %s, in the claim window, %d times; want 1", id, asked)
		}
	}

	// A seat claims the window's first row, which moves the replica into it.
	claimant := agreementTemplate + "-1"
	if err := cityMem.Update(ahead[0], beads.UpdateOpts{Assignee: &claimant}); err != nil {
		t.Fatalf("claim %s: %v", ahead[0], err)
	}
	if ids := pass(); !slices.Equal(ids, ahead[1:]) {
		t.Errorf("demand = %v, want %v: the unclaimed rows, the replica shadowed once the window reached it", ids, ahead[1:])
	}
	if asked := rig.probes("gc-stale"); asked != 1 {
		t.Errorf("the view was asked about gc-stale, now in the claim window, %d times; want 1", asked)
	}
}

// TestDemandClaimViewRefusesARawParkedRigCopy (#9446 review, iteration 4): a
// bd-backed read folds a parked status (blocked, deferred, pinned, hooked) to
// open, but bd's claim updates only a row whose recorded status is open, so a
// seat claiming a parked rig copy is refused ("issue not claimable: status
// deferred") and the refusal feeds no guard. The stale city replica of a
// parked rig copy is therefore no demand, in a legacy pass and in the
// external-reads lane's recording: the claim view reads the copy's raw status.
// The rig copy is decoded by a production BdStore, as the claim view reads it;
// a MemStore keeps the status it is given and folds none.
func TestDemandClaimViewRefusesARawParkedRigCopy(t *testing.T) {
	for _, tc := range []struct {
		raw       string
		claimable bool
	}{
		{raw: "open", claimable: true},
		{raw: "blocked"},
		{raw: "deferred"},
		{raw: "pinned"},
		{raw: "hooked"},
		{raw: "closed"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			var shows int
			rig := beads.NewBdStore(t.TempDir(), func(_, name string, args ...string) ([]byte, error) {
				if name != "bd" || !slices.Equal(args, []string{"show", "--json", "canary-x"}) {
					t.Errorf("unexpected rig command: %s %q", name, args)
					return nil, fmt.Errorf("unexpected command: %s %q", name, args)
				}
				shows++
				return []byte(`[{"id":"canary-x","title":"x","status":"` + tc.raw + `","issue_type":"task","created_at":"2026-11-23T00:00:00Z","metadata":{"gc.routed_to":"` + agreementTemplate + `"}}]`), nil
			})
			city := beads.NewMemStore()
			city.HonorExplicitIDs = true
			mustCreateRow(t, city, claimViewRow("canary-x", agreementTemplate, "open"), "open")
			own := defaultScaleCheckTarget{template: agreementTemplate, store: rig, storeKey: "rig:rig"}
			targets := crossStoreDemandLegs([]defaultScaleCheckTarget{{template: agreementTemplate, store: city, storeKey: "city"}}, own)
			var want []string
			if tc.claimable {
				want = []string{"canary-x"}
			}

			counts, demand, partial, errs := defaultScaleCheckCountsAndDemand(agreementConfig(), targets)
			if len(errs) != 0 || partial[agreementTemplate] {
				t.Fatalf("errs = %v partial = %t, want a complete pass", errs, partial[agreementTemplate])
			}
			if ids := demand[agreementTemplate].WorkBeadIDs; counts[agreementTemplate] != len(want) || !slices.Equal(ids, want) {
				t.Errorf("demand = %d %v, want %v: the rig copy's raw status is %s", counts[agreementTemplate], ids, want, tc.raw)
			}

			sets := demandClaimViewSets(targets)
			if len(sets) != 1 {
				t.Fatalf("claim view sets = %d, want the rig pool's one", len(sets))
			}
			recorded, err := recordDemandClaimView(agreementConfig(), sets[0], newLaneLegReady().read)
			if err != nil {
				t.Fatalf("recordDemandClaimView: %v", err)
			}
			if v, ok := recorded["canary-x"]; !ok || v.Err != nil || v.Claimable != tc.claimable {
				t.Errorf("the lane's verdict for canary-x = %+v (recorded %t), want claimable=%t: the rig copy's raw status is %s", v, ok, tc.claimable, tc.raw)
			}
			if shows != 2 {
				t.Errorf("the rig was read by id %d times, want 2: one bd show for the pass, one for the lane", shows)
			}
		})
	}
}

// TestRecordDemandClaimViewReadsTheViewOnce (#9446 review, iterations 3 and
// 4): the external-reads lane asks a claim view, in one read by id, about the
// rows a pass can ask it about: those inside each pool's claim window, the
// first demandClaimViewWindow its legs serve the pool in the canonical ready
// order, however many rows there are. It records a verdict for each of them
// and for no row behind the window, which a pass counts unasked. A read that
// fails answers every row it asked with its error and fails the source, so a
// row the lane did not ask about reads partial too.
func TestRecordDemandClaimViewReadsTheViewOnce(t *testing.T) {
	cause := errors.New("bd show: connection refused")
	for _, tc := range []struct {
		name string
		fail error
	}{
		{name: "the view answers"},
		{name: "the view's read fails", fail: cause},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			cfg := warmCrossStoreCfg(t, cityPath)
			city := beads.NewMemStore()
			city.HonorExplicitIDs = true
			rigMem := beads.NewMemStore()
			rigMem.HonorExplicitIDs = true
			mustCreateRow(t, city, claimViewRow("gc-stale", warmWorkerTemplate, "open"), "open")
			mustCreateRow(t, rigMem, claimViewRow("gc-stale", warmWorkerTemplate, "closed"), "closed")
			want := map[string]demandClaimViewVerdict{"gc-stale": {}}
			var behind []string
			for n := range 30 {
				id := fmt.Sprintf("gc-city-%02d", n)
				mustCreateRow(t, city, claimViewRow(id, warmWorkerTemplate, "open"), "open")
				if len(want) < demandClaimViewWindow {
					want[id] = demandClaimViewVerdict{Claimable: true}
				} else {
					behind = append(behind, id)
				}
			}
			rig := newClaimViewProbeStore(rigMem)
			if tc.fail != nil {
				rig.fail["gc-stale"] = tc.fail
				for id := range want {
					want[id] = demandClaimViewVerdict{Err: tc.fail}
				}
			}
			rigs := map[string]beads.Store{"gascity": rig}
			targets := buildDemandTargets("", cityPath, cfg, city, rigs, nil, nil, noProbeEnv, io.Discard)
			sets := demandClaimViewSets(targets.defaultScaleTargets)
			if len(sets) != 1 {
				t.Fatalf("claim view sets = %d, want the rig pool's one", len(sets))
			}

			got, err := recordDemandClaimView(cfg, sets[0], newLaneLegReady().read)
			if !errors.Is(err, tc.fail) {
				t.Errorf("recordDemandClaimView error = %v, want %v", err, tc.fail)
			}
			if len(got) != len(want) {
				t.Errorf("recorded %d verdicts, want %d", len(got), len(want))
			}
			for id, w := range want {
				if v, ok := got[id]; !ok || v.Claimable != w.Claimable || !errors.Is(v.Err, w.Err) {
					t.Errorf("verdict for %s = %+v (recorded %t), want %+v", id, v, ok, w)
				}
			}
			for _, id := range behind {
				if v, ok := got[id]; ok || rig.probes(id) != 0 {
					t.Errorf("%s, behind the claim window, was asked %d times and recorded %+v; want neither", id, rig.probes(id), v)
				}
			}
			if reads := rig.readsByID(); reads != 1 {
				t.Errorf("the view was read %d times by id, want 1: all %d rows in one read", reads, len(want))
			}
		})
	}
}

// TestV2RigPoolStaysServedOverALargeRoutedBacklog (#9446 review, iterations
// 3 and 4): the external-reads lane asked a claim view one row at a time, so
// once the routed backlog grew its read outlasted the recording's freshness
// (3 x patrol), and the pool read partial, creating no seat, for much of every
// cycle (60 rows at 1.5 s a read: 44% of steady state). One read for all of
// the rows still grew with the backlog, a bd show per 200 ids, past the
// source deadline at 1,400 rows. The lane now asks the view only about the
// rows inside the pool's claim window, in one read by id. With 1,400
// city-only rows behind a view that takes 1.5 s a read, the pool is served
// within a patrol, then reads complete, every row counted, at every sample,
// and no read of the view asks more than the window.
func TestV2RigPoolStaysServedOverALargeRoutedBacklog(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const backlog = 1400
		cityPath := t.TempDir()
		cfg := warmCrossStoreCfg(t, cityPath)
		var rows []beads.Bead
		for n := range backlog {
			rows = append(rows, claimViewRow(fmt.Sprintf("gc-city-%04d", n), warmWorkerTemplate, "open"))
		}
		city, _ := newDemandCache(t, false, rows...)
		view := newClaimViewProbeStore(beads.NewMemStore())
		view.latency = 1500 * time.Millisecond
		rig := beads.NewCachingStoreForTest(view, nil)
		if err := rig.Prime(context.Background()); err != nil {
			t.Fatalf("prime the rig cache: %v", err)
		}
		rigs := map[string]beads.Store{"gascity": rig}
		targets := buildDemandTargets("", cityPath, cfg, city, rigs, nil, nil, noProbeEnv, io.Discard)
		lane, _ := newTestBackstopLane(externalReadsEnv{
			CityPath: cityPath, Cfg: cfg, CityStore: city, RigStores: rigs,
			ProbeStores: demandProbeStores(targets), ClaimViews: demandClaimViewSets(targets.defaultScaleTargets),
		})
		lane.steps = nil
		env := demandGatherEnv{CityPath: cityPath, Cfg: cfg, CityStore: city, RigStores: rigs}
		start := time.Now()
		startBackstopLaneInBubble(t, lane)
		var warmUp time.Duration
		var samples, partial, short int
		for end := start.Add(5 * time.Minute); time.Now().Before(end); {
			<-time.After(time.Second)
			v, err := gatherDemand(env, newV2DemandReads(time.Now(), lane.recording()))
			if err != nil {
				t.Fatalf("gatherDemand at %s: %v", time.Since(start), err)
			}
			served := !mergeCollectedDemand(cfg, v.Collected).PoolScaleCheckPartial[warmWorkerTemplate]
			if warmUp == 0 {
				if served {
					warmUp = time.Since(start)
				}
				continue
			}
			samples++
			if !served {
				partial++
			}
			if v.Collected.DefaultCounts[warmWorkerTemplate] != backlog {
				short++
			}
		}
		if warmUp == 0 || warmUp > backstopTestInterval || partial > 0 || short > 0 {
			t.Errorf("served after %s, then partial at %d and short of the %d rows at %d of %d samples; want served within a patrol (%s), then complete with every row at each sample", warmUp, partial, backlog, short, samples, backstopTestInterval)
		}
		if widest := view.widestRead(); widest == 0 || widest > demandClaimViewWindow {
			t.Errorf("the widest read of the view asked %d ids, want at most the claim window's %d", widest, demandClaimViewWindow)
		}
	})
}
