package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

// fileStoreCity is an unsplit city on a real file work store (GC_BEADS=file,
// .gc/beads.json), with a city.toml on disk and no controller live: the
// state `gc storage split` starts from.
type fileStoreCity struct {
	request storageOperatorRequest
	to      string
	// unsplit is city.toml as the city wrote it, before any split.
	unsplit []byte
}

const fileStoreCityTOML = "# a city that was never split\n[workspace]\nname = \"restore-city\"\n"

func newFileStoreCity(t *testing.T) fileStoreCity {
	t.Helper()
	clearGCEnv(t)
	t.Setenv("GC_BEADS", "file")
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(fileStoreCityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(cityPath, ".gc", "beads.json"), beads.WithFileStoreIDPrefix("rc"))
	if err != nil {
		t.Fatal(err)
	}
	session := mustCreateInfraBead(t, store, beads.Bead{Title: "a session", Type: "session", Labels: []string{"gc:session"}})
	wisp := mustCreateInfraBead(t, store, beads.Bead{Title: "a finished step", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	if err := store.Close(wisp.ID); err != nil {
		t.Fatal(err)
	}
	work := mustCreateInfraBead(t, store, beads.Bead{Title: "real work", Type: "task"})
	if err := store.DepAdd(work.ID, session.ID, "related"); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	stubInfraControllerPing(t, 0)
	stubSessionInventory(t, nil, true, nil)
	stubOtherCities(t)
	stubMigrationWindowProvider(t, &migrationWindowProvider{})
	return fileStoreCity{
		request: storageOperatorRequest{CityPath: cityPath, Cfg: cfg, FleetStopped: true},
		to:      filepath.Join(t.TempDir(), "snapshots"),
		unsplit: []byte(fileStoreCityTOML),
	}
}

func (c fileStoreCity) snapshot(t *testing.T) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := doStorageSnapshot(context.Background(), c.request, snapshotOptions{Work: true, To: c.to, Keep: 5}, "gc storage snapshot", &stdout, &stderr); code != 0 {
		t.Fatalf("snapshot = %d: %s%s", code, stdout.String(), stderr.String())
	}
	id, _ := onlySnapshot(t, c.to)
	return id
}

func (c fileStoreCity) restore(t *testing.T, id string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := doStorageSnapshotRestoreCity(context.Background(), c.request, c.to, id, "gc storage snapshot restore", &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// split migrates the city onto a binding the way the split leaves it: its
// [storage] block in city.toml, the copy and the clear, the id floor and the
// served-binding note.
func (c *fileStoreCity) split(t *testing.T) infraBindingTarget {
	t.Helper()
	text := string(c.unsplit) + "\n[storage.classes]\nwork = \"work\"\ngraph = \"infra\"\nsessions = \"infra\"\nmessaging = \"infra\"\norders = \"infra\"\nnudges = \"infra\"\n\n[storage.bindings.infra]\nprovider = \"sqlite-beads\"\npath = \".gc/store\"\n"
	if err := os.WriteFile(filepath.Join(c.request.CityPath, "city.toml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(minGCVersionPath(c.request.CityPath), []byte("1.6.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(c.request.CityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	if report := migrateInfraClasses(t, c.request.CityPath, cfg, &log); report.Outcome != infraMigrationConverged {
		t.Fatalf("the split = %v: %s", report.Outcome, log.String())
	}
	target := mustResolveInfraTarget(t, c.request.CityPath, cfg)
	plan, err := resolveCityStoragePlan(c.request.CityPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordServedBinding(plan, c.request.CityPath, cfg.EffectiveStorage(), target); err != nil {
		t.Fatal(err)
	}
	c.request.Cfg = cfg
	return target
}

func workStoreBytes(t *testing.T, cityPath string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cityPath, ".gc", "beads.json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestCityRestoreRoundTripsASplit is S-3 acceptance 3: a snapshot taken
// before the split puts the city back as it was. The work store holds every
// row it held, city.toml is the unsplit one, the id floor and the served note
// the split wrote are gone, and the binding root is moved aside, so the city
// serves from its work store alone; nothing the restore replaced is deleted.
func TestCityRestoreRoundTripsASplit(t *testing.T) {
	city := newFileStoreCity(t)
	before := workStoreBytes(t, city.request.CityPath)
	id := city.snapshot(t)
	_, m := onlySnapshot(t, city.to)
	if m.CityRecord == nil || m.CityRecord.CityTOML == nil || m.Stores[0].Exact.Method != snapshotMethodFileCopy {
		t.Fatalf("the snapshot does not record the city and the work store's file: %+v", m)
	}

	target := city.split(t)
	if bytes.Equal(workStoreBytes(t, city.request.CityPath), before) {
		t.Fatal("the split left the work store as it was, so this proves nothing")
	}

	code, stdout, stderr := city.restore(t, id)
	if code != 0 {
		t.Fatalf("restore = %d: %s%s", code, stdout, stderr)
	}
	if got := workStoreBytes(t, city.request.CityPath); !bytes.Equal(got, before) {
		t.Errorf("the restored work store differs from the one snapshotted")
	}
	if got, _ := os.ReadFile(filepath.Join(city.request.CityPath, "city.toml")); !bytes.Equal(got, city.unsplit) {
		t.Errorf("city.toml after the restore:\n%s", got)
	}
	for _, gone := range []string{minGCVersionPath(city.request.CityPath), bornSplitServedNotePath(city.request.CityPath), target.Root} {
		if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the restore (%v)", gone, err)
		}
	}
	j, present, err := readStorageCityRestoreJournal(city.request.CityPath)
	if err != nil || !present || !j.done(cityRestoreStepVerified) {
		t.Fatalf("journal %+v present %v (%v), want it verified", j, present, err)
	}
	for _, kept := range []string{target.Root, minGCVersionPath(city.request.CityPath), filepath.Join(city.request.CityPath, "city.toml"), filepath.Join(city.request.CityPath, ".gc", "beads.json")} {
		if _, err := os.Stat(kept + j.Suffix); err != nil {
			t.Errorf("the replaced %s is not kept as %s (%v)", kept, kept+j.Suffix, err)
		}
	}

	// The restored city is an unsplit one the split can run on again.
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(city.request.CityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage != nil {
		t.Fatalf("the restored city still authors [storage]: %+v", cfg.Storage)
	}
	city.request.Cfg = cfg
	city.split(t)
}

// TestCityRestoreFinishesWhatAnInterruptedRunStarted: a restore stopped
// part-way holds every other writing storage command off, names itself, and
// the same command finishes it.
func TestCityRestoreFinishesWhatAnInterruptedRunStarted(t *testing.T) {
	city := newFileStoreCity(t)
	before := workStoreBytes(t, city.request.CityPath)
	id := city.snapshot(t)
	city.split(t)

	prev := storageSnapshotStep
	storageSnapshotStep = func(step string) error {
		if step == "city restore "+cityRestoreStepRecords {
			return errors.New("killed in this test")
		}
		return nil
	}
	code, _, stderr := city.restore(t, id)
	storageSnapshotStep = prev
	if code == 0 || !strings.Contains(stderr, "run this command again to finish") {
		t.Fatalf("the interrupted restore = %d: %s", code, stderr)
	}
	var mstdout, mstderr bytes.Buffer
	if code := doStorageMigrate(context.Background(), city.request, &mstdout, &mstderr); code == 0 || !strings.Contains(mstderr.String(), "city restore of snapshot "+id+" is unfinished") {
		t.Fatalf("a migration during the unfinished restore = %d: %s", code, mstderr.String())
	}
	if code, stdout, stderr := city.restore(t, id); code != 0 || !strings.Contains(stderr, "finishing the city restore") {
		t.Fatalf("the rerun = %d: %s%s", code, stdout, stderr)
	}
	if got := workStoreBytes(t, city.request.CityPath); !bytes.Equal(got, before) {
		t.Error("the finished restore's work store differs from the snapshot's")
	}
}

// TestCityRestoreRefusesWhatItCannotPutBack: every refusal comes before the
// first move, so the city is as it was.
func TestCityRestoreRefusesWhatItCannotPutBack(t *testing.T) {
	t.Run("without the attestation", func(t *testing.T) {
		city := newFileStoreCity(t)
		id := city.snapshot(t)
		city.request.FleetStopped = false
		if code, _, stderr := city.restore(t, id); code == 0 || !strings.Contains(stderr, "--fleet-stopped") {
			t.Fatalf("restore = %d: %s", code, stderr)
		}
	})
	for _, tc := range []struct {
		name string
		edit func(m *snapshotManifest)
		want string
	}{
		{"a store with a Dolt tag", func(m *snapshotManifest) { m.Stores[0].Exact = snapshotExact{Method: snapshotMethodDoltTag, Tag: "t"} }, "Dolt tag"},
		{"a store with a bd export", func(m *snapshotManifest) { m.Stores[0].Exact.Method = snapshotMethodBdExport }, "bd export"},
		{"a store with no copy", func(m *snapshotManifest) { m.Stores[0].Exact = snapshotExact{Method: snapshotMethodNone} }, "no exact copy"},
		{"a binding", func(m *snapshotManifest) {
			m.Stores = append(m.Stores, snapshotStoreRecord{Name: "binding", Exact: snapshotExact{Method: snapshotMethodVacuumInto}})
		}, "restore its database with --binding"},
		{"no city record", func(m *snapshotManifest) { m.CityRecord = nil }, "records no city"},
		{"another work store", func(m *snapshotManifest) { m.Identity = "file:/elsewhere" }, "taken of work store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := newFileStoreCity(t)
			id := city.snapshot(t)
			_, m := onlySnapshot(t, city.to)
			tc.edit(&m)
			if _, err := planCityRestore(city.request, m); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("planCityRestore = %v, want %q", err, tc.want)
			}
			// The command proves the manifest first; the plan's refusals
			// are what an honest manifest of that shape meets.
			_ = id
		})
	}
	t.Run("a running managed Dolt", func(t *testing.T) {
		city := newFileStoreCity(t)
		city.snapshot(t)
		_, m := onlySnapshot(t, city.to)
		m.Stores[0].Exact = snapshotExact{Method: snapshotMethodDoltDataDir, Tree: &snapshotCopiedTree{Path: snapshotCityDoltDir, Files: []snapshotFile{{Path: snapshotCityDoltDir + "/x"}}}}
		prevDir := snapshotManagedDoltDataDir
		snapshotManagedDoltDataDir = func(string) (string, error) { return t.TempDir(), nil }
		t.Cleanup(func() { snapshotManagedDoltDataDir = prevDir })
		prevRunning := snapshotManagedDoltRunning
		snapshotManagedDoltRunning = func(string) bool { return true }
		t.Cleanup(func() { snapshotManagedDoltRunning = prevRunning })
		if _, err := planCityRestore(city.request, m); err == nil || !strings.Contains(err.Error(), "managed Dolt is running") {
			t.Fatalf("planCityRestore = %v, want the running Dolt refused", err)
		}
	})
}

// TestSnapshotRecordsTheCity: a snapshot with the work store records
// city.toml and every storage record, present or absent, and verify proves
// them; a tampered copy does not verify.
func TestSnapshotRecordsTheCity(t *testing.T) {
	city := newFileStoreCity(t)
	if err := os.WriteFile(minGCVersionPath(city.request.CityPath), []byte("1.6.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id := city.snapshot(t)
	_, m := onlySnapshot(t, city.to)
	files := map[string]bool{}
	for _, f := range m.CityRecord.Files {
		files[f.Rel] = f.File != nil
	}
	if !files[filepath.Join(".gc", minGCVersionFileName)] {
		t.Errorf("the id floor file is not recorded present: %v", files)
	}
	if present, ok := files[rel(city.request.CityPath, bornSplitServedNotePath(city.request.CityPath))]; !ok || present {
		t.Errorf("the absent served note is not recorded absent: %v", files)
	}
	dir := filepath.Join(city.to, id)
	if err := os.WriteFile(filepath.Join(dir, m.CityRecord.CityTOML.Path), []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifySnapshotFiles(context.Background(), dir, m); err == nil {
		t.Fatal("a snapshot whose city.toml copy changed verifies")
	}
}

// TestSplitPreflightSaysWhetherToWorkCanCarryEveryEdge: an edge the copy
// carries with a payload cannot go back through --to-work on native Dolt,
// so preflight --split names the city restore as the way back there.
func TestSplitPreflightSaysWhetherToWorkCanCarryEveryEdge(t *testing.T) {
	pair := beads.DepPair{IssueID: "gc-a", DependsOnID: "gc-b"}
	carrying := infraCopyPlan{copied: map[beads.DepPair]bool{pair: true}, payloads: map[beads.DepPair]string{pair: `{"gate":"waits_for"}`}}
	bare := infraCopyPlan{copied: map[beads.DepPair]bool{pair: true}, payloads: map[beads.DepPair]string{}}
	for _, tc := range []struct {
		name  string
		plan  infraCopyPlan
		diag  beads.BeadsDiagnostic
		known bool
		tag   string
		want  string
	}{
		{"no payload", bare, nativeDoltLane, true, preflightPassTag, "can carry every one back"},
		{"a payload on native Dolt", carrying, nativeDoltLane, true, preflightNoteTag, "--city"},
		{"a payload on a lane that writes one", carrying, beads.BeadsDiagnostic{Store: beads.BeadsStoreNameFileStore}, true, preflightPassTag, "can carry them back"},
	} {
		var out bytes.Buffer
		splitPreflightReverseNote(tc.plan, tc.diag, tc.known, &out)
		if !preflightRow(out.String(), tc.tag, "way back") || !strings.Contains(out.String(), tc.want) {
			t.Errorf("%s: %s", tc.name, out.String())
		}
	}
}

// TestCityRestoreHoldsBootUntilEveryFileIsBack: a city restore stopped before
// its last move holds gc start, as an open reverse and a binding restore do,
// and names the command that finishes it. Booted part-way, the city would
// serve stores, records and a city.toml from different moments, and every
// write it took would make the restore's own proof fail. Once every file is
// back, boot is the restored city's own.
func TestCityRestoreHoldsBootUntilEveryFileIsBack(t *testing.T) {
	boot := func(t *testing.T, city fileStoreCity) error {
		t.Helper()
		cfg, err := loadCityConfigWithoutBuiltinPackRefresh(city.request.CityPath, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		_, err = storageBootGate(city.request.CityPath, cfg, "gc start", nil, io.Discard)
		return err
	}
	for _, step := range []string{cityRestoreStepStores, cityRestoreStepRecords, cityRestoreStepCityTOML} {
		t.Run("stopped after "+step, func(t *testing.T) {
			city := newFileStoreCity(t)
			id := city.snapshot(t)
			city.split(t)
			prev := storageSnapshotStep
			storageSnapshotStep = func(s string) error {
				if s == "city restore "+step {
					return errors.New("killed in this test")
				}
				return nil
			}
			code, _, stderr := city.restore(t, id)
			storageSnapshotStep = prev
			if code == 0 {
				t.Fatalf("the restore was not interrupted: %s", stderr)
			}
			err := boot(t, city)
			if err == nil || !strings.Contains(err.Error(), "city restore of snapshot "+id+" is unfinished") || !strings.Contains(err.Error(), snapshotCityRestoreInstruction(id, city.to)) {
				t.Fatalf("boot over the part-restored city = %v; want it held, naming the restore and the command that finishes it", err)
			}
			if code, stdout, stderr := city.restore(t, id); code != 0 {
				t.Fatalf("finishing the restore = %d: %s%s", code, stdout, stderr)
			}
			if err := boot(t, city); err != nil {
				t.Fatalf("boot after the finished restore = %v", err)
			}
		})
	}
	t.Run("every file back, the proof still to run", func(t *testing.T) {
		city := newFileStoreCity(t)
		id := city.snapshot(t)
		city.split(t)
		prev := storageSnapshotStep
		storageSnapshotStep = func(s string) error {
			if s == "city restore "+cityRestoreStepBinding {
				return errors.New("killed in this test")
			}
			return nil
		}
		code, _, _ := city.restore(t, id)
		storageSnapshotStep = prev
		if code == 0 {
			t.Fatal("the restore was not interrupted")
		}
		if err := boot(t, city); err != nil {
			t.Fatalf("boot with every file back = %v; the proof that remains holds the storage commands, not the city", err)
		}
	})
}

// TestCityRestoreKeepsCityTOMLWhenItsInstallFails: city.toml is what every
// gc loads the city from, the command that finishes an interrupted restore
// among them, so a restore that fails while putting it back leaves the one it
// replaces in place, and the same command then finishes.
func TestCityRestoreKeepsCityTOMLWhenItsInstallFails(t *testing.T) {
	city := newFileStoreCity(t)
	id := city.snapshot(t)
	city.split(t)
	tomlPath := filepath.Join(city.request.CityPath, "city.toml")
	split, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	// The install's temporary file cannot be created: a directory holds its
	// name.
	obstacle := tomlPath + ".restoring"
	if err := os.MkdirAll(filepath.Join(obstacle, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := city.restore(t, id); code == 0 || !strings.Contains(stderr, "run this command again to finish") {
		t.Fatalf("the restore = %d: %s", code, stderr)
	}
	if got, err := os.ReadFile(tomlPath); err != nil || !bytes.Equal(got, split) {
		t.Fatalf("city.toml after the failed install: %q (%v); want the split one still in place", got, err)
	}
	if err := os.RemoveAll(obstacle); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := city.restore(t, id); code != 0 {
		t.Fatalf("finishing the restore = %d: %s%s", code, stdout, stderr)
	}
	if got, _ := os.ReadFile(tomlPath); !bytes.Equal(got, city.unsplit) {
		t.Errorf("city.toml after the restore:\n%s", got)
	}
	j, _, err := readStorageCityRestoreJournal(city.request.CityPath)
	if err != nil {
		t.Fatal(err)
	}
	if kept, err := os.ReadFile(tomlPath + j.Suffix); err != nil || !bytes.Equal(kept, split) {
		t.Errorf("the replaced city.toml is not kept as %s: %q (%v)", tomlPath+j.Suffix, kept, err)
	}
}

// TestCityRestoreRefusesASnapshotOfASplitCity: the city restore puts back a
// city from before the split. A --work snapshot of a split city holds no
// binding, so restoring it would move the binding, the only copy of the
// city's infrastructure state, aside and leave a split city.toml pointing at
// nothing. It is refused before anything moves.
func TestCityRestoreRefusesASnapshotOfASplitCity(t *testing.T) {
	city := newFileStoreCity(t)
	target := city.split(t)
	id := city.snapshot(t)
	code, stdout, stderr := city.restore(t, id)
	if code == 0 || !strings.Contains(stderr, "taken of a split city") {
		t.Fatalf("restore = %d: %s%s; want the snapshot of a split city refused", code, stdout, stderr)
	}
	if _, err := os.Stat(target.MarkerPath()); err != nil {
		t.Errorf("the binding root moved: %v", err)
	}
	if _, present, _ := readStorageCityRestoreJournal(city.request.CityPath); present {
		t.Error("a refused restore wrote its journal")
	}
}

// TestCityRestoreRefusesARigImportedSinceTheSnapshot: a rig import moves
// rows out of the rig's store into the binding root the city restore moves
// aside. A snapshot that holds no copy of that rig's store, taken before the
// import, would leave those rows in no store the city reads, so it is refused
// (the binding restore's snapshotRigImportsLost is the same rule).
func TestCityRestoreRefusesARigImportedSinceTheSnapshot(t *testing.T) {
	city := newFileStoreCity(t)
	rigPath := t.TempDir()
	text := fileStoreCityTOML + fmt.Sprintf("\n[[rigs]]\nname = \"packs\"\npath = %q\nprefix = \"gp\"\n", rigPath)
	if err := os.WriteFile(filepath.Join(city.request.CityPath, "city.toml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(city.request.CityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	city.request.Cfg = cfg
	id := city.snapshot(t)
	_, m := onlySnapshot(t, city.to)
	if _, err := planCityRestore(city.request, m); err != nil {
		t.Fatalf("a rig never imported refuses: %v", err)
	}
	note := rigGraphClearedNotePath(city.request.CityPath, "packs")
	if err := writeInfraClearedNoteAt(note, infraClearedNote{Binding: "infra", Complete: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := planCityRestore(city.request, m); err == nil || !strings.Contains(err.Error(), "rig:packs") || !strings.Contains(err.Error(), note) {
		t.Fatalf("planCityRestore = %v; want rig packs, imported since the snapshot, refused", err)
	}
	_ = id
}

// TestSnapshotOfTheManagedDoltDataDirKeepsTheDoltFloorFree: the copy of a
// managed Dolt's data directory is refused before the server stops when it
// would leave less than the managed Dolt's critical floor free on the
// snapshot's filesystem, where a city's Dolt (this one's or another's) stops
// writing. With room, the server is stopped for the copy and started again.
func TestSnapshotOfTheManagedDoltDataDirKeepsTheDoltFloorFree(t *testing.T) {
	cityPath, lock := windowCity(t)
	provider := &migrationWindowProvider{down: true}
	stubMigrationWindowProvider(t, provider)
	window, err := openTestWindow(t, lock, cityPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = window.Close() })
	dataDir := t.TempDir()
	const size = 1 << 20
	if err := os.MkdirAll(filepath.Join(dataDir, "hq", ".dolt", "noms"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "hq", ".dolt", "noms", "journal.idx"), bytes.Repeat([]byte("x"), size), 0o644); err != nil {
		t.Fatal(err)
	}
	prevDir := snapshotManagedDoltDataDir
	snapshotManagedDoltDataDir = func(string) (string, error) { return dataDir, nil }
	t.Cleanup(func() { snapshotManagedDoltDataDir = prevDir })
	const floor = 8 << 20
	t.Setenv("GC_DOLT_MIN_FREE_BYTES", strconv.Itoa(floor))
	t.Setenv("GC_DOLT_WARN_FREE_BYTES", strconv.Itoa(floor))
	free := int64(floor + size/2)
	prevFree := doltContainerFreeBytesFunc
	doltContainerFreeBytesFunc = func(string) (int64, error) { return free, nil }
	t.Cleanup(func() { doltContainerFreeBytesFunc = prevFree })
	stopsBefore, startsBefore := provider.stops, provider.starts

	if _, _, err := snapshotDoltDataDir(cityPath, t.TempDir()); err == nil || !strings.Contains(err.Error(), "must leave") {
		t.Fatalf("snapshotDoltDataDir = %v; want the copy refused for the floor", err)
	}
	if provider.stops != stopsBefore || provider.starts != startsBefore {
		t.Fatalf("the server was stopped (%d) or started (%d) for a copy that was refused", provider.stops-stopsBefore, provider.starts-startsBefore)
	}

	free = 1 << 40
	exact, why, err := snapshotDoltDataDir(cityPath, t.TempDir())
	if err != nil || exact == nil || exact.Method != snapshotMethodDoltDataDir {
		t.Fatalf("snapshotDoltDataDir with room = %+v, %q, %v", exact, why, err)
	}
	if provider.stops != stopsBefore+1 || provider.starts != startsBefore+1 {
		t.Errorf("stops %d, starts %d; want the server stopped once for the copy and started again", provider.stops-stopsBefore, provider.starts-startsBefore)
	}
}

// TestSnapshotTreeKeepsEmptyDirectories: a data directory's empty
// directories are part of it (a Dolt git remote's cache keeps its refs in
// one, and git does not read a repository whose refs directory is gone), so
// the snapshot records them and the restore puts them back.
func TestSnapshotTreeKeepsEmptyDirectories(t *testing.T) {
	src := t.TempDir()
	for _, d := range []string{"gp/.dolt/git-remote-cache/c/repo.git/refs/heads", "hq/.dolt/temptf"} {
		if err := os.MkdirAll(filepath.Join(src, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "gp", ".dolt", "git-remote-cache", "c", "repo.git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tree, err := copySnapshotTree(dir, src, snapshotCityDoltDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifySnapshotTree(dir, tree); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "dolt")
	if err := installSnapshotTree(dir, *tree, dst); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"gp/.dolt/git-remote-cache/c/repo.git/refs/heads", "hq/.dolt/temptf"} {
		if info, err := os.Stat(filepath.Join(dst, d)); err != nil || !info.IsDir() {
			t.Errorf("the empty directory %s did not come back: %v", d, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(dst, "gp", ".dolt", "git-remote-cache", "c", "repo.git", "HEAD")); err != nil || string(got) != "ref: refs/heads/main\n" {
		t.Errorf("the file did not come back: %q (%v)", got, err)
	}
}

// TestCityRestoreProvesTheRestoredStoreAgainstTheJSONL: the restore's last
// step reads each restored store back and proves its rows equal to the
// snapshot's JSONL. A snapshot whose exact copy and JSONL disagree (each
// proven by its own hash) restores files that do not hold what the snapshot
// says, so the restore does not report success, and the journal stays open.
func TestCityRestoreProvesTheRestoredStoreAgainstTheJSONL(t *testing.T) {
	city := newFileStoreCity(t)
	id := city.snapshot(t)
	city.split(t)
	dir := filepath.Join(city.to, id)
	m, err := readSnapshotManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(dir, m.Stores[0].Exact.File.Path)
	store, err := beads.OpenFileStore(fsys.OSFS{}, copyPath, beads.WithFileStoreIDPrefix("rc"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := store.List(beads.ListQuery{IncludeClosed: true, AllowScan: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatal(err)
	}
	// The work row: every reader sees it, the split-strict gate's work view
	// included.
	dropped := ""
	for _, b := range rows {
		if b.Title == "real work" {
			dropped = b.ID
		}
	}
	if dropped == "" {
		t.Fatalf("the copy holds no work row: %+v", rows)
	}
	if err := store.Delete(dropped); err != nil {
		t.Fatal(err)
	}
	file, err := hashSnapshotFile(dir, copyPath)
	if err != nil {
		t.Fatal(err)
	}
	m.Stores[0].Exact.File = &file
	if err := os.Remove(filepath.Join(dir, storageSnapshotManifestName)); err != nil {
		t.Fatal(err)
	}
	if err := publishSnapshotManifest(context.Background(), dir, m); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := city.restore(t, id)
	if code == 0 || !strings.Contains(stderr, "verified: work:") {
		t.Fatalf("restore = %d: %s%s; want the proof against the JSONL to fail", code, stdout, stderr)
	}
	if j, present, err := readStorageCityRestoreJournal(city.request.CityPath); err != nil || !present || j.done(cityRestoreStepVerified) {
		t.Fatalf("journal %+v present %v (%v); want it open, the proof not done", j, present, err)
	}
}
