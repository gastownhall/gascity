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
	// reverseMinGCVersionName is the version floor a split records (§7.4).
	reverseMinGCVersionName = "min-gc-version"
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
		if state, err := readInfraConvergenceState(target); err != nil || state != infraConvergenceMarked {
			return fail(fmt.Errorf("the binding %s has not converged (no proven copy behind %s), so it is not the authority a reverse copies from. Finish the migration (`%s`) or restore the binding first", target.Binding, target.MarkerPath(), storageClearInstruction()))
		}
		if err := infraRestoreRecopyRefusal(target); err != nil {
			return fail(err)
		}
	}
	if opts.Mode == reverseToWork && (!open || journal.Phase < reverseR4) && opts.Reconciler == "" {
		if mode, _, _ := request.Cfg.Daemon.SessionReconcilerMode(); mode == config.SessionReconcilerV2 {
			return fail(fmt.Errorf("[daemon] session_reconciler names v2, which refuses a city whose sessions class is on the work store (C0.7), so the city would not start after the reverse. Pass --%s=%s to rewrite it in the same city.toml edit, or remove the key yourself first", storageReconcilerFlag, config.SessionReconcilerLegacy))
		}
	}

	// The controller lock, the liveness checks and the attestation, then the
	// guard, then the window: the order every writing storage verb takes.
	controllerLock, err := acquireStorageVerbExclusion(request, storageVerbScope{})
	if err != nil {
		return fail(err)
	}
	defer controllerLock.Close() //nolint:errcheck // released last
	if pid := infraMigrationForeignControllerPID(request.CityPath); pid != 0 {
		return fail(fmt.Errorf("controller PID %d is live on this city. Stop it (%s) and run this again", pid, storageStopCommand))
	}
	if !request.FleetStopped {
		return fail(fmt.Errorf("pass --%s to attest that %s. A write that lands in the binding during the reverse is not copied back", storageFleetStoppedFlag, storageFleetStoppedAttestation))
	}
	guard, err := acquireStorageMigrationGuard(ctx, request.CityPath)
	if err != nil {
		return fail(err)
	}
	defer guard.Release() //nolint:errcheck // released before the lock
	window, err := openMigrationSource(controllerLock, request.CityPath, request.Cfg, logPrefix, stderr, reverseRigRoots(request.CityPath, request.Cfg)...)
	if err != nil {
		return fail(err)
	}
	defer window.closeReporting(logPrefix, stderr, &code)
	window.setTimeBudget(request.TimeBudget)
	recordStorageAttestation(request.CityPath, logPrefix, request.Force.acceptedHazards(), stderr)

	run := reverseRun{request: request, opts: opts, target: target, journal: journal, logPrefix: logPrefix, stdout: stdout, stderr: stderr}
	if !open {
		now := time.Now().UTC()
		run.journal = storageReverseJournal{
			Format: storageReverseJournalFormat, Mode: opts.Mode, Rigs: sortedUnique(opts.Rigs), Phase: reverseStarted,
			Run: now.Format("20060102T150405Z"), StartedAt: now, Reconciler: opts.Reconciler,
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
	window.closeReporting(logPrefix, stderr, &code)
	if code != 0 {
		return code
	}
	if opts.Mode == reverseToRigs {
		fmt.Fprintf(stdout, "Reversed. Rig(s) %s hold their imported rows again and read as awaiting import; the city stays split. Snapshot %s is pinned as the way back. Start the city with `gc start`.\n", strings.Join(run.journal.Rigs, ", "), run.journal.Snapshot) //nolint:errcheck // best-effort stdout
		return 0
	}
	fmt.Fprintf(stdout, "Reversed. Every class is on the work store again; the binding is archived as %s and snapshot %s is pinned as the way back. Start the city with `gc start`.\n", target.Root+run.journal.suffix(), run.journal.Snapshot) //nolint:errcheck // best-effort stdout
	return 0
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
	}
	plan := planReverse(in)
	for _, dest := range sortedMapKeys(plan.Rows) {
		if t := targets[dest]; t != nil && t.unsupported != "" && len(plan.Rows[dest]) > 0 {
			refusals = append(refusals, rigImportRefusal{IDs: []string{reverseDestNoun(dest)}, Reason: t.unsupported, Remedy: "nothing was written; keep the city split, or roll back by restoring the snapshot (the runbook's restore steps)"})
		}
	}
	if r.opts.Mode == reverseToWork && r.journal.Phase < reverseR4 {
		if _, _, err := reverseCityTOML(cityPath, r.opts.Reconciler); err != nil {
			refusals = append(refusals, rigImportRefusal{IDs: []string{"city.toml"}, Reason: err.Error(), Remedy: "remove [storage] by hand where it is declared, then re-run"})
		}
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
		for _, dest := range sortedMapKeys(r.planned.Rows) {
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
		if err := r.writeManifests(); err != nil {
			return err
		}
		if err := r.advance(reverseR3); err != nil {
			return err
		}
	}
	if r.journal.Phase < reverseR4 {
		var err error
		if r.opts.Mode == reverseToRigs {
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
	opts := snapshotOptions{Binding: true, Work: r.opts.Mode == reverseToWork, Rigs: true, To: storageSnapshotDefaultDir(r.request.CityPath), Keep: storageSnapshotDefaultKeep}
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
	if len(rows) == 0 && len(edges) == 0 {
		return nil
	}
	open := func() (*reverseTarget, error) { return reverseTargetOpener(r.request.CityPath, r.request.Cfg, dest) }
	t, err := open()
	if err != nil {
		return err
	}
	defer t.close()
	copied, err := reverseCopyInto(t, open, rows, edges, r.journal.Stamp, clock)
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
				return fmt.Errorf("the binding holds %s, stamped as an import of rig %s, which this reverse did not return to the rig; nothing was removed. Abandon the reverse with `%s` and start again", b.ID, rig, storageAbortReverseInstruction())
			}
			going = append(going, b.ID)
		}
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
		found, _ := filepath.Glob(filepath.Join(r.target.Root, "infra.rig."+rig+".*"))
		found = append(found, rigGraphClearedNotePath(r.request.CityPath, rig))
		renames = append(renames, reverseRenames(found, r.journal.suffix())...)
	}
	r.journal.Renamed = append(r.journal.Renamed, renames...)
	if err := writeStorageReverseJournal(r.request.CityPath, r.journal); err != nil {
		return err
	}
	return applyReverseRenames(r.journal.Renamed)
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

// applyReverseRenames renames each record; one already renamed is skipped.
func applyReverseRenames(renames []reverseRename) error {
	for _, rn := range renames {
		if !fileExists(rn.From) && fileExists(rn.To) {
			continue
		}
		if err := os.Rename(rn.From, rn.To); err != nil {
			return fmt.Errorf("retiring %s: %w", rn.From, err)
		}
	}
	return nil
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
		if data, err := os.ReadFile(filepath.Join(cityPath, ".gc", reverseMinGCVersionName)); err == nil {
			floor := string(data)
			r.journal.MinGCVersion = &floor
		}
		var paths []string
		paths = append(paths, bornSplitServedNotePath(cityPath))
		notes, _ := filepath.Glob(filepath.Join(cityPath, ".gc", "storage-infra-cleared*.json"))
		paths = append(paths, notes...)
		rigRecords, _ := filepath.Glob(filepath.Join(r.target.Root, "infra.rig.*"))
		paths = append(paths, rigRecords...)
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
		if err := writeCityTOMLAtomic(tomlPath, edited); err != nil {
			return err
		}
		if err := reverseCheckCityTOML(cityPath, r.opts.Reconciler); err != nil {
			return errors.Join(err, writeCityTOMLAtomic(tomlPath, []byte(*r.journal.CityTOMLBefore)))
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
	if err := os.Remove(filepath.Join(cityPath, ".gc", reverseMinGCVersionName)); err != nil && !errors.Is(err, os.ErrNotExist) {
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
	if before, ok := readSplitJournalCityTOML(cityPath); ok && config.SameTOMLDocument(before, removed) {
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
// recorded it before the split, proven by its sha256; ok is false when there
// is no journal or it does not prove.
func readSplitJournalCityTOML(cityPath string) ([]byte, bool) {
	data, err := os.ReadFile(filepath.Join(cityPath, ".gc", storageSplitJournalName))
	if err != nil {
		return nil, false
	}
	var journal struct {
		CityTOMLBefore       *string `json:"city_toml_before"`
		CityTOMLBeforeSHA256 string  `json:"city_toml_before_sha256"`
	}
	if json.Unmarshal(data, &journal) != nil || journal.CityTOMLBefore == nil {
		return nil, false
	}
	before := []byte(*journal.CityTOMLBefore)
	if sha256Hex(before) != journal.CityTOMLBeforeSHA256 {
		return nil, false
	}
	return before, true
}

// reverseCheckCityTOML reloads the city after R4's edit: it must carry no
// [storage], and, with --reconciler, name that reconciler.
func reverseCheckCityTOML(cityPath, reconciler string) error {
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		return fmt.Errorf("the edited city.toml does not load: %w", err)
	}
	if cfg.Storage != nil {
		return errors.New("the edited city.toml still configures [storage]")
	}
	if reconciler != "" {
		if mode, _, _ := cfg.Daemon.SessionReconcilerMode(); mode != reconciler {
			return fmt.Errorf("the edited city.toml resolves session_reconciler to %q, want %q", mode, reconciler)
		}
	}
	return nil
}

// writeCityTOMLAtomic replaces city.toml atomically, keeping its mode.
func writeCityTOMLAtomic(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return fsys.WriteFileAtomic(fsys.OSFS{}, path, data, mode)
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
// exactly the rows carrying the open reverse's stamp from the work and rig
// stores, puts back what R4 changed, removes the reverse's records and
// unpins its snapshot. The binding was never written, so nothing else moves.
func doStorageAbortReverse(ctx context.Context, request storageOperatorRequest, stdout, stderr io.Writer) (code int) {
	logPrefix := "gc storage migrate --" + storageAbortReverseFlag
	fail := func(err error) int {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
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
	case journal.Mode == reverseToRigs && journal.Phase >= reverseR4:
		return fail(finish)
	case journal.Mode == reverseToRigs && journal.Phase == reverseR3:
		if moved, err := reverseRigRowsLeftBinding(target, journal); err != nil || moved {
			return fail(errors.Join(finish, err))
		}
	case journal.Mode == reverseToWork && !fileExists(target.Root):
		return fail(finish)
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
	guard, err := acquireStorageMigrationGuard(ctx, request.CityPath)
	if err != nil {
		return fail(err)
	}
	defer guard.Release() //nolint:errcheck // released before the lock
	window, err := openMigrationSource(controllerLock, request.CityPath, request.Cfg, logPrefix, stderr, reverseRigRoots(request.CityPath, request.Cfg)...)
	if err != nil {
		return fail(err)
	}
	defer window.closeReporting(logPrefix, stderr, &code)

	dests := []string{reverseCityDest}
	for _, rig := range reverseRigs(request.CityPath, request.Cfg) {
		dests = append(dests, rigImportStamp(rig.Name))
	}
	removed := 0
	for _, dest := range dests {
		n, err := abortReverseIn(request.CityPath, request.Cfg, dest, journal.Stamp)
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

// abortReverseIn removes from dest every row carrying stamp, and nothing
// else.
func abortReverseIn(cityPath string, cfg *config.City, dest, stamp string) (int, error) {
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
	return len(ids), reverseDeleteRows(t, ids)
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
		path := filepath.Join(cityPath, ".gc", reverseMinGCVersionName)
		if !fileExists(path) {
			if err := writeRigImportRecord(path, strings.TrimSuffix(*journal.MinGCVersion, "\n")); err != nil {
				return err
			}
		}
	}
	return nil
}
