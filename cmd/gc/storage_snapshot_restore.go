package main

// `gc storage snapshot restore <id> --binding --fleet-stopped`: put a
// snapshot's binding database back (S5 §5.3).
//
// A binding is its database AND the records beside it, which say what the
// database holds and where it was carried from (the convergence marker, the
// copy manifest, each rig's import records, the city's cleared notes). A
// restore puts back only the database, so it refuses unless those records are
// the ones the snapshot recorded: restoring a database from before a rig
// import, under the import's records, would leave the rig's rows nowhere. It
// refuses a snapshot of another city or another work store too.
//
// Under the controller lock, the migration guard and the snapshot directory's
// lock, it then:
//
//  1. folds the live database's WAL into it, refusing when another connection
//     holds the database (a busy checkpoint, or a -wal or -shm that outlives
//     this command's connection);
//  2. writes infra.restoring beside the marker, so a boot that finds the
//     binding stale names this command rather than a re-copy;
//  3. copies the snapshot's database beside the live one, proves the copy's
//     sha256, and switches it to WAL (VACUUM INTO writes rollback-journal
//     mode, and a store opened over an existing database keeps the mode);
//  4. raises the id floor to the most of the live floor, the snapshot's and
//     the live database's high-water, so no id the replaced database held is
//     minted again;
//  5. moves the live database, then its -wal, -shm and -journal, aside as
//     *.pre-restore-<ts> (a run killed after the first move leaves no
//     database at the binding path, which boot refuses), and renames the copy
//     into place;
//  6. removes infra.restoring.
//
// Nothing is deleted. Every step is safe to run again.

import (
	"context"
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

// snapshotRestoreInstruction is the command that restores snapshot id from to.
func snapshotRestoreInstruction(id, to string) string {
	return "gc storage snapshot restore " + id + " --binding --" + storageFleetStoppedFlag + " --to " + to
}

// infraStaleBindingRemedy is the command that repairs a stale binding (a
// marker whose database is gone): the interrupted restore when one is
// recorded, else the re-copy, from the retained-source backup when the work
// store's copies were cleared.
func infraStaleBindingRemedy(target infraBindingTarget) string {
	if data, err := os.ReadFile(target.RestoreRecordPath()); err == nil {
		line := strings.TrimSpace(string(data))
		if i := strings.LastIndex(line, "`"); i > 0 {
			if j := strings.LastIndex(line[:i], "`"); j >= 0 {
				return line[j+1 : i]
			}
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

It refuses a snapshot of another city or another work store, and one taken
before the binding's records changed (a migration, a recovery or a rig import
since): the database is only half of a binding, and the records beside it are
not restored.

The snapshot is proven against its manifest first, then installed under the
controller lock with the city stopped, in WAL mode, with the id floor raised
past every id the replaced database held. The database it replaces, with its
-wal, -shm and -journal, is kept beside it as *.pre-restore-<ts>. A restore
interrupted before the new database is in place leaves no database at the
binding path, which every boot refuses, naming this command; run it again.

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
	file, err := snapshotRestorable(m, request, target)
	if err != nil {
		return fail(fmt.Errorf("snapshot %s cannot be restored here, so nothing was restored: %w", id, err))
	}
	aside, err := installSnapshotBinding(ctx, target, filepath.Join(dir, file.Path), file.SHA256, m.Binding.SequenceFloor, snapshotRestoreInstruction(id, to), time.Now().UTC())
	if err != nil {
		return fail(fmt.Errorf("%w. Run this command again to finish", err))
	}
	fmt.Fprintf(stdout, "Restored the binding %s from snapshot %s.\n", target.Database, id) //nolint:errcheck // best-effort stdout
	if len(aside) > 0 {
		fmt.Fprintf(stdout, "The database it replaced is kept as %s.\n", strings.Join(aside, ", ")) //nolint:errcheck // best-effort stdout
	}
	fmt.Fprintf(stdout, "Check the city with `%s` before `gc start`.\n", storageStatusInstruction()) //nolint:errcheck // best-effort stdout
	return 0
}

// snapshotRestorable is the binding copy of m, refused when m is of another
// city or another work store, or when the binding's records have changed
// since m was taken.
func snapshotRestorable(m snapshotManifest, request storageOperatorRequest, target infraBindingTarget) (snapshotFile, error) {
	var file *snapshotFile
	for _, rec := range m.Stores {
		if rec.Name == "binding" && rec.Exact.Method == snapshotMethodVacuumInto {
			file = rec.Exact.File
		}
	}
	if file == nil || m.Binding == nil {
		return snapshotFile{}, errors.New("it holds no binding copy")
	}
	if !samePath(m.City, request.CityPath) {
		return snapshotFile{}, fmt.Errorf("it is a snapshot of the city at %s, not of this one (%s)", m.City, request.CityPath)
	}
	identity, err := currentWorkStoreIdentity(request.CityPath)
	if err != nil {
		return snapshotFile{}, err
	}
	if m.Identity != identity.String() {
		return snapshotFile{}, fmt.Errorf("it was taken against the work store %s, and this city's is now %s: the binding belongs to the work store it was carried from", m.Identity, identity.String())
	}
	current, err := readSnapshotBindingRecords(request.CityPath, target)
	if err != nil {
		return snapshotFile{}, err
	}
	if diff := diffSnapshotRecords(m.Binding.Records, current.Records); len(diff) > 0 {
		return snapshotFile{}, fmt.Errorf("the binding's records have changed since it was taken: %s. A restore puts back only the database, so it would serve rows those records no longer describe (a rig import since the snapshot would leave that rig's rows in neither store). Restore a snapshot taken since, or roll the change back first", strings.Join(diff, "; "))
	}
	return *file, nil
}

// installSnapshotBinding is restore steps 1-6 (the file comment) for target,
// from the snapshot copy src recorded with wantSHA and the id floor floor.
func installSnapshotBinding(ctx context.Context, target infraBindingTarget, src, wantSHA string, floor int64, instruction string, now time.Time) ([]string, error) {
	database, dir := target.Database, filepath.Dir(target.Database)
	if err := quiesceSnapshotRestoreTarget(ctx, database); err != nil {
		return nil, err
	}
	if err := writeRigImportRecord(target.RestoreRecordPath(), fmt.Sprintf("restore of binding %s into %s under way since %s; until it finishes the binding is stale. Finish it with `%s`", target.Binding, database, now.Format(time.RFC3339), instruction)); err != nil {
		return nil, fmt.Errorf("recording the restore at %s: %w", target.RestoreRecordPath(), err)
	}
	// Temp copies a killed restore left.
	if stale, err := filepath.Glob(database + ".restore-*"); err == nil {
		for _, p := range stale {
			_ = os.Remove(p)
		}
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(database)+".restore-*")
	if err != nil {
		return nil, err
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
	if err := raiseRestoredSequenceFloor(ctx, target, floor); err != nil {
		return failCopy(err)
	}
	if err := storageSnapshotStep("restore copied"); err != nil {
		return failCopy(err)
	}
	suffix := ".pre-restore-" + now.Format("20060102T150405Z")
	var aside []string
	for _, p := range []string{database, database + "-wal", database + "-shm", database + "-journal"} {
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
	if err := os.Remove(target.RestoreRecordPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return aside, fmt.Errorf("removing the restore record %s: %w", target.RestoreRecordPath(), err)
	}
	syncInfraDir(target.Root)
	return aside, nil
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

// raiseRestoredSequenceFloor raises the binding's id floor to the most of its
// current value, the snapshot's (recorded) and the replaced database's
// high-water, so the restored database mints no id the replaced one held.
func raiseRestoredSequenceFloor(ctx context.Context, target infraBindingTarget, recorded int64) error {
	prefix, err := infraBindingIDPrefix()
	if err != nil {
		return err
	}
	floor, err := beads.SQLiteSequenceFloorAt(target.Dir, prefix)
	if err != nil {
		return fmt.Errorf("reading the binding's id floor: %w", err)
	}
	floor = max(floor, recorded)
	// A database too damaged to read gives no high-water, and needs none: the
	// floor leads the allocator (every block is fsynced to it before an id in
	// it is handed out), so it already covers every id that database minted.
	if hw, err := beads.SQLiteSequenceHighWaterAt(ctx, target.Database, prefix); err == nil {
		floor = max(floor, hw)
	}
	if err := beads.RaiseSQLiteSequenceFloorAt(target.Dir, prefix, floor); err != nil {
		return fmt.Errorf("raising the binding's id floor to %d: %w", floor, err)
	}
	return nil
}
