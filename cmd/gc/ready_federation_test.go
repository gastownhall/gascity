package main

// Tests for how `gc ready` opens and reads its legs: concurrently, under a
// bound, over the serving frame only, and with one composed config load per
// invocation.
//
// cmd_ready_test.go pins the contracts these extend: first-leg-wins dedupe and
// the fail-loud rule at both ends (open and read). Both are stated in terms of
// leg POSITION, which is exactly what a naive concurrent reader that collected
// results off a completion-ordered channel would break while still passing
// every sequential test, so the tests here force legs to finish out of order.
//
// Overlap is measured inside testing/synctest bubbles: synctest.Wait returns
// only once every goroutine is parked, so "how many legs are in flight" is a
// count, not a timing. Completion order is forced with channels; no test here
// sleeps or bounds wall-clock time (TESTING.md: wait for facts).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// readyGatedStore parks every read on probe before delegating, so a synctest
// bubble can count how many legs are being read at once.
type readyGatedStore struct {
	beads.Store
	probe *fanOutProbe
}

func (s readyGatedStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	s.probe.call(0)
	return s.Store.Ready(query...)
}

func (s readyGatedStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.probe.call(0)
	return s.Store.List(query)
}

// completionOrder lets a test force one operation to finish strictly after
// another without timing either: the operation holding waitFor blocks until
// the one holding signal has finished.
type completionOrder struct {
	finished chan struct{}
	once     sync.Once
}

func newCompletionOrder() *completionOrder {
	return &completionOrder{finished: make(chan struct{})}
}

func (c *completionOrder) markFinished() { c.once.Do(func() { close(c.finished) }) }

// readyOrderedStore finishes its reads after the operation it waits for and
// then signals its own completion, optionally failing. Either order field may
// be nil.
type readyOrderedStore struct {
	beads.Store
	waitFor *completionOrder
	signal  *completionOrder
	err     error
}

func (s readyOrderedStore) read(fn func() ([]beads.Bead, error)) ([]beads.Bead, error) {
	if s.waitFor != nil {
		<-s.waitFor.finished
	}
	if s.signal != nil {
		defer s.signal.markFinished()
	}
	if s.err != nil {
		return nil, s.err
	}
	return fn()
}

func (s readyOrderedStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	return s.read(func() ([]beads.Bead, error) { return s.Store.Ready(query...) })
}

func (s readyOrderedStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	return s.read(func() ([]beads.Bead, error) { return s.Store.List(query) })
}

// gatedLegs builds n legs over empty stores, every read parked on probe.
func gatedLegs(n int, probe *fanOutProbe) []readyLeg {
	legs := make([]readyLeg, n)
	for i := range legs {
		legs[i] = readyTestLeg(fmt.Sprintf("leg-%d", i), readyGatedStore{Store: beads.NewMemStore(), probe: probe})
	}
	return legs
}

// TestFederateReadyBeadsReadsEveryLegAtOnce pins the read half of the latency
// fix: with fewer legs than the fan-out bound, every leg is being read at the
// same time, so a federated read costs the slowest leg rather than the sum.
func TestFederateReadyBeadsReadsEveryLegAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const legCount = 5
		probe := newFanOutProbe()
		defer probe.open()

		var err error
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, err = federateReadyBeads(gatedLegs(legCount, probe), beads.ReadyQuery{TierMode: beads.FederatedReadTier})
		}()
		synctest.Wait()
		if got := probe.running.Load(); got != legCount {
			t.Fatalf("%d of %d legs were being read at once, want all of them: a leg-after-leg reader makes one gc ready cost the sum of every store's latency", got, legCount)
		}
		probe.open()
		<-done
		if err != nil {
			t.Fatalf("federateReadyBeads: %v", err)
		}
	})
}

// TestFederateReadyBeadsBoundsTheLegFanOut pins the bound: a city with more
// legs than readyLegFanOut never has more than that many reads in flight.
func TestFederateReadyBeadsBoundsTheLegFanOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		legCount := readyLegFanOut + 3
		probe := newFanOutProbe()
		defer probe.open()

		var rows []beads.Bead
		var err error
		done := make(chan struct{})
		go func() {
			defer close(done)
			rows, err = federateReadyBeads(gatedLegs(legCount, probe), beads.ReadyQuery{TierMode: beads.FederatedReadTier})
		}()
		synctest.Wait()
		if got := probe.running.Load(); got != int64(readyLegFanOut) {
			t.Fatalf("%d legs being read at once over a %d-leg city, want exactly the bound %d", got, legCount, readyLegFanOut)
		}
		probe.open()
		<-done
		if err != nil || len(rows) != 0 {
			t.Fatalf("federateReadyBeads = %v, %v; want an empty answer from empty legs", rows, err)
		}
		if got := len(probe.sortedVisits()); got != legCount {
			t.Fatalf("%d of %d legs were read, want every leg exactly once", got, legCount)
		}
	})
}

// TestFederateListBeadsWithOwnerReadsEveryLegAtOnce is the same property for
// the crash-recovery arm, which restates the merge loop rather than sharing it.
func TestFederateListBeadsWithOwnerReadsEveryLegAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const legCount = 4
		probe := newFanOutProbe()
		defer probe.open()

		var err error
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _, err = federateListBeadsWithOwner(gatedLegs(legCount, probe), beads.ListQuery{Status: readyStatusInProgress, Live: true, TierMode: beads.FederatedReadTier})
		}()
		synctest.Wait()
		if got := probe.running.Load(); got != legCount {
			t.Fatalf("%d of %d legs were being listed at once, want all of them", got, legCount)
		}
		probe.open()
		<-done
		if err != nil {
			t.Fatalf("federateListBeadsWithOwner: %v", err)
		}
	})
}

// seedCoResidentPair puts one bead in the work store and a forced copy of the
// same id in the class store, titled after the leg that holds it: the steady
// state of a migrated city, where the migration preserves ids.
func seedCoResidentPair(t *testing.T) (work, graph beads.Store, id string) {
	t.Helper()
	work, graph = splittest.NewSplitStores(t)
	workCopy := mustCreateReadyBead(t, work, beads.Bead{Title: "work leg row", Type: "task"})
	forced, ok := graph.(beads.ForeignIDCreator)
	if !ok {
		t.Fatalf("class store %T cannot model the migration's forced foreign-id copy", graph)
	}
	if _, err := forced.CreateWithForeignID(beads.Bead{ID: workCopy.ID, Title: "graph leg row", Type: "task"}); err != nil {
		t.Fatalf("copy %s into the class store: %v", workCopy.ID, err)
	}
	return work, graph, workCopy.ID
}

// TestFederateReadyBeadsDedupeIsByLegPositionNotCompletion is
// TestReadyDedupeIsFirstLegWins's concurrent twin: the first leg is forced to
// answer LAST, and its row must still win.
func TestFederateReadyBeadsDedupeIsByLegPositionNotCompletion(t *testing.T) {
	work, graph, id := seedCoResidentPair(t)
	synctest.Test(t, func(t *testing.T) {
		graphFirst := newCompletionOrder()
		defer graphFirst.markFinished()
		legs := []readyLeg{
			readyTestLeg("city", readyOrderedStore{Store: work, waitFor: graphFirst}),
			readyTestLeg("graph", readyOrderedStore{Store: graph, signal: graphFirst}),
		}
		rows, err := federateReadyBeads(legs, beads.ReadyQuery{TierMode: beads.FederatedReadTier})
		if err != nil {
			t.Fatalf("federateReadyBeads: %v", err)
		}
		if len(rows) != 1 || rows[0].ID != id || rows[0].Title != "work leg row" {
			t.Fatalf("merged = %v, want only the first leg's row for %s even though the second leg answered first", rows, id)
		}
	})
}

// TestFederateListBeadsWithOwnerResolvesOwnershipByLegPosition pins the same
// rule for the merged rows AND the ownership map the blocked_by enrichment
// reads through.
func TestFederateListBeadsWithOwnerResolvesOwnershipByLegPosition(t *testing.T) {
	work, graph, id := seedCoResidentPair(t)
	inProgress, assignee := readyStatusInProgress, "worker-1"
	for _, store := range []beads.Store{work, graph} {
		if err := store.Update(id, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
			t.Fatalf("claim %s: %v", id, err)
		}
	}
	synctest.Test(t, func(t *testing.T) {
		graphFirst := newCompletionOrder()
		defer graphFirst.markFinished()
		legs := []readyLeg{
			readyTestLeg("city", readyOrderedStore{Store: work, waitFor: graphFirst}),
			readyTestLeg("graph", readyOrderedStore{Store: graph, signal: graphFirst}),
		}
		rows, owners, err := federateListBeadsWithOwner(legs, beads.ListQuery{Status: readyStatusInProgress, Live: true, TierMode: beads.FederatedReadTier})
		if err != nil {
			t.Fatalf("federateListBeadsWithOwner: %v", err)
		}
		if len(rows) != 1 || rows[0].Title != "work leg row" {
			t.Fatalf("merged = %v, want only the first leg's row", rows)
		}
		if owner := owners[id]; owner.label != "city" {
			t.Fatalf("owner of %s = %q, want %q: ownership resolves by leg position, not by which leg answered first", id, owner.label, "city")
		}
	})
}

// TestFederateReadyBeadsReportsTheFirstFailingLegByPosition pins error
// determinism: when two legs fail, the error names the one a sequential reader
// would have stopped at, even though the later leg failed first. And a failing
// leg still fails the whole answer — concurrency does not turn the fail-loud
// rule into a partial array.
func TestFederateReadyBeadsReportsTheFirstFailingLegByPosition(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		laterFailsFirst := newCompletionOrder()
		defer laterFailsFirst.markFinished()
		healthy := beads.NewMemStore()
		mustCreateReadyBead(t, healthy, beads.Bead{Title: "healthy work", Type: "task"})
		legs := []readyLeg{
			readyTestLeg("city", healthy),
			readyTestLeg("rig alpha", readyOrderedStore{Store: beads.NewMemStore(), waitFor: laterFailsFirst, err: errors.New("alpha is locked")}),
			readyTestLeg("rig beta", readyOrderedStore{Store: beads.NewMemStore(), signal: laterFailsFirst, err: errors.New("beta is locked")}),
		}
		rows, err := federateReadyBeads(legs, beads.ReadyQuery{TierMode: beads.FederatedReadTier})
		if err == nil {
			t.Fatalf("two dead legs produced an answer of %d rows; a short array is indistinguishable from \"no work\"", len(rows))
		}
		if !strings.Contains(err.Error(), "rig alpha store") || !strings.Contains(err.Error(), "alpha is locked") {
			t.Fatalf("error = %v, want the first-POSITION failing leg (rig alpha) named", err)
		}
	})
}

// TestOpenStandaloneRigStoresOpensUpToTheParallelismAtOnce pins the open half
// of the latency fix: each rig open is a preflight subprocess plus schema
// probes, so opening rigs one after another made `gc ready` pay their sum.
func TestOpenStandaloneRigStoresOpensUpToTheParallelismAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rigs := fakeRigs(5)
		probe := newFanOutProbe()
		defer probe.open()

		var stores map[string]beads.Store
		var failures []rigStoreOpenFailure
		done := make(chan struct{})
		go func() {
			defer close(done)
			stores, failures = openStandaloneRigStores(rigs, "/fake/city", func(string, string) (beads.Store, error) {
				probe.call(0)
				return beads.NewMemStore(), nil
			}, readyLegFanOut)
		}()
		synctest.Wait()
		if got := probe.running.Load(); got != int64(len(rigs)) {
			t.Fatalf("%d of %d rig stores were opening at once, want all of them", got, len(rigs))
		}
		probe.open()
		<-done
		if len(failures) != 0 || len(stores) != len(rigs) {
			t.Fatalf("opened %d stores with failures %v, want %d stores and none", len(stores), failures, len(rigs))
		}
	})
}

// TestOpenStandaloneRigStoresWithParallelismOneIsTheSequentialWalk pins what
// the controller and the other one-shot callers still get: one open at a time.
func TestOpenStandaloneRigStoresWithParallelismOneIsTheSequentialWalk(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rigs := fakeRigs(3)
		probe := newFanOutProbe()
		defer probe.open()
		done := make(chan struct{})
		go func() {
			defer close(done)
			openStandaloneRigStores(rigs, "/fake/city", func(string, string) (beads.Store, error) {
				probe.call(0)
				return beads.NewMemStore(), nil
			}, 1)
		}()
		synctest.Wait()
		if got := probe.running.Load(); got != 1 {
			t.Fatalf("%d rig stores opening at once with parallelism 1, want exactly 1", got)
		}
		probe.open()
		<-done
	})
}

// TestOpenStandaloneRigStoresReportsFailuresInRigOrder pins the failure list
// against completion order: the first rig in config order fails LAST, and is
// still reported first, so `gc ready`'s joined error and the supervisor's
// warnings read the same however the opens raced.
func TestOpenStandaloneRigStoresReportsFailuresInRigOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rigs := []config.Rig{
			{Name: "slow-bad", Path: "/fake/slow-bad"},
			{Name: "fast-bad", Path: "/fake/fast-bad"},
			{Name: "good", Path: "/fake/good"},
			{Name: "unbound"},
		}
		fastFirst := newCompletionOrder()
		defer fastFirst.markFinished()
		stores, failures := openStandaloneRigStores(rigs, "/fake/city", func(rigPath, _ string) (beads.Store, error) {
			switch rigPath {
			case "/fake/slow-bad":
				<-fastFirst.finished
				return nil, errors.New("slow-bad is unmounted")
			case "/fake/fast-bad":
				defer fastFirst.markFinished()
				return nil, errors.New("fast-bad is unmounted")
			case "":
				t.Errorf("opened the unbound rig; an empty path aliases the city scope")
			}
			return beads.NewMemStore(), nil
		}, readyLegFanOut)
		if len(failures) != 2 || failures[0].rig != "slow-bad" || failures[1].rig != "fast-bad" {
			t.Fatalf("failures = %v, want [slow-bad fast-bad] in config order regardless of completion order", failures)
		}
		if _, ok := stores["good"]; !ok || len(stores) != 1 {
			t.Fatalf("stores = %v, want exactly the good rig", stores)
		}
	})
}

func fakeRigs(n int) []config.Rig {
	rigs := make([]config.Rig, n)
	for i := range rigs {
		rigs[i] = config.Rig{Name: fmt.Sprintf("rig-%d", i), Path: fmt.Sprintf("/fake/rig-%d", i)}
	}
	return rigs
}

// readySuspensionRig is one rig of the suspension fixture: its authored
// startup default and, when non-nil, the runtime override `gc rig suspend` /
// `gc rig resume` records.
type readySuspensionRig struct {
	name             string
	suspendedOnStart bool
	runtime          *bool
}

func boolRef(v bool) *bool { return &v }

// readySuspensionRigs is the fixture's frame: one of each way a rig can be in
// or out of it.
var readySuspensionRigs = []readySuspensionRig{
	{name: "alpha"},                                                  // serving
	{name: "beta", runtime: boolRef(true)},                           // `gc rig suspend`
	{name: "gamma", suspendedOnStart: true},                          // dormant by default
	{name: "delta", suspendedOnStart: true, runtime: boolRef(false)}, // dormant, then `gc rig resume`
}

// readyServingRigNames is the frame readySuspensionRigs must produce.
var readyServingRigNames = []string{"alpha", "delta"}

// newReadySuspensionCity writes a real on-disk file-provider city with rigs
// suspended in each way the runtime understands, left ambient (GC_CITY) like
// the other gc ready fixtures. It returns the city dir and each rig's dir.
func newReadySuspensionCity(t *testing.T, rigs []readySuspensionRig) (string, map[string]string) {
	t.Helper()
	cityDir := t.TempDir()
	var cityToml strings.Builder
	cityToml.WriteString("[workspace]\nname = \"readysuspended\"\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n\n[[agent]]\nname = \"worker\"\nstart_command = \"true\"\nmax_active_sessions = 1\n")
	rigDirs := make(map[string]string, len(rigs))
	for _, rig := range rigs {
		dir := filepath.Join(cityDir, "rigs", rig.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("creating rig dir %s: %v", dir, err)
		}
		rigDirs[rig.name] = dir
		cityToml.WriteString("\n[[rigs]]\nname = " + strconv.Quote(rig.name) + "\npath = " + strconv.Quote(dir) + "\n")
		if rig.suspendedOnStart {
			cityToml.WriteString("suspended_on_start = true\n")
		}
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml.String()), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensuring scoped file store layout: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatalf("ensuring city file store: %v", err)
	}
	for _, dir := range rigDirs {
		if err := ensurePersistedScopeLocalFileStore(dir); err != nil {
			t.Fatalf("ensuring file store at %s: %v", dir, err)
		}
	}
	for _, rig := range rigs {
		if rig.runtime == nil {
			continue
		}
		if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cityDir, rig.name, rig.runtime); err != nil {
			t.Fatalf("recording runtime suspension for %s: %v", rig.name, err)
		}
	}
	prevCityFlag, prevRigFlag := cityFlag, rigFlag
	cityFlag, rigFlag = "", ""
	t.Cleanup(func() { cityFlag, rigFlag = prevCityFlag, prevRigFlag })
	t.Setenv("GC_CITY", cityDir)
	return cityDir, rigDirs
}

// recordReadyRigOpens swaps the rig-leg opener for one that records which rig
// paths `gc ready` opened, and how often.
func recordReadyRigOpens(t *testing.T) func() map[string]int {
	t.Helper()
	var mu sync.Mutex
	opens := map[string]int{}
	prev := readyRigStoreOpener
	readyRigStoreOpener = func(cfg *config.City) rigStoreOpener {
		inner := prev(cfg)
		return func(rigPath, cityPath string) (beads.Store, error) {
			mu.Lock()
			opens[filepath.Base(rigPath)]++
			mu.Unlock()
			return inner(rigPath, cityPath)
		}
	}
	t.Cleanup(func() { readyRigStoreOpener = prev })
	return func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]int, len(opens))
		for k, v := range opens {
			out[k] = v
		}
		return out
	}
}

// TestReadyRigLegStoresOpenOnlyTheServingFrame pins which rig stores `gc ready`
// opens: every bound rig that is not suspended — `gc rig suspend`, or
// suspended_on_start with no runtime resume — each exactly once. A suspended
// rig is never opened: suspension is quiescence, and any read of a bd-owned
// scope restarts the proxy and Dolt the controller retired for it.
func TestReadyRigLegStoresOpenOnlyTheServingFrame(t *testing.T) {
	cityDir, _ := newReadySuspensionCity(t, readySuspensionRigs)
	opens := recordReadyRigOpens(t)
	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}

	stores, err := readyRigLegStores(cfg, cityDir)
	if err != nil {
		t.Fatalf("readyRigLegStores: %v", err)
	}
	if got := sortedStoreNames(stores); !slices.Equal(got, readyServingRigNames) {
		t.Fatalf("gc ready federates rigs %v, want the serving frame %v", got, readyServingRigNames)
	}
	got := opens()
	for _, name := range readyServingRigNames {
		if got[name] != 1 {
			t.Fatalf("serving rig %q opened %d times, want exactly once (opens: %v)", name, got[name], got)
		}
	}
	for _, name := range []string{"beta", "gamma"} {
		if got[name] != 0 {
			t.Fatalf("suspended rig %q was opened %d times; a suspended scope must not be touched (opens: %v)", name, got[name], got)
		}
	}
}

// TestReadyServingFrameIsTheCensusFrame is the suspension half of reader
// agreement: the rigs `gc ready` federates are exactly the rigs the
// controller's demand census reads (servingRigStores). A rig only one of them
// reads is work counted-but-unclaimable or claimable-but-uncounted — the
// D1-D9 divergence class, through suspension instead of residency.
func TestReadyServingFrameIsTheCensusFrame(t *testing.T) {
	cityDir, _ := newReadySuspensionCity(t, readySuspensionRigs)
	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}
	serving, err := readyServingRigs(cfg, cityDir)
	if err != nil {
		t.Fatalf("readyServingRigs: %v", err)
	}
	claimSide := make([]string, 0, len(serving))
	for _, rig := range serving {
		claimSide = append(claimSide, rig.Name)
	}
	slices.Sort(claimSide)

	opened := make(map[string]beads.Store, len(cfg.Rigs))
	for _, rig := range cfg.Rigs {
		opened[rig.Name] = beads.NewMemStore()
	}
	censusSide := sortedStoreNames(servingRigStores(cfg, opened, buildSuspendedRigPathsForCity(cfg, cityDir)))

	if !slices.Equal(claimSide, censusSide) {
		t.Fatalf("gc ready reads rigs %v but the controller census reads %v; the two frames must be one", claimSide, censusSide)
	}
	if !slices.Equal(claimSide, readyServingRigNames) {
		t.Fatalf("agreed frame %v, want %v", claimSide, readyServingRigNames)
	}
}

// TestReadyServesNoWorkFromASuspendedRigUntilItResumes is the end-to-end
// shape the claim path reads: routed, unassigned work in the city store and in
// every rig. A suspended rig's routed row is out of the answer — the same frame
// the controller counts demand over, so no seat is spawned for it either — and
// it comes back the moment the rig resumes: nothing is lost, it is paused.
// The city store is never skipped.
func TestReadyServesNoWorkFromASuspendedRigUntilItResumes(t *testing.T) {
	rigs := []readySuspensionRig{{name: "alpha"}, {name: "beta", runtime: boolRef(true)}}
	cityDir, rigDirs := newReadySuspensionCity(t, rigs)
	routed := map[string]string{"gc.routed_to": "worker"}
	seed := func(scope string) string {
		store, err := openStoreAtForCity(scope, cityDir)
		if err != nil {
			t.Fatalf("open store at %s: %v", scope, err)
		}
		return mustCreateReadyBead(t, store, beads.Bead{Title: "routed in " + filepath.Base(scope), Type: "task", Metadata: routed}).ID
	}
	cityBead, alphaBead, betaBead := seed(cityDir), seed(rigDirs["alpha"]), seed(rigDirs["beta"])
	routedOpts := readyOpts{metadataFields: []string{beadmeta.RoutedToMetadataKey + "=worker"}, unassigned: true}

	rows := runCmdReady(t, routedOpts)
	for _, id := range []string{cityBead, alphaBead} {
		if !containsReadyID(rows, id) {
			t.Fatalf("gc ready = %v, want serving work %s", readyWireIDs(rows), id)
		}
	}
	if containsReadyID(rows, betaBead) {
		t.Fatalf("gc ready = %v, served %s from the suspended rig beta", readyWireIDs(rows), betaBead)
	}

	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cityDir, "beta", boolRef(false)); err != nil {
		t.Fatalf("resuming beta: %v", err)
	}
	if rows := runCmdReady(t, routedOpts); !containsReadyID(rows, betaBead) {
		t.Fatalf("gc ready = %v after `gc rig resume beta`, want its routed work %s back", readyWireIDs(rows), betaBead)
	}
}

// TestReadyIsNotFailedByADarkSuspendedRig: a rig whose store cannot be opened
// fails `gc ready` while it serves (TestReadyFailsLoudWhenALegErrors), but a
// SUSPENDED rig is routinely dark — the controller stops its Dolt — and is not
// in the frame, so it is not opened and cannot fail the claim of every agent
// in the city.
func TestReadyIsNotFailedByADarkSuspendedRig(t *testing.T) {
	cityDir := newReadyCityWithBrokenRig(t)
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cityDir, readyBrokenRigName, boolRef(true)); err != nil {
		t.Fatalf("suspending %s: %v", readyBrokenRigName, err)
	}
	var stdout, stderr bytes.Buffer
	if code := cmdReady(readyOpts{}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc ready = %d with only the suspended rig dark; stderr=%s", code, stderr.String())
	}
	if openFailure := "rig " + strconv.Quote(readyBrokenRigName) + " store:"; strings.Contains(stderr.String(), openFailure) {
		t.Fatalf("gc ready stderr = %q, but the suspended rig is outside the frame and must not be opened", stderr.String())
	}
	var rows []readyBead
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("gc ready emitted %q, which is not the bd array: %v", stdout.String(), err)
	}
}

// TestReadyRefusesAnUnreadableSuspensionState: the frame cannot be computed
// without the suspension state, and guessing it either way is wrong — read
// every rig and a suspended scope is woken; skip by the authored defaults and a
// rig resumed at runtime has its work silently dropped from an array that has
// nowhere to say so. `gc hook` refuses on the same file for the same reason.
func TestReadyRefusesAnUnreadableSuspensionState(t *testing.T) {
	cityDir := newReadyCityWithRigs(t)
	statePath := citylayout.SuspensionStateFile(cityDir)
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatalf("creating runtime dir: %v", err)
	}
	if err := os.WriteFile(statePath, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("corrupting the suspension state: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdReady(readyOpts{}, &stdout, &stderr); code == 0 {
		t.Fatalf("gc ready exited 0 over an unreadable suspension state; stdout=%s", stdout.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("gc ready emitted %s alongside the failure", stdout.String())
	}
	if !strings.Contains(stderr.String(), "suspension state") {
		t.Fatalf("gc ready stderr = %q, want it to name the suspension state it could not read", stderr.String())
	}
}

// TestReadyComposesTheCityConfigOncePerInvocation pins the config half of
// reuse within one command. On a bd-backed city each rig open resolves the
// pinned bd (workspacePinnedBdBinary), which composed the whole city config
// again — 22 composed loads per `gc ready` on maintainer-city. The opener here
// makes the same load inside every rig open; the invocation memo must answer
// all of them from the one load the command already made.
//
// Not parallel: composedCityConfigLoads and the opener seam are process-wide.
func TestReadyComposesTheCityConfigOncePerInvocation(t *testing.T) {
	newReadyCityWithRigs(t)
	t.Setenv("GC_RIG", readyCountedRigNames[0])
	prev := readyRigStoreOpener
	readyRigStoreOpener = func(cfg *config.City) rigStoreOpener {
		inner := prev(cfg)
		return func(rigPath, cityPath string) (beads.Store, error) {
			if _, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard); err != nil {
				return nil, err
			}
			return inner(rigPath, cityPath)
		}
	}
	t.Cleanup(func() { readyRigStoreOpener = prev })

	var stdout, stderr bytes.Buffer
	before := composedCityConfigLoads.Load()
	if code := cmdReady(readyOpts{}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdReady = %d; stderr=%s", code, stderr.String())
	}
	if grew := composedCityConfigLoads.Load() - before; grew != 1 {
		t.Fatalf("gc ready composed the city config %d times over %d rig opens, want exactly 1: every open in one invocation must reuse the config the command loaded", grew, len(readyCountedRigNames))
	}
	if currentInvocationConfigMemo() != nil {
		t.Fatal("gc ready left its invocation config memo open after returning")
	}
}
