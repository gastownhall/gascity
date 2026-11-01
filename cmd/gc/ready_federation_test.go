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
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fanout/fanouttest"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/readywork"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// TestOpenStandaloneRigStoresOpensUpToTheParallelismAtOnce pins the open half
// of the latency fix: each rig open is a preflight subprocess plus schema
// probes, so opening rigs one after another made `gc ready` pay their sum.
func TestOpenStandaloneRigStoresOpensUpToTheParallelismAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rigs := fakeRigs(5)
		probe := fanouttest.NewProbe()
		defer probe.Open()

		var stores map[string]beads.Store
		var failures []rigStoreOpenFailure
		done := make(chan struct{})
		go func() {
			defer close(done)
			stores, failures = openStandaloneRigStores(rigs, "/fake/city", func(string, string) (beads.Store, error) {
				probe.Call(0)
				return beads.NewMemStore(), nil
			}, readyLegFanOut)
		}()
		synctest.Wait()
		if got := probe.Running(); got != int64(len(rigs)) {
			t.Fatalf("%d of %d rig stores were opening at once, want all of them", got, len(rigs))
		}
		probe.Open()
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
		probe := fanouttest.NewProbe()
		defer probe.Open()
		done := make(chan struct{})
		go func() {
			defer close(done)
			openStandaloneRigStores(rigs, "/fake/city", func(string, string) (beads.Store, error) {
				probe.Call(0)
				return beads.NewMemStore(), nil
			}, 1)
		}()
		synctest.Wait()
		if got := probe.Running(); got != 1 {
			t.Fatalf("%d rig stores opening at once with parallelism 1, want exactly 1", got)
		}
		probe.Open()
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
		fastFirst := make(chan struct{})
		var fastOnce sync.Once
		markFastFinished := func() { fastOnce.Do(func() { close(fastFirst) }) }
		defer markFastFinished()
		stores, failures := openStandaloneRigStores(rigs, "/fake/city", func(rigPath, _ string) (beads.Store, error) {
			switch rigPath {
			case "/fake/slow-bad":
				<-fastFirst
				return nil, errors.New("slow-bad is unmounted")
			case "/fake/fast-bad":
				defer markFastFinished()
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
	routedOpts := readywork.Query{MetadataFields: []string{beadmeta.RoutedToMetadataKey + "=worker"}, Unassigned: true}

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
	if code := cmdReady(readyFlags{}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc ready = %d with only the suspended rig dark; stderr=%s", code, stderr.String())
	}
	if openFailure := "rig " + strconv.Quote(readyBrokenRigName) + " store:"; strings.Contains(stderr.String(), openFailure) {
		t.Fatalf("gc ready stderr = %q, but the suspended rig is outside the frame and must not be opened", stderr.String())
	}
	var rows []readywork.ReadyBead
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
	if code := cmdReady(readyFlags{}, &stdout, &stderr); code == 0 {
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
	if code := cmdReady(readyFlags{}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdReady = %d; stderr=%s", code, stderr.String())
	}
	if grew := composedCityConfigLoads.Load() - before; grew != 1 {
		t.Fatalf("gc ready composed the city config %d times over %d rig opens, want exactly 1: every open in one invocation must reuse the config the command loaded", grew, len(readyCountedRigNames))
	}
	if currentInvocationConfigMemo() != nil {
		t.Fatal("gc ready left its invocation config memo open after returning")
	}
}
