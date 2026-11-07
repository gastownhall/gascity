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
// past it either: rows the live binding holds stamped as imported from a rig
// (the cross-store gates the import minted aside), which neither the snapshot
// nor that rig's own store holds, refuse too (snapshotRigImportsLost). It
// refuses a snapshot of another work store too, judged as boot judges a
// marker (workStoreIdentityMismatch).
//
// Under the controller lock, the migration guard and the snapshot directory's
// lock, it then:
//
//  1. folds the live database's WAL into it, refusing when another connection
//     holds the database (a busy checkpoint, or a -wal or -shm that outlives
//     this command's connection);
//  2. writes infra.restoring beside the marker, superseding the record an
//     earlier run left and carrying on the suffixes that run moved files aside
//     with. From here until step 7 the binding is stale whatever its database
//     holds (readInfraConvergenceState), so every gc that boots refuses it
//     rather than write to a database about to be replaced, and names this
//     command;
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
//  7. removes infra.recopying, the record of a re-copy that never finished,
//     which the installed database supersedes (it is a whole binding, and
//     step 5 raised the floor past every id the partial copy held), then
//     infra.restoring.
//
// A refusal before step 6 changes nothing: the copy goes, and so does this
// run's record, the one an earlier run left put back in its place. Once step
// 6 has begun, the record holds the binding stale until the same command
// finishes it. Nothing is deleted.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/dispatch"
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
	// EarlierAsideSuffixes are the suffixes of the earlier runs whose records
	// this one superseded: a run that stopped after a move left files aside
	// under its own suffix, and nothing else names them.
	EarlierAsideSuffixes []string `json:"earlier_aside_suffixes,omitempty"`
	// Command finishes the restore.
	Command string `json:"command"`
}

// asideSuffixes are the suffixes every run this record speaks for moved
// files aside with, the earliest first.
func (rec infraRestoreRecord) asideSuffixes() []string {
	var out []string
	for _, suffix := range append(slices.Clone(rec.EarlierAsideSuffixes), rec.AsideSuffix) {
		if suffix != "" && !slices.Contains(out, suffix) {
			out = append(out, suffix)
		}
	}
	return out
}

// asideFiles lists the files the runs this record speaks for moved aside from
// database.
func (rec infraRestoreRecord) asideFiles(database string) []string {
	var out []string
	for _, suffix := range rec.asideSuffixes() {
		found, _ := filepath.Glob(database + "*" + suffix)
		out = append(out, found...)
	}
	return out
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
	return writeInfraRestoreRecordData(target, append(data, '\n'))
}

// writeInfraRestoreRecordData writes data to infra.restoring atomically.
func writeInfraRestoreRecordData(target infraBindingTarget, data []byte) error {
	tmp, err := os.CreateTemp(target.Root, infraRestoreRecordName+".tmp-*")
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

// supersededInfraRestoreRecord reads the record an earlier run left, before
// this run supersedes it: its bytes (nil when there is none), which put the
// binding back as it was when this run stops before it moves anything, and
// the suffixes its runs moved files aside with, which this run's record
// carries on. A record that does not parse names no suffix; the stale message
// already says it cannot be read.
func supersededInfraRestoreRecord(target infraBindingTarget) ([]byte, []string, error) {
	data, err := os.ReadFile(target.RestoreRecordPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("reading the restore record %s: %w", target.RestoreRecordPath(), err)
	}
	var earlier infraRestoreRecord
	if json.Unmarshal(data, &earlier) != nil {
		return data, nil, nil
	}
	return data, earlier.asideSuffixes(), nil
}

// putBackInfraRestoreRecord undoes this run's record, for a run that moved
// nothing: the record an earlier run left (prior) goes back in its place, or,
// with none, the record goes.
func putBackInfraRestoreRecord(target infraBindingTarget, prior []byte) error {
	if prior != nil {
		return writeInfraRestoreRecordData(target, prior)
	}
	if err := os.Remove(target.RestoreRecordPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing the restore record %s: %w", target.RestoreRecordPath(), err)
	}
	return nil
}

// snapshotRestoreInstruction is the command that restores snapshot id from to.
func snapshotRestoreInstruction(id, to string) string {
	return "gc storage snapshot restore " + id + " --binding --" + storageFleetStoppedFlag + " --to " + to
}

// infraRestoreStaleCause says what an unfinished restore has done, for a
// stale binding's message; ok=false when none is recorded. It tells the three
// crash points apart: nothing moved yet, the live database moved aside, and
// the snapshot's database installed with only the record left. It names the
// files every run the record speaks for moved aside, so a re-run that stopped
// early still names those an earlier run moved.
func infraRestoreStaleCause(target infraBindingTarget) (string, bool) {
	rec, present, err := readInfraRestoreRecord(target)
	switch {
	case !present:
		return "", false
	case err != nil:
		return fmt.Sprintf("a restore of %s is under way and its record cannot be read (%v)", target.Database, err), true
	}
	began := fmt.Sprintf("a restore of snapshot %s into %s began at %s and has not finished", rec.Snapshot, target.Database, rec.StartedAt.Format(time.RFC3339))
	aside := rec.asideFiles(target.Database)
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

// infraRestoreRecopyRefusal is the refusal a re-copy of a stale binding meets
// while a restore is recorded, or nil when none is: the restore owns the
// binding, and a re-copy would race it or copy over the database it installs.
// The migration and its preflight both answer with it.
func infraRestoreRecopyRefusal(target infraBindingTarget) error {
	cause, restoring := infraRestoreStaleCause(target)
	if !restoring {
		return nil
	}
	return fmt.Errorf("%s claims convergence but %s; finish it with:  %s", target.MarkerPath(), cause, infraStaleBindingRemedy(target))
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
it are not restored. It refuses, too, while the live binding holds rows a rig
import carried in since the snapshot that neither the snapshot nor that rig's
own store holds. It reads the rig's store for them, starting the city's
bead-store provider for the read when it is stopped. A live binding too
damaged to read is restored over, with a warning that its rig imports went
unchecked.

The snapshot is proven against its manifest first, then installed under the
controller lock with the city stopped, in WAL mode, with the id floor raised
past every id the replaced database held. The database it replaces, with its
-wal, -shm and -journal, is kept beside it as *.pre-restore-<ts>. The
installed database is a whole binding, so it also replaces a re-copy that
never finished, and removes that re-copy's record (infra.recopying).

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
	dirLock, err := lockSnapshotForRestore(to, id)
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
	env := snapshotRestoreEnv{request: request, target: target, lock: lock, logPrefix: logPrefix, stderr: stderr}
	file, err := snapshotRestorable(env, m, dir)
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

// lockSnapshotForRestore takes to's snapshot lock for a restore of snapshot
// id. An id with no directory in to is refused first, by name: a snapshot
// looked for in the wrong place is missing, not a lock on a directory that
// does not exist or a damaged snapshot.
func lockSnapshotForRestore(to, id string) (*os.File, error) {
	if err := snapshotPresent(to, id); err != nil {
		return nil, err
	}
	return lockSnapshotDir(to)
}

// snapshotRestoreEnv is what a restore's checks read beyond the snapshot: the
// city and its binding, the controller lock a maintenance window over the
// rigs' stores needs, and where a warning goes.
type snapshotRestoreEnv struct {
	request   storageOperatorRequest
	target    infraBindingTarget
	lock      *os.File
	logPrefix string
	stderr    io.Writer
}

// snapshotRestorable is the binding copy of m, refused when m is of another
// work store, when the binding's records are no longer compatible with the
// ones m recorded, or when the live binding holds rows a rig import carried
// in that a restore would leave in no store.
func snapshotRestorable(env snapshotRestoreEnv, m snapshotManifest, snapshotDir string) (snapshotFile, error) {
	request, target := env.request, env.target
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
	if lost, err := snapshotRigImportsLost(env, filepath.Join(snapshotDir, file.Path)); err != nil {
		return snapshotFile{}, err
	} else if len(lost) > 0 {
		return snapshotFile{}, fmt.Errorf("the binding holds rows imported from rigs since the snapshot, which neither the snapshot nor the rig's own store holds: %s. Restoring it would leave those rows in no store; put them back in each rig's own store first (the runbook's \"Rolling back past a rig import\" says from where), then run this again", strings.Join(lost, "; "))
	}
	return *file, nil
}

// snapshotImportedRigs is every rig the live binding holds rows stamped as
// imported from (gc.infra_migrated_from=rig:<R>), read from its metadata
// index. The stamps name a rig whatever else does not: a rig removed from the
// config, whose records were moved aside, is named by its rows alone.
func snapshotImportedRigs(target infraBindingTarget) ([]string, error) {
	stamps, err := beads.SQLiteMetadataValues(context.Background(), target.Database, infraMigrationStampKey)
	if err != nil {
		return nil, fmt.Errorf("%w (reading which rigs it holds rows imported from: %w)", errSnapshotLiveUnreadable, err)
	}
	var rigs []string
	for _, stamp := range stamps {
		if rig, ok := strings.CutPrefix(stamp, rigImportStamp("")); ok && rig != "" {
			rigs = append(rigs, rig)
		}
	}
	return rigs, nil
}

// errSnapshotLiveUnreadable marks a live binding the content check could not
// open or list.
var errSnapshotLiveUnreadable = errors.New("the live binding cannot be read")

// snapshotRigImportsLost names, per rig, rows the live binding holds stamped
// as imported from it (gc.infra_migrated_from=rig:<R>) that neither the
// snapshot's copy nor the rig's own store holds: rows a restore would leave in
// no store, because the import cleared them from the rig. It is the content
// check behind the record check, for records moved aside by hand. The rig's
// store vouches for a row it holds under the same id and created_at, as it
// does once the runbook's rollback has put the row back.
//
// A live database that is gone (a restore killed mid-move) is not checked;
// the run that moved it was. One that cannot be opened or listed is not
// checked either, and the restore says so on stderr: damage is what a restore
// is often for, and the database is kept aside like any other it replaces.
func snapshotRigImportsLost(env snapshotRestoreEnv, copyPath string) ([]string, error) {
	imported, err := snapshotRigImportsUnsnapshotted(env.target, copyPath)
	if errors.Is(err, errSnapshotLiveUnreadable) {
		fmt.Fprintf(env.stderr, "%s: warning: %v, so its rows imported from rigs are not checked against the snapshot. The restore keeps it beside the database it installs, as *.pre-restore-<ts>\n", env.logPrefix, err) //nolint:errcheck // best-effort stderr
		return nil, nil
	}
	if err != nil || len(imported) == 0 {
		return nil, err
	}
	return snapshotRigImportsNotBack(env, imported)
}

// snapshotRigImportsUnsnapshotted lists, per rig its stamps name
// (snapshotImportedRigs), the rows the live binding holds stamped as imported
// from it that the snapshot's copy lacks. A cross-store gate the import minted
// in the binding, stamped as its own, is not one: the rig never held it, the
// restore drops it with the binding, and the edge it stands for goes back to
// the rig with its owner.
func snapshotRigImportsUnsnapshotted(target infraBindingTarget, copyPath string) (map[string][]beads.Bead, error) {
	if present, err := infraPathExists(target.Database); err != nil || !present {
		return nil, err
	}
	rigs, err := snapshotImportedRigs(target)
	if err != nil || len(rigs) == 0 {
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
		return nil, fmt.Errorf("%w (opening it to check its rig imports: %w)", errSnapshotLiveUnreadable, err)
	}
	defer closeBeadStoreHandle(live) //nolint:errcheck // read-only
	out := map[string][]beads.Bead{}
	for _, rig := range rigs {
		rows, err := live.List(beads.ListQuery{Metadata: map[string]string{infraMigrationStampKey: rigImportStamp(rig)}, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
		if err != nil {
			return nil, fmt.Errorf("%w (listing its rows imported from rig %s: %w)", errSnapshotLiveUnreadable, rig, err)
		}
		for _, b := range rows {
			if !held[b.ID] && !hasLabel(b.Labels, dispatch.CrossStoreGateLabel) {
				out[rig] = append(out[rig], b)
			}
		}
	}
	return out, nil
}

// snapshotRigImportsNotBack reads each rig's own store for the rows the
// binding holds imported from it that the snapshot lacks, and names, per rig,
// those the rig does not hold under the same id and created_at
// (snapshotRigLostEntry). The stores are read in a maintenance window
// (storage_maintenance.go), which starts the city's bead-store provider when
// it is stopped and stops it again, so a restore that finds no such row
// starts nothing.
func snapshotRigImportsNotBack(env snapshotRestoreEnv, imported map[string][]beads.Bead) (lost []string, err error) {
	rigs := sortedMapKeys(imported)
	roots := map[string]string{}
	var windowRoots []string
	for _, name := range rigs {
		if rig, ok := rigImportRigNamed(env.request.Cfg, name); ok {
			roots[name] = resolveStoreScopeRoot(env.request.CityPath, rig.Path)
			windowRoots = append(windowRoots, roots[name])
		}
	}
	if len(windowRoots) > 0 {
		window, openErr := openMigrationSource(env.lock, env.request.CityPath, env.request.Cfg, env.logPrefix, env.stderr, windowRoots...)
		if openErr != nil {
			return nil, fmt.Errorf("opening a maintenance window to read the rigs' stores for the rows they imported: %w", openErr)
		}
		defer func() { err = errors.Join(err, window.Close()) }()
	}
	for _, name := range rigs {
		missing, unread := snapshotRigRowsNotHeld(env.request.CityPath, roots[name], imported[name])
		if len(missing) == 0 {
			continue
		}
		backup := newRigImportLocation(env.target, env.request.CityPath, name).BackupPath()
		lost = append(lost, snapshotRigLostEntry(name, imported[name], missing, unread, backup))
	}
	return lost, nil
}

// snapshotRigLostEntry is the refusal's entry for rig name, whose store does
// not hold the rows missing names (of rows, the binding's copies), saying why
// the store vouched for none when it could not be read (unread). For a store
// that was read, it names the ones that are ephemeral or no-history rows (the
// copies carry both flags as the rig's store reported them): bd keeps those
// in its dolt_ignore'd tables, so a rollback that reset the rig to a Dolt tag
// still lacks them. backup is the rig import's backup of every row it cleared
// from the rig, which holds them.
func snapshotRigLostEntry(name string, rows []beads.Bead, missing []string, unread error, backup string) string {
	entry := fmt.Sprintf("rig %s: %d row(s), e.g. %s", name, len(missing), infraStrandedIDList(missing))
	if unread != nil {
		return entry + fmt.Sprintf(" (its store cannot be read: %v)", unread)
	}
	gone := make(map[string]bool, len(missing))
	for _, id := range missing {
		gone[id] = true
	}
	var tiered []string
	for _, b := range rows {
		if gone[b.ID] && (b.Ephemeral || b.NoHistory) {
			tiered = append(tiered, b.ID)
		}
	}
	if len(tiered) == 0 {
		return entry
	}
	sort.Strings(tiered)
	which := "all of them are"
	if len(tiered) < len(missing) {
		which = fmt.Sprintf("%d of them, e.g. %s, are", len(tiered), infraStrandedIDList(tiered))
	}
	return entry + fmt.Sprintf(" (%s ephemeral or no-history rows, which bd keeps out of Dolt history, so no Dolt tag of the rig holds them; they are in the rig import's backup, %s)", which, backup)
}

// snapshotRigRowsNotHeld lists, in id order, the rows the rig store at root
// does not hold under the same id and created_at. A store that cannot be read
// vouches for none: every row is listed, with why (unread). root is empty for
// a rig the config no longer names.
func snapshotRigRowsNotHeld(cityPath, root string, rows []beads.Bead) (missing []string, unread error) {
	ids := make([]string, 0, len(rows))
	for _, b := range rows {
		ids = append(ids, b.ID)
	}
	sort.Strings(ids)
	if root == "" {
		return ids, errors.New("the rig is not in this city's config")
	}
	store, err := openStorageScopeStore(root, cityPath)
	if err != nil {
		return ids, err
	}
	defer closeBeadStoreHandle(store) //nolint:errcheck // read-only
	held, err := beads.GetMany(store, ids)
	if err != nil {
		return ids, err
	}
	for _, b := range rows {
		if h, ok := held[b.ID]; !ok || !rigSameCreated(b, h) {
			missing = append(missing, b.ID)
		}
	}
	sort.Strings(missing)
	return missing, nil
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
// from the snapshot copy src recorded with wantSHA and the id floor floor. It
// returns the files the restore keeps aside. On success that is every file
// the restore's runs moved: this run's, and those of the earlier runs whose
// record it superseded. On failure it is what this run changed: the files it
// moved, then the copy or the record once it got that far, so empty means it
// changed nothing.
func installSnapshotBinding(ctx context.Context, target infraBindingTarget, src, wantSHA string, floor int64, rec infraRestoreRecord) ([]string, error) {
	database, dir := target.Database, filepath.Dir(target.Database)
	if err := quiesceSnapshotRestoreTarget(ctx, database); err != nil {
		return nil, err
	}
	prior, err := recordSnapshotRestore(target, &rec)
	if err != nil {
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
		return nil, errors.Join(err, putBackInfraRestoreRecord(target, prior))
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(tmpPath)
	// Until the live database moves, a failure leaves the binding as it was:
	// the copy goes, and the record is put back as this run found it, so one
	// an earlier run left still names the files that run moved aside.
	failCopy := func(err error) ([]string, error) {
		for _, p := range []string{tmpPath, tmpPath + "-wal", tmpPath + "-shm"} {
			_ = os.Remove(p)
		}
		return nil, errors.Join(err, putBackInfraRestoreRecord(target, prior))
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
	return finishSnapshotRestore(target, rec, aside)
}

// recordSnapshotRestore is restore step 2: it writes rec as infra.restoring
// under a new aside suffix, superseding the record an earlier run left and
// carrying on the suffixes that run moved files aside with. It returns the
// earlier record's bytes, nil when there was none, to put back
// (putBackInfraRestoreRecord) if this run stops before it moves anything.
func recordSnapshotRestore(target infraBindingTarget, rec *infraRestoreRecord) ([]byte, error) {
	prior, earlier, err := supersededInfraRestoreRecord(target)
	if err != nil {
		return nil, err
	}
	rec.AsideSuffix, rec.EarlierAsideSuffixes = snapshotAsideSuffix(target.Database, rec.StartedAt), earlier
	return prior, writeInfraRestoreRecord(target, *rec)
}

// finishSnapshotRestore is restore step 7, once the snapshot's database is in
// place. The installed database is a whole binding, so it supersedes a
// re-copy that never finished, whose record would hold it stale under a
// remedy that refuses a populated database: that record goes first, while
// this restore's own still holds the binding stale, and then its own. It
// returns what installSnapshotBinding does, moved being what this run moved.
func finishSnapshotRestore(target infraBindingTarget, rec infraRestoreRecord, moved []string) ([]string, error) {
	if err := removeInfraRecopyRecord(target); err != nil {
		return append(moved, target.Database), err
	}
	if err := storageSnapshotStep("restore installed"); err != nil {
		return append(moved, target.Database), err
	}
	kept := rec.asideFiles(target.Database)
	if err := os.Remove(target.RestoreRecordPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return append(moved, target.RestoreRecordPath()), fmt.Errorf("removing the restore record %s: %w", target.RestoreRecordPath(), err)
	}
	syncInfraDir(target.Root)
	return kept, nil
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
