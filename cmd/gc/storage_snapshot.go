package main

// `gc storage snapshot`: the mandatory snapshot and the binding's backup
// (S5 §5.3, and S6's finding that nothing else backs the binding up).
//
// A snapshot is a directory, <to>/<id>/, holding for each store asked for an
// exact copy where the store kind has one, and always the universal JSONL:
//
//   - the SQLite binding: VACUUM INTO over a read-only connection (one read
//     transaction, consistent under WAL with a live controller), then PRAGMA
//     integrity_check on the copy before the manifest. Its JSONL is read from
//     the copy, a page of ids at a time, so the two agree row for row and the
//     read holds one page in memory;
//   - a native Dolt work or rig store: a Dolt tag of HEAD (exact, copies
//     nothing; beads.NativeDoltTagger). It covers the committed tables, not
//     the dolt_ignore'd ones bd keeps wisps in: the JSONL holds those;
//   - a bd-backed store on another lane (bd CLI, hosted bd, proxied): `bd
//     export --all`, the core jsonl-export format;
//   - a file store: a copy of its file;
//   - a generic exec store, or anything else: the JSONL alone.
//
// The JSONL is the retained-source backup's line shape (infraBackupHeader,
// then one infraBackupEntry per row: the row, its class and every edge it
// holds with the payload the store carried on it), under its own format name,
// gc.storage.snapshot/v1, written and read by the same helpers
// (writeInfraBackupFile, scanInfraBackupFile). No tool restores a store from
// it yet; the manifest says so (JSONLRestorable).
//
// manifest.json is written LAST, atomically, and only after every file it
// names has been re-read from disk and proven against it
// (storage_snapshot_files.go). A run that fails removes its directory; one a
// SIGKILL ends leaves a directory without a manifest, which verify refuses and
// the next snapshot into the same directory removes before it measures free
// space. Snapshots into one directory are serialized by a flock there.
//
// Every snapshot takes the migration guard, so it never reads a binding a
// migrator is writing, and refuses while a re-copy or a rig import is in
// flight. The binding alone runs live otherwise: it only reads. A snapshot
// that includes the work store or the rigs runs in the maintenance window,
// under the controller lock and the --fleet-stopped attestation, as every
// writing storage verb does (storage_verb_exclusion.go), because a stopped
// managed-Dolt city has no server to read until the window starts one.
//
// Restore (storage_snapshot_restore.go) puts a snapshot's binding back. A
// work-store restore is a runbook step (owner decision O7).
//
// No function here takes or returns a store: each store is opened, read and
// closed inside one function, and the readers take the narrow read surfaces
// they use.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storebinding"
)

const (
	storageSnapshotVerb = "snapshot"
	// storageSnapshotDefaultKeep is the retention an operator snapshot gets
	// (O8: MC's binding is 2.15 GB per copy).
	storageSnapshotDefaultKeep = 2
	// storageSnapshotPage is the rows read, and held, at a time.
	storageSnapshotPage = 500
)

// The exact-copy methods a manifest records.
const (
	snapshotMethodVacuumInto = "vacuum-into"
	snapshotMethodDoltTag    = "dolt-tag"
	snapshotMethodBdExport   = "bd-export"
	snapshotMethodFileCopy   = "file-copy"
	snapshotMethodNone       = "none"
)

// The payload verdicts a manifest records per store.
const (
	snapshotPayloadsRead       = "read"
	snapshotPayloadsNone       = "none-by-protocol"
	snapshotPayloadsUnanswered = "unanswerable"
)

// snapshotDoltTagNote is what a Dolt-tag record says about the tag's reach.
const snapshotDoltTagNote = "the tag covers the committed tables; the dolt_ignore'd ones bd keeps wisps in are not in it, and the JSONL holds them"

// storageSnapshotStep runs between the snapshot's and the restore's steps; a
// test kills or fails the run there.
var storageSnapshotStep = func(string) error { return nil }

// snapshotBdExport runs `bd export --all -o out` in a bd-backed scope; a test
// seam. rigDir is empty for the city.
var snapshotBdExport = func(cityPath string, cfg *config.City, rigDir, out string) error {
	runner, dir := bdCommandRunnerForCity(cityPath), cityPath
	if rigDir != "" {
		runner, dir = bdCommandRunnerForRig(cityPath, cfg, rigDir), rigDir
	}
	if output, err := runner(dir, "bd", "export", "--all", "-o", out); err != nil {
		return fmt.Errorf("bd export: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// snapshotOptions is one `gc storage snapshot` run.
type snapshotOptions struct {
	Binding, Work, Rigs bool
	// To is the directory snapshots are written into.
	To   string
	Keep int
	// IfBound makes a city with no binding a successful no-op (the order). It
	// goes with --binding alone.
	IfBound bool
}

// storageSnapshotDefaultDir is .gc/snapshots: under .gc, so `gc init --from`
// (initFromSkip) never carries it, and excluded from the work-dir export
// (internal/citylayout).
func storageSnapshotDefaultDir(cityPath string) string {
	return filepath.Join(cityPath, ".gc", "snapshots")
}

func newStorageSnapshotCmd(surface storageCommandSurface, stdout, stderr io.Writer) *cobra.Command {
	var (
		opts         snapshotOptions
		verifyID     string
		fleetStopped bool
		forced       []string
	)
	prefix := "gc " + surface.Namespace + " " + storageSnapshotVerb
	cmd := &cobra.Command{
		Use:          storageSnapshotVerb,
		Short:        "Snapshot this city's stores: the binding, the work store, the rigs",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		Long: `Write a snapshot of this city's stores into a new directory <to>/<id>/.

Each store gets an exact copy where its kind has one and, always, a JSONL of
every row with its edges and their payloads:

  binding (SQLite)   VACUUM INTO, then PRAGMA integrity_check on the copy
  native Dolt        a Dolt tag of HEAD (wisps, in dolt_ignore'd tables, are
                     not in it; the JSONL holds them)
  bd CLI, hosted bd  bd export --all
  file               a copy of the file
  generic exec       the JSONL alone

The JSONL is a record, not yet a restore source: no tool imports it.

manifest.json records the stores, their row and edge counts, each file's
sha256, the gc version, the work store's identity, and the binding root's
records. It is written last, after every file is re-read and proven, so a
directory without one is an interrupted snapshot, never a complete one.

--binding alone runs against a live city: it only reads. --work and --rigs
need the city stopped and --fleet-stopped, and run in the maintenance window.
Every snapshot refuses while a migration, a re-copy, a rig import or a restore
holds the city.

--keep N keeps the newest N of this city's snapshots in <to> (this one always
among them, and a pinned one never counted or removed) and removes the rest of
this city's. Snapshots of another city or work store sharing <to> are left
alone. --verify ID re-reads a snapshot and proves it against its manifest.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if verifyID != "" {
				for _, flag := range []string{"binding", "work", "rigs", "keep", "if-bound", storageFleetStoppedFlag, storageForceFlag} {
					if cmd.Flags().Changed(flag) {
						fmt.Fprintf(stderr, "%s: --verify reads one snapshot and writes nothing; --%s means nothing with it\n", prefix, flag) //nolint:errcheck // best-effort stderr
						return errExit
					}
				}
			}
			request, err := resolveStorageOperatorRequest()
			if err != nil {
				fmt.Fprintf(stderr, "%s: %v\n", prefix, err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if opts.To == "" {
				opts.To = storageSnapshotDefaultDir(request.CityPath)
			}
			if verifyID != "" {
				return exitForCode(doStorageSnapshotVerify(cmd.Context(), opts.To, verifyID, prefix, stdout, stderr))
			}
			force, err := parseStorageForce(forced)
			if err != nil {
				fmt.Fprintf(stderr, "%s: %v\n", prefix, err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			request.FleetStopped, request.Force = fleetStopped, force
			return exitForCode(doStorageSnapshot(cmd.Context(), request, opts, prefix, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&opts.Binding, "binding", false, "snapshot the infrastructure binding (live; read-only)")
	cmd.Flags().BoolVar(&opts.Work, "work", false, "snapshot the city's work store (needs --"+storageFleetStoppedFlag+")")
	cmd.Flags().BoolVar(&opts.Rigs, "rigs", false, "snapshot every rig's store (needs --"+storageFleetStoppedFlag+")")
	cmd.Flags().StringVar(&opts.To, "to", "", "directory to write snapshots into (default <city>/.gc/snapshots)")
	cmd.Flags().IntVar(&opts.Keep, "keep", storageSnapshotDefaultKeep, "keep the newest N of this city's snapshots in --to, this one included, and remove the rest of this city's (at least 1); other cities' snapshots there are left alone")
	cmd.Flags().StringVar(&verifyID, "verify", "", "re-read snapshot ID in --to and prove it against its manifest, writing nothing")
	cmd.Flags().BoolVar(&opts.IfBound, "if-bound", false, "with --binding alone: on a city with no binding, do nothing and exit 0 (the binding-snapshot order)")
	cmd.Flags().BoolVar(&fleetStopped, storageFleetStoppedFlag, false, "attest that "+storageFleetStoppedAttestation)
	addStorageForceFlag(cmd, &forced)
	cmd.AddCommand(newStorageSnapshotRestoreCmd(prefix, stdout, stderr))
	return cmd
}

// doStorageSnapshot writes one snapshot and applies retention.
func doStorageSnapshot(ctx context.Context, request storageOperatorRequest, opts snapshotOptions, logPrefix string, stdout, stderr io.Writer) (code int) {
	fail := func(err error) int {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !opts.Binding && !opts.Work && !opts.Rigs {
		return fail(errors.New("name what to snapshot: --binding, --work, --rigs"))
	}
	if opts.IfBound && (opts.Work || opts.Rigs || !opts.Binding) {
		return fail(errors.New("--if-bound goes with --binding alone: it makes a city with no binding a no-op, and a work store or a rig is there either way"))
	}
	if opts.Keep < 1 {
		return fail(errors.New("--keep must be at least 1; the snapshot this run writes is always kept"))
	}
	target, bound, err := resolveInfraBindingTarget(request.CityPath, request.Cfg)
	if err != nil {
		return fail(err)
	}
	if !bound {
		if opts.IfBound {
			fmt.Fprintf(stdout, "%s: this city has no infrastructure binding; nothing to snapshot.\n", logPrefix) //nolint:errcheck // best-effort stdout
			return 0
		}
		if opts.Binding {
			return fail(errors.New("--binding: this city's [storage.classes] assign its infrastructure classes to no binding, so there is no binding to snapshot"))
		}
	}
	identity, err := currentWorkStoreIdentity(request.CityPath)
	if err != nil {
		return fail(err)
	}

	// The work store and the rigs are read in the maintenance window, under
	// the controller lock; every snapshot takes the migration guard.
	window := opts.Work || opts.Rigs
	var lock *os.File
	if window {
		if !request.FleetStopped {
			return fail(fmt.Errorf("--work and --rigs read stores only a stopped city may have opened by another process; pass --%s to attest that %s", storageFleetStoppedFlag, storageFleetStoppedAttestation))
		}
		if lock, err = acquireStorageVerbExclusion(request, storageVerbScope{}); err != nil {
			return fail(err)
		}
		defer lock.Close() //nolint:errcheck // released last
	}
	guard, err := storebinding.AcquireMigrationGuard(ctx, cityMigrationGuardDirectory(request.CityPath), storageMigrationGeneration)
	if errors.Is(err, storebinding.ErrMigrationGuardBusy) {
		return fail(errors.New("another storage command (a migration, a recovery, a purge or a restore) holds this city, so no snapshot was taken; the binding may be mid-write. Run this again when it is done (the binding-snapshot order retries at its next interval)"))
	}
	if err != nil {
		return fail(fmt.Errorf("taking the migration guard: %w", err))
	}
	defer guard.Release() //nolint:errcheck // released before the lock
	if bound {
		if err := snapshotInFlightRefusal(target); err != nil {
			return fail(err)
		}
	}
	if window {
		recordStorageAttestation(request.CityPath, logPrefix, request.Force.acceptedHazards(), stderr)
		var rigRoots []string
		if opts.Rigs {
			for _, rig := range snapshotRigs(request) {
				rigRoots = append(rigRoots, rig.root)
			}
		}
		w, err := openMigrationSource(lock, request.CityPath, request.Cfg, logPrefix, stderr, rigRoots...)
		if err != nil {
			return fail(err)
		}
		defer w.closeReporting(logPrefix, stderr, &code)
	}

	to, err := filepath.Abs(opts.To)
	if err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		return fail(err)
	}
	dirLock, err := lockSnapshotDir(to)
	if err != nil {
		return fail(err)
	}
	defer dirLock.Close() //nolint:errcheck // releases the flock
	// Interrupted snapshots go first, so the space they held is counted free.
	swept, err := sweepInterruptedSnapshots(to)
	for _, name := range swept {
		fmt.Fprintf(stdout, "removed the interrupted snapshot %s\n", filepath.Join(to, name)) //nolint:errcheck // best-effort stdout
	}
	if err != nil {
		return fail(err)
	}
	if opts.Binding {
		if err := snapshotDiskPreflight(to, target.Database, logPrefix, stderr); err != nil {
			return fail(err)
		}
	}

	now := time.Now().UTC()
	id, err := newSnapshotID(to, now)
	if err != nil {
		return fail(err)
	}
	dir := filepath.Join(to, id)
	manifest := snapshotManifest{
		Format:          storageSnapshotManifestFormat,
		ID:              id,
		CreatedAt:       now,
		GCVersion:       version + " " + commit,
		City:            request.CityPath,
		Identity:        identity.String(),
		JSONLRestorable: false,
		Notes:           []string{snapshotJSONLNote},
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		return fail(err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(dir) // still under the directory's flock
		}
	}()
	add := func(rec snapshotStoreRecord, err error) error {
		if err != nil {
			return fmt.Errorf("%s: %w", rec.Name, err)
		}
		manifest.Stores = append(manifest.Stores, rec)
		return storageSnapshotStep("stored " + rec.Name)
	}
	if opts.Binding {
		records, err := readSnapshotBindingRecords(request.CityPath, target)
		if err != nil {
			return fail(err)
		}
		manifest.Binding = &records
		err = add(snapshotBinding(ctx, target, dir))
		if err != nil {
			return fail(err)
		}
	}
	if opts.Work {
		if err := add(snapshotScope(request, snapshotScopeRef{name: "work", root: request.CityPath}, snapshotTagName(id, to), dir)); err != nil {
			return fail(err)
		}
	}
	if opts.Rigs {
		for _, rig := range snapshotRigs(request) {
			if err := add(snapshotScope(request, rig, snapshotTagName(id, to), dir)); err != nil {
				return fail(err)
			}
		}
	}
	if err := publishSnapshotManifest(ctx, dir, manifest); err != nil {
		return fail(err)
	}
	published = true
	removed, err := applySnapshotRetention(to, opts.Keep, id, snapshotOwner{City: request.CityPath, Identity: identity})
	for _, name := range removed {
		fmt.Fprintf(stdout, "removed %s\n", filepath.Join(to, name)) //nolint:errcheck // best-effort stdout
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: retention: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		code = 1
	}
	fmt.Fprintf(stdout, "Snapshot %s written to %s:\n", id, dir) //nolint:errcheck // best-effort stdout
	for _, rec := range manifest.Stores {
		fmt.Fprintf(stdout, "  %-10s %s, %d row(s), %d edge(s), payloads %s\n", rec.Name, describeSnapshotExact(rec.Exact), rec.Rows, rec.Edges, rec.Payloads) //nolint:errcheck // best-effort stdout
		for _, note := range rec.Notes {
			fmt.Fprintf(stdout, "             note: %s\n", note) //nolint:errcheck // best-effort stdout
		}
	}
	return code
}

func describeSnapshotExact(e snapshotExact) string {
	switch e.Method {
	case snapshotMethodDoltTag:
		return fmt.Sprintf("Dolt tag %s (%s)", e.Tag, e.Commit)
	case snapshotMethodNone:
		return "no exact copy (JSONL only)"
	}
	return e.Method + " " + e.File.Path
}

// snapshotTagName is the Dolt tag a snapshot takes: its id and a hash of the
// directory it is written into, so snapshots of one second into two
// directories do not take one tag.
func snapshotTagName(id, to string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(to)))
	return "gc-storage-snapshot-" + id + "-" + hex.EncodeToString(sum[:])[:8]
}

// snapshotInFlightRefusal refuses a binding a re-copy or a rig import is
// writing: a snapshot of either is a half-written binding.
func snapshotInFlightRefusal(target infraBindingTarget) error {
	present, err := infraPathExists(target.RecopyRecordPath())
	if err != nil {
		return fmt.Errorf("reading the re-copy record %s: %w", target.RecopyRecordPath(), err)
	}
	if present {
		return fmt.Errorf("a re-copy of the binding is in flight (%s); a snapshot of it would hold a partial copy. Finish it first", target.RecopyRecordPath())
	}
	// A restore under way reads a snapshot from a directory this snapshot's
	// retention could prune, and its binding is mid-replacement.
	if rec, restoring, _ := readInfraRestoreRecord(target); restoring {
		return fmt.Errorf("a restore of the binding is in flight (%s, snapshot %s); a snapshot now would copy a binding mid-replacement, and its retention could remove the snapshot the restore needs. Finish the restore first", target.RestoreRecordPath(), rec.Snapshot)
	}
	inFlight, err := filepath.Glob(filepath.Join(target.Root, rigGraphInFlightName("*")))
	if err != nil {
		return err
	}
	if len(inFlight) > 0 {
		sort.Strings(inFlight)
		return fmt.Errorf("a rig import is in flight (%s); a snapshot would hold its rows without their edges. Finish it first", strings.Join(inFlight, ", "))
	}
	return nil
}

// snapshotScopeRef names one work or rig scope.
type snapshotScopeRef struct {
	name, root, rigDir string
}

// snapshotRigs is every rig with a store of its own, in config order.
func snapshotRigs(request storageOperatorRequest) []snapshotScopeRef {
	var out []snapshotScopeRef
	cityRoot := resolveStoreScopeRoot(request.CityPath, request.CityPath)
	for _, rig := range request.Cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		root := resolveStoreScopeRoot(request.CityPath, rig.Path)
		if samePath(root, cityRoot) {
			continue
		}
		out = append(out, snapshotScopeRef{name: "rig:" + rig.Name, root: root, rigDir: root})
	}
	return out
}

// snapshotBinding copies the binding with VACUUM INTO and writes its JSONL
// from the copy, a page of ids at a time.
func snapshotBinding(ctx context.Context, target infraBindingTarget, dir string) (snapshotStoreRecord, error) {
	rec := snapshotStoreRecord{Name: "binding", Provider: config.StorageProviderSQLiteBeads}
	if _, err := os.Stat(target.Database); err != nil {
		return rec, fmt.Errorf("the binding database %s: %w", target.Database, err)
	}
	copyDir := filepath.Join(dir, "binding")
	if err := os.Mkdir(copyDir, 0o755); err != nil {
		return rec, err
	}
	copyPath := filepath.Join(copyDir, filepath.Base(target.Database))
	if err := beads.VacuumSQLiteInto(ctx, target.Database, copyPath); err != nil {
		return rec, err
	}
	// The copy is proven by integrity_check before the manifest
	// (verifySnapshotFiles), after its hash.
	file, err := hashSnapshotFile(dir, copyPath)
	if err != nil {
		return rec, err
	}
	rec.Exact = snapshotExact{Method: snapshotMethodVacuumInto, File: &file}
	rec.Notes = append(rec.Notes, "the copy is in rollback-journal mode, one self-contained file; restore switches it to WAL")
	if err := storageSnapshotStep("binding copied"); err != nil {
		return rec, err
	}
	ids, err := beads.SQLiteBeadIDs(ctx, copyPath)
	if err != nil {
		return rec, err
	}
	prefix, err := infraBindingIDPrefix()
	if err != nil {
		return rec, err
	}
	store, err := beads.OpenSQLiteStore(copyDir, beads.WithSQLiteStoreReadOnlyNoMmap(), beads.WithSQLiteStoreIDPrefix(prefix))
	if err != nil {
		return rec, fmt.Errorf("opening the copy: %w", err)
	}
	defer closeBeadStoreHandle(store) //nolint:errcheck // read-only
	batch, _ := beads.As[beads.DependencyBatchLister](store)
	payloadBatch, _ := beads.As[beads.DepMetadataBatchReader](store)
	payloads, _ := beads.As[beads.DepMetadataReader](store)
	_, absent := beads.DepMetadataAbsent(store)
	rec.Rows = len(ids)
	rec.JSONL, err = writeSnapshotJSONL(dir, "binding.jsonl", rec.Name, len(ids), func(emit func(infraBackupEntry) error) error {
		for start := 0; start < len(ids); start += storageSnapshotPage {
			page := ids[start:min(start+storageSnapshotPage, len(ids))]
			rows, err := store.List(beads.ListQuery{IDs: page, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
			if err != nil {
				return fmt.Errorf("listing the copy: %w", err)
			}
			if err := snapshotPageConsistency(page, rows); err != nil {
				return err
			}
			edges, verdict, err := emitSnapshotEntries(rows, store, batch, payloadBatch, payloads, absent, emit)
			if err != nil {
				return err
			}
			rec.Edges += edges
			rec.Payloads = verdict
		}
		return nil
	})
	if rec.Payloads == "" {
		rec.Payloads = snapshotPayloadVerdict(payloads, payloadBatch, absent)
	}
	return rec, err
}

// snapshotPageConsistency refuses a page whose rows are not exactly the ids
// it asked for. The page's ids come from the database's id column, and a row
// decodes its id from the bead it stores: a row whose two disagree is a
// damaged binding, and it is named rather than silently dropped or doubled
// (a copy of it would not restore the bead its id names).
func snapshotPageConsistency(page []string, rows []beads.Bead) error {
	want := make(map[string]bool, len(page))
	for _, id := range page {
		want[id] = true
	}
	var foreign []string
	for _, b := range rows {
		if !want[b.ID] {
			foreign = append(foreign, b.ID)
			continue
		}
		delete(want, b.ID)
	}
	if len(want) == 0 && len(foreign) == 0 {
		return nil
	}
	missing := sortedMapKeys(want)
	sort.Strings(foreign)
	readBack := "no row reads back under it"
	if len(foreign) > 0 {
		readBack = "the page read back as " + strings.Join(foreign, ", ")
	}
	return fmt.Errorf("the binding is inconsistent: row(s) %s are stored under an id their bead does not carry (%s), so no JSONL can record them faithfully. Repair those rows, then snapshot again", strings.Join(missing, ", "), readBack)
}

// snapshotScope snapshots the city's work store or one rig's: the exact copy
// its kind has, then its JSONL.
func snapshotScope(request storageOperatorRequest, scope snapshotScopeRef, tag, dir string) (snapshotStoreRecord, error) {
	provider := rawBeadsProviderForScope(scope.root, request.CityPath)
	rec := snapshotStoreRecord{Name: scope.name, Provider: provider}
	var store beads.Store
	var err error
	if scope.rigDir == "" {
		store, err = openInfraMigrationSource(request.CityPath)
	} else {
		store, err = openStorageScopeStore(scope.rigDir, request.CityPath)
	}
	if err != nil {
		return rec, fmt.Errorf("opening the store: %w", err)
	}
	defer closeBeadStoreHandle(store) //nolint:errcheck // best-effort close
	under, _, _ := unwrapBeadPolicyStore(store)
	tagger, taggable := beads.NativeDoltTagger(under)
	files := strings.ReplaceAll(scope.name, ":", "-")

	method := snapshotExactMethod(provider, taggable)
	if method == snapshotMethodDoltTag {
		result, err := tagger.DoltTag(tag, "gc storage snapshot "+filepath.Base(dir))
		switch {
		case err == nil:
			rec.Exact = snapshotExact{Method: snapshotMethodDoltTag, Tag: tag, Commit: result.Commit}
			rec.Notes = append(rec.Notes, snapshotDoltTagNote)
			if result.DirtyWorkingSet {
				rec.Notes = append(rec.Notes, "the working set held uncommitted changes the tag does not cover; the JSONL does")
			}
		case snapshotTagFallsBack(err) && providerUsesBdStoreContract(provider):
			// A read-only (proxied) or served handle cannot tag; bd can export.
			rec.Notes = append(rec.Notes, "this lane cannot take a Dolt tag ("+err.Error()+"); bd export instead")
			method = snapshotMethodBdExport
		default:
			return rec, err
		}
	}
	switch method {
	case snapshotMethodDoltTag:
	case snapshotMethodBdExport:
		if err := os.Mkdir(filepath.Join(dir, files), 0o755); err != nil {
			return rec, err
		}
		out := filepath.Join(dir, files, "export.jsonl")
		if err := snapshotBdExport(request.CityPath, request.Cfg, scope.rigDir, out); err != nil {
			return rec, err
		}
		file, err := syncAndHashSnapshotFile(dir, out)
		if err != nil {
			return rec, err
		}
		rec.Exact = snapshotExact{Method: snapshotMethodBdExport, File: &file}
	case snapshotMethodFileCopy:
		file, err := copySnapshotFile(dir, filepath.Join(scope.root, ".gc", "beads.json"), filepath.Join(files, "beads.json"))
		if err != nil {
			return rec, err
		}
		rec.Exact = snapshotExact{Method: snapshotMethodFileCopy, File: &file}
	default:
		rec.Exact = snapshotExact{Method: snapshotMethodNone}
		rec.Notes = append(rec.Notes, "provider "+provider+" has no exact snapshot, and no tool restores a store from the JSONL yet: this snapshot is a record of the store, not a way back to it")
	}

	rows, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return rec, fmt.Errorf("listing the store: %w", err)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	batch, _ := beads.As[beads.DependencyBatchLister](under)
	payloadBatch, _ := beads.As[beads.DepMetadataBatchReader](under)
	payloads, _ := beads.As[beads.DepMetadataReader](under)
	_, absent := beads.DepMetadataAbsent(under)
	rec.Rows = len(rows)
	rec.Payloads = snapshotPayloadVerdict(payloads, payloadBatch, absent)
	rec.JSONL, err = writeSnapshotJSONL(dir, files+".jsonl", rec.Name, len(rows), func(emit func(infraBackupEntry) error) error {
		for start := 0; start < len(rows); start += storageSnapshotPage {
			edges, _, err := emitSnapshotEntries(rows[start:min(start+storageSnapshotPage, len(rows))], store, batch, payloadBatch, payloads, absent, emit)
			if err != nil {
				return err
			}
			rec.Edges += edges
		}
		return nil
	})
	return rec, err
}

// snapshotExactMethod is the exact copy a scope's kind has (S5 §5.3 table).
func snapshotExactMethod(provider string, taggable bool) string {
	switch {
	case provider == "file":
		return snapshotMethodFileCopy
	case taggable:
		return snapshotMethodDoltTag
	case providerUsesBdStoreContract(provider):
		return snapshotMethodBdExport
	}
	return snapshotMethodNone
}

// snapshotTagFallsBack reports a tag refusal that says the lane cannot tag,
// not that the store failed.
func snapshotTagFallsBack(err error) bool {
	return errors.Is(err, beads.ErrProxiedNativeReadOnly) || errors.Is(err, beads.ErrDoltTagUnsupported)
}

// snapshotPayloadVerdict is what a store can say about its edges' payloads.
func snapshotPayloadVerdict(payloads beads.DepMetadataReader, payloadBatch beads.DepMetadataBatchReader, absent bool) string {
	switch {
	case absent:
		return snapshotPayloadsNone
	case payloads == nil && payloadBatch == nil:
		return snapshotPayloadsUnanswered
	}
	return snapshotPayloadsRead
}

// emitSnapshotEntries builds one page's JSONL entries with the copy engine's
// readers (infra_copy_engine.go), every row's down edges and their payloads,
// in batches where the store reads them so, and emits them in id order. Every
// row of the store is recorded, so each edge is recorded once, on the row it
// leaves, and a page needs no Dependents.
func emitSnapshotEntries(rows []beads.Bead, one infraEdgeLister, batch beads.DependencyBatchLister, payloadBatch beads.DepMetadataBatchReader, payloads beads.DepMetadataReader, absent bool, emit func(infraBackupEntry) error) (int, string, error) {
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	ids := make([]string, 0, len(rows))
	page := make(map[string]bool, len(rows))
	for _, b := range rows {
		ids = append(ids, b.ID)
		page[b.ID] = true
	}
	down, err := readInfraDownEdges(batch, one, ids)
	if err != nil {
		return 0, "", err
	}
	verdict, carried := snapshotPayloadVerdict(payloads, payloadBatch, absent), map[beads.DepPair]string{}
	if verdict == snapshotPayloadsRead {
		every := func(beads.Dep) bool { return true }
		if carried, err = readInfraPayloads(payloadBatch, payloads, down, every); err != nil {
			return 0, "", err
		}
	}
	edges := 0
	for _, entry := range infraBackupEntriesFrom(rows, down, carried, page) {
		edges += len(entry.Deps)
		if err := emit(entry); err != nil {
			return 0, "", err
		}
	}
	return edges, verdict, nil
}
