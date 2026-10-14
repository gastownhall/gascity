package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// demandBacking is the store behind a CachingStore in the demand read tests:
// a MemStore whose reads are logged once armed, and whose live reads and
// metadata writes can be made to fail. Embedding the Store interface hides
// MemStore's optional capabilities, so every backing read the cache makes
// passes through the methods below.
type demandBacking struct {
	beads.Store
	armed      atomic.Bool
	failLive   atomic.Bool
	failWrites atomic.Bool
	mu         sync.Mutex
	ops        []string
}

var errDemandBackingDown = errors.New("demand backing down")

// exactDemandBacking opts the backing into exact cached reads, as the
// controller's SQLite binding is.
type exactDemandBacking struct{ *demandBacking }

func (exactDemandBacking) CachedReadExact() bool { return true }

func newDemandBacking(seed ...beads.Bead) *demandBacking {
	return &demandBacking{Store: beads.NewMemStoreFrom(0, seed, nil)}
}

// log records a read in the compact form the goldens pin: the operation and
// the query fields that select a read tier or a row set.
func (b *demandBacking) log(format string, args ...any) {
	if !b.armed.Load() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ops = append(b.ops, fmt.Sprintf(format, args...))
}

func (b *demandBacking) readLog() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.ops)
}

func (b *demandBacking) Get(id string) (beads.Bead, error) {
	b.log("Get %s", id)
	return b.Store.Get(id)
}

func (b *demandBacking) List(q beads.ListQuery) ([]beads.Bead, error) {
	b.log("List status=%s live=%t", q.Status, q.Live)
	if q.Live && b.failLive.Load() {
		return nil, errDemandBackingDown
	}
	return b.Store.List(q)
}

func (b *demandBacking) ListOpen(status ...string) ([]beads.Bead, error) {
	b.log("ListOpen %v", status)
	return b.Store.ListOpen(status...)
}

func (b *demandBacking) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	b.log("Ready %+v", q)
	if b.failLive.Load() {
		return nil, errDemandBackingDown
	}
	return b.Store.Ready(q...)
}

func (b *demandBacking) Children(parentID string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	b.log("Children %s", parentID)
	return b.Store.Children(parentID, opts...)
}

func (b *demandBacking) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	b.log("ListByLabel %s", label)
	return b.Store.ListByLabel(label, limit, opts...)
}

func (b *demandBacking) ListByAssignee(assignee, status string, limit int) ([]beads.Bead, error) {
	b.log("ListByAssignee %s", assignee)
	return b.Store.ListByAssignee(assignee, status, limit)
}

func (b *demandBacking) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	b.log("ListByMetadata %v", filters)
	return b.Store.ListByMetadata(filters, limit, opts...)
}

func (b *demandBacking) DepList(id, direction string) ([]beads.Dep, error) {
	b.log("DepList %s", id)
	return b.Store.DepList(id, direction)
}

func (b *demandBacking) SetMetadataBatch(id string, kvs map[string]string) error {
	if b.failWrites.Load() {
		return errDemandBackingDown
	}
	return b.Store.SetMetadataBatch(id, kvs)
}

// newDemandCache primes a CachingStore over a demandBacking seeded with seed,
// exact or not, and returns both.
func newDemandCache(t *testing.T, exact bool, seed ...beads.Bead) (*beads.CachingStore, *demandBacking) {
	t.Helper()
	backing := newDemandBacking(seed...)
	var store beads.Store = backing
	if exact {
		store = exactDemandBacking{backing}
	}
	cache := beads.NewCachingStoreForTest(store, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("prime demand cache: %v", err)
	}
	return cache, backing
}

// dirtyDemandRow marks id dirty in cache: a metadata write the backing
// rejects leaves the cached row fenced until a read refreshes it.
func dirtyDemandRow(t *testing.T, cache *beads.CachingStore, backing *demandBacking, id string) {
	t.Helper()
	backing.failWrites.Store(true)
	defer backing.failWrites.Store(false)
	if err := cache.SetMetadataBatch(id, map[string]string{"demand.test": "dirty"}); err == nil {
		t.Fatalf("dirtying %s: the backing accepted the write", id)
	}
	if _, ok := cache.CachedList(beads.ListQuery{Status: "open"}); ok {
		t.Fatalf("dirtying %s: the cache still serves strict reads", id)
	}
}

func routedDemandBead(id string) beads.Bead {
	return beads.Bead{ID: id, Title: id, Type: "task", Status: "open", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}}
}

func assignedDemandBead(id, status string) beads.Bead {
	return beads.Bead{ID: id, Title: id, Type: "task", Status: status, Assignee: "worker-1"}
}

// assignedWorkflowRoot is an open assigned workflow root: wake demand read
// from the raw open List (isOpenAssignedMoleculeWork).
func assignedWorkflowRoot(id, assignee string) beads.Bead {
	return beads.Bead{
		ID: id, Title: id, Type: "wisp", Status: "open", Assignee: assignee,
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
	}
}

func demandReadsTestConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}},
	}
}

// demandRun is everything the four collectors return over one store,
// flattened for comparison.
type demandRun struct {
	Assigned         []string
	ReadyAssigned    []string
	AssignedPartial  bool
	Routed           []string
	RoutedPartial    bool
	ScaleCounts      map[string]int
	ScalePartials    map[string]bool
	ScaleErrs        []string
	NamedPartials    map[string]bool
	NamedErrs        []string
	ReadyAssignedSet map[string]bool
}

// runDemandCollectors runs the four legacy collectors over store with
// newCache supplying each pass's readyDemandCache and reads passed to
// collectOpenUnassignedRoutedWork.
func runDemandCollectors(cfg *config.City, store beads.Store, newCache func() *readyDemandCache, reads demandReads) demandRun {
	var run demandRun
	assigned, _, _, readyAssigned, partial := collectAssignedWorkBeadsWithStores("", cfg, store, nil, nil, nil, newCache())
	// Cached Lists come back in map order, so compare row sets.
	run.Assigned, run.AssignedPartial = slices.Sorted(slices.Values(ids(assigned))), partial
	run.ReadyAssignedSet = make(map[string]bool)
	for k := range readyAssigned {
		run.ReadyAssigned = append(run.ReadyAssigned, k.ID)
		run.ReadyAssignedSet[k.ID] = true
	}
	slices.Sort(run.ReadyAssigned)
	routed, _, _, routedPartial := collectOpenUnassignedRoutedWork("", cfg, store, nil, nil, io.Discard, nil, reads)
	run.Routed, run.RoutedPartial = slices.Sorted(slices.Values(ids(routed))), routedPartial
	targets := []defaultScaleCheckTarget{{template: "worker", storeKey: "city", store: store}}
	demandCache := newCache()
	counts, _, partials, errs := defaultScaleCheckCountsAndDemand(cfg, targets, demandCache)
	run.ScaleCounts, run.ScalePartials, run.ScaleErrs = counts, partials, errorTexts(errs)
	_, namedPartials, namedErrs := defaultNamedSessionDemand(targets, cfg, "", demandCache)
	run.NamedPartials, run.NamedErrs = namedPartials, errorTexts(namedErrs)
	return run
}

func errorTexts(errs []error) []string {
	var out []string
	for _, err := range errs {
		out = append(out, err.Error())
	}
	return out
}

// Kills: the seam changing a legacy read. Each legacyDemandReads method must
// be the pre-seam call, result and backing reads alike, on every store shape
// legacy meets (plain, cached clean, cached dirty, live outage); and every
// collector must read the same with a nil reads as with legacyDemandReads.
func TestLegacyDemandReadsByteIdenticalToDirectCalls(t *testing.T) {
	cfg := demandReadsTestConfig()
	cfg.Daemon.MaxWakesPerTick = intPtr(1)
	seed := []beads.Bead{
		routedDemandBead("gc-r1"),
		routedDemandBead("gc-r2"),
		// Two ready assigned rows against a wake budget of 1, and no ready
		// in-progress capture, so the assigned Ready pass runs and truncates.
		assignedDemandBead("gc-a1", "open"),
		assignedDemandBead("gc-a2", "open"),
	}
	type fixture struct {
		store   beads.Store
		backing *demandBacking
	}
	fixtures := map[string]func(t *testing.T) fixture{
		"plain": func(*testing.T) fixture {
			b := newDemandBacking(seed...)
			return fixture{b, b}
		},
		"cached": func(t *testing.T) fixture {
			c, b := newDemandCache(t, false, seed...)
			return fixture{c, b}
		},
		"cached dirty": func(t *testing.T) fixture {
			c, b := newDemandCache(t, false, seed...)
			dirtyDemandRow(t, c, b, "gc-r1")
			return fixture{c, b}
		},
		"cached live outage": func(t *testing.T) fixture {
			c, b := newDemandCache(t, false, seed...)
			b.failLive.Store(true)
			return fixture{c, b}
		},
		"plain live outage": func(*testing.T) fixture {
			b := newDemandBacking(seed...)
			b.failLive.Store(true)
			return fixture{b, b}
		},
	}
	// The pre-seam calls, verbatim.
	direct := map[string]func(beads.Store) ([]beads.Bead, error){
		"RawOpen": func(s beads.Store) ([]beads.Bead, error) {
			return beads.HandlesFor(s).Live.List(beads.ListQuery{Status: "open", AllowScan: true})
		},
		"Cached": func(s beads.Store) ([]beads.Bead, error) {
			handles := beads.HandlesFor(s)
			rows, err := handles.Cached.List(beads.ListQuery{Status: "open"})
			if errors.Is(err, beads.ErrCacheUnavailable) {
				return handles.Live.List(beads.ListQuery{Status: "open"})
			}
			return rows, err
		},
		"ReadyAll": func(s beads.Store) ([]beads.Bead, error) {
			return beads.HandlesFor(s).Live.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
		},
		"CachedReady": func(s beads.Store) ([]beads.Bead, error) {
			return beads.HandlesFor(s).Cached.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
		},
	}
	seam := map[string]func(beads.Store) ([]beads.Bead, error){
		"RawOpen": legacyDemandReads{}.RawOpen,
		"Cached": func(s beads.Store) ([]beads.Bead, error) {
			return legacyDemandReads{}.Cached(s, beads.ListQuery{Status: "open"})
		},
		"ReadyAll":    legacyDemandReads{}.ReadyAll,
		"CachedReady": legacyDemandReads{}.CachedReady,
	}
	observe := func(t *testing.T, build func(*testing.T) fixture, read func(beads.Store) ([]beads.Bead, error)) string {
		f := build(t)
		f.backing.armed.Store(true)
		rows, err := read(f.store)
		// A cached List comes back in map order, so compare the row set.
		return fmt.Sprintf("rows=%v err=%v backing=%q", slices.Sorted(slices.Values(ids(rows))), err, f.backing.readLog())
	}
	for name, build := range fixtures {
		for read, want := range direct {
			if got, want := observe(t, build, seam[read]), observe(t, build, want); got != want {
				t.Errorf("%s: legacyDemandReads.%s\n got  %s\n want %s", name, read, got, want)
			}
		}
		if got, want := (legacyDemandReads{}).ReadyLimit(cfg), cfg.Daemon.MaxWakesPerTickOrDefault(); got != want {
			t.Errorf("legacyDemandReads.ReadyLimit(cfg) = %d, want max_wakes_per_tick %d", got, want)
		}
		if got := (legacyDemandReads{}).ReadyLimit(nil); got != config.DefaultMaxWakesPerTick {
			t.Errorf("legacyDemandReads.ReadyLimit(nil) = %d, want the default %d", got, config.DefaultMaxWakesPerTick)
		}

		collect := func(newCache func() *readyDemandCache, reads demandReads) (string, []string) {
			f := build(t)
			f.backing.armed.Store(true)
			run := runDemandCollectors(cfg, f.store, newCache, reads)
			return fmt.Sprintf("%+v", run), f.backing.readLog()
		}
		nilRun, nilLog := collect(newReadyDemandCache, nil)
		legacyRun, legacyLog := collect(func() *readyDemandCache { return newReadyDemandCacheWithReads(legacyDemandReads{}) }, legacyDemandReads{})
		if nilRun != legacyRun || !slices.Equal(nilLog, legacyLog) {
			t.Errorf("%s: collectors read differently through legacyDemandReads than through nil reads\n nil    %s %q\n legacy %s %q", name, nilRun, nilLog, legacyRun, legacyLog)
		}
		if want := legacyCollectorBackingReads[name]; !slices.Equal(nilLog, want) {
			t.Errorf("%s: the collectors' backing reads changed\n got  %q\n want %q", name, nilLog, want)
		}
	}
}

// legacyCollectorBackingReads pins, per fixture, the backing reads the four
// collectors make on origin/main (collectAssignedWorkBeadsWithStores,
// collectOpenUnassignedRoutedWork, defaultScaleCheckCountsAndDemand,
// defaultNamedSessionDemand, in that order): the live raw-status open List,
// the cached-tier Lists (served by a clean cache, read through a plain store,
// falling back live past a dirty row), and one unfiltered Ready per cache.
var legacyCollectorBackingReads = func() map[string][]string {
	const ready = "Ready [{Assignee: Limit:0 TierMode:2}]"
	plain := []string{"List status=in_progress live=false", "List status=open live=true", "List status=open live=false", ready, "List status=open live=true", ready}
	cached := []string{"List status=open live=true", ready, "List status=open live=true", ready}
	return map[string][]string{
		"plain":              plain,
		"plain live outage":  plain,
		"cached":             cached,
		"cached live outage": cached,
		"cached dirty":       {"List status=in_progress live=true", "List status=open live=true", "List status=open live=true", ready, "List status=open live=true", ready},
	}
}()

// Kills: a strict refusal that blanks a cached exact leg. Clean, all four
// collectors leave the backing untouched; with a dirty row whose stored copy
// moved on, they still count the leg's demand, serve the row's current
// value, and touch only the local backing: the overlay's per-row refresh,
// after which the strict Ready serves. Neither pass reads partial, including
// the closed named-session index an on_demand named session asks for.
func TestV2DemandCachedLegDirtyRowServes(t *testing.T) {
	cfg := demandReadsTestConfig()
	// An on_demand named session makes the assigned-work collector consult
	// the closed named-session index, which legacy reads live
	// (IncludeClosed); v2 must take it from the recording.
	cfg.NamedSessions = []config.NamedSession{{Name: "mayor", Template: "worker", Mode: "on_demand"}}
	cache, backing := newDemandCache(t, true,
		routedDemandBead("gc-r1"),
		assignedDemandBead("gc-p1", "in_progress"),
		assignedWorkflowRoot("gc-root", "worker-2"),
	)
	backing.armed.Store(true)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

	for _, step := range []struct {
		name  string
		dirty bool
		reads []string
	}{
		{name: "clean cache"},
		{name: "dirty row", dirty: true, reads: []string{"Get gc-r1", "DepList gc-r1"}},
	} {
		if step.dirty {
			dirtyDemandRow(t, cache, backing, "gc-r1")
			if err := backing.Store.SetMetadataBatch("gc-r1", map[string]string{"demand.test": "stored"}); err != nil {
				t.Fatal(err)
			}
			backing.ops = nil
		}
		reads := newV2DemandReads(now, nil)
		run := runDemandCollectors(cfg, cache, func() *readyDemandCache { return newReadyDemandCacheWithReads(reads) }, reads)
		if run.AssignedPartial || run.RoutedPartial || len(run.ScalePartials) > 0 || len(run.NamedPartials) > 0 {
			t.Errorf("%s: an exact leg read partial: %+v", step.name, run)
		}
		if !slices.Equal(run.ReadyAssigned, []string{"gc-p1", "gc-root"}) {
			t.Errorf("%s: ready assigned work = %v, want [gc-p1 gc-root]", step.name, run.ReadyAssigned)
		}
		if !slices.Equal(run.Routed, []string{"gc-r1"}) || run.ScaleCounts["worker"] != 1 {
			t.Errorf("%s: routed = %v, worker demand = %d, want [gc-r1] and 1", step.name, run.Routed, run.ScaleCounts["worker"])
		}
		if ops := backing.readLog(); !slices.Equal(ops, step.reads) {
			t.Errorf("%s: backing reads %q, want %q", step.name, ops, step.reads)
		}
		if step.dirty {
			rows, err := reads.RawOpen(cache)
			if i := slices.IndexFunc(rows, func(b beads.Bead) bool { return b.ID == "gc-r1" }); err != nil || i < 0 || rows[i].Metadata["demand.test"] != "stored" {
				t.Errorf("%s: RawOpen = %v, %v; want gc-r1 at its stored value", step.name, rows, err)
			}
		}
	}

	// Ready alone on a dirty row: the strict read refuses, and Ready reads
	// the local backing instead of reading partial.
	dirtyDemandRow(t, cache, backing, "gc-r1")
	backing.ops = nil
	if rows, err := newV2DemandReads(now, nil).ReadyAll(cache); err != nil || !slices.Contains(ids(rows), "gc-r1") {
		t.Errorf("dirty row: ReadyAll = %v, %v; want gc-r1", ids(rows), err)
	}
	if ops, want := backing.readLog(), []string{"Ready [{Assignee: Limit:0 TierMode:2}]"}; !slices.Equal(ops, want) {
		t.Errorf("dirty row: ReadyAll backing reads %q, want %q", ops, want)
	}
}

// Kills: a live read inside the pass (C0.4), and bd-leg demand read from the
// cache (EB-42o8). The cache holds a blocked bead folded to "open" — a routed
// row and an assigned workflow root — that the leg's live raw-status read
// excluded; v2 must serve the recording, not the cache. A dirty row never
// sends the leg to its backing: the live reads still come from the recording
// and the strict cached List reads partial.
func TestV2DemandLaneLegReadsRecordingOnly(t *testing.T) {
	cfg := demandReadsTestConfig()
	cache, backing := newDemandCache(t, false, routedDemandBead("gc-blocked"), routedDemandBead("gc-open"), assignedWorkflowRoot("gc-root", "worker-dead"))
	backing.armed.Store(true)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	open := routedDemandBead("gc-open")
	rec := &externalReadsRecording{Seq: 1, FreshFor: time.Minute, Sources: map[sourceKey]sourceResult{
		keyOf(sourceDemand, cache): {EndedAt: now, sourcePayload: sourcePayload{Leg: legRecording{RawOpen: []beads.Bead{open}, ReadyAll: []beads.Bead{open}}}},
	}}
	reads := newV2DemandReads(now, rec)

	run := runDemandCollectors(cfg, cache, func() *readyDemandCache { return newReadyDemandCacheWithReads(reads) }, reads)

	if !slices.Equal(run.Routed, []string{"gc-open"}) {
		t.Errorf("unassigned routed work = %v, want only gc-open: the folded blocked row came from the cache", run.Routed)
	}
	if run.ScaleCounts["worker"] != 1 {
		t.Errorf("worker demand = %d, want 1: the cache's folded Ready was counted", run.ScaleCounts["worker"])
	}
	if run.ReadyAssignedSet["gc-root"] {
		t.Errorf("the blocked assigned workflow root counted as ready assigned work: %v", run.ReadyAssigned)
	}
	if run.AssignedPartial || run.RoutedPartial || len(run.ScalePartials) > 0 {
		t.Errorf("a fresh recording read partial: %+v", run)
	}
	if ops := backing.readLog(); len(ops) > 0 {
		t.Errorf("v2 demand reads touched a non-exact leg's backing: %q", ops)
	}

	// The recorded live Ready failed: the leg's templates read partial with
	// no demand, never topped up from the cache's folded rows.
	rec.Sources[keyOf(sourceDemand, cache)] = sourceResult{EndedAt: now, sourcePayload: sourcePayload{Leg: legRecording{RawOpen: []beads.Bead{open}, ReadyAllErr: errDemandBackingDown}}}
	reads = newV2DemandReads(now, rec)
	targets := []defaultScaleCheckTarget{{template: "worker", storeKey: "city", store: cache}}
	counts, _, partials, _ := defaultScaleCheckCountsAndDemand(cfg, targets, newReadyDemandCacheWithReads(reads))
	if counts["worker"] != 0 || !partials["worker"] {
		t.Errorf("failed recorded Ready: worker demand = %d, partials = %v; want 0 and worker partial", counts["worker"], partials)
	}

	rec.Sources[keyOf(sourceDemand, cache)] = sourceResult{EndedAt: now, sourcePayload: sourcePayload{Leg: legRecording{RawOpen: []beads.Bead{open}, ReadyAll: []beads.Bead{open}}}}
	dirtyDemandRow(t, cache, backing, "gc-open")
	backing.ops = nil
	reads = newV2DemandReads(now, rec)
	if rows, err := reads.RawOpen(cache); err != nil || !slices.Equal(ids(rows), []string{"gc-open"}) {
		t.Errorf("dirty lane leg: RawOpen = %v, %v; want the recorded gc-open", ids(rows), err)
	}
	if rows, err := reads.ReadyAll(cache); err != nil || !slices.Equal(ids(rows), []string{"gc-open"}) {
		t.Errorf("dirty lane leg: ReadyAll = %v, %v; want the recorded gc-open", ids(rows), err)
	}
	if rows, err := reads.Cached(cache, beads.ListQuery{Status: "in_progress"}); len(rows) != 0 || !beads.IsPartialResult(err) || !errors.Is(err, beads.ErrCacheUnavailable) {
		t.Errorf("dirty lane leg: Cached = %v, %v; want no rows and a partial cache-unavailable error", ids(rows), err)
	}
	if ops := backing.readLog(); len(ops) > 0 {
		t.Errorf("v2 demand reads touched a dirty lane leg's backing: %q", ops)
	}
}

// Kills: a missing or stale recording read as zero demand. Every collector
// must report the leg partial instead; a fresh recording is the control.
func TestV2DemandReadsMissingOrStaleRecordingIsPartial(t *testing.T) {
	cfg := demandReadsTestConfig()
	cache, _ := newDemandCache(t, false, routedDemandBead("gc-r1"))
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	const maxAge = 30 * time.Second
	recordedAt := func(at time.Time) *externalReadsRecording {
		r1 := routedDemandBead("gc-r1")
		return &externalReadsRecording{Seq: 1, FreshFor: maxAge, Sources: map[sourceKey]sourceResult{
			keyOf(sourceDemand, cache): {EndedAt: at, sourcePayload: sourcePayload{Leg: legRecording{RawOpen: []beads.Bead{r1}, ReadyAll: []beads.Bead{r1}}}},
		}}
	}
	for _, tc := range []struct {
		name    string
		rec     *externalReadsRecording
		partial bool
	}{
		{name: "fresh recording", rec: recordedAt(now.Add(-maxAge))},
		{name: "no recording", rec: nil, partial: true},
		{name: "leg not recorded", rec: &externalReadsRecording{Seq: 1, FreshFor: maxAge, Sources: map[sourceKey]sourceResult{}}, partial: true},
		{name: "stale recording", rec: recordedAt(now.Add(-maxAge - time.Nanosecond)), partial: true},
	} {
		reads := newV2DemandReads(now, tc.rec)
		run := runDemandCollectors(cfg, cache, func() *readyDemandCache { return newReadyDemandCacheWithReads(reads) }, reads)
		if run.AssignedPartial != tc.partial || run.RoutedPartial != tc.partial || run.ScalePartials["worker"] != tc.partial || run.NamedPartials["worker"] != tc.partial {
			t.Errorf("%s: partial assigned=%t routed=%t scale=%t named=%t, want all %t",
				tc.name, run.AssignedPartial, run.RoutedPartial, run.ScalePartials["worker"], run.NamedPartials["worker"], tc.partial)
		}
		if wantCount := map[bool]int{false: 1, true: 0}[tc.partial]; run.ScaleCounts["worker"] != wantCount {
			t.Errorf("%s: worker demand = %d, want %d", tc.name, run.ScaleCounts["worker"], wantCount)
		}
	}
}

// Kills: max_wakes_per_tick truncating the assigned Ready set (BEHAVIORS #31
// F). With a budget of 1 and three ready assigned beads, legacy keeps one;
// v2 must keep all three.
func TestV2DemandReadsNoReadyTruncation(t *testing.T) {
	cfg := demandReadsTestConfig()
	cfg.Daemon.MaxWakesPerTick = intPtr(1)
	cache, _ := newDemandCache(t, true,
		assignedDemandBead("gc-a1", "open"),
		assignedDemandBead("gc-a2", "open"),
		assignedDemandBead("gc-a3", "open"),
	)
	readyAssigned := func(c *readyDemandCache) []string {
		_, _, _, ready, _ := collectAssignedWorkBeadsWithStores("", cfg, cache, nil, nil, nil, c)
		var out []string
		for k := range ready {
			out = append(out, k.ID)
		}
		slices.Sort(out)
		return out
	}

	if got := readyAssigned(newReadyDemandCache()); len(got) != 1 {
		t.Fatalf("legacy ready assigned work = %v, want 1 row: the fixture must exercise the wake budget", got)
	}
	reads := newV2DemandReads(time.Now(), nil)
	if got, want := readyAssigned(newReadyDemandCacheWithReads(reads)), []string{"gc-a1", "gc-a2", "gc-a3"}; !slices.Equal(got, want) {
		t.Errorf("v2 ready assigned work = %v, want %v", got, want)
	}
}

// Kills: a wrong leg classification. Exactness comes from the cache's
// backing declaration, through the bead-policy front door; a store with no
// cache is never exact.
func TestDemandLegCacheClassifiesExactLegs(t *testing.T) {
	opened, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	sqlite := opened.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = sqlite.CloseStore() })
	sqliteCache := beads.NewCachingStoreForTest(sqlite, nil)
	memCache := beads.NewCachingStoreForTest(beads.NewMemStore(), nil)
	for _, tc := range []struct {
		name      string
		store     beads.Store
		wantCache *beads.CachingStore
		exact     bool
	}{
		{"cache over SQLite", sqliteCache, sqliteCache, true},
		{"policy front door over a SQLite cache", wrapStoreWithBeadPolicies(sqliteCache, demandReadsTestConfig()), sqliteCache, true},
		{"cache over MemStore", memCache, memCache, false},
		{"bare SQLite", sqlite, nil, false},
		{"bare MemStore", beads.NewMemStore(), nil, false},
	} {
		cache, exact := demandLegCache(tc.store)
		if cache != tc.wantCache || exact != tc.exact {
			t.Errorf("%s: demandLegCache = (%p, %t), want (%p, %t)", tc.name, cache, exact, tc.wantCache, tc.exact)
		}
	}
}

// falseDeclarer is a backing that carries the declaration and answers false.
type falseDeclarer struct{ *demandBacking }

func (falseDeclarer) CachedReadExact() bool { return false }

// Kills: a declarer counted exact for carrying the method (M8), and a
// wrapper reporting its engine's declaration (L1). A cache over the CLI
// emitter over SQLite cannot see SQLite's unexported ready projection, so a
// foreign blocker that SQLite's live Ready honors is invisible to the
// cache's strict Ready: the leg must read through the lane.
func TestDemandLegCacheNotExactForFalseDeclarerOrWrapper(t *testing.T) {
	opened, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	sqlite := opened.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = sqlite.CloseStore() })
	blocked, err := sqlite.Create(beads.Bead{Title: "blocked by foreign", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlite.DepAdd(blocked.ID, "zzforeign-1", "blocks"); err != nil {
		t.Fatal(err)
	}
	overEmitter := beads.NewCachingStoreForTest(&emittingClassStore{Store: sqlite, cityPath: t.TempDir()}, nil)
	if err := overEmitter.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	live, _ := sqlite.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
	cached, err := overEmitter.ReadyContext(context.Background(), beads.ReadyQuery{TierMode: beads.TierBoth})
	t.Logf("SQLite live Ready %d rows, cache over the emitter %d rows (err %v)", len(live), len(cached), err)

	for name, store := range map[string]beads.Store{
		"cache over the emitter over SQLite": overEmitter,
		"cache over a false declarer":        beads.NewCachingStoreForTest(falseDeclarer{newDemandBacking()}, nil),
	} {
		if cache, exact := demandLegCache(store); cache == nil || exact {
			t.Errorf("%s: demandLegCache = (%p, %t), want the cache, not exact", name, cache, exact)
		}
	}
}

// Kills: a v2 cached-tier read of a leg with no cache falling back to a live
// read (M38). The read is partial, every collector reports the leg partial,
// and the store is never touched.
func TestV2DemandReadsCachedWithoutCacheIsPartial(t *testing.T) {
	cfg := demandReadsTestConfig()
	backing := newDemandBacking(routedDemandBead("gc-r1"), assignedDemandBead("gc-p1", "in_progress"))
	backing.armed.Store(true)
	reads := newV2DemandReads(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC), nil)
	if rows, err := reads.Cached(backing, beads.ListQuery{Status: "in_progress"}); len(rows) != 0 || !beads.IsPartialResult(err) || !errors.Is(err, errDemandLegUncached) {
		t.Errorf("Cached on an uncached leg = %v, %v; want no rows and a partial uncached error", ids(rows), err)
	}
	run := runDemandCollectors(cfg, backing, func() *readyDemandCache { return newReadyDemandCacheWithReads(reads) }, reads)
	if !run.AssignedPartial || len(run.Assigned) != 0 {
		t.Errorf("assigned work over an uncached leg = %v partial=%t, want none and partial", run.Assigned, run.AssignedPartial)
	}
	if ops := backing.readLog(); len(ops) > 0 {
		t.Errorf("v2 reads touched an uncached leg: %q", ops)
	}
}

// Kills: a v2 closed named-session index read from the store, served past
// the recording's expiry, or served for a store the recording does not hold;
// and the legacy method diverging from the direct call.
func TestClosedNamedIndexLegacyIsDirectCallV2ServesRecording(t *testing.T) {
	backing := newDemandBacking(closedNamedSessionBead("gc-closed", "mayor"))
	backing.armed.Store(true)
	direct, directErr := session.BuildClosedNamedSessionBeadIndex(backing)
	directLog := backing.readLog()
	backing.ops = nil
	legacy, legacyErr := legacyDemandReads{}.ClosedNamedIndex(backing)
	if !reflect.DeepEqual(legacy, direct) || errorText(legacyErr) != errorText(directErr) || !slices.Equal(backing.readLog(), directLog) {
		t.Errorf("legacy ClosedNamedIndex = %+v %v reads %q, direct %+v %v reads %q", legacy, legacyErr, backing.readLog(), direct, directErr, directLog)
	}
	backing.ops = nil

	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	rec := &externalReadsRecording{FreshFor: time.Minute, Sources: map[sourceKey]sourceResult{keyOf(sourceClosedNamed, backing): {EndedAt: now, sourcePayload: sourcePayload{ClosedNamed: direct}}}}
	// A partial read keeps the rows it got and its error, as legacy's does.
	partial := &beads.PartialResultError{Op: "closed index", Err: errors.New("one leg down")}
	partialRec := &externalReadsRecording{FreshFor: time.Minute, Sources: map[sourceKey]sourceResult{keyOf(sourceClosedNamed, backing): {EndedAt: now, sourcePayload: sourcePayload{ClosedNamed: direct}, Err: partial}}}
	for _, tc := range []struct {
		name    string
		now     time.Time
		rec     *externalReadsRecording
		wantErr error
		found   bool
	}{
		{"fresh", now.Add(time.Minute), rec, nil, true},
		{"recorded partial read", now, partialRec, partial, true},
		{"no recording", now, nil, errDemandRecordingMissing, false},
		{"store not recorded", now, &externalReadsRecording{FreshFor: time.Minute}, errDemandRecordingMissing, false},
		{"expired", now.Add(time.Minute + time.Nanosecond), rec, errDemandRecordingStale, false},
	} {
		idx, err := newV2DemandReads(tc.now, tc.rec).ClosedNamedIndex(backing)
		_, found := idx.Find("mayor")
		if !errors.Is(err, tc.wantErr) || found != tc.found {
			t.Errorf("%s: ClosedNamedIndex found mayor=%t err=%v, want found=%t err %v", tc.name, found, err, tc.found, tc.wantErr)
		}
	}
	if ops := backing.readLog(); len(ops) > 0 {
		t.Errorf("v2 ClosedNamedIndex read the store: %q", ops)
	}
}

// Kills: v2 diverging from legacy on a clean exact leg. Over a rich SQLite
// fixture (blocked, foreign-blocked, parent-blocked, deferred, ephemeral and
// no-history rows, assigned work and workflow roots), the four collectors
// read the same through v2 as through legacy's live reads, with #31's Ready
// limit neutralized.
func TestV2DemandReadsExactLegMatchesLegacyOnSQLite(t *testing.T) {
	opened, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sqlite := opened.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = sqlite.CloseStore() })
	mk := func(b beads.Bead) beads.Bead {
		t.Helper()
		got, err := sqlite.Create(b)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	dep := func(id, on, kind string) {
		t.Helper()
		if err := sqlite.DepAdd(id, on, kind); err != nil {
			t.Fatal(err)
		}
	}
	routed := func(title string) beads.Bead {
		return beads.Bead{Title: title, Type: "task", Status: "open", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}}
	}
	blocker := mk(beads.Bead{Title: "blocker", Type: "task", Status: "open"})
	mk(routed("ready routed"))
	dep(mk(routed("blocked routed")).ID, blocker.ID, "blocks")
	dep(mk(routed("foreign-blocked routed")).ID, "zzforeign-9", "blocks")
	parent := mk(beads.Bead{Title: "parent", Type: "task", Status: "open"})
	dep(parent.ID, blocker.ID, "blocks")
	dep(mk(routed("child of blocked parent")).ID, parent.ID, "parent-child")
	future := time.Now().Add(24 * time.Hour)
	deferred := routed("deferred routed")
	deferred.DeferUntil = &future
	mk(deferred)
	mk(assignedDemandBead("", "in_progress"))
	mk(assignedDemandBead("", "open"))
	// Open, assigned and routed: the orphan-release pass's cached open List
	// captures it, which a memo shared with the in-progress List would lose.
	openRouted := assignedDemandBead("", "open")
	openRouted.Metadata = map[string]string{beadmeta.RoutedToMetadataKey: "worker"}
	dep(mk(openRouted).ID, blocker.ID, "blocks")
	mk(assignedWorkflowRoot("", "worker-2"))
	dep(mk(assignedDemandBead("", "open")).ID, blocker.ID, "blocks")
	eph := routed("ephemeral routed")
	eph.Ephemeral = true
	mk(eph)
	root := assignedWorkflowRoot("", "worker-3")
	root.Ephemeral = true
	mk(root)
	noHistory := routed("no-history routed")
	noHistory.NoHistory = true
	mk(noHistory)

	cfg := demandReadsTestConfig()
	cfg.Daemon.MaxWakesPerTick = intPtr(1000)
	cache := beads.NewCachingStoreForTest(sqlite, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	legacy := runDemandCollectors(cfg, cache, newReadyDemandCache, nil)
	reads := newV2DemandReads(time.Now(), nil)
	v2 := runDemandCollectors(cfg, cache, func() *readyDemandCache { return newReadyDemandCacheWithReads(reads) }, reads)
	if l, v := fmt.Sprintf("%+v", legacy), fmt.Sprintf("%+v", v2); l != v {
		t.Errorf("exact-leg v2 differs from legacy\n legacy %s\n v2     %s", l, v)
	}
	if len(legacy.Routed) == 0 || len(legacy.ReadyAssigned) == 0 {
		t.Errorf("the fixture counted no demand: %+v", legacy)
	}
}

// legacyControlRoutes runs legacy's in-tick control route repair over a copy
// of rows and returns the edited copy, the gaps, and what
// openControlDispatcherDemand counts from it.
func legacyControlRoutes(t *testing.T, cfg *config.City, rows []beads.Bead, stores []beads.Store, refs []string) ([]beads.Bead, []ControlDispatcherScopeGap, map[string]bool) {
	t.Helper()
	edited := cloneBeadRows(rows)
	gaps := repairControlDispatcherRoutesForStoreScope(t.Name(), cfg, edited, stores, refs, io.Discard)
	return edited, gaps, openControlDispatcherDemand(cfg, edited)
}

func cloneBeadRows(rows []beads.Bead) []beads.Bead {
	out := slices.Clone(rows)
	for i := range out {
		out[i].Metadata = maps.Clone(out[i].Metadata)
	}
	return out
}

// Kills: v2 counting control work legacy suppresses (a scope gap, #3765),
// and a projection that edits the rows it was given. On the gap-scope
// fixture legacy's demand is empty with one gap; v2's collection, projected,
// counts the same, reports the same gap, and leaves its input untouched.
func TestProjectControlDispatcherRoutesMatchesLegacyOnScopeGap(t *testing.T) {
	cfg := cityOnlyDispatcherFixtureConfig(t)
	seed := beads.Bead{ID: "gcg-ctl", Title: "ctl", Type: "task", Status: "open", Metadata: map[string]string{
		beadmeta.KindMetadataKey:         beadmeta.KindWorkflowFinalize,
		beadmeta.RoutedToMetadataKey:     cfg.Agents[0].QualifiedName(),
		beadmeta.RootStoreRefMetadataKey: "rig:fixture",
	}}
	cityPath := t.TempDir()
	binding, _ := newDemandCache(t, true, seed)
	routes := splitRoutes(binding)
	registerResidencyRoutes(cityPath, routes, func() beads.Store { return beads.NewMemStore() })
	t.Cleanup(func() { unregisterResidencyRoutes(cityPath, routes) })
	rigs := map[string]beads.Store{"fixture": beads.NewMemStore()}

	rows, stores, refs, _ := collectOpenUnassignedRoutedWork(cityPath, cfg, binding, rigs, nil, io.Discard, nil, nil)
	_, legacyGaps, legacyDemand := legacyControlRoutes(t, cfg, rows, stores, refs)
	if len(legacyDemand) != 0 || len(legacyGaps) != 1 {
		t.Fatalf("fixture: legacy demand %v with %d gaps, want none with 1", legacyDemand, len(legacyGaps))
	}

	reads := newV2DemandReads(time.Now(), nil)
	v2Rows, _, v2Refs, _ := collectOpenUnassignedRoutedWork(cityPath, cfg, binding, rigs, nil, io.Discard, nil, reads)
	before := cloneBeadRows(v2Rows)
	if got := openControlDispatcherDemand(cfg, v2Rows); len(got) == 0 {
		t.Fatalf("fixture: v2's unprojected rows count no demand (%v); the projection would prove nothing", got)
	}
	projected, gaps := projectControlDispatcherRoutes(cfg, v2Rows, v2Refs)
	if got := openControlDispatcherDemand(cfg, projected); !reflect.DeepEqual(got, legacyDemand) {
		t.Errorf("projected demand = %v, want legacy's %v", got, legacyDemand)
	}
	if !reflect.DeepEqual(gaps, legacyGaps) {
		t.Errorf("projected gaps = %+v, want legacy's %+v", gaps, legacyGaps)
	}
	if !reflect.DeepEqual(v2Rows, before) {
		t.Error("the projection edited its input rows")
	}
}

// failingUpdateStore refuses every Update, so legacy's repair defers.
type failingUpdateStore struct{ beads.Store }

func (failingUpdateStore) Update(string, beads.UpdateOpts) error { return errDemandBackingDown }

// Kills: a projection that diverges from legacy's in-tick rows with no write
// landing (the lane owns the writes), row by row: a route awaiting repair in
// a scope with a dispatcher is dropped, a fallback-only cleanup keeps its
// route, an unscoped control row and a non-control row are untouched, and a
// misaligned input suppresses every control route with no gap.
func TestProjectControlDispatcherRoutesMatchesLegacyDeferredRows(t *testing.T) {
	cfg := classBindingDispatcherFixtureConfig(t)
	cityRoute, rigRoute := cfg.Agents[0].QualifiedName(), cfg.Agents[1].QualifiedName()
	control := func(id, route, root string, extra ...string) beads.Bead {
		md := map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflowFinalize, beadmeta.RoutedToMetadataKey: route}
		if root != "" {
			md[beadmeta.RootStoreRefMetadataKey] = root
		}
		for i := 0; i+1 < len(extra); i += 2 {
			md[extra[i]] = extra[i+1]
		}
		return beads.Bead{ID: id, Type: "task", Status: "open", Metadata: md}
	}
	rows := []beads.Bead{
		control("ctl-repair", cityRoute, "rig:fixture"),
		control("ctl-fallback", rigRoute, "rig:fixture", beadmeta.ControlDispatcherFallbackMetadataKey, "true"),
		control("ctl-unscoped", cityRoute, ""),
		routedDemandBead("work-1"),
	}
	store := failingUpdateStore{beads.NewMemStore()}
	stores := []beads.Store{store, store, store, store}
	refs := []string{"rig:fixture", "rig:fixture", "", ""}

	legacyRows, legacyGaps, legacyDemand := legacyControlRoutes(t, cfg, rows, stores, refs)
	projected, gaps := projectControlDispatcherRoutes(cfg, rows, refs)
	routesOf := func(rows []beads.Bead) map[string]string {
		out := make(map[string]string)
		for _, b := range rows {
			out[b.ID] = b.Metadata[beadmeta.RoutedToMetadataKey]
		}
		return out
	}
	if got, want := routesOf(projected), routesOf(legacyRows); !reflect.DeepEqual(got, want) {
		t.Errorf("projected routes %v, legacy's deferred in-tick routes %v", got, want)
	}
	if routesOf(projected)["ctl-repair"] != "" || routesOf(projected)["ctl-fallback"] != rigRoute {
		t.Errorf("projected routes %v: want ctl-repair dropped and ctl-fallback kept", routesOf(projected))
	}
	if got := openControlDispatcherDemand(cfg, projected); !reflect.DeepEqual(got, legacyDemand) || len(gaps) != 0 || len(legacyGaps) != 0 {
		t.Errorf("projected demand %v gaps %v, legacy %v gaps %v", got, gaps, legacyDemand, legacyGaps)
	}
	if &projected[3] == &rows[3] || !reflect.DeepEqual(projected[3], rows[3]) {
		t.Error("the projection did not copy on write, or changed a non-control row")
	}

	misaligned, gaps := projectControlDispatcherRoutes(cfg, rows, refs[:1])
	if r := routesOf(misaligned); r["ctl-repair"] != "" || r["ctl-fallback"] != "" || r["ctl-unscoped"] != "" || r["work-1"] != "worker" || gaps != nil {
		t.Errorf("misaligned input: routes %v gaps %v, want every control route dropped and no gap", r, gaps)
	}
	if rows[0].Metadata[beadmeta.RoutedToMetadataKey] != cityRoute {
		t.Error("the projection edited its input rows")
	}
}
