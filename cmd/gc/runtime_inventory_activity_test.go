package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// activityLeaf scripts GetLastActivity per name: an answer, an error, or a
// read that blocks until release closes.
type activityLeaf struct {
	*runtime.Fake
	mu      sync.Mutex
	at      map[string]time.Time
	err     map[string]error
	block   map[string]bool
	release chan struct{}
	reads   map[string]int
}

func (l *activityLeaf) GetLastActivity(name string) (time.Time, error) {
	l.mu.Lock()
	l.reads[name]++
	at, err, block := l.at[name], l.err[name], l.block[name]
	l.mu.Unlock()
	if block {
		<-l.release
	}
	return at, err
}

func newActivityLeaf(t *testing.T) *activityLeaf {
	l := &activityLeaf{Fake: runtime.NewFake(), at: map[string]time.Time{}, err: map[string]error{}, block: map[string]bool{}, release: make(chan struct{}), reads: map[string]int{}}
	t.Cleanup(func() { close(l.release) })
	return l
}

// TestReadActivitiesKnownOnlyOnACleanRead (C6a2b). Kills an activity fact
// that reads idle where nothing was proved: only an error-free, non-zero
// answer is known; an error, a zero time, or a leaf that cannot report
// activity stays unknown, and AL1 never sleeps a live row on unknown
// activity.
func TestReadActivitiesKnownOnlyOnACleanRead(t *testing.T) {
	at := gatherNow.Add(-time.Hour)
	leaf := newActivityLeaf(t)
	leaf.at["a"], leaf.at["b"], leaf.err["b"] = at, at, errors.New("probe failed")
	attrs := map[string]InventoryAttrs{"a": {}, "b": {}, "c": {}}
	hosts := map[string]runtime.Provider{"a": leaf, "b": leaf, "c": leaf}
	reads := readActivities(context.Background(), attrs, hosts, 4)
	if got := attrs["a"]; !got.ActivityKnown || !got.LastActivity.Equal(at) {
		t.Fatalf("a clean read: %+v, want known at %v", got, at)
	}
	for _, name := range []string{"b", "c"} {
		if attrs[name].ActivityKnown {
			t.Errorf("%s: activity known, want unknown", name)
		}
	}
	if reads != 3 {
		t.Fatalf("reads %d (%v), want 3", reads, leaf.reads)
	}
	if n := readActivities(context.Background(), map[string]InventoryAttrs{"x": {}}, map[string]runtime.Provider{"x": unreportable{leaf.Fake}}, 4); n != 0 {
		t.Fatalf("an unreportable leaf was read %d times, want 0", n)
	}
}

// TestLaneReadsActivityOnALeafWithoutAnInventory (C6a2b). Kills an activity
// read the lane never makes on a leaf without a batched inventory (acp,
// subprocess), whose names enrich leaves without pane liveness: one real
// inventory pass reads every listed name there and publishes its clean
// answer.
func TestLaneReadsActivityOnALeafWithoutAnInventory(t *testing.T) {
	at := gatherNow.Add(-time.Hour)
	leaf := newActivityLeaf(t)
	for _, name := range []string{"s", "t"} {
		if err := leaf.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		leaf.at[name] = at
	}
	if _, batched := runtime.Provider(leaf).(runtime.InventoryProvider); batched || !sessionActivityReportable(leaf, "s") {
		t.Fatal("fixture: want a leaf without an inventory that reports activity")
	}
	cr := inventoryLaneTestRuntime(t, leaf, nil)
	runTestInventoryPass(cr)
	snap := cr.inventoryLane.cache.Snapshot()
	for _, name := range []string{"s", "t"} {
		if obs := snap.ByName[name]; !obs.LastActivityKnown || !obs.LastActivity.Equal(at) {
			t.Fatalf("%s: published activity %v (known %v), want %v; reads %v", name, obs.LastActivity, obs.LastActivityKnown, at, leaf.reads)
		}
	}
}

// TestReadActivitiesStartsNoneAfterTheBound (C6a2b). Kills reads spawned
// past the pass's bound: a done ctx starts no read.
func TestReadActivitiesStartsNoneAfterTheBound(t *testing.T) {
	leaf := newActivityLeaf(t)
	leaf.at["a"] = gatherNow
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	attrs := map[string]InventoryAttrs{"a": {}}
	if n := readActivities(ctx, attrs, map[string]runtime.Provider{"a": leaf}, 4); n != 0 || leaf.reads["a"] != 0 || attrs["a"].ActivityKnown {
		t.Fatalf("a done ctx: %d reads (%v), attrs %+v, want none", n, leaf.reads, attrs["a"])
	}
}

// unreportable is a leaf whose sleep capability is disabled, so it cannot
// report activity (sessionActivityReportable).
type unreportable struct{ *runtime.Fake }

func (unreportable) SleepCapability(string) runtime.SessionSleepCapability {
	return runtime.SessionSleepCapabilityDisabled
}

// TestReadActivitiesIsBounded (C6a2b). Kills a pass held by a wedged
// activity read: the read past the pass's bound is unknown, and the others
// are read.
func TestReadActivitiesIsBounded(t *testing.T) {
	leaf := newActivityLeaf(t)
	leaf.at["a"], leaf.block["w"] = gatherNow, true
	attrs := map[string]InventoryAttrs{"a": {}, "w": {}}
	hosts := map[string]runtime.Provider{"a": leaf, "w": leaf}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { readActivities(ctx, attrs, hosts, 2); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a wedged activity read held the pass past its bound")
	}
	if attrs["w"].ActivityKnown || !attrs["a"].ActivityKnown {
		t.Fatalf("attrs %+v, want the wedged read unknown and the other known", attrs)
	}
}

// TestActivityNeverCarriesOver (C6a2b). Kills a stale activity fact: a pass
// that did not read a name's activity, or did not enrich it, leaves it unknown, never the last
// pass's time, so a missed read cannot make a row look idle.
func TestActivityNeverCarriesOver(t *testing.T) {
	c := newObserveCache()
	at := censusNow.Add(-time.Hour)
	first := c.publish(censusNow, map[string]InventoryAttrs{"s": {DeadKnown: true, ActivityKnown: true, LastActivity: at}}, completeBackend("tmux", "s"))
	if obs := first.ByName["s"]; !obs.LastActivityKnown || !obs.LastActivity.Equal(at) {
		t.Fatalf("first pass: %+v, want activity known at %v", obs, at)
	}
	second := c.publish(censusNow.Add(time.Second), map[string]InventoryAttrs{"s": {DeadKnown: true}}, completeBackend("tmux", "s"))
	if obs := second.ByName["s"]; obs.LastActivityKnown || !obs.LastActivity.IsZero() {
		t.Fatalf("second pass with no read: %+v, want activity unknown", obs)
	}
	// Listed but not enriched (its inventory failed or omitted it): the
	// other facts keep, the activity does not.
	c.publish(censusNow.Add(2*time.Second), map[string]InventoryAttrs{"s": {DeadKnown: true, ActivityKnown: true, LastActivity: at}}, completeBackend("tmux", "s"))
	unenriched := c.publish(censusNow.Add(3*time.Second), map[string]InventoryAttrs{}, completeBackend("tmux", "s"))
	if obs := unenriched.ByName["s"]; obs.LastActivityKnown || !obs.LastActivity.IsZero() {
		t.Fatalf("a listed, unenriched pass: %+v, want activity unknown", obs)
	}
}

// TestInventoryActivityIsTheBatchedRead (C6a2b, cost). Kills a per-name
// activity read on a leaf whose batched inventory carries it (tmux, where
// GetLastActivity refreshes the fleet snapshot, a full process scan): the
// lane's enrichment takes the inventory's activity, and readActivities reads
// nothing there.
func TestInventoryActivityIsTheBatchedRead(t *testing.T) {
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "s", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	at := gatherNow.Add(-time.Hour)
	fake.SetActivity("s", at)
	leaf := runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: fake}) // reports activity, as tmux does
	if !sessionActivityReportable(leaf, "s") {
		t.Fatal("fixture: the tmux-profile leaf does not report activity")
	}
	l := &runtimeInventoryLane{}
	listing := []inventoryBackend{{label: "tmux", provider: leaf, names: []string{"s"}}}
	attrs, hosts, errs := l.enrich(context.Background(), listing, []BackendPass{completeBackend("tmux", "s")})
	if len(errs) != 0 || !attrs["s"].ActivityKnown || !attrs["s"].LastActivity.Equal(at) {
		t.Fatalf("enrich = %+v (%v), want the inventory's activity %v", attrs["s"], errs, at)
	}
	// A live pane, which the fake's inventory does not report: the read
	// is skipped for the batched leaf, not for its pane.
	a := attrs["s"]
	a.DeadKnown = true
	attrs["s"] = a
	before := fake.CountCalls("GetLastActivity", "s")
	if n := readActivities(context.Background(), attrs, hosts, 4); n != 0 || fake.CountCalls("GetLastActivity", "s") != before {
		t.Fatalf("readActivities read %d names on a batched leaf, want none", n)
	}
}

// contentLeaf scripts SnapshotIdle per name: idle, an error, or a read
// that blocks until its test ends; it panics on a name it was not given.
type contentLeaf struct {
	*runtime.Fake
	idle  map[string]bool
	err   map[string]error
	block map[string]bool
	done  chan struct{}
}

func (l *contentLeaf) SnapshotIdle(name string) (bool, error) {
	if l.block[name] {
		<-l.done
	}
	idle, ok := l.idle[name]
	if !ok && l.err[name] == nil {
		panic("unscripted " + name)
	}
	return idle, l.err[name]
}

// Kills a content read that lies or overreaches (C7a-1, ga-07mi8): the lane
// reads only the wanted, enriched names on a leaf that can snapshot them;
// idle, busy, indeterminate and an error read as such, a panicking read is
// an error, and a read past the bound stays unread.
func TestReadContents(t *testing.T) {
	leaf := &contentLeaf{Fake: runtime.NewFake(), idle: map[string]bool{"idle": true, "busy": false}, err: map[string]error{"unsure": runtime.ErrIdleIndeterminate, "gone": errors.New("no pane")}, block: map[string]bool{"slow": true}, done: make(chan struct{})}
	t.Cleanup(func() { close(leaf.done) })
	names := []string{"idle", "busy", "unsure", "gone", "boom", "unwanted", "plain"}
	attrs, hosts := map[string]InventoryAttrs{}, map[string]runtime.Provider{}
	for _, n := range names {
		attrs[n], hosts[n] = InventoryAttrs{}, leaf
	}
	hosts["plain"] = runtime.NewFake() // no snapshot
	want := map[string]bool{"idle": true, "busy": true, "unsure": true, "gone": true, "boom": true, "plain": true, "unlisted": true}
	if n := readContents(context.Background(), attrs, hosts, want, 2); n != 5 {
		t.Errorf("reads %d, want the five wanted, enriched snapshot names", n)
	}
	for n, st := range map[string]contentState{"idle": contentIdle, "busy": contentBusy, "unsure": contentIndeterminate, "gone": contentError, "boom": contentError, "unwanted": contentUnread, "plain": contentUnread} {
		if attrs[n].Content != st {
			t.Errorf("%s: content %d, want %d", n, attrs[n].Content, st)
		}
	}
	if _, ok := attrs["unlisted"]; ok {
		t.Fatal("an unlisted name was read into the attrs")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	slow := map[string]InventoryAttrs{"slow": {Content: contentIdle}}
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		readContents(ctx, slow, map[string]runtime.Provider{"slow": leaf}, map[string]bool{"slow": true}, 1)
	}()
	select { // the read respects its context: it returns at the bound, the hung snapshot abandoned
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("readContents outlived its context on a hung snapshot")
	}
	if slow["slow"].Content != contentUnread {
		t.Fatalf("a read past the bound: %d, want unread", slow["slow"].Content)
	}
}

// snapshotProvider is a probing inventory leaf whose panes a snapshot reads
// after delay: idle, or err.
type snapshotProvider struct {
	*probingInventoryProvider
	delay time.Duration
	err   error
}

func (p snapshotProvider) SnapshotIdle(string) (bool, error) {
	<-time.After(p.delay)
	return p.err == nil, p.err
}

// Kills content reads with no share of the enrichment bound, or none at
// all (v5.9s): with the batched reads taking the bound less its reserve, a
// wanted pane's snapshot still reads within the reserve, and the lane
// meters it; a snapshot that never returns ends at the bound.
func TestLaneContentReadsHaveTheirReserve(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		probe := func(string, []string) (runtime.Liveness, error) { return runtime.Liveness{Running: true}, nil }
		sp := snapshotProvider{probingInventoryProvider: newProbingProvider(probe, "gc-a"), delay: inventoryEnrichBound / 8}
		cr := inventoryLaneTestRuntime(t, sp, nil)
		cr.inventoryLane.cache.WantContent(map[string]bool{"gc-a": true})
		runTestInventoryPass(cr)                                                                                       // reads the identity
		sp.inventoryHook = func(context.Context, int) { <-time.After(inventoryEnrichBound - inventoryEnrichBound/16) } // all but a sliver of the bound
		runTestInventoryPass(cr)
		if got := cr.inventoryLane.cache.Snapshot().ByName["gc-a"].Content; got != contentIdle {
			t.Fatalf("content %d after a slow inventory, want the reserved read's idle", got)
		}
		cr.inventoryLane.statusMu.Lock()
		fields := cr.inventoryLane.status.v2Fields
		cr.inventoryLane.statusMu.Unlock()
		if fields["content_reads"] != 1 || fields["content_read_ms"] == nil {
			t.Fatalf("lane fields %v, want the content read metered", fields)
		}
	})
}

// Kills a content read that outlives its pass (C7a-1): the cache carries a
// pass's content read onto the observation and forgets it on the next
// pass that lists it without reading it.
func TestObservationCarriesContent(t *testing.T) {
	c := newObserveCache()
	c.WantContent(map[string]bool{"s": true})
	if !c.contentWanted()["s"] {
		t.Fatal("WantContent not kept")
	}
	snap := c.publish(censusNow, map[string]InventoryAttrs{"s": {Content: contentIdle}}, completeBackend("tmux", "s"))
	if obs, _ := snap.Observation("s", censusNow, observeMaxAge); obs.Content != contentIdle {
		t.Fatalf("content %d, want idle", obs.Content)
	}
	snap = c.publish(censusNow.Add(time.Second), nil, completeBackend("tmux", "s")) // listed, not enriched
	if obs, _ := snap.Observation("s", censusNow.Add(time.Second), observeMaxAge); obs.Content != contentUnread {
		t.Fatalf("next pass: content %d, want unread", obs.Content)
	}
}
