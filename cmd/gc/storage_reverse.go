package main

// `gc storage migrate --to-work`, `--to-rigs` and `--abort-reverse`: the
// reverse migration (S5 §4, S4c).
//
// --to-work copies the binding's CURRENT rows back into the stores they would
// live in on a single-store city, so everything written since the cutover is
// kept, and then retires the split:
//
//	R0  the mandatory snapshot (binding, work store, rigs), pinned; the journal
//	    .gc/storage-reverse.json and infra.reverting beside the binding
//	R1  per rig: its imported rows back into its store, proven
//	R2  the city: every other row into the work store, proven; recorded cross
//	    edges restored; the work store's id floor
//	R3  the reverse manifest (infra.reversed.beads), proven
//	R4  city.toml without [storage] (the pre-split bytes when the split
//	    journal holds them and nothing else changed since); the served note
//	    and the city's and each rig's cleared notes renamed *.reverted-<run>;
//	    .gc/min-gc-version removed
//	R5  the binding root renamed *.reverted-<run>, the rigs' import records
//	    with it; the journal completed
//
// --to-rigs --rig X is the same engine for one rig's import, with the city
// phase skipped: R1 into rig X, R3 its manifest, then R4 removes the rows from
// the binding (the city stays split) and renames rig X's import records, so
// the rig reads as awaiting import again.
//
// Every row it writes carries gc.infra_reversed_from=<binding>/<run>. Nothing
// writes the binding before R5 (to-work) or R4 (to-rigs), so until then the
// reverse can always be abandoned: --abort-reverse deletes exactly the rows
// carrying this run's stamp, puts back what R4 changed, and removes the
// records. Every phase is idempotent and resumable: a rerun keeps the rows an
// earlier run wrote that still equal the plan, and deletes the rest. While the
// journal is open, boot and every storage verb refuse the city and name both
// commands.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/fsys"
)

const (
	storageToWorkFlag       = "to-work"
	storageToRigsFlag       = "to-rigs"
	storageAbortReverseFlag = "abort-reverse"
	storageReconcilerFlag   = "reconciler"
	storageDryRunFlag       = "dry-run"

	// storageReverseJournalName is the reverse's journal under the city's .gc.
	storageReverseJournalName = "storage-reverse.json"
	// storageReverseJournalFormat names the journal's shape.
	storageReverseJournalFormat = "gc.storage.reverse/v1"
	// infraRevertingRecordName is the record beside the binding while a
	// reverse is under way.
	infraRevertingRecordName = "infra.reverting"
	// infraReversedManifestName is the reverse manifest of a to-work run;
	// a to-rigs run writes infraReversedRigManifestName(rig).
	infraReversedManifestName = "infra.reversed.beads"
	// storageSplitJournalName is the guided split's journal (S5-H), which
	// holds city.toml as it was before the split.
	storageSplitJournalName = "storage-split.journal.json"
	// reverseKeepSplitRemedy is the fix of a refusal no change to the city
	// can clear: the reverse cannot take this city back.
	reverseKeepSplitRemedy = "nothing was written; keep the city split, or roll back by restoring the snapshot (the runbook's restore steps)"
)

// The reverse's phases, as the journal records the last one done.
const (
	reverseStarted  = iota // the journal exists; R0's snapshot is not taken
	reverseR0              // snapshot pinned, infra.reverting written
	reverseR1              // rigs copied and proven
	reverseR2              // the city copied and proven
	reverseR3              // the reverse manifest proven
	reverseR4              // config and records retired (to-work); binding cleared (to-rigs)
	reverseComplete        // R5
)

func infraReversedRigManifestName(rig string) string {
	return "infra.reversed.rig." + rig + ".beads"
}

// storageReverseInstruction is the command that starts or resumes a reverse.
func storageReverseInstruction(mode reverseMode, rigs []string) string {
	if mode == reverseToRigs {
		var parts []string
		for _, rig := range rigs {
			parts = append(parts, "--"+storageRigFlag+" "+rig)
		}
		return "gc storage migrate --" + storageToRigsFlag + " " + strings.Join(parts, " ") + " --" + storageFleetStoppedFlag
	}
	return "gc storage migrate --" + storageToWorkFlag + " --" + storageFleetStoppedFlag
}

// storageAbortReverseInstruction is the command that abandons a reverse.
func storageAbortReverseInstruction() string {
	return "gc storage migrate --" + storageAbortReverseFlag + " --" + storageFleetStoppedFlag
}

// storageReverseRollbackAdvice is the rollback a refused hand revert of the
// split names first: the reverse copies the binding's rows back before it
// retires the split, so it loses nothing written since the cutover.
func storageReverseRollbackAdvice() string {
	return fmt.Sprintf("To roll back to the work store and keep every bead written to the binding since the cutover, put [storage] back as the split had it and run `%s`, which copies the binding's rows back before it retires the split (\"Reversing the split\" in docs/runbooks/split-storage-classes.md).", storageReverseInstruction(reverseToWork, nil))
}

// reverseRename is one record R4 moved aside.
type reverseRename struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// storageReverseJournal is .gc/storage-reverse.json.
type storageReverseJournal struct {
	Format    string      `json:"format"`
	Mode      reverseMode `json:"mode"`
	Rigs      []string    `json:"rigs,omitempty"`
	Phase     int         `json:"phase"`
	Stamp     string      `json:"stamp"`
	Run       string      `json:"run"`
	StartedAt time.Time   `json:"started_at"`
	Binding   string      `json:"binding"`
	Root      string      `json:"root"`
	Dir       string      `json:"dir"`
	Database  string      `json:"database"`
	// SnapshotTo and Snapshot name R0's snapshot.
	SnapshotTo string `json:"snapshot_to,omitempty"`
	Snapshot   string `json:"snapshot,omitempty"`
	// Reconciler is the [daemon] session_reconciler R4 writes, or "".
	Reconciler string `json:"reconciler,omitempty"`
	// CityTOMLBefore and CityTOMLAfterSHA256 are city.toml before and after
	// R4's edit; MinGCVersion is the floor file R4 removed.
	CityTOMLBefore      *string         `json:"city_toml_before,omitempty"`
	CityTOMLAfterSHA256 string          `json:"city_toml_after_sha256,omitempty"`
	MinGCVersion        *string         `json:"min_gc_version,omitempty"`
	Renamed             []reverseRename `json:"renamed,omitempty"`
	// Dests are the stores this reverse writes; --abort-reverse touches
	// these and no other.
	Dests []string `json:"dests,omitempty"`
	// Preserved are the edges each destination held onto a planned row
	// before the reverse wrote anything, put back after a delete of the row.
	Preserved map[string][]reverseEdge `json:"preserved,omitempty"`
	// BindingSHA256 fingerprints the binding the copies were planned from;
	// the binding is re-proven against it before R3 and before it changes.
	BindingSHA256 string `json:"binding_sha256,omitempty"`
	// ClearStarted records that a to-rigs R4 has begun to remove rows from
	// the binding, which then no longer matches its fingerprint: from here
	// the rig may hold the only copy, so the reverse never goes back below
	// R3 and cannot be abandoned.
	ClearStarted bool `json:"clear_started,omitempty"`
	// Recopying is the rows a re-copy after the clear began is returning to
	// their rigs, recorded before it writes: until one proves them, every
	// run finishes it.
	Recopying []string `json:"recopying,omitempty"`
	// ClearEdges is every edge that touched a row the to-rigs clear removes,
	// in the binding ("binding") and in each rig (its destination), both
	// directions and with payloads, recorded with ClearStarted before the
	// first delete: a re-copy after the clear began restores a lost row's
	// edges from here, those from rows the binding no longer holds included.
	ClearEdges map[string][]reverseEdge `json:"clear_edges,omitempty"`
	// PreservedRecorded records that Preserved was computed, on the run's
	// first plan and only then: an empty Preserved is an answer, not a
	// question still open.
	PreservedRecorded bool `json:"preserved_recorded"`
	// Aborting records that --abort-reverse has begun deleting this run's
	// rows: the reverse can then only be abandoned, never resumed.
	Aborting bool `json:"aborting,omitempty"`
}

func (j storageReverseJournal) target() infraBindingTarget {
	return infraBindingTarget{Binding: j.Binding, Root: j.Root, Dir: j.Dir, Database: j.Database}
}

func (j storageReverseJournal) suffix() string { return ".reverted-" + j.Run }

func storageReverseJournalPath(cityPath string) string {
	return filepath.Join(cityPath, ".gc", storageReverseJournalName)
}

// readStorageReverseJournal reads the journal; present is false when there
// is none. An unreadable journal is an error, never an absence.
func readStorageReverseJournal(cityPath string) (storageReverseJournal, bool, error) {
	var j storageReverseJournal
	data, err := os.ReadFile(storageReverseJournalPath(cityPath))
	if errors.Is(err, os.ErrNotExist) {
		return j, false, nil
	}
	if err != nil {
		return j, true, fmt.Errorf("reading the reverse journal %s: %w", storageReverseJournalPath(cityPath), err)
	}
	if err := json.Unmarshal(data, &j); err != nil {
		return j, true, fmt.Errorf("decoding the reverse journal %s: %w", storageReverseJournalPath(cityPath), err)
	}
	return j, true, nil
}

// writeStorageReverseJournal writes the journal atomically.
func writeStorageReverseJournal(cityPath string, j storageReverseJournal) error {
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return writeRigImportRecord(storageReverseJournalPath(cityPath), string(data))
}

// storageReverseRefusal is the hold an open reverse puts on boot and on every
// other storage verb, or nil: a journal not yet complete, an unreadable one,
// or an infra.reverting record at root (the resolved binding root, "" when
// the city has none) with no journal. A file read, like a rig's .importing.
func storageReverseRefusal(cityPath, root string) error {
	j, present, err := readStorageReverseJournal(cityPath)
	switch {
	case err != nil:
		return fmt.Errorf("a reverse migration's journal cannot be read (%w); it holds the city as an open reverse would. Repair it, then finish the reverse or abandon it with `%s`", err, storageAbortReverseInstruction())
	case present && j.Phase < reverseComplete && j.Aborting:
		return fmt.Errorf("an abort of a reverse migration (--%s) is in progress (%s): the work and rig stores may hold part of the binding's rows. Finish abandoning it with `%s`", j.Mode, reversePhaseName(j.Phase), storageAbortReverseInstruction())
	case present && j.Phase < reverseComplete:
		return fmt.Errorf("a reverse migration (--%s) is in progress (%s): the work and rig stores may hold part of the binding's rows. Finish it with `%s`, or abandon it with `%s`", j.Mode, reversePhaseName(j.Phase), storageReverseInstruction(j.Mode, j.Rigs), storageAbortReverseInstruction())
	case present:
		return nil
	}
	if root == "" {
		return nil
	}
	if marker := filepath.Join(root, infraRevertingRecordName); fileExists(marker) {
		return fmt.Errorf("%s records a reverse migration under way, and its journal %s is gone, so nothing says how far it got. Restore the journal from a backup of the city's .gc and finish or abandon the reverse", marker, storageReverseJournalPath(cityPath))
	}
	return nil
}

// storageReverseRoot is the binding root the city's config resolves, or "".
func storageReverseRoot(cityPath string, cfg *config.City) string {
	if cfg == nil || cfg.Storage == nil {
		return ""
	}
	target, ok, err := resolveInfraBindingTarget(cityPath, cfg)
	if err != nil || !ok {
		return ""
	}
	return target.Root
}

// reversePhaseName names a journal phase for an operator.
func reversePhaseName(phase int) string {
	switch phase {
	case reverseStarted:
		return "taking the snapshot (R0)"
	case reverseR0:
		return "snapshot taken; copying the rigs (R1)"
	case reverseR1:
		return "rigs copied; copying the city (R2)"
	case reverseR2:
		return "copies proven; writing the reverse manifest (R3)"
	case reverseR3:
		return "manifest proven; retiring the split's records (R4)"
	case reverseR4:
		return "records retired; archiving the binding (R5)"
	}
	return "complete"
}

// reverseOptions is one reverse run's flags.
type reverseOptions struct {
	Mode       reverseMode
	Rigs       []string
	Reconciler string
	DryRun     bool
}

// reverseGuardedSeam is the point a run passes once it holds the lock and the
// guard, before it judges the journal again: nothing has been written there,
// so it is a seam for tests, not a boundary to resume from.
const reverseGuardedSeam = "reverse guarded"

// doStorageReverse is `gc storage migrate --to-work` and `--to-rigs`.
func doStorageReverse(ctx context.Context, request storageOperatorRequest, opts reverseOptions, stdout, stderr io.Writer) (code int) {
	logPrefix := "gc storage migrate --" + string(opts.Mode)
	fail := func(err error) int {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if opts.Mode == reverseToRigs && len(opts.Rigs) == 0 {
		return fail(fmt.Errorf("--%s needs at least one --%s: it returns those rigs' imports and keeps the city split", storageToRigsFlag, storageRigFlag))
	}
	if opts.Mode == reverseToWork && len(opts.Rigs) > 0 {
		return fail(fmt.Errorf("--%s returns every rig with the city; to return one rig and keep the city split, run `%s`", storageToWorkFlag, storageReverseInstruction(reverseToRigs, opts.Rigs)))
	}
	if opts.Reconciler != "" && (opts.Reconciler != config.SessionReconcilerLegacy || opts.Mode != reverseToWork) {
		return fail(fmt.Errorf("--%s takes %q, with --%s only", storageReconcilerFlag, config.SessionReconcilerLegacy, storageToWorkFlag))
	}
	journal, open, err := readStorageReverseJournal(request.CityPath)
	if err != nil {
		return fail(err)
	}
	open = open && journal.Phase < reverseComplete
	if open && journal.Aborting {
		return fail(fmt.Errorf("this reverse is being abandoned: an abort has begun deleting its rows, so it cannot be resumed. Finish abandoning it with `%s`", storageAbortReverseInstruction()))
	}
	if open && (journal.Mode != opts.Mode || (opts.Mode == reverseToRigs && strings.Join(journal.Rigs, ",") != strings.Join(sortedUnique(opts.Rigs), ","))) {
		return fail(fmt.Errorf("another reverse is in progress: finish it with `%s`, or abandon it with `%s`", storageReverseInstruction(journal.Mode, journal.Rigs), storageAbortReverseInstruction()))
	}
	if open && opts.Reconciler != "" && opts.Reconciler != journal.Reconciler {
		return fail(fmt.Errorf("this reverse started without --%s=%s; finish it as it started, or abandon it with `%s`", storageReconcilerFlag, opts.Reconciler, storageAbortReverseInstruction()))
	}
	var target infraBindingTarget
	if open {
		target = journal.target()
		opts.Reconciler = journal.Reconciler
	} else {
		var ok bool
		target, ok, err = resolveInfraBindingTarget(request.CityPath, request.Cfg)
		if err != nil {
			return fail(err)
		}
		if !ok {
			return fail(errors.New("this city's [storage.classes] assign its infrastructure classes to no binding: every class is already on the work store, so there is nothing to reverse"))
		}
		state, err := readInfraConvergenceState(target)
		if err == nil && state == infraConvergenceStale {
			return fail(fmt.Errorf("the binding %s is stale: its marker is present and %s, so it is not the authority a reverse copies from. Repair it with `%s` first", target.Binding, infraStaleCause(target), infraStaleBindingRemedy(target)))
		}
		if err != nil || state != infraConvergenceMarked {
			return fail(fmt.Errorf("the binding %s has not converged (no proven copy behind %s), so it is not the authority a reverse copies from. Finish the migration (`%s`) or restore the binding first", target.Binding, target.MarkerPath(), storageClearInstruction()))
		}
	}
	if opts.Mode == reverseToWork && (!open || journal.Phase < reverseR4) && opts.Reconciler == "" {
		if mode, _, _ := request.Cfg.Daemon.SessionReconcilerMode(); mode == config.SessionReconcilerV2 {
			return fail(fmt.Errorf("[daemon] session_reconciler = \"v2\" would refuse to start the reverted city: v2 serves sessions only from a store that fences its writes (C0.7), and with the split gone the sessions class is on the work store. Pass --%s=%s to set it to \"legacy\" in the same city.toml edit, or set it to \"legacy\" yourself first", storageReconcilerFlag, config.SessionReconcilerLegacy))
		}
	}
	if opts.Mode == reverseToRigs {
		rigs := opts.Rigs
		if open {
			rigs = journal.Rigs
		}
		retired := open && (journal.Phase >= reverseR4 || journal.ClearStarted || len(journal.Renamed) > 0)
		if err := reverseRigsRefusal(request.CityPath, request.Cfg, target, rigs, retired); err != nil {
			return fail(err)
		}
	}

	var window *migrationSourceWindow
	if !opts.DryRun {
		// The controller lock, the liveness checks and the attestation, the
		// shared-store check, then the guard, then the window: the order every
		// writing storage verb takes. A dry run is a live rehearsal: it takes
		// none of them and writes nothing.
		controllerLock, err := acquireStorageVerbExclusion(request, storageVerbScope{})
		if err != nil {
			return fail(err)
		}
		defer controllerLock.Close() //nolint:errcheck // released last
		if pid := infraMigrationForeignControllerPID(request.CityPath); pid != 0 {
			return fail(fmt.Errorf("controller PID %d is live on this city. Stop it (%s) and run this again", pid, storageStopCommand))
		}
		shared, err := sharedWorkStoreRefusal(request.CityPath, request.Force)
		if err != nil {
			return fail(err)
		}
		defer shared.Release() // the co-tenants stay stopped until the reverse is done
		hazards := request.Force.acceptedHazards(shared)
		rigShared, err := reverseRigSharedStoreRefusal(request.CityPath, request.Cfg, request.Force)
		defer func() {
			for _, x := range rigShared {
				x.Release()
			}
		}()
		if err != nil {
			return fail(err)
		}
		for _, x := range rigShared {
			hazards = append(hazards, x.hazards()...)
		}
		if !request.FleetStopped {
			return fail(fmt.Errorf("pass --%s to attest that %s. A write that lands in the binding during the reverse is not copied back", storageFleetStoppedFlag, storageFleetStoppedAttestation))
		}
		guard, err := acquireStorageMigrationGuard(ctx, request.CityPath)
		if err != nil {
			return fail(err)
		}
		defer guard.Release() //nolint:errcheck // released before the lock
		if err := infraEngineCrashPoint(reverseGuardedSeam); err != nil {
			return fail(err)
		}
		// The journal is judged again as it is under the lock and the guard:
		// another reverse, or an abort, may have moved it while this one
		// waited.
		again, againOpen, err := readStorageReverseJournal(request.CityPath)
		if err != nil {
			return fail(err)
		}
		if againOpen = againOpen && again.Phase < reverseComplete; againOpen != open || (open && !reflect.DeepEqual(again, journal)) {
			return fail(fmt.Errorf("the reverse journal %s changed while this run waited for the controller lock and the migration guard; nothing was written. Run this again", storageReverseJournalPath(request.CityPath)))
		}
		window, err = openMigrationSource(controllerLock, request.CityPath, request.Cfg, logPrefix, stderr, reverseRigRoots(request.CityPath, request.Cfg)...)
		if err != nil {
			return fail(err)
		}
		defer window.closeReporting(logPrefix, stderr, &code)
		window.setTimeBudget(request.TimeBudget)
		recordStorageAttestation(request.CityPath, logPrefix, sortedUnique(hazards), stderr)
	} else {
		// The dry run's opens take the lanes the run's window would force,
		// in a window that starts nothing and holds no lock.
		defer openPreflightLaneWindow(request.CityPath, request.Cfg)()
	}

	run := reverseRun{request: request, opts: opts, target: target, journal: journal, resumed: open, logPrefix: logPrefix, stdout: stdout, stderr: stderr}
	if !open {
		now := time.Now().UTC()
		run.journal = storageReverseJournal{
			Format: storageReverseJournalFormat, Mode: opts.Mode, Rigs: sortedUnique(opts.Rigs), Phase: reverseStarted,
			Run: reverseRunID(now), StartedAt: now, Reconciler: opts.Reconciler,
			Binding: target.Binding, Root: target.Root, Dir: target.Dir, Database: target.Database,
		}
		run.journal.Stamp = target.Binding + "/" + run.journal.Run
	}
	if run.journal.Phase < reverseR3 || opts.DryRun {
		plan, err := run.plan()
		if err != nil {
			return fail(err)
		}
		run.report(plan)
		if len(plan.Refusals) > 0 {
			fmt.Fprintf(stderr, "%s: REFUSED — %d shape(s) cannot be reversed; nothing was written:\n", logPrefix, len(plan.Refusals)) //nolint:errcheck // best-effort stderr
			for _, r := range plan.Refusals {
				fmt.Fprintf(stderr, "  %s\n", r) //nolint:errcheck // best-effort stderr
			}
			return 1
		}
		if opts.DryRun {
			fmt.Fprintf(stdout, "Dry run: nothing was written. Run `%s` to reverse.\n", storageReverseInstruction(opts.Mode, opts.Rigs)) //nolint:errcheck // best-effort stdout
			return 0
		}
		run.planned = &plan
	}
	if err := run.phases(ctx); err != nil {
		var pause *infraEnginePause
		if errors.As(err, &pause) {
			fmt.Fprintf(stderr, "%s: %v (exit %d)\n", logPrefix, pause, storagePausedExitCode) //nolint:errcheck // best-effort stderr
			return storagePausedExitCode
		}
		return fail(fmt.Errorf("%w. The reverse stopped in %s; run `%s` to continue, or `%s` to abandon it", err, reversePhaseName(run.journal.Phase), storageReverseInstruction(opts.Mode, run.journal.Rigs), storageAbortReverseInstruction()))
	}
	// The window closes before the run reports: a provider its stop left
	// running fails the run.
	window.closeReporting(logPrefix, stderr, &code)
	if code != 0 {
		return code
	}
	if opts.Mode == reverseToRigs {
		fmt.Fprintf(stdout, "Reversed. Rig(s) %s hold their imported rows again and read as awaiting import; the city stays split. Snapshot %s, of the stores as they were before the reverse, stays pinned (remove its pinned file to let retention prune it). Start the city with `gc start`.\n", strings.Join(run.journal.Rigs, ", "), run.journal.Snapshot) //nolint:errcheck // best-effort stdout
		return 0
	}
	fmt.Fprintf(stdout, "Reversed. Every class is on the work store again; the binding is archived as %s. Snapshot %s, of the stores as they were before the reverse, stays pinned (remove its pinned file to let retention prune it). Start the city with `gc start`.\n", target.Root+run.journal.suffix(), run.journal.Snapshot) //nolint:errcheck // best-effort stdout
	return 0
}

// reverseRunID names one run: its start to the nanosecond and a random
// suffix, so two runs never share a stamp.
func reverseRunID(now time.Time) string {
	var suffix [4]byte
	_, _ = rand.Read(suffix[:])
	return now.Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(suffix[:])
}

// reverseRun is one reverse in progress.
type reverseRun struct {
	request storageOperatorRequest
	opts    reverseOptions
	target  infraBindingTarget
	journal storageReverseJournal
	// resumed records that the run found the journal open: every other
	// storage verb refuses the city until the reverse is finished or
	// abandoned (storageReverseRefusal).
	resumed   bool
	planned   *reversePlan
	logPrefix string
	stdout    io.Writer
	stderr    io.Writer
}

// reverseRigsRefusal checks the rigs a --to-rigs names, before anything is
// locked or opened: each must be a rig this city configures with a store of
// its own, under a name that can name its records, and imported (its marker
// and manifest read), unless the reverse has already retired its records.
func reverseRigsRefusal(cityPath string, cfg *config.City, target infraBindingTarget, rigs []string, retired bool) error {
	own := map[string]bool{}
	for _, rig := range reverseRigs(cityPath, cfg) {
		own[rig.Name] = true
	}
	for _, name := range rigs {
		switch {
		case !rigNameIsFileSafe(name) || strings.ContainsAny(name, "*?[]"):
			return fmt.Errorf("--%s %q is not a rig name: it cannot name the rig's records", storageRigFlag, name)
		case !own[name]:
			return fmt.Errorf("this city configures no rig %q with a store of its own, so there is nothing to return to it", name)
		case retired:
			continue
		}
		manifest, marked, err := newRigImportLocation(target, cityPath, name).readManifest()
		if err != nil {
			return err
		}
		if !marked || manifest == nil {
			return fmt.Errorf("rig %s has no import marker and manifest beside the binding: it was never imported, so there is nothing to return", name)
		}
	}
	return nil
}

// reverseRigSharedStoreRefusal is the shared-store check for each rig store
// a reverse writes, as the rig import makes it (rigImportSharedStoreRefusal):
// a store another registered city also uses is refused unless attested, and
// an attested one keeps its co-tenants stopped until the caller releases it.
func reverseRigSharedStoreRefusal(cityPath string, cfg *config.City, force storageForce) ([]sharedStoreExclusion, error) {
	var held []sharedStoreExclusion
	for _, rig := range reverseRigs(cityPath, cfg) {
		x, err := sharedStoreRefusal(cityPath, "rig "+rig.Name, rigStoreRoot(cityPath, rig), force)
		if err != nil {
			return held, err
		}
		held = append(held, x)
	}
	return held, nil
}

// reverseRigRoots are the scope roots of the rigs with a store of their own.
func reverseRigRoots(cityPath string, cfg *config.City) []string {
	var roots []string
	for _, rig := range reverseRigs(cityPath, cfg) {
		roots = append(roots, rigStoreRoot(cityPath, rig))
	}
	return roots
}

// reverseArchiveRefusals refuses a binding root R5 cannot archive. R5 renames
// the root with everything under it, so it refuses a root that would take
// with it:
//
//   - the city's own state or a rig: a root that is or holds the city's .gc
//     (a path of ".gc" or ".", or any directory above the city) or a rig;
//   - a store, or part of one: a root that is, holds or lies inside a
//     directory a store keeps its files in, the city's .beads (bd's, and the
//     managed Dolt server's data, .beads/dolt) and each rig's .beads and .gc.
//
// A root inside the city's .gc, such as the default .gc/store, takes nothing
// else with it.
func reverseArchiveRefusals(cityPath string, cfg *config.City, target infraBindingTarget) []rigImportRefusal {
	var held []string
	if gcDir := filepath.Join(cityPath, ".gc"); pathIsWithin(target.Root, gcDir) {
		held = append(held, "the city's .gc ("+gcDir+")")
	}
	held = append(held, reverseStoreDirTaken(target.Root, filepath.Join(cityPath, ".beads"), "the city's store directory .beads")...)
	for _, rig := range reverseRigs(cityPath, cfg) {
		root := rigStoreRoot(cityPath, rig)
		if pathIsWithin(target.Root, root) {
			held = append(held, "rig "+rig.Name+" ("+root+")")
			continue
		}
		for _, dir := range []string{".beads", ".gc"} {
			held = append(held, reverseStoreDirTaken(target.Root, filepath.Join(root, dir), "rig "+rig.Name+"'s store directory "+dir)...)
		}
	}
	if len(held) == 0 {
		return nil
	}
	return []rigImportRefusal{{
		IDs:    []string{"binding " + target.Binding + " at " + target.Root},
		Reason: fmt.Sprintf("R5 archives the binding root whole, renaming it %s.reverted-<run>, and that would take %s with it", target.Root, strings.Join(held, " and ")),
		Remedy: reverseKeepSplitRemedy,
	}}
}

// reverseStoreDirTaken names what an archive of root would take of dir, the
// directory named name where a store keeps its files: all of it when root is
// or holds dir, part of it when root lies inside dir, and nothing otherwise.
func reverseStoreDirTaken(root, dir, name string) []string {
	switch {
	case pathIsWithin(root, dir):
		return []string{name + " (" + dir + ")"}
	case pathIsWithin(dir, root):
		return []string{"part of " + name + " (" + dir + ")"}
	}
	return nil
}

// reverseRigs are the configured rigs with a store of their own.
func reverseRigs(cityPath string, cfg *config.City) []config.Rig {
	if cfg == nil {
		return nil
	}
	cityRoot := resolveStoreScopeRoot(cityPath, cityPath)
	var out []config.Rig
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" || samePath(rigStoreRoot(cityPath, rig), cityRoot) {
			continue
		}
		out = append(out, rig)
	}
	return out
}

// reverseBindingRead is one read of the binding: every row, edge and
// payload.
type reverseBindingRead struct {
	rows     []beads.Bead
	down     map[string][]beads.Dep
	payloads map[beads.DepPair]string
}

// fingerprint is a digest of every row, edge and payload the read holds.
func (r reverseBindingRead) fingerprint() string {
	rows := sortedBeads(r.rows)
	sum := sha256.New()
	for _, b := range rows {
		b.IsBlocked = nil
		data, _ := json.Marshal(b)
		sum.Write(data) //nolint:errcheck // hashing a writer that cannot fail
		edges := censusEdges(r.down[b.ID])
		sort.Strings(edges)
		for _, e := range edges {
			fmt.Fprintln(sum, e) //nolint:errcheck // hashing a writer that cannot fail
		}
	}
	for _, pair := range sortedPairs(r.payloads) {
		fmt.Fprintf(sum, "%s>%s=%s\n", pair.IssueID, pair.DependsOnID, r.payloads[pair]) //nolint:errcheck // hashing a writer that cannot fail
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func sortedPairs(m map[beads.DepPair]string) []beads.DepPair {
	out := make([]beads.DepPair, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IssueID != out[j].IssueID {
			return out[i].IssueID < out[j].IssueID
		}
		return out[i].DependsOnID < out[j].DependsOnID
	})
	return out
}

// reproveBinding re-reads the binding and compares it with the one the
// copies were planned from (S5 §2.3: a source that moved during the copy is
// refused). A binding that changed puts the reverse back to R0, so the next
// run plans and copies again.
func (r *reverseRun) reproveBinding(before string) error {
	read, err := readReverseBinding(r.target)
	if err != nil {
		return err
	}
	if read.fingerprint() == r.journal.BindingSHA256 {
		return nil
	}
	r.journal.Phase = reverseR0
	if err := writeStorageReverseJournal(r.request.CityPath, r.journal); err != nil {
		return err
	}
	return fmt.Errorf("the binding changed after its rows were copied (before %s), so a writer still reaches it. Stop it; the next run copies again", before)
}

// readReverseBinding reads the binding read-only, in batches where it can.
func readReverseBinding(target infraBindingTarget) (reverseBindingRead, error) {
	binding, err := openInfraBindingReadOnly(target)
	if err != nil {
		return reverseBindingRead{}, fmt.Errorf("opening the binding %s: %w", target.Database, err)
	}
	defer closeBeadStoreHandle(binding) //nolint:errcheck // read-only
	rows, err := binding.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return reverseBindingRead{}, fmt.Errorf("listing the binding: %w", err)
	}
	ids := make([]string, 0, len(rows))
	for _, b := range rows {
		ids = append(ids, b.ID)
	}
	sort.Strings(ids)
	lister, _ := beads.As[beads.DependencyBatchLister](binding)
	down, err := readInfraDownEdges(lister, binding, ids)
	if err != nil {
		return reverseBindingRead{}, err
	}
	single, _ := beads.As[beads.DepMetadataReader](binding)
	batch, _ := beads.As[beads.DepMetadataBatchReader](binding)
	payloads, err := readInfraPayloads(batch, single, down, func(beads.Dep) bool { return true })
	if err != nil {
		return reverseBindingRead{}, err
	}
	return reverseBindingRead{rows: rows, down: down, payloads: payloads}, nil
}

// plan reads the binding, the rig records and every destination, and plans
// the reverse. Every refusal the run can know before writing is in the plan.
func (r *reverseRun) plan() (reversePlan, error) {
	cityPath, cfg := r.request.CityPath, r.request.Cfg
	read, err := readReverseBinding(r.target)
	if err != nil {
		return reversePlan{}, err
	}
	in := reverseInput{
		Mode: r.opts.Mode, Rigs: setOf(r.journal.Rigs), Stamp: r.journal.Stamp,
		Binding: read.rows, Down: read.down, Payloads: read.payloads,
		Manifests: map[string]*infraCopyManifest{}, Configured: map[string]bool{},
		Held: map[string]map[string]beads.Bead{}, CarriesPayload: map[string]bool{},
		DerivesParent: map[string]bool{}, DropsRef: map[string]bool{},
		Fresh: r.journal.Phase == reverseStarted && len(r.journal.Dests) == 0,
	}
	// The fingerprint is of the binding the copies are made from. A resume
	// from R2 plans again but copies nothing, so it keeps the fingerprint its
	// copies were proven against, and the re-proofs compare the binding with
	// that one.
	if r.journal.Phase < reverseR2 {
		r.journal.BindingSHA256 = read.fingerprint()
	}
	var refusals []rigImportRefusal
	dests := []string{reverseCityDest}
	for _, rig := range reverseRigs(cityPath, cfg) {
		if !rigNameIsFileSafe(rig.Name) {
			continue
		}
		in.Configured[rig.Name] = true
		dests = append(dests, rigImportStamp(rig.Name))
		loc := newRigImportLocation(r.target, cityPath, rig.Name)
		manifest, _, err := loc.readManifest()
		if err != nil {
			return reversePlan{}, err
		}
		in.Manifests[rig.Name] = manifest
		note, notePresent, err := readInfraClearedNoteAt(loc.NotePath())
		if err != nil {
			return reversePlan{}, err
		}
		if fileExists(loc.InFlightPath()) || (notePresent && !note.Complete) {
			refusals = append(refusals, rigImportRefusal{IDs: []string{rig.Name}, Reason: "the rig's import has not finished, so its rows may be in both stores", Remedy: "finish it with `" + rigImportInstruction(rig.Name) + "`, then re-run"})
		}
	}
	targets := map[string]*reverseTarget{}
	defer func() {
		for _, t := range targets {
			t.close()
		}
	}()
	for _, dest := range dests {
		t, err := reverseTargetOpener(cityPath, cfg, dest)
		if err != nil {
			return reversePlan{}, err
		}
		targets[dest] = t
		rows, err := t.rows.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
		if err != nil {
			return reversePlan{}, fmt.Errorf("listing %s: %w", reverseDestNoun(dest), err)
		}
		held := make(map[string]beads.Bead, len(rows))
		for _, b := range rows {
			held[b.ID] = b
		}
		in.Held[dest], in.CarriesPayload[dest] = held, t.carriesPayload
		in.DerivesParent[dest], in.DropsRef[dest] = t.form != nil, t.dropsRef()
	}
	plan := planReverse(in)
	plan.BindingIDs = make(map[string]bool, len(read.rows))
	for _, b := range read.rows {
		plan.BindingIDs[b.ID] = true
	}
	// The edges each destination holds onto a planned row, recorded once,
	// before the reverse first writes: after that the store holds the
	// reverse's own edges too, and nothing tells them apart.
	var payloadKept []string
	if !r.journal.PreservedRecorded {
		r.journal.PreservedRecorded = true
		r.journal.Preserved = map[string][]reverseEdge{}
		for _, dest := range sortedMapKeys(plan.Rows) {
			planned := map[string]bool{}
			for _, b := range plan.Rows[dest] {
				planned[b.ID] = true
			}
			edges, err := reverseEdgesOnto(targets[dest], planned)
			if err != nil {
				return reversePlan{}, err
			}
			if len(edges) > 0 {
				r.journal.Preserved[dest] = edges
			}
			for _, e := range edges {
				if e.Payload != "" && !targets[dest].carriesPayload {
					payloadKept = append(payloadKept, fmt.Sprintf("%s -[%s]-> %s (%s)", e.From, e.Type, e.To, reverseDestNoun(dest)))
				}
			}
		}
	}
	dests = r.journal.Dests
	for dest := range plan.Rows {
		dests = append(dests, dest)
	}
	for dest := range plan.Edges {
		dests = append(dests, dest)
	}
	for dest := range r.journal.Preserved {
		dests = append(dests, dest)
	}
	r.journal.Dests = sortedUnique(dests)
	if note, present, err := readInfraClearedNote(cityPath); err != nil {
		return reversePlan{}, err
	} else if present && !note.Complete {
		// The work store still holds copies of rows the binding holds: one
		// refusal for the city, not one per row.
		plan.Refusals = []rigImportRefusal{{IDs: []string{"the city work store"}, Reason: fmt.Sprintf("the city's clear of its retained copies has not finished (%s is pending), so the work store holds copies of rows the binding holds", infraClearedNotePath(cityPath)), Remedy: "finish it with `" + storageClearInstruction() + "`, then re-run"}}
		return plan, nil
	}
	// The split's leftovers in the stores the reverse gives every class back
	// come first, as the pending clear does: the plan's own refusal of a
	// leftover under an id it writes (one held "as another bead") names the
	// wrong fix.
	leftovers, err := r.storeLeftovers(in, &plan)
	if err != nil {
		return reversePlan{}, err
	}
	if len(leftovers) > 0 {
		plan.Refusals = leftovers
		return plan, nil
	}
	for _, dest := range sortedMapKeys(plan.Rows) {
		if t := targets[dest]; t != nil && t.unsupported != "" && len(plan.Rows[dest]) > 0 {
			refusals = append(refusals, rigImportRefusal{IDs: []string{reverseDestNoun(dest)}, Reason: t.unsupported, Remedy: reverseKeepSplitRemedy})
		}
	}
	if r.opts.Mode == reverseToWork && r.journal.CityTOMLBefore == nil {
		// R4's edit, composed in memory now: a city whose [storage] a fragment
		// declares, or whose edited config would not load, is refused before
		// the copy, not at R4.
		edited, _, err := reverseCityTOML(cityPath, r.opts.Reconciler)
		if err == nil {
			err = reverseCheckCityTOML(cityPath, edited, r.opts.Reconciler)
		}
		if err != nil {
			refusals = append(refusals, rigImportRefusal{IDs: []string{"city.toml"}, Reason: err.Error(), Remedy: "remove [storage] by hand where it is declared, then re-run"})
		}
	}
	if r.opts.Mode == reverseToWork {
		refusals = append(refusals, reverseArchiveRefusals(cityPath, cfg, r.target)...)
	}
	if len(payloadKept) > 0 {
		refusals = append(refusals, rigImportRefusal{IDs: payloadKept, Reason: "each edge carries a payload and is held onto a row the reverse writes, in a store that cannot write a payload: a resume or an abort that has to put it back would lose its payload", Remedy: "keep the city split, or remove the edge once it is no longer needed; nothing was written"})
	}
	plan.Refusals = append(refusals, plan.Refusals...)
	return plan, nil
}

// storeLeftovers is the containment check of every store the reverse gives
// every class back: the work store on a --to-work (workStoreLeftovers), and
// each rig it returns (rigStoreLeftovers).
func (r *reverseRun) storeLeftovers(in reverseInput, plan *reversePlan) ([]rigImportRefusal, error) {
	var refusals []rigImportRefusal
	if r.opts.Mode == reverseToWork {
		city, err := r.workStoreLeftovers(in.Held[reverseCityDest], plan)
		if err != nil {
			return nil, err
		}
		refusals = city
	}
	return append(refusals, r.rigStoreLeftovers(in, plan)...), nil
}

// heldRemedy is remedy, a fix that runs another storage verb, as this run
// names it: a resumed reverse that can still be abandoned holds every such
// verb (storageReverseRefusal), so the abort comes first.
func (r *reverseRun) heldRemedy(remedy string) string {
	if !r.resumed || r.journal.ClearStarted {
		return remedy
	}
	return fmt.Sprintf("a fix here that runs a `gc storage` command needs this reverse abandoned first (`%s`), as each refuses while it is open, and the reverse started again once the rows are settled; %s", storageAbortReverseInstruction(), remedy)
}

// reverseStrands sorts the stranded rows of one store the reverse gives
// every class back, without this run's own, as the reverse settles them.
// Refused, each labeled with its type and status: the ids under which the
// binding holds a different bead (twice), and the open rows the store would
// serve again (reviving). Left there as they are: the rest (left).
type reverseStrands struct {
	twice, reviving []string
	left            reverseLeftovers
}

// add files the stranded row id: b as the plan read it (known is false when
// it read none), twice when the binding holds a different bead under id, and
// asWork the metadata keys alone that make it infrastructure, nil when they
// do not.
func (s *reverseStrands) add(id string, b beads.Bead, known, twice bool, asWork []string) {
	label := id
	if known {
		label = fmt.Sprintf("%s (%s, %s)", id, b.Type, b.Status)
	}
	switch {
	case twice:
		s.twice = append(s.twice, label)
	case asWork != nil:
		if s.left.AsWork == nil {
			s.left.AsWork = map[string][]string{}
		}
		s.left.AsWork[id] = asWork
	case known && !rigRowOpen(b):
		s.left.Closed = append(s.left.Closed, id)
	default:
		s.reviving = append(s.reviving, label)
	}
}

// leave records in plan the rows the reverse leaves in dest.
func (s reverseStrands) leave(plan *reversePlan, dest string) {
	if len(s.left.AsWork) == 0 && len(s.left.Closed) == 0 && len(s.left.Unread) == 0 {
		return
	}
	if plan.Leftovers == nil {
		plan.Leftovers = map[string]reverseLeftovers{}
	}
	plan.Leftovers[dest] = s.left
}

// workStoreLeftovers is a --to-work's containment check: boot's own
// classification (classifyInfraContainmentGap) of the work store's
// infrastructure rows against the forward copy's manifest, without the rows
// this run wrote (they carry its stamp). Once the split is retired the work
// store serves every class, so a row the binding does not serve is no longer
// hidden from its class readers, as it is while a stranded city serves
// degraded:
//
//   - an open stranded row would come back to life: an open message is
//     delivered, an open session or wait runs again. It is refused and
//     named, with the ways to settle it.
//   - a stranded id the binding holds for a different bead would name two
//     beads once the binding's row is copied back. It is refused with
//     recover-stranded's per-id choices.
//   - a retained copy of a row the binding owns sits under an id the plan
//     writes. One refusal names the clear, as boot does, not one "held as
//     another bead" per row, whose hand delete skips the clear's backup and
//     edge detach.
//   - a row that is infrastructure class only because of metadata, and a
//     closed stranded row, stay in the work store as they are, where a
//     single-store city keeps them. The plan reports them.
//
// workRows is the work store's rows by id, as the plan read them. A binding
// that converged before its manifest was recorded cannot be checked, since
// nothing tells a stranded row from one the binding's GC collected: the plan
// says so and goes on, because the reverse is the one rollback that keeps
// every row the binding holds.
func (r *reverseRun) workStoreLeftovers(workRows map[string]beads.Bead, plan *reversePlan) ([]rigImportRefusal, error) {
	manifest, recorded, err := readInfraCopyManifestRecord(r.target)
	if err != nil {
		return nil, err
	}
	if !recorded {
		plan.StrandsUnchecked = true
		return nil, nil
	}
	// The rows this run wrote carry its stamp: they are left out before the
	// check, which would otherwise read the binding once for each.
	gap, err := classifyInfraContainmentGapLeaving(r.request.CityPath, r.target, manifest, func(b beads.Bead) bool {
		return b.Metadata[beadmeta.InfraReversedFromMetadataKey] == r.journal.Stamp
	})
	if err != nil {
		return nil, err
	}
	heldDifferent := setOf(gap.HeldDifferent)
	var strands reverseStrands
	stranded := len(gap.Stranded) > 0
	for _, id := range gap.Stranded {
		b, known := workRows[id]
		strands.add(id, b, known, heldDifferent[id], gap.Declassifiable[id])
	}
	strands.leave(plan, reverseCityDest)
	var refusals []rigImportRefusal
	if len(strands.reviving) > 0 {
		refusals = append(refusals, rigImportRefusal{
			IDs:    strands.reviving,
			Reason: "each is an open infrastructure row the work store holds that the binding does not serve (stranded): no class reader sees it while the city is split, and once the split is retired the work store serves it, so an open message would be delivered and an open session or wait would come back to life",
			Remedy: r.heldRemedy(fmt.Sprintf("close each one that must not come back with `bd close <id>`, run from the city directory (a closed one stays in the work store as it is); purge stranded mail nobody will read instead (rehearse it with `%s --%s message --dry-run`); or carry them on (`%s` copies every stranded row into the binding as it is, `%s` clears their work-store copies, and the reverse brings them back); then re-run",
				storageRecoveryCommand, strandedPurgeTypeFlag, storageRecoveryInstruction(), storageClearInstruction())),
		})
	}
	if len(strands.twice) > 0 {
		refusals = append(refusals, rigImportRefusal{
			IDs:    strands.twice,
			Reason: "the binding holds a different bead under each of these ids (the work store minted the id again after the copy carried the original), so copying the binding's row back would put two beads under one id",
			Remedy: r.heldRemedy(fmt.Sprintf("decide per id with `%s` and --%s <id> (the work store's row replaces the binding's) or --%s <id> (the work store's row is removed and the binding's stays), each backing up the row it removes first, then re-run",
				storageRecoveryInstruction(), strandedReplaceBindingRowFlag, strandedDropWorkRowFlag)),
		})
	}
	if len(gap.Retained) > 0 {
		refusal := reverseRetainedRefusal(gap.Retained, gap.Unrecorded, stranded)
		refusal.Remedy = r.heldRemedy(refusal.Remedy)
		refusals = append(refusals, refusal)
	}
	return refusals, nil
}

// rigStoreLeftovers is the containment check of each rig the reverse gives
// every class back (--to-work: every rig with a store of its own; --to-rigs:
// the rigs it names): the census's own classification (classifyRigRows) of
// the rig's rows against its import manifest, without the rows this run
// wrote. Readers of graph, orders and sessions rows skip the store of a rig
// proven relic-free (rigGraphRelicFree), so an infrastructure row a raw
// writer put there after the rig's import, or its clean record, is hidden
// from them, and the census calls the rig stranded. A --to-work makes the
// rig's store serve every class, and a --to-rigs gives the rig its relic legs
// back, so:
//
//   - an open stranded row of the graph, orders or sessions class would come
//     back to life. It is refused and named, with the rig's ways to settle it.
//   - an open stranded mail or nudge row stays in the rig as it is, and the
//     plan reports it: those classes are read only from the city's store
//     (reverseLeftovers.Unread), split or not, so nothing delivers it.
//   - a row of any class under an id the rig's import moved for a different
//     bead would share the id with that bead once the reverse returns it. It
//     is refused with the import's own fix for such a row.
//   - a row that is infrastructure class only because of metadata, and a
//     closed stranded row, stay in the rig as they are. The plan reports
//     them.
//
// A rig that is not relic-free hides nothing: its relic legs serve it.
func (r *reverseRun) rigStoreLeftovers(in reverseInput, plan *reversePlan) []rigImportRefusal {
	rigs := r.journal.Rigs
	if r.opts.Mode == reverseToWork {
		rigs = sortedMapKeys(in.Configured)
	}
	routes := &storageRoutes{rigMarkers: &rigGraphMarkers{bindingRoot: r.target.Root, cityPath: r.request.CityPath, rigs: reverseRigs(r.request.CityPath, r.request.Cfg)}}
	var refusals []rigImportRefusal
	for _, rig := range rigs {
		if !routes.rigGraphRelicFree(rig) {
			continue
		}
		dest := rigImportStamp(rig)
		held := in.Held[dest]
		classes := classifyRigRows(slices.Collect(maps.Values(held)), in.Manifests[rig])
		ours := func(id string) bool {
			return held[id].Metadata[beadmeta.InfraReversedFromMetadataKey] == r.journal.Stamp
		}
		twice := map[string]bool{}
		var strands reverseStrands
		for _, id := range classes.Reused {
			if !ours(id) && plan.BindingIDs[id] {
				twice[id] = true
				strands.add(id, held[id], true, true, nil)
			}
		}
		for _, id := range classes.Stranded {
			if ours(id) || twice[id] {
				continue
			}
			asWork, _ := coordclass.ClassMetadataOnly(held[id])
			if asWork == nil && rigRowOpen(held[id]) && reverseUnreadInARig(held[id]) {
				strands.left.Unread = append(strands.left.Unread, id)
				continue
			}
			strands.add(id, held[id], true, false, asWork)
		}
		strands.leave(plan, dest)
		refusals = append(refusals, r.rigStrandRefusals(rig, strands)...)
	}
	return refusals
}

// reverseUnreadInARig reports whether b is a row of a class nothing reads from
// a rig's store (reverseLeftovers.Unread): mail and nudges.
func reverseUnreadInARig(b beads.Bead) bool {
	switch coordclass.Classify(b) {
	case coordclass.ClassMessaging, coordclass.ClassNudges:
		return true
	}
	return false
}

// rigStrandRefusals are the refusals of the stranded rows rig's store holds
// (rigStoreLeftovers).
func (r *reverseRun) rigStrandRefusals(rig string, strands reverseStrands) []rigImportRefusal {
	var refusals []rigImportRefusal
	if len(strands.reviving) > 0 {
		served := "once the split is retired its store serves every class"
		if r.opts.Mode == reverseToRigs {
			served = "once its import is returned its relic legs serve it again"
		}
		configured, _ := rigImportRigNamed(r.request.Cfg, rig)
		refusals = append(refusals, rigImportRefusal{
			IDs:    strands.reviving,
			Reason: fmt.Sprintf("each is an open infrastructure row rig %s holds that its import never carried (stranded): no class reader sees it while the rig is relic-free, and %s, so an open session or wait would come back to life and an open workflow or order row would run again", rig, served),
			Remedy: r.heldRemedy(fmt.Sprintf("close each one that must not come back with `bd close <id>`, run from the rig's directory %s (a closed one stays in the rig as it is); or carry them on (`%s` copies them into the binding, and the reverse brings them back with the rig's other rows); then re-run",
				rigStoreRoot(r.request.CityPath, configured), rigImportInstruction(rig))),
		})
	}
	if len(strands.twice) > 0 {
		refusals = append(refusals, rigImportRefusal{
			IDs:    strands.twice,
			Reason: fmt.Sprintf("rig %s minted each of these ids again after its import moved the bead that first held it, and the reverse returns that bead under its id, so the two would share one id", rig),
			Remedy: rigReusedIDRemedy,
		})
	}
	return refusals
}

// reverseRetainedRefusal is the one refusal of the work store's retained
// copies. It names the clear, boot's remedy (infraMigrationOperatorAdvice),
// with recover-stranded first for the copies the manifest does not record,
// which the clear refuses. stranded is whether the work store also holds a
// stranded row, which the clear refuses while it is there.
func reverseRetainedRefusal(retained, unrecorded []string, stranded bool) rigImportRefusal {
	remedy := fmt.Sprintf("clear them with `%s`, which backs up and proves each before it removes it, then re-run", storageClearInstruction())
	if len(unrecorded) > 0 {
		remedy = fmt.Sprintf("the copy manifest does not record %d of them (%s), and the clear removes only what it records: record them with `%s`, then clear them all with `%s`, which backs up and proves each before it removes it, then re-run",
			len(unrecorded), infraStrandedIDList(unrecorded), storageRecoveryInstruction(), storageClearInstruction())
	}
	if stranded {
		remedy = fmt.Sprintf("the clear refuses while the work store holds a stranded row, so settle those first (`%s` names each with its repair); then %s", storageStatusInstruction(), remedy)
	}
	return rigImportRefusal{
		IDs:    []string{reverseDestNoun(reverseCityDest)},
		Reason: fmt.Sprintf("it still holds %d retained pre-migration cop(ies) of rows the binding owns (%s), under the ids the reverse writes the binding's rows back to", len(retained), infraStrandedIDList(retained)),
		Remedy: remedy,
	}
}

// report prints what the plan moves.
func (r *reverseRun) report(plan reversePlan) {
	for _, dest := range sortedMapKeys(plan.Rows) {
		fmt.Fprintf(r.stdout, "%s: %d row(s) and %d edge(s) back into %s\n", r.logPrefix, len(plan.Rows[dest]), len(plan.Edges[dest]), reverseDestNoun(dest)) //nolint:errcheck // best-effort stdout
	}
	if plan.Restored > 0 {
		fmt.Fprintf(r.stdout, "%s: %d recorded cross-store edge(s) restored\n", r.logPrefix, plan.Restored) //nolint:errcheck // best-effort stdout
	}
	if plan.RefsKept > 0 {
		fmt.Fprintf(r.stdout, "%s: %d row(s) carry a step ref their store does not hold; each is kept in %s\n", r.logPrefix, plan.RefsKept, beadmeta.StepRefMetadataKey) //nolint:errcheck // best-effort stdout
	}
	if plan.RefsDropped > 0 {
		fmt.Fprintf(r.stdout, "%s: %d workflow root(s) carry their formula's name as a ref, which their store does not hold; it is not carried\n", r.logPrefix, plan.RefsDropped) //nolint:errcheck // best-effort stdout
	}
	if plan.Gates > 0 {
		fmt.Fprintf(r.stdout, "%s: %d closed cross-store gate(s) not carried (each row that waited on one gets the edge it stood for, row -> blocker, where the two land in one store; where an operator closed the gate and that edge would hold an open row again, the reverse refuses)\n", r.logPrefix, plan.Gates) //nolint:errcheck // best-effort stdout
	}
	for _, d := range plan.Dropped {
		fmt.Fprintf(r.stdout, "%s: edge not carried: %s\n", r.logPrefix, d) //nolint:errcheck // best-effort stdout
	}
	r.reportLeftovers(plan)
}

// reportLeftovers prints the rows the binding does not serve that the reverse
// leaves as they are in the stores it gives every class back
// (storeLeftovers), and a check that could not run.
func (r *reverseRun) reportLeftovers(plan reversePlan) {
	for _, dest := range sortedMapKeys(plan.Leftovers) {
		left, where := plan.Leftovers[dest], reverseDestNoun(dest)
		if len(left.AsWork) > 0 {
			keys := map[string]bool{}
			for _, ks := range left.AsWork {
				for _, k := range ks {
					keys[k] = true
				}
			}
			fmt.Fprintf(r.stdout, "%s: %d row(s) %s holds that the binding does not serve are infrastructure class only because of metadata (%s); they stay there as the work they are: %s\n", r.logPrefix, len(left.AsWork), where, strings.Join(sortedSetKeys(keys), ", "), infraStrandedIDList(sortedMapKeys(left.AsWork))) //nolint:errcheck // best-effort stdout
		}
		if len(left.Closed) > 0 {
			fmt.Fprintf(r.stdout, "%s: %d closed row(s) %s holds that the binding does not serve (stranded) stay there as they are: %s\n", r.logPrefix, len(left.Closed), where, infraStrandedIDList(left.Closed)) //nolint:errcheck // best-effort stdout
		}
		if len(left.Unread) > 0 {
			fmt.Fprintf(r.stdout, "%s: %d open mail or nudge row(s) %s holds that the binding does not serve (stranded) stay there as they are: mail and nudges are read only from the city's store, so nothing delivers them from there: %s\n", r.logPrefix, len(left.Unread), where, infraStrandedIDList(left.Unread)) //nolint:errcheck // best-effort stdout
		}
	}
	if plan.StrandsUnchecked {
		fmt.Fprintf(r.stdout, "%s: %s converged before %s was recorded, so a row %s holds that the binding does not serve (a stranded write) cannot be told from one the binding collected, and none is checked: once the split is retired, every row there is served\n", r.logPrefix, r.target.Database, r.target.ManifestPath(), reverseDestNoun(reverseCityDest)) //nolint:errcheck // best-effort stdout
	}
}

// advance records phase done in the journal, then passes its crash seam.
func (r *reverseRun) advance(phase int) error {
	r.journal.Phase = phase
	if err := writeStorageReverseJournal(r.request.CityPath, r.journal); err != nil {
		return err
	}
	return infraEngineCrashPoint("reverse phase " + reversePhaseName(phase))
}

// phases runs every phase the journal has not recorded done.
func (r *reverseRun) phases(ctx context.Context) error {
	cityPath := r.request.CityPath
	if r.journal.Phase == reverseStarted {
		if err := writeStorageReverseJournal(cityPath, r.journal); err != nil {
			return err
		}
		if err := infraEngineCrashPoint("reverse R0 journal"); err != nil {
			return err
		}
		if err := r.snapshot(ctx); err != nil {
			return err
		}
		marker := filepath.Join(r.target.Root, infraRevertingRecordName)
		line := fmt.Sprintf("reverse migration --%s %s under way since %s; journal %s", r.journal.Mode, r.journal.Stamp, r.journal.StartedAt.Format(time.RFC3339), storageReverseJournalPath(cityPath))
		if err := writeRigImportRecord(marker, line); err != nil {
			return err
		}
		if err := r.advance(reverseR0); err != nil {
			return err
		}
	}
	clock := infraEngineClockFor(cityPath)
	if r.journal.Phase < reverseR2 {
		// Every store the plan writes, and every store an earlier run of
		// this reverse wrote: a store the plan no longer writes may still
		// hold rows under the stamp, which its prepare judges.
		for _, dest := range r.copyDests() {
			if dest == reverseCityDest {
				continue
			}
			if err := r.copyInto(dest, clock); err != nil {
				return err
			}
		}
		if r.journal.Phase < reverseR1 {
			if err := r.advance(reverseR1); err != nil {
				return err
			}
		}
		if r.opts.Mode == reverseToWork {
			if err := r.copyInto(reverseCityDest, clock); err != nil {
				return err
			}
		}
		if err := r.advance(reverseR2); err != nil {
			return err
		}
	}
	if r.journal.Phase < reverseR3 {
		if err := r.reproveBinding("the reverse manifest (R3)"); err != nil {
			return err
		}
		if err := r.writeManifests(); err != nil {
			return err
		}
		if err := r.advance(reverseR3); err != nil {
			return err
		}
	}
	if r.journal.Phase < reverseR4 {
		if err := r.destinationsHold("the split's records change (R4)"); err != nil {
			return err
		}
		var err error
		if r.opts.Mode == reverseToRigs {
			if !r.journal.ClearStarted {
				if err := r.reproveBinding("the binding clear (R4)"); err != nil {
					return err
				}
			}
			err = r.clearRigRowsFromBinding()
		} else {
			err = r.retireSplit()
		}
		if err != nil {
			return err
		}
		if err := r.advance(reverseR4); err != nil {
			return err
		}
	}
	if r.opts.Mode == reverseToWork && fileExists(r.target.Root) {
		if err := r.reproveBinding("the archive (R5)"); err != nil {
			return err
		}
		if err := r.destinationsHold("the archive (R5)"); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(r.target.Root, infraRevertingRecordName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if r.opts.Mode == reverseToWork {
		if err := infraEngineCrashPoint("reverse R5 record removed"); err != nil {
			return err
		}
		archive := r.target.Root + r.journal.suffix()
		if _, err := os.Stat(r.target.Root); err == nil {
			if err := os.Rename(r.target.Root, archive); err != nil {
				return fmt.Errorf("archiving the binding root %s: %w", r.target.Root, err)
			}
			syncInfraDir(filepath.Dir(r.target.Root))
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := infraEngineCrashPoint("reverse R5 archived"); err != nil {
			return err
		}
	}
	return r.advance(reverseComplete)
}

// snapshot is R0's mandatory snapshot, pinned.
func (r *reverseRun) snapshot(ctx context.Context) error {
	opts := snapshotOptions{Binding: true, Work: r.opts.Mode == reverseToWork, Rigs: true, To: storageSnapshotDefaultDir(r.request.CityPath), Keep: storageSnapshotDefaultKeep, NoRetention: true}
	identity, err := currentWorkStoreIdentity(r.request.CityPath)
	if err != nil {
		return err
	}
	if err := snapshotInFlightRefusal(r.target); err != nil {
		return err
	}
	id, _, err := takeStorageSnapshot(ctx, r.request, opts, r.target, identity, r.stdout, r.stderr, r.logPrefix)
	if err != nil {
		return fmt.Errorf("the mandatory snapshot: %w", err)
	}
	reason := fmt.Sprintf("the way back from the reverse migration %s; --abort-reverse unpins it", r.journal.Stamp)
	if err := pinStorageSnapshot(opts.To, id, reason); err != nil {
		return fmt.Errorf("pinning the mandatory snapshot %s: %w", id, err)
	}
	r.journal.SnapshotTo, r.journal.Snapshot = opts.To, id
	return nil
}

// copyInto copies the plan's rows and edges for dest and proves them, then
// raises the store's id floor past them.
func (r *reverseRun) copyInto(dest string, clock infraEngineClock) error {
	rows, edges := r.planned.Rows[dest], r.planned.Edges[dest]
	// The edges the store held onto a planned row before the reverse go back
	// with the row: a resume that deletes and re-copies the row takes them
	// with it (reverseDeleteRows).
	planned := map[beads.DepPair]bool{}
	for _, e := range edges {
		planned[beads.DepPair{IssueID: e.IssueID, DependsOnID: e.DependsOnID}] = true
	}
	for _, e := range r.journal.Preserved[dest] {
		if !planned[beads.DepPair{IssueID: e.From, DependsOnID: e.To}] {
			edges = append(edges, e.dep())
		}
	}
	open := func() (*reverseTarget, error) { return reverseTargetOpener(r.request.CityPath, r.request.Cfg, dest) }
	t, err := open()
	if err != nil {
		return err
	}
	defer t.close()
	copied, err := reverseCopyInto(t, open, rows, edges, r.planned.BindingIDs, r.journal.Stamp, clock)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.stderr, "%s: %s holds its %d row(s), proven (%d copied by this run)\n", r.logPrefix, reverseDestNoun(dest), len(rows), copied) //nolint:errcheck // best-effort stderr
	ids := make([]string, 0, len(rows))
	for _, b := range rows {
		ids = append(ids, b.ID)
	}
	return r.reserveIDs(dest, ids)
}

// copyDests is every store the plan writes and every store the journal
// records this reverse writing.
func (r *reverseRun) copyDests() []string {
	dests := append([]string(nil), r.journal.Dests...)
	for dest := range r.planned.Rows {
		dests = append(dests, dest)
	}
	for dest := range r.planned.Edges {
		dests = append(dests, dest)
	}
	return sortedUnique(dests)
}

// reserveIDs raises a destination's bd id floor past the ids the reverse put
// back (S5-D's floor; idempotent, never lowered).
func (r *reverseRun) reserveIDs(dest string, ids []string) error {
	if dest == reverseCityDest {
		floor, err := reserveCityIDs(r.request.CityPath, ids)
		if err != nil {
			return err
		}
		if floor.Floor > 0 || floor.Skipped != "" {
			fmt.Fprintf(r.stderr, "%s: %s\n", r.logPrefix, floor) //nolint:errcheck // best-effort stderr
		}
		return nil
	}
	rig, _ := rigImportRigNamed(r.request.Cfg, strings.TrimPrefix(dest, rigImportStamp("")))
	conf, err := rigImportHashConfigOpener(r.request.CityPath, r.request.Cfg, rig)
	if err != nil {
		return nil //nolint:nilerr // a rig whose minting this build cannot configure keeps no floor, as its import did
	}
	_, _, err = raiseBdHashFloor(conf, rigImportHashFloor(ids))
	return err
}

// writeManifests is R3: the reverse manifest of each run, (id, created_at),
// re-read and proven against the rows written.
func (r *reverseRun) writeManifests() error {
	byManifest := map[string][]beads.Bead{}
	for dest, rows := range r.planned.Rows {
		name := infraReversedManifestName
		if r.opts.Mode == reverseToRigs {
			name = infraReversedRigManifestName(strings.TrimPrefix(dest, rigImportStamp("")))
		}
		byManifest[name] = append(byManifest[name], rows...)
	}
	for _, name := range r.manifestNames() {
		if byManifest[name] == nil {
			byManifest[name] = []beads.Bead{}
		}
	}
	now := time.Now().UTC()
	for _, name := range sortedMapKeys(byManifest) {
		rows := byManifest[name]
		ids := make([]string, 0, len(rows))
		created := make(map[string]time.Time, len(rows))
		for _, b := range rows {
			ids = append(ids, b.ID)
			created[b.ID] = b.CreatedAt
		}
		sort.Strings(ids)
		path := filepath.Join(r.target.Root, name)
		extra := []string{"#gc:reversed " + r.journal.Stamp}
		if err := writeInfraCopyManifestFileAt(r.target.Root, path, ids, created, now, now, coordclass.CurrentGeneration, extra); err != nil {
			return err
		}
		got, _, err := readInfraCopyManifestRecordAt(path, "")
		if err != nil {
			return err
		}
		if diff := stringSetDifference("reversed id", ids, sortedMapKeys(got.IDs)); diff != "" {
			return fmt.Errorf("the reverse manifest %s does not read back: %s", path, diff)
		}
	}
	return nil
}

// clearRigRowsFromBinding is a to-rigs R4: with the rows proven in the rig,
// the binding's copies go, and with them the closed gates that go with them
// (planReverse): those the rig's import minted, and those the runtime minted
// for a row the reverse returned (by owner, as they carry no import stamp),
// but not one a row the binding keeps still waits on, which stays with it;
// each edge to a row the binding keeps goes first. Then the rig's import
// records are renamed so it reads as awaiting import. Every binding row
// stamped as the rig's import must be one the reverse manifest records or a
// closed gate, and every gate a returned row owns must be closed, or nothing
// is removed.
func (r *reverseRun) clearRigRowsFromBinding() error {
	binding, err := openInfraDestination(r.target)
	if err != nil {
		return err
	}
	defer closeBeadStoreHandle(binding) //nolint:errcheck // best-effort close
	next := fmt.Sprintf("Abandon the reverse with `%s` and start again", storageAbortReverseInstruction())
	if r.journal.ClearStarted {
		next = fmt.Sprintf("The clear has begun, so the reverse cannot be abandoned: return the row to the rig by hand, or restore from snapshot %s in %s", r.journal.Snapshot, r.journal.SnapshotTo)
	}
	var going []string
	gates := map[string]bool{}
	returned := map[string]string{} // each row the reverse returned, to its rig
	for _, rig := range r.journal.Rigs {
		m, _, err := readInfraCopyManifestRecordAt(filepath.Join(r.target.Root, infraReversedRigManifestName(rig)), "")
		if err != nil {
			return err
		}
		for id := range m.IDs {
			returned[id] = rig
		}
		stamped, err := binding.List(beads.ListQuery{Metadata: map[string]string{infraMigrationStampKey: rigImportStamp(rig)}, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
		if err != nil {
			return err
		}
		for _, b := range stamped {
			if !m.IDs[b.ID] && (!isCrossStoreGate(b) || rigRowOpen(b)) {
				return fmt.Errorf("the binding holds %s, stamped as an import of rig %s, which this reverse did not return to the rig; nothing was removed. %s", b.ID, rig, next)
			}
			going = append(going, b.ID)
			if isCrossStoreGate(b) {
				gates[b.ID] = true
			}
		}
	}
	lister, _ := beads.As[beads.DependencyBatchLister](binding)
	single, _ := beads.As[beads.DepMetadataReader](binding)
	batch, _ := beads.As[beads.DepMetadataBatchReader](binding)
	t := &reverseTarget{
		dest:      reverseClearBinding,
		rows:      binding,
		down:      func(ids []string) (map[string][]beads.Dep, error) { return readInfraDownEdges(lister, binding, ids) },
		deleteIDs: func(ids []string) error { return deleteInBatches(binding, ids) },
		payloads: func(edges map[string][]beads.Dep) (map[beads.DepPair]string, error) {
			return readInfraPayloads(batch, single, edges, func(beads.Dep) bool { return true })
		},
	}
	owned, err := returnedRowGates(t, returned, gates, next)
	if err != nil {
		return err
	}
	for _, id := range owned {
		going = append(going, id)
		gates[id] = true
	}
	waited, err := reverseWaitedGates(t, setOf(going), gates)
	if err != nil {
		return err
	}
	going = slices.DeleteFunc(going, func(id string) bool { return waited[id] })
	if !r.journal.ClearStarted {
		edges, err := r.clearEdges(t, setOf(going), gates)
		if err != nil {
			return err
		}
		r.journal.ClearEdges = edges
	}
	r.journal.ClearStarted = true
	if err := writeStorageReverseJournal(r.request.CityPath, r.journal); err != nil {
		return err
	}
	if err := infraEngineCrashPoint("reverse R4 clear recorded"); err != nil {
		return err
	}
	if err := reverseDeleteRows(t, going); err != nil {
		return err
	}
	if err := infraEngineCrashPoint("reverse R4 cleared"); err != nil {
		return err
	}
	var renames []reverseRename
	for _, rig := range r.journal.Rigs {
		loc := newRigImportLocation(r.target, r.request.CityPath, rig)
		records := []string{loc.MarkerPath(), loc.ManifestPath(), loc.CleanPath(), loc.BackupPath(), loc.InFlightPath(), loc.NotePath()}
		renames = append(renames, reverseRenames(records, r.journal.suffix())...)
	}
	r.journal.Renamed = append(r.journal.Renamed, renames...)
	if err := writeStorageReverseJournal(r.request.CityPath, r.journal); err != nil {
		return err
	}
	if err := applyReverseRenames(r.journal.Renamed); err != nil {
		return err
	}
	return infraEngineCrashPoint("reverse R4 renamed")
}

// returnedRowGates is each cross-store gate the binding (t) holds whose owner
// the reverse returned (returned: each row, to its rig) and that gates does
// not already name: one the runtime minted for that row, with no import
// stamp. Each must be closed: an open one refuses the clear, naming next.
func returnedRowGates(t *reverseTarget, returned map[string]string, gates map[string]bool, next string) ([]string, error) {
	minted, err := t.rows.List(beads.ListQuery{Label: dispatch.CrossStoreGateLabel, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, b := range minted {
		owner := b.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey]
		rig, owned := returned[owner]
		switch {
		case !owned || gates[b.ID]:
		case rigRowOpen(b):
			return nil, fmt.Errorf("the binding holds %s, an open cross-store gate for %s, which this reverse returned to rig %s; nothing was removed. %s", b.ID, owner, rig, next)
		default:
			out = append(out, b.ID)
		}
	}
	return out, nil
}

// reverseWaitedGates is each of gates, the closed gates the clear judges,
// that a row the binding keeps (one going does not name) still waits on: it
// stays in the binding with that row, by the plan's rule (reverseKeptGates).
func reverseWaitedGates(t *reverseTarget, going, gates map[string]bool) (map[string]bool, error) {
	all, err := t.rows.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", reverseDestNoun(t.dest), err)
	}
	var kept []string
	for _, b := range all {
		if !going[b.ID] {
			kept = append(kept, b.ID)
		}
	}
	down, err := t.down(kept)
	if err != nil {
		return nil, err
	}
	return reverseKeptGates(down, gates, func(id string) bool { return !going[id] }), nil
}

// reverseClearBinding keys the binding's edges in ClearEdges.
const reverseClearBinding = "binding"

// clearEdges reads every edge that touches one of going, in the binding (t)
// and in each rig the reverse returns rows to. An edge onto one of the closed
// gates the clear judges, one it removes or one it keeps for a row the
// binding keeps, is left out: the gate is not carried, and the edge it stood
// for (row -> blocker) is the plan's to restore.
func (r *reverseRun) clearEdges(binding *reverseTarget, going, gates map[string]bool) (map[string][]reverseEdge, error) {
	out := map[string][]reverseEdge{}
	edges, err := reverseEdgesTouching(binding, going)
	if err != nil {
		return nil, err
	}
	for _, e := range edges {
		if !gates[e.From] && !gates[e.To] {
			out[reverseClearBinding] = append(out[reverseClearBinding], e)
		}
	}
	for _, rig := range r.journal.Rigs {
		dest := rigImportStamp(rig)
		t, err := reverseTargetOpener(r.request.CityPath, r.request.Cfg, dest)
		if err != nil {
			return nil, err
		}
		edges, err := reverseEdgesTouching(t, going)
		t.close()
		if err != nil {
			return nil, err
		}
		if len(edges) > 0 {
			out[dest] = edges
		}
	}
	return out, nil
}

// destinationsHold proves, before the reverse changes the binding or the
// split's records (R4) and before it archives the binding (R5), that the
// stores it wrote still hold every row its reverse manifests record, under
// this run's stamp: an abort killed partway, or a writer, can have removed
// some. Before a to-rigs clear has begun, a shortfall puts the reverse back
// to R0, and the next run copies again. Once it has begun the reverse never
// goes back: a row the binding still holds is copied back from it, and one
// gone from both is refused.
func (r *reverseRun) destinationsHold(before string) error {
	missing, err := r.missingReturned()
	if err != nil {
		return err
	}
	if r.journal.ClearStarted && len(missing)+len(r.journal.Recopying) > 0 {
		return r.recopyAfterClear(sortedUnique(append(missing, r.journal.Recopying...)))
	}
	if len(missing) == 0 {
		return nil
	}
	r.journal.Phase = reverseR0
	if err := writeStorageReverseJournal(r.request.CityPath, r.journal); err != nil {
		return err
	}
	return fmt.Errorf("the stores this reverse wrote no longer hold %d row(s) it returned (%s), checked before %s, so the binding and the split's records are left as they are; the next run copies them again", len(missing), infraStrandedIDList(missing), before)
}

// recopyAfterClear returns to its rig, once a to-rigs clear has begun, each
// returned row the rig no longer holds under the stamp that the binding still
// does, with its edges: its binding copy is the authority it was before the
// clear. A row gone from both is refused, nothing written, naming the stamp a
// row put back by hand must carry. The rows are recorded in the journal
// before anything is written, and the re-copy writes only what is absent, so
// a run that stops partway is finished by the next.
func (r *reverseRun) recopyAfterClear(missing []string) error {
	binding, err := openInfraBindingReadOnly(r.target)
	if err != nil {
		return err
	}
	all, err := binding.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	_ = closeBeadStoreHandle(binding)
	if err != nil {
		return err
	}
	inBinding := make(map[string]string, len(all))
	for _, b := range all {
		inBinding[b.ID] = b.Status
	}
	var gone []string
	for _, id := range missing {
		if _, held := inBinding[id]; !held {
			gone = append(gone, id)
		}
	}
	if len(gone) > 0 {
		return fmt.Errorf("neither the rigs nor the binding hold %d row(s) this reverse returned (%s): its binding clear removed them, and the rig has lost them since. Nothing more was changed. Put each back into its rig by hand carrying %s=%s (snapshot %s in %s, taken before the reverse, holds each as it was), then run this again", len(gone), infraStrandedIDList(gone), beadmeta.InfraReversedFromMetadataKey, r.journal.Stamp, r.journal.Snapshot, r.journal.SnapshotTo)
	}
	plan, err := r.plan()
	if err != nil {
		return err
	}
	if len(plan.Refusals) > 0 {
		reasons := make([]string, 0, len(plan.Refusals))
		for _, refusal := range plan.Refusals {
			reasons = append(reasons, refusal.String())
		}
		return fmt.Errorf("the rigs no longer hold %d row(s) this reverse returned (%s), and copying them back from the binding is refused: %s", len(missing), infraStrandedIDList(missing), strings.Join(reasons, "; "))
	}
	r.journal.Recopying = missing
	if err := writeStorageReverseJournal(r.request.CityPath, r.journal); err != nil {
		return err
	}
	lost := setOf(missing)
	found := map[string]bool{}
	for _, dest := range sortedMapKeys(plan.Rows) {
		var rows []beads.Bead
		for _, b := range plan.Rows[dest] {
			if lost[b.ID] {
				rows = append(rows, b)
				found[b.ID] = true
			}
		}
		var edges []beads.DepWithPayload
		for _, e := range plan.Edges[dest] {
			if lost[e.IssueID] || lost[e.DependsOnID] {
				edges = append(edges, e)
			}
		}
		var recorded []reverseEdge
		for _, store := range []string{dest, reverseClearBinding} {
			for _, e := range r.journal.ClearEdges[store] {
				if lost[e.From] || lost[e.To] {
					recorded = append(recorded, e)
				}
			}
		}
		if len(rows) > 0 {
			if err := r.recopyRows(dest, rows, edges, recorded, lost, inBinding); err != nil {
				return err
			}
		}
	}
	if len(found) != len(lost) {
		return fmt.Errorf("the rigs no longer hold %d row(s) this reverse returned (%s), and the binding no longer routes each to a rig; nothing more was changed", len(missing), infraStrandedIDList(missing))
	}
	if still, err := r.missingReturned(); err != nil || len(still) > 0 {
		return errors.Join(err, fmt.Errorf("the rigs still do not hold %s after copying them back", infraStrandedIDList(still)))
	}
	r.journal.Recopying = nil
	if err := writeStorageReverseJournal(r.request.CityPath, r.journal); err != nil {
		return err
	}
	fmt.Fprintf(r.stdout, "%s: %d row(s) a rig lost after the binding clear began were copied back from the binding\n", r.logPrefix, len(missing)) //nolint:errcheck // best-effort stdout
	return nil
}

// recopyRows writes each of rows dest does not hold as planned, adds each
// edge it does not hold, and proves them, among the rows it already holds
// under the stamp. It deletes only a row an earlier re-copy wrote that
// differs from the plan, to write it again. Each recorded edge (ClearEdges)
// joining a lost row to a row dest then holds is restored too; one whose
// other end only the binding holds crosses stores, and the row records it
// dropped; one whose other end neither holds is dropped and reported.
func (r *reverseRun) recopyRows(dest string, rows []beads.Bead, edges []beads.DepWithPayload, recorded []reverseEdge, lost map[string]bool, inBinding map[string]string) error {
	open := func() (*reverseTarget, error) {
		return reverseTargetOpener(r.request.CityPath, r.request.Cfg, dest)
	}
	t, err := open()
	if err != nil {
		return err
	}
	defer t.close()
	phase := "reverse " + dest + " re-copy"
	stamped, err := t.rows.List(reverseStampQuery(r.journal.Stamp))
	if err != nil {
		return err
	}
	all, err := t.rows.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return err
	}
	inDest := map[string]bool{}
	for _, b := range all {
		inDest[b.ID] = true
	}
	for _, b := range rows {
		inDest[b.ID] = true
	}
	planned := map[beads.DepPair]bool{}
	for _, e := range edges {
		planned[beads.DepPair{IssueID: e.IssueID, DependsOnID: e.DependsOnID}] = true
	}
	var dropped []string
	crossing := map[string][]rigCrossEdgeRecord{}
	for _, e := range recorded {
		pair := beads.DepPair{IssueID: e.From, DependsOnID: e.To}
		on, other, dir := e.From, e.To, "out"
		if !lost[e.From] {
			on, other, dir = e.To, e.From, "in"
		}
		status, otherInBinding := inBinding[other]
		switch {
		case planned[pair]:
		case inDest[e.From] && (inDest[e.To] || strings.HasPrefix(e.To, "external:")):
			planned[pair] = true
			edges = append(edges, beads.DepWithPayload{Dep: beads.Dep{IssueID: e.From, DependsOnID: e.To, Type: e.Type}, Payload: e.Payload})
		case otherInBinding:
			// A cross-store edge: the row records it dropped, as the plan
			// does while the binding still holds the edge (the clear's
			// first pass removes it before the rows).
			crossing[on] = append(crossing[on], rigCrossEdgeRecord{Dir: dir, Type: e.Type, Other: other, OtherStatus: status, Payload: e.Payload, Action: rigEdgeDropped})
		default:
			dropped = append(dropped, fmt.Sprintf("%s -[%s]-> %s", e.From, e.Type, e.To))
		}
	}
	for _, e := range edges {
		if e.Payload != "" && !t.carriesPayload {
			return fmt.Errorf("%s cannot write the payload on %s -> %s; nothing was written", reverseDestNoun(dest), e.IssueID, e.DependsOnID)
		}
	}
	if len(dropped) > 0 {
		sort.Strings(dropped)
		fmt.Fprintf(r.stdout, "%s: %d recorded edge(s) of a row copied back into %s not restored, their other end in neither store: %s\n", r.logPrefix, len(dropped), reverseDestNoun(dest), strings.Join(dropped, ", ")) //nolint:errcheck // best-effort stdout
	}
	for i := range rows {
		if add := crossing[rows[i].ID]; len(add) > 0 {
			merged, _, err := mergeCrossEdgeRecords(rows[i].Metadata[beadmeta.MigratedCrossEdgesMetadataKey], add)
			if err != nil {
				return fmt.Errorf("%s: %w", rows[i].ID, err)
			}
			metadata := maps.Clone(rows[i].Metadata)
			metadata[beadmeta.MigratedCrossEdgesMetadataKey] = merged
			rows[i].Metadata = metadata
		}
	}
	// A row an earlier re-copy wrote that differs from the plan is deleted
	// and written again: the binding holds it (recopyAfterClear proved
	// that), so the rig's copy is not the only one.
	held := map[string]beads.Bead{}
	for _, b := range stamped {
		if b.Metadata[beadmeta.InfraReversedFromMetadataKey] == r.journal.Stamp {
			held[b.ID] = b
		}
	}
	var create []beads.Bead
	var redo []string
	for _, b := range rows {
		got, ok := held[b.ID]
		switch {
		case !ok:
			create = append(create, b)
		case reverseRowDifference(t.form, b, got) != "":
			redo = append(redo, b.ID)
			create = append(create, b)
		}
	}
	if err := reverseDeleteRows(t, redo); err != nil {
		return err
	}
	if len(create) > 0 {
		if err := t.create(create); err != nil {
			return fmt.Errorf("copying back into %s: %w", reverseDestNoun(dest), err)
		}
	}
	if err := infraEngineCrashPoint(phase + " rows"); err != nil {
		return err
	}
	if err := reverseAddEdges(t, edges, infraEngineClock{}, phase); err != nil {
		return err
	}
	if err := reverseVerify(open, rows, edges, r.journal.Stamp, true); err != nil {
		return fmt.Errorf("verifying %s: %w", reverseDestNoun(dest), err)
	}
	return nil
}

// manifestNames are the reverse manifests R3 writes, each one even when the
// plan returns no row to it, and missingReturned reads back: one per rig for
// --to-rigs, the city's for --to-work.
func (r *reverseRun) manifestNames() []string {
	if r.opts.Mode != reverseToRigs {
		return []string{infraReversedManifestName}
	}
	names := make([]string, 0, len(r.journal.Rigs))
	for _, rig := range r.journal.Rigs {
		names = append(names, infraReversedRigManifestName(rig))
	}
	return names
}

// missingReturned is each row the reverse manifests record that no store the
// journal names holds under this run's stamp.
func (r *reverseRun) missingReturned() ([]string, error) {
	want := map[string]bool{}
	for _, name := range r.manifestNames() {
		path := filepath.Join(r.target.Root, name)
		m, recorded, err := readInfraCopyManifestRecordAt(path, "")
		if err != nil {
			return nil, err
		}
		if !recorded {
			return nil, fmt.Errorf("the reverse manifest %s is gone, so nothing says which rows this reverse wrote", path)
		}
		for id := range m.IDs {
			want[id] = true
		}
	}
	held := map[string]bool{}
	for _, dest := range r.journal.Dests {
		t, err := reverseTargetOpener(r.request.CityPath, r.request.Cfg, dest)
		if err != nil {
			return nil, err
		}
		stamped, err := t.rows.List(reverseStampQuery(r.journal.Stamp))
		t.close()
		if err != nil {
			return nil, err
		}
		for _, b := range stamped {
			if b.Metadata[beadmeta.InfraReversedFromMetadataKey] == r.journal.Stamp {
				held[b.ID] = true
			}
		}
	}
	var missing []string
	for _, id := range sortedMapKeys(want) {
		if !held[id] {
			missing = append(missing, id)
		}
	}
	return missing, nil
}

// reverseRenames pairs each existing path with its retired name.
func reverseRenames(paths []string, suffix string) []reverseRename {
	var out []reverseRename
	for _, p := range paths {
		if strings.Contains(filepath.Base(p), ".reverted-") || !fileExists(p) {
			continue
		}
		out = append(out, reverseRename{From: p, To: p + suffix})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].From < out[j].From })
	return out
}

// applyReverseRenames renames each record, durably; one already renamed is
// skipped.
func applyReverseRenames(renames []reverseRename) error {
	dirs := map[string]bool{}
	for _, rn := range renames {
		if !fileExists(rn.From) && fileExists(rn.To) {
			continue
		}
		if err := os.Rename(rn.From, rn.To); err != nil {
			return fmt.Errorf("retiring %s: %w", rn.From, err)
		}
		dirs[filepath.Dir(rn.To)] = true
	}
	for _, dir := range sortedMapKeys(dirs) {
		syncInfraDir(dir)
	}
	return nil
}

// reverseRigClearedNotes are the rigs' cleared notes in the city's .gc, read
// from the directory rather than matched by a pattern over the city path.
func reverseRigClearedNotes(cityPath string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(cityPath, ".gc"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() && strings.HasPrefix(name, "storage-infra-cleared.rig.") && strings.HasSuffix(name, ".json") {
			out = append(out, filepath.Join(cityPath, ".gc", name))
		}
	}
	return out, nil
}

// retireSplit is a to-work R4: city.toml without [storage], the split's
// records renamed, and the version floor removed. Everything it changes is
// recorded in the journal first, so --abort-reverse can put it back.
func (r *reverseRun) retireSplit() error {
	cityPath := r.request.CityPath
	tomlPath, err := reverseCityTOMLPath(cityPath)
	if err != nil {
		return err
	}
	current, err := os.ReadFile(tomlPath)
	if err != nil {
		return err
	}
	if r.journal.CityTOMLBefore == nil {
		edited, how, err := reverseCityTOML(cityPath, r.opts.Reconciler)
		if err != nil {
			return err
		}
		before := string(current)
		r.journal.CityTOMLBefore = &before
		r.journal.CityTOMLAfterSHA256 = sha256Hex(edited)
		if data, err := os.ReadFile(minGCVersionPath(cityPath)); err == nil {
			floor := string(data)
			r.journal.MinGCVersion = &floor
		}
		paths := []string{bornSplitServedNotePath(cityPath), infraClearedNotePath(cityPath)}
		rigNotes, err := reverseRigClearedNotes(cityPath)
		if err != nil {
			return err
		}
		paths = append(paths, rigNotes...)
		// The rigs' records beside the binding go with its root at R5.
		r.journal.Renamed = reverseRenames(paths, r.journal.suffix())
		if err := writeStorageReverseJournal(cityPath, r.journal); err != nil {
			return err
		}
		fmt.Fprintf(r.stderr, "%s: city.toml: %s\n", r.logPrefix, how) //nolint:errcheck // best-effort stderr
	}
	switch sha256Hex(current) {
	case r.journal.CityTOMLAfterSHA256:
	case sha256Hex([]byte(*r.journal.CityTOMLBefore)):
		edited, _, err := reverseCityTOML(cityPath, r.opts.Reconciler)
		if err != nil {
			return err
		}
		if sha256Hex(edited) != r.journal.CityTOMLAfterSHA256 {
			return fmt.Errorf("city.toml's edit no longer computes to the one this reverse recorded; abandon it with `%s` and start again", storageAbortReverseInstruction())
		}
		if err := reverseCheckCityTOML(cityPath, edited, r.opts.Reconciler); err != nil {
			return err
		}
		if err := writeCityTOMLAtomic(tomlPath, edited); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%s changed while the reverse was under way, so this run cannot tell its edit from yours; put it back (the journal %s holds the earlier file as city_toml_before), or abandon the reverse with `%s`, which keeps your change once the file declares the split's [storage]", tomlPath, storageReverseJournalPath(cityPath), storageAbortReverseInstruction())
	}
	if err := infraEngineCrashPoint("reverse R4 config"); err != nil {
		return err
	}
	if err := applyReverseRenames(r.journal.Renamed); err != nil {
		return err
	}
	if err := os.Remove(minGCVersionPath(cityPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// reverseCityTOMLPath is the city.toml R4 edits, through a symlink.
func reverseCityTOMLPath(cityPath string) (string, error) {
	path := filepath.Join(cityPath, "city.toml")
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

// reverseCityTOML computes R4's city.toml (decision O5) and says how: the
// pre-split bytes the guided split's journal holds when the file has not
// changed since except for [storage], otherwise the current file with
// [storage] removed in place; then, with --reconciler=legacy, the reconciler
// key rewritten in place. It writes nothing.
func reverseCityTOML(cityPath, reconciler string) ([]byte, string, error) {
	tomlPath, err := reverseCityTOMLPath(cityPath)
	if err != nil {
		return nil, "", err
	}
	current, err := os.ReadFile(tomlPath)
	if err != nil {
		return nil, "", err
	}
	declared, err := config.CityTOMLDeclaresStorage(current)
	if err != nil {
		return nil, "", err
	}
	if !declared {
		return nil, "", fmt.Errorf("%s declares no [storage] section: a fragment declares it, and a fragment is the operator's to edit", tomlPath)
	}
	removed, err := config.RemoveStorageFromCityTOML(current)
	if err != nil {
		return nil, "", err
	}
	edited, how := removed, "[storage] removed in place"
	if before, after, ok := readSplitJournalCityTOML(cityPath); ok && after == sha256Hex(current) {
		// Nothing changed since the split wrote the file, comments included:
		// its bytes from before the split are the file.
		edited, how = before, "restored to its bytes from before the split ("+storageSplitJournalName+")"
	}
	if reconciler != "" {
		if edited, err = config.SetSessionReconcilerInCityTOML(edited, reconciler); err != nil {
			return nil, "", err
		}
		how += "; [daemon] session_reconciler = " + fmt.Sprintf("%q", reconciler)
	}
	return edited, how, nil
}

// readSplitJournalCityTOML is city.toml as the guided split's journal (S5-H)
// recorded it before the split, proven by its sha256, and the sha256 of the
// file the split wrote; ok is false when there is no journal or it does not
// prove.
func readSplitJournalCityTOML(cityPath string) ([]byte, string, bool) {
	data, err := os.ReadFile(filepath.Join(cityPath, ".gc", storageSplitJournalName))
	if err != nil {
		return nil, "", false
	}
	var journal struct {
		CityTOMLBefore       *string `json:"city_toml_before"`
		CityTOMLBeforeSHA256 string  `json:"city_toml_before_sha256"`
		CityTOMLAfterSHA256  string  `json:"city_toml_after_sha256"`
	}
	if json.Unmarshal(data, &journal) != nil || journal.CityTOMLBefore == nil || journal.CityTOMLAfterSHA256 == "" {
		return nil, "", false
	}
	before := []byte(*journal.CityTOMLBefore)
	if sha256Hex(before) != journal.CityTOMLBeforeSHA256 {
		return nil, "", false
	}
	return before, journal.CityTOMLAfterSHA256, true
}

// reverseCheckCityTOML composes the city with edited in place of its
// city.toml, in memory, fragments and includes as they are on disk: it must
// load, carry no [storage], and, with --reconciler, name that reconciler.
func reverseCheckCityTOML(cityPath string, edited []byte, reconciler string) error {
	path := filepath.Join(cityPath, "city.toml")
	cfg, err := loadCityConfigWithoutBuiltinPackRefreshFS(reverseOverlayFS{FS: fsys.OSFS{}, path: path, data: edited}, path, io.Discard)
	if err != nil {
		return fmt.Errorf("city.toml without [storage] would not load: %w", err)
	}
	if cfg.Storage != nil {
		return errors.New("city.toml without its [storage] section still composes a [storage]: a fragment it includes declares one, and a fragment is the operator's to edit")
	}
	if reconciler != "" {
		if mode, _, _ := cfg.Daemon.SessionReconcilerMode(); mode != reconciler {
			return fmt.Errorf("the edited city.toml resolves session_reconciler to %q, want %q (a fragment sets it)", mode, reconciler)
		}
	}
	return nil
}

// reverseOverlayFS reads data in place of the file at path.
type reverseOverlayFS struct {
	fsys.FS
	path string
	data []byte
}

func (o reverseOverlayFS) ReadFile(name string) ([]byte, error) {
	if samePath(name, o.path) {
		return append([]byte(nil), o.data...), nil
	}
	return o.FS.ReadFile(name)
}

// writeCityTOMLAtomic replaces city.toml atomically and durably, keeping its
// mode: a synced temp file renamed into place, then the directory synced.
func writeCityTOMLAtomic(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		_ = os.Remove(name)
		return err
	}
	syncInfraDir(dir)
	return nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func sortedUnique(values []string) []string {
	set := map[string]bool{}
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			set[v] = true
		}
	}
	return sortedMapKeys(set)
}

// doStorageAbortReverse is `gc storage migrate --abort-reverse`: it deletes
// exactly the rows carrying the open reverse's stamp from the destinations
// its journal names, puts back the edges those stores held onto them before
// the reverse, puts back what R4 changed, removes the reverse's records and
// unpins its snapshot. The binding was never written, so nothing else moves.
func doStorageAbortReverse(ctx context.Context, request storageOperatorRequest, stdout, stderr io.Writer) (code int) {
	logPrefix := "gc storage migrate --" + storageAbortReverseFlag
	fail := func(err error) int {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if _, present, err := readStorageReverseJournal(request.CityPath); err != nil {
		return fail(err)
	} else if !present {
		return fail(errors.New("no reverse migration is in progress, so there is nothing to abandon"))
	}
	controllerLock, err := acquireStorageVerbExclusion(request, storageVerbScope{})
	if err != nil {
		return fail(err)
	}
	defer controllerLock.Close() //nolint:errcheck // released last
	if pid := infraMigrationForeignControllerPID(request.CityPath); pid != 0 {
		return fail(fmt.Errorf("controller PID %d is live on this city. Stop it (%s) and run this again", pid, storageStopCommand))
	}
	if !request.FleetStopped {
		return fail(fmt.Errorf("pass --%s to attest that %s", storageFleetStoppedFlag, storageFleetStoppedAttestation))
	}
	shared, err := sharedWorkStoreRefusal(request.CityPath, request.Force)
	if err != nil {
		return fail(err)
	}
	defer shared.Release() // the co-tenants stay stopped until the abort is done
	rigShared, err := reverseRigSharedStoreRefusal(request.CityPath, request.Cfg, request.Force)
	defer func() {
		for _, x := range rigShared {
			x.Release()
		}
	}()
	if err != nil {
		return fail(err)
	}
	guard, err := acquireStorageMigrationGuard(ctx, request.CityPath)
	if err != nil {
		return fail(err)
	}
	defer guard.Release() //nolint:errcheck // released before the lock
	if err := infraEngineCrashPoint("abort-reverse guarded"); err != nil {
		return fail(err)
	}
	// The phase is judged on the journal as it is under the lock and the
	// guard, not as it was before them.
	journal, present, err := readStorageReverseJournal(request.CityPath)
	if err != nil {
		return fail(err)
	}
	if !present || journal.Phase >= reverseComplete {
		return fail(errors.New("no reverse migration is in progress, so there is nothing to abandon"))
	}
	target := journal.target()
	finish := fmt.Errorf("the reverse has begun to change the binding, so it can only be finished: run `%s`", storageReverseInstruction(journal.Mode, journal.Rigs))
	switch {
	case journal.Mode == reverseToRigs && (journal.Phase >= reverseR4 || journal.ClearStarted):
		return fail(finish)
	case journal.Mode == reverseToRigs && journal.Phase == reverseR3:
		if moved, err := reverseRigRowsLeftBinding(target, journal); err != nil || moved {
			return fail(errors.Join(finish, err))
		}
	case journal.Mode == reverseToWork && !fileExists(target.Root):
		return fail(finish)
	}
	window, err := openMigrationSource(controllerLock, request.CityPath, request.Cfg, logPrefix, stderr, reverseRigRoots(request.CityPath, request.Cfg)...)
	if err != nil {
		return fail(err)
	}
	defer window.closeReporting(logPrefix, stderr, &code)

	// Every edge a store held onto a row this reverse wrote must go back once
	// the row is gone. A store that refuses an edge onto a row it does not
	// hold (native Dolt, for its own prefix) cannot take it back: refused
	// here, before any row is deleted.
	var dangling []string
	for _, dest := range journal.Dests {
		ids, judge, carries, err := abortReverseStamped(request.CityPath, request.Cfg, dest, journal.Stamp)
		if err != nil {
			return fail(err)
		}
		for _, e := range journal.Preserved[dest] {
			if ids[e.To] && (!judge(e.From, e.To) || (e.Payload != "" && !carries)) {
				dangling = append(dangling, fmt.Sprintf("%s -[%s]-> %s (%s)", e.From, e.Type, e.To, reverseDestNoun(dest)))
			}
		}
	}
	if len(dangling) > 0 {
		return fail(fmt.Errorf("%s held edge(s) onto rows this reverse wrote before it wrote them, and could not take each back whole once the row is gone (an edge onto a row it does not hold, or a payload it cannot write), so abandoning the reverse would lose them: %s. Nothing was removed; finish the reverse with `%s` instead", "the destination store(s)", strings.Join(dangling, ", "), storageReverseInstruction(journal.Mode, journal.Rigs)))
	}
	// A city.toml the abort could not put back refuses here too, before any
	// row is deleted, not after every row is gone.
	if _, _, err := reverseCityTOMLToUndo(request.CityPath, journal); err != nil {
		return fail(err)
	}
	// From the first delete on, the reverse can only be abandoned: a resume
	// would find its own rows gone and archive the binding over them.
	journal.Aborting = true
	if err := writeStorageReverseJournal(request.CityPath, journal); err != nil {
		return fail(err)
	}
	removed := 0
	for _, dest := range journal.Dests {
		n, err := abortReverseIn(request.CityPath, request.Cfg, dest, journal.Stamp, journal.Preserved[dest])
		removed += n
		if err != nil {
			return fail(fmt.Errorf("%w; %d row(s) removed so far. Run this again to finish abandoning the reverse", err, removed))
		}
	}
	if err := infraEngineCrashPoint("abort-reverse rows"); err != nil {
		return fail(err)
	}
	if err := undoReverseRetirement(request.CityPath, journal); err != nil {
		return fail(err)
	}
	for _, name := range []string{infraRevertingRecordName, infraReversedManifestName} {
		if err := os.Remove(filepath.Join(target.Root, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
	}
	for _, rig := range journal.Rigs {
		if err := os.Remove(filepath.Join(target.Root, infraReversedRigManifestName(rig))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
	}
	if journal.Snapshot != "" {
		_ = os.Remove(filepath.Join(journal.SnapshotTo, journal.Snapshot, storageSnapshotPinName))
	}
	if err := os.Remove(storageReverseJournalPath(request.CityPath)); err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "Abandoned the reverse %s: %d row(s) it wrote removed; the split is as it was.\n", journal.Stamp, removed) //nolint:errcheck // best-effort stdout
	return 0
}

// abortReverseStamped is the ids dest holds under stamp, its rule for an edge
// onto a row it does not hold, and whether it can write an edge payload.
func abortReverseStamped(cityPath string, cfg *config.City, dest, stamp string) (map[string]bool, func(string, string) bool, bool, error) {
	t, err := reverseTargetOpener(cityPath, cfg, dest)
	if err != nil {
		return nil, nil, false, err
	}
	defer t.close()
	stamped, err := t.rows.List(reverseStampQuery(stamp))
	if err != nil {
		return nil, nil, false, err
	}
	ids := map[string]bool{}
	for _, b := range stamped {
		if b.Metadata[beadmeta.InfraReversedFromMetadataKey] == stamp {
			ids[b.ID] = true
		}
	}
	return ids, t.acceptsDangling, t.carriesPayload, nil
}

// abortReverseIn removes from dest every row carrying stamp, and nothing
// else, then puts back each edge the store held onto one of them before the
// reverse (preserved), from a row it still holds.
func abortReverseIn(cityPath string, cfg *config.City, dest, stamp string, preserved []reverseEdge) (int, error) {
	t, err := reverseTargetOpener(cityPath, cfg, dest)
	if err != nil {
		return 0, err
	}
	defer t.close()
	stamped, err := t.rows.List(reverseStampQuery(stamp))
	if err != nil {
		return 0, err
	}
	ids := make([]string, 0, len(stamped))
	for _, b := range stamped {
		if b.Metadata[beadmeta.InfraReversedFromMetadataKey] == stamp {
			ids = append(ids, b.ID)
		}
	}
	if err := reverseDeleteRows(t, ids); err != nil {
		return 0, err
	}
	if len(preserved) == 0 {
		return len(ids), nil
	}
	present, err := t.rows.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return len(ids), err
	}
	held := map[string]bool{}
	for _, b := range present {
		held[b.ID] = true
	}
	var back []beads.DepWithPayload
	for _, e := range preserved {
		if held[e.From] {
			back = append(back, e.dep())
		}
	}
	if err := t.addEdges(back); err != nil {
		return len(ids), fmt.Errorf("putting back the edges %s held before the reverse: %w", reverseDestNoun(dest), err)
	}
	return len(ids), nil
}

// reverseRigRowsLeftBinding reports whether a to-rigs reverse's binding clear
// has removed any row its manifests record.
func reverseRigRowsLeftBinding(target infraBindingTarget, journal storageReverseJournal) (bool, error) {
	binding, err := openInfraBindingReadOnly(target)
	if err != nil {
		return false, err
	}
	defer closeBeadStoreHandle(binding) //nolint:errcheck // read-only
	for _, rig := range journal.Rigs {
		m, recorded, err := readInfraCopyManifestRecordAt(filepath.Join(target.Root, infraReversedRigManifestName(rig)), "")
		if err != nil || !recorded {
			return false, err
		}
		held, err := beads.GetMany(binding, sortedMapKeys(m.IDs))
		if err != nil {
			return false, err
		}
		if len(held) != len(m.IDs) {
			return true, nil
		}
	}
	return false, nil
}

// undoReverseRetirement puts back what R4 changed: city.toml, unless the
// operator changed it since (reverseCityTOMLToUndo), the renamed records and
// the version floor.
func undoReverseRetirement(cityPath string, journal storageReverseJournal) error {
	tomlPath, undo, err := reverseCityTOMLToUndo(cityPath, journal)
	if err != nil {
		return err
	}
	if undo {
		if err := writeCityTOMLAtomic(tomlPath, []byte(*journal.CityTOMLBefore)); err != nil {
			return err
		}
	}
	for _, rn := range journal.Renamed {
		if fileExists(rn.To) && !fileExists(rn.From) {
			if err := os.Rename(rn.To, rn.From); err != nil {
				return err
			}
		}
	}
	if journal.MinGCVersion != nil {
		path := minGCVersionPath(cityPath)
		if !fileExists(path) {
			if err := writeRigImportRecord(path, strings.TrimSuffix(*journal.MinGCVersion, "\n")); err != nil {
				return err
			}
		}
	}
	return nil
}

// reverseCityTOMLToUndo judges city.toml against what an open --to-work's R4
// did to it (journal): undo is true when it is R4's own edit, which an abort
// replaces with the bytes from before it. The file from before R4, or any file
// the city composes into a [storage] that serves the journal's binding (the
// operator put the split back by hand, a change of their own kept), stays as
// it is. Any other file is an error, since putting the earlier bytes back
// would undo the operator's change. Until the journal says Aborting the abort
// has deleted no row (it records that first), so the error then says nothing
// was removed and names the resume as the other way out. path is city.toml as
// R4 edits it, through a symlink, or "" when R4 recorded no edit.
func reverseCityTOMLToUndo(cityPath string, journal storageReverseJournal) (path string, undo bool, err error) {
	if journal.CityTOMLBefore == nil {
		return "", false, nil
	}
	if path, err = reverseCityTOMLPath(cityPath); err != nil {
		return "", false, err
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	switch sha256Hex(current) {
	case sha256Hex([]byte(*journal.CityTOMLBefore)):
		return path, false, nil
	case journal.CityTOMLAfterSHA256:
		return path, true, nil
	}
	serves, loadErr := reverseCityTOMLServesBinding(cityPath, current, journal)
	if serves {
		return path, false, nil
	}
	why := fmt.Sprintf("composes no [storage] that serves the binding %s at %s", journal.Binding, journal.Root)
	if loadErr != nil {
		why = fmt.Sprintf("does not load (%v)", loadErr)
	}
	refusal := fmt.Sprintf("%s changed since the reverse edited it and %s, so putting back its earlier bytes would undo your change", path, why)
	remedy := fmt.Sprintf("put [storage] back as the split had it, keeping your change (the journal %s holds the earlier file as city_toml_before), then run this again", storageReverseJournalPath(cityPath))
	if journal.Aborting {
		return "", false, fmt.Errorf("%s; %s", refusal, remedy)
	}
	return "", false, fmt.Errorf("%s. Nothing was removed: %s, or undo your change and finish the reverse with `%s`", refusal, remedy, storageReverseInstruction(journal.Mode, journal.Rigs))
}

// reverseCityTOMLServesBinding reports whether the city, composed with data in
// place of its city.toml (fragments and includes as they are on disk),
// declares a [storage] whose infrastructure binding is the journal's, by name
// and root: the split as the reverse found it.
func reverseCityTOMLServesBinding(cityPath string, data []byte, journal storageReverseJournal) (bool, error) {
	path := filepath.Join(cityPath, "city.toml")
	cfg, err := loadCityConfigWithoutBuiltinPackRefreshFS(reverseOverlayFS{FS: fsys.OSFS{}, path: path, data: data}, path, io.Discard)
	if err != nil {
		return false, err
	}
	target, ok, err := resolveInfraBindingTarget(cityPath, cfg)
	if err != nil || !ok {
		return false, err
	}
	return target.Binding == journal.Binding && samePath(target.Root, journal.Root), nil
}
