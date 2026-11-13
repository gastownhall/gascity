package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/doctor"
)

// productionSplitDemandRepairs is P2's closure as it ships; the fixture
// stubs the seam with a counter.
var productionSplitDemandRepairs = splitDemandRepairs

// setGCVersion runs the test as a gc whose normalized version is v, built as
// built (a Go pseudo-version keeps its suffix there).
func setGCVersion(t *testing.T, v, built string) {
	t.Helper()
	prev, prevBuilt := version, versionBuilt
	version, versionBuilt = v, built
	t.Cleanup(func() { version, versionBuilt = prev, prevBuilt })
}

// assertSplitWroteNothing: city.toml as before, no journal, no floor.
func (c *guidedSplitCity) assertSplitWroteNothing(before string) {
	c.t.Helper()
	if c.readTOML() != before {
		c.t.Fatal("a refused split changed city.toml")
	}
	if _, present, _ := readStorageSplitJournal(c.request.CityPath); present {
		c.t.Fatal("a refused split wrote a journal")
	}
	if fileExists(minGCVersionPath(c.request.CityPath)) {
		c.t.Fatal("a refused split wrote the version floor")
	}
}

// TestSplitMinGCVersionIsAComparableRelease pins the constant's shape: one
// MAJOR.MINOR.PATCH the floor file accepts and the comparison reads as a
// release. And it pins the comparison (M1): releases and release
// candidates are compared, dev and Go pseudo-versions are not.
func TestSplitMinGCVersionIsAComparableRelease(t *testing.T) {
	if !minGCVersionPattern.MatchString(splitMinGCVersion) {
		t.Fatalf("splitMinGCVersion %q is not MAJOR.MINOR.PATCH", splitMinGCVersion)
	}
	if core, ok := gcVersionCore(splitMinGCVersion); !ok || core != splitMinGCVersion {
		t.Fatalf("gcVersionCore(%q) = %q, %t", splitMinGCVersion, core, ok)
	}
	for _, tc := range []struct {
		running string
		below   bool
	}{
		{"1.5.9", true},
		{"v1.5.9", true},
		{"1.5.9-rc1", true},
		{"1.6.0-rc1", false},
		{"1.6.0", false},
		{"1.10.0", false},
		{"dev", false},
		{"v1.4.3-0.20261113052233-a07d754c75ab", false},
		{"1.4.3-0.20261113052233-a07d754c75ab", false},
		{"v1.6.0-rc.1.0.20261113052233-a07d754c75ab", false},
		{"v0.0.0-20261113052233-a07d754c75ab", false},
	} {
		if got := gcVersionBelowFloor(tc.running, splitMinGCVersion); got != tc.below {
			t.Errorf("gcVersionBelowFloor(%q, %s) = %t, want %t", tc.running, splitMinGCVersion, got, tc.below)
		}
	}
}

// TestVersionFloorBootGateAgrees: boot reads this gc's version the way the
// split does (gcVersionForFloor), a pseudo-version build included, whose
// normalized version names a release below the floor.
func TestVersionFloorBootGateAgrees(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMinGCVersion(cityPath, splitMinGCVersion); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		version, built string
		refuses        bool
	}{
		{"1.5.9", "1.5.9", true},
		{"1.6.0-rc1", "v1.6.0-rc1", false},
		{"dev", "dev", false},
		{"1.4.3", "v1.4.3-0.20261113052233-a07d754c75ab", false},
	} {
		setGCVersion(t, tc.version, tc.built)
		_, err := storageBootGate(cityPath, nil, "gc start", nil, io.Discard)
		if (err != nil) != tc.refuses {
			t.Errorf("boot as %s (built %s) = %v, want refused %t", tc.version, tc.built, err, tc.refuses)
		}
		if got := splitVersionFloorRefusal(cityPath, splitMinGCVersion); (got != nil) != tc.refuses {
			t.Errorf("the split's floor check as %s = %v, want refused %t", tc.version, got, tc.refuses)
		}
	}
}

// TestStorageSplitRefusesAGcBelowItsVersionFloor (M1): a gc below the floor
// the split would leave refuses before P1, and in --dry-run, writing
// nothing; a release candidate of the floor, dev and a pseudo-version split.
func TestStorageSplitRefusesAGcBelowItsVersionFloor(t *testing.T) {
	t.Run("below", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		before := c.readTOML()
		setGCVersion(t, "1.5.9", "1.5.9")
		// The lock is held: the refusal comes before P1 takes it.
		held, err := acquireControllerLock(c.request.CityPath)
		if err != nil {
			t.Fatal(err)
		}
		code, _, stderr := c.split(splitOptions{})
		held.Close() //nolint:errcheck // test cleanup
		if code == 0 || !strings.Contains(stderr, "below the version floor "+splitMinGCVersion) {
			t.Fatalf("split as 1.5.9 = %d: %s", code, stderr)
		}
		c.assertSplitWroteNothing(before)
		code, stdout, stderr := c.split(splitOptions{DryRun: true})
		if code == 0 || !strings.Contains(stdout+stderr, "gc version") || !strings.Contains(stdout+stderr, "below the version floor") {
			t.Fatalf("--dry-run as 1.5.9 = %d:\n%s%s", code, stdout, stderr)
		}
		c.assertSplitWroteNothing(before)
	})
	t.Run("below a higher floor already there", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		if err := writeMinGCVersion(c.request.CityPath, "1.7.0"); err != nil {
			t.Fatal(err)
		}
		setGCVersion(t, "1.6.5", "1.6.5")
		if code, _, stderr := c.split(splitOptions{}); code == 0 || !strings.Contains(stderr, "below the version floor 1.7.0") {
			t.Fatalf("split as 1.6.5 under a 1.7.0 floor = %d: %s", code, stderr)
		}
	})
	t.Run("a resume compares the journal's floor", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		// Killed once city.toml is written: the rerun resumes (killed before
		// it, the split starts over and records the floor afresh).
		c.killSplitAt("split city.toml")
		j := c.journal()
		j.MinGCVersion = "1.7.0"
		if err := writeStorageSplitJournal(c.request.CityPath, j); err != nil {
			t.Fatal(err)
		}
		setGCVersion(t, "1.6.5", "1.6.5")
		if code, _, stderr := c.split(splitOptions{}); code == 0 || !strings.Contains(stderr, "below the version floor 1.7.0") {
			t.Fatalf("a resume as 1.6.5 of a split recording 1.7.0 = %d: %s", code, stderr)
		}
		if c.journal().Phase != splitPhaseConfiguring {
			t.Fatal("the refused resume moved the journal")
		}
	})
	for _, tc := range []struct{ name, version, built string }{
		{"release candidate of the floor", "1.6.0-rc1", "v1.6.0-rc1"},
		{"dev", "dev", "dev"},
		{"pseudo-version", "1.4.3", "v1.4.3-0.20261113052233-a07d754c75ab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newGuidedSplitCity(t)
			setGCVersion(t, tc.version, tc.built)
			if code, stdout, stderr := c.split(splitOptions{}); code != 0 {
				t.Fatalf("split as %s = %d:\n%s\n%s", tc.built, code, stdout, stderr)
			}
			c.assertSplit()
		})
	}
}

// TestStorageSplitNeverImportsRigsOntoAnUnconvergedBinding (S1): a resume
// whose journal says the city converged, over a binding that no longer
// holds it, refuses before the rig import and leaves the rig as it was.
func TestStorageSplitNeverImportsRigsOntoAnUnconvergedBinding(t *testing.T) {
	c := newGuidedSplitCity(t)
	c.killSplitAt("split " + splitPhaseCityConverged)
	target := c.target()
	rigRows := len(rigTestList(t, c.rig()))
	if err := os.Rename(target.Root, target.Root+".lost"); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := c.split(splitOptions{})
	if code == 0 || !strings.Contains(stderr, "a rig is never imported onto a binding that does not hold the city's state") {
		t.Fatalf("a resume over a lost binding = %d: %s", code, stderr)
	}
	if strings.Contains(stderr, "--abandon") {
		t.Fatalf("the refusal offers --abandon past the city's copy: %s", stderr)
	}
	if got := len(rigTestList(t, c.rig())); got != rigRows {
		t.Fatalf("the rig holds %d row(s), want %d", got, rigRows)
	}
	if fileExists(target.Database) {
		t.Fatal("the refused resume created a binding")
	}
	if c.journal().Phase != splitPhaseCityConverged {
		t.Fatalf("the journal = %s, want city_converged", c.journal().Phase)
	}
	if code, _, stderr := c.split(splitOptions{Abandon: true}); code == 0 || !strings.Contains(stderr, "converged the city's copy") {
		t.Fatalf("--abandon past the city's copy = %d: %s", code, stderr)
	}
}

// TestStorageSplitResumeKeepsTheAfterHashOfWhatItWrote (S2, design M3): the
// split stops after it wrote city.toml, the operator edits the file, the
// split resumes and finishes, and the reverse removes [storage] in place,
// keeping the edit, because the journal still records what the split wrote.
func TestStorageSplitResumeKeepsTheAfterHashOfWhatItWrote(t *testing.T) {
	c := newGuidedSplitCity(t)
	c.killSplitAt("split " + splitPhaseConfigured)
	written := c.journal().CityTOMLAfterSHA256
	c.writeTOML(strings.Replace(c.readTOML(), "# trailing comment", "# edited while the split was stopped", 1))
	code, stdout, stderr := c.split(splitOptions{})
	if code != 0 {
		t.Fatalf("the resume = %d:\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "in place and keeps the change") {
		t.Errorf("the resume did not say the file changed: %s", stderr)
	}
	if c.journal().CityTOMLAfterSHA256 != written {
		t.Fatal("the resume rewrote the after-hash to the edited file")
	}
	var out, errOut bytes.Buffer
	if code := doStorageReverse(context.Background(), c.reload(), reverseOptions{Mode: reverseToWork}, &out, &errOut); code != 0 {
		t.Fatalf("--to-work = %d:\n%s\n%s", code, out.String(), errOut.String())
	}
	if got := c.readTOML(); !strings.Contains(got, "# edited while the split was stopped") || strings.Contains(got, "[storage") {
		t.Fatalf("city.toml after the reverse:\n%s\nwant the edit kept and [storage] gone", got)
	}
}

// TestStorageSplitDemandRepairsRunAfterTheSnapshot (S2, design M8): P2 runs
// legacy's repairs for real, after P3: the binding carries the repaired row,
// and the snapshot carries it as it was before the split.
func TestStorageSplitDemandRepairsRunAfterTheSnapshot(t *testing.T) {
	c := newGuidedSplitCity(t)
	splitDemandRepairs = productionSplitDemandRepairs
	c.writeTOML(c.readTOML() + "\n[[agent]]\nname = \"worker\"\ndir = \"packs\"\nmin_active_sessions = 0\nmax_active_sessions = 3\n")
	step := graphRow("gd-step1", "gd-step1", "", map[string]string{beadmeta.RoutedToMetadataKey: "packs/worker-2"})
	if _, err := c.work().Create(step); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := c.split(splitOptions{}); code != 0 {
		t.Fatalf("split = %d:\n%s\n%s", code, stdout, stderr)
	}
	binding, err := openInfraDestination(c.target())
	if err != nil {
		t.Fatal(err)
	}
	got, err := binding.Get("gd-step1")
	_ = closeBeadStoreHandle(binding)
	if err != nil {
		t.Fatalf("the binding does not hold the routed step: %v", err)
	}
	if route := got.Metadata[beadmeta.RoutedToMetadataKey]; route != "packs/worker" {
		t.Fatalf("the binding's step is routed to %q, want the repaired %q", route, "packs/worker")
	}
	j := c.journal()
	var snapshotted string
	_ = filepath.Walk(filepath.Join(j.SnapshotTo, j.Snapshot), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".jsonl") || filepath.Base(path) == "export.jsonl" {
			return nil //nolint:nilerr // a walk over a test's own snapshot
		}
		for _, e := range readSnapshotJSONLEntries(t, path) {
			if e.Bead.ID == "gd-step1" {
				snapshotted = e.Bead.Metadata[beadmeta.RoutedToMetadataKey]
			}
		}
		return nil
	})
	if snapshotted != "packs/worker-2" {
		t.Fatalf("the snapshot carries the step routed to %q, want it as it was before the split (%q)", snapshotted, "packs/worker-2")
	}
}

// TestStorageSplitAbandonBeforeItWroteCityTOML (S3): a split stopped before
// P4a wrote no [storage], floor or binding row, so --abandon touches none of
// those, even when the operator authored them since; it closes the journal
// and unpins the snapshot. The split's refusal and doctor both name it.
func TestStorageSplitAbandonBeforeItWroteCityTOML(t *testing.T) {
	c := newGuidedSplitCity(t)
	if err := writeMinGCVersion(c.request.CityPath, "1.5.1"); err != nil {
		t.Fatal(err)
	}
	c.killSplitAt("split " + splitPhaseReadied)
	j := c.journal()
	// The operator authors the split by hand and starts its migration.
	c.writeTOML(c.readTOML() + "\n" + splitStorageTOML(splitHypotheticalConfig(c.request.Cfg)))
	prev := infraEngineCrashPoint
	infraEngineCrashPoint = func(p string) error {
		if strings.HasPrefix(p, "copying rows") {
			return errors.New("killed")
		}
		return nil
	}
	var out, errOut bytes.Buffer
	_ = doStorageMigrate(context.Background(), c.reload(), &out, &errOut)
	infraEngineCrashPoint = prev
	binding, err := openInfraDestination(c.target())
	if err != nil {
		t.Fatal(err)
	}
	rows := len(rigTestList(t, binding))
	_ = closeBeadStoreHandle(binding)
	if rows == 0 {
		t.Fatal("precondition: the hand migration copied no row")
	}
	authored := c.readTOML()

	code, _, stderr := c.split(splitOptions{})
	if code == 0 || !strings.Contains(stderr, "stopped before it wrote city.toml") || !strings.Contains(stderr, storageSplitAbandonInstruction()) {
		t.Fatalf("split over a hand-authored [storage] = %d: %s", code, stderr)
	}
	res := newStorageSplitCheck(c.reload().Cfg, c.request.CityPath).Run(nil)
	if !strings.Contains(strings.Join(append([]string{res.Message}, res.Details...), "\n"), storageSplitAbandonInstruction()) {
		t.Fatalf("doctor does not name --abandon for the stuck split: %s %v", res.Message, res.Details)
	}

	code, stdout, stderr := c.split(splitOptions{Abandon: true})
	if code != 0 {
		t.Fatalf("--abandon = %d:\n%s\n%s", code, stdout, stderr)
	}
	if c.readTOML() != authored {
		t.Fatal("--abandon changed a city.toml the split never wrote")
	}
	if data, _ := os.ReadFile(minGCVersionPath(c.request.CityPath)); string(data) != "1.5.1\n" {
		t.Fatalf("--abandon changed the floor to %q", data)
	}
	binding, err = openInfraDestination(c.target())
	if err != nil {
		t.Fatal(err)
	}
	after := len(rigTestList(t, binding))
	_ = closeBeadStoreHandle(binding)
	if after != rows {
		t.Fatalf("--abandon deleted binding rows the split never wrote: %d, want %d", after, rows)
	}
	if got := c.journal(); got.Phase != splitPhaseAbandoned {
		t.Fatalf("the journal = %s, want abandoned", got.Phase)
	}
	if fileExists(filepath.Join(j.SnapshotTo, j.Snapshot, storageSnapshotPinName)) {
		t.Fatal("the snapshot is still pinned")
	}
}

// TestStorageSplitReportsALiveRigCoTenantUpFront (S4): a live city sharing a
// rig store the split imports is named by the rehearsal (--dry-run and P0),
// and the split still refuses it at the rig step, with the city split.
func TestStorageSplitReportsALiveRigCoTenantUpFront(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("lock holders are inspected on Linux and Darwin")
	}
	c := newGuidedSplitCity(t)
	otherRig := filepath.Join(t.TempDir(), "elsewhere")
	other := otherCityWithRig(t, otherRig)
	if err := os.MkdirAll(filepath.Join(other, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubOtherCities(t, other)
	rigs := map[string]bool{canonicalIdentityPath(c.rigPath): true, canonicalIdentityPath(otherRig): true}
	prev := storeDoltIdentityOf
	t.Cleanup(func() { storeDoltIdentityOf = prev })
	storeDoltIdentityOf = func(_, root string) storeDoltIdentity {
		if rigs[canonicalIdentityPath(root)] {
			return storeDoltIdentity{Database: "packs", Endpoint: "loopback:3399"}
		}
		sum := sha256.Sum256([]byte(canonicalIdentityPath(root)))
		return storeDoltIdentity{Database: "own_" + hex.EncodeToString(sum[:4]), Endpoint: "loopback:3400"}
	}
	c.request.Force = storageForce{SharedStores: []string{"packs@loopback:3399"}}
	held, err := acquireControllerLock(other)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close() //nolint:errcheck // test cleanup

	_, stdout, stderr := c.split(splitOptions{DryRun: true})
	if !strings.Contains(stdout+stderr, "which is live") || !strings.Contains(stdout+stderr, other) {
		t.Fatalf("--dry-run does not name the live co-tenant:\n%s%s", stdout, stderr)
	}
	code, stdout, stderr := c.split(splitOptions{})
	if code == 0 || !strings.Contains(stderr, "holds its controller lock") {
		t.Fatalf("split beside a live rig co-tenant = %d:\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "which is live") {
		t.Fatalf("the split's rehearsal (P0) did not name the live co-tenant:\n%s", stdout)
	}
	if c.journal().Phase != splitPhaseCityConverged {
		t.Fatalf("the journal = %s, want city_converged (the rig step refuses)", c.journal().Phase)
	}
	if !controllerLockHeld(other) {
		t.Fatal("precondition: the co-tenant's lock is still held")
	}
}

// TestStorageSplitAbandonRestoresOnlyBytesThatProve (NIT 1): bytes in the
// journal that do not prove against their recorded sha256 (a damaged or
// hand-edited journal) are never written back; the abandon removes [storage]
// in place instead.
func TestStorageSplitAbandonRestoresOnlyBytesThatProve(t *testing.T) {
	c := newGuidedSplitCity(t)
	before := c.readTOML()
	c.killSplitAt("copying rows")
	j := c.journal()
	damaged := "[workspace]\nname = \"not the city\"\n"
	j.CityTOMLBefore = &damaged
	if err := writeStorageSplitJournal(c.request.CityPath, j); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := c.split(splitOptions{Abandon: true}); code != 0 {
		t.Fatalf("--abandon = %d:\n%s\n%s", code, stdout, stderr)
	}
	got := c.readTOML()
	if strings.Contains(got, "not the city") || strings.Contains(got, "[storage") || !strings.Contains(got, "name = \"guided\"") {
		t.Fatalf("city.toml after --abandon:\n%s\nwant the file as it was, [storage] removed in place (before the split:\n%s)", got, before)
	}
}

// TestStorageSplitHintOffersAbandonOnlyWhereItWorks (NIT 2): a split whose
// clear fails after the marker does not offer --abandon, which refuses
// there; and --abandon refuses a stale marker too (O3).
func TestStorageSplitHintOffersAbandonOnlyWhereItWorks(t *testing.T) {
	t.Run("after the marker", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		prev := clearRetainedInfraCopiesFn
		clearRetainedInfraCopiesFn = func(string, infraBindingTarget, io.Writer) (infraClearResult, error) {
			return infraClearResult{}, errors.New("the clear failed")
		}
		t.Cleanup(func() { clearRetainedInfraCopiesFn = prev })
		code, _, stderr := c.split(splitOptions{})
		if code == 0 || strings.Contains(stderr, "--abandon") {
			t.Fatalf("a split stopped after the marker = %d, offering --abandon: %s", code, stderr)
		}
		clearRetainedInfraCopiesFn = prev
		if code, _, stderr := c.split(splitOptions{Abandon: true}); code == 0 || !strings.Contains(stderr, "converged the city's copy") {
			t.Fatalf("--abandon after the marker = %d: %s", code, stderr)
		}
	})
	t.Run("before the marker", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		prev := infraEngineCrashPoint
		infraEngineCrashPoint = func(p string) error {
			if strings.HasPrefix(p, "copying rows") {
				return errors.New("the copy failed")
			}
			return nil
		}
		code, _, stderr := c.split(splitOptions{})
		infraEngineCrashPoint = prev
		if code == 0 || !strings.Contains(stderr, storageSplitAbandonInstruction()) {
			t.Fatalf("a split stopped before the marker = %d, not offering --abandon: %s", code, stderr)
		}
	})
	t.Run("a stale marker", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		c.killSplitAt("clearing")
		target := c.target()
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(target.Database + suffix)
		}
		if state, err := readInfraConvergenceState(target); err != nil || state != infraConvergenceStale {
			t.Fatalf("precondition: the binding is %v, %v; want stale", state, err)
		}
		before := c.readTOML()
		if code, _, stderr := c.split(splitOptions{Abandon: true}); code == 0 || !strings.Contains(stderr, "converged the city's copy") {
			t.Fatalf("--abandon over a stale marker = %d: %s", code, stderr)
		}
		if c.readTOML() != before {
			t.Fatal("the refused abandon changed city.toml")
		}
	})
}

// TestStorageSplitCarriesASupersededFloorRecord (NIT 3): a split that wrote
// the floor and was superseded leaves its record of the floor before it, so
// the new split's --abandon removes the floor rather than keeping the one
// the superseded split wrote.
func TestStorageSplitCarriesASupersededFloorRecord(t *testing.T) {
	c := newGuidedSplitCity(t)
	c.killSplitAt("split floor")
	if !fileExists(minGCVersionPath(c.request.CityPath)) {
		t.Fatal("precondition: the floor is written")
	}
	c.writeTOML(c.readTOML() + "# an operator edit\n")
	c.killSplitAt("copying rows")
	if j := c.journal(); j.MinGCVersionBefore != nil {
		t.Fatalf("the new split records the floor before it as %q, want none", *j.MinGCVersionBefore)
	}
	if code, stdout, stderr := c.split(splitOptions{Abandon: true}); code != 0 {
		t.Fatalf("--abandon = %d:\n%s\n%s", code, stdout, stderr)
	}
	if fileExists(minGCVersionPath(c.request.CityPath)) {
		t.Fatal("--abandon kept the floor a superseded split wrote")
	}
}

// TestStorageSplitPinsOnlyASnapshotItsJournalNames (NIT 4): a kill between
// the journal naming the snapshot and its pin leaves it unpinned, never a
// pinned snapshot no journal names.
func TestStorageSplitPinsOnlyASnapshotItsJournalNames(t *testing.T) {
	c := newGuidedSplitCity(t)
	c.killSplitAt("split " + splitPhaseSnapshotted)
	j := c.journal()
	if j.Snapshot == "" {
		t.Fatal("the journal names no snapshot")
	}
	entries, err := os.ReadDir(j.SnapshotTo)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if fileExists(filepath.Join(j.SnapshotTo, e.Name(), storageSnapshotPinName)) {
			t.Fatalf("snapshot %s is pinned before the run got past its pin", e.Name())
		}
	}
}

// TestStorageSplitReadsCityTOMLUnderTheLock (NIT 5): the bytes P4a records
// are read under the lock and must be the ones the run judged; and a resume
// of the edit refuses a city.toml that is neither the bytes before nor the
// bytes it writes.
func TestStorageSplitReadsCityTOMLUnderTheLock(t *testing.T) {
	t.Run("changed while waiting", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		changed := c.readTOML() + "# changed under the run\n"
		prev := infraEngineCrashPoint
		t.Cleanup(func() { infraEngineCrashPoint = prev })
		infraEngineCrashPoint = func(p string) error {
			if p == "split guarded" {
				c.writeTOML(changed)
			}
			return nil
		}
		code, _, stderr := c.split(splitOptions{})
		if code == 0 || !strings.Contains(stderr, "changed while this run rehearsed the split") {
			t.Fatalf("split over a city.toml changed under it = %d: %s", code, stderr)
		}
		if c.readTOML() != changed {
			t.Fatal("the refused split wrote city.toml")
		}
	})
	t.Run("neither before nor after", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		c.killSplitAt("split city.toml")
		edited := strings.Replace(c.readTOML(), "# trailing comment", "# edited mid-write", 1)
		c.writeTOML(edited)
		code, _, stderr := c.split(splitOptions{})
		if code == 0 || !strings.Contains(stderr, "neither the file the split recorded") {
			t.Fatalf("a resume over an unrecognized city.toml = %d: %s", code, stderr)
		}
		// The judge refuses it up front (round 2, Codex F4): no note promises
		// to keep an edit that the write would then refuse.
		if strings.Contains(stderr, "keeps the change") {
			t.Fatalf("the refusal also promised to keep the edit: %s", stderr)
		}
		if c.readTOML() != edited || c.journal().Phase != splitPhaseConfiguring {
			t.Fatal("the refused resume wrote city.toml or moved the journal")
		}
	})
}

// TestDoctorStorageSplitKeepsTheJournalHintBesideAnError (NIT 8): an
// unconverged split is a doctor error, and its details still name the split
// that stopped and the command that continues it.
func TestDoctorStorageSplitKeepsTheJournalHintBesideAnError(t *testing.T) {
	c := newGuidedSplitCity(t)
	c.killSplitAt("copying rows")
	res := newStorageSplitCheck(c.reload().Cfg, c.request.CityPath).Run(nil)
	if res.Status != doctor.StatusError {
		t.Fatalf("doctor over an unconverged split = %v: %s", res.Status, res.Message)
	}
	if !strings.Contains(strings.Join(res.Details, "\n"), "stopped at "+splitPhaseConfigured) {
		t.Fatalf("the error's details do not name the stopped split: %v", res.Details)
	}
}

// TestDoctorStorageSplitIsQuietAtRigsImported (O30): a split past its rig
// imports has only its check left, which is doctor's own; doctor does not
// warn about the journal then, so P9 does not see its own run.
func TestDoctorStorageSplitIsQuietAtRigsImported(t *testing.T) {
	c := newGuidedSplitCity(t)
	c.killSplitAt("split " + splitPhaseRigsImported)
	res := newStorageSplitCheck(c.reload().Cfg, c.request.CityPath).Run(nil)
	if strings.Contains(res.Message, "guided split") || strings.Contains(strings.Join(res.Details, "\n"), "guided split") {
		t.Fatalf("doctor at rigs_imported names the journal: %s %v", res.Message, res.Details)
	}
}

// TestStorageSplitVerifyFailsOnStatus (O11): P9 runs gc storage status; a rig
// whose store gained an infrastructure row after its import fails it (the
// doctor check only warns there), and the journal stays before complete.
func TestStorageSplitVerifyFailsOnStatus(t *testing.T) {
	c := newGuidedSplitCity(t)
	prev := infraEngineCrashPoint
	t.Cleanup(func() { infraEngineCrashPoint = prev })
	infraEngineCrashPoint = func(p string) error {
		if p == "split "+splitPhaseRigsImported {
			_, err := c.rig().Create(graphRow("gp-late", "gp-late", "", nil))
			return err
		}
		return nil
	}
	code, _, stderr := c.split(splitOptions{})
	if code == 0 || !strings.Contains(stderr, "does not pass") {
		t.Fatalf("split with a rig stranded after its import = %d: %s", code, stderr)
	}
	if c.journal().Phase != splitPhaseRigsImported {
		t.Fatalf("the journal = %s, want rigs_imported", c.journal().Phase)
	}
}

// TestStorageSplitAbandonTakesTheExclusions (O18, O19): --abandon needs the
// attestation, and refuses a journal another run moved while it waited.
func TestStorageSplitAbandonTakesTheExclusions(t *testing.T) {
	t.Run("no attestation", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		c.killSplitAt("copying rows")
		before := c.readTOML()
		c.request.FleetStopped = false
		if code, _, stderr := c.split(splitOptions{Abandon: true}); code == 0 || !strings.Contains(stderr, "--fleet-stopped") {
			t.Fatalf("--abandon without --fleet-stopped = %d: %s", code, stderr)
		}
		if c.readTOML() != before || c.journal().Phase != splitPhaseConfigured {
			t.Fatal("the refused abandon wrote something")
		}
	})
	t.Run("journal moved under the guard", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		c.killSplitAt("copying rows")
		before := c.readTOML()
		prev := infraEngineCrashPoint
		t.Cleanup(func() { infraEngineCrashPoint = prev })
		infraEngineCrashPoint = func(p string) error {
			if p == "split abandon guarded" {
				j := c.journal()
				j.Forced = []string{"moved"}
				return writeStorageSplitJournal(c.request.CityPath, j)
			}
			return nil
		}
		if code, _, stderr := c.split(splitOptions{Abandon: true}); code == 0 || !strings.Contains(stderr, "changed while this run waited") {
			t.Fatalf("--abandon over a journal moved under the guard = %d: %s", code, stderr)
		}
		if c.readTOML() != before || c.journal().Phase != splitPhaseConfigured {
			t.Fatal("the refused abandon wrote something")
		}
	})
}

// Review round 2 (adopted at 45df7df50e).

// TestStorageSplitRehearsalRefusesAStaleSessionPathGC (round 2, B10): P0
// holds the gc an agent session runs against the floor the split leaves, so
// a stale one refuses the split with nothing written, and --dry-run blocks
// on it, rather than P9 finding it with the city cut over and its rigs
// imported. A gc the probe cannot run or compare is a note, and one at the
// floor passes.
func TestStorageSplitRehearsalRefusesAStaleSessionPathGC(t *testing.T) {
	t.Run("below the floor", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		before := c.readTOML()
		rigRows := len(rigTestList(t, c.rig()))
		stubSessionPathGC(t, "1.5.1", nil)
		code, stdout, stderr := c.split(splitOptions{})
		if code == 0 || !strings.Contains(stderr, "the rehearsal refused the split (gc version") {
			t.Fatalf("split with a 1.5.1 gc on the session PATH = %d:\n%s\n%s", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "the gc on the session PATH (/usr/local/bin/gc) is 1.5.1, below the floor "+splitMinGCVersion) {
			t.Fatalf("the rehearsal does not name the stale gc:\n%s", stdout)
		}
		c.assertSplitWroteNothing(before)
		if state, err := readInfraConvergenceState(c.target()); err != nil || state != infraConvergenceAbsent {
			t.Fatalf("the refused split left the binding %v, %v", state, err)
		}
		if got := len(rigTestList(t, c.rig())); got != rigRows {
			t.Fatalf("the refused split moved the rig's rows: %d, want %d", got, rigRows)
		}
		code, stdout, stderr = c.split(splitOptions{DryRun: true})
		if code == 0 || !strings.Contains(stdout+stderr, "is 1.5.1, below the floor "+splitMinGCVersion) {
			t.Fatalf("--dry-run with a 1.5.1 gc on the session PATH = %d:\n%s%s", code, stdout, stderr)
		}
		c.assertSplitWroteNothing(before)
	})
	t.Run("below a higher floor already there", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		if err := writeMinGCVersion(c.request.CityPath, "1.7.0"); err != nil {
			t.Fatal(err)
		}
		setGCVersion(t, "1.7.2", "1.7.2")
		stubSessionPathGC(t, "1.6.5", nil)
		if code, stdout, stderr := c.split(splitOptions{DryRun: true}); code == 0 || !strings.Contains(stdout, "is 1.6.5, below the floor 1.7.0") {
			t.Fatalf("--dry-run with a 1.6.5 session gc under a 1.7.0 floor = %d:\n%s%s", code, stdout, stderr)
		}
	})
	for _, tc := range []struct {
		name, version string
		err           error
		row           string
	}{
		{"no gc on the session PATH", "", nil, "no gc on the session PATH to compare with the floor " + splitMinGCVersion},
		{"a gc that cannot report", "", errors.New("exec format error"), "could not report its version (exec format error)"},
		{"a dev build", "dev", nil, "reports version dev, which cannot be compared"},
		{"at the floor", splitMinGCVersion, nil, "is " + splitMinGCVersion + ", at or above the floor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newGuidedSplitCity(t)
			stubSessionPathGC(t, tc.version, tc.err)
			code, stdout, stderr := c.split(splitOptions{})
			if code != 0 {
				t.Fatalf("split = %d:\n%s\n%s", code, stdout, stderr)
			}
			if !strings.Contains(stdout, tc.row) {
				t.Fatalf("the rehearsal does not say %q:\n%s", tc.row, stdout)
			}
			c.assertSplit()
		})
	}
}

// TestStorageSplitReversedSplitIsClosedByAbandon (round 2): a split stopped
// past its city's copy and then reversed (`--to-work`) is undone. Doctor
// names --abandon and a new split, not a command that continues it;
// --abandon closes the journal and unpins the snapshot and changes nothing
// else (the floor stays as the reverse left it); a kill mid-abandon is
// finished by the next; and a new split then starts over and finishes.
func TestStorageSplitReversedSplitIsClosedByAbandon(t *testing.T) {
	for _, phase := range []string{splitPhaseCityConverged, splitPhaseRigsImported} {
		t.Run(phase, func(t *testing.T) {
			c := newGuidedSplitCity(t)
			if err := os.WriteFile(minGCVersionPath(c.request.CityPath), []byte("1.5.1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			c.killSplitAt("split " + phase)
			first := c.journal()
			var stdout, stderr bytes.Buffer
			if code := doStorageReverse(context.Background(), c.reload(), reverseOptions{Mode: reverseToWork}, &stdout, &stderr); code != 0 {
				t.Fatalf("--to-work = %d:\n%s\n%s", code, stdout.String(), stderr.String())
			}
			reversed := c.readTOML()
			request := c.reload()
			if request.Cfg.Storage != nil || fileExists(minGCVersionPath(c.request.CityPath)) {
				t.Fatal("precondition: the reverse removed [storage] and the floor")
			}
			res := newStorageSplitCheck(request.Cfg, c.request.CityPath).Run(nil)
			if res.Status != doctor.StatusWarning || !strings.Contains(res.Message, "was undone since") ||
				!strings.Contains(res.Message, "`"+storageSplitAbandonInstruction()+"` closes it") || strings.Contains(res.Message, "continues it") {
				t.Fatalf("doctor over a reversed split = %v: %s", res.Status, res.Message)
			}

			prev := infraEngineCrashPoint
			infraEngineCrashPoint = func(p string) error {
				if p == "split "+splitPhaseAbandoning {
					return errors.New("killed")
				}
				return nil
			}
			code, _, _ := c.split(splitOptions{Abandon: true})
			infraEngineCrashPoint = prev
			if code == 0 || c.journal().Phase != splitPhaseAbandoning {
				t.Fatalf("the kill mid-abandon = %d, journal %s", code, c.journal().Phase)
			}
			code, out, errOut := c.split(splitOptions{Abandon: true})
			if code != 0 || !strings.Contains(out, "and undone since") || !strings.Contains(out, "nothing else was changed") {
				t.Fatalf("--abandon of the reversed split = %d:\n%s\n%s", code, out, errOut)
			}
			j := c.journal()
			if j.Phase != splitPhaseAbandoned || j.AbandonedFrom != phase || j.Run != first.Run {
				t.Fatalf("the journal after --abandon = %s from %s (run %s, want %s)", j.Phase, j.AbandonedFrom, j.Run, first.Run)
			}
			if fileExists(filepath.Join(first.SnapshotTo, first.Snapshot, storageSnapshotPinName)) {
				t.Fatal("the reversed split's snapshot is still pinned")
			}
			if c.readTOML() != reversed {
				t.Fatal("--abandon changed the city.toml the reverse left")
			}
			if fileExists(minGCVersionPath(c.request.CityPath)) {
				t.Fatal("--abandon wrote a floor back over the reverse")
			}
			if res := newStorageSplitCheck(c.reload().Cfg, c.request.CityPath).Run(nil); strings.Contains(res.Message, "guided split") {
				t.Fatalf("doctor after --abandon still names the split: %s", res.Message)
			}

			if code, out, errOut := c.split(splitOptions{}); code != 0 {
				t.Fatalf("a split after the abandon = %d:\n%s\n%s", code, out, errOut)
			}
			c.assertSplit()
			if !fileExists(storageSplitJournalPath(c.request.CityPath) + ".abandoned-" + first.Run) {
				t.Fatal("the abandoned journal was not kept aside")
			}
		})
	}
}

// TestStorageSplitAbandonRefusesAHandRevertedSplit (round 2): past the
// city's copy, a split whose [storage] was taken out of city.toml by hand
// and whose binding root moved aside is not undone: the served-binding note
// still holds the city to the binding, whose rows are the city's state. So
// --abandon refuses and keeps the snapshot pinned, and doctor does not call
// the split undone.
func TestStorageSplitAbandonRefusesAHandRevertedSplit(t *testing.T) {
	c := newGuidedSplitCity(t)
	before := c.readTOML()
	c.killSplitAt("split " + splitPhaseCityConverged)
	j := c.journal()
	c.writeTOML(before)
	target := c.target()
	if err := os.Rename(target.Root, target.Root+".moved"); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := c.split(splitOptions{Abandon: true})
	if code == 0 || !strings.Contains(stderr, "converged the city's copy") {
		t.Fatalf("--abandon of a hand-reverted split = %d: %s", code, stderr)
	}
	if c.journal().Phase != splitPhaseCityConverged {
		t.Fatalf("the refused abandon moved the journal to %s", c.journal().Phase)
	}
	if !fileExists(filepath.Join(j.SnapshotTo, j.Snapshot, storageSnapshotPinName)) {
		t.Fatal("the refused abandon unpinned the snapshot")
	}
	res := newStorageSplitCheck(c.reload().Cfg, c.request.CityPath).Run(nil)
	if strings.Contains(strings.Join(append([]string{res.Message}, res.Details...), "\n"), "undone since") {
		t.Fatalf("doctor calls a hand-reverted split undone: %s %v", res.Message, res.Details)
	}
}

// TestStorageSplitRecordsTheFloorBeforeAnythingIsWritten (round 2): the
// floor from before the split is in the journal from its first write, and a
// superseded split's record is carried by every run after it, so a run
// stopped before P4a no longer loses it: --abandon puts back the floor the
// city had before the first split. A split that stopped before it could
// write the floor carries nothing, and the next one reads the file.
func TestStorageSplitRecordsTheFloorBeforeAnythingIsWritten(t *testing.T) {
	t.Run("carried through a run stopped before P4a", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		c.killSplitAt("split floor")
		if !fileExists(minGCVersionPath(c.request.CityPath)) {
			t.Fatal("precondition: split A wrote the floor")
		}
		c.writeTOML(c.readTOML() + "# an operator edit\n")
		c.killSplitAt("split " + splitPhaseSnapshotted)
		if j := c.journal(); j.Phase != splitPhaseSnapshotted || j.MinGCVersionBefore != nil || !j.MinGCVersionBeforeCarried {
			t.Fatalf("split B's journal = %s, floor before %v, carried %t; want snapshotted, none, carried", j.Phase, j.MinGCVersionBefore, j.MinGCVersionBeforeCarried)
		}
		c.killSplitAt("copying rows")
		if j := c.journal(); j.MinGCVersionBefore != nil {
			t.Fatalf("split C records the floor before it as %q; the city had none before split A", *j.MinGCVersionBefore)
		}
		if code, stdout, stderr := c.split(splitOptions{Abandon: true}); code != 0 {
			t.Fatalf("--abandon = %d:\n%s\n%s", code, stdout, stderr)
		}
		if fileExists(minGCVersionPath(c.request.CityPath)) {
			t.Fatal("--abandon left the floor split A wrote; the city had none before the split")
		}
	})
	t.Run("not carried from a split that never wrote it", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		c.killSplitAt("split " + splitPhaseReadied)
		if err := os.WriteFile(minGCVersionPath(c.request.CityPath), []byte("1.5.1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		c.killSplitAt("copying rows")
		if j := c.journal(); j.MinGCVersionBefore == nil || *j.MinGCVersionBefore != "1.5.1\n" || j.MinGCVersionBeforeCarried {
			t.Fatalf("the floor record = %v, carried %t; want the file's 1.5.1, read", j.MinGCVersionBefore, j.MinGCVersionBeforeCarried)
		}
		if code, stdout, stderr := c.split(splitOptions{Abandon: true}); code != 0 {
			t.Fatalf("--abandon = %d:\n%s\n%s", code, stdout, stderr)
		}
		if data, _ := os.ReadFile(minGCVersionPath(c.request.CityPath)); string(data) != "1.5.1\n" {
			t.Fatalf("the floor after --abandon = %q, want the 1.5.1 set before the split", data)
		}
	})
}

// TestStorageSplitStoppedBeforeCityTOMLStartsOver (round 2): a split stopped
// at configuring, before it wrote city.toml, starts over like one stopped
// earlier, because a boot between the runs serves the city unsplit: the
// rerun rehearses again and takes a new snapshot, which holds what the city
// wrote meanwhile, and the stopped split's journal goes aside with its
// snapshot unpinned and its floor record carried.
func TestStorageSplitStoppedBeforeCityTOMLStartsOver(t *testing.T) {
	for _, seam := range []string{"split " + splitPhaseConfiguring, "split floor"} {
		t.Run(seam, func(t *testing.T) {
			c := newGuidedSplitCity(t)
			c.killSplitAt(seam)
			first := c.journal()
			c.assertBoot("serves")
			written, err := c.work().Create(beads.Bead{Title: "written while the city ran", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			code, stdout, stderr := c.split(splitOptions{})
			if code != 0 {
				t.Fatalf("the rerun = %d:\n%s\n%s", code, stdout, stderr)
			}
			if !strings.Contains(stderr, "stopped before it wrote city.toml ("+splitPhaseConfiguring+"), and the city may have run since; starting it over") || !strings.Contains(stdout, "the rehearsal is ready") {
				t.Fatalf("the rerun did not start over with a rehearsal:\n%s\n%s", stdout, stderr)
			}
			j := c.journal()
			if j.Run == first.Run || j.Snapshot == first.Snapshot {
				t.Fatalf("the rerun kept run %s / snapshot %s", j.Run, j.Snapshot)
			}
			if j.MinGCVersionBefore != nil || !j.MinGCVersionBeforeCarried {
				t.Fatalf("the new journal's floor record = %v, carried %t; want the stopped split's (none), carried", j.MinGCVersionBefore, j.MinGCVersionBeforeCarried)
			}
			if !fileExists(storageSplitJournalPath(c.request.CityPath) + ".superseded-" + first.Run) {
				t.Fatal("the stopped split's journal was not kept aside")
			}
			if fileExists(filepath.Join(first.SnapshotTo, first.Snapshot, storageSnapshotPinName)) {
				t.Fatal("the stopped split's snapshot is still pinned")
			}
			if !splitSnapshotHolds(t, j, written.ID) {
				t.Fatalf("the new snapshot %s does not hold %s, written between the runs", j.Snapshot, written.ID)
			}
			c.assertSplit()
		})
	}
}

// splitSnapshotHolds reports whether the snapshot j names holds bead id.
func splitSnapshotHolds(t *testing.T, j storageSplitJournal, id string) bool {
	t.Helper()
	found := false
	_ = filepath.Walk(filepath.Join(j.SnapshotTo, j.Snapshot), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".jsonl") || filepath.Base(path) == "export.jsonl" {
			return nil //nolint:nilerr // a walk over a test's own snapshot
		}
		for _, e := range readSnapshotJSONLEntries(t, path) {
			found = found || e.Bead.ID == id
		}
		return nil
	})
	return found
}

// TestStorageSplitUnpinFailureIsNotSwallowed (round 2): --abandon whose
// snapshot pin cannot be removed fails before it closes the journal and
// names the pin, so the next --abandon removes it; a split that supersedes a
// stopped one warns, naming the pin it could not remove.
func TestStorageSplitUnpinFailureIsNotSwallowed(t *testing.T) {
	// jam replaces the pin with a directory that is not empty, which no
	// remove takes.
	jam := func(t *testing.T, j storageSplitJournal) string {
		t.Helper()
		pin := filepath.Join(j.SnapshotTo, j.Snapshot, storageSnapshotPinName)
		if err := os.Remove(pin); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(pin, "held"), 0o755); err != nil {
			t.Fatal(err)
		}
		return pin
	}
	for _, seam := range []string{"split " + splitPhaseReadied, "copying rows"} {
		t.Run("abandon after "+seam, func(t *testing.T) {
			c := newGuidedSplitCity(t)
			before := c.readTOML()
			c.killSplitAt(seam)
			pin := jam(t, c.journal())
			code, _, stderr := c.split(splitOptions{Abandon: true})
			if code == 0 || !strings.Contains(stderr, pin) {
				t.Fatalf("--abandon over a pin it cannot remove = %d: %s", code, stderr)
			}
			if c.journal().Phase != splitPhaseAbandoning {
				t.Fatalf("the journal = %s, want it left abandoning", c.journal().Phase)
			}
			// The pin is a plain file again, as a fixed permission leaves it:
			// the next --abandon removes it and closes the journal.
			if err := os.RemoveAll(pin); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(pin, []byte("pinned\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if code, stdout, stderr := c.split(splitOptions{Abandon: true}); code != 0 {
				t.Fatalf("the abandon's rerun = %d:\n%s\n%s", code, stdout, stderr)
			}
			c.assertAbandoned(before)
		})
	}
	t.Run("a superseding split", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		c.killSplitAt("split " + splitPhaseReadied)
		first := c.journal()
		pin := jam(t, first)
		code, stdout, stderr := c.split(splitOptions{})
		if code != 0 {
			t.Fatalf("the superseding split = %d:\n%s\n%s", code, stdout, stderr)
		}
		if !strings.Contains(stderr, "keeps its snapshot pinned") || !strings.Contains(stderr, pin) {
			t.Fatalf("the superseding split did not warn about the pin %s: %s", pin, stderr)
		}
		if !fileExists(storageSplitJournalPath(c.request.CityPath) + ".superseded-" + first.Run) {
			t.Fatal("the stopped split's journal was not kept aside")
		}
		c.assertSplit()
	})
}

// TestStorageSplitContinueHintsCarryTheAttestations (round 2): a split
// accepted under --force names the command that continues it with those
// attestations, in its failure line, in doctor and in --abandon's refusal;
// its copy stopping before the marker names that command alone, not the
// engine's `gc storage migrate --from-work`. The bare command refuses at P1,
// and the one the hints name finishes the split.
func TestStorageSplitContinueHintsCarryTheAttestations(t *testing.T) {
	want := storageSplitInstruction() + " --" + storageForceFlag + "=" + storageHazardRemoteRuntime
	if got := (storageSplitJournal{Forced: []string{storageHazardRemoteRuntime, storageHazardSharedStore + ":abc"}}).continueInstruction(); got != want+" --"+storageForceFlag+"="+storageHazardSharedStore+":abc" {
		t.Fatalf("continueInstruction = %q", got)
	}
	t.Run("before the marker", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		stubSessionInventory(t, nil, false, nil)
		c.request.Force = storageForce{RemoteRuntime: true}
		prev := infraEngineCrashPoint
		t.Cleanup(func() { infraEngineCrashPoint = prev })
		infraEngineCrashPoint = func(p string) error {
			if strings.HasPrefix(p, "copying rows") {
				return errors.New("the copy failed")
			}
			return nil
		}
		code, _, stderr := c.split(splitOptions{})
		infraEngineCrashPoint = prev
		if code == 0 || !strings.Contains(stderr, "Run `"+want+"` to continue") {
			t.Fatalf("a split stopped before the marker = %d, not naming %q: %s", code, want, stderr)
		}
		if strings.Contains(stderr, storageMigrationCommand) || !strings.Contains(stderr, "stopped before its convergence marker") {
			t.Fatalf("the stop names a second remedy, or not the split's own: %s", stderr)
		}
		if res := newStorageSplitCheck(c.reload().Cfg, c.request.CityPath).Run(nil); !strings.Contains(strings.Join(res.Details, "\n"), "`"+want+"` continues it") {
			t.Fatalf("doctor does not name %q: %s %v", want, res.Message, res.Details)
		}
		c.request.Force = storageForce{}
		if code, _, stderr := c.split(splitOptions{}); code == 0 || !strings.Contains(stderr, "--"+storageForceFlag+"="+storageHazardRemoteRuntime) {
			t.Fatalf("the bare command = %d: %s", code, stderr)
		}
		c.request.Force = storageForce{RemoteRuntime: true}
		if code, stdout, stderr := c.split(splitOptions{}); code != 0 {
			t.Fatalf("%s = %d:\n%s\n%s", want, code, stdout, stderr)
		}
		c.assertSplit()
	})
	t.Run("after the marker", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		stubSessionInventory(t, nil, false, nil)
		c.request.Force = storageForce{RemoteRuntime: true}
		prev := clearRetainedInfraCopiesFn
		t.Cleanup(func() { clearRetainedInfraCopiesFn = prev })
		clearRetainedInfraCopiesFn = func(string, infraBindingTarget, io.Writer) (infraClearResult, error) {
			return infraClearResult{}, errors.New("the clear failed")
		}
		if code, _, stderr := c.split(splitOptions{}); code == 0 || !strings.Contains(stderr, "Run `"+want+"` to continue") {
			t.Fatalf("a split stopped after the marker = %d: %s", code, stderr)
		}
		if code, _, stderr := c.split(splitOptions{Abandon: true}); code == 0 || !strings.Contains(stderr, "Finish the split with `"+want+"`") {
			t.Fatalf("--abandon after the marker = %d: %s", code, stderr)
		}
	})
}
