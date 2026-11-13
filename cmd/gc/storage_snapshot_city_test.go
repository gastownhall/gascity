package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
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
