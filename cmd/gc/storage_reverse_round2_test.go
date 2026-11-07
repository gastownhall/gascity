package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// withOldClearEdge leaves the work store as a build whose clear re-added the
// edges it kept left it: a work bead W with an edge onto an open infra row D
// that only the binding holds. It returns W and D.
func withOldClearEdge(t *testing.T, city reverseEngineCity) (string, string) {
	t.Helper()
	var w, d string
	for id, b := range city.afterForward {
		if !isInfra(b) && b.Metadata[beadmeta.MigratedCrossEdgesMetadataKey] == "" && (w == "" || id < w) {
			w = id
		}
	}
	for id, b := range city.before {
		if isInfra(b) && b.Type == "task" && b.Status == "open" && (d == "" || id < d) {
			d = id
		}
	}
	if err := city.source.DepAdd(w, d, "blocks"); err != nil {
		t.Fatalf("adding the old clear's edge %s -> %s: %v", w, d, err)
	}
	city.extraEdges[[2]string{w, d}] = "blocks"
	return w, d
}

func hasEdge(t *testing.T, store beads.Store, from, to string) bool {
	t.Helper()
	deps, err := store.DepList(from, "down")
	if err != nil {
		t.Fatal(err)
	}
	for _, dep := range deps {
		if dep.DependsOnID == to {
			return true
		}
	}
	return false
}

// TestReverseKeepsTheEdgesAStoreHeldBeforeIt (MUST 1): an edge the work store
// held onto a row only the binding held (W -> D, left by an old clear) is
// recorded before the reverse writes anything. An abort puts it back after
// deleting D, a resume that re-copies D puts it back with D, and a finished
// reverse keeps it.
func TestReverseKeepsTheEdgesAStoreHeldBeforeIt(t *testing.T) {
	t.Run("abort", func(t *testing.T) {
		city := newReverseEngineCity(t, 300, "")
		closeBindingGates(t, city)
		w, d := withOldClearEdge(t, city)
		before := workStoreFingerprint(t, city.source)
		killReverseAt(t, city, "reverse city verify")
		if !hasEdge(t, city.source, w, d) {
			t.Fatal("the reverse lost the old clear's edge while copying")
		}
		if code, stdout, stderr := city.abort(t); code != 0 {
			t.Fatalf("--abort-reverse = %d: %s%s", code, stdout, stderr)
		}
		if got := workStoreFingerprint(t, city.source); got != before {
			t.Fatalf("the abort did not leave the work store as it was (W -> D):\n%s", got)
		}
	})
	t.Run("resume re-copy", func(t *testing.T) {
		city := newReverseEngineCity(t, 300, "")
		closeBindingGates(t, city)
		w, d := withOldClearEdge(t, city)
		killReverseAt(t, city, "reverse city verify")
		retitleInBinding(t, city, d, "changed since the copy")
		if code, stdout, stderr := city.reverse(t, reverseOptions{}); code != 0 {
			t.Fatalf("the resume = %d: %s%s", code, stdout, stderr)
		}
		if !hasEdge(t, city.source, w, d) {
			t.Fatalf("the resume that re-copied %s lost %s -> %s", d, w, d)
		}
		city.before[d] = bindingRowAsBeforeFromArchive(t, city, d)
		city.assertReversedNotLossy(t)
	})
	t.Run("a store that refuses the edge back refuses the abort", func(t *testing.T) {
		city := newReverseEngineCity(t, 300, "")
		closeBindingGates(t, city)
		w, d := withOldClearEdge(t, city)
		killReverseAt(t, city, "reverse city verify")
		before := workStoreFingerprint(t, city.source)
		prev := reverseTargetOpener
		reverseTargetOpener = func(cityPath string, cfg *config.City, dest string) (*reverseTarget, error) {
			target, err := openReverseTarget(cityPath, cfg, dest)
			if err == nil {
				target.acceptsDangling = func(string, string) bool { return false }
			}
			return target, err
		}
		code, _, stderr := city.abort(t)
		reverseTargetOpener = prev
		if code == 0 || !strings.Contains(stderr, fmt.Sprintf("%s -[blocks]-> %s", w, d)) || !strings.Contains(stderr, "Nothing was removed") {
			t.Fatalf("--abort-reverse over a store that refuses the edge back = %d: %s", code, stderr)
		}
		if got := workStoreFingerprint(t, city.source); got != before {
			t.Fatal("the refused abort removed rows")
		}
	})
}

func killReverseAt(t *testing.T, city reverseEngineCity, point string) {
	t.Helper()
	prev := infraEngineCrashPoint
	infraEngineCrashPoint = func(p string) error {
		if strings.HasPrefix(p, point) {
			return errors.New("killed at " + point)
		}
		return nil
	}
	code, _, stderr := city.reverse(t, reverseOptions{})
	infraEngineCrashPoint = prev
	if code == 0 || !strings.Contains(stderr, "killed at "+point) {
		t.Fatalf("the kill at %s did not stop the reverse: %d: %s", point, code, stderr)
	}
}

func retitleInBinding(t *testing.T, city reverseEngineCity, id, title string) {
	t.Helper()
	binding, err := openInfraDestination(city.target)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBeadStoreHandle(binding) //nolint:errcheck // test cleanup
	if err := binding.Update(id, beads.UpdateOpts{Title: &title}); err != nil {
		t.Fatal(err)
	}
}

// bindingRowAsBefore is the binding's row id as the work store held it
// before the split: without the forward stamp and its record.
func bindingRowAsBefore(t *testing.T, city reverseEngineCity, id string) beads.Bead {
	t.Helper()
	binding, err := openInfraBindingReadOnly(city.target)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBeadStoreHandle(binding) //nolint:errcheck // test cleanup
	b, err := binding.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	delete(b.Metadata, infraMigrationStampKey)
	delete(b.Metadata, beadmeta.MigratedCrossEdgesMetadataKey) // every recorded edge is restored
	return b
}

// TestReverseReprovesTheBindingBeforeItChanges (SHOULD 9): a binding that
// changes after its rows were copied, before the reverse manifest (R3) or
// before the archive (R5), puts the reverse back to R0, and the next run
// copies the change.
func TestReverseReprovesTheBindingBeforeItChanges(t *testing.T) {
	for _, tc := range []struct {
		name, after string
		kill        bool
	}{
		{"before R3", "reverse phase " + reversePhaseName(reverseR2), false},
		{"before R5, on a resume", "reverse phase " + reversePhaseName(reverseR4), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := newReverseEngineCity(t, 120, "")
			closeBindingGates(t, city)
			var changed string
			for id, b := range city.before {
				if isInfra(b) && (changed == "" || id < changed) {
					changed = id
				}
			}
			prev := infraEngineCrashPoint
			infraEngineCrashPoint = func(p string) error {
				if p != tc.after {
					return nil
				}
				retitleInBinding(t, city, changed, "written after the copy")
				if tc.kill {
					return errors.New("killed")
				}
				return nil
			}
			code, _, stderr := city.reverse(t, reverseOptions{})
			infraEngineCrashPoint = prev
			if tc.kill {
				if code == 0 {
					t.Fatal("the kill did not stop the reverse")
				}
				code, _, stderr = city.reverse(t, reverseOptions{})
			}
			if code == 0 || !strings.Contains(stderr, "the binding changed after its rows were copied") {
				t.Fatalf("a run over a changed binding = %d: %s", code, stderr)
			}
			if journal, _, _ := readStorageReverseJournal(city.cityPath); journal.Phase != reverseR0 {
				t.Fatalf("journal phase %d, want R0", journal.Phase)
			}
			if !fileExists(city.target.Root) {
				t.Fatal("the binding was archived over a changed binding")
			}
			if code, stdout, stderr := city.reverse(t, reverseOptions{}); code != 0 {
				t.Fatalf("the next run = %d: %s%s", code, stdout, stderr)
			}
			city.before[changed] = bindingRowAsBeforeFromArchive(t, city, changed)
			city.assertReversedNotLossy(t)
			city.assertRevertedCityServesSingleStore(t)
		})
	}
}

// bindingRowAsBeforeFromArchive is bindingRowAsBefore read from the archived
// binding root.
func bindingRowAsBeforeFromArchive(t *testing.T, city reverseEngineCity, id string) beads.Bead {
	t.Helper()
	archived, _ := filepath.Glob(city.target.Root + ".reverted-*")
	if len(archived) != 1 {
		t.Fatalf("archives %v", archived)
	}
	moved := city
	moved.target = infraBindingTarget{Binding: city.target.Binding, Root: archived[0], Dir: filepath.Join(archived[0], filepath.Base(city.target.Dir)), Database: filepath.Join(archived[0], filepath.Base(city.target.Dir), filepath.Base(city.target.Database))}
	return bindingRowAsBefore(t, moved, id)
}

// TestReverseRunsNeverShareAStamp (MUST 3): run ids are unique to the
// nanosecond and a random suffix; a destination already holding the new
// stamp refuses; and an abort deletes only from the journal's destinations.
func TestReverseRunsNeverShareAStamp(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := reverseRunID(now)
		if seen[id] {
			t.Fatalf("two runs in one instant share the id %s", id)
		}
		seen[id] = true
	}
	in := planInput([]beads.Bead{planRow("mc-1", "closed", "work", "")}, nil)
	in.Fresh = true
	in.Held[rigImportStamp("packs")]["gp-9"] = reverseRow(planRow("gp-9", "closed", "", ""), planTestStamp, false)
	if got := planRefusal(planReverse(in)); !strings.Contains(got, "gp-9 (the store of rig packs)") || !strings.Contains(got, "under this run's stamp") {
		t.Fatalf("a destination holding the new stamp = %q", got)
	}

	city := newReverseEngineCity(t, 120, "")
	closeBindingGates(t, city)
	killReverseAt(t, city, "reverse city verify")
	journal, _, _ := readStorageReverseJournal(city.cityPath)
	if strings.Join(journal.Dests, ",") != reverseCityDest {
		t.Fatalf("the journal names destinations %v, want the city alone", journal.Dests)
	}
	opened := map[string]bool{}
	prev := reverseTargetOpener
	reverseTargetOpener = func(cityPath string, cfg *config.City, dest string) (*reverseTarget, error) {
		opened[dest] = true
		return openReverseTarget(cityPath, cfg, dest)
	}
	t.Cleanup(func() { reverseTargetOpener = prev })
	city.request.Cfg.Rigs = append(city.request.Cfg.Rigs, config.Rig{Name: "added-since", Path: t.TempDir()})
	if code, stdout, stderr := city.abort(t); code != 0 {
		t.Fatalf("--abort-reverse = %d: %s%s", code, stdout, stderr)
	}
	if opened[rigImportStamp("added-since")] {
		t.Fatal("the abort opened a rig configured after the reverse started")
	}
}

// TestReverseToRigsValidatesItsRigs (MUST 2): a --rig that is a pattern, a
// typo, a rig that was never imported, or one sharing the city's store is
// refused before anything is locked, opened or renamed.
func TestReverseToRigsValidatesItsRigs(t *testing.T) {
	r := newReverseRigFixture(t)
	records := map[string]string{}
	for _, rig := range []string{"packs", "other"} {
		loc := r.loc(rig)
		for _, path := range []string{loc.MarkerPath(), loc.ManifestPath(), loc.CleanPath(), loc.NotePath()} {
			if data, err := os.ReadFile(path); err == nil {
				records[path] = string(data)
			}
		}
	}
	for _, tc := range []struct{ rig, want string }{
		{"*", "is not a rig name"},
		{"pack?", "is not a rig name"},
		{"typo", "configures no rig \"typo\""},
		{"other", "it was never imported"},
	} {
		code, _, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{tc.rig}})
		if code == 0 || !strings.Contains(stderr, tc.want) {
			t.Fatalf("--to-rigs --rig %q = %d: %s, want %q", tc.rig, code, stderr, tc.want)
		}
	}
	if fileExists(storageReverseJournalPath(r.request.CityPath)) {
		t.Fatal("a refused --to-rigs wrote its journal")
	}
	for path, data := range records {
		if got, err := os.ReadFile(path); err != nil || string(got) != data {
			t.Fatalf("a refused --to-rigs changed %s", path)
		}
	}
}

// TestReverseRefusesAPendingCityClearOnce (SHOULD 12): the work store holding
// the clear's unfinished copies is one refusal naming the clear, not one per
// row.
func TestReverseRefusesAPendingCityClearOnce(t *testing.T) {
	city := newReverseEngineCity(t, 120, "")
	closeBindingGates(t, city)
	note, _, _ := readInfraClearedNote(city.cityPath)
	note.Complete, note.Pending = false, []string{"mc-000001"}
	if err := writeInfraClearedNote(city.cityPath, note); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := city.reverse(t, reverseOptions{})
	if code == 0 || !strings.Contains(stderr, "REFUSED — 1 shape(s)") || !strings.Contains(stderr, storageClearInstruction()) {
		t.Fatalf("--to-work over a pending clear = %d: %s", code, stderr)
	}
}

// TestReverseRefusesAFragmentDeclaredStorageBeforeTheCopy (SHOULD 7): R4's
// edit is composed in memory at plan time, so a [storage] a fragment
// declares is refused before anything is written.
func TestReverseRefusesAFragmentDeclaredStorageBeforeTheCopy(t *testing.T) {
	city := newReverseEngineCity(t, 60, "")
	closeBindingGates(t, city)
	path := filepath.Join(city.cityPath, "city.toml")
	data, _ := os.ReadFile(path)
	text := string(data)
	split := strings.Index(text, "[storage.bindings.infra]")
	fragment := "[storage.bindings.infra]\nprovider = \"sqlite-beads\"\npath = \".gc/store\"\n"
	if err := os.WriteFile(filepath.Join(city.cityPath, "storage-fragment.toml"), []byte(fragment), 0o644); err != nil {
		t.Fatal(err)
	}
	root := "include = [\"storage-fragment.toml\"]\n" + text[:split]
	if err := os.WriteFile(path, []byte(root), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCityConfigWithoutBuiltinPackRefresh(city.cityPath, io.Discard); err != nil {
		t.Skipf("this build composes no include of that shape: %v", err)
	}
	before := workStoreFingerprint(t, city.source)
	code, _, stderr := city.reverse(t, reverseOptions{})
	if code == 0 || !strings.Contains(stderr, "city.toml") {
		t.Fatalf("--to-work with a fragment-declared [storage] = %d: %s", code, stderr)
	}
	if after := workStoreFingerprint(t, city.source); after != before || fileExists(storageReverseJournalPath(city.cityPath)) {
		t.Fatal("the refusal came after the copy began")
	}
}

// TestReverseOnACityPathWithGlobCharacters (SHOULD 8): the split's notes are
// found by name in the city's .gc, never by a pattern over the city path.
func TestReverseOnACityPathWithGlobCharacters(t *testing.T) {
	city := newReverseEngineCity(t, 60, "")
	closeBindingGates(t, city)
	odd := filepath.Join(t.TempDir(), "city[1]")
	if err := os.Rename(city.cityPath, odd); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(odd, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	city.cityPath, city.request.CityPath, city.request.Cfg = odd, odd, cfg
	city.target = mustResolveInfraTarget(t, odd, infraSplitConfig(".gc/store"))
	if err := writeInfraClearedNoteAt(rigGraphClearedNotePath(odd, "packs"), infraClearedNote{Complete: true}); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := city.reverse(t, reverseOptions{}); code != 0 {
		t.Fatalf("--to-work on %s = %d: %s%s", odd, code, stdout, stderr)
	}
	for _, path := range []string{bornSplitServedNotePath(odd), infraClearedNotePath(odd), rigGraphClearedNotePath(odd, "packs")} {
		if fileExists(path) {
			t.Fatalf("%s was not retired on a city path with glob characters", path)
		}
	}
}

// TestReverseKeepsAnOlderUnpinnedSnapshot (SHOULD 14): R0's snapshot runs no
// retention, so the snapshots taken before it, the pre-split one among them,
// survive.
func TestReverseKeepsAnOlderUnpinnedSnapshot(t *testing.T) {
	city := newReverseEngineCity(t, 60, "")
	closeBindingGates(t, city)
	to := storageSnapshotDefaultDir(city.cityPath)
	var older []string
	for i := 0; i < 3; i++ {
		var out, errOut bytes.Buffer
		if code := doStorageSnapshot(context.Background(), city.request, snapshotOptions{Binding: true, To: to, Keep: 10}, "snapshot", &out, &errOut); code != 0 {
			t.Fatalf("snapshot = %d: %s", code, errOut.String())
		}
		complete, _, _ := snapshotIDs(to)
		older = complete
		time.Sleep(1100 * time.Millisecond)
	}
	if code, stdout, stderr := city.reverse(t, reverseOptions{}); code != 0 {
		t.Fatalf("--to-work = %d: %s%s", code, stdout, stderr)
	}
	for _, id := range older {
		if !fileExists(filepath.Join(to, id)) {
			t.Fatalf("the reverse's snapshot pruned the older snapshot %s", id)
		}
	}
}

// TestDoctorSeesAnOpenReverse (SHOULD 15): past R4 the city carries no
// [storage], and the storage-split doctor check still sees the open reverse.
func TestDoctorSeesAnOpenReverse(t *testing.T) {
	city := newReverseEngineCity(t, 60, "")
	closeBindingGates(t, city)
	killReverseAt(t, city, "reverse phase "+reversePhaseName(reverseR4))
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(city.cityPath, io.Discard)
	if err != nil || cfg.Storage != nil {
		t.Fatalf("precondition: R4 removed [storage]: %v %v", cfg.Storage, err)
	}
	result := newStorageSplitCheck(cfg, city.cityPath).Run(nil)
	if !strings.Contains(result.Message, "reverse migration") {
		t.Fatalf("doctor storage-split on an open reverse = %+v", result)
	}
}

// TestReverseDryRunIsALiveRehearsal (SHOULD 17): --dry-run takes no lock and
// no attestation and writes nothing, so it runs beside a live controller.
func TestReverseDryRunIsALiveRehearsal(t *testing.T) {
	city := newReverseEngineCity(t, 60, "")
	closeBindingGates(t, city)
	lock, err := acquireControllerLock(city.cityPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close() //nolint:errcheck // test cleanup
	city.request.FleetStopped = false
	before := workStoreFingerprint(t, city.source)
	code, stdout, stderr := city.reverse(t, reverseOptions{DryRun: true})
	if code != 0 || !strings.Contains(stdout, "Dry run") {
		t.Fatalf("--dry-run beside a held controller lock = %d: %s%s", code, stdout, stderr)
	}
	if after := workStoreFingerprint(t, city.source); after != before || fileExists(storageReverseJournalPath(city.cityPath)) || fileExists(migrationWindowRecordPath(city.cityPath)) {
		t.Fatal("--dry-run wrote something")
	}
}

// TestReverseRefusesAProxiedDestination (SHOULD 6): a store whose foreign-id
// create falls back to a plain create is refused at plan time.
func TestReverseRefusesAProxiedDestination(t *testing.T) {
	city := newReverseEngineCity(t, 60, "")
	closeBindingGates(t, city)
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return mintingFallbackStore{Store: city.source}, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	code, _, stderr := city.reverse(t, reverseOptions{})
	if code == 0 || !strings.Contains(stderr, "falls back to a plain create") {
		t.Fatalf("--to-work into a proxied store = %d: %s", code, stderr)
	}
}

// mintingFallbackStore declares, as a ProxiedStore over a bd leaf does, that
// its foreign-id create does not keep the row.
type mintingFallbackStore struct{ beads.Store }

func (mintingFallbackStore) ForeignIDCreateKeepsRows() bool { return false }

// TestReverseAbortCrashAfterRowsResumes: an abort killed after it removed the
// rows (the abort-reverse rows point) is finished by running it again.
func TestReverseAbortCrashAfterRowsResumes(t *testing.T) {
	city := newReverseEngineCity(t, 120, "")
	closeBindingGates(t, city)
	before := workStoreFingerprint(t, city.source)
	killReverseAt(t, city, "reverse phase "+reversePhaseName(reverseR4))
	prev := infraEngineCrashPoint
	infraEngineCrashPoint = func(p string) error {
		if p == "abort-reverse rows" {
			return errors.New("killed")
		}
		return nil
	}
	code, _, _ := city.abort(t)
	infraEngineCrashPoint = prev
	if code == 0 {
		t.Fatal("the kill did not stop the abort")
	}
	if _, err := storageBootGate(city.cityPath, city.request.Cfg, "gc start", nil, io.Discard); err == nil {
		t.Fatal("boot served a city whose abort did not finish")
	}
	if code, stdout, stderr := city.abort(t); code != 0 {
		t.Fatalf("the second abort = %d: %s%s", code, stdout, stderr)
	}
	if got := workStoreFingerprint(t, city.source); got != before {
		t.Fatal("the resumed abort did not leave the work store as it was")
	}
	if _, err := storageBootGate(city.cityPath, city.request.Cfg, "gc start", nil, io.Discard); err != nil {
		t.Fatalf("boot after the resumed abort = %v", err)
	}
}
