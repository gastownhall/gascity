package main

// The city half of a storage snapshot, and `gc storage snapshot restore <id>
// --city --fleet-stopped` (S-3 acceptance 3; owner decisions D-b and D-c).
//
// A city is more than its stores. What it serves from is decided by
// city.toml's [storage], what a gc may run on it by .gc/min-gc-version, and
// what a split has done by the storage records under .gc (the served-binding
// note, the cleared notes). `gc storage snapshot --work` also records those
// (snapshotCityRecord): city.toml's bytes, and each record's bytes or its
// absence.
//
// A managed Dolt's exact copy is its data directory, copied with the server
// stopped (D-c; the five live rollbacks before this verb did exactly that).
// A Dolt tag covers the committed tables only, and bd keeps its wisps in
// dolt_ignore'd ones, so a tag is no way back to a store. The snapshot runs in
// the maintenance window, whose server is one it started or adopted, so it
// stops that server for the copy and starts it again, with the rig scopes the
// window reads, for the JSONL read (snapshotDoltDataDir). The directory is the
// one the server serves, never one an inherited GC_DOLT_DATA_DIR names. A rig
// whose database lives in the city's data directory (the managed layout) is in
// the same copy, and the manifest names each store's database in it. A server
// the window did not start keeps the Dolt tag, and the manifest says why.
//
// The copy holds every database the server serves, and the census
// (doltCopyCensus) sorts those no store of the snapshot records: one a scope
// of the city keeps its store in (a rig the snapshot was taken without) would
// come back unproven, so the snapshot refuses it, before it starts or stops
// anything when --rigs is missing; any other (gc's probe database, a removed
// rig's, a test's) belongs to no store of the city, and the snapshot names it
// in a note on the work store.
//
// The city restore puts a city back as the snapshot found it: every store the
// snapshot holds, from its exact copy; then the storage records; then
// city.toml; then, when the snapshot holds no binding (one taken before the
// split), the live binding root is moved aside, so the city serves from its
// work store alone again. It runs under the controller lock, the migration
// guard and the snapshot directory's lock, with the city stopped and attested
// so, and refuses before it moves anything:
//
//   - a snapshot that does not verify, that records no city, or that was
//     taken of another work store;
//   - a store whose exact copy restores nothing exactly (a Dolt tag, a bd
//     export, no copy at all): the JSONL is a record, not a restore source;
//   - a snapshot that holds a binding: the binding's records beside its
//     database are recorded by hash, not kept, so a split city is restored
//     with `--binding` (its database) and this restore is for a snapshot
//     from before the split;
//   - a snapshot of a split city, or one whose city.toml, composed with the
//     city's include fragments as they are now, still routes a class off the
//     work store: the city it would leave does not boot;
//   - a data-directory copy holding a database a scope of the city keeps its
//     store in that the snapshot records no store for: the restore replaces
//     the whole directory, so that store would come back unproven. Any other
//     database the copy holds is put back as the copy holds it, and the
//     restore names it unproven;
//   - a rig imported since the snapshot whose store the snapshot does not
//     hold;
//   - a store it replaces that another registered city also uses: the work
//     store, a rig's, or any other database of the managed Dolt's data
//     directory, live or copied. The restore replaces a store whole, so it
//     cannot leave another city's rows as they are, and, unlike the writing
//     verbs that select rows, it takes no --force=shared-store:<identity>;
//   - a live controller; a managed Dolt that is running; too little disk for
//     the data directory's copy; or a reverse migration in progress.
//
// Every file it replaces is moved aside as <path>.pre-restore-<ts>, never
// deleted. The journal (.gc/storage-city-restore.json) records the run before
// the first move and each step once it is done; run again, the same command
// finishes the steps that are not. Until the restore ends the journal holds
// gc start and every other writing storage command, and the snapshot is
// pinned, so retention keeps it.
//
// Once every file is back the restore reads each restored store back (in a
// maintenance window opened with the restored city.toml, which starts a
// stopped provider and stops it after) and proves its rows equal to the
// snapshot's JSONL, row for row. The JSONL is read from the live store after
// the copy, so a row may have changed in between: a row the JSONL dates inside
// that window (snapshotExact.CopiedAt to snapshotStoreRecord.JSONLReadAt, each
// to the second) is reported as changed or created during the snapshot, with
// where its later content survives, and the restore ends. Any other difference
// fails the proof, and the city stays held. The same command ends the restore
// once the difference is resolved, and, when it cannot be, --accept-unproven
// ends it anyway: it records what the proof failed on in the journal and, row
// by row, in the attestation log, keeps every file the restore replaced, and
// releases the city.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doltpool"
	"github.com/gastownhall/gascity/internal/fsys"
)

// snapshotMethodDoltDataDir is a managed Dolt's data directory, copied with
// the server stopped.
const snapshotMethodDoltDataDir = "dolt-data-dir"

// snapshotCityDoltDir is where a snapshot keeps the managed Dolt's data
// directory.
const snapshotCityDoltDir = "dolt-data"

// storageCityRestoreJournalName is the city restore's journal under .gc.
const storageCityRestoreJournalName = "storage-city-restore.json"

// storageCityRestoreFormat versions the journal.
const storageCityRestoreFormat = "gc.storage.city-restore/v1"

// cityRestoreAcceptFlag ends an unfinished city restore whose proof fails.
const cityRestoreAcceptFlag = "accept-unproven"

// cityRestoreUnprovenHazard is the attestation an accepted unproven restore
// records in the attestation log, with the snapshot's id after a colon.
const cityRestoreUnprovenHazard = "city-restore-unproven"

// cityRestoreSnapshotCommand takes the snapshot a city restore puts back,
// before [storage] is added and before `gc storage migrate --from-work`, as
// `gc storage preflight --split` names it.
const cityRestoreSnapshotCommand = "gc storage snapshot --work --rigs --" + storageFleetStoppedFlag

// cityRestoreListed caps the rows a failed proof's message names; the
// journal of an accepted one records them all.
const cityRestoreListed = 5

// snapshotCityRecord is the city in a snapshot: city.toml and the storage
// records under .gc, each by its bytes or its absence.
type snapshotCityRecord struct {
	// CityTOML is the copy of city.toml, nil when the city had none (a
	// directory gc does not load as a city; the restore refuses it).
	CityTOML *snapshotFile `json:"city_toml,omitempty"`
	// Files are the storage records, each relative to the city: the copy when
	// it was there, nil when it was not.
	Files []snapshotCityFile `json:"files"`
}

// snapshotCityFile is one storage record of the city.
type snapshotCityFile struct {
	Rel  string        `json:"rel"`
	File *snapshotFile `json:"file,omitempty"`
}

// snapshotCopiedTree is a directory in a snapshot: every regular file under
// it, and every directory, relative to it, so one that holds no file comes
// back too (a Dolt git remote's cache keeps its refs in one, and git does not
// read a repository whose refs directory is gone).
type snapshotCopiedTree struct {
	Path  string         `json:"path"`
	Files []snapshotFile `json:"files"`
	Dirs  []string       `json:"dirs,omitempty"`
}

// bytes is the size of every file the tree holds.
func (t snapshotCopiedTree) bytes() int64 {
	var n int64
	for _, f := range t.Files {
		n += f.Bytes
	}
	return n
}

// databases are the Dolt databases the tree holds, sorted: each top-level
// directory with a .dolt directory in it.
func (t snapshotCopiedTree) databases() []string {
	var out []string
	for _, d := range t.Dirs {
		parent := filepath.Dir(filepath.Clean(d))
		if filepath.Base(d) == ".dolt" && parent != "." && filepath.Dir(parent) == "." {
			out = append(out, parent)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// doltDataDirDatabases are the Dolt databases a data directory holds, sorted:
// each directory in it with a .dolt directory inside. A data directory that is
// not there holds none.
func doltDataDirDatabases(dataDir string) ([]string, error) {
	entries, err := os.ReadDir(dataDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := os.Stat(filepath.Join(dataDir, e.Name(), ".dolt"))
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return nil, err
		case info.IsDir():
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// cityScopeDoltDatabases maps each database a scope of the city keeps its
// store in, in the city's managed Dolt, to the scope: work, and rig:<name> for
// each rig with a store of its own (snapshotRigs).
func cityScopeDoltDatabases(request storageOperatorRequest) map[string]string {
	named := map[string]string{}
	for _, s := range append([]snapshotScopeRef{{name: "work", root: request.CityPath}}, snapshotRigs(request)...) {
		if db := snapshotScopeDoltDatabase(request.CityPath, s.root); db != "" {
			named[db] = s.name
		}
	}
	return named
}

// doltCopyCensus sorts the databases a copy of the managed Dolt's data
// directory holds (copied, sorted) that no store of the snapshot records
// (recorded). owned are those a scope of the city keeps its store in (named,
// database to scope), each as "<scope> (database <db>)": a restore of the copy
// would roll that store back unproven, so the snapshot and the restore refuse
// them. unproven are the rest (gc's probe database, a removed rig's, a
// test's), which no store of the city keeps: the restore puts them back as
// the copy holds them.
func doltCopyCensus(copied []string, recorded map[string]bool, named map[string]string) (owned, unproven []string) {
	for _, db := range copied {
		scope, isNamed := named[db]
		switch {
		case recorded[db]:
		case isNamed:
			owned = append(owned, fmt.Sprintf("%s (database %s)", scope, db))
		default:
			unproven = append(unproven, db)
		}
	}
	return owned, unproven
}

// snapshotDoltOwnedRefusal refuses a snapshot whose copy of the managed Dolt's
// data directory holds stores of the city it records no store for (owned,
// doltCopyCensus): `gc storage snapshot restore --city` would refuse it, and
// once the split has run no snapshot it accepts can be taken.
func snapshotDoltOwnedRefusal(owned []string) error {
	if len(owned) == 0 {
		return nil
	}
	return fmt.Errorf("the copy of the managed Dolt's data directory holds %s, which this city keeps its store(s) in, and this snapshot would record no store for them, so `gc storage snapshot restore --city` would refuse it as the way back; take it with --rigs (`%s`)", strings.Join(owned, ", "), cityRestoreSnapshotCommand)
}

// snapshotDoltCensus runs the census (doltCopyCensus) on a snapshot's copy of
// the managed Dolt's data directory, tree, once manifest records every store.
// A store of the city it holds and records no store for refuses the snapshot
// (snapshotDoltOwnedRefusal). Every other database it records no store for is
// named in a note on the work store, which the snapshot prints: the city
// restore puts it back as the copy holds it, unproven.
func snapshotDoltCensus(request storageOperatorRequest, tree snapshotCopiedTree, manifest *snapshotManifest) error {
	recorded := map[string]bool{}
	work := -1
	for i, rec := range manifest.Stores {
		if rec.Exact.Method == snapshotMethodDoltDataDir && rec.Exact.Database != "" {
			recorded[rec.Exact.Database] = true
		}
		if rec.Name == "work" {
			work = i
		}
	}
	owned, unproven := doltCopyCensus(tree.databases(), recorded, cityScopeDoltDatabases(request))
	if err := snapshotDoltOwnedRefusal(owned); err != nil {
		return err
	}
	if len(unproven) > 0 && work >= 0 {
		manifest.Stores[work].Notes = append(manifest.Stores[work].Notes, fmt.Sprintf("the copy of the managed Dolt's data directory also holds database(s) %s, which no store of this city keeps (gc's probe database, a removed rig's, a test's); `gc storage snapshot restore --city` puts them back as the copy holds them, unproven", strings.Join(unproven, ", ")))
	}
	return nil
}

// snapshotCityRigsRefusal refuses, before the snapshot locks, starts or stops
// anything, a snapshot that is a way back to the city (opts.City) taken
// without --rigs of a city whose rigs keep their stores in its managed Dolt
// beside the work store: the copy of the data directory would hold their
// databases, which the snapshot records no store for, so the census would
// refuse it once copied.
func snapshotCityRigsRefusal(request storageOperatorRequest, opts snapshotOptions) error {
	if !opts.City || opts.Rigs {
		return nil
	}
	workDatabase := snapshotScopeDoltDatabase(request.CityPath, request.CityPath)
	if workDatabase == "" {
		return nil
	}
	named := cityScopeDoltDatabases(request)
	owned, _ := doltCopyCensus(slices.Sorted(maps.Keys(named)), map[string]bool{workDatabase: true}, named)
	return snapshotDoltOwnedRefusal(owned)
}

// snapshotCityRecordRels are the city's storage records a snapshot keeps
// (relative to the city), the fixed ones and those the rigs' cleared notes
// add.
func snapshotCityRecordRels(cityPath string) ([]string, error) {
	rels := []string{
		filepath.Join(".gc", minGCVersionFileName),
		rel(cityPath, infraClearedNotePath(cityPath)),
		rel(cityPath, bornSplitServedNotePath(cityPath)),
	}
	notes, err := filepath.Glob(rigGraphClearedNotePath(cityPath, "*"))
	if err != nil {
		return nil, err
	}
	for _, p := range notes {
		rels = append(rels, rel(cityPath, p))
	}
	sort.Strings(rels)
	return slices.Compact(rels), nil
}

// rel is path relative to base; both are under one city.
func rel(base, path string) string {
	r, err := filepath.Rel(base, path)
	if err != nil {
		return path
	}
	return r
}

// cityTOMLFile is city.toml as a file, through a symlink.
func cityTOMLFile(cityPath string) (string, error) {
	p := filepath.Join(cityPath, "city.toml")
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", p, err)
	}
	return resolved, nil
}

// snapshotCityFiles records the city in a snapshot at dir.
func snapshotCityFiles(cityPath, dir string) (*snapshotCityRecord, error) {
	rec := &snapshotCityRecord{}
	switch _, err := os.Lstat(filepath.Join(cityPath, "city.toml")); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		toml, err := cityTOMLFile(cityPath)
		if err != nil {
			return nil, err
		}
		file, err := copySnapshotFile(dir, toml, filepath.Join("city", "city.toml"))
		if err != nil {
			return nil, fmt.Errorf("copying city.toml: %w", err)
		}
		rec.CityTOML = &file
	}
	rels, err := snapshotCityRecordRels(cityPath)
	if err != nil {
		return nil, err
	}
	for _, r := range rels {
		src := filepath.Join(cityPath, r)
		switch _, err := os.Stat(src); {
		case errors.Is(err, fs.ErrNotExist):
			rec.Files = append(rec.Files, snapshotCityFile{Rel: r})
			continue
		case err != nil:
			return nil, fmt.Errorf("reading %s: %w", src, err)
		}
		copied, err := copySnapshotFile(dir, src, filepath.Join("city", r))
		if err != nil {
			return nil, err
		}
		rec.Files = append(rec.Files, snapshotCityFile{Rel: r, File: &copied})
	}
	return rec, nil
}

// verifySnapshotCity proves the city record's files against the manifest.
func verifySnapshotCity(dir string, rec *snapshotCityRecord) error {
	if rec == nil {
		return nil
	}
	if rec.CityTOML != nil {
		if err := verifySnapshotFile(dir, *rec.CityTOML); err != nil {
			return fmt.Errorf("city: %w", err)
		}
	}
	for _, f := range rec.Files {
		if f.File == nil {
			continue
		}
		if err := verifySnapshotFile(dir, *f.File); err != nil {
			return fmt.Errorf("city: %w", err)
		}
	}
	return nil
}

// verifySnapshotTree proves every file a tree records, and that every
// directory it records is one inside it.
func verifySnapshotTree(dir string, tree *snapshotCopiedTree) error {
	if tree == nil || len(tree.Files) == 0 {
		return errors.New("records no file")
	}
	for _, d := range tree.Dirs {
		if !snapshotTreeRelInside(d) {
			return fmt.Errorf("the directory %q is outside the tree %s", d, tree.Path)
		}
		if info, err := os.Stat(filepath.Join(dir, tree.Path, d)); err != nil || !info.IsDir() {
			return fmt.Errorf("the directory %s is missing from the tree %s", d, tree.Path)
		}
	}
	for _, f := range tree.Files {
		if !strings.HasPrefix(filepath.Clean(f.Path)+string(filepath.Separator), filepath.Clean(tree.Path)+string(filepath.Separator)) {
			return fmt.Errorf("%s is outside the tree %s", f.Path, tree.Path)
		}
		if err := verifySnapshotFile(dir, f); err != nil {
			return err
		}
	}
	return nil
}

// Test seams for the managed Dolt the snapshot copies and the restore puts
// back: the database a scope's store keeps in the city's managed Dolt,
// whether the server runs, and its data directory.
var (
	snapshotScopeDoltDatabase  = scopeManagedDoltDatabase
	snapshotManagedDoltRunning = func(cityPath string) bool { return currentManagedDoltPort(cityPath) != "" }
	snapshotManagedDoltDataDir = managedDoltServedDataDir
)

// snapshotScopeInManagedDolt reports whether a scope's store lives in the
// city's managed Dolt.
func snapshotScopeInManagedDolt(cityPath, scopeRoot string) bool {
	return snapshotScopeDoltDatabase(cityPath, scopeRoot) != ""
}

// scopeManagedDoltDatabase is the database a scope's store keeps in the
// city's managed Dolt (scopeStoreLivesInTheCitysManagedDolt), or "" when the
// store lives elsewhere.
func scopeManagedDoltDatabase(cityPath, scopeRoot string) string {
	if !scopeStoreLivesInTheCitysManagedDolt(cityPath, scopeRoot) {
		return ""
	}
	database, _, err := contract.ReadDoltDatabase(fsys.OSFS{}, scopeMetadataJSONPath(scopeRoot))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(database)
}

// managedDoltServedDataDir is the data directory the city's managed Dolt
// serves: <city>/.beads/dolt, which the provider pins for the server
// (providerLifecycleDoltPathEnv) and resolveManagedDoltRuntimeLayoutStrict
// resolves from the city alone. The snapshot copies this directory and the
// restore replaces it, so a GC_DOLT_DATA_DIR this process inherited (gc
// exports one into order environments, another city's among them) must not
// turn either onto another directory: one naming another directory refuses.
func managedDoltServedDataDir(cityPath string) (string, error) {
	layout, err := resolveManagedDoltRuntimeLayoutStrict(cityPath)
	if err != nil {
		return "", err
	}
	if inherited := strings.TrimSpace(os.Getenv("GC_DOLT_DATA_DIR")); inherited != "" && !samePath(inherited, layout.DataDir) {
		return "", fmt.Errorf("GC_DOLT_DATA_DIR in this environment names %s, and the city's managed Dolt serves %s; unset it and run this again", inherited, layout.DataDir)
	}
	return layout.DataDir, nil
}

// cityRestoreUnrestorable says why the city restore would refuse the snapshot
// `gc storage preflight --split` names (cityRestoreSnapshotCommand) taken of
// the city as it is now, or "": a store of the city, the work store or a rig
// with a store of its own, that the snapshot takes no copy of the restore puts
// back. A file store's copy is its file; a store in the city's managed Dolt is
// in the copy of the data directory, which the snapshot takes when the work
// store is in it. A database no store of the city keeps refuses nothing: the
// restore puts it back as the copy holds it.
func cityRestoreUnrestorable(request storageOperatorRequest) string {
	cityPath := request.CityPath
	workInDolt := snapshotScopeInManagedDolt(cityPath, cityPath)
	var refused []string
	for _, s := range append([]snapshotScopeRef{{name: "work", root: cityPath}}, snapshotRigs(request)...) {
		fileCopy := snapshotExactMethod(rawBeadsProviderForScope(s.root, cityPath), false) == snapshotMethodFileCopy
		if !fileCopy && (!workInDolt || !snapshotScopeInManagedDolt(cityPath, s.root)) {
			refused = append(refused, s.name)
		}
	}
	if len(refused) == 0 {
		return ""
	}
	return fmt.Sprintf("the split's snapshot of %s is a Dolt tag, a bd export or no copy, which the city restore does not put back", strings.Join(refused, ", "))
}

// snapshotDoltDataDir copies the city's managed Dolt data directory into
// dir with the server stopped: the window open on the city stops the server
// it started or adopted, the copy is taken, and the window starts it again.
// It returns the exact copy, or nil and why none was taken.
func snapshotDoltDataDir(cityPath, dir string) (*snapshotExact, string, error) {
	w := activeMigrationWindow(cityPath)
	if w == nil || !w.started {
		return nil, "the managed Dolt was running before this run and the run did not start it, so its data directory was not copied (a Dolt tag instead, which no restore reads); stop the city's Dolt and snapshot again for a copy `gc storage snapshot restore --city` restores", nil
	}
	dataDir, err := snapshotManagedDoltDataDir(cityPath)
	if err != nil {
		return nil, "", fmt.Errorf("resolving the managed Dolt's data directory: %w", err)
	}
	// Sized and judged before the server stops: the copy lands on a
	// filesystem a managed Dolt (this city's, or another's) may write to.
	size, err := snapshotTreeBytes(dataDir)
	if err != nil {
		return nil, "", fmt.Errorf("sizing the managed Dolt's data directory %s: %w", dataDir, err)
	}
	if free, floor, short := snapshotTreeDiskShortfall(dir, size); short {
		return nil, "", fmt.Errorf("%s has %d bytes free; the copy of the managed Dolt's data directory needs %d and must leave %d free for a managed Dolt on the same filesystem. Free space, or pass --to elsewhere; the server was not stopped and nothing was copied", dir, free, size, floor)
	}
	doltpool.Shutdown()
	if err := migrationWindowStopProvider(cityPath); err != nil {
		return nil, "", fmt.Errorf("stopping the managed Dolt for the copy of its data directory: %w", err)
	}
	copiedAt := time.Now().UTC()
	tree, copyErr := copySnapshotTree(dir, dataDir, snapshotCityDoltDir)
	if err := w.restartProvider(); err != nil {
		return nil, "", errors.Join(copyErr, fmt.Errorf("starting the managed Dolt again after the copy of its data directory: %w", err))
	}
	if copyErr != nil {
		return nil, "", copyErr
	}
	return &snapshotExact{Method: snapshotMethodDoltDataDir, Tree: tree, CopiedAt: copiedAt}, "", nil
}

// snapshotTreeBytes is the size of every regular file under root.
func snapshotTreeBytes(root string) (int64, error) {
	var n int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		n += info.Size()
		return nil
	})
	return n, err
}

// snapshotTreeDiskShortfall reports whether a copy of size bytes into dir
// would leave less than the managed Dolt's critical floor free there, with
// the bytes free and the floor: a managed Dolt (this city's, or another's)
// may write to that filesystem. A free-space probe that fails reports no
// shortfall, as a binding snapshot's does (snapshotDiskPreflight).
func snapshotTreeDiskShortfall(dir string, size int64) (free, floor int64, short bool) {
	free, err := doltContainerFreeBytesFunc(dir)
	if err != nil {
		return 0, 0, false
	}
	floor = doltDiskMinFreeBytes()
	short, _ = snapshotDiskVerdict(free, size, floor, doltDiskWarnFreeBytes())
	return free, floor, short
}

// copySnapshotTree copies every regular file under src into the snapshot at
// rel, fsynced, and records each. A symlink or another special file refuses:
// a data directory holds none, and following one would copy what is not the
// store.
func copySnapshotTree(dir, src, relDir string) (*snapshotCopiedTree, error) {
	tree := &snapshotCopiedTree{Path: relDir}
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		r, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if r != "." {
				tree.Dirs = append(tree.Dirs, r)
			}
			return os.MkdirAll(filepath.Join(dir, relDir, r), 0o755)
		case !d.Type().IsRegular():
			return fmt.Errorf("%s is not a regular file or a directory", path)
		}
		file, err := copySnapshotFile(dir, path, filepath.Join(relDir, r))
		if err != nil {
			return err
		}
		tree.Files = append(tree.Files, file)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("copying the data directory %s: %w", src, err)
	}
	if len(tree.Files) == 0 {
		return nil, fmt.Errorf("the data directory %s holds no file", src)
	}
	return tree, nil
}

// snapshotCityRestorable reports why a snapshot's store cannot be restored
// exactly, or "".
func snapshotCityRestorable(rec snapshotStoreRecord) string {
	switch rec.Exact.Method {
	case snapshotMethodFileCopy, snapshotMethodDoltDataDir:
		return ""
	case snapshotMethodDoltTag:
		return "its exact copy is a Dolt tag, which does not cover the dolt_ignore'd tables bd keeps wisps in; snapshot the city again with its Dolt stopped, which copies the data directory"
	case snapshotMethodBdExport:
		return "its exact copy is a bd export, which bd cannot import into the store it came from as it was"
	}
	return "it has no exact copy; the JSONL is a record, not a restore source"
}

// storageCityRestoreJournal is .gc/storage-city-restore.json.
type storageCityRestoreJournal struct {
	Format    string    `json:"format"`
	Snapshot  string    `json:"snapshot"`
	To        string    `json:"to"`
	Suffix    string    `json:"aside_suffix"`
	StartedAt time.Time `json:"started_at"`
	// Done are the steps finished, in order.
	Done []string `json:"done"`
	// Binding is the binding root the restore moves aside, or "".
	Binding string `json:"binding,omitempty"`
	// PinnedSource reports that the restore pinned its snapshot, which it
	// found unpinned, so retention keeps it; the restore unpins it when it
	// ends.
	PinnedSource bool `json:"pinned_source,omitempty"`
	// Unproven is what the proof failed on when the operator ended the
	// restore with --accept-unproven: each row it failed on, by store, or why
	// it could not run.
	Unproven []string `json:"unproven,omitempty"`
}

// The city restore's steps, in order. It ends at verified, or at
// accepted-unproven when the operator ends it with a proof that fails.
const (
	cityRestoreStepStores   = "stores"
	cityRestoreStepRecords  = "records"
	cityRestoreStepCityTOML = "city.toml"
	cityRestoreStepBinding  = "binding"
	cityRestoreStepVerified = "verified"
	cityRestoreStepAccepted = "accepted-unproven"
)

func storageCityRestoreJournalPath(cityPath string) string {
	return filepath.Join(cityPath, ".gc", storageCityRestoreJournalName)
}

// readStorageCityRestoreJournal reads the journal; present is false when
// there is none. An unreadable one is an error, never an absence.
func readStorageCityRestoreJournal(cityPath string) (storageCityRestoreJournal, bool, error) {
	var j storageCityRestoreJournal
	data, err := os.ReadFile(storageCityRestoreJournalPath(cityPath))
	if errors.Is(err, fs.ErrNotExist) {
		return j, false, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &j)
	}
	if err != nil {
		return j, true, fmt.Errorf("reading the city restore journal %s: %w", storageCityRestoreJournalPath(cityPath), err)
	}
	return j, true, nil
}

// readUnfinishedCityRestore reads the journal of a restore that has not
// ended; unfinished is false when there is none.
func readUnfinishedCityRestore(cityPath string) (storageCityRestoreJournal, bool, error) {
	j, present, err := readStorageCityRestoreJournal(cityPath)
	return j, present && err == nil && !j.ended(), err
}

func writeStorageCityRestoreJournal(cityPath string, j storageCityRestoreJournal) error {
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, storageCityRestoreJournalPath(cityPath), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing the city restore journal: %w", err)
	}
	return nil
}

func (j storageCityRestoreJournal) done(step string) bool { return slices.Contains(j.Done, step) }

// ended reports that the restore is over: proven, or accepted unproven.
func (j storageCityRestoreJournal) ended() bool {
	return j.done(cityRestoreStepVerified) || j.done(cityRestoreStepAccepted)
}

// filesBack reports that every file the restore moves is back, so only the
// proof is left.
func (j storageCityRestoreJournal) filesBack() bool { return j.done(cityRestoreStepBinding) }

// state says what an unfinished restore has left.
func (j storageCityRestoreJournal) state() string {
	if j.filesBack() {
		return "every file is back, and the restored stores are not yet proven against the snapshot"
	}
	return "the city's stores, storage records and city.toml may be part-way back"
}

// remedy names the command that ends an unfinished restore and, once every
// file is back, the one that ends it when its proof fails.
func (j storageCityRestoreJournal) remedy() string {
	finish := snapshotCityRestoreInstruction(j.Snapshot, j.To)
	if !j.filesBack() {
		return fmt.Sprintf("Finish it with `%s`", finish)
	}
	return fmt.Sprintf("Finish it with `%s`; if its proof fails, `%s --%s` ends it with what the proof failed on recorded", finish, finish, cityRestoreAcceptFlag)
}

// snapshotCityRestoreInstruction is the command that restores snapshot id
// from to as the whole city.
func snapshotCityRestoreInstruction(id, to string) string {
	return "gc storage snapshot restore " + id + " --city --" + storageFleetStoppedFlag + " --to " + to
}

// storageCityRestoreRefusal refuses a writing storage verb while a city
// restore is unfinished: the city's stores, records and city.toml may be
// part-way back, and a write would land where the restore's proof reads.
func storageCityRestoreRefusal(cityPath string) error {
	j, unfinished, err := readUnfinishedCityRestore(cityPath)
	if err != nil || !unfinished {
		return err
	}
	return fmt.Errorf("a city restore of snapshot %s is unfinished (%s): %s. %s", j.Snapshot, storageCityRestoreJournalPath(cityPath), j.state(), j.remedy())
}

// storageCityRestoreBootRefusal holds boot until a city restore ends. Part-way,
// the city's stores, records and city.toml may be from different moments; with
// every file back, the proof that remains reads the restored stores, and a city
// that served them would write rows the proof cannot tell from the
// snapshot's, failing it for good.
func storageCityRestoreBootRefusal(cityPath string) error {
	j, unfinished, err := readUnfinishedCityRestore(cityPath)
	if err != nil || !unfinished {
		return err
	}
	return fmt.Errorf("a city restore of snapshot %s is unfinished (%s): %s, so the city does not start. %s", j.Snapshot, storageCityRestoreJournalPath(cityPath), j.state(), j.remedy())
}

// doStorageSnapshotRestoreCity puts a city back as snapshot id found it. With
// acceptUnproven it ends an unfinished restore of id whose every file is back
// and whose proof fails.
func doStorageSnapshotRestoreCity(ctx context.Context, request storageOperatorRequest, to, id string, acceptUnproven bool, logPrefix string, stdout, stderr io.Writer) int {
	r := &cityRestoreRun{request: request, id: id, logPrefix: logPrefix, stdout: stdout, stderr: stderr}
	bindingRoot, err := cityRestorePreconditions(request, id)
	if err != nil {
		return r.fail(err)
	}
	lock, err := acquireCityRestoreLock(request)
	if err != nil {
		return r.fail(err)
	}
	defer lock.Close() //nolint:errcheck // released last
	guard, err := acquireStorageMigrationGuard(ctx, request.CityPath)
	if err != nil {
		return r.fail(err)
	}
	defer guard.Release() //nolint:errcheck // released before the lock
	if r.to, err = filepath.Abs(to); err != nil {
		return r.fail(err)
	}
	dirLock, err := lockSnapshotForRestore(r.to, id)
	if err != nil {
		return r.fail(err)
	}
	defer dirLock.Close() //nolint:errcheck // releases the flock
	r.dir, r.lock = filepath.Join(r.to, id), lock
	if err := r.begin(ctx, bindingRoot, acceptUnproven); err != nil {
		return r.fail(err)
	}
	if err := r.moveFiles(); err != nil {
		return r.fail(fmt.Errorf("%w. The city is part-restored and held by %s: run this command again to finish", err, storageCityRestoreJournalPath(request.CityPath)))
	}
	return r.prove(acceptUnproven)
}

// cityRestorePreconditions is what the city restore checks before it takes a
// lock: the snapshot id, no shared-store attestation (it lifts nothing here:
// cityRestoreSharedStoreRefusal), no reverse migration in progress, and the
// operator's attestation that the fleet is stopped. It returns the binding
// root the city serves from, which the restore moves aside, or "".
func cityRestorePreconditions(request storageOperatorRequest, id string) (string, error) {
	if !storageSnapshotIDPattern.MatchString(id) {
		return "", fmt.Errorf("%q is not a snapshot id", id)
	}
	if len(request.Force.SharedStores) > 0 {
		return "", fmt.Errorf("--%s=%s:<identity> does not apply to --city: the restore replaces each store it puts back whole with the snapshot's copy, so it cannot leave another city's rows as they are, and it refuses a store another registered city uses whether or not that is attested; run it without the attestation", storageForceFlag, storageHazardSharedStore)
	}
	target, bound, err := resolveInfraBindingTarget(request.CityPath, request.Cfg)
	if err != nil {
		return "", err
	}
	root := ""
	if bound {
		root = target.Root
	}
	if err := storageReverseRefusal(request.CityPath, root); err != nil {
		return "", err
	}
	if !request.FleetStopped {
		return "", fmt.Errorf("pass --%s to attest that %s. A process this command cannot see that still has a store open would keep writing to the store this replaces", storageFleetStoppedFlag, storageFleetStoppedAttestation)
	}
	return root, nil
}

// acquireCityRestoreLock takes the controller lock with the liveness checks
// every writing storage verb runs (acquireStorageVerbExclusion), then the
// check every writer of the work store runs under it: no controller answers
// on the city (infraMigrationForeignControllerPID). The caller closes the
// lock when it exits.
func acquireCityRestoreLock(request storageOperatorRequest) (*os.File, error) {
	lock, err := acquireStorageVerbExclusion(request, storageVerbScope{CityRestore: true})
	if err != nil {
		return nil, err
	}
	if pid := infraMigrationForeignControllerPID(request.CityPath); pid != 0 {
		lock.Close() //nolint:errcheck // releasing on a refusal
		return nil, fmt.Errorf("controller PID %d is live on this city; while it runs, any writer may change the stores this replaces. Stop it (%s) and run this again", pid, storageStopCommand)
	}
	return lock, nil
}

// cityRestoreSharedStoreRefusal refuses a restore that would replace a store
// another registered city also uses (findStoreSharers): each store the plan
// puts back, the work store and each rig's, and every other database of the
// managed Dolt's data directory the restore replaces, live or copied, which
// no store of the city keeps but another city's may. The restore replaces a
// store whole with the snapshot's copy, so it cannot leave another city's rows
// as they are, and no attestation lifts this, as it does for the writing verbs
// that select the rows they carry. A registry or config that cannot be read
// refuses: absence of a sharer must be shown, not assumed.
func cityRestoreSharedStoreRefusal(cityPath string, plan cityRestorePlan) error {
	var refusals []string
	collect := func(_ string, sharers []storeSharer, err error) error {
		for _, s := range sharers {
			refusals = append(refusals, s.Refusal)
		}
		return err
	}
	for _, s := range plan.stores {
		label, root := sharedWorkStoreLabel, cityPath
		if s.rigDir != "" {
			label, root = "rig "+strings.TrimPrefix(s.rec.Name, "rig:"), s.rigDir
		}
		if err := collect(findStoreSharers(cityPath, label, root)); err != nil {
			return err
		}
	}
	for _, db := range plan.dolt.others() {
		if err := collect(findIdentitySharers(cityPath, "this city's managed Dolt", "", managedStoreIdentity(cityPath, db))); err != nil {
			return err
		}
	}
	if len(refusals) == 0 {
		return nil
	}
	return fmt.Errorf("%s. The city restore replaces each of these stores whole with the snapshot's copy, so it would roll back the rows that city owns with this city's, and no --%s=%s attestation lifts that. Move the other city off the store, or unregister it if it no longer uses it (`gc unregister <path>`), and run this again; `gc storage migrate --to-work` is the way back for a city whose store another city shares. This run moved nothing", strings.Join(refusals, "; "), storageForceFlag, storageHazardSharedStore)
}

// cityRestoreRun is one run of the city restore, under its locks.
type cityRestoreRun struct {
	request storageOperatorRequest
	id, to  string
	// dir is the snapshot's directory.
	dir       string
	lock      *os.File
	logPrefix string
	stdout    io.Writer
	stderr    io.Writer
	m         snapshotManifest
	plan      cityRestorePlan
	journal   storageCityRestoreJournal
}

// fail says err on stderr and returns the exit code of a failed run.
func (r *cityRestoreRun) fail(err error) int {
	fmt.Fprintf(r.stderr, "%s: %v\n", r.logPrefix, err) //nolint:errcheck // best-effort stderr
	return 1
}

// begin reads the journal, proves the snapshot and plans the restore. A run
// that finds this snapshot's restore unfinished resumes it; any other starts a
// new one.
func (r *cityRestoreRun) begin(ctx context.Context, bindingRoot string, acceptUnproven bool) error {
	journal, unfinished, err := readUnfinishedCityRestore(r.request.CityPath)
	if err != nil {
		return err
	}
	if unfinished && (journal.Snapshot != r.id || journal.To != r.to) {
		return fmt.Errorf("a city restore of snapshot %s from %s is unfinished (%s); finish it with `%s` before restoring another", journal.Snapshot, journal.To, storageCityRestoreJournalPath(r.request.CityPath), snapshotCityRestoreInstruction(journal.Snapshot, journal.To))
	}
	if acceptUnproven {
		if err := cityRestoreAcceptRefusal(journal, unfinished); err != nil {
			return err
		}
	}
	m, err := readSnapshotManifest(r.dir)
	if err == nil {
		err = verifySnapshotFiles(ctx, r.dir, m)
	}
	if err != nil {
		return fmt.Errorf("snapshot %s does not verify, so this run moved nothing: %w", r.id, err)
	}
	plan, err := planCityRestore(r.request, r.dir, m)
	if err != nil {
		return fmt.Errorf("snapshot %s cannot restore this city, so this run moved nothing: %w", r.id, err)
	}
	if err := cityRestoreSharedStoreRefusal(r.request.CityPath, plan); err != nil {
		return err
	}
	r.m, r.plan = m, plan
	recordStorageAttestation(r.request.CityPath, r.logPrefix, r.request.Force.acceptedHazards(), r.stderr)
	if !unfinished {
		return r.start(bindingRoot)
	}
	r.journal = journal
	fmt.Fprintf(r.stderr, "%s: finishing the city restore of snapshot %s that began at %s\n", r.logPrefix, r.id, journal.StartedAt.Format(time.RFC3339)) //nolint:errcheck // best-effort stderr
	return r.pinSource()
}

// cityRestoreAcceptRefusal refuses --accept-unproven where it ends nothing: it
// ends an unfinished restore whose every file is back and whose proof fails.
func cityRestoreAcceptRefusal(j storageCityRestoreJournal, unfinished bool) error {
	switch {
	case !unfinished:
		return fmt.Errorf("--%s ends an unfinished city restore whose proof fails, and none is unfinished; run the restore without it", cityRestoreAcceptFlag)
	case !j.filesBack():
		return fmt.Errorf("the city restore of snapshot %s has files left to move, so there is no proof yet to accept; finish it with `%s`, and pass --%s only if its proof then fails", j.Snapshot, snapshotCityRestoreInstruction(j.Snapshot, j.To), cityRestoreAcceptFlag)
	}
	return nil
}

// start begins a new restore: the disk check, then the journal, written
// before the first move, and the pin on the snapshot.
func (r *cityRestoreRun) start(bindingRoot string) error {
	if err := cityRestoreDiskRefusal(r.plan); err != nil {
		return err
	}
	pin := filepath.Join(r.dir, storageSnapshotPinName)
	_, pinErr := os.Stat(pin)
	if pinErr != nil && !errors.Is(pinErr, fs.ErrNotExist) {
		return fmt.Errorf("reading whether snapshot %s is pinned (%s): %w", r.id, pin, pinErr)
	}
	now := snapshotRestoreNow().UTC()
	r.journal = storageCityRestoreJournal{
		Format: storageCityRestoreFormat, Snapshot: r.id, To: r.to, StartedAt: now,
		Suffix:       ".pre-restore-" + now.Format("20060102T150405Z"),
		Binding:      bindingRoot,
		PinnedSource: pinErr != nil,
	}
	if err := writeStorageCityRestoreJournal(r.request.CityPath, r.journal); err != nil {
		return err
	}
	for _, notice := range r.plan.dolt.notices(r.journal.Suffix) {
		fmt.Fprintf(r.stderr, "%s: %s\n", r.logPrefix, notice) //nolint:errcheck // best-effort stderr
	}
	return r.pinSource()
}

// pinSource keeps retention (a later snapshot's --keep) off the snapshot
// while the restore is unfinished, when the restore owns the pin. Pinning
// again is harmless, so a resumed run pins it again.
func (r *cityRestoreRun) pinSource() error {
	if !r.journal.PinnedSource {
		return nil
	}
	reason := fmt.Sprintf("the source of the unfinished city restore %s; the restore unpins it when it ends", storageCityRestoreJournalPath(r.request.CityPath))
	if err := pinStorageSnapshot(r.to, r.id, reason); err != nil {
		return fmt.Errorf("pinning snapshot %s while the restore reads it: %w", r.id, err)
	}
	return nil
}

// cityRestoreStep is one journaled step that moves files.
type cityRestoreStep struct {
	name string
	run  func() error
}

// moveFiles puts every file back, one journaled step at a time: the stores,
// the storage records, city.toml, then the binding root moved aside.
func (r *cityRestoreRun) moveFiles() error {
	cityPath, suffix := r.request.CityPath, r.journal.Suffix
	for _, s := range []cityRestoreStep{
		{cityRestoreStepStores, func() error { return restoreCityStores(r.dir, r.plan, suffix) }},
		{cityRestoreStepRecords, func() error { return restoreCityRecords(cityPath, r.dir, r.m.CityRecord, suffix) }},
		{cityRestoreStepCityTOML, func() error { return restoreCityTOML(cityPath, r.dir, *r.m.CityRecord.CityTOML, suffix) }},
		{cityRestoreStepBinding, func() error { return moveBindingRootAside(r.journal.Binding, suffix) }},
	} {
		if r.journal.done(s.name) {
			continue
		}
		if err := s.run(); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
		if err := r.markDone(s.name); err != nil {
			return err
		}
	}
	return nil
}

// markDone journals step as done.
func (r *cityRestoreRun) markDone(step string) error {
	r.journal.Done = append(r.journal.Done, step)
	if err := writeStorageCityRestoreJournal(r.request.CityPath, r.journal); err != nil {
		return err
	}
	return storageSnapshotStep("city restore " + step)
}

// prove reads each restored store back and proves it against the snapshot's
// JSONL, then ends the restore: proven, or, when the proof fails and the
// operator passed --accept-unproven, accepted with what it failed on. A proof
// that fails without it leaves the city held.
func (r *cityRestoreRun) prove(acceptUnproven bool) int {
	proof, err := verifyRestoredCity(r.lock, r.request.CityPath, r.dir, r.plan, r.logPrefix, r.stderr)
	failed := proof.failures()
	if err != nil {
		failed = []string{"the proof could not run: " + err.Error()}
	}
	switch {
	case len(failed) == 0:
		return r.end(cityRestoreStepVerified, proof)
	case acceptUnproven:
		r.journal.Unproven = failed
		return r.end(cityRestoreStepAccepted, proof)
	}
	why := fmt.Sprintf("%d row(s) fail the proof against the snapshot's JSONL: %s", len(failed), cityRestoreList(failed))
	if err != nil {
		why = err.Error()
	}
	instruction := snapshotCityRestoreInstruction(r.id, r.to)
	return r.fail(fmt.Errorf("%s: %s. Every file is back as the snapshot holds it, and %s holds the city until the restore ends: resolve that and run this command again, or end the restore unproven with `%s --%s`, which records what the proof failed on and releases the city", cityRestoreStepVerified, why, storageCityRestoreJournalPath(r.request.CityPath), instruction, cityRestoreAcceptFlag))
}

// cityRestoreList names up to cityRestoreListed entries, and counts the rest.
func cityRestoreList(entries []string) string {
	if len(entries) <= cityRestoreListed {
		return strings.Join(entries, "; ")
	}
	return fmt.Sprintf("%s; and %d more", strings.Join(entries[:cityRestoreListed], "; "), len(entries)-cityRestoreListed)
}

// end closes the journal at step, records an accepted proof's attestation,
// unpins the snapshot the restore pinned, and reports.
func (r *cityRestoreRun) end(step string, proof cityRestoreProof) int {
	if err := r.markDone(step); err != nil {
		return r.fail(err)
	}
	if step == cityRestoreStepAccepted {
		r.recordAcceptance()
	}
	if r.journal.PinnedSource {
		if err := os.Remove(filepath.Join(r.dir, storageSnapshotPinName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(r.stderr, "%s: unpinning snapshot %s: %v; remove %s so retention may remove it\n", r.logPrefix, r.id, err, filepath.Join(r.dir, storageSnapshotPinName)) //nolint:errcheck // best-effort stderr
		}
	}
	r.report(step, proof)
	return 0
}

// recordAcceptance records the operator's --accept-unproven in the attestation
// log, then each entry of what the proof failed on, one line each
// (cityRestoreUnprovenLine): the journal holds them too, and the next city
// restore replaces the journal, while the log is only ever appended to.
func (r *cityRestoreRun) recordAcceptance() {
	operator, at := storageAttestationOperator(), time.Now().UTC().Format(time.RFC3339)
	hazard := cityRestoreUnprovenHazard + ":" + r.id
	fmt.Fprintf(r.stderr, "%s: attested --%s for snapshot %s (%s, %s)\n", r.logPrefix, cityRestoreAcceptFlag, r.id, operator, at) //nolint:errcheck // best-effort stderr
	lines := []string{storageAttestationLine(hazard, operator, at, r.logPrefix)}
	for _, entry := range r.journal.Unproven {
		lines = append(lines, cityRestoreUnprovenLine(hazard, entry))
	}
	appendStorageAttestationLines(r.request.CityPath, r.logPrefix, lines, r.stderr)
}

// cityRestoreUnprovenTag starts each line of the attestation log that names
// one entry an accepted city restore's proof failed on; gc doctor reads only
// the #gc:attested lines.
const cityRestoreUnprovenTag = "#gc:unproven"

// cityRestoreUnprovenLine is the attestation log's line for one entry of what
// an accepted restore's proof failed on, after the acceptance it belongs to
// (hazard), on one line whatever the entry holds.
func cityRestoreUnprovenLine(hazard, entry string) string {
	return cityRestoreUnprovenTag + " " + hazard + " " + strings.Join(strings.Fields(entry), " ")
}

// report says how the restore ended, the rows the snapshot's window excused,
// and everything the restore kept aside.
func (r *cityRestoreRun) report(step string, proof cityRestoreProof) {
	if step == cityRestoreStepAccepted {
		fmt.Fprintf(r.stdout, "Ended the city restore of snapshot %s unproven; %s records what its proof failed on:\n", r.id, storageCityRestoreJournalPath(r.request.CityPath)) //nolint:errcheck // best-effort stdout
		for _, f := range r.journal.Unproven {
			fmt.Fprintf(r.stdout, "  %s\n", f) //nolint:errcheck // best-effort stdout
		}
	} else {
		fmt.Fprintf(r.stdout, "Restored the city from snapshot %s: %s, city.toml and its storage records.\n", r.id, strings.Join(r.plan.names(), ", ")) //nolint:errcheck // best-effort stdout
	}
	for _, s := range proof {
		s.report(r.stdout, r.dir, s.store.aside(r.plan, r.journal.Suffix))
	}
	if d := r.plan.dolt; d != nil && len(d.unproven) > 0 {
		fmt.Fprintf(r.stdout, "The managed Dolt's database(s) %s, which no store of this city keeps, are back as the snapshot's copy holds them, unproven.\n", strings.Join(d.unproven, ", ")) //nolint:errcheck // best-effort stdout
	}
	if r.journal.Binding != "" {
		fmt.Fprintf(r.stdout, "The binding root it no longer serves from is kept as %s.\n", r.journal.Binding+r.journal.Suffix) //nolint:errcheck // best-effort stdout
	}
	fmt.Fprintf(r.stdout, "Every file it replaced is kept beside it as *%s:\n", r.journal.Suffix) //nolint:errcheck // best-effort stdout
	for _, aside := range r.asides() {
		fmt.Fprintf(r.stdout, "  %s\n", aside) //nolint:errcheck // best-effort stdout
	}
	fmt.Fprintf(r.stdout, "Check the city with `%s`, then `gc start`.\n", storageStatusInstruction()) //nolint:errcheck // best-effort stdout
}

// asides lists what the restore kept aside: each *<suffix> entry in a
// directory it replaced something in.
func (r *cityRestoreRun) asides() []string {
	dirs := []string{r.request.CityPath, filepath.Join(r.request.CityPath, ".gc")}
	for _, s := range r.plan.stores {
		if s.file != "" {
			dirs = append(dirs, filepath.Dir(s.file))
		}
	}
	if r.plan.dolt != nil {
		dirs = append(dirs, filepath.Dir(r.plan.dolt.dataDir))
	}
	if toml, err := cityTOMLFile(r.request.CityPath); err == nil {
		dirs = append(dirs, filepath.Dir(toml))
	}
	if r.journal.Binding != "" {
		dirs = append(dirs, filepath.Dir(r.journal.Binding))
	}
	var kept []string
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			kept = append(kept, fmt.Sprintf("(%s could not be listed: %v)", d, err))
			continue
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), r.journal.Suffix) {
				kept = append(kept, filepath.Join(d, e.Name()))
			}
		}
	}
	sort.Strings(kept)
	return slices.Compact(kept)
}

// cityRestorePlan is what a city restore puts back.
type cityRestorePlan struct {
	// stores are the snapshot's stores, each with where its exact copy goes.
	stores []cityRestoreStore
	// dolt is the managed Dolt's data directory, when the snapshot holds it.
	dolt *cityRestoreDolt
}

// cityRestoreStore is one store a city restore checks after it is put back.
type cityRestoreStore struct {
	rec snapshotStoreRecord
	// rigDir is the rig's store root, "" for the work store.
	rigDir string
	// file is where a file-copy goes back, "" for a store in the Dolt tree.
	file string
}

// aside is where the restore keeps the store it replaced.
func (s cityRestoreStore) aside(plan cityRestorePlan, suffix string) string {
	if s.file != "" || plan.dolt == nil {
		return s.file + suffix
	}
	return plan.dolt.dataDir + suffix
}

// cityRestoreDolt is the managed Dolt data directory a city restore puts
// back.
type cityRestoreDolt struct {
	tree    snapshotCopiedTree
	dataDir string
	// since are the databases the live data directory holds that the copy
	// does not (created since the snapshot); the restore moves them aside
	// with the directory.
	since []string
	// unproven are the databases the copy holds that no store of the
	// snapshot records and no scope of the city keeps its store in
	// (doltCopyCensus); the restore puts them back as the copy holds them,
	// and the proof does not read them.
	unproven []string
}

// others are the databases the restore replaces that no store it proves
// keeps: the live ones the copy does not hold, and the copy's unproven ones.
func (d *cityRestoreDolt) others() []string {
	if d == nil {
		return nil
	}
	out := slices.Concat(d.since, d.unproven)
	sort.Strings(out)
	return slices.Compact(out)
}

// notices say what the restore does with the databases no store it proves
// keeps, before it moves anything: the live ones the copy does not hold go
// aside with the directory, to dataDir+suffix, and the copy's unproven ones
// come back as it holds them.
func (d *cityRestoreDolt) notices(suffix string) []string {
	if d == nil {
		return nil
	}
	var out []string
	if len(d.since) > 0 {
		out = append(out, fmt.Sprintf("the managed Dolt's data directory holds database(s) %s that the snapshot does not (created since it was taken); the restore moves them aside with the directory, to %s", strings.Join(d.since, ", "), d.dataDir+suffix))
	}
	if len(d.unproven) > 0 {
		out = append(out, fmt.Sprintf("the snapshot's copy of the managed Dolt's data directory holds database(s) %s that no store of the snapshot records and no store of this city keeps; the restore puts them back as the copy holds them, unproven", strings.Join(d.unproven, ", ")))
	}
	return out
}

func (p cityRestorePlan) names() []string {
	out := make([]string, 0, len(p.stores))
	for _, s := range p.stores {
		out = append(out, s.rec.Name)
	}
	return out
}

// rigRoots are the rig stores the plan puts back, which the proof's
// maintenance window starts.
func (p cityRestorePlan) rigRoots() []string {
	var out []string
	for _, s := range p.stores {
		if s.rigDir != "" {
			out = append(out, s.rigDir)
		}
	}
	return out
}

// planCityRestore resolves where each of the snapshot's stores goes back, and
// refuses a snapshot this restore cannot put back exactly (the file comment).
// dir is the snapshot's directory.
func planCityRestore(request storageOperatorRequest, dir string, m snapshotManifest) (cityRestorePlan, error) {
	if err := cityRestoreRecordRefusal(request.CityPath, m); err != nil {
		return cityRestorePlan{}, err
	}
	plan, refused, err := planCityRestoreStores(request, m)
	if err != nil {
		return plan, err
	}
	refused = append(refused, cityRestoreSplitRefusals(request.CityPath, dir, m)...)
	refused = append(refused, cityRestoreRigsLost(request, m, plan)...)
	refused = append(refused, plan.censusDolt(cityScopeDoltDatabases(request))...)
	if len(refused) > 0 {
		return plan, errors.New(strings.Join(refused, "; "))
	}
	if !slices.ContainsFunc(plan.stores, func(s cityRestoreStore) bool { return s.rec.Name == "work" }) {
		return plan, errors.New("it holds no work store")
	}
	if plan.dolt != nil && snapshotManagedDoltRunning(request.CityPath) {
		return plan, fmt.Errorf("the city's managed Dolt is running, and its data directory is what this restores. Stop it with `%s` and run this again", storageStopCommand)
	}
	return plan, nil
}

// cityRestoreRecordRefusal refuses a snapshot that records no city to put
// back, or that was taken of another work store.
func cityRestoreRecordRefusal(cityPath string, m snapshotManifest) error {
	if m.CityRecord == nil {
		return errors.New("it records no city (it was taken without --work, or by a gc that did not record the city)")
	}
	if m.CityRecord.CityTOML == nil {
		return errors.New("it recorded no city.toml, so it cannot put the city's configuration back")
	}
	identity, err := currentWorkStoreIdentity(cityPath)
	if err != nil {
		return err
	}
	if m.Identity != identity.String() {
		return fmt.Errorf("it was taken of work store %s, and this city's is %s", m.Identity, identity.String())
	}
	return nil
}

// planCityRestoreStores places each of the snapshot's stores: a file copy at
// its scope's file, a data-directory copy once for every store in it. It
// lists each store it cannot place, with why.
func planCityRestoreStores(request storageOperatorRequest, m snapshotManifest) (cityRestorePlan, []string, error) {
	var plan cityRestorePlan
	var refused []string
	rigs := map[string]string{}
	for _, r := range snapshotRigs(request) {
		rigs[r.name] = r.rigDir
	}
	for _, rec := range m.Stores {
		store, why := cityRestoreStoreFor(request.CityPath, rigs, rec)
		if why == "" && rec.Exact.Method == snapshotMethodDoltDataDir {
			var err error
			if why, err = plan.placeDolt(request.CityPath, rec); err != nil {
				return plan, nil, err
			}
		}
		if why != "" {
			refused = append(refused, rec.Name+": "+why)
			continue
		}
		plan.stores = append(plan.stores, store)
	}
	return plan, refused, nil
}

// cityRestoreStoreFor is where rec goes back, or why it cannot. rigs maps
// each rig the city has to its store root.
func cityRestoreStoreFor(cityPath string, rigs map[string]string, rec snapshotStoreRecord) (cityRestoreStore, string) {
	if rec.Name == "binding" {
		return cityRestoreStore{}, "the snapshot holds a binding, whose records it keeps by hash and does not restore; restore its database with --binding, and use --city for a snapshot from before the split"
	}
	if why := snapshotCityRestorable(rec); why != "" {
		return cityRestoreStore{}, why
	}
	store := cityRestoreStore{rec: rec}
	root := cityPath
	if rec.Name != "work" {
		dir, ok := rigs[rec.Name]
		if !ok {
			return store, "this city has no such rig any more"
		}
		store.rigDir, root = dir, dir
	}
	if rec.Exact.Method == snapshotMethodFileCopy {
		store.file = filepath.Join(root, ".gc", "beads.json")
	}
	return store, ""
}

// placeDolt places the managed Dolt's data directory the first
// data-directory copy holds, with the live databases it does not; a later
// store whose copy is another tree cannot go back with it.
func (p *cityRestorePlan) placeDolt(cityPath string, rec snapshotStoreRecord) (string, error) {
	if p.dolt != nil {
		if rec.Exact.Tree.Path != p.dolt.tree.Path {
			return "its Dolt data directory is not the one the work store's copy holds", nil
		}
		return "", nil
	}
	dataDir, err := snapshotManagedDoltDataDir(cityPath)
	if err != nil {
		return "", fmt.Errorf("resolving the managed Dolt's data directory: %w", err)
	}
	live, err := doltDataDirDatabases(dataDir)
	if err != nil {
		return "", fmt.Errorf("reading the managed Dolt's data directory %s: %w", dataDir, err)
	}
	copied := rec.Exact.Tree.databases()
	since := slices.DeleteFunc(live, func(db string) bool { return slices.Contains(copied, db) })
	p.dolt = &cityRestoreDolt{tree: *rec.Exact.Tree, dataDir: dataDir, since: since}
	return "", nil
}

// censusDolt refuses a data-directory copy where the restore, which replaces
// the whole directory, would lose a store of the city unseen: a store whose
// database the copy does not hold has nothing to come back from, and a
// database a scope of the city keeps its store in (named, database to scope)
// that no store records would come back unproven (doltCopyCensus). Every other
// database the copy holds comes back as the copy holds it, and the plan keeps
// it as unproven, to name.
func (p *cityRestorePlan) censusDolt(named map[string]string) []string {
	if p.dolt == nil {
		return nil
	}
	copied := p.dolt.tree.databases()
	recorded := map[string]bool{}
	var refused []string
	for _, s := range p.stores {
		db := s.rec.Exact.Database
		switch {
		case s.rec.Exact.Method != snapshotMethodDoltDataDir:
		case db == "":
			refused = append(refused, s.rec.Name+": the snapshot records no database of the managed Dolt's data directory for it (a gc that did not record one took it); take a new snapshot")
		case !slices.Contains(copied, db):
			refused = append(refused, fmt.Sprintf("%s: the snapshot's copy of the managed Dolt's data directory holds no database %s", s.rec.Name, db))
		default:
			recorded[db] = true
		}
	}
	owned, unproven := doltCopyCensus(copied, recorded, named)
	if len(owned) > 0 {
		refused = append(refused, fmt.Sprintf("the snapshot's copy of the managed Dolt's data directory holds %s, which this city keeps its store(s) in, and the snapshot records no store for them, so the restore, which replaces the whole directory, would roll those stores back unproven; restore a snapshot that records them (`%s`, run before the split, takes one)", strings.Join(owned, ", "), cityRestoreSnapshotCommand))
	}
	p.dolt.unproven = unproven
	return refused
}

// cityRestoreSplitRefusals refuses a snapshot whose restore would leave the
// city split: one of a split city (it records a served-binding or cleared
// note and holds no binding, so the binding, the only copy of the city's
// infrastructure state, would be moved aside), and one whose city.toml,
// composed with the city's include fragments as they are now, routes a class
// off the work store, which boot refuses once the binding root is moved
// aside.
func cityRestoreSplitRefusals(cityPath, dir string, m snapshotManifest) []string {
	var refused []string
	if !slices.ContainsFunc(m.Stores, func(rec snapshotStoreRecord) bool { return rec.Name == "binding" }) {
		if rel := snapshotCitySplitRecord(m.CityRecord); rel != "" {
			refused = append(refused, fmt.Sprintf("it was taken of a split city (it records %s) and holds no binding, so restoring it would move the binding, the only copy of the city's infrastructure state, aside and leave the city split; the city restore puts back a city from before the split", rel))
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, m.CityRecord.CityTOML.Path))
	if err == nil {
		err = cityRestoreConfigRefusal(cityPath, data)
	}
	if err != nil {
		refused = append(refused, err.Error())
	}
	return refused
}

// cityTOMLOverlay reads city.toml as data and every other file from disk: the
// configuration a restore that puts city.toml's bytes back leaves.
type cityTOMLOverlay struct {
	fsys.OSFS
	path string
	data []byte
}

// ReadFile returns the overlay's bytes for city.toml and reads any other file.
func (o cityTOMLOverlay) ReadFile(name string) ([]byte, error) {
	if filepath.Clean(name) == filepath.Clean(o.path) {
		return slices.Clone(o.data), nil
	}
	return o.OSFS.ReadFile(name)
}

// cityRestoreConfigRefusal refuses a city.toml (its bytes, data) that,
// composed with the city's include fragments as they are now
// (config.ComposeFragments; the restore puts back city.toml alone), routes
// any class off the work store: boot refuses that city once the binding root
// is moved aside. It names the files that declare [storage].
func cityRestoreConfigRefusal(cityPath string, data []byte) error {
	tomlPath := filepath.Join(cityPath, "city.toml")
	cfg, prov, err := config.ComposeFragments(cityTOMLOverlay{path: tomlPath, data: data}, tomlPath)
	if err != nil {
		return fmt.Errorf("its city.toml does not compose with this city's include fragments as they are now: %w", err)
	}
	if shape, _ := storageSplitShapeOf(cfg.EffectiveStorage()); shape == storageSplitNone {
		return nil
	}
	var declared []string
	if md, err := toml.Decode(string(data), &struct{}{}); err == nil && md.IsDefined("storage") {
		declared = append(declared, "the snapshot's city.toml declares [storage] (it was taken after the split was authored; a city restore needs one from before)")
	}
	if len(prov.StorageFragments) > 0 {
		declared = append(declared, fmt.Sprintf("the include fragment(s) %s declare [storage], and the restore puts back city.toml alone: remove [storage] from them and run this again", strings.Join(prov.StorageFragments, ", ")))
	}
	return fmt.Errorf("the configuration it would leave routes infrastructure classes off the work store, and the city would not boot once the binding root is moved aside: %s", strings.Join(declared, "; "))
}

// snapshotCitySplitRecord names a record the snapshot holds that only a
// split city has (the served-binding note, the city's or a rig's cleared
// note), or "" when it holds none.
func snapshotCitySplitRecord(rec *snapshotCityRecord) string {
	if rec == nil {
		return ""
	}
	for _, f := range rec.Files {
		if f.File == nil {
			continue
		}
		base := filepath.Base(f.Rel)
		if base == filepath.Base(bornSplitServedNotePath("")) || base == filepath.Base(infraClearedNotePath("")) || strings.HasPrefix(base, "storage-infra-cleared.rig.") {
			return f.Rel
		}
	}
	return ""
}

// cityRestoreRigsLost refuses each rig the snapshot holds no store for whose
// import into the binding ran since the snapshot (its cleared note is not
// the one the snapshot recorded): the import moved rows out of that store
// into the binding root this restore moves aside, and the restore would leave
// them in no store the city reads. The binding restore's
// snapshotRigImportsLost is the same rule. A rig whose database the managed
// Dolt's copy holds is a store the snapshot holds (censusDolt refuses the copy
// otherwise), so a rig the snapshot does not hold has no database there.
func cityRestoreRigsLost(request storageOperatorRequest, m snapshotManifest, plan cityRestorePlan) []string {
	held := map[string]bool{}
	for _, s := range plan.stores {
		held[s.rec.Name] = true
	}
	recorded := map[string]*snapshotFile{}
	if m.CityRecord != nil {
		for _, f := range m.CityRecord.Files {
			recorded[f.Rel] = f.File
		}
	}
	var refused []string
	for _, r := range snapshotRigs(request) {
		if held[r.name] {
			continue
		}
		note := rigGraphClearedNotePath(request.CityPath, strings.TrimPrefix(r.name, "rig:"))
		current, err := hashSnapshotFile(filepath.Dir(note), note)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			refused = append(refused, fmt.Sprintf("%s: its cleared note %s cannot be read (%v), so whether its import ran since the snapshot cannot be told", r.name, note, err))
			continue
		}
		if was := recorded[rel(request.CityPath, note)]; was != nil && was.SHA256 == current.SHA256 {
			continue
		}
		refused = append(refused, fmt.Sprintf("%s: its import into the binding ran since the snapshot (%s), and the snapshot holds no copy of its store, so the rows the import moved would be left only in the binding root this restore moves aside; restore a snapshot taken with `%s` before the split, or move the rig's rows back first with `gc storage migrate --to-rigs --rig %s`", r.name, note, cityRestoreSnapshotCommand, strings.TrimPrefix(r.name, "rig:")))
	}
	return refused
}

// cityRestoreDiskRefusal refuses a restore whose copy of the managed Dolt's
// data directory would leave less than the managed Dolt's critical floor
// free where it lands: the restore installs the copy beside the directory it
// moves aside, on a filesystem a managed Dolt (this city's, or another's)
// may write to.
func cityRestoreDiskRefusal(plan cityRestorePlan) error {
	if plan.dolt == nil {
		return nil
	}
	parent := filepath.Dir(plan.dolt.dataDir)
	size := plan.dolt.tree.bytes()
	if free, floor, short := snapshotTreeDiskShortfall(parent, size); short {
		return fmt.Errorf("%s has %d bytes free; the restore installs the snapshot's copy of the managed Dolt's data directory (%d bytes) beside the one it moves aside and must leave %d free for a managed Dolt on the same filesystem. Free space and run this again; nothing was moved", parent, free, size, floor)
	}
	return nil
}

// moveAside renames path to path+suffix, unless a run already did; a path
// that is not there is nothing to move.
func moveAside(path, suffix string) error {
	if _, err := os.Lstat(path + suffix); err == nil {
		// An earlier run moved it; what is at path now is this restore's.
		return nil
	}
	if err := os.Rename(path, path+suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("moving %s aside: %w", path, err)
	}
	return nil
}

// installSnapshotCopy puts the snapshot's file at dst, proven by its sha256,
// through a temporary file renamed into place.
func installSnapshotCopy(dir string, want snapshotFile, dst string) error {
	if err := verifySnapshotFile(dir, want); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".restoring"
	_ = os.Remove(tmp)
	if err := copyFileSynced(filepath.Join(dir, want.Path), tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("installing %s: %w", dst, err)
	}
	syncInfraDir(filepath.Dir(dst))
	return nil
}

// restoreCityStores puts each store's exact copy back: a file store's file,
// and the managed Dolt's data directory once for every store in it.
func restoreCityStores(dir string, plan cityRestorePlan, suffix string) error {
	for _, s := range plan.stores {
		if s.file == "" {
			continue
		}
		if err := moveAside(s.file, suffix); err != nil {
			return err
		}
		if err := installSnapshotCopy(dir, *s.rec.Exact.File, s.file); err != nil {
			return fmt.Errorf("%s: %w", s.rec.Name, err)
		}
	}
	if plan.dolt == nil {
		return nil
	}
	if err := moveAside(plan.dolt.dataDir, suffix); err != nil {
		return err
	}
	// A run that stopped part-way through the copy left a partial tree.
	if err := os.RemoveAll(plan.dolt.dataDir); err != nil {
		return err
	}
	if err := installSnapshotTree(dir, plan.dolt.tree, plan.dolt.dataDir); err != nil {
		return fmt.Errorf("the managed Dolt's data directory: %w", err)
	}
	return nil
}

// snapshotTreeRelInside reports whether r, a path relative to a tree's root,
// stays inside it.
func snapshotTreeRelInside(r string) bool {
	clean := filepath.Clean(r)
	return r != "" && !filepath.IsAbs(r) && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

// installSnapshotTree puts a tree the snapshot holds back at dst: its
// directories, then its files.
func installSnapshotTree(dir string, tree snapshotCopiedTree, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, d := range tree.Dirs {
		if !snapshotTreeRelInside(d) {
			return fmt.Errorf("the directory %q is outside the tree %s", d, tree.Path)
		}
		if err := os.MkdirAll(filepath.Join(dst, d), 0o755); err != nil {
			return err
		}
	}
	for _, f := range tree.Files {
		r, err := filepath.Rel(tree.Path, f.Path)
		if err != nil {
			return err
		}
		if err := installSnapshotCopy(dir, f, filepath.Join(dst, r)); err != nil {
			return err
		}
	}
	return nil
}

// restoreCityRecords puts each storage record back as the snapshot found it:
// its bytes, or, when it was absent, moved aside. A rig cleared note written
// since the snapshot is moved aside too.
func restoreCityRecords(cityPath, dir string, rec *snapshotCityRecord, suffix string) error {
	kept := map[string]bool{}
	for _, f := range rec.Files {
		kept[f.Rel] = true
		dst := filepath.Join(cityPath, f.Rel)
		if err := moveAside(dst, suffix); err != nil {
			return err
		}
		if f.File == nil {
			continue
		}
		if err := installSnapshotCopy(dir, *f.File, dst); err != nil {
			return err
		}
	}
	now, err := snapshotCityRecordRels(cityPath)
	if err != nil {
		return err
	}
	for _, r := range now {
		if !kept[r] {
			if err := moveAside(filepath.Join(cityPath, r), suffix); err != nil {
				return err
			}
		}
	}
	return nil
}

// restoreCityTOML puts city.toml's bytes back. The file it replaces is
// copied aside, not moved: city.toml is what every gc loads the city from,
// the command that finishes an interrupted restore among them, so it stays in
// place until the snapshot's copy is renamed over it.
func restoreCityTOML(cityPath, dir string, want snapshotFile, suffix string) error {
	toml, err := cityTOMLFile(cityPath)
	if err != nil {
		return err
	}
	if err := copyAside(toml, suffix); err != nil {
		return err
	}
	return installSnapshotCopy(dir, want, toml)
}

// copyAside copies path to path+suffix, fsynced and with path's mode, unless
// a run already did; path itself is left where it is.
func copyAside(path, suffix string) error {
	aside := path + suffix
	if _, err := os.Lstat(aside); err == nil {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("keeping %s aside: %w", path, err)
	}
	tmp := aside + ".tmp"
	_ = os.Remove(tmp)
	err = copyFileSynced(path, tmp)
	if err == nil {
		err = os.Chmod(tmp, info.Mode().Perm())
	}
	if err == nil {
		err = os.Rename(tmp, aside)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("keeping %s aside: %w", path, err)
	}
	syncInfraDir(filepath.Dir(path))
	return nil
}

// moveBindingRootAside moves the binding root the city served from aside.
func moveBindingRootAside(root, suffix string) error {
	if root == "" {
		return nil
	}
	return moveAside(root, suffix)
}

// cityRestoreProof is each restored store read back against the snapshot's
// JSONL, in the plan's order.
type cityRestoreProof []restoredStoreProof

// failures lists what the proof failed on, each failing row by its store.
func (p cityRestoreProof) failures() []string {
	var out []string
	for _, s := range p {
		for _, f := range s.failed {
			out = append(out, s.store.rec.Name+": "+f)
		}
	}
	return out
}

// restoredStoreProof is one restored store read back against the snapshot's
// JSONL.
type restoredStoreProof struct {
	store cityRestoreStore
	// changedDuring are rows the JSONL dates in the snapshot's window that
	// the restored store holds as the copy found them; createdDuring are rows
	// it dates there that the restored store does not hold, because the copy
	// predates them. Both sorted.
	changedDuring, createdDuring []string
	// failed are the rows the proof fails on, each with why, sorted.
	failed []string
}

// verifyRestoredCity reads each restored store back, in a maintenance window
// opened with the restored city.toml, and proves it against the snapshot's
// JSONL. It first proves the restored configuration serves every class from
// the work store: a city the restore left split does not boot. An error is a
// proof that could not run.
func verifyRestoredCity(lock *os.File, cityPath, dir string, plan cityRestorePlan, logPrefix string, stderr io.Writer) (proof cityRestoreProof, err error) {
	toml, err := cityTOMLFile(cityPath)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(toml)
	if err != nil {
		return nil, fmt.Errorf("reading the restored city.toml: %w", err)
	}
	if err := cityRestoreConfigRefusal(cityPath, data); err != nil {
		return nil, err
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		return nil, fmt.Errorf("loading the restored city.toml: %w", err)
	}
	w, err := openMigrationSource(lock, cityPath, cfg, logPrefix, stderr, plan.rigRoots()...)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, w.Close()) }()
	for _, s := range plan.stores {
		p, err := verifyRestoredStore(cityPath, dir, s)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.rec.Name, err)
		}
		proof = append(proof, p)
	}
	return proof, nil
}

// snapshotRowChangedAt is when a row last changed, as its store records it:
// its update time, or its creation time when it records none.
func snapshotRowChangedAt(b beads.Bead) time.Time {
	if b.UpdatedAt.IsZero() {
		return b.CreatedAt
	}
	return b.UpdatedAt
}

// snapshotWindowHolds reports whether t falls in a snapshot's window: at or
// after copiedAt, the moment its store's copy was taken, and no later than
// readAt, when its rows were read for the JSONL, each to the second
// (snapshotReadBound). A snapshot that records either moment as zero (taken
// before gc recorded it) has no window.
func snapshotWindowHolds(t, copiedAt, readAt time.Time) bool {
	if copiedAt.IsZero() || readAt.IsZero() {
		return false
	}
	return !t.Before(copiedAt.Truncate(time.Second)) && !t.After(snapshotReadBound(readAt))
}

// snapshotReadBound is the latest time a row read at readAt can carry. A store
// may keep its times to the second, and Dolt rounds a DATETIME to the nearest
// one, so a write in the read's own second may be stored in the next; the
// JSONL holds no write made after its read, so the slack admits none.
func snapshotReadBound(readAt time.Time) time.Time {
	return readAt.Truncate(time.Second).Add(time.Second)
}

// verifyRestoredStore reads one restored store back and proves its rows
// against the snapshot's JSONL, row for row. The snapshot reads its JSONL
// from the live store after the copy, so a write in between lands in the
// JSONL alone: a row the restored store lacks or holds differently is excused
// when the JSONL dates it in the snapshot's window, and, for one it holds,
// the restored row changed no later than the JSONL read, to the second (a
// later change is none of the copy's). A restored row the JSONL lacks has no
// JSONL time to
// judge it by, so it always fails. An error is a store or JSONL that cannot
// be read.
func verifyRestoredStore(cityPath, dir string, s cityRestoreStore) (restoredStoreProof, error) {
	proof := restoredStoreProof{store: s}
	want, err := readSnapshotJSONLRows(dir, s.rec)
	if err != nil {
		return proof, fmt.Errorf("reading the snapshot's JSONL: %w", err)
	}
	rows, err := listRestoredStore(cityPath, s)
	if err != nil {
		return proof, err
	}
	for _, got := range rows {
		w, inJSONL := want[got.ID]
		delete(want, got.ID)
		proof.judgeHeld(got, w, inJSONL)
	}
	for _, id := range slices.Sorted(maps.Keys(want)) {
		proof.judgeMissing(want[id])
	}
	sort.Strings(proof.changedDuring)
	sort.Strings(proof.failed)
	return proof, nil
}

// readSnapshotJSONLRows reads a store's rows from the snapshot's JSONL, by
// id.
func readSnapshotJSONLRows(dir string, rec snapshotStoreRecord) (map[string]beads.Bead, error) {
	want := map[string]beads.Bead{}
	_, present, err := scanInfraBackupFile(filepath.Join(dir, rec.JSONL.Path), storageSnapshotJSONLFormat, rec.Name, func(e infraBackupEntry) error {
		want[e.Bead.ID] = e.Bead
		return nil
	})
	if err == nil && !present {
		err = errors.New("it is missing")
	}
	return want, err
}

// listRestoredStore lists every row of a restored store, closed and
// ephemeral ones included.
func listRestoredStore(cityPath string, s cityRestoreStore) ([]beads.Bead, error) {
	var store beads.Store
	var err error
	if s.rigDir == "" {
		store, err = openInfraMigrationSource(cityPath)
	} else {
		store, err = openStorageScopeStore(s.rigDir, cityPath)
	}
	if err != nil {
		return nil, fmt.Errorf("opening the restored store: %w", err)
	}
	defer closeBeadStoreHandle(store) //nolint:errcheck // read-only
	rows, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return nil, fmt.Errorf("listing the restored store: %w", err)
	}
	return rows, nil
}

// judgeHeld judges a row the restored store holds against its JSONL row w;
// inJSONL is false when the JSONL has none.
func (p *restoredStoreProof) judgeHeld(got, w beads.Bead, inJSONL bool) {
	if !inJSONL {
		p.failed = append(p.failed, got.ID+": the restored store holds it, and the snapshot's JSONL does not")
		return
	}
	got.IsBlocked, got.Dependencies, w.IsBlocked, w.Dependencies = nil, nil, nil, nil
	diff := beadCopyDifference(w, got)
	switch {
	case diff == "":
	case p.inWindow(w) && !snapshotRowChangedAt(got).After(snapshotReadBound(p.store.rec.JSONLReadAt)):
		p.changedDuring = append(p.changedDuring, got.ID)
	default:
		p.failed = append(p.failed, fmt.Sprintf("%s: differs from the snapshot's JSONL: %s", got.ID, diff))
	}
}

// judgeMissing judges a JSONL row the restored store lacks.
func (p *restoredStoreProof) judgeMissing(w beads.Bead) {
	if p.inWindow(w) {
		p.createdDuring = append(p.createdDuring, w.ID)
		return
	}
	p.failed = append(p.failed, w.ID+": the snapshot's JSONL holds it, and the restored store does not")
}

// inWindow reports whether the JSONL dates row b in the snapshot's window.
func (p *restoredStoreProof) inWindow(b beads.Bead) bool {
	return snapshotWindowHolds(snapshotRowChangedAt(b), p.store.rec.Exact.CopiedAt, p.store.rec.JSONLReadAt)
}

// report names the rows the snapshot's window excused, and where their
// later content survives: the snapshot's JSONL, and the store the restore
// kept aside (aside).
func (p *restoredStoreProof) report(w io.Writer, dir, aside string) {
	rec := p.store.rec
	window := fmt.Sprintf("after its copy (%s) and before its JSONL was read (%s)", rec.Exact.CopiedAt.Format(time.RFC3339), rec.JSONLReadAt.Format(time.RFC3339))
	survive := fmt.Sprintf("their later content is in the snapshot's JSONL %s and in the store kept aside as %s", filepath.Join(dir, rec.JSONL.Path), aside)
	if n := len(p.changedDuring); n > 0 {
		fmt.Fprintf(w, "%s: %d row(s) changed during the snapshot, %s, are restored as the copy found them: %s; %s.\n", rec.Name, n, window, strings.Join(p.changedDuring, ", "), survive) //nolint:errcheck // best-effort stdout
	}
	if n := len(p.createdDuring); n > 0 {
		fmt.Fprintf(w, "%s: %d row(s) created during the snapshot, %s, are not restored: %s; %s.\n", rec.Name, n, window, strings.Join(p.createdDuring, ", "), survive) //nolint:errcheck // best-effort stdout
	}
}

// snapshotCityConfigNote is a manifest note for a snapshot that records the
// city.
const snapshotCityConfigNote = "the snapshot records city.toml and the city's storage records (the id-floor file, the served-binding and cleared notes); `gc storage snapshot restore <id> --city` puts them back with the stores"
