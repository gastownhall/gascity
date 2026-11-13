package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// fileStoreCity is an unsplit city on a real file work store (GC_BEADS=file,
// .gc/beads.json), with a city.toml on disk and no controller live: the
// state the split starts from.
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
	return c.restoreWith(t, id, false)
}

// acceptUnproven runs the restore of id with --accept-unproven.
func (c fileStoreCity) acceptUnproven(t *testing.T, id string) (int, string, string) {
	t.Helper()
	return c.restoreWith(t, id, true)
}

func (c fileStoreCity) restoreWith(t *testing.T, id string, acceptUnproven bool) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := doStorageSnapshotRestoreCity(context.Background(), c.request, c.to, id, acceptUnproven, "gc storage snapshot restore", &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// boot runs the storage gate gc start runs, on the city's config as it is
// on disk.
func (c fileStoreCity) boot(t *testing.T) error {
	t.Helper()
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(c.request.CityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	_, err = storageBootGate(c.request.CityPath, cfg, "gc start", nil, io.Discard)
	return err
}

// stubSnapshotScopeDoltDatabase puts each scope root dbs names in the city's
// managed Dolt, as the database it maps to; every other scope lives
// elsewhere.
func stubSnapshotScopeDoltDatabase(t *testing.T, dbs map[string]string) {
	t.Helper()
	prev := snapshotScopeDoltDatabase
	t.Cleanup(func() { snapshotScopeDoltDatabase = prev })
	snapshotScopeDoltDatabase = func(_, scopeRoot string) string {
		for root, db := range dbs {
			if samePath(root, scopeRoot) {
				return db
			}
		}
		return ""
	}
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
			// The command proves the manifest first; the plan's refusals
			// are what an honest manifest of that shape meets.
			if _, err := planCityRestore(city.request, filepath.Join(city.to, id), m); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("planCityRestore = %v, want %q", err, tc.want)
			}
		})
	}
	t.Run("a running managed Dolt", func(t *testing.T) {
		city := newFileStoreCity(t)
		id := city.snapshot(t)
		_, m := onlySnapshot(t, city.to)
		m.Stores[0].Exact = dataDirExact("hq", "hq")
		prevDir := snapshotManagedDoltDataDir
		snapshotManagedDoltDataDir = func(string) (string, error) { return t.TempDir(), nil }
		t.Cleanup(func() { snapshotManagedDoltDataDir = prevDir })
		prevRunning := snapshotManagedDoltRunning
		snapshotManagedDoltRunning = func(string) bool { return true }
		t.Cleanup(func() { snapshotManagedDoltRunning = prevRunning })
		if _, err := planCityRestore(city.request, filepath.Join(city.to, id), m); err == nil || !strings.Contains(err.Error(), "managed Dolt is running") {
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

// TestSplitPreflightSaysWhetherToWorkCanCarryEveryEdge: --to-work carries
// an edge's payload only into a work store whose lane writes one
// (beads.DepMetadataWriter, which only the SQLite engine implements), so
// preflight --split names the way back by what the work store can do: the
// reverse, the city restore when the split's snapshot copies every store of
// the city so the restore puts it back (a file store's file, a managed Dolt's
// data directory), and otherwise none, naming the store it would not.
func TestSplitPreflightSaysWhetherToWorkCanCarryEveryEdge(t *testing.T) {
	pair := beads.DepPair{IssueID: "gc-a", DependsOnID: "gc-b"}
	carrying := infraCopyPlan{copied: map[beads.DepPair]bool{pair: true}, payloads: map[beads.DepPair]string{pair: `{"gate":"waits_for"}`}}
	bare := infraCopyPlan{copied: map[beads.DepPair]bool{pair: true}, payloads: map[beads.DepPair]string{}}
	const unrestorable = "the split's snapshot of rig:packs is a Dolt tag, a bd export or no copy, which the city restore does not put back"
	for _, tc := range []struct {
		name           string
		plan           infraCopyPlan
		writesPayloads bool
		unrestorable   string
		tag            string
		want           string
	}{
		{"no payload", bare, false, unrestorable, preflightPassTag, "can carry every one back"},
		{"a payload a lane writes", carrying, true, unrestorable, preflightPassTag, "can carry them back"},
		{"a payload in the managed Dolt or a file store", carrying, false, "", preflightNoteTag, "--city"},
		{"a payload in a city a store of which the restore does not put back", carrying, false, unrestorable, preflightNoteTag, unrestorable + ", so no automated way back"},
	} {
		var out bytes.Buffer
		splitPreflightReverseNote(tc.plan, tc.writesPayloads, tc.unrestorable, &out)
		if !preflightRow(out.String(), tc.tag, "way back") || !strings.Contains(out.String(), tc.want) {
			t.Errorf("%s: %s", tc.name, out.String())
		}
	}
	file, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatal(err)
	}
	// preflightWorkStore closes the store it opened.
	sqlite, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		store beads.Store
		want  bool
	}{{"a file store", file, false}, {"the SQLite engine", sqlite, true}} {
		prev := openInfraMigrationSource
		openInfraMigrationSource = func(string) (beads.Store, error) { return tc.store, nil }
		var found preflightFindings
		preflightWorkStore(storageOperatorRequest{CityPath: t.TempDir(), Cfg: &config.City{}}, &found, io.Discard, io.Discard)
		openInfraMigrationSource = prev
		if !found.read || found.writesPayloads != tc.want {
			t.Errorf("%s: read %v, writes payloads %v; want %v", tc.name, found.read, found.writesPayloads, tc.want)
		}
	}
	city := newFileStoreCity(t)
	if why := cityRestoreUnrestorable(city.request); why != "" {
		t.Errorf("a file store city's snapshot is no copy the city restore puts back: %s", why)
	}
	// A rig whose store is neither a file nor in the managed Dolt beside the
	// work store takes a bd export, which the city restore refuses. GC_BEADS
	// names the city's provider alone, so the rig's own metadata decides its.
	t.Setenv("GC_BEADS_SCOPE_ROOT", city.request.CityPath)
	rig := t.TempDir()
	writeScopeBeadsMetadata(t, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"gp"}`)
	city.request.Cfg.Rigs = []config.Rig{{Name: "packs", Path: rig}}
	if why := cityRestoreUnrestorable(city.request); !strings.Contains(why, "rig:packs") || strings.Contains(why, "work") {
		t.Errorf("cityRestoreUnrestorable with a bd-export rig = %q; want rig:packs named, the work store not", why)
	}
	// In the managed Dolt beside the work store, the rig is in the copy of the
	// data directory the snapshot takes.
	stubSnapshotScopeDoltDatabase(t, map[string]string{city.request.CityPath: "hq", rig: "gp"})
	if why := cityRestoreUnrestorable(city.request); why != "" {
		t.Errorf("cityRestoreUnrestorable with the rig in the managed Dolt = %q; want restorable", why)
	}
}

// TestCityRestoreHoldsBootUntilItEnds: a city restore stopped at any step
// holds gc start, as an open reverse and a binding restore do, and names the
// command that finishes it. Booted part-way, the city would serve stores,
// records and a city.toml from different moments; booted with every file
// back and the proof still to run, every row it wrote would fail that proof
// for good. Once the restore ends, boot is the restored city's own.
func TestCityRestoreHoldsBootUntilItEnds(t *testing.T) {
	for _, step := range []string{cityRestoreStepStores, cityRestoreStepRecords, cityRestoreStepCityTOML, cityRestoreStepBinding} {
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
			err := city.boot(t)
			if err == nil || !strings.Contains(err.Error(), "city restore of snapshot "+id+" is unfinished") || !strings.Contains(err.Error(), snapshotCityRestoreInstruction(id, city.to)) {
				t.Fatalf("boot over the unfinished restore = %v; want it held, naming the restore and the command that finishes it", err)
			}
			if every := step == cityRestoreStepBinding; every != strings.Contains(err.Error(), "--"+cityRestoreAcceptFlag) {
				t.Errorf("boot refusal with every file back = %v names --%s: %v; want it named once only the proof is left", every, cityRestoreAcceptFlag, err)
			}
			if code, stdout, stderr := city.restore(t, id); code != 0 {
				t.Fatalf("finishing the restore = %d: %s%s", code, stdout, stderr)
			}
			if err := city.boot(t); err != nil {
				t.Fatalf("boot after the finished restore = %v", err)
			}
		})
	}
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
	dir := filepath.Join(city.to, id)
	_, m := onlySnapshot(t, city.to)
	if _, err := planCityRestore(city.request, dir, m); err != nil {
		t.Fatalf("a rig never imported refuses: %v", err)
	}
	note := rigGraphClearedNotePath(city.request.CityPath, "packs")
	if err := writeInfraClearedNoteAt(note, infraClearedNote{Binding: "infra", Complete: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := planCityRestore(city.request, dir, m); err == nil || !strings.Contains(err.Error(), "rig:packs") || !strings.Contains(err.Error(), note) || !strings.Contains(err.Error(), cityRestoreSnapshotCommand) {
		t.Fatalf("planCityRestore = %v; want rig packs, imported since the snapshot, refused, naming the snapshot that holds it", err)
	}
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
	// The copy was taken well after every row was written, so no row of the
	// JSONL can have been written during the snapshot: the disagreement is
	// the copy's.
	m.Stores[0].Exact.CopiedAt = time.Now().UTC().Add(time.Hour)
	republishSnapshotManifest(t, dir, m)
	code, stdout, stderr := city.restore(t, id)
	if code == 0 || !strings.Contains(stderr, "verified: 1 row(s) fail the proof") || !strings.Contains(stderr, "work: "+dropped+": the snapshot's JSONL holds it, and the restored store does not") {
		t.Fatalf("restore = %d: %s%s; want the proof against the JSONL to fail on %s", code, stdout, stderr, dropped)
	}
	// Every file is back: the failure names the way out, not a part-restored
	// city.
	if strings.Contains(stderr, "part-restored") || !strings.Contains(stderr, "--"+cityRestoreAcceptFlag) {
		t.Errorf("the failed proof's message: %s; want --%s named and no part-restored city", stderr, cityRestoreAcceptFlag)
	}
	if j, present, err := readStorageCityRestoreJournal(city.request.CityPath); err != nil || !present || j.ended() {
		t.Fatalf("journal %+v present %v (%v); want it open, the proof not done", j, present, err)
	}
}

// republishSnapshotManifest replaces dir's manifest with m.
func republishSnapshotManifest(t *testing.T, dir string, m snapshotManifest) {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, storageSnapshotManifestName)); err != nil {
		t.Fatal(err)
	}
	if err := publishSnapshotManifest(context.Background(), dir, m); err != nil {
		t.Fatal(err)
	}
}

// TestSnapshotRecordsWhenItsCopyWasTaken: the exact copy carries the moment
// it was taken, so a restore can tell a row written after the copy (and
// before the JSONL read) from a copy that lost it.
func TestSnapshotRecordsWhenItsCopyWasTaken(t *testing.T) {
	city := newFileStoreCity(t)
	before := time.Now().UTC()
	city.snapshot(t)
	after := time.Now().UTC()
	_, m := onlySnapshot(t, city.to)
	got := m.Stores[0].Exact.CopiedAt
	if got.Before(before) || got.After(after) {
		t.Fatalf("copied_at = %s, want it within the snapshot's run [%s, %s]", got, before, after)
	}
}

// TestCityRestoreReportsRowsWrittenDuringTheSnapshot: the snapshot reads the
// JSONL after the copy (a managed Dolt is started again for it), so a row
// written in between is in the JSONL and not in the copy. The restore puts
// the copy back, reports each such row as written during the snapshot, and
// finishes: the proof does not fail forever and hold every writing storage
// verb behind its journal.
func TestCityRestoreReportsRowsWrittenDuringTheSnapshot(t *testing.T) {
	city := newFileStoreCity(t)
	// Work rows only: every reader sees them, the split-strict gate's work
	// view included.
	live, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(city.request.CityPath, ".gc", "beads.json"), beads.WithFileStoreIDPrefix("rc"))
	if err != nil {
		t.Fatal(err)
	}
	mustCreateInfraBead(t, live, beads.Bead{Title: "more work", Type: "task"})
	id := city.snapshot(t)
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
	// The copy as it would be had it been taken before "real work" was
	// created and before "more work" was retitled.
	var created, changed string
	var earliest time.Time
	for _, b := range rows {
		if earliest.IsZero() || b.CreatedAt.Before(earliest) {
			earliest = b.CreatedAt
		}
		switch b.Title {
		case "real work":
			created = b.ID
		case "more work":
			changed = b.ID
		}
	}
	if created == "" || changed == "" {
		t.Fatalf("the copy lacks the rows the test changes: %+v", rows)
	}
	if err := store.Delete(created); err != nil {
		t.Fatal(err)
	}
	title := "more work, before its retitle"
	if err := store.Update(changed, beads.UpdateOpts{Title: &title}); err != nil {
		t.Fatal(err)
	}
	file, err := hashSnapshotFile(dir, copyPath)
	if err != nil {
		t.Fatal(err)
	}
	m.Stores[0].Exact.File = &file
	// The copy was taken before every row, and the JSONL read after this
	// test's edits to the copy: the edited rows are the snapshot's window.
	m.Stores[0].Exact.CopiedAt = earliest.Add(-time.Minute)
	m.Stores[0].JSONLReadAt = time.Now().UTC()
	republishSnapshotManifest(t, dir, m)

	city.split(t)
	code, stdout, stderr := city.restore(t, id)
	if code != 0 {
		t.Fatalf("restore = %d: %s%s; want rows written during the snapshot reported, not a failed proof", code, stdout, stderr)
	}
	j, present, err := readStorageCityRestoreJournal(city.request.CityPath)
	if err != nil || !present || !j.done(cityRestoreStepVerified) {
		t.Fatalf("journal %+v present %v (%v); want the proof done", j, present, err)
	}
	jsonl, aside := filepath.Join(dir, m.Stores[0].JSONL.Path), filepath.Join(city.request.CityPath, ".gc", "beads.json")+j.Suffix
	for _, want := range []string{
		"work: 1 row(s) changed during the snapshot", "are restored as the copy found them: " + changed,
		"work: 1 row(s) created during the snapshot", "are not restored: " + created,
		"in the snapshot's JSONL " + jsonl, "in the store kept aside as " + aside,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if err := storageCityRestoreRefusal(city.request.CityPath); err != nil {
		t.Fatalf("writing verbs still refused after the restore: %v", err)
	}
}

// TestCityRestoreProofFailsAStoreRowTheJSONLLacks is a row deleted between
// the copy and the JSONL read: the restored store holds it, the JSONL does
// not and so cannot date it, and the proof fails on every run. The city
// stays held, boot and the writing storage verbs alike, with the snapshot
// pinned, and --accept-unproven is the way out: it records the row in the
// journal and, after the acceptance, in the attestation log, which a later
// restore does not replace as it does the journal; keeps and names every file
// the restore replaced, unpins the snapshot and releases the city.
func TestCityRestoreProofFailsAStoreRowTheJSONLLacks(t *testing.T) {
	city := newFileStoreCity(t)
	id := city.snapshot(t)
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
	extra := mustCreateInfraBead(t, store, beads.Bead{Title: "only in the copy", Type: "task"})
	file, err := hashSnapshotFile(dir, copyPath)
	if err != nil {
		t.Fatal(err)
	}
	m.Stores[0].Exact.File = &file
	m.Stores[0].Exact.CopiedAt = time.Unix(0, 0).UTC()
	republishSnapshotManifest(t, dir, m)

	target := city.split(t)
	code, stdout, stderr := city.restore(t, id)
	if code == 0 || !strings.Contains(stderr, extra.ID) {
		t.Fatalf("restore = %d: %s%s; want the proof to fail on %s", code, stdout, stderr, extra.ID)
	}
	pin := filepath.Join(dir, storageSnapshotPinName)
	if !fileExists(pin) {
		t.Error("the snapshot the unfinished restore reads is not pinned, so retention may remove it")
	}
	for name, err := range map[string]error{"boot": city.boot(t), "a writing storage verb": storageCityRestoreRefusal(city.request.CityPath)} {
		if err == nil || !strings.Contains(err.Error(), "--"+cityRestoreAcceptFlag) {
			t.Errorf("%s while the proof fails = %v; want it held, naming --%s", name, err, cityRestoreAcceptFlag)
		}
	}
	if code, _, stderr := city.restore(t, id); code == 0 || !strings.Contains(stderr, extra.ID) {
		t.Fatalf("the rerun = %d: %s; want the same proof to fail again", code, stderr)
	}

	code, stdout, stderr = city.acceptUnproven(t, id)
	if code != 0 {
		t.Fatalf("--%s = %d: %s%s", cityRestoreAcceptFlag, code, stdout, stderr)
	}
	j, _, err := readStorageCityRestoreJournal(city.request.CityPath)
	if err != nil || !j.ended() || !j.done(cityRestoreStepAccepted) || j.done(cityRestoreStepVerified) {
		t.Fatalf("journal %+v (%v); want it ended, accepted and not verified", j, err)
	}
	if len(j.Unproven) != 1 || !strings.Contains(j.Unproven[0], "work: "+extra.ID+": ") {
		t.Errorf("the journal records %q; want the row the proof failed on", j.Unproven)
	}
	for _, want := range []string{"unproven", extra.ID, filepath.Join(city.request.CityPath, ".gc", "beads.json") + j.Suffix, target.Root + j.Suffix, filepath.Join(city.request.CityPath, "city.toml") + j.Suffix} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the accepted restore's report lacks %q:\n%s", want, stdout)
		}
	}
	log, err := os.ReadFile(filepath.Join(city.request.CityPath, ".gc", storageAttestationLog))
	if err != nil || !strings.Contains(string(log), "#gc:attested "+cityRestoreUnprovenHazard+":"+id+" ") {
		t.Errorf("the attestation log %q (%v) does not record the accepted restore", log, err)
	}
	if want := cityRestoreUnprovenTag + " " + cityRestoreUnprovenHazard + ":" + id + " work: " + extra.ID + ": "; !strings.Contains(string(log), want) {
		t.Errorf("the attestation log %q does not record the row the proof failed on (%q)", log, want)
	}
	if fileExists(pin) {
		t.Error("the ended restore left its snapshot pinned")
	}
	if err := city.boot(t); err != nil {
		t.Errorf("boot after the accepted restore = %v", err)
	}
	if err := storageCityRestoreRefusal(city.request.CityPath); err != nil {
		t.Errorf("writing verbs after the accepted restore: %v", err)
	}
	if code, _, stderr := city.acceptUnproven(t, id); code == 0 || !strings.Contains(stderr, "none is unfinished") {
		t.Errorf("--%s with no unfinished restore = %d: %s", cityRestoreAcceptFlag, code, stderr)
	}
}

// TestCityRestoreAcceptUnprovenNeedsEveryFileBack: --accept-unproven ends a
// restore whose proof fails, never one with files left to move: that city is
// part-way back, and the restore finishes it.
func TestCityRestoreAcceptUnprovenNeedsEveryFileBack(t *testing.T) {
	city := newFileStoreCity(t)
	id := city.snapshot(t)
	city.split(t)
	prev := storageSnapshotStep
	storageSnapshotStep = func(s string) error {
		if s == "city restore "+cityRestoreStepRecords {
			return errors.New("killed in this test")
		}
		return nil
	}
	code, _, _ := city.restore(t, id)
	storageSnapshotStep = prev
	if code == 0 {
		t.Fatal("the restore was not interrupted")
	}
	if code, _, stderr := city.acceptUnproven(t, id); code == 0 || !strings.Contains(stderr, "files left to move") {
		t.Fatalf("--%s part-way = %d: %s; want it refused", cityRestoreAcceptFlag, code, stderr)
	}
	j, _, err := readStorageCityRestoreJournal(city.request.CityPath)
	if err != nil || j.ended() || j.done(cityRestoreStepCityTOML) {
		t.Fatalf("journal %+v (%v); want the refused run to have moved nothing", j, err)
	}
	if code, stdout, stderr := city.restore(t, id); code != 0 {
		t.Fatalf("finishing the restore = %d: %s%s", code, stdout, stderr)
	}
}

// TestCityRestorePinsItsSnapshotUntilItEnds: the restore pins the snapshot
// it reads for as long as it is unfinished, so a later snapshot's retention
// cannot remove it from under the command that finishes it; a snapshot
// pinned before (the reverse's) keeps its pin.
func TestCityRestorePinsItsSnapshotUntilItEnds(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		city := newFileStoreCity(t)
		id := city.snapshot(t)
		city.split(t)
		pin := filepath.Join(city.to, id, storageSnapshotPinName)
		if pinned {
			if err := pinStorageSnapshot(city.to, id, "pinned before this test's restore"); err != nil {
				t.Fatal(err)
			}
		}
		prev := storageSnapshotStep
		storageSnapshotStep = func(s string) error {
			if s == "city restore "+cityRestoreStepStores {
				return errors.New("killed in this test")
			}
			return nil
		}
		code, _, _ := city.restore(t, id)
		storageSnapshotStep = prev
		if code == 0 {
			t.Fatal("the restore was not interrupted")
		}
		removed, err := applySnapshotRetention(city.to, 1, "", snapshotOwner{City: city.request.CityPath, Identity: mustCurrentWorkStoreIdentity(t, city.request.CityPath)})
		if err != nil || len(removed) != 0 || !fileExists(pin) {
			t.Fatalf("pinned before %v: retention removed %v (%v), pin present %v; want the restore's snapshot kept", pinned, removed, err, fileExists(pin))
		}
		if code, stdout, stderr := city.restore(t, id); code != 0 {
			t.Fatalf("finishing the restore = %d: %s%s", code, stdout, stderr)
		}
		if got, err := os.ReadFile(pin); pinned != (err == nil) || (pinned && !strings.Contains(string(got), "before this test")) {
			t.Errorf("pinned before %v: after the restore the pin reads %q (%v); want only a pin the restore did not write", pinned, got, err)
		}
	}
}

// mustCurrentWorkStoreIdentity is the city's work store identity.
func mustCurrentWorkStoreIdentity(t *testing.T, cityPath string) workStoreIdentity {
	t.Helper()
	identity, err := currentWorkStoreIdentity(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

// TestSnapshotWindowHolds pins the snapshot's window, the only rows the
// city restore's proof excuses: from the copy's own second through the JSONL
// read's, to the second at both ends (a store may keep its times to the
// second, and Dolt rounds a DATETIME to the nearest one, so a write in the
// read's own second may be stored in the next), and nothing for a snapshot
// that recorded either moment as zero.
func TestSnapshotWindowHolds(t *testing.T) {
	copiedAt := time.Date(2026, 11, 13, 10, 0, 5, 700_000_000, time.UTC)
	readAt := copiedAt.Add(3 * time.Second).Add(200 * time.Millisecond)
	// A write at 10:00:08.6, before the read at 10:00:08.9, that Dolt stores
	// as 10:00:09.
	rounded := time.Date(2026, 11, 13, 10, 0, 8, 600_000_000, time.UTC).Round(time.Second)
	for _, tc := range []struct {
		name         string
		at           time.Time
		copied, read time.Time
		want         bool
	}{
		{"the copy's own second, before the copy", copiedAt.Truncate(time.Second), copiedAt, readAt, true},
		{"the copy's own second, after the copy", copiedAt.Add(100 * time.Millisecond), copiedAt, readAt, true},
		{"the second before the copy's", copiedAt.Truncate(time.Second).Add(-time.Nanosecond), copiedAt, readAt, false},
		{"at the JSONL read", readAt, copiedAt, readAt, true},
		{"a write in the read's own second, stored rounded into the next", rounded, copiedAt, readAt, true},
		{"after the second the read's own rounds into", readAt.Truncate(time.Second).Add(time.Second + time.Nanosecond), copiedAt, readAt, false},
		{"no copy time", copiedAt.Add(time.Second), time.Time{}, readAt, false},
		{"no JSONL read time", copiedAt.Add(time.Second), copiedAt, time.Time{}, false},
	} {
		if got := snapshotWindowHolds(tc.at, tc.copied, tc.read); got != tc.want {
			t.Errorf("%s: snapshotWindowHolds(%s, %s, %s) = %v, want %v", tc.name, tc.at, tc.copied, tc.read, got, tc.want)
		}
	}
	created := copiedAt.Add(time.Second)
	if got := snapshotRowChangedAt(beads.Bead{CreatedAt: created}); !got.Equal(created) {
		t.Errorf("a row with no update time changed at %s, want its creation %s", got, created)
	}
	if got := snapshotRowChangedAt(beads.Bead{CreatedAt: created, UpdatedAt: readAt}); !got.Equal(readAt) {
		t.Errorf("an updated row changed at %s, want its update %s", got, readAt)
	}
}

// TestRestoredStoreProofJudgesEachRow: rows the JSONL dates before the copy
// fail when they differ, while rows it dates in the window are reported as
// changed or created during the snapshot; a restored row changed after the
// JSONL read (a write after the restore) is no copy's and fails, and so does
// a restored row the JSONL lacks.
func TestRestoredStoreProofJudgesEachRow(t *testing.T) {
	copiedAt := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	readAt := copiedAt.Add(time.Minute)
	before, during, after := copiedAt.Add(-time.Hour), copiedAt.Add(30*time.Second), readAt.Add(time.Hour)
	row := func(id, title string, at time.Time) beads.Bead {
		return beads.Bead{ID: id, Title: title, Type: "task", Status: "open", CreatedAt: before, UpdatedAt: at}
	}
	proof := restoredStoreProof{store: cityRestoreStore{rec: snapshotStoreRecord{Name: "work", Exact: snapshotExact{CopiedAt: copiedAt}, JSONLReadAt: readAt}}}
	proof.judgeHeld(row("rc-same", "same", before), row("rc-same", "same", before), true)
	proof.judgeHeld(row("rc-old", "the copy's", before), row("rc-old", "changed before the copy", before), true)
	proof.judgeHeld(row("rc-window", "the copy's", before), row("rc-window", "changed in the window", during), true)
	proof.judgeHeld(row("rc-later", "changed after the restore", after), row("rc-later", "the JSONL's", during), true)
	proof.judgeHeld(row("rc-extra", "only restored", before), beads.Bead{}, false)
	proof.judgeMissing(row("rc-created", "created in the window", during))
	proof.judgeMissing(row("rc-lost", "lost before the copy", before))
	if strings.Join(proof.changedDuring, ",") != "rc-window" || strings.Join(proof.createdDuring, ",") != "rc-created" {
		t.Errorf("excused changed %v, created %v; want rc-window and rc-created", proof.changedDuring, proof.createdDuring)
	}
	var failed []string
	for _, f := range proof.failed {
		failed = append(failed, strings.SplitN(f, ":", 2)[0])
	}
	if strings.Join(failed, ",") != "rc-old,rc-later,rc-extra,rc-lost" {
		t.Errorf("failed on %v; want rc-old, rc-later, rc-extra and rc-lost", proof.failed)
	}
}

// TestCityRestoreRefusesAConfigThatLeavesTheCitySplit: the restore puts
// back city.toml alone, so the configuration it leaves is the snapshot's
// city.toml composed with the city's include fragments as they are now. A
// [storage] in either (the snapshot's city.toml, when it was taken after the
// split was authored; an include fragment, which the runbook allows) would
// leave a split city that does not boot once the binding root is moved
// aside, so the restore refuses before it moves anything, naming the file.
func TestCityRestoreRefusesAConfigThatLeavesTheCitySplit(t *testing.T) {
	const storageBlock = "\n[storage.classes]\nwork = \"work\"\ngraph = \"infra\"\nsessions = \"infra\"\nmessaging = \"infra\"\norders = \"infra\"\nnudges = \"infra\"\n\n[storage.bindings.infra]\nprovider = \"sqlite-beads\"\npath = \".gc/store\"\n"
	writeCityTOML := func(t *testing.T, city *fileStoreCity, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(city.request.CityPath, "city.toml"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadCityConfigWithoutBuiltinPackRefresh(city.request.CityPath, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		city.request.Cfg = cfg
	}
	assertRefused := func(t *testing.T, city fileStoreCity, id, want string) {
		t.Helper()
		before, _ := os.ReadFile(filepath.Join(city.request.CityPath, "city.toml"))
		code, stdout, stderr := city.restore(t, id)
		if code == 0 || !strings.Contains(stderr, "routes infrastructure classes off the work store") || !strings.Contains(stderr, want) {
			t.Fatalf("restore = %d: %s%s; want it refused, naming %q", code, stdout, stderr, want)
		}
		if _, present, _ := readStorageCityRestoreJournal(city.request.CityPath); present {
			t.Error("a refused restore wrote its journal")
		}
		if after, _ := os.ReadFile(filepath.Join(city.request.CityPath, "city.toml")); !bytes.Equal(after, before) {
			t.Errorf("a refused restore changed city.toml:\n%s", after)
		}
	}
	t.Run("in the snapshot's city.toml", func(t *testing.T) {
		city := newFileStoreCity(t)
		writeCityTOML(t, &city, fileStoreCityTOML+storageBlock)
		id := city.snapshot(t)
		assertRefused(t, city, id, "the snapshot's city.toml declares [storage]")
	})
	t.Run("in an include fragment", func(t *testing.T) {
		city := newFileStoreCity(t)
		fragment := filepath.Join(city.request.CityPath, "storage.toml")
		if err := os.WriteFile(fragment, []byte("[daemon]\npatrol_interval = \"1m\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		writeCityTOML(t, &city, "include = [\"storage.toml\"]\n\n"+fileStoreCityTOML)
		id := city.snapshot(t)
		if err := os.WriteFile(fragment, []byte(storageBlock), 0o644); err != nil {
			t.Fatal(err)
		}
		assertRefused(t, city, id, fragment)
	})
}

// TestVerifyRestoredCityRefusesASplitConfig: the proof asserts the
// configuration the restore left serves every class from the work store
// before it reads a store.
func TestVerifyRestoredCityRefusesASplitConfig(t *testing.T) {
	city := newFileStoreCity(t)
	city.split(t)
	if _, err := verifyRestoredCity(nil, city.request.CityPath, t.TempDir(), cityRestorePlan{}, "gc storage snapshot restore", io.Discard); err == nil || !strings.Contains(err.Error(), "routes infrastructure classes off the work store") {
		t.Fatalf("verifyRestoredCity on a split city = %v; want the split configuration refused", err)
	}
}

// dataDirExact is a data-directory exact copy holding databases, for a store
// whose own is database.
func dataDirExact(database string, databases ...string) snapshotExact {
	tree := &snapshotCopiedTree{Path: snapshotCityDoltDir}
	for _, db := range databases {
		tree.Dirs = append(tree.Dirs, db, filepath.Join(db, ".dolt"))
		tree.Files = append(tree.Files, snapshotFile{Path: filepath.Join(snapshotCityDoltDir, db, ".dolt", "manifest"), Bytes: 1 << 10})
	}
	return snapshotExact{Method: snapshotMethodDoltDataDir, Tree: tree, Database: database}
}

// withRig gives the city a rig named packs whose file store, holding one row,
// lives at a new directory, which it returns.
func (c *fileStoreCity) withRig(t *testing.T) string {
	t.Helper()
	rigPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(rigPath, ".gc", "beads.json"), beads.WithFileStoreIDPrefix("gp"))
	if err != nil {
		t.Fatal(err)
	}
	mustCreateInfraBead(t, store, beads.Bead{Title: "rig work", Type: "task"})
	text := fileStoreCityTOML + fmt.Sprintf("\n[[rigs]]\nname = \"packs\"\npath = %q\nprefix = \"gp\"\n", rigPath)
	if err := os.WriteFile(filepath.Join(c.request.CityPath, "city.toml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(c.request.CityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	c.request.Cfg = cfg
	return rigPath
}

// TestCityRestoreRefusesTheCitysStoresTheSnapshotDoesNotRecord: the copy of
// the managed Dolt's data directory holds every database the server serves,
// and the restore replaces the whole directory. A database a scope of the
// city keeps its store in that the snapshot records no store for (a rig a
// --work snapshot was taken without) would be rolled back unproven, so it is
// refused, naming the rig and its database. Every other database the copy
// holds (gc's probe database, a removed rig's) belongs to no store of the
// city: it does not refuse, and the plan names it to put back unproven. With
// the rig's store recorded the plan places both stores, and a database created
// since the snapshot is named as moved aside with the directory.
func TestCityRestoreRefusesTheCitysStoresTheSnapshotDoesNotRecord(t *testing.T) {
	city := newFileStoreCity(t)
	rigPath := city.withRig(t)
	id := city.snapshot(t)
	dir := filepath.Join(city.to, id)
	_, m := onlySnapshot(t, city.to)
	stubSnapshotScopeDoltDatabase(t, map[string]string{city.request.CityPath: "hq", rigPath: "gp"})
	live := t.TempDir()
	for _, db := range []string{"hq", "gp", "since"} {
		if err := os.MkdirAll(filepath.Join(live, db, ".dolt"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	prevDir := snapshotManagedDoltDataDir
	snapshotManagedDoltDataDir = func(string) (string, error) { return live, nil }
	t.Cleanup(func() { snapshotManagedDoltDataDir = prevDir })
	copied := []string{"hq", "gp", "__gc_probe", "oldrig"}

	m.Stores[0].Exact = dataDirExact("hq", copied...)
	_, err := planCityRestore(city.request, dir, m)
	if err == nil || !strings.Contains(err.Error(), "holds rig:packs (database gp), which this city keeps its store(s) in") || !strings.Contains(err.Error(), cityRestoreSnapshotCommand) {
		t.Fatalf("planCityRestore of a --work snapshot = %v; want the rig's unrecorded database refused", err)
	}
	for _, stray := range []string{"__gc_probe", "oldrig", "without --rigs"} {
		if strings.Contains(err.Error(), stray) {
			t.Errorf("the refusal names %q: %v; want only the rig's store refused, and no claim of how the snapshot was taken", stray, err)
		}
	}

	m.Stores = append(m.Stores, snapshotStoreRecord{Name: "rig:packs", Exact: dataDirExact("gp", copied...)})
	plan, err := planCityRestore(city.request, dir, m)
	if err != nil {
		t.Fatalf("planCityRestore with the rig's store recorded = %v", err)
	}
	if got := strings.Join(plan.names(), ","); got != "work,rig:packs" || plan.dolt == nil || strings.Join(plan.dolt.since, ",") != "since" || strings.Join(plan.dolt.unproven, ",") != "__gc_probe,oldrig" {
		t.Fatalf("the plan puts back %s with the data directory %+v; want work and rig:packs, the database created since and the two no store keeps named", got, plan.dolt)
	}

	m.Stores[1].Exact.Database = "elsewhere"
	if _, err := planCityRestore(city.request, dir, m); err == nil || !strings.Contains(err.Error(), "holds no database elsewhere") {
		t.Fatalf("planCityRestore of a store whose database the copy lacks = %v", err)
	}
}

// managedDoltCity puts each scope root scopes names in the city's managed
// Dolt, as the database it maps to, and stubs that Dolt: a data directory
// holding databases (each a .dolt directory with a file in it), and a
// provider the snapshot's maintenance window starts, so a --work snapshot
// copies that directory with the server stopped. It returns the data
// directory and the provider.
func managedDoltCity(t *testing.T, scopes map[string]string, databases ...string) (string, *migrationWindowProvider) {
	t.Helper()
	provider := &migrationWindowProvider{down: true}
	stubMigrationWindowProvider(t, provider)
	stubSnapshotScopeDoltDatabase(t, scopes)
	dataDir := filepath.Join(t.TempDir(), "dolt")
	for _, db := range databases {
		if err := os.MkdirAll(filepath.Join(dataDir, db, ".dolt"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dataDir, db, ".dolt", "manifest"), []byte(db), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	prevDir := snapshotManagedDoltDataDir
	snapshotManagedDoltDataDir = func(string) (string, error) { return dataDir, nil }
	t.Cleanup(func() { snapshotManagedDoltDataDir = prevDir })
	prevFree := doltContainerFreeBytesFunc
	doltContainerFreeBytesFunc = func(string) (int64, error) { return 1 << 50, nil }
	t.Cleanup(func() { doltContainerFreeBytesFunc = prevFree })
	return dataDir, provider
}

// TestCityRestorePutsBackDatabasesNoStoreOfTheCityKeeps: a managed Dolt
// routinely holds databases no store of the city keeps, gc's legacy probe
// database and the one `gc rig remove` leaves behind among them, and no
// snapshot can record a store for them. The snapshot names them in a note,
// and the city restore of it, the way back, puts them back as the copy holds
// them and names them unproven rather than refusing.
func TestCityRestorePutsBackDatabasesNoStoreOfTheCityKeeps(t *testing.T) {
	city := newFileStoreCity(t)
	dataDir, _ := managedDoltCity(t, map[string]string{city.request.CityPath: "hq"}, "hq", "__gc_probe", "oldrig")
	var stdout, stderr bytes.Buffer
	if code := doStorageSnapshot(context.Background(), city.request, snapshotOptions{Work: true, To: city.to, Keep: 5}, "gc storage snapshot", &stdout, &stderr); code != 0 {
		t.Fatalf("snapshot = %d: %s%s", code, stdout.String(), stderr.String())
	}
	id, m := onlySnapshot(t, city.to)
	if m.Stores[0].Exact.Method != snapshotMethodDoltDataDir || m.Stores[0].Exact.Database != "hq" {
		t.Fatalf("the work store's exact copy is %+v; want the data directory, its database hq", m.Stores[0].Exact)
	}
	note := "also holds database(s) __gc_probe, oldrig, which no store of this city keeps"
	if !strings.Contains(stdout.String(), note) || !strings.Contains(strings.Join(m.Stores[0].Notes, "\n"), note) {
		t.Errorf("the snapshot names the databases no store keeps neither on stdout nor in its manifest (%q):\n%s\nnotes %q", note, stdout.String(), m.Stores[0].Notes)
	}
	if err := os.RemoveAll(filepath.Join(dataDir, "oldrig")); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := city.restore(t, id)
	if code != 0 {
		t.Fatalf("restore = %d: %s%s; want the databases no store keeps put back, not refused", code, out, errOut)
	}
	if !strings.Contains(errOut, "holds database(s) __gc_probe, oldrig that no store of the snapshot records") || !strings.Contains(out, "The managed Dolt's database(s) __gc_probe, oldrig, which no store of this city keeps, are back as the snapshot's copy holds them, unproven.") {
		t.Errorf("the restore does not name the databases it puts back unproven:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	j, _, err := readStorageCityRestoreJournal(city.request.CityPath)
	if err != nil || !j.done(cityRestoreStepVerified) {
		t.Fatalf("journal %+v (%v); want the restore proven", j, err)
	}
	for _, db := range []string{"hq", "__gc_probe", "oldrig"} {
		if got, err := os.ReadFile(filepath.Join(dataDir, db, ".dolt", "manifest")); err != nil || string(got) != db {
			t.Errorf("database %s is not back as the copy holds it: %q (%v)", db, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir+j.Suffix, "hq", ".dolt", "manifest")); err != nil {
		t.Errorf("the data directory the restore replaced is not kept as %s: %v", dataDir+j.Suffix, err)
	}
}

// TestSnapshotRefusesToCopyARigItDoesNotRecord: a --work snapshot of a city
// whose rig keeps its store in the managed Dolt beside the work store would
// copy the rig's database and record no store for it, so the city restore
// would refuse it as the way back. The snapshot refuses before it starts or
// stops the server, naming the rig, its database and --rigs, and keeps
// nothing; with --rigs it records both stores in the one copy.
func TestSnapshotRefusesToCopyARigItDoesNotRecord(t *testing.T) {
	city := newFileStoreCity(t)
	rigPath := city.withRig(t)
	_, provider := managedDoltCity(t, map[string]string{city.request.CityPath: "hq", rigPath: "gp"}, "hq", "gp")
	var stdout, stderr bytes.Buffer
	code := doStorageSnapshot(context.Background(), city.request, snapshotOptions{Work: true, To: city.to, Keep: 5}, "gc storage snapshot", &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "holds rig:packs (database gp), which this city keeps its store(s) in") || !strings.Contains(stderr.String(), "take it with --rigs") {
		t.Fatalf("a --work snapshot of a city whose rig is in the managed Dolt = %d: %s%s; want it refused, naming the rig, its database and --rigs", code, stdout.String(), stderr.String())
	}
	if provider.starts != 0 || provider.stops != 0 {
		t.Errorf("the provider was started %d and stopped %d time(s); want neither, for a snapshot refused before its window", provider.starts, provider.stops)
	}
	if entries, _ := os.ReadDir(city.to); slices.ContainsFunc(entries, func(e os.DirEntry) bool { return storageSnapshotIDPattern.MatchString(e.Name()) }) {
		t.Errorf("the refused snapshot left %v in %s", entries, city.to)
	}

	stdout.Reset()
	stderr.Reset()
	if code := doStorageSnapshot(context.Background(), city.request, snapshotOptions{Work: true, Rigs: true, To: city.to, Keep: 5}, "gc storage snapshot", &stdout, &stderr); code != 0 {
		t.Fatalf("snapshot with --rigs = %d: %s%s", code, stdout.String(), stderr.String())
	}
	_, m := onlySnapshot(t, city.to)
	databases := map[string]string{}
	for _, rec := range m.Stores {
		if rec.Exact.Method == snapshotMethodDoltDataDir {
			databases[rec.Name] = rec.Exact.Database
		}
	}
	if databases["work"] != "hq" || databases["rig:packs"] != "gp" {
		t.Fatalf("the stores in the copy are %v; want work in hq and rig:packs in gp", databases)
	}
}

// TestCityRestoreRefusesAStoreAnotherCityShares: the restore replaces each
// store it puts back whole with the snapshot's copy, so a rig store another
// registered city also uses would have that city's rows rolled back with this
// city's. It refuses before it moves anything, naming the rig and the other
// city. No --force=shared-store attestation lifts that, as it does for the
// writing verbs that select the rows they carry: the restore refuses the
// attestation outright.
func TestCityRestoreRefusesAStoreAnotherCityShares(t *testing.T) {
	city := newFileStoreCity(t)
	rigPath := city.withRig(t)
	var stdout, stderr bytes.Buffer
	if code := doStorageSnapshot(context.Background(), city.request, snapshotOptions{Work: true, Rigs: true, To: city.to, Keep: 5}, "gc storage snapshot", &stdout, &stderr); code != 0 {
		t.Fatalf("snapshot = %d: %s%s", code, stdout.String(), stderr.String())
	}
	id, m := onlySnapshot(t, city.to)
	if len(m.Stores) != 2 || m.Stores[1].Name != "rig:packs" || m.Stores[1].Exact.Method != snapshotMethodFileCopy {
		t.Fatalf("the snapshot holds %+v; want the work store and rig:packs's file", m.Stores)
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "city.toml"), []byte(fmt.Sprintf("[workspace]\nname = \"other-city\"\n\n[[rigs]]\nname = \"theirs\"\npath = %q\nprefix = \"gp\"\n", rigPath)), 0o644); err != nil {
		t.Fatal(err)
	}
	stubOtherCities(t, other)
	rigStore := filepath.Join(rigPath, ".gc", "beads.json")
	before, err := os.ReadFile(rigStore)
	if err != nil {
		t.Fatal(err)
	}

	code, out, errOut := city.restore(t, id)
	if code == 0 || !strings.Contains(errOut, "rig packs's store ") || !strings.Contains(errOut, " is also city "+other+"'s rig theirs") || !strings.Contains(errOut, "replaces each of these stores whole") {
		t.Fatalf("restore over a shared rig store = %d: %s%s; want it refused, naming the rig and the other city", code, out, errOut)
	}
	if _, present, _ := readStorageCityRestoreJournal(city.request.CityPath); present {
		t.Error("a refused restore wrote its journal")
	}
	if after, err := os.ReadFile(rigStore); err != nil || !bytes.Equal(after, before) {
		t.Errorf("a refused restore changed the shared rig store (%v)", err)
	}

	city.request.Force = storageForce{SharedStores: []string{"anything"}}
	if code, _, errOut := city.restore(t, id); code == 0 || !strings.Contains(errOut, "does not apply to --city") {
		t.Fatalf("restore with a shared-store attestation = %d: %s; want the attestation refused", code, errOut)
	}
}

// TestCityRestoreRefusesADatabaseAnotherCityKeepsInItsManagedDolt: the
// restore replaces the managed Dolt's whole data directory, so a database no
// store of this city keeps may still be another registered city's store on
// this city's server. The restore refuses it before it moves anything, naming
// the database and the other city, rather than rolling that store back.
func TestCityRestoreRefusesADatabaseAnotherCityKeepsInItsManagedDolt(t *testing.T) {
	city := newFileStoreCity(t)
	dataDir, _ := managedDoltCity(t, map[string]string{city.request.CityPath: "hq"}, "hq", "theirs")
	id := city.snapshot(t)
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "city.toml"), []byte("[workspace]\nname = \"other-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubOtherCities(t, other)
	prev := storeDoltIdentityOf
	t.Cleanup(func() { storeDoltIdentityOf = prev })
	storeDoltIdentityOf = func(cityPath, scopeRoot string) storeDoltIdentity {
		if samePath(scopeRoot, other) {
			return managedStoreIdentity(city.request.CityPath, "theirs")
		}
		return prev(cityPath, scopeRoot)
	}

	code, out, errOut := city.restore(t, id)
	if code == 0 || !strings.Contains(errOut, "this city's managed Dolt's Dolt database theirs") || !strings.Contains(errOut, "is also city "+other+"'s city store") {
		t.Fatalf("restore over a database another city keeps = %d: %s%s; want it refused, naming the database and the other city", code, out, errOut)
	}
	if _, present, _ := readStorageCityRestoreJournal(city.request.CityPath); present {
		t.Error("a refused restore wrote its journal")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "theirs", ".dolt", "manifest")); err != nil {
		t.Errorf("a refused restore moved the other city's database: %v", err)
	}
}

// TestManagedDoltServedDataDirIgnoresAnInheritedDataDir: the data directory
// the snapshot copies and the restore replaces is the one the city's managed
// Dolt serves, <city>/.beads/dolt. A GC_DOLT_DATA_DIR naming another
// directory (gc exports one into order environments, another city's among
// them) is refused by both before anything is stopped or moved.
func TestManagedDoltServedDataDirIgnoresAnInheritedDataDir(t *testing.T) {
	cityPath, lock := windowCity(t)
	served := filepath.Join(normalizePathForCompare(cityPath), ".beads", "dolt")
	t.Setenv("GC_DOLT_DATA_DIR", "")
	if got, err := managedDoltServedDataDir(cityPath); err != nil || got != served {
		t.Fatalf("managedDoltServedDataDir = %q, %v; want %q", got, err, served)
	}
	t.Setenv("GC_DOLT_DATA_DIR", served)
	if got, err := managedDoltServedDataDir(cityPath); err != nil || got != served {
		t.Fatalf("with GC_DOLT_DATA_DIR naming the served directory = %q, %v; want %q", got, err, served)
	}
	other := t.TempDir()
	t.Setenv("GC_DOLT_DATA_DIR", other)
	if _, err := managedDoltServedDataDir(cityPath); err == nil || !strings.Contains(err.Error(), other) || !strings.Contains(err.Error(), served) {
		t.Fatalf("with GC_DOLT_DATA_DIR naming another directory = %v; want it refused, naming both", err)
	}

	provider := &migrationWindowProvider{down: true}
	stubMigrationWindowProvider(t, provider)
	window, err := openTestWindow(t, lock, cityPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = window.Close() })
	stops := provider.stops
	if _, _, err := snapshotDoltDataDir(cityPath, t.TempDir()); err == nil || !strings.Contains(err.Error(), "GC_DOLT_DATA_DIR") {
		t.Fatalf("snapshotDoltDataDir = %v; want the inherited directory refused", err)
	}
	if provider.stops != stops {
		t.Error("the server was stopped for a copy that was refused")
	}
	var plan cityRestorePlan
	if _, err := plan.placeDolt(cityPath, snapshotStoreRecord{Name: "work", Exact: dataDirExact("hq", "hq")}); err == nil || !strings.Contains(err.Error(), "GC_DOLT_DATA_DIR") {
		t.Fatalf("the restore's plan = %v; want the inherited directory refused", err)
	}
}

// TestSnapshotOfTheManagedDoltDataDirRestartsTheRigScopes: the provider stop
// the copy takes retires every provider-owned scope, and ensureBeadsProvider
// starts the city's alone, so the window starts again each rig scope bd owns
// that the run reads: a --rigs snapshot reads them after the copy.
func TestSnapshotOfTheManagedDoltDataDirRestartsTheRigScopes(t *testing.T) {
	cityPath, lock := windowCity(t)
	provider := &migrationWindowProvider{down: true}
	stubMigrationWindowProvider(t, provider)
	rig := t.TempDir()
	writeScopeBeadsMetadata(t, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"gp"}`)
	window, err := openMigrationSource(lock, cityPath, &config.City{}, "gc storage snapshot", &bytes.Buffer{}, rig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = window.Close() })
	if len(provider.scopeStarts) != 1 {
		t.Fatalf("the window started rig scopes %v; want the proxied rig once", provider.scopeStarts)
	}
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "hq", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "hq", ".dolt", "manifest"), []byte("m"), 0o644); err != nil {
		t.Fatal(err)
	}
	prevDir := snapshotManagedDoltDataDir
	snapshotManagedDoltDataDir = func(string) (string, error) { return dataDir, nil }
	t.Cleanup(func() { snapshotManagedDoltDataDir = prevDir })
	if exact, why, err := snapshotDoltDataDir(cityPath, t.TempDir()); err != nil || exact == nil {
		t.Fatalf("snapshotDoltDataDir = %+v, %q, %v", exact, why, err)
	}
	if provider.stops != 1 || provider.starts != 2 {
		t.Errorf("stops %d, starts %d; want the provider stopped once for the copy and started again", provider.stops, provider.starts)
	}
	if len(provider.scopeStarts) != 2 || !samePath(provider.scopeStarts[1], rig) {
		t.Errorf("rig scopes started %v; want the proxied rig started again after the copy", provider.scopeStarts)
	}
}

// TestCityRestoreRunsTheWorkStoreWritersChecks: the restore replaces the
// whole work store, so it runs what every writer of it runs under the
// controller lock: a live controller refuses, and so does a work store
// another registered city shares. That one refuses attested or not: the
// restore cannot leave the other city's rows as they are, so it offers no
// attestation and refuses --force=shared-store:<identity>.
func TestCityRestoreRunsTheWorkStoreWritersChecks(t *testing.T) {
	t.Run("a live controller", func(t *testing.T) {
		city := newFileStoreCity(t)
		id := city.snapshot(t)
		city.split(t)
		stubInfraControllerPing(t, 4242)
		if code, _, stderr := city.restore(t, id); code == 0 || !strings.Contains(stderr, "controller PID 4242") {
			t.Fatalf("restore beside a live controller = %d: %s", code, stderr)
		}
		if _, present, _ := readStorageCityRestoreJournal(city.request.CityPath); present {
			t.Error("a refused restore wrote its journal")
		}
	})
	t.Run("a shared work store", func(t *testing.T) {
		city := newFileStoreCity(t)
		// The co-tenant first: it pins this city's project id, which is part
		// of the work store's identity the snapshot records.
		other := sharedStoreCities(t, city.request)
		id := city.snapshot(t)
		city.split(t)
		code, _, stderr := city.restore(t, id)
		if code == 0 || !strings.Contains(stderr, other) || !strings.Contains(stderr, "replaces each of these stores whole") || strings.Contains(stderr, "--force=shared-store:"+sharedToken) {
			t.Fatalf("restore over a shared work store = %d: %s; want it refused, naming the co-tenant and offering no attestation", code, stderr)
		}
		city.request.Force = storageForce{SharedStores: []string{sharedToken}}
		if code, _, stderr := city.restore(t, id); code == 0 || !strings.Contains(stderr, "does not apply to --city") {
			t.Fatalf("attested restore = %d: %s; want the attestation refused", code, stderr)
		}
		if _, present, _ := readStorageCityRestoreJournal(city.request.CityPath); present {
			t.Error("a refused restore wrote its journal")
		}
		if log, err := os.ReadFile(filepath.Join(city.request.CityPath, ".gc", storageAttestationLog)); err == nil && strings.Contains(string(log), storageHazardSharedStore) {
			t.Errorf("a refused restore recorded a shared-store attestation: %q", log)
		}
	})
}

// TestReverseMandatorySnapshotTakesNoDataDirCopy: the reverse's mandatory
// snapshot (R0) holds the binding, which the city restore never restores,
// so it records no city and copies no managed Dolt data directory: --to-work
// neither stops the managed Dolt a second time mid-reverse nor needs a copy's
// worth of disk.
func TestReverseMandatorySnapshotTakesNoDataDirCopy(t *testing.T) {
	city := newReverseEngineCity(t, 30, "")
	closeBindingGates(t, city)
	provider := &migrationWindowProvider{down: true}
	stubMigrationWindowProvider(t, provider)
	stubSnapshotScopeDoltDatabase(t, map[string]string{city.cityPath: "hq"})
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "hq", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "hq", ".dolt", "manifest"), []byte("m"), 0o644); err != nil {
		t.Fatal(err)
	}
	prevDir := snapshotManagedDoltDataDir
	snapshotManagedDoltDataDir = func(string) (string, error) { return dataDir, nil }
	t.Cleanup(func() { snapshotManagedDoltDataDir = prevDir })

	if code, stdout, stderr := city.reverse(t, reverseOptions{}); code != 0 {
		t.Fatalf("--to-work = %d: %s%s", code, stdout, stderr)
	}
	if provider.stops != 1 || provider.starts != 1 {
		t.Errorf("the provider was started %d and stopped %d time(s); want once each, by the reverse's window alone", provider.starts, provider.stops)
	}
	journal, _, _ := readStorageReverseJournal(city.cityPath)
	m, err := readSnapshotManifest(filepath.Join(journal.SnapshotTo, journal.Snapshot))
	if err != nil {
		t.Fatal(err)
	}
	if m.CityRecord != nil {
		t.Error("the mandatory snapshot records the city, which no restore of it reads")
	}
	for _, rec := range m.Stores {
		if rec.Exact.Method == snapshotMethodDoltDataDir {
			t.Errorf("the mandatory snapshot's %s took the managed Dolt's data directory", rec.Name)
		}
	}
}

// TestCityRestoreRefusesACopyTheDiskCannotHold: the restore installs the
// snapshot's copy of the managed Dolt's data directory beside the one it
// moves aside, on a filesystem a managed Dolt writes to, so a copy that
// would leave less than the critical floor free there is refused before the
// journal is written.
func TestCityRestoreRefusesACopyTheDiskCannotHold(t *testing.T) {
	const floor = 8 << 20
	t.Setenv("GC_DOLT_MIN_FREE_BYTES", strconv.Itoa(floor))
	t.Setenv("GC_DOLT_WARN_FREE_BYTES", strconv.Itoa(floor))
	free := int64(floor)
	prevFree := doltContainerFreeBytesFunc
	doltContainerFreeBytesFunc = func(string) (int64, error) { return free, nil }
	t.Cleanup(func() { doltContainerFreeBytesFunc = prevFree })
	dataDir := filepath.Join(t.TempDir(), "dolt")
	plan := cityRestorePlan{dolt: &cityRestoreDolt{tree: *dataDirExact("hq", "hq").Tree, dataDir: dataDir}}
	if err := cityRestoreDiskRefusal(plan); err == nil || !strings.Contains(err.Error(), "must leave") || !strings.Contains(err.Error(), filepath.Dir(dataDir)) {
		t.Fatalf("cityRestoreDiskRefusal = %v; want the copy refused for the floor", err)
	}
	free = 1 << 40
	if err := cityRestoreDiskRefusal(plan); err != nil {
		t.Fatalf("cityRestoreDiskRefusal with room = %v", err)
	}
	if err := cityRestoreDiskRefusal(cityRestorePlan{}); err != nil {
		t.Fatalf("a restore with no data directory to copy = %v", err)
	}
}
