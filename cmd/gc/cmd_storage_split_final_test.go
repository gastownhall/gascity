package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// linkedVersionForTest is version as the linker left it: package variables
// are initialized before any init function runs, so this is the value
// cmd_version.go's init reads before it normalizes version.
var linkedVersionForTest = version

// TestVersionBuiltIsRecordedByInit (V1w, V1n): init records the version this
// binary was built as through builtVersion, unnormalized.
func TestVersionBuiltIsRecordedByInit(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	if want := builtVersion(linkedVersionForTest, ok, info); versionBuilt != want {
		t.Fatalf("versionBuilt = %q, want builtVersion(%q, build info) = %q", versionBuilt, linkedVersionForTest, want)
	}
}

// interruptAbandon runs --abandon with its first step failing once it has
// recorded abandoning.
func (c *guidedSplitCity) interruptAbandon() {
	c.t.Helper()
	prev := infraEngineCrashPoint
	defer func() { infraEngineCrashPoint = prev }()
	infraEngineCrashPoint = func(p string) error {
		if p == "split "+splitPhaseAbandoning {
			return errors.New("city.toml could not be written")
		}
		return nil
	}
	if code, _, _ := c.split(splitOptions{Abandon: true}); code == 0 {
		c.t.Fatal("precondition: the interrupted --abandon succeeded")
	}
	if j := c.journal(); j.Phase != splitPhaseAbandoning {
		c.t.Fatalf("precondition: the journal = %s, want abandoning", j.Phase)
	}
}

// bindingRows counts the rows the city's binding holds.
func (c *guidedSplitCity) bindingRows() int {
	c.t.Helper()
	binding, err := openInfraDestination(c.target())
	if err != nil {
		c.t.Fatal(err)
	}
	defer closeBeadStoreHandle(binding) //nolint:errcheck // test cleanup
	return len(rigTestList(c.t, binding))
}

// TestStorageSplitAnInterruptedAbandonThenBootsRemedyResumes (SHOULD A, A19,
// A23): an --abandon that stops once it recorded abandoning, then the boot
// gate's remedy (`gc storage migrate --from-work`), which converges the
// binding. The binding is the city's state now: --abandon refuses and names
// the split, never "Finish the split", doctor says the same, and the split
// resumes from where it was abandoned and finishes, over a city.toml edited
// since too, which still composes the split's [storage].
func TestStorageSplitAnInterruptedAbandonThenBootsRemedyResumes(t *testing.T) {
	for _, tc := range []struct {
		name, seam, from string
		edit             bool
	}{
		{"abandoned from configured", "copying rows", splitPhaseConfigured, false},
		{"abandoned from configuring", "split city.toml", splitPhaseConfiguring, false},
		{"abandoned from configuring, city.toml edited since", "split city.toml", splitPhaseConfiguring, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newGuidedSplitCity(t)
			c.killSplitAt(tc.seam)
			c.interruptAbandon()
			c.assertBoot("gc storage migrate --from-work")
			var out, errOut bytes.Buffer
			if code := doStorageMigrate(context.Background(), c.reload(), &out, &errOut); code != 0 {
				t.Fatalf("the boot's remedy = %d: %s", code, errOut.String())
			}
			if state, _ := readInfraConvergenceState(c.target()); state != infraConvergenceMarked {
				t.Fatalf("precondition: the binding is %v after the remedy", state)
			}
			if tc.edit {
				c.writeTOML(c.readTOML() + "# an operator's edit after the remedy\n")
			}
			toml, rows := c.readTOML(), c.bindingRows()

			code, _, stderr := c.split(splitOptions{Abandon: true})
			if code == 0 || !strings.Contains(stderr, "resumes the split from "+tc.from) || strings.Contains(stderr, "Finish the split") {
				t.Fatalf("--abandon over the converged binding = %d: %s", code, stderr)
			}
			if c.readTOML() != toml || c.bindingRows() != rows || c.journal().Phase != splitPhaseAbandoning {
				t.Fatal("the refused --abandon changed city.toml, the binding or the journal")
			}
			res := newStorageSplitCheck(c.reload().Cfg, c.request.CityPath).Run(nil)
			if all := strings.Join(append([]string{res.Message}, res.Details...), "\n"); !strings.Contains(all, "resumes the split from "+tc.from) || strings.Contains(all, "is being abandoned") {
				t.Fatalf("doctor over the stopped abandon: %s %v", res.Message, res.Details)
			}

			code, stdout, stderr := c.split(splitOptions{})
			if code != 0 || !strings.Contains(stderr, "the split resumes from "+tc.from) {
				t.Fatalf("the split after the remedy = %d:\n%s\n%s", code, stdout, stderr)
			}
			if j := c.journal(); j.AbandonedFrom != "" {
				t.Fatalf("the resumed split still records an abandon from %s", j.AbandonedFrom)
			}
			c.assertSplit()
			if c.readTOML() != toml {
				t.Fatalf("the resumed split rewrote city.toml:\n%s\nwant it kept as it was:\n%s", c.readTOML(), toml)
			}
		})
	}
}

// TestStorageSplitResumesAtConfiguringOverAnEditedCityTOML: a split killed
// once it wrote city.toml, its journal still at configuring, whose city.toml
// is then edited, with or without boot's remedy (`gc storage migrate
// --from-work`) converging the binding in between. The file is neither one
// the journal records, but it composes the split's [storage], so the split
// resumes, keeps the edit and finishes; the journal keeps the after-hash of
// the file the split wrote, so the reverse removes [storage] in place and
// keeps the edit.
func TestStorageSplitResumesAtConfiguringOverAnEditedCityTOML(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remedy bool
	}{
		{"after boot's remedy converged the binding", true},
		{"unconverged", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newGuidedSplitCity(t)
			c.killSplitAt("split city.toml")
			written := c.journal().CityTOMLAfterSHA256
			if tc.remedy {
				c.assertBoot("gc storage migrate --from-work")
				var out, errOut bytes.Buffer
				if code := doStorageMigrate(context.Background(), c.reload(), &out, &errOut); code != 0 {
					t.Fatalf("the boot's remedy = %d: %s", code, errOut.String())
				}
				if state, _ := readInfraConvergenceState(c.target()); state != infraConvergenceMarked {
					t.Fatalf("precondition: the binding is %v after the remedy", state)
				}
			}
			// The edit is outside [storage], so removing the section in place
			// keeps it.
			const edit = "# an operator's edit while the split was stopped"
			c.writeTOML(strings.Replace(c.readTOML(), "# trailing comment", edit, 1))
			if j := c.journal(); j.Phase != splitPhaseConfiguring || !strings.Contains(c.readTOML(), edit) || sha256Hex([]byte(c.readTOML())) == written {
				t.Fatalf("precondition: the journal is at %s, or city.toml is not edited:\n%s", j.Phase, c.readTOML())
			}

			code, stdout, stderr := c.split(splitOptions{})
			if code != 0 || !strings.Contains(stderr, "in place and keeps the change") {
				t.Fatalf("the split over the edited city.toml = %d:\n%s\n%s", code, stdout, stderr)
			}
			c.assertSplit()
			if !strings.Contains(c.readTOML(), edit) {
				t.Fatalf("the split did not keep the edit:\n%s", c.readTOML())
			}
			if c.journal().CityTOMLAfterSHA256 != written {
				t.Fatal("the resume rewrote the after-hash to the edited file")
			}
			var out, errOut bytes.Buffer
			if code := doStorageReverse(context.Background(), c.reload(), reverseOptions{Mode: reverseToWork}, &out, &errOut); code != 0 {
				t.Fatalf("--to-work = %d:\n%s\n%s", code, out.String(), errOut.String())
			}
			if got := c.readTOML(); !strings.Contains(got, edit) || strings.Contains(got, "[storage") {
				t.Fatalf("city.toml after the reverse:\n%s\nwant the edit kept and [storage] gone", got)
			}
		})
	}
}

// TestStorageSplitAbandonRefusesOverTheClearedNote: a split killed during its
// clear has written the city-side cleared note. With the binding's root moved
// aside its marker reads absent, yet the work store no longer holds the
// city's infrastructure state, so --abandon refuses, names the note and
// changes nothing: the journal, city.toml, the floor and the snapshot's pin
// stay. With the root put back, the split finishes.
func TestStorageSplitAbandonRefusesOverTheClearedNote(t *testing.T) {
	c := newGuidedSplitCity(t)
	c.killSplitAt("clearing")
	target := c.target()
	aside := target.Root + ".aside"
	if err := os.Rename(target.Root, aside); err != nil {
		t.Fatal(err)
	}
	if state, err := readInfraConvergenceState(target); err != nil || state != infraConvergenceAbsent {
		t.Fatalf("precondition: the binding is %v, %v; want absent", state, err)
	}
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	journalPath, floorPath := storageSplitJournalPath(c.request.CityPath), minGCVersionPath(c.request.CityPath)
	j, journal, toml, floor := c.journal(), read(journalPath), c.readTOML(), read(floorPath)
	code, _, stderr := c.split(splitOptions{Abandon: true})
	if code == 0 || !strings.Contains(stderr, infraClearedNotePath(c.request.CityPath)) {
		t.Fatalf("--abandon over a cleared work store = %d: %s", code, stderr)
	}
	if read(journalPath) != journal || c.readTOML() != toml || read(floorPath) != floor || !fileExists(splitSnapshotPinPath(j)) {
		t.Fatal("the refused --abandon changed the journal, city.toml, the floor or the snapshot's pin")
	}
	if err := os.Rename(aside, target.Root); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := c.split(splitOptions{}); code != 0 {
		t.Fatalf("the split with the binding's root back = %d:\n%s\n%s", code, stdout, stderr)
	}
	c.assertSplit()
}

// TestStorageSplitAbandonRefusesAnUnknownPhase (NIT 2): a journal phase this
// build does not know refuses --abandon, as it refuses the split, and nothing
// is changed.
func TestStorageSplitAbandonRefusesAnUnknownPhase(t *testing.T) {
	c := newGuidedSplitCity(t)
	c.killSplitAt("split " + splitPhaseCityConverged)
	j := c.journal()
	j.Phase = "verifying_rigs"
	if err := writeStorageSplitJournal(c.request.CityPath, j); err != nil {
		t.Fatal(err)
	}
	toml := c.readTOML()
	if code, _, stderr := c.split(splitOptions{Abandon: true}); code == 0 || !strings.Contains(stderr, "does not know") {
		t.Fatalf("--abandon of an unknown phase = %d: %s", code, stderr)
	}
	if c.readTOML() != toml || c.journal().Phase != "verifying_rigs" || !fileExists(filepath.Join(j.SnapshotTo, j.Snapshot, storageSnapshotPinName)) {
		t.Fatal("the refused --abandon changed something")
	}
}

// TestStorageSplitAbandonIsHeldByAnOpenReverse (NIT 8): an open reverse holds
// --abandon too.
func TestStorageSplitAbandonIsHeldByAnOpenReverse(t *testing.T) {
	c := newGuidedSplitCity(t)
	c.killSplitAt("copying rows")
	if err := writeStorageReverseJournal(c.request.CityPath, storageReverseJournal{Format: storageReverseJournalFormat, Mode: reverseToWork, Phase: reverseR1, Run: "r"}); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := c.split(splitOptions{Abandon: true}); code == 0 || !strings.Contains(stderr, "reverse migration") {
		t.Fatalf("--abandon during a reverse = %d: %s", code, stderr)
	}
	if c.journal().Phase != splitPhaseConfigured {
		t.Fatal("the held --abandon moved the journal")
	}
}

// TestStorageSplitKeepsTheOldJournalLiveUntilTheNewOne (SHOULD B, edge A): a
// split that supersedes one which wrote the floor keeps the old journal live
// until its own replaces it: a snapshot that fails, or a run killed right
// after the old journal is kept aside, leaves the old journal and its floor
// record live, and the split after carries it; --abandon then removes the
// floor the city did not have before the first split.
func TestStorageSplitKeepsTheOldJournalLiveUntilTheNewOne(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through a read-only directory")
	}
	check := func(t *testing.T, c *guidedSplitCity, a storageSplitJournal) {
		t.Helper()
		if j := c.journal(); j.Run != a.Run || j.Phase != splitPhaseConfiguring {
			t.Fatalf("the live journal = %s at %s, want split A's %s at configuring", j.Run, j.Phase, a.Run)
		}
		c.killSplitAt("copying rows")
		if j := c.journal(); j.Run == a.Run || j.MinGCVersionBefore != nil || !j.MinGCVersionBeforeCarried {
			t.Fatalf("the next split's floor record = %v, carried %t; want split A's (none), carried", j.MinGCVersionBefore, j.MinGCVersionBeforeCarried)
		}
		if code, stdout, stderr := c.split(splitOptions{Abandon: true}); code != 0 {
			t.Fatalf("--abandon = %d:\n%s\n%s", code, stdout, stderr)
		}
		if fileExists(minGCVersionPath(c.request.CityPath)) {
			t.Fatal("--abandon left the floor split A wrote; the city had none before the split")
		}
	}
	t.Run("the snapshot fails", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		c.killSplitAt("split floor")
		a := c.journal()
		snaps := storageSnapshotDefaultDir(c.request.CityPath)
		if err := os.Chmod(snaps, 0o555); err != nil {
			t.Fatal(err)
		}
		code, _, stderr := c.split(splitOptions{})
		if err := os.Chmod(snaps, 0o755); err != nil {
			t.Fatal(err)
		}
		if code == 0 || !strings.Contains(stderr, "the mandatory snapshot") {
			t.Fatalf("the split with an unwritable snapshot directory = %d: %s", code, stderr)
		}
		check(t, c, a)
	})
	t.Run("killed after the old journal is kept aside", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		c.killSplitAt("split floor")
		a := c.journal()
		c.killSplitAt("split retired")
		check(t, c, a)
	})
}

// TestStorageSplitJournalOnlyAbandonRestoresACarriedFloor (SHOULD B, edge B):
// a split stopped before P4a that carried a superseded split's floor record
// puts that floor back when it is abandoned (the file holds the superseded
// split's floor), unless city.toml authors a [storage], whose floor it is.
func TestStorageSplitJournalOnlyAbandonRestoresACarriedFloor(t *testing.T) {
	t.Run("unsplit city.toml", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		c.killSplitAt("split floor")
		c.killSplitAt("split " + splitPhaseReadied)
		if j := c.journal(); !j.MinGCVersionBeforeCarried || j.MinGCVersionBefore != nil {
			t.Fatalf("precondition: split B's record = %v, carried %t", j.MinGCVersionBefore, j.MinGCVersionBeforeCarried)
		}
		toml := c.readTOML()
		code, stdout, stderr := c.split(splitOptions{Abandon: true})
		if code != 0 || !strings.Contains(stdout, "as before the first split") {
			t.Fatalf("--abandon = %d:\n%s\n%s", code, stdout, stderr)
		}
		if fileExists(minGCVersionPath(c.request.CityPath)) {
			t.Fatal("--abandon left the floor the superseded split wrote")
		}
		if c.readTOML() != toml {
			t.Fatal("--abandon changed city.toml")
		}
	})
	t.Run("city.toml authors [storage]", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		c.killSplitAt("split floor")
		c.killSplitAt("split " + splitPhaseReadied)
		c.writeTOML(c.readTOML() + "\n" + splitStorageTOML(splitHypotheticalConfig(c.request.Cfg)))
		if code, stdout, stderr := c.split(splitOptions{Abandon: true}); code != 0 {
			t.Fatalf("--abandon = %d:\n%s\n%s", code, stdout, stderr)
		}
		if floor, present, _ := readMinGCVersion(c.request.CityPath); !present || floor != splitMinGCVersion {
			t.Fatalf("--abandon changed the floor of a city.toml that authors [storage]: %q %t", floor, present)
		}
	})
}

// TestStorageSplitRecordsRigOnlyAttestations (NIT 6): an attestation given
// only for a shared rig store is in the journal's forced list, so the hint
// that continues the split repeats it.
func TestStorageSplitRecordsRigOnlyAttestations(t *testing.T) {
	c := newGuidedSplitCity(t)
	otherRig := filepath.Join(t.TempDir(), "elsewhere")
	other := otherCityWithRig(t, otherRig)
	stubOtherCities(t, other)
	rigs := map[string]bool{canonicalIdentityPath(c.rigPath): true, canonicalIdentityPath(otherRig): true}
	prev := storeDoltIdentityOf
	t.Cleanup(func() { storeDoltIdentityOf = prev })
	storeDoltIdentityOf = func(_, root string) storeDoltIdentity {
		if rigs[canonicalIdentityPath(root)] {
			return storeDoltIdentity{Database: "packs", Endpoint: "loopback:3399"}
		}
		return storeDoltIdentity{Database: "own_" + filepath.Base(root), Endpoint: "loopback:3400"}
	}
	c.request.Force = storageForce{SharedStores: []string{"packs@loopback:3399"}}
	c.killSplitAt("copying rows")
	want := storageHazardSharedStore + ":packs@loopback:3399"
	if j := c.journal(); strings.Join(j.Forced, ",") != want {
		t.Fatalf("the journal's forced = %v, want [%s]", j.Forced, want)
	}
	if res := newStorageSplitCheck(c.reload().Cfg, c.request.CityPath).Run(nil); !strings.Contains(strings.Join(res.Details, "\n"), "--force="+want) {
		t.Fatalf("the continue hint drops the rig's attestation: %v", res.Details)
	}
}

// TestStorageSplitDemandRepairsCloseWhatTheyOpened (D1, D2): the production
// P2 closes the store of every scope its sweep opened, the city and the rig.
func TestStorageSplitDemandRepairsCloseWhatTheyOpened(t *testing.T) {
	c := newGuidedSplitCity(t)
	splitDemandRepairs = productionSplitDemandRepairs
	var closed []string
	prev := splitDemandRepairClosed
	t.Cleanup(func() { splitDemandRepairClosed = prev })
	splitDemandRepairClosed = func(scope string) { closed = append(closed, canonicalIdentityPath(scope)) }
	if code, stdout, stderr := c.split(splitOptions{}); code != 0 {
		t.Fatalf("split = %d:\n%s\n%s", code, stdout, stderr)
	}
	sort.Strings(closed)
	want := []string{canonicalIdentityPath(c.request.CityPath), canonicalIdentityPath(c.rigPath)}
	sort.Strings(want)
	if strings.Join(closed, ",") != strings.Join(want, ",") {
		t.Fatalf("the demand repairs closed %v, want %v", closed, want)
	}
}

// TestPerRunStoreFactoryObservesEachOpenOnce (D2): the observer is told each
// scope the factory opens, once, and not a failed open.
func TestPerRunStoreFactoryObservesEachOpenOnce(t *testing.T) {
	var seen []string
	open := perRunStoreFactory(func(dir string) (beads.Store, error) {
		if dir == "bad" {
			return nil, errors.New("no store")
		}
		return beads.NewMemStore(), nil
	}, func(dir string) { seen = append(seen, dir) })
	for _, dir := range []string{"a", "a", "b", "bad", "bad"} {
		_, _ = open(dir)
	}
	if strings.Join(seen, ",") != "a,b" {
		t.Fatalf("observed %v, want [a b]", seen)
	}
}

// TestStorageSplitKeepsTheEngineAdviceForAnUnreadableBinding (U2): a copy
// that stops with the binding root unreadable keeps the engine's advice,
// which puts the missing-volume hazard first, instead of the split's own.
func TestStorageSplitKeepsTheEngineAdviceForAnUnreadableBinding(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through an unreadable directory")
	}
	c := newGuidedSplitCity(t)
	root := c.target().Root
	prev := infraEngineCrashPoint
	t.Cleanup(func() {
		infraEngineCrashPoint = prev
		_ = os.Chmod(root, 0o755)
	})
	infraEngineCrashPoint = func(p string) error {
		if strings.HasPrefix(p, "copying rows") {
			if err := os.Chmod(root, 0o000); err != nil {
				return err
			}
			return errors.New("the copy failed")
		}
		return nil
	}
	code, _, stderr := c.split(splitOptions{})
	_ = os.Chmod(root, 0o755)
	if code == 0 || !strings.Contains(stderr, "could not read that binding") {
		t.Fatalf("a copy that stops over an unreadable binding = %d: %s", code, stderr)
	}
}

// TestStorageSplitAfterAReverseReadsTheFloorAfresh (F5): a completed split's
// record is history, not carried: after it was reversed (R4 removes the
// floor), the next split records the floor the city has now, none.
func TestStorageSplitAfterAReverseReadsTheFloorAfresh(t *testing.T) {
	c := newGuidedSplitCity(t)
	if err := writeMinGCVersion(c.request.CityPath, "1.5.1"); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := c.split(splitOptions{}); code != 0 {
		t.Fatalf("split = %d:\n%s\n%s", code, stdout, stderr)
	}
	var out, errOut bytes.Buffer
	if code := doStorageReverse(context.Background(), c.reload(), reverseOptions{Mode: reverseToWork}, &out, &errOut); code != 0 {
		t.Fatalf("--to-work = %d:\n%s", code, errOut.String())
	}
	c.killSplitAt("copying rows")
	if j := c.journal(); j.MinGCVersionBefore != nil || j.MinGCVersionBeforeCarried {
		t.Fatalf("the split after the reverse records %v, carried %t; want the file's (none), read", j.MinGCVersionBefore, j.MinGCVersionBeforeCarried)
	}
}

// TestStorageSplitResumeWritesTheJournalsFloor (F9): P4b writes the floor the
// journal recorded at P4a, which a resume does not recompute.
func TestStorageSplitResumeWritesTheJournalsFloor(t *testing.T) {
	c := newGuidedSplitCity(t)
	c.killSplitAt("split city.toml")
	j := c.journal()
	j.MinGCVersion = "1.7.0"
	if err := writeStorageSplitJournal(c.request.CityPath, j); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := c.split(splitOptions{}); code != 0 {
		t.Fatalf("the resume = %d:\n%s\n%s", code, stdout, stderr)
	}
	if floor, _, _ := readMinGCVersion(c.request.CityPath); floor != "1.7.0" {
		t.Fatalf("the floor after the resume = %q, want the journal's 1.7.0", floor)
	}
}

// TestDoctorStorageSplitSaysAnUnwrittenSplitStartsOver (NIT 5): a split
// stopped before it wrote city.toml is started over by the next run, so
// doctor says so, not that the run continues it.
func TestDoctorStorageSplitSaysAnUnwrittenSplitStartsOver(t *testing.T) {
	for _, seam := range []string{"split " + splitPhaseReadied, "split floor"} {
		t.Run(seam, func(t *testing.T) {
			c := newGuidedSplitCity(t)
			c.killSplitAt(seam)
			res := newStorageSplitCheck(c.reload().Cfg, c.request.CityPath).Run(nil)
			if !strings.Contains(res.Message, "starts it over") || strings.Contains(res.Message, "continues it") {
				t.Fatalf("doctor over a split stopped at %s: %s", seam, res.Message)
			}
		})
	}
}
