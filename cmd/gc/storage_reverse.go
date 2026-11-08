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
//	    journal holds them and nothing else changed since); the served note,
//	    the cleared notes and every rig record renamed *.reverted-<run>;
//	    .gc/min-gc-version removed
//	R5  the binding root renamed *.reverted-<run>; the journal completed
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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
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
		if err := infraRestoreRecopyRefusal(target); err != nil {
			return fail(err)
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

	run := reverseRun{request: request, opts: opts, target: target, journal: journal, logPrefix: logPrefix, stdout: stdout, stderr: stderr}
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
	request   storageOperatorRequest
	opts      reverseOptions
	target    infraBindingTarget
	journal   storageReverseJournal
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
	r.journal.BindingSHA256 = read.fingerprint()
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
	for _, dest := range sortedMapKeys(plan.Rows) {
		if t := targets[dest]; t != nil && t.unsupported != "" && len(plan.Rows[dest]) > 0 {
			refusals = append(refusals, rigImportRefusal{IDs: []string{reverseDestNoun(dest)}, Reason: t.unsupported, Remedy: "nothing was written; keep the city split, or roll back by restoring the snapshot (the runbook's restore steps)"})
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
	if len(payloadKept) > 0 {
		refusals = append(refusals, rigImportRefusal{IDs: payloadKept, Reason: "each edge carries a payload and is held onto a row the reverse writes, in a store that cannot write a payload: a resume or an abort that has to put it back would lose its payload", Remedy: "keep the city split, or remove the edge once it is no longer needed; nothing was written"})
	}
	plan.Refusals = append(refusals, plan.Refusals...)
	return plan, nil
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
		fmt.Fprintf(r.stdout, "%s: %d closed cross-store gate(s) not carried (each owner's record restores the edge it stood for)\n", r.logPrefix, plan.Gates) //nolint:errcheck // best-effort stdout
	}
	for _, d := range plan.Dropped {
		fmt.Fprintf(r.stdout, "%s: edge not carried: %s\n", r.logPrefix, d) //nolint:errcheck // best-effort stdout
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
	if r.opts.Mode == reverseToRigs {
		for _, rig := range r.journal.Rigs {
			if name := infraReversedRigManifestName(rig); byManifest[name] == nil {
				byManifest[name] = []beads.Bead{}
			}
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
// the binding's copies go, and with them the closed gates the rig's import
// minted, each edge to a row the binding keeps first; then the rig's import
// records are renamed so it reads as awaiting import. Every binding row
// stamped as the rig's import must be one the reverse manifest records or a
// closed gate, or nothing is removed.
func (r *reverseRun) clearRigRowsFromBinding() error {
	binding, err := openInfraDestination(r.target)
	if err != nil {
		return err
	}
	defer closeBeadStoreHandle(binding) //nolint:errcheck // best-effort close
	var going []string
	for _, rig := range r.journal.Rigs {
		m, _, err := readInfraCopyManifestRecordAt(filepath.Join(r.target.Root, infraReversedRigManifestName(rig)), "")
		if err != nil {
			return err
		}
		stamped, err := binding.List(beads.ListQuery{Metadata: map[string]string{infraMigrationStampKey: rigImportStamp(rig)}, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
		if err != nil {
			return err
		}
		for _, b := range stamped {
			if !m.IDs[b.ID] && (!isCrossStoreGate(b) || rigRowOpen(b)) {
				next := fmt.Sprintf("Abandon the reverse with `%s` and start again", storageAbortReverseInstruction())
				if r.journal.ClearStarted {
					next = fmt.Sprintf("The clear has begun, so the reverse cannot be abandoned: return the row to the rig by hand, or restore from snapshot %s in %s", r.journal.Snapshot, r.journal.SnapshotTo)
				}
				return fmt.Errorf("the binding holds %s, stamped as an import of rig %s, which this reverse did not return to the rig; nothing was removed. %s", b.ID, rig, next)
			}
			going = append(going, b.ID)
		}
	}
	r.journal.ClearStarted = true
	if err := writeStorageReverseJournal(r.request.CityPath, r.journal); err != nil {
		return err
	}
	lister, _ := beads.As[beads.DependencyBatchLister](binding)
	t := &reverseTarget{
		dest:      "binding",
		rows:      binding,
		down:      func(ids []string) (map[string][]beads.Dep, error) { return readInfraDownEdges(lister, binding, ids) },
		deleteIDs: func(ids []string) error { return deleteInBatches(binding, ids) },
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

// destinationsHold proves, before the reverse changes the binding or the
// split's records (R4) and before it archives the binding (R5), that the
// stores it wrote still hold every row its reverse manifests record, under
// this run's stamp: an abort killed partway, or a writer, can have removed
// some. Before a to-rigs clear has begun, a shortfall puts the reverse back
// to R0, and the next run copies again. Once it has begun the rigs may hold
// the only copy of what the binding no longer does, so it only refuses,
// naming the snapshot.
func (r *reverseRun) destinationsHold(before string) error {
	var manifests []string
	if r.opts.Mode == reverseToRigs {
		for _, rig := range r.journal.Rigs {
			manifests = append(manifests, filepath.Join(r.target.Root, infraReversedRigManifestName(rig)))
		}
	} else {
		manifests = append(manifests, filepath.Join(r.target.Root, infraReversedManifestName))
	}
	want := map[string]bool{}
	for _, path := range manifests {
		m, recorded, err := readInfraCopyManifestRecordAt(path, "")
		if err != nil {
			return err
		}
		if !recorded {
			return fmt.Errorf("the reverse manifest %s is gone, so nothing says which rows this reverse wrote", path)
		}
		for id := range m.IDs {
			want[id] = true
		}
	}
	held := map[string]bool{}
	for _, dest := range r.journal.Dests {
		t, err := reverseTargetOpener(r.request.CityPath, r.request.Cfg, dest)
		if err != nil {
			return err
		}
		stamped, err := t.rows.List(reverseStampQuery(r.journal.Stamp))
		t.close()
		if err != nil {
			return err
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
	if len(missing) == 0 {
		return nil
	}
	if r.journal.ClearStarted {
		return fmt.Errorf("the stores this reverse wrote no longer hold %d row(s) it returned (%s), and its binding clear has begun, so the binding may not hold them either. Nothing more was changed; restore them from snapshot %s in %s (the runbook's restore steps)", len(missing), infraStrandedIDList(missing), r.journal.Snapshot, r.journal.SnapshotTo)
	}
	r.journal.Phase = reverseR0
	if err := writeStorageReverseJournal(r.request.CityPath, r.journal); err != nil {
		return err
	}
	return fmt.Errorf("the stores this reverse wrote no longer hold %d row(s) it returned (%s), checked before %s, so the binding and the split's records are left as they are; the next run copies them again", len(missing), infraStrandedIDList(missing), before)
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
		return fmt.Errorf("%s changed while the reverse was under way, so this run cannot tell its edit from yours; put it back, or abandon the reverse with `%s`", tomlPath, storageAbortReverseInstruction())
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

// undoReverseRetirement puts back what R4 changed: city.toml (unless the
// operator changed it since), the renamed records and the version floor.
func undoReverseRetirement(cityPath string, journal storageReverseJournal) error {
	if journal.CityTOMLBefore != nil {
		path, err := reverseCityTOMLPath(cityPath)
		if err != nil {
			return err
		}
		current, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		switch sha256Hex(current) {
		case sha256Hex([]byte(*journal.CityTOMLBefore)):
		case journal.CityTOMLAfterSHA256:
			if err := writeCityTOMLAtomic(path, []byte(*journal.CityTOMLBefore)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s changed since the reverse edited it, so putting back its earlier bytes would undo your change; restore [storage] by hand (the journal %s holds the earlier bytes), then run this again", path, storageReverseJournalPath(cityPath))
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
