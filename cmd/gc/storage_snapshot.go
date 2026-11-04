package main

// `gc storage snapshot`: the mandatory snapshot and the binding's backup
// (S5 §5.3, and S6's finding that nothing else backs the binding up).
//
// A snapshot is a directory, <to>/<id>/, holding for each store asked for an
// exact copy where the store kind has one, and always the universal JSONL:
//
//   - the SQLite binding: VACUUM INTO over a read-only connection (one read
//     transaction, consistent under WAL with a live controller), then PRAGMA
//     integrity_check on the copy. Its JSONL is read from the copy, so the two
//     agree row for row;
//   - a native Dolt work or rig store: a Dolt tag of HEAD (exact, copies
//     nothing; beads.DoltTagger);
//   - a bd-backed store on another lane (bd CLI, hosted bd, proxied): `bd
//     export --all`, the core jsonl-export format;
//   - a file store: a copy of its file;
//   - a generic exec store, or anything else: the JSONL alone.
//
// The JSONL is the retained-source backup's line shape
// (infraBackupHeader, then one infraBackupEntry per row: the row, its class
// and every edge it holds with the payload the store carried on it), under
// its own format name, gc.storage.snapshot/v1.
//
// manifest.json is written LAST, atomically (temp, fsync, rename, directory
// fsync), and only after every file it names has been re-read from disk and
// proven against it. A directory without one is an interrupted snapshot, at
// any point a SIGKILL can land: verify refuses it and the next snapshot into
// the same directory removes it. Snapshots into one directory are serialized
// by a flock there, so the one that removes it is never racing the one that
// is writing it.
//
// The binding alone runs live and takes no lock: it only reads. A snapshot
// that includes the work store or the rigs runs in the maintenance window,
// under the controller lock and the --fleet-stopped attestation, as every
// writing storage verb does (storage_verb_exclusion.go), because a stopped
// managed-Dolt city has no server to read until the window starts one.
//
// Restore puts a snapshot's binding back, under the same lock and
// attestation. A work-store restore is a runbook step (owner decision O7).
//
// No function here takes or returns a store: each store is opened, read and
// closed inside one function, and the readers below take the narrow read
// surfaces they use.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

const (
	storageSnapshotVerb = "snapshot"
	// storageSnapshotManifestName is written last; its presence is what makes
	// a snapshot directory a snapshot.
	storageSnapshotManifestName = "manifest.json"
	// storageSnapshotManifestFormat versions manifest.json.
	storageSnapshotManifestFormat = "gc.storage.snapshot.manifest/v1"
	// storageSnapshotJSONLFormat versions the universal JSONL.
	storageSnapshotJSONLFormat = "gc.storage.snapshot/v1"
	// storageSnapshotDefaultKeep is the retention an operator snapshot gets
	// (O8: MC's binding is 2.15 GB per copy).
	storageSnapshotDefaultKeep = 2
	// storageSnapshotLockName serializes snapshots into one directory.
	storageSnapshotLockName = ".snapshot.lock"
	// storageSnapshotDiskMargin is the free space a snapshot leaves beyond
	// its estimate.
	storageSnapshotDiskMargin = 64 << 20
	// storageSnapshotBatch is the anchors per batched edge read.
	storageSnapshotBatch = 500
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

// storageSnapshotIDPattern is a snapshot id: a UTC second, and a suffix when
// two land in one second. Retention touches nothing else in the directory.
var storageSnapshotIDPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z(-[0-9]+)?$`)

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

// snapshotManifest is manifest.json.
type snapshotManifest struct {
	Format    string    `json:"format"`
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	// GCVersion is the binary that wrote the snapshot.
	GCVersion string `json:"gc_version"`
	City      string `json:"city"`
	// Identity is the city work store's identity tuple (storage_store_identity.go).
	Identity string                `json:"identity"`
	Stores   []snapshotStoreRecord `json:"stores"`
}

// snapshotStoreRecord is one store in a snapshot.
type snapshotStoreRecord struct {
	// Name is binding, work, or rig:<name>.
	Name     string        `json:"name"`
	Provider string        `json:"provider"`
	Exact    snapshotExact `json:"exact"`
	JSONL    snapshotFile  `json:"jsonl"`
	Rows     int           `json:"rows"`
	Edges    int           `json:"edges"`
	Payloads string        `json:"payloads"`
	Notes    []string      `json:"notes,omitempty"`
}

// snapshotExact is a store's exact copy.
type snapshotExact struct {
	Method string        `json:"method"`
	File   *snapshotFile `json:"file,omitempty"`
	Tag    string        `json:"tag,omitempty"`
	Commit string        `json:"commit,omitempty"`
}

// snapshotFile is one file in a snapshot, relative to its directory.
type snapshotFile struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// snapshotOptions is one `gc storage snapshot` run.
type snapshotOptions struct {
	Binding, Work, Rigs bool
	// To is the directory snapshots are written into.
	To   string
	Keep int
	// IfBound makes a city with no binding a successful no-op (the order).
	IfBound bool
}

// storageSnapshotDefaultDir is .gc/snapshots: under .gc, so neither the
// jsonl-export archive nor `gc init --from` (initFromSkip) carries it.
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
  native Dolt        a Dolt tag of HEAD (gc-storage-snapshot-<id>)
  bd CLI, hosted bd  bd export --all
  file               a copy of the file
  generic exec       the JSONL alone

manifest.json records the stores, their row and edge counts, each file's
sha256, the gc version and the work store's identity. It is written last, after
every file is re-read and proven, so a directory without one is an interrupted
snapshot, never a complete one.

--binding alone runs against a live city and takes no lock: it only reads.
--work and --rigs need the city stopped and --fleet-stopped, and run in the
maintenance window. --keep N keeps the newest N snapshots in <to> and removes
the rest, interrupted ones included. --verify ID re-reads a snapshot and proves
it against its manifest.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
	cmd.Flags().IntVar(&opts.Keep, "keep", storageSnapshotDefaultKeep, "keep the newest N snapshots in --to and remove the rest")
	cmd.Flags().StringVar(&verifyID, "verify", "", "re-read snapshot ID in --to and prove it against its manifest, writing nothing")
	cmd.Flags().BoolVar(&opts.IfBound, "if-bound", false, "on a city with no binding, do nothing and exit 0 (the binding-snapshot order)")
	cmd.Flags().BoolVar(&fleetStopped, storageFleetStoppedFlag, false, "attest that "+storageFleetStoppedAttestation)
	addStorageForceFlag(cmd, &forced)
	cmd.AddCommand(newStorageSnapshotRestoreCmd(prefix, stdout, stderr))
	return cmd
}

func newStorageSnapshotRestoreCmd(prefix string, stdout, stderr io.Writer) *cobra.Command {
	var (
		binding, fleetStopped bool
		to                    string
		forced                []string
	)
	prefix += " restore"
	cmd := &cobra.Command{
		Use:          "restore <id>",
		Short:        "Restore the binding from a snapshot (city stopped)",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		Long: `Put snapshot <id>'s binding database back in place of the live one.

The snapshot is proven against its manifest first (sha256 and integrity_check),
then installed under the controller lock with the city stopped. The database it
replaces, with its -wal and -shm, is kept beside it as *.pre-restore-<ts>. A
restore interrupted before the new database is in place leaves no database at
the binding path, which every boot refuses; run the same command again.

Only --binding is restorable here. A work store is restored by the runbook:
DOLT_RESET --hard to the snapshot's tag, or bd import of its export.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !binding {
				fmt.Fprintf(stderr, "%s: pass --binding; it is the one store this command restores (a work store is restored by DOLT_RESET --hard to the snapshot's tag, or bd import of its export)\n", prefix) //nolint:errcheck // best-effort stderr
				return errExit
			}
			force, err := parseStorageForce(forced)
			if err != nil {
				fmt.Fprintf(stderr, "%s: %v\n", prefix, err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			request, err := resolveStorageOperatorRequest()
			if err != nil {
				fmt.Fprintf(stderr, "%s: %v\n", prefix, err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			request.FleetStopped, request.Force = fleetStopped, force
			if to == "" {
				to = storageSnapshotDefaultDir(request.CityPath)
			}
			return exitForCode(doStorageSnapshotRestore(cmd.Context(), request, to, args[0], prefix, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&binding, "binding", false, "restore the infrastructure binding")
	cmd.Flags().StringVar(&to, "to", "", "directory the snapshot is in (default <city>/.gc/snapshots)")
	cmd.Flags().BoolVar(&fleetStopped, storageFleetStoppedFlag, false, "attest that "+storageFleetStoppedAttestation)
	addStorageForceFlag(cmd, &forced)
	return cmd
}

// doStorageSnapshot writes one snapshot and applies retention.
func doStorageSnapshot(ctx context.Context, request storageOperatorRequest, opts snapshotOptions, logPrefix string, stdout, stderr io.Writer) (code int) {
	if !opts.Binding && !opts.Work && !opts.Rigs {
		fmt.Fprintf(stderr, "%s: name what to snapshot: --binding, --work, --rigs\n", logPrefix) //nolint:errcheck // best-effort stderr
		return 1
	}
	if opts.Keep < 1 {
		fmt.Fprintf(stderr, "%s: --keep must be at least 1; the snapshot this run writes is always kept\n", logPrefix) //nolint:errcheck // best-effort stderr
		return 1
	}
	target, bound, err := resolveInfraBindingTarget(request.CityPath, request.Cfg)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !bound {
		if opts.IfBound {
			fmt.Fprintf(stdout, "%s: this city has no infrastructure binding; nothing to snapshot.\n", logPrefix) //nolint:errcheck // best-effort stdout
			return 0
		}
		if opts.Binding {
			fmt.Fprintf(stderr, "%s: --binding: this city's [storage.classes] assign its infrastructure classes to no binding, so there is no binding to snapshot\n", logPrefix) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	identity, err := currentWorkStoreIdentity(request.CityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// The work store and the rigs are read in the maintenance window.
	if opts.Work || opts.Rigs {
		if !request.FleetStopped {
			fmt.Fprintf(stderr, "%s: --work and --rigs read stores only a stopped city may have opened by another process; pass --%s to attest that %s\n", logPrefix, storageFleetStoppedFlag, storageFleetStoppedAttestation) //nolint:errcheck // best-effort stderr
			return 1
		}
		lock, err := acquireStorageVerbExclusion(request, storageVerbScope{})
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		defer lock.Close() //nolint:errcheck // released last
		guard, err := acquireStorageMigrationGuard(ctx, request.CityPath)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		defer guard.Release() //nolint:errcheck // released before the lock
		recordStorageAttestation(request.CityPath, logPrefix, request.Force.acceptedHazards(), stderr)
		var rigRoots []string
		if opts.Rigs {
			for _, rig := range snapshotRigs(request) {
				rigRoots = append(rigRoots, rig.root)
			}
		}
		window, err := openMigrationSource(lock, request.CityPath, request.Cfg, logPrefix, stderr, rigRoots...)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		defer window.closeReporting(logPrefix, stderr, &code)
	}

	to, err := filepath.Abs(opts.To)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	dirLock, err := lockSnapshotDir(to)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer dirLock.Close() //nolint:errcheck // releases the flock
	if opts.Binding {
		if err := snapshotDiskPreflight(to, target.Database); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	now := time.Now().UTC()
	id, err := newSnapshotID(to, now)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	dir := filepath.Join(to, id)
	manifest := snapshotManifest{
		Format:    storageSnapshotManifestFormat,
		ID:        id,
		CreatedAt: now,
		GCVersion: version + " " + commit,
		City:      request.CityPath,
		Identity:  identity.String(),
	}
	add := func(rec snapshotStoreRecord, err error) error {
		if err != nil {
			return fmt.Errorf("%s: %w", rec.Name, err)
		}
		manifest.Stores = append(manifest.Stores, rec)
		return storageSnapshotStep("stored " + rec.Name)
	}
	err = os.Mkdir(dir, 0o755)
	if err == nil && opts.Binding {
		err = add(snapshotBinding(ctx, target, dir))
	}
	if err == nil && opts.Work {
		err = add(snapshotScope(request, snapshotScopeRef{name: "work", root: request.CityPath}, id, dir))
	}
	if err == nil && opts.Rigs {
		for _, rig := range snapshotRigs(request) {
			if err = add(snapshotScope(request, rig, id, dir)); err != nil {
				break
			}
		}
	}
	if err == nil {
		err = publishSnapshotManifest(ctx, dir, manifest)
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v. %s holds no manifest, so it is not a snapshot; the next snapshot into %s removes it\n", logPrefix, err, dir, to) //nolint:errcheck // best-effort stderr
		return 1
	}
	removed, err := applySnapshotRetention(to, opts.Keep)
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
// from the copy.
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
	// (verifySnapshotFiles), after its hash, so a check here would only repeat it.
	file, err := hashSnapshotFile(dir, copyPath)
	if err != nil {
		return rec, err
	}
	rec.Exact = snapshotExact{Method: snapshotMethodVacuumInto, File: &file}
	if err := storageSnapshotStep("binding copied"); err != nil {
		return rec, err
	}
	prefix, err := infraBindingIDPrefix()
	if err != nil {
		return rec, err
	}
	store, err := beads.OpenSQLiteStore(copyDir, beads.WithSQLiteStoreReadOnly(), beads.WithSQLiteStoreIDPrefix(prefix))
	if err != nil {
		return rec, fmt.Errorf("opening the copy: %w", err)
	}
	defer closeBeadStoreHandle(store) //nolint:errcheck // read-only
	rows, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return rec, fmt.Errorf("listing the copy: %w", err)
	}
	batch, _ := beads.As[beads.DependencyBatchLister](store)
	payloads, _ := beads.As[beads.DepMetadataReader](store)
	_, absent := beads.DepMetadataAbsent(store)
	entries, edges, verdict, err := readSnapshotEntries(rows, store, batch, payloads, absent)
	if err != nil {
		return rec, err
	}
	rec.Rows, rec.Edges, rec.Payloads = len(entries), edges, verdict
	rec.JSONL, err = writeSnapshotJSONL(dir, "binding.jsonl", rec.Name, entries)
	return rec, err
}

// snapshotScope snapshots the city's work store or one rig's: the exact copy
// its kind has, then its JSONL.
func snapshotScope(request storageOperatorRequest, scope snapshotScopeRef, id, dir string) (snapshotStoreRecord, error) {
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
	tagger, taggable := beads.As[beads.DoltTagger](under)
	files := strings.ReplaceAll(scope.name, ":", "-")

	method := snapshotExactMethod(provider, taggable)
	if method == snapshotMethodDoltTag {
		tag := "gc-storage-snapshot-" + id
		result, err := tagger.DoltTag(tag, "gc storage snapshot "+id)
		switch {
		case err == nil:
			rec.Exact = snapshotExact{Method: snapshotMethodDoltTag, Tag: tag, Commit: result.Commit}
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
		rec.Notes = append(rec.Notes, "provider "+provider+" has no exact snapshot; the JSONL is the snapshot")
	}

	rows, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return rec, fmt.Errorf("listing the store: %w", err)
	}
	batch, _ := beads.As[beads.DependencyBatchLister](under)
	payloads, _ := beads.As[beads.DepMetadataReader](under)
	_, absent := beads.DepMetadataAbsent(under)
	entries, edges, verdict, err := readSnapshotEntries(rows, store, batch, payloads, absent)
	if err != nil {
		return rec, err
	}
	rec.Rows, rec.Edges, rec.Payloads = len(entries), edges, verdict
	rec.JSONL, err = writeSnapshotJSONL(dir, files+".jsonl", rec.Name, entries)
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

// snapshotDepLister is the per-row edge read every store answers.
type snapshotDepLister interface {
	DepList(id, direction string) ([]beads.Dep, error)
}

// readSnapshotEntries reads every row's down edges, in batches when batch is
// non-nil, and each edge's payload when payloads can be asked. absent says
// the store's protocol carries none. Edges are recorded once, on the row they
// leave, so a whole store needs no Dependents.
func readSnapshotEntries(rows []beads.Bead, one snapshotDepLister, batch beads.DependencyBatchLister, payloads beads.DepMetadataReader, absent bool) ([]infraBackupEntry, int, string, error) {
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	down := make(map[string][]beads.Dep, len(rows))
	batched := batch != nil
	for start := 0; batched && start < len(rows); start += storageSnapshotBatch {
		ids := make([]string, 0, storageSnapshotBatch)
		for _, b := range rows[start:min(start+storageSnapshotBatch, len(rows))] {
			ids = append(ids, b.ID)
		}
		got, err := batch.DepListBatch(ids)
		if errors.Is(err, beads.ErrDepListBatchUnsupported) {
			batched = false
			break
		}
		if err != nil {
			return nil, 0, "", fmt.Errorf("reading edges: %w", err)
		}
		for _, id := range ids {
			down[id] = got[id]
		}
	}
	if !batched {
		for _, b := range rows {
			deps, err := one.DepList(b.ID, "down")
			if err != nil {
				return nil, 0, "", fmt.Errorf("reading the edges of %s: %w", b.ID, err)
			}
			down[b.ID] = deps
		}
	}
	verdict := snapshotPayloadsRead
	switch {
	case absent:
		verdict = snapshotPayloadsNone
	case payloads == nil:
		verdict = snapshotPayloadsUnanswered
	}
	entries := make([]infraBackupEntry, 0, len(rows))
	edges := 0
	for _, b := range rows {
		entry := infraBackupEntry{Bead: b, Class: coordclass.Classify(b).String()}
		for _, d := range down[b.ID] {
			edge := infraBackupEdge{IssueID: d.IssueID, DependsOnID: d.DependsOnID, Type: d.Type}
			if verdict == snapshotPayloadsRead {
				payload, err := infraReadEdgePayloadFrom(payloads, d.IssueID, d.DependsOnID)
				if err != nil {
					return nil, 0, "", err
				}
				edge.Payload, edge.Carried = payload.Payload, payload.Carried
			}
			entry.Deps = append(entry.Deps, edge)
		}
		sortInfraBackupEdges(entry.Deps)
		edges += len(entry.Deps)
		entries = append(entries, entry)
	}
	return entries, edges, verdict, nil
}

// writeSnapshotJSONL writes the universal JSONL, fsynced, and returns its
// record.
func writeSnapshotJSONL(dir, name, store string, entries []infraBackupEntry) (snapshotFile, error) {
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return snapshotFile{}, err
	}
	sum := sha256.New()
	counted := &countingWriter{target: io.MultiWriter(f, sum)}
	writer := bufio.NewWriter(counted)
	encoder := json.NewEncoder(writer)
	err = encoder.Encode(infraBackupHeader{Format: storageSnapshotJSONLFormat, Binding: store, Beads: len(entries), WrittenAt: time.Now().UTC()})
	for i := 0; err == nil && i < len(entries); i++ {
		err = encoder.Encode(entries[i])
	}
	if err == nil {
		err = writer.Flush()
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return snapshotFile{}, fmt.Errorf("writing %s: %w", path, err)
	}
	return snapshotFile{Path: name, Bytes: counted.n, SHA256: hex.EncodeToString(sum.Sum(nil))}, nil
}

// hashSnapshotFile records a file already in the snapshot.
func hashSnapshotFile(dir, path string) (snapshotFile, error) {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return snapshotFile{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return snapshotFile{}, err
	}
	defer f.Close() //nolint:errcheck // read-only
	sum := sha256.New()
	n, err := io.Copy(sum, f)
	if err != nil {
		return snapshotFile{}, fmt.Errorf("hashing %s: %w", path, err)
	}
	return snapshotFile{Path: rel, Bytes: n, SHA256: hex.EncodeToString(sum.Sum(nil))}, nil
}

// syncAndHashSnapshotFile fsyncs a file another program wrote into the
// snapshot, then records it.
func syncAndHashSnapshotFile(dir, path string) (snapshotFile, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return snapshotFile{}, err
	}
	err = f.Sync()
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return snapshotFile{}, err
	}
	return hashSnapshotFile(dir, path)
}

// copySnapshotFile copies src into the snapshot at rel, fsynced.
func copySnapshotFile(dir, src, rel string) (snapshotFile, error) {
	dst := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return snapshotFile{}, err
	}
	if err := copyFileSynced(src, dst); err != nil {
		return snapshotFile{}, err
	}
	return hashSnapshotFile(dir, dst)
}

func copyFileSynced(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // read-only
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("copying %s to %s: %w", src, dst, err)
	}
	return nil
}

// publishSnapshotManifest proves every file the manifest names, re-read from
// disk, and only then writes the manifest, atomically, and reads it back.
func publishSnapshotManifest(ctx context.Context, dir string, manifest snapshotManifest) error {
	if err := verifySnapshotFiles(ctx, dir, manifest); err != nil {
		return err
	}
	if err := storageSnapshotStep("before manifest"); err != nil {
		return err
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, storageSnapshotManifestName+".tmp-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	path := filepath.Join(dir, storageSnapshotManifestName)
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing the manifest: %w", err)
	}
	syncInfraDir(dir)
	syncInfraDir(filepath.Dir(dir))
	back, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading the manifest back: %w", err)
	}
	if !bytes.Equal(back, data) {
		return fmt.Errorf("the manifest %s does not read back as written", path)
	}
	return nil
}

// readSnapshotManifest reads a snapshot's manifest. A directory without one
// is an interrupted snapshot.
func readSnapshotManifest(dir string) (snapshotManifest, error) {
	var m snapshotManifest
	data, err := os.ReadFile(filepath.Join(dir, storageSnapshotManifestName))
	if errors.Is(err, os.ErrNotExist) {
		return m, fmt.Errorf("%s has no %s: it is an interrupted snapshot, not a snapshot", dir, storageSnapshotManifestName)
	}
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("reading %s: %w", filepath.Join(dir, storageSnapshotManifestName), err)
	}
	if m.Format != storageSnapshotManifestFormat {
		return m, fmt.Errorf("%s: manifest format %q, want %q", dir, m.Format, storageSnapshotManifestFormat)
	}
	if m.ID != filepath.Base(dir) {
		return m, fmt.Errorf("%s: manifest names snapshot %q", dir, m.ID)
	}
	return m, nil
}

// verifySnapshotFiles proves every file a manifest names: size and sha256,
// integrity_check on a SQLite copy, and the JSONL's header and counts.
func verifySnapshotFiles(ctx context.Context, dir string, m snapshotManifest) error {
	if len(m.Stores) == 0 {
		return errors.New("the manifest records no store")
	}
	for _, rec := range m.Stores {
		if rec.Exact.File != nil {
			if err := verifySnapshotFile(dir, *rec.Exact.File); err != nil {
				return fmt.Errorf("%s: %w", rec.Name, err)
			}
			if rec.Exact.Method == snapshotMethodVacuumInto {
				if err := beads.SQLiteIntegrityCheck(ctx, filepath.Join(dir, rec.Exact.File.Path)); err != nil {
					return fmt.Errorf("%s: %w", rec.Name, err)
				}
			}
		} else if rec.Exact.Method != snapshotMethodDoltTag && rec.Exact.Method != snapshotMethodNone {
			return fmt.Errorf("%s: %s records no file", rec.Name, rec.Exact.Method)
		}
		if err := verifySnapshotFile(dir, rec.JSONL); err != nil {
			return fmt.Errorf("%s: %w", rec.Name, err)
		}
		rows, edges, err := countSnapshotJSONL(filepath.Join(dir, rec.JSONL.Path))
		if err != nil {
			return fmt.Errorf("%s: %w", rec.Name, err)
		}
		if rows != rec.Rows || edges != rec.Edges {
			return fmt.Errorf("%s: the JSONL holds %d row(s) and %d edge(s), the manifest records %d and %d", rec.Name, rows, edges, rec.Rows, rec.Edges)
		}
	}
	return nil
}

func verifySnapshotFile(dir string, want snapshotFile) error {
	if want.Path == "" || filepath.IsAbs(want.Path) || strings.HasPrefix(filepath.Clean(want.Path), "..") {
		return fmt.Errorf("the manifest names %q, which is not a file inside the snapshot", want.Path)
	}
	got, err := hashSnapshotFile(dir, filepath.Join(dir, want.Path))
	if err != nil {
		return err
	}
	if got.Bytes != want.Bytes || got.SHA256 != want.SHA256 {
		return fmt.Errorf("%s is %d bytes, sha256 %s; the manifest records %d bytes, sha256 %s", want.Path, got.Bytes, got.SHA256, want.Bytes, want.SHA256)
	}
	return nil
}

// countSnapshotJSONL counts the rows and edges of a universal JSONL, refusing
// a header of another format or one whose count disagrees with the body.
func countSnapshotJSONL(path string) (rows, edges int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close() //nolint:errcheck // read-only
	decoder := json.NewDecoder(bufio.NewReader(f))
	var header infraBackupHeader
	if err := decoder.Decode(&header); err != nil {
		return 0, 0, fmt.Errorf("%s: header: %w", path, err)
	}
	if header.Format != storageSnapshotJSONLFormat {
		return 0, 0, fmt.Errorf("%s: format %q, want %q", path, header.Format, storageSnapshotJSONLFormat)
	}
	for decoder.More() {
		var entry infraBackupEntry
		if err := decoder.Decode(&entry); err != nil {
			return 0, 0, fmt.Errorf("%s: row %d: %w", path, rows+1, err)
		}
		rows++
		edges += len(entry.Deps) + len(entry.Dependents)
	}
	if rows != header.Beads {
		return 0, 0, fmt.Errorf("%s: header records %d row(s), file holds %d", path, header.Beads, rows)
	}
	return rows, edges, nil
}

// doStorageSnapshotVerify re-reads one snapshot and proves it. It writes
// nothing and takes no lock.
func doStorageSnapshotVerify(ctx context.Context, to, id, logPrefix string, stdout, stderr io.Writer) int {
	if !storageSnapshotIDPattern.MatchString(id) {
		fmt.Fprintf(stderr, "%s: %q is not a snapshot id\n", logPrefix, id) //nolint:errcheck // best-effort stderr
		return 1
	}
	dir := filepath.Join(to, id)
	m, err := readSnapshotManifest(dir)
	if err == nil {
		err = verifySnapshotFiles(ctx, dir, m)
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: snapshot %s does NOT verify: %v\n", logPrefix, id, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "Snapshot %s verifies: %d store(s), written %s by gc %s.\n", id, len(m.Stores), m.CreatedAt.Format(time.RFC3339), m.GCVersion) //nolint:errcheck // best-effort stdout
	for _, rec := range m.Stores {
		if rec.Exact.Method == snapshotMethodDoltTag {
			fmt.Fprintf(stdout, "  %s: the Dolt tag %s lives in the store and is not re-checked here\n", rec.Name, rec.Exact.Tag) //nolint:errcheck // best-effort stdout
		}
	}
	return 0
}

// newSnapshotID is now's id, suffixed when a snapshot of that second exists.
func newSnapshotID(to string, now time.Time) (string, error) {
	base := now.UTC().Format("20060102T150405Z")
	id := base
	for n := 2; ; n++ {
		if _, err := os.Lstat(filepath.Join(to, id)); errors.Is(err, os.ErrNotExist) {
			return id, nil
		} else if err != nil {
			return "", err
		}
		id = fmt.Sprintf("%s-%d", base, n)
	}
}

// lockSnapshotDir takes the directory's snapshot flock, refusing when another
// snapshot holds it.
func lockSnapshotDir(to string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(to, storageSnapshotLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another snapshot is writing into %s; run this again when it is done", to)
	}
	return f, nil
}

// snapshotDiskPreflight refuses a binding snapshot that would not fit: the
// copy is at most the database plus its WAL, and the JSONL is estimated at
// that again.
func snapshotDiskPreflight(to, database string) error {
	var need int64 = storageSnapshotDiskMargin
	for _, p := range []string{database, database + "-wal"} {
		if info, err := os.Stat(p); err == nil {
			need += 2 * info.Size()
		}
	}
	free, err := doltContainerFreeBytesFunc(to)
	if err != nil {
		return nil // fail open, as the managed-Dolt preflight does
	}
	if free < need {
		return fmt.Errorf("%s has %d bytes free and a binding snapshot needs about %d (twice the database and its WAL, plus %d); free space or pass --to elsewhere. Nothing was written", to, free, need, int64(storageSnapshotDiskMargin))
	}
	return nil
}

// applySnapshotRetention keeps the newest keep complete snapshots in to and
// removes every older one, and every interrupted one (no manifest). Only
// directories named as snapshot ids are touched. The caller holds the
// directory's snapshot lock, so no interrupted one is still being written.
func applySnapshotRetention(to string, keep int) ([]string, error) {
	entries, err := os.ReadDir(to)
	if err != nil {
		return nil, err
	}
	var complete, interrupted []string
	for _, e := range entries {
		if !e.IsDir() || !storageSnapshotIDPattern.MatchString(e.Name()) {
			continue
		}
		if _, err := os.Stat(filepath.Join(to, e.Name(), storageSnapshotManifestName)); err == nil {
			complete = append(complete, e.Name())
		} else {
			interrupted = append(interrupted, e.Name())
		}
	}
	sort.Slice(complete, func(i, j int) bool { return snapshotIDLess(complete[j], complete[i]) })
	doomed := interrupted
	if len(complete) > keep {
		doomed = append(doomed, complete[keep:]...)
	}
	var removed []string
	var errs []error
	for _, name := range doomed {
		if err := os.RemoveAll(filepath.Join(to, name)); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, name)
	}
	return removed, errors.Join(errs...)
}

// snapshotIDLess orders ids by time, then by suffix.
func snapshotIDLess(a, b string) bool {
	baseA, sufA, _ := strings.Cut(a, "-")
	baseB, sufB, _ := strings.Cut(b, "-")
	if baseA != baseB {
		return baseA < baseB
	}
	if len(sufA) != len(sufB) {
		return len(sufA) < len(sufB)
	}
	return sufA < sufB
}

// doStorageSnapshotRestore puts a snapshot's binding back.
func doStorageSnapshotRestore(ctx context.Context, request storageOperatorRequest, to, id, logPrefix string, stdout, stderr io.Writer) int {
	target, bound, err := resolveInfraBindingTarget(request.CityPath, request.Cfg)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !bound {
		fmt.Fprintf(stderr, "%s: this city's [storage.classes] assign its infrastructure classes to no binding, so there is no binding to restore\n", logPrefix) //nolint:errcheck // best-effort stderr
		return 1
	}
	lock, err := acquireStorageVerbExclusion(request, storageVerbScope{})
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer lock.Close() //nolint:errcheck // released last
	if pid := infraMigrationForeignControllerPID(request.CityPath); pid != 0 {
		fmt.Fprintf(stderr, "%s: controller PID %d is live on this city and is serving the binding. Stop it (%s) and run this again\n", logPrefix, pid, storageStopCommand) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !request.FleetStopped {
		fmt.Fprintf(stderr, "%s: pass --%s to attest that %s. A process this command cannot see that still has the binding open would keep writing to the database this replaces\n", logPrefix, storageFleetStoppedFlag, storageFleetStoppedAttestation) //nolint:errcheck // best-effort stderr
		return 1
	}
	guard, err := acquireStorageMigrationGuard(ctx, request.CityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer guard.Release() //nolint:errcheck // released before the lock
	recordStorageAttestation(request.CityPath, logPrefix, request.Force.acceptedHazards(), stderr)

	if !storageSnapshotIDPattern.MatchString(id) {
		fmt.Fprintf(stderr, "%s: %q is not a snapshot id\n", logPrefix, id) //nolint:errcheck // best-effort stderr
		return 1
	}
	dir := filepath.Join(to, id)
	m, err := readSnapshotManifest(dir)
	if err == nil {
		err = verifySnapshotFiles(ctx, dir, m)
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: snapshot %s does not verify, so nothing was restored: %v\n", logPrefix, id, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	var file *snapshotFile
	for _, rec := range m.Stores {
		if rec.Name == "binding" && rec.Exact.Method == snapshotMethodVacuumInto {
			file = rec.Exact.File
		}
	}
	if file == nil {
		fmt.Fprintf(stderr, "%s: snapshot %s holds no binding copy\n", logPrefix, id) //nolint:errcheck // best-effort stderr
		return 1
	}
	aside, err := installSnapshotBinding(ctx, filepath.Join(dir, file.Path), file.SHA256, target.Database, time.Now().UTC(), logPrefix, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v. Run this command again to finish\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "Restored the binding %s from snapshot %s.\n", target.Database, id) //nolint:errcheck // best-effort stdout
	if len(aside) > 0 {
		fmt.Fprintf(stdout, "The database it replaced is kept as %s.\n", strings.Join(aside, ", ")) //nolint:errcheck // best-effort stdout
	}
	fmt.Fprintf(stdout, "Check the city with `%s` before `gc start`.\n", storageStatusInstruction()) //nolint:errcheck // best-effort stdout
	return 0
}

// installSnapshotBinding copies src beside database, proves the copy (sha256
// and integrity_check), moves the live database and then its -wal and -shm
// aside, and renames the copy into place.
//
// The live database's WAL is checkpointed into it first, then the database
// moves before its -wal and -shm: a run killed after that leaves no database
// at the binding path, which boot reads as a stale binding and refuses,
// rather than a database separated from its WAL, which it would serve, and
// the -wal it leaves is empty. Nothing is deleted.
func installSnapshotBinding(ctx context.Context, src, wantSHA, database string, now time.Time, logPrefix string, stderr io.Writer) ([]string, error) {
	dir := filepath.Dir(database)
	tmp, err := os.CreateTemp(dir, filepath.Base(database)+".restore-*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(tmpPath)
	fail := func(err error) ([]string, error) {
		_ = os.Remove(tmpPath)
		return nil, err
	}
	if err := copyFileSynced(src, tmpPath); err != nil {
		return fail(err)
	}
	got, err := hashSnapshotFile(dir, tmpPath)
	if err != nil {
		return fail(err)
	}
	if got.SHA256 != wantSHA {
		return fail(fmt.Errorf("the copy of %s has sha256 %s, the snapshot records %s", src, got.SHA256, wantSHA))
	}
	if err := beads.SQLiteIntegrityCheck(ctx, tmpPath); err != nil {
		return fail(err)
	}
	if err := storageSnapshotStep("restore copied"); err != nil {
		return fail(err)
	}
	// Fold the WAL into the database first, so a run killed between the
	// renames below leaves at most an empty -wal behind. A database too
	// damaged to checkpoint (the reason for a restore, often) moves as it is.
	if _, err := os.Lstat(database); err == nil {
		if err := beads.CheckpointSQLiteWAL(ctx, database); err != nil {
			fmt.Fprintf(stderr, "%s: %v; moving the database and its WAL aside as they are\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		}
	}
	suffix := ".pre-restore-" + now.Format("20060102T150405Z")
	var aside []string
	for _, p := range []string{database, database + "-wal", database + "-shm"} {
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := os.Rename(p, p+suffix); err != nil {
			return aside, fmt.Errorf("moving %s aside: %w", p, err)
		}
		aside = append(aside, p+suffix)
		if err := storageSnapshotStep("restore moved " + filepath.Base(p)); err != nil {
			return aside, err
		}
	}
	if err := os.Rename(tmpPath, database); err != nil {
		return aside, fmt.Errorf("installing %s: %w", database, err)
	}
	syncInfraDir(dir)
	return aside, nil
}
