package main

// `gc storage snapshot restore <id> --binding --fleet-stopped`: put a
// snapshot's binding database back (S5 §5.3).
//
// A binding is its database AND the records beside it, which say what the
// database holds and where it was carried from (the convergence marker, the
// copy manifest, each rig's import records, the city's cleared notes). A
// restore puts back only the database, so it refuses unless those records are
// compatible with the ones the snapshot recorded (snapshotRecordsCompatible):
// restoring a database from before a rig import, under the import's records,
// would leave the rig's rows nowhere. Records moved aside by hand do not get
// past it either: rows the live binding holds stamped as imported from a rig,
// which the snapshot lacks, refuse too (snapshotRigImportsLost). It refuses a
// snapshot of another work
// store too, judged as boot judges a marker (workStoreIdentityMismatch).
//
// Under the controller lock, the migration guard and the snapshot directory's
// lock, it then:
//
//  1. folds the live database's WAL into it, refusing when another connection
//     holds the database (a busy checkpoint, or a -wal or -shm that outlives
//     this command's connection);
//  2. writes infra.restoring beside the marker. From here until step 7 the
//     binding is stale whatever its database holds (readInfraConvergenceState),
//     so every gc that boots refuses it rather than write to a database about
//     to be replaced, and names this command;
//  3. copies the snapshot's database beside the live one, proves the copy's
//     sha256, and switches it to WAL (VACUUM INTO writes rollback-journal
//     mode, and a store opened over an existing database keeps the mode);
//  4. repeats step 1, so a process that opened the binding during the copy,
//     past its boot gate, is refused before anything moves;
//  5. raises the id floor to the latest, in allocation order, of the live
//     floor, the snapshot's and the live database's high-water, so no id the
//     replaced database held is minted again, a wrapped range included;
//  6. moves the live database, then its -wal, -shm and -journal, aside as
//     *.pre-restore-<ts> (numbered when a restore of the same second used
//     that name), and renames the copy into place;
//  7. removes infra.restoring.
//
// A refusal before step 6 changes nothing (the copy and the record go). Once
// step 6 has begun, the record holds the binding stale until the same command
// finishes it. Nothing is deleted.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beads"
)

// infraRestoreRecordName is the record a restore writes beside the marker
// while the binding's database is being replaced.
const infraRestoreRecordName = "infra.restoring"

// RestoreRecordPath is where a restore records that it is under way.
func (t infraBindingTarget) RestoreRecordPath() string {
	return filepath.Join(t.Root, infraRestoreRecordName)
}

// infraRestoreRecord is the content of infra.restoring.
type infraRestoreRecord struct {
	Snapshot  string    `json:"snapshot"`
	To        string    `json:"to"`
	Database  string    `json:"database"`
	StartedAt time.Time `json:"started_at"`
	// AsideSuffix is what the files it moves aside are renamed with.
	AsideSuffix string `json:"aside_suffix"`
	// Command finishes the restore.
	Command string `json:"command"`
}

// Test seams for the restore's failure paths, and its clock.
var (
	snapshotRestoreRename     = os.Rename
	snapshotRestoreCreateTemp = os.CreateTemp
	snapshotRestoreNow        = time.Now
)

// readInfraRestoreRecord reads infra.restoring. present=false with no error
// means no restore is under way.
func readInfraRestoreRecord(target infraBindingTarget) (infraRestoreRecord, bool, error) {
	var rec infraRestoreRecord
	data, err := os.ReadFile(target.RestoreRecordPath())
	if errors.Is(err, os.ErrNotExist) {
		return rec, false, nil
	}
	if err != nil {
		return rec, true, fmt.Errorf("reading the restore record %s: %w", target.RestoreRecordPath(), err)
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, true, fmt.Errorf("reading the restore record %s: %w", target.RestoreRecordPath(), err)
	}
	return rec, true, nil
}

// writeInfraRestoreRecord writes infra.restoring atomically. An earlier
// run's record is superseded: this run is the one the binding now waits on.
func writeInfraRestoreRecord(target infraBindingTarget, rec infraRestoreRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(target.Root, infraRestoreRecordName+".tmp-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(append(data, '\n'))
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = infraMigrationRename(tmp.Name(), target.RestoreRecordPath())
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("recording the restore at %s: %w", target.RestoreRecordPath(), err)
	}
	syncInfraDir(target.Root)
	return nil
}

// snapshotRestoreInstruction is the command that restores snapshot id from to.
func snapshotRestoreInstruction(id, to string) string {
	return "gc storage snapshot restore " + id + " --binding --" + storageFleetStoppedFlag + " --to " + to
}

// infraRestoreStaleCause says what an unfinished restore has done, for a
// stale binding's message; ok=false when none is recorded. It tells the three
// crash points apart: nothing moved yet, the live database moved aside, and
// the snapshot's database installed with only the record left.
func infraRestoreStaleCause(target infraBindingTarget) (string, bool) {
	rec, present, err := readInfraRestoreRecord(target)
	switch {
	case !present:
		return "", false
	case err != nil:
		return fmt.Sprintf("a restore of %s is under way and its record cannot be read (%v)", target.Database, err), true
	}
	began := fmt.Sprintf("a restore of snapshot %s into %s began at %s and has not finished", rec.Snapshot, target.Database, rec.StartedAt.Format(time.RFC3339))
	var aside []string
	if rec.AsideSuffix != "" {
		aside, _ = filepath.Glob(target.Database + "*" + rec.AsideSuffix)
	}
	dbPresent, _ := infraPathExists(target.Database)
	switch {
	case dbPresent && len(aside) == 0:
		return began + fmt.Sprintf("; nothing has moved, so the database in place is the one it would replace. Re-run the restore, or remove %s to keep that database", target.RestoreRecordPath()), true
	case dbPresent:
		return began + fmt.Sprintf("; the snapshot's database is installed and the one it replaced is kept as %s, but the restore stopped before it removed its record. Re-run the restore to finish", strings.Join(aside, ", ")), true
	case len(aside) > 0:
		return began + fmt.Sprintf("; the database it replaces is moved aside as %s, and the snapshot's is not yet in place. Re-run the restore to finish; to abandon it instead, move those files back to %s and remove %s", strings.Join(aside, ", "), target.Database, target.RestoreRecordPath()), true
	}
	return began + "; the database it replaces is gone and the snapshot's is not in place. Re-run the restore to finish", true
}

// infraStaleBindingRemedy is the command that repairs a stale binding: the
// restore its record names when one is under way, else the re-copy, from the
// retained-source backup when the work store's copies were cleared.
func infraStaleBindingRemedy(target infraBindingTarget) string {
	if rec, present, err := readInfraRestoreRecord(target); present {
		if err == nil && rec.Command != "" {
			return rec.Command
		}
		return "gc storage snapshot restore <id> --binding --" + storageFleetStoppedFlag + " (re-run the interrupted restore; " + target.RestoreRecordPath() + " names it)"
	}
	if present, err := infraPathExists(target.RetainedBackupPath()); err == nil && present {
		return storageRecopyFromBackupInstruction()
	}
	return storageClearInstruction()
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

It refuses a snapshot of another work store, and one taken before the
binding's records changed (a migration, a recovery or a rig import since; a
rig marked clean since, or a marker that only gained its identity line, is
compatible): the database is only half of a binding, and the records beside
it are not restored.

The snapshot is proven against its manifest first, then installed under the
controller lock with the city stopped, in WAL mode, with the id floor raised
past every id the replaced database held. The database it replaces, with its
-wal, -shm and -journal, is kept beside it as *.pre-restore-<ts>.

From the moment it starts copying until it finishes, the binding counts as
stale (infra.restoring beside it): every gc that boots refuses it and names
this command. A refusal before the live database moves changes nothing. A
restore interrupted after that holds the city stale until you run it again.

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

// doStorageSnapshotRestore puts a snapshot's binding back.
func doStorageSnapshotRestore(ctx context.Context, request storageOperatorRequest, to, id, logPrefix string, stdout, stderr io.Writer) int {
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
	if !bound {
		return fail(errors.New("this city's [storage.classes] assign its infrastructure classes to no binding, so there is no binding to restore"))
	}
	lock, err := acquireStorageVerbExclusion(request, storageVerbScope{})
	if err != nil {
		return fail(err)
	}
	defer lock.Close() //nolint:errcheck // released last
	if pid := infraMigrationForeignControllerPID(request.CityPath); pid != 0 {
		return fail(fmt.Errorf("controller PID %d is live on this city and is serving the binding. Stop it (%s) and run this again", pid, storageStopCommand))
	}
	if !request.FleetStopped {
		return fail(fmt.Errorf("pass --%s to attest that %s. A process this command cannot see that still has the binding open would keep writing to the database this replaces", storageFleetStoppedFlag, storageFleetStoppedAttestation))
	}
	guard, err := acquireStorageMigrationGuard(ctx, request.CityPath)
	if err != nil {
		return fail(err)
	}
	defer guard.Release() //nolint:errcheck // released before the lock
	to, err = filepath.Abs(to)
	if err != nil {
		return fail(err)
	}
	dirLock, err := lockSnapshotDir(to)
	if err != nil {
		return fail(err)
	}
	defer dirLock.Close() //nolint:errcheck // releases the flock
	recordStorageAttestation(request.CityPath, logPrefix, request.Force.acceptedHazards(), stderr)

	dir := filepath.Join(to, id)
	m, err := readSnapshotManifest(dir)
	if err == nil {
		err = verifySnapshotFiles(ctx, dir, m)
	}
	if err != nil {
		return fail(fmt.Errorf("snapshot %s does not verify, so nothing was restored: %w", id, err))
	}
	file, err := snapshotRestorable(m, dir, request, target)
	if err != nil {
		return fail(fmt.Errorf("snapshot %s cannot be restored here, so nothing was restored: %w", id, err))
	}
	rec := infraRestoreRecord{Snapshot: id, To: to, Database: target.Database, StartedAt: snapshotRestoreNow().UTC(), Command: snapshotRestoreInstruction(id, to)}
	aside, err := installSnapshotBinding(ctx, target, filepath.Join(dir, file.Path), file.SHA256, m.Binding.SequenceFloor, rec)
	if err != nil {
		if len(aside) == 0 {
			return fail(fmt.Errorf("%w. Nothing was moved; the binding is as it was", err))
		}
		return fail(fmt.Errorf("%w. The binding is held stale until this restore finishes: run this command again", err))
	}
	fmt.Fprintf(stdout, "Restored the binding %s from snapshot %s.\n", target.Database, id) //nolint:errcheck // best-effort stdout
	if len(aside) > 0 {
		fmt.Fprintf(stdout, "The database it replaced is kept as %s.\n", strings.Join(aside, ", ")) //nolint:errcheck // best-effort stdout
	}
	fmt.Fprintf(stdout, "Check the city with `%s` before `gc start`.\n", storageStatusInstruction()) //nolint:errcheck // best-effort stdout
	return 0
}

// snapshotRestorable is the binding copy of m, refused when m is of another
// work store, or when the binding's records are no longer compatible with
// the ones m recorded.
func snapshotRestorable(m snapshotManifest, snapshotDir string, request storageOperatorRequest, target infraBindingTarget) (snapshotFile, error) {
	var file *snapshotFile
	for _, rec := range m.Stores {
		if rec.Name == "binding" && rec.Exact.Method == snapshotMethodVacuumInto {
			file = rec.Exact.File
		}
	}
	if file == nil || m.Binding == nil {
		return snapshotFile{}, errors.New("it holds no binding copy")
	}
	recorded, err := parseWorkStoreIdentity(m.Identity)
	if err != nil {
		return snapshotFile{}, fmt.Errorf("reading its work-store identity: %w", err)
	}
	current, err := currentWorkStoreIdentity(request.CityPath)
	if err != nil {
		return snapshotFile{}, err
	}
	if mismatch := workStoreIdentityMismatch(recorded, current); mismatch != "" {
		return snapshotFile{}, fmt.Errorf("it was taken against another work store: %s. The binding belongs to the work store it was carried from", strings.Replace(mismatch, "the binding was converged from", "the snapshot's binding was converged from", 1))
	}
	records, err := readSnapshotBindingRecords(request.CityPath, target)
	if err != nil {
		return snapshotFile{}, err
	}
	if diff := snapshotRecordsCompatible(m.Binding.Records, records.Records); len(diff) > 0 {
		return snapshotFile{}, fmt.Errorf("the binding's records have changed since it was taken: %s. A restore puts back only the database, so it would serve rows those records no longer describe (a rig import since the snapshot would leave that rig's rows in neither store). Restore a snapshot taken since, or roll the change back first (the runbook's \"Restoring the binding\" names the steps)", strings.Join(diff, "; "))
	}
	if lost, err := snapshotRigImportsLost(target, filepath.Join(snapshotDir, file.Path), snapshotRigNames(request, m.Binding.Records, records.Records)); err != nil {
		return snapshotFile{}, err
	} else if len(lost) > 0 {
		return snapshotFile{}, fmt.Errorf("the binding holds rows imported from rigs since the snapshot, which the snapshot lacks: %s. Restoring it would leave those rows in neither store; restore each rig's own store from before its import first (the runbook's \"Rolling back past a rig import\"), then run this again", strings.Join(lost, "; "))
	}
	return *file, nil
}

// snapshotRigNames is every rig a restore must check for imported rows: the
// configured rigs and every rig a record names, then or now (a rig's records
// moved aside still leave its rows stamped in the binding).
func snapshotRigNames(request storageOperatorRequest, recorded, current []snapshotRecord) []string {
	seen := map[string]bool{}
	for _, rig := range request.Cfg.Rigs {
		seen[rig.Name] = true
	}
	for _, records := range [][]snapshotRecord{recorded, current} {
		for _, r := range records {
			name := strings.TrimPrefix(r.Name, "binding/infra.rig.")
			if name == r.Name {
				continue
			}
			if dot := strings.LastIndex(name, "."); dot > 0 {
				seen[name[:dot]] = true
			}
		}
	}
	return sortedMapKeys(seen)
}

// snapshotRigImportsLost names, per rig, rows the live binding holds stamped
// as imported from it (gc.infra_migrated_from=rig:<R>) that the snapshot's
// copy lacks: rows a restore would leave in neither store, because the import
// cleared them from the rig. It is the content check behind the record check,
// for records moved aside by hand. A live database that is gone (a restore
// killed mid-move) cannot be read and is not checked; the run that moved it
// was checked.
func snapshotRigImportsLost(target infraBindingTarget, copyPath string, rigs []string) ([]string, error) {
	if present, err := infraPathExists(target.Database); err != nil || !present || len(rigs) == 0 {
		return nil, err
	}
	ids, err := beads.SQLiteBeadIDs(context.Background(), copyPath)
	if err != nil {
		return nil, err
	}
	held := make(map[string]bool, len(ids))
	for _, id := range ids {
		held[id] = true
	}
	live, err := openInfraBindingReadOnly(target)
	if err != nil {
		return nil, fmt.Errorf("opening the live binding to check its rig imports: %w", err)
	}
	defer closeBeadStoreHandle(live) //nolint:errcheck // read-only
	var out []string
	for _, rig := range rigs {
		rows, err := live.List(beads.ListQuery{Metadata: map[string]string{infraMigrationStampKey: rigImportStamp(rig)}, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
		if err != nil {
			return nil, fmt.Errorf("listing the live binding's rows imported from rig %s: %w", rig, err)
		}
		var missing []string
		for _, b := range rows {
			if !held[b.ID] {
				missing = append(missing, b.ID)
			}
		}
		if len(missing) > 0 {
			out = append(out, fmt.Sprintf("rig %s: %d row(s), e.g. %s", rig, len(missing), infraStrandedIDList(missing)))
		}
	}
	return out, nil
}

// snapshotRecordsCompatible is diffSnapshotRecords less the changes a restore
// of the snapshot's database stays true under: a rig marked clean since (it
// moved nothing into the binding). The marker's identity line is already out
// of its hash (snapshotRecordSum).
func snapshotRecordsCompatible(recorded, current []snapshotRecord) []string {
	was := make(map[string]bool, len(recorded))
	for _, r := range recorded {
		was[r.Name] = true
	}
	var kept []snapshotRecord
	for _, r := range current {
		if !was[r.Name] && strings.HasSuffix(r.Name, ".clean") {
			continue
		}
		kept = append(kept, r)
	}
	return diffSnapshotRecords(recorded, kept)
}

// installSnapshotBinding is restore steps 1-7 (the file comment) for target,
// from the snapshot copy src recorded with wantSHA and the id floor floor.
// aside is what it moved; empty means it changed nothing.
func installSnapshotBinding(ctx context.Context, target infraBindingTarget, src, wantSHA string, floor int64, rec infraRestoreRecord) ([]string, error) {
	database, dir := target.Database, filepath.Dir(target.Database)
	if err := quiesceSnapshotRestoreTarget(ctx, database); err != nil {
		return nil, err
	}
	rec.AsideSuffix = snapshotAsideSuffix(database, rec.StartedAt)
	if err := writeInfraRestoreRecord(target, rec); err != nil {
		return nil, err
	}
	// Temp copies a killed restore left.
	if stale, err := filepath.Glob(database + ".restore-*"); err == nil {
		for _, p := range stale {
			_ = os.Remove(p)
		}
	}
	tmp, err := snapshotRestoreCreateTemp(dir, filepath.Base(database)+".restore-*")
	if err != nil {
		return nil, errors.Join(err, os.Remove(target.RestoreRecordPath()))
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(tmpPath)
	// Until the live database moves, a failure leaves it as it was: the copy
	// and the restore record go.
	failCopy := func(err error) ([]string, error) {
		for _, p := range []string{tmpPath, tmpPath + "-wal", tmpPath + "-shm", target.RestoreRecordPath()} {
			_ = os.Remove(p)
		}
		return nil, err
	}
	if err := copyFileSynced(src, tmpPath); err != nil {
		return failCopy(err)
	}
	got, err := hashSnapshotFile(dir, tmpPath)
	if err != nil {
		return failCopy(err)
	}
	if got.SHA256 != wantSHA {
		return failCopy(fmt.Errorf("the copy of %s has sha256 %s, the snapshot records %s", src, got.SHA256, wantSHA))
	}
	if err := beads.SetSQLiteJournalModeWAL(ctx, tmpPath); err != nil {
		return failCopy(err)
	}
	if err := storageSnapshotStep("restore copied"); err != nil {
		return failCopy(err)
	}
	// A process that opened the binding during the copy, past its boot gate,
	// is refused here, before anything moves.
	if err := quiesceSnapshotRestoreTarget(ctx, database); err != nil {
		return failCopy(err)
	}
	prefix, err := infraBindingIDPrefix()
	if err != nil {
		return failCopy(err)
	}
	if _, err := beads.RaiseSQLiteSequenceFloorPast(target.Dir, prefix, []int64{floor}, database); err != nil {
		return failCopy(fmt.Errorf("raising the binding's id floor: %w", err))
	}
	suffix := rec.AsideSuffix
	var aside []string
	for _, p := range []string{database, database + "-wal", database + "-shm", database + "-journal"} {
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := snapshotRestoreRename(p, p+suffix); err != nil {
			if len(aside) == 0 {
				return failCopy(fmt.Errorf("moving %s aside: %w", p, err))
			}
			return aside, fmt.Errorf("moving %s aside: %w", p, err)
		}
		aside = append(aside, p+suffix)
		if err := storageSnapshotStep("restore moved " + filepath.Base(p)); err != nil {
			return aside, err
		}
	}
	if err := os.Rename(tmpPath, database); err != nil {
		return append(aside, tmpPath), fmt.Errorf("installing %s: %w", database, err)
	}
	syncInfraDir(dir)
	if err := os.Remove(target.RestoreRecordPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return append(aside, target.RestoreRecordPath()), fmt.Errorf("removing the restore record %s: %w", target.RestoreRecordPath(), err)
	}
	syncInfraDir(target.Root)
	return aside, nil
}

// snapshotAsideSuffix is the suffix a restore renames the files it replaces
// with: .pre-restore-<ts>, numbered when an earlier restore of the same second
// already used it, so no restore's kept files are overwritten.
func snapshotAsideSuffix(database string, at time.Time) string {
	base := ".pre-restore-" + at.UTC().Format("20060102T150405Z")
	suffix := base
	for n := 2; ; n++ {
		taken := false
		for _, p := range []string{database, database + "-wal", database + "-shm", database + "-journal"} {
			if _, err := os.Lstat(p + suffix); err == nil {
				taken = true
			}
		}
		if !taken {
			return suffix
		}
		suffix = fmt.Sprintf("%s-%d", base, n)
	}
}

// quiesceSnapshotRestoreTarget folds the live database's WAL into it, so a
// run killed between the moves leaves at most an empty -wal, and refuses when
// another connection holds the database: its checkpoint is busy, or a -wal or
// -shm outlives this command's own connection, whose close removes both when
// it is the last. A database too damaged to checkpoint (the reason for a
// restore, often) moves as it is.
func quiesceSnapshotRestoreTarget(ctx context.Context, database string) error {
	if _, err := os.Lstat(database); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	err := beads.CheckpointSQLiteWAL(ctx, database)
	if errors.Is(err, beads.ErrSQLiteCheckpointBusy) {
		return fmt.Errorf("another process has the binding %s open (its WAL could not be folded in); stop it and run this again", database)
	}
	if err != nil {
		return nil
	}
	for _, p := range []string{database + "-wal", database + "-shm"} {
		if _, err := os.Lstat(p); err == nil {
			return fmt.Errorf("another process has the binding %s open (%s outlives this command's connection); stop it and run this again", database, p)
		}
	}
	return nil
}
