package main

// The city half of a storage snapshot, and `gc storage snapshot restore <id>
// --city --fleet-stopped` (S-3 acceptance 3; owner decisions D-b and D-c).
//
// A city is more than its stores. What it serves from is decided by
// city.toml's [storage], what a gc may run on it by .gc/min-gc-version, and
// what a split has done by the storage records under .gc (the served-binding
// note, the cleared notes). A snapshot that includes the work store also
// records those (snapshotCityRecord): city.toml's bytes, and each record's
// bytes or its absence.
//
// A managed Dolt's exact copy is its data directory, copied with the server
// stopped (D-c; the five live rollbacks before this verb did exactly that).
// A Dolt tag covers the committed tables only, and bd keeps its wisps in
// dolt_ignore'd ones, so a tag is no way back to a store. The snapshot runs in
// the maintenance window, whose server is one it started or adopted, so it
// stops that server for the copy and starts it again for the JSONL read
// (snapshotManagedDoltDataDir). A rig whose database lives in the city's data
// directory (the managed layout) is in the same copy. A server the window did
// not start keeps the Dolt tag, and the manifest says why.
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
//   - a managed Dolt that is running, or a reverse migration in progress.
//
// Every file it replaces is moved aside as <path>.pre-restore-<ts>, never
// deleted. The journal (.gc/storage-city-restore.json) records the run before
// the first move and each step once it is done; run again, the same command
// finishes the steps that are not. Once every step is done the restore reads
// each restored store back (in a maintenance window opened with the restored
// city.toml, which starts a stopped provider and stops it after) and proves
// its rows equal to the snapshot's JSONL, row for row.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
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

// snapshotCopiedTree is a directory in a snapshot: every regular file under it.
type snapshotCopiedTree struct {
	Path  string         `json:"path"`
	Files []snapshotFile `json:"files"`
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

// verifySnapshotTree proves every file a tree records.
func verifySnapshotTree(dir string, tree *snapshotCopiedTree) error {
	if tree == nil || len(tree.Files) == 0 {
		return errors.New("records no file")
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
// back: whether a scope's store lives in the city's managed Dolt, whether
// the server runs, and its data directory.
var (
	snapshotScopeInManagedDolt = scopeStoreLivesInTheCitysManagedDolt
	snapshotManagedDoltRunning = func(cityPath string) bool { return currentManagedDoltPort(cityPath) != "" }
	snapshotManagedDoltDataDir = func(cityPath string) (string, error) {
		layout, err := resolveManagedDoltRuntimeLayout(cityPath)
		if err != nil {
			return "", err
		}
		return layout.DataDir, nil
	}
)

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
	doltpool.Shutdown()
	if err := migrationWindowStopProvider(cityPath); err != nil {
		return nil, "", fmt.Errorf("stopping the managed Dolt for the copy of its data directory: %w", err)
	}
	tree, copyErr := copySnapshotTree(dir, dataDir, snapshotCityDoltDir)
	if err := migrationWindowStartProvider(cityPath); err != nil {
		return nil, "", errors.Join(copyErr, fmt.Errorf("starting the managed Dolt again after the copy of its data directory: %w", err))
	}
	if copyErr != nil {
		return nil, "", copyErr
	}
	return &snapshotExact{Method: snapshotMethodDoltDataDir, Tree: tree}, "", nil
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
}

// The city restore's steps, in order.
const (
	cityRestoreStepStores   = "stores"
	cityRestoreStepRecords  = "records"
	cityRestoreStepCityTOML = "city.toml"
	cityRestoreStepBinding  = "binding"
	cityRestoreStepVerified = "verified"
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

// snapshotCityRestoreInstruction is the command that restores snapshot id
// from to as the whole city.
func snapshotCityRestoreInstruction(id, to string) string {
	return "gc storage snapshot restore " + id + " --city --" + storageFleetStoppedFlag + " --to " + to
}

// doStorageSnapshotRestoreCity puts a city back as snapshot id found it.
func doStorageSnapshotRestoreCity(ctx context.Context, request storageOperatorRequest, to, id, logPrefix string, stdout, stderr io.Writer) (code int) {
	fail := func(err error) int {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !storageSnapshotIDPattern.MatchString(id) {
		return fail(fmt.Errorf("%q is not a snapshot id", id))
	}
	target, bound, err := resolveInfraBindingTarget(request.CityPath, request.Cfg)
	if err != nil {
		return fail(err)
	}
	root := ""
	if bound {
		root = target.Root
	}
	if err := storageReverseRefusal(request.CityPath, root); err != nil {
		return fail(err)
	}
	if !request.FleetStopped {
		return fail(fmt.Errorf("pass --%s to attest that %s. A process this command cannot see that still has a store open would keep writing to the store this replaces", storageFleetStoppedFlag, storageFleetStoppedAttestation))
	}
	lock, err := acquireStorageVerbExclusion(request, storageVerbScope{CityRestore: true})
	if err != nil {
		return fail(err)
	}
	defer lock.Close() //nolint:errcheck // released last
	guard, err := acquireStorageMigrationGuard(ctx, request.CityPath)
	if err != nil {
		return fail(err)
	}
	defer guard.Release() //nolint:errcheck // released before the lock
	to, err = filepath.Abs(to)
	if err != nil {
		return fail(err)
	}
	dirLock, err := lockSnapshotForRestore(to, id)
	if err != nil {
		return fail(err)
	}
	defer dirLock.Close() //nolint:errcheck // releases the flock
	recordStorageAttestation(request.CityPath, logPrefix, request.Force.acceptedHazards(), stderr)

	journal, resuming, err := readStorageCityRestoreJournal(request.CityPath)
	if err != nil {
		return fail(err)
	}
	if resuming && (journal.Snapshot != id || journal.To != to) && !journal.done(cityRestoreStepVerified) {
		return fail(fmt.Errorf("a city restore of snapshot %s from %s is unfinished (%s); finish it with `%s` before restoring another", journal.Snapshot, journal.To, storageCityRestoreJournalPath(request.CityPath), snapshotCityRestoreInstruction(journal.Snapshot, journal.To)))
	}
	if resuming && journal.done(cityRestoreStepVerified) {
		resuming = false
	}

	dir := filepath.Join(to, id)
	m, err := readSnapshotManifest(dir)
	if err == nil {
		err = verifySnapshotFiles(ctx, dir, m)
	}
	if err != nil {
		return fail(fmt.Errorf("snapshot %s does not verify, so nothing was restored: %w", id, err))
	}
	plan, err := planCityRestore(request, m)
	if err != nil {
		return fail(fmt.Errorf("snapshot %s cannot restore this city, so nothing was restored: %w", id, err))
	}
	if !resuming {
		journal = storageCityRestoreJournal{
			Format: storageCityRestoreFormat, Snapshot: id, To: to, StartedAt: snapshotRestoreNow().UTC(),
			Suffix: ".pre-restore-" + snapshotRestoreNow().UTC().Format("20060102T150405Z"),
		}
		if bound {
			journal.Binding = target.Root
		}
		if err := writeStorageCityRestoreJournal(request.CityPath, journal); err != nil {
			return fail(err)
		}
	} else {
		fmt.Fprintf(stderr, "%s: finishing the city restore of snapshot %s that began at %s\n", logPrefix, id, journal.StartedAt.Format(time.RFC3339)) //nolint:errcheck // best-effort stderr
	}
	step := func(name string, run func() error) error {
		if journal.done(name) {
			return nil
		}
		if err := run(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		journal.Done = append(journal.Done, name)
		if err := writeStorageCityRestoreJournal(request.CityPath, journal); err != nil {
			return err
		}
		return storageSnapshotStep("city restore " + name)
	}
	unfinished := func(err error) int {
		return fail(fmt.Errorf("%w. The city is part-restored and held by %s: run this command again to finish", err, storageCityRestoreJournalPath(request.CityPath)))
	}
	if err := step(cityRestoreStepStores, func() error { return restoreCityStores(dir, plan, journal.Suffix) }); err != nil {
		return unfinished(err)
	}
	if err := step(cityRestoreStepRecords, func() error { return restoreCityRecords(request.CityPath, dir, m.CityRecord, journal.Suffix) }); err != nil {
		return unfinished(err)
	}
	if err := step(cityRestoreStepCityTOML, func() error { return restoreCityTOML(request.CityPath, dir, *m.CityRecord.CityTOML, journal.Suffix) }); err != nil {
		return unfinished(err)
	}
	if err := step(cityRestoreStepBinding, func() error { return moveBindingRootAside(journal.Binding, journal.Suffix) }); err != nil {
		return unfinished(err)
	}
	if err := step(cityRestoreStepVerified, func() error { return verifyRestoredCity(lock, request.CityPath, dir, plan, logPrefix, stderr) }); err != nil {
		return unfinished(err)
	}
	fmt.Fprintf(stdout, "Restored the city from snapshot %s: %s, city.toml and its storage records.\n", id, strings.Join(plan.names(), ", ")) //nolint:errcheck // best-effort stdout
	if journal.Binding != "" {
		fmt.Fprintf(stdout, "The binding root it no longer serves from is kept as %s.\n", journal.Binding+journal.Suffix) //nolint:errcheck // best-effort stdout
	}
	fmt.Fprintf(stdout, "Every file it replaced is kept beside it as *%s. Check the city with `%s`, then `gc start`.\n", journal.Suffix, storageStatusInstruction()) //nolint:errcheck // best-effort stdout
	return 0
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

// cityRestoreDolt is the managed Dolt data directory a city restore puts
// back.
type cityRestoreDolt struct {
	tree    snapshotCopiedTree
	dataDir string
}

func (p cityRestorePlan) names() []string {
	out := make([]string, 0, len(p.stores))
	for _, s := range p.stores {
		out = append(out, s.rec.Name)
	}
	return out
}

// planCityRestore resolves where each of the snapshot's stores goes back, and
// refuses a snapshot this restore cannot put back exactly (the file comment).
func planCityRestore(request storageOperatorRequest, m snapshotManifest) (cityRestorePlan, error) {
	var plan cityRestorePlan
	if m.CityRecord == nil {
		return plan, errors.New("it records no city (it was taken without --work, or by a gc that did not record the city)")
	}
	if m.CityRecord.CityTOML == nil {
		return plan, errors.New("it recorded no city.toml, so it cannot put the city's configuration back")
	}
	identity, err := currentWorkStoreIdentity(request.CityPath)
	if err != nil {
		return plan, err
	}
	if m.Identity != identity.String() {
		return plan, fmt.Errorf("it was taken of work store %s, and this city's is %s", m.Identity, identity.String())
	}
	rigs := map[string]string{}
	for _, r := range snapshotRigs(request) {
		rigs[r.name] = r.rigDir
	}
	var refused []string
	for _, rec := range m.Stores {
		if rec.Name == "binding" {
			refused = append(refused, "binding: the snapshot holds a binding, whose records it keeps by hash and does not restore; restore its database with --binding, and use --city for a snapshot from before the split")
			continue
		}
		if why := snapshotCityRestorable(rec); why != "" {
			refused = append(refused, rec.Name+": "+why)
			continue
		}
		store := cityRestoreStore{rec: rec}
		root := request.CityPath
		if rec.Name != "work" {
			dir, ok := rigs[rec.Name]
			if !ok {
				refused = append(refused, rec.Name+": this city has no such rig any more")
				continue
			}
			store.rigDir, root = dir, dir
		}
		switch rec.Exact.Method {
		case snapshotMethodFileCopy:
			store.file = filepath.Join(root, ".gc", "beads.json")
		case snapshotMethodDoltDataDir:
			if plan.dolt == nil {
				dataDir, err := snapshotManagedDoltDataDir(request.CityPath)
				if err != nil {
					return plan, fmt.Errorf("resolving the managed Dolt's data directory: %w", err)
				}
				plan.dolt = &cityRestoreDolt{tree: *rec.Exact.Tree, dataDir: dataDir}
			} else if rec.Exact.Tree.Path != plan.dolt.tree.Path {
				refused = append(refused, rec.Name+": its Dolt data directory is not the one the work store's copy holds")
				continue
			}
		}
		plan.stores = append(plan.stores, store)
	}
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
	for _, f := range plan.dolt.tree.Files {
		r, err := filepath.Rel(plan.dolt.tree.Path, f.Path)
		if err != nil {
			return err
		}
		if err := installSnapshotCopy(dir, f, filepath.Join(plan.dolt.dataDir, r)); err != nil {
			return fmt.Errorf("the managed Dolt's data directory: %w", err)
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

// restoreCityTOML puts city.toml's bytes back.
func restoreCityTOML(cityPath, dir string, want snapshotFile, suffix string) error {
	toml, err := cityTOMLFile(cityPath)
	if err != nil {
		return err
	}
	if err := moveAside(toml, suffix); err != nil {
		return err
	}
	return installSnapshotCopy(dir, want, toml)
}

// moveBindingRootAside moves the binding root the city served from aside.
func moveBindingRootAside(root, suffix string) error {
	if root == "" {
		return nil
	}
	return moveAside(root, suffix)
}

// verifyRestoredCity reads each restored store back, in a maintenance window
// opened with the restored city.toml, and proves its rows equal to the
// snapshot's JSONL.
func verifyRestoredCity(lock *os.File, cityPath, dir string, plan cityRestorePlan, logPrefix string, stderr io.Writer) (err error) {
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		return fmt.Errorf("loading the restored city.toml: %w", err)
	}
	var rigRoots []string
	for _, s := range plan.stores {
		if s.rigDir != "" {
			rigRoots = append(rigRoots, s.rigDir)
		}
	}
	w, err := openMigrationSource(lock, cityPath, cfg, logPrefix, stderr, rigRoots...)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, w.Close()) }()
	for _, s := range plan.stores {
		if err := verifyRestoredStore(cityPath, dir, s); err != nil {
			return fmt.Errorf("%s: %w", s.rec.Name, err)
		}
	}
	return nil
}

// verifyRestoredStore proves one restored store's rows equal to the
// snapshot's JSONL, row for row.
func verifyRestoredStore(cityPath, dir string, s cityRestoreStore) error {
	want := map[string]beads.Bead{}
	_, present, err := scanInfraBackupFile(filepath.Join(dir, s.rec.JSONL.Path), storageSnapshotJSONLFormat, s.rec.Name, func(e infraBackupEntry) error {
		want[e.Bead.ID] = e.Bead
		return nil
	})
	if err == nil && !present {
		err = errors.New("it is missing")
	}
	if err != nil {
		return fmt.Errorf("reading the snapshot's JSONL: %w", err)
	}
	var store beads.Store
	if s.rigDir == "" {
		store, err = openInfraMigrationSource(cityPath)
	} else {
		store, err = openStorageScopeStore(s.rigDir, cityPath)
	}
	if err != nil {
		return fmt.Errorf("opening the restored store: %w", err)
	}
	defer closeBeadStoreHandle(store) //nolint:errcheck // read-only
	rows, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return fmt.Errorf("listing the restored store: %w", err)
	}
	if len(rows) != len(want) {
		return fmt.Errorf("the restored store holds %d row(s), the snapshot %d", len(rows), len(want))
	}
	for _, got := range rows {
		w, ok := want[got.ID]
		if !ok {
			return fmt.Errorf("the restored store holds %s, which the snapshot does not", got.ID)
		}
		got.IsBlocked, got.Dependencies, w.IsBlocked, w.Dependencies = nil, nil, nil, nil
		if diff := beadCopyDifference(w, got); diff != "" {
			return fmt.Errorf("%s differs from the snapshot: %s", got.ID, diff)
		}
	}
	return nil
}

// storageCityRestoreRefusal refuses a writing storage verb while a city
// restore is unfinished: the city's stores, records and city.toml may be
// part-way back.
func storageCityRestoreRefusal(cityPath string) error {
	j, present, err := readStorageCityRestoreJournal(cityPath)
	switch {
	case err != nil:
		return err
	case !present || j.done(cityRestoreStepVerified):
		return nil
	}
	return fmt.Errorf("a city restore of snapshot %s is unfinished (%s): the city's stores, storage records and city.toml may be part-way back. Finish it with `%s`", j.Snapshot, storageCityRestoreJournalPath(cityPath), snapshotCityRestoreInstruction(j.Snapshot, j.To))
}

// snapshotCityConfigNote is a manifest note for a snapshot that records the
// city.
const snapshotCityConfigNote = "the snapshot records city.toml and the city's storage records (the id-floor file, the served-binding and cleared notes); `gc storage snapshot restore <id> --city` puts them back with the stores"
