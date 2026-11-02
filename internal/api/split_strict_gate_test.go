package api

import (
	"os"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
)

// The split-strict test gate for internal/api.
//
// With GC_TEST_SPLIT_STRICT=1 (//internal/api:api_split_strict_test) every
// fakeState work accessor (CityBeadStore, BeadStore, BeadStores) hands out the
// gate's WORK view of the physical store and every class accessor left on the
// city store (Sessions, Graph, Nudges, Orders, and the default mail provider)
// its CLASS view, so the package's tests run against a projection split: a
// handler that reads a session through the work store sees nothing, as it would
// on a split city, and the read is charged to its production call site. See
// internal/beads/splittest/projection.go.
//
// TestMain reports the ratchet over the keys the run reached, and the failing
// tests against the known failures, as two separate verdicts
// (internal/beads/splittest/gate.go). Regenerate the baseline from a full run
// with scripts/split-strict-baseline.

// splitStrictFiles are this package's checked-in gate inputs, relative to the
// package directory the test binary runs in.
var splitStrictFiles = splittest.GateFiles{
	Baseline:      "testdata/split_strict_baseline.tsv",
	KnownFailures: "testdata/split_strict_known_failures.list",
	KnownFlaky:    "testdata/split_strict_known_flaky.list",
}

func init() {
	if splittest.GateEnabled() {
		splittest.SetPackageUnderTest("internal/api")
		splittest.SetGateFiles(splitStrictFiles)
	}
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
	known, err := splittest.ParseKnownFailures(k)
	if err != nil {
		t.Fatalf("%s: %v", splitStrictFiles.KnownFailures, err)
	}
	for name, reason := range known {
		if splittest.Untriaged(reason) {
			t.Errorf("%s: %s is untriaged; replace its reason with why it fails under the gate", splitStrictFiles.KnownFailures, name)
		}
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

func TestFakeStateHandsOutGateViewsOnlyUnderTheGate(t *testing.T) {
	fs := newFakeState(t)
	fs.cityBeadStore = fs.stores["myrig"]
	if splittest.GateEnabled() {
		p := splittest.ProjectionOf(fs.cityBeadStore)
		if fs.CityBeadStore() != p.WorkView() || fs.BeadStore("myrig") != p.WorkView() {
			t.Fatal("work accessors do not hand out the gate's work view")
		}
		if fs.SessionsBeadStore().Store != p.ClassView() || fs.GraphBeadStore().Store != p.ClassView() {
			t.Fatal("class accessors do not hand out the gate's class view")
		}
	} else if fs.CityBeadStore() != fs.cityBeadStore || fs.SessionsBeadStore().Store != fs.cityBeadStore {
		t.Fatal("accessors wrap the store although the gate is off")
	}
	relocated := beads.NewMemStore()
	fs.graphBeadStore = relocated
	if fs.GraphBeadStore().Store != beads.Store(relocated) {
		t.Fatal("a store relocated to its own binding was wrapped")
	}
}
