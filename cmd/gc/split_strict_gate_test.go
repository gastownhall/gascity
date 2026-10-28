package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/rogpeppe/go-internal/testscript"
)

// The split-strict test gate for cmd/gc.
//
// With GC_TEST_SPLIT_STRICT=1 (//cmd/gc:gc_split_strict_test) this binary
// installs splitStrictSeam, so every city whose routes relocate nothing looks
// split to the code under test: the controller's and runtime's work accessors
// and one-shot scope-store opens hand out the gate's WORK view, and
// resolveClassStore's identity branch its CLASS view (split_strict_seam.go).
// A read that reaches a wrong-class row through the wrong view is charged to
// its production call site; splitStrictTestingM reports the ratchet and the
// failing-test check separately (internal/beads/splittest/gate.go). Fixtures
// that already build a physical split (splitEnv, newSplitStores) keep their
// own stores: their relocated classes never reach the identity branch.

// splitStrictFiles are this package's checked-in gate inputs, relative to the
// package directory the test binary runs in.
//
// The known-failures list is a .list, not a .txt: TestTutorial01 runs every
// *.txt in testdata as a testscript.
var splitStrictFiles = splittest.GateFiles{
	Baseline:      "testdata/split_strict_baseline.tsv",
	KnownFailures: "testdata/split_strict_known_failures.list",
	KnownFlaky:    "testdata/split_strict_known_flaky.list",
}

func init() {
	// Pass-through wrappers: a violation is charged to whoever called INTO
	// them, not to the wrapper. The strict kit leaves need this too, gate or
	// not: a fixture seeding through the policy store is test code.
	splittest.TreatFilesAsPlumbing(
		"cmd/gc/bead_policy_store.go",
		"cmd/gc/class_store_emit.go",
		"cmd/gc/split_strict_seam.go",
	)
	if !splittest.GateEnabled() {
		return
	}
	splitStrictSeam = splitStrictGate{}
	splittest.SetPackageUnderTest("cmd/gc")
	splittest.SetGateFiles(splitStrictFiles)
}

// splitStrictGate hands out the splittest projection's views over the store
// it is given (see splittest.Projection for root and front).
type splitStrictGate struct{}

func (splitStrictGate) workView(store beads.Store) beads.Store {
	return splittest.ProjectionOf(store).WorkViewOf(store)
}

func (splitStrictGate) classView(store beads.Store) beads.Store {
	return splittest.ProjectionOf(store).ClassViewOf(store)
}

// splitStrictTestingM starts the gate before the package's tests and reports
// it after (a no-op without GC_TEST_SPLIT_STRICT=1).
type splitStrictTestingM struct{ m testscript.TestingM }

func (m splitStrictTestingM) Run() int {
	// A hung test exits the binary from the watchdog; run TestMain's
	// teardown first (temp roots, managed Dolt, the tmux socket parent), or
	// the hang leaks them.
	splittest.SetHangCleanup(func() {
		if testProcessTeardown != nil {
			testProcessTeardown()
		}
	})
	splittest.GateStart()
	return splittest.GateMain(m.m.Run(), splitStrictFiles, os.Stderr)
}

func TestSplitStrictGateInputsParse(t *testing.T) {
	f, err := os.Open(splitStrictFiles.Baseline)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // read-only
	entries, err := splittest.ParseBaseline(f)
	if err != nil {
		t.Fatalf("%s: %v", splitStrictFiles.Baseline, err)
	}
	for _, e := range entries {
		if e.Verdict == splittest.VerdictTriage {
			t.Errorf("%s: %s is unadjudicated (triage); give it a verdict", splitStrictFiles.Baseline, e.Key)
		}
	}
	k, err := os.Open(splitStrictFiles.KnownFailures)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close() //nolint:errcheck // read-only
	if _, err := splittest.ParseKnownFailures(k); err != nil {
		t.Fatalf("%s: %v", splitStrictFiles.KnownFailures, err)
	}
	fl, err := os.Open(splitStrictFiles.KnownFlaky)
	if err != nil {
		t.Fatal(err)
	}
	defer fl.Close() //nolint:errcheck // read-only
	if _, err := splittest.ParseKnownFlaky(fl); err != nil {
		t.Fatalf("%s: %v", splitStrictFiles.KnownFlaky, err)
	}
}

func TestSplitStrictSeamIsInstalledOnlyUnderTheGate(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{Workspace: config.Workspace{Name: "c"}}
	cs := &controllerState{cityBeadStore: store, cityName: "c", cfg: cfg}
	if !splittest.GateEnabled() {
		if splitStrictSeam != nil {
			t.Fatal("the seam is installed although the gate is off")
		}
		if cs.CityBeadStore() != beads.Store(store) || cs.SessionsBeadStore().Store != beads.Store(store) {
			t.Fatal("accessors wrap the store although the gate is off")
		}
		return
	}
	p := splittest.ProjectionOf(store)
	if cs.CityBeadStore() != p.WorkView() {
		t.Fatal("CityBeadStore does not hand out the gate's work view")
	}
	if cs.SessionsBeadStore().Store != p.ClassView() || cs.GraphBeadStore().Store != p.ClassView() {
		t.Fatal("the class accessors do not hand out the gate's class view")
	}
}

// TestSplitStrictControllerSharesOneProjection pins the controller shape:
// newControllerState opens the city store long-lived, wraps it in its cache and
// policy store, and every accessor then resolves to ONE projection over the
// physical store. A session created and read through the sessions accessor is
// not a violation, and its id carries the binding's prefix.
func TestSplitStrictControllerSharesOneProjection(t *testing.T) {
	if !splittest.GateEnabled() {
		t.Skip("exercises the split-strict gate (GC_TEST_SPLIT_STRICT=1)")
	}
	t.Setenv("GC_BEADS", "file")
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cs := newControllerState(ctx, cfg, runtime.NewFake(), events.NewFake(), "test-city", t.TempDir())
	if cs.cityBeadStore == nil {
		t.Fatal("no city store")
	}
	work := cs.CityBeadStore()
	sessions := cs.SessionsBeadStore().Store
	if splittest.ProjectionOf(work) != splittest.ProjectionOf(sessions) {
		t.Fatal("the work and sessions accessors resolved to different projections")
	}
	before := len(splittest.GateSites())
	created, err := sessions.Create(beads.Bead{Title: "s", Type: session.BeadType, Labels: []string{session.LabelSession}})
	if err != nil {
		t.Fatalf("creating a session through the sessions accessor: %v", err)
	}
	if !strings.HasPrefix(created.ID, "gcg-") {
		t.Fatalf("session id = %q; the mint hook did not reach the physical store under the cache", created.ID)
	}
	if got, err := sessions.Get(created.ID); err != nil || got.ID != created.ID {
		t.Fatalf("reading the session back through the sessions accessor = %+v, %v", got, err)
	}
	if after := len(splittest.GateSites()); after != before {
		t.Fatalf("a legitimate session create and read added %d violation key(s)", after-before)
	}
}
