package main

// `gc storage split` (S5 §5, S5-H): the guided split of an unsplit city.
//
// One command, under one controller lock, one migration guard and one
// maintenance window, runs what an operator would otherwise run by hand
// (the footer of `gc storage preflight --split` lists it):
//
//	P1  the lock and the liveness checks, as every writing storage verb
//	P5  the maintenance window, before the rehearsal: on a stopped city the
//	    rehearsal reads nothing outside one
//	P0  `gc storage preflight --split`, inside the window; any block refuses
//	    and nothing is written
//	P3  a snapshot of the work store and the rigs, pinned
//	P2  legacy's demand-migration repairs, when the split city runs v2, so the
//	    rows the copy carries are repaired ones
//	P4  [storage] appended to city.toml, and .gc/min-gc-version
//	P6  the city migration (copy, clear, id floor, served note, relower)
//	P8  every rig's import (S4b)
//	P9  `gc storage status` and the storage-split doctor check, in process
//	P10 the window closed; `gc start` and the rollback named
//
// The journal, .gc/storage-split.journal.json, records each phase once it is
// done and, before city.toml is written, the file's bytes and the sha256 of
// what the split writes: S5-G's reverse (storage_reverse.go,
// readSplitJournalCityTOML) restores those bytes when city.toml is still the
// one the split wrote. Run again, the command resumes a split whose city.toml
// carries the split's [storage]; a split stopped before that write is started
// over, because the city could have run since. --abandon backs out a split
// stopped before the city copy's convergence marker; past it, the reverse
// (`gc storage migrate --to-work`) is the way back.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/deps"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
)

const (
	storageSplitVerb    = "split"
	storageAbandonFlag  = "abandon"
	storageSplitLogName = "gc storage split"
	// storageSplitJournalFormat names the journal's shape.
	storageSplitJournalFormat = "gc.storage.split/v1"
)

// The split's phases, as the journal records the last one done. A journal
// in any phase but complete and abandoned is open.
const (
	splitPhaseSnapshotted   = "snapshotted"    // P3: the snapshot pinned
	splitPhaseReadied       = "readied"        // P2: the demand repairs run
	splitPhaseConfiguring   = "configuring"    // P4: city.toml's before and after recorded
	splitPhaseConfigured    = "configured"     // P4: city.toml and the floor written
	splitPhaseCityConverged = "city_converged" // P6
	splitPhaseRigsImported  = "rigs_imported"  // P8
	splitPhaseComplete      = "complete"       // P9
	splitPhaseAbandoning    = "abandoning"     // --abandon under way
	splitPhaseAbandoned     = "abandoned"
)

// splitPhaseRank orders the forward phases.
var splitPhaseRank = map[string]int{
	splitPhaseSnapshotted:   1,
	splitPhaseReadied:       2,
	splitPhaseConfiguring:   3,
	splitPhaseConfigured:    4,
	splitPhaseCityConverged: 5,
	splitPhaseRigsImported:  6,
	splitPhaseComplete:      7,
}

// storageSplitJournal is .gc/storage-split.journal.json.
type storageSplitJournal struct {
	Format      string    `json:"format"`
	Run         string    `json:"run"`
	Phase       string    `json:"phase"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at,omitzero"`
	Binding     string    `json:"binding"`
	Root        string    `json:"root"`
	Dir         string    `json:"dir"`
	Database    string    `json:"database"`
	// SnapshotTo and Snapshot name P3's pinned snapshot.
	SnapshotTo string `json:"snapshot_to,omitempty"`
	Snapshot   string `json:"snapshot,omitempty"`
	// Forced are the attestations the run was accepted under.
	Forced []string `json:"forced,omitempty"`
	// The S5-G contract (readSplitJournalCityTOML): city.toml's bytes before
	// the split, their sha256, and the sha256 of the bytes the split wrote.
	// CityTOMLPath is the file, through any symlink; the hashes are over it.
	CityTOMLPath         string  `json:"city_toml_path,omitempty"`
	CityTOMLBefore       *string `json:"city_toml_before"`
	CityTOMLBeforeSHA256 string  `json:"city_toml_before_sha256,omitempty"`
	CityTOMLAfterSHA256  string  `json:"city_toml_after_sha256,omitempty"`
	// MinGCVersion is the floor P4 writes; MinGCVersionBefore the file it
	// replaced, null when there was none.
	MinGCVersion       string  `json:"min_gc_version,omitempty"`
	MinGCVersionBefore *string `json:"min_gc_version_before"`
	// AbandonedFrom is the phase an --abandon began from.
	AbandonedFrom string `json:"abandoned_from,omitempty"`
}

func (j storageSplitJournal) target() infraBindingTarget {
	return infraBindingTarget{Binding: j.Binding, Root: j.Root, Dir: j.Dir, Database: j.Database}
}

// open reports a journal whose split is neither finished nor abandoned.
func (j storageSplitJournal) open() bool {
	return j.Phase != splitPhaseComplete && j.Phase != splitPhaseAbandoned
}

func storageSplitJournalPath(cityPath string) string {
	return filepath.Join(cityPath, ".gc", storageSplitJournalName)
}

// readStorageSplitJournal reads the journal; present is false when there is
// none. An unreadable journal is an error, never an absence.
func readStorageSplitJournal(cityPath string) (storageSplitJournal, bool, error) {
	var j storageSplitJournal
	data, err := os.ReadFile(storageSplitJournalPath(cityPath))
	if errors.Is(err, os.ErrNotExist) {
		return j, false, nil
	}
	if err != nil {
		return j, true, fmt.Errorf("reading the split journal %s: %w", storageSplitJournalPath(cityPath), err)
	}
	if err := json.Unmarshal(data, &j); err != nil {
		return j, true, fmt.Errorf("decoding the split journal %s: %w", storageSplitJournalPath(cityPath), err)
	}
	return j, true, nil
}

// writeStorageSplitJournal writes the journal atomically and durably.
func writeStorageSplitJournal(cityPath string, j storageSplitJournal) error {
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return writeRigImportRecord(storageSplitJournalPath(cityPath), string(data))
}

// storageSplitInstruction is the command that starts or continues a split.
func storageSplitInstruction() string {
	return "gc storage " + storageSplitVerb + " --" + storageFleetStoppedFlag
}

// storageSplitAbandonInstruction is the command that backs one out.
func storageSplitAbandonInstruction() string {
	return storageSplitInstruction() + " --" + storageAbandonFlag
}

// splitStorageMatches reports whether cfg authors exactly the split's
// [storage]: the born-split block (bornSplitStorageConfig).
func splitStorageMatches(cfg *config.City) bool {
	return cfg != nil && cfg.Storage != nil && cfg.EffectiveStorage().Equal(splitHypotheticalConfig(cfg).EffectiveStorage())
}

// splitDecision is what the judge makes of the journal and city.toml.
type splitDecision int

const (
	splitFresh splitDecision = iota
	splitResume
	splitRefuse
)

// splitJudgement is the judge's input: the journal (nil when absent),
// whether city.toml composes a [storage] and whether it is the split's, and
// the sha256 of the resolved city.toml.
type splitJudgement struct {
	journal   *storageSplitJournal
	authored  bool
	splitting bool
	sha       string
}

// judgeSplit decides whether a run starts a split, resumes one, or refuses
// (S5-H design §3.2). It reads nothing. note is a line to say on a
// decision that is not a refusal.
func judgeSplit(in splitJudgement) (decision splitDecision, note string, err error) {
	authoredRefusal := fmt.Errorf("this city already authors [storage], so there is no split to write; `%s` and `%s` continue an authored split", storageClearInstruction(), rigImportAllInstruction())
	j := in.journal
	switch {
	case j != nil && j.Phase == splitPhaseComplete && in.authored:
		return splitRefuse, "", fmt.Errorf("the guided split %s completed at %s, so this city is split already; `%s` reports it", j.Run, j.CompletedAt.Format(time.RFC3339), storageStatusInstruction())
	case j == nil || !j.open():
		if in.authored {
			return splitRefuse, "", authoredRefusal
		}
		return splitFresh, "", nil
	case j.Phase == splitPhaseAbandoning:
		return splitRefuse, "", fmt.Errorf("the split %s is being abandoned; finish abandoning it with `%s`", j.Run, storageSplitAbandonInstruction())
	}
	rank, known := splitPhaseRank[j.Phase]
	switch {
	case !known:
		return splitRefuse, "", fmt.Errorf("the split journal %s records a phase this build does not know (%q); it was written by another gc", storageSplitJournalName, j.Phase)
	case rank < splitPhaseRank[splitPhaseConfiguring]:
		if in.authored {
			return splitRefuse, "", fmt.Errorf("the split %s stopped before it wrote city.toml (%s), and city.toml now authors [storage], which the split did not write. Close the split with `%s`, which changes nothing but its journal and its snapshot's pin; then `%s` and `%s` continue the authored split", j.Run, j.Phase, storageSplitAbandonInstruction(), storageClearInstruction(), rigImportAllInstruction())
		}
		return splitFresh, fmt.Sprintf("the split %s stopped before it wrote city.toml (%s), and the city may have run since; starting it over", j.Run, j.Phase), nil
	case j.Phase == splitPhaseConfiguring && !in.authored && in.sha == j.CityTOMLBeforeSHA256:
		return splitResume, "", nil
	case in.authored && in.splitting:
		// At configuring, a file that is not the one the split writes is
		// refused by the edit itself (configure), so it says nothing here.
		if in.sha != j.CityTOMLAfterSHA256 && j.Phase != splitPhaseConfiguring {
			note = fmt.Sprintf("city.toml changed since the split %s wrote it; a later `%s` removes [storage] from it in place and keeps the change", j.Run, storageReverseInstruction(reverseToWork, nil))
		}
		return splitResume, note, nil
	case in.authored:
		return splitRefuse, "", fmt.Errorf("city.toml's [storage] is not the one the split %s wrote; put it back as the journal %s records (city_toml_before plus the block `gc storage preflight --split` prints), or back the split out with `%s`", j.Run, storageSplitJournalName, storageSplitAbandonInstruction())
	}
	return splitFresh, fmt.Sprintf("the split %s stopped at %s and city.toml no longer carries its [storage] (a reverse, or a hand edit, undid it); starting a new split", j.Run, j.Phase), nil
}

// splitCityTOML is the resolved city.toml and its bytes.
type splitCityTOML struct {
	path  string
	bytes []byte
}

func readSplitCityTOML(cityPath string) (splitCityTOML, error) {
	path, err := reverseCityTOMLPath(cityPath)
	if err != nil {
		return splitCityTOML{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return splitCityTOML{}, err
	}
	return splitCityTOML{path: path, bytes: data}, nil
}

// splitOptions is one run's flags.
type splitOptions struct {
	DryRun  bool
	Abandon bool
}

// newStorageSplitCmd is `gc storage split`.
func newStorageSplitCmd(surface storageCommandSurface, stdout, stderr io.Writer) *cobra.Command {
	var (
		opts         splitOptions
		fleetStopped bool
		forced       []string
		timeBudget   time.Duration
	)
	cmd := &cobra.Command{
		Use:          storageSplitVerb,
		Short:        "Split an unsplit city: its infrastructure classes into a binding, its rigs' graph rows with them",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		Long: `Split this city, in one command, the way the runbook does it by hand.

With the city stopped (gc stop) it takes the controller lock and the
maintenance window, rehearses the split (gc storage preflight --split) and
refuses on any block, then: snapshots the work store and the rigs (pinned),
runs legacy's demand-migration repairs when the split city runs the v2
reconciler, appends the [storage] block to city.toml and writes
.gc/min-gc-version, migrates the city's infrastructure classes into the
binding (gc storage migrate --from-work), imports every rig (--from-rigs), and
checks the result (gc storage status, the storage-split doctor check).

It records each step in .gc/storage-split.journal.json, with city.toml's
bytes from before the split, which gc storage migrate --to-work restores.
Run it again to continue a split that stopped: on its time budget (exit 75),
on a rig the import refused, or killed. A split stopped before it wrote
city.toml starts over.

--abandon backs out a split stopped before the city's copy converged: city.toml
gets its bytes from before the split back (or [storage] removed, if it changed
since), the half-copied binding rows are deleted, and the version floor is put
back as it was before the split (removed if there was none). A split stopped
before it wrote city.toml only has its journal closed and its snapshot
unpinned. Past the convergence marker the way back is gc storage migrate
--to-work, which keeps every bead written to the binding since.

--dry-run is gc storage preflight --split.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if opts.Abandon && (opts.DryRun || cmd.Flags().Changed(storageTimeBudgetFlag)) {
				fmt.Fprintf(stderr, "gc %s %s: --%s takes neither --%s nor --%s\n", surface.Namespace, storageSplitVerb, storageAbandonFlag, storageDryRunFlag, storageTimeBudgetFlag) //nolint:errcheck // best-effort stderr
				return errExit
			}
			force, err := parseStorageForce(forced)
			if err != nil {
				fmt.Fprintf(stderr, "gc %s %s: %v\n", surface.Namespace, storageSplitVerb, err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			request, err := resolveStorageOperatorRequest()
			if err != nil {
				fmt.Fprintf(stderr, "gc %s %s: %v\n", surface.Namespace, storageSplitVerb, err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			request.FleetStopped = fleetStopped
			request.Force = force
			request.TimeBudget = timeBudget
			return exitForCode(doStorageSplit(cmd.Context(), request, opts, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&fleetStopped, storageFleetStoppedFlag, false, "attest that "+storageFleetStoppedAttestation)
	cmd.Flags().BoolVar(&opts.DryRun, storageDryRunFlag, false, "rehearse the split (gc storage preflight --split): no lock, nothing written")
	cmd.Flags().BoolVar(&opts.Abandon, storageAbandonFlag, false, "back out a split stopped before the city's copy converged")
	cmd.Flags().DurationVar(&timeBudget, storageTimeBudgetFlag, storageDefaultTimeBudget,
		"stop the city's copy or clear at the next batch boundary once this much time has passed, exit 75, and continue on the next run (0: no budget)")
	addStorageForceFlag(cmd, &forced)
	return cmd
}

// splitRun is one split in progress.
type splitRun struct {
	request   storageOperatorRequest
	journal   storageSplitJournal
	target    infraBindingTarget
	stdout    io.Writer
	stderr    io.Writer
	logPrefix string
}

// advance records phase done.
func (r *splitRun) advance(phase string) error {
	r.journal.Phase = phase
	if phase == splitPhaseComplete {
		r.journal.CompletedAt = time.Now().UTC()
	}
	if err := writeStorageSplitJournal(r.request.CityPath, r.journal); err != nil {
		return fmt.Errorf("recording the split's %s phase: %w", phase, err)
	}
	return infraEngineCrashPoint("split " + phase)
}

// doStorageSplit is `gc storage split`.
func doStorageSplit(ctx context.Context, request storageOperatorRequest, opts splitOptions, stdout, stderr io.Writer) (code int) {
	logPrefix := storageSplitLogName
	fail := func(err error) int {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if opts.DryRun {
		code, _ := doStorageSplitPreflight(request, stdout, stderr)
		return code
	}
	if opts.Abandon {
		return doStorageSplitAbandon(ctx, request, stdout, stderr)
	}
	cityPath := request.CityPath
	if err := storageReverseRefusal(cityPath, storageReverseRoot(cityPath, request.Cfg)); err != nil {
		return fail(err)
	}
	journal, present, err := readStorageSplitJournal(cityPath)
	if err != nil {
		return fail(err)
	}
	toml, err := readSplitCityTOML(cityPath)
	if err != nil {
		return fail(fmt.Errorf("reading city.toml: %w", err))
	}
	in := splitJudgement{authored: request.Cfg.Storage != nil, splitting: splitStorageMatches(request.Cfg), sha: sha256Hex(toml.bytes)}
	if present {
		in.journal = &journal
	}
	decision, note, err := judgeSplit(in)
	if err != nil {
		return fail(err)
	}
	if note != "" {
		fmt.Fprintf(stderr, "%s: %s\n", logPrefix, note) //nolint:errcheck // best-effort stderr
	}
	// The floor the split records is one every gc that reads it obeys, this
	// one included: a gc below it would split the city and then refuse it.
	floor := splitMinGCVersion
	if decision == splitResume && journal.MinGCVersion != "" {
		floor = journal.MinGCVersion
	}
	if err := splitVersionFloorRefusal(cityPath, floor); err != nil {
		return fail(err)
	}
	// A resumed split's binding is the one its journal recorded; a fresh
	// one's, the one the split's [storage] resolves.
	target := journal.target()
	if decision == splitFresh {
		resolved, ok, err := resolveInfraBindingTarget(cityPath, splitHypotheticalConfig(request.Cfg))
		if err != nil {
			return fail(err)
		}
		if !ok {
			return fail(errors.New("the split's [storage] resolves no infrastructure binding"))
		}
		target = resolved
	}

	// P1: the controller lock, the liveness checks (a remote runtime among
	// them: Splits), the advisory ping, the work store's shared-store check,
	// the attestation, then the guard: the order every writing storage verb
	// takes.
	controllerLock, err := acquireStorageVerbExclusion(request, storageVerbScope{Splits: true})
	if err != nil {
		return fail(err)
	}
	defer controllerLock.Close() //nolint:errcheck // released last
	if pid := infraMigrationForeignControllerPID(cityPath); pid != 0 {
		return fail(fmt.Errorf("controller PID %d is live on this city. Stop it (%s) and run this again", pid, storageStopCommand))
	}
	shared, err := sharedWorkStoreRefusal(cityPath, request.Force)
	if err != nil {
		return fail(err)
	}
	defer shared.Release() // the co-tenants stay stopped until the split is done
	if !request.FleetStopped {
		return fail(fmt.Errorf("pass --%s to attest that %s. This command proves the controller is stopped and cannot prove anything about the rest", storageFleetStoppedFlag, storageFleetStoppedAttestation))
	}
	guard, err := acquireStorageMigrationGuard(ctx, cityPath)
	if err != nil {
		return fail(err)
	}
	defer guard.Release() //nolint:errcheck // released before the lock
	if err := infraEngineCrashPoint("split guarded"); err != nil {
		return fail(err)
	}
	// The journal as it is under the lock and the guard: another run may
	// have moved it while this one waited.
	again, againPresent, err := readStorageSplitJournal(cityPath)
	if err != nil {
		return fail(err)
	}
	if againPresent != present || (present && !reflect.DeepEqual(again, journal)) {
		return fail(fmt.Errorf("the split journal %s changed while this run waited for the controller lock and the migration guard; nothing was written. Run this again", storageSplitJournalPath(cityPath)))
	}

	// P5: the window, with every rig the import reads. Closed before the
	// guard and the lock are released.
	window, err := openMigrationSource(controllerLock, cityPath, request.Cfg, logPrefix, stderr, reverseRigRoots(cityPath, request.Cfg)...)
	if err != nil {
		return fail(err)
	}
	defer window.closeReporting(logPrefix, stderr, &code)
	window.setTimeBudget(request.TimeBudget)

	run := &splitRun{request: request, journal: journal, target: target, stdout: stdout, stderr: stderr, logPrefix: logPrefix}
	if decision == splitFresh {
		if err := run.start(ctx, toml, present, shared); err != nil {
			return fail(err)
		}
	}
	if code := run.finish(ctx, shared); code != 0 {
		return code
	}
	// The window closes before the run reports: a provider its stop left
	// running fails the run.
	window.closeReporting(logPrefix, stderr, &code)
	if code != 0 {
		return code
	}
	mode, _ := doctorReconcilerMode(run.request.Cfg, cityPath, nil)
	fmt.Fprintf(stdout, "Split. %s now serves %s from %s, and every rig with a store of its own is imported; the session reconciler is %s. Start the city with `gc start`.\n", //nolint:errcheck // best-effort stdout
		target.Binding, infraMigrationClassList(), target.Database, mode)
	fmt.Fprintf(stdout, "To roll back, keeping every bead written since: `%s`. Snapshot %s of the stores before the split stays pinned in %s.\n", //nolint:errcheck // best-effort stdout
		storageReverseInstruction(reverseToWork, nil), run.journal.Snapshot, run.journal.SnapshotTo)
	return 0
}

// start runs a fresh split up to its city.toml edit: P0, P3, P2 and P4's
// record. The caller holds the lock, the guard and the window.
func (r *splitRun) start(ctx context.Context, toml splitCityTOML, present bool, shared sharedStoreExclusion) error {
	cityPath := r.request.CityPath
	// P0, inside the window, which is what lets it read a stopped city.
	var rehearsal bytes.Buffer
	pcode, summary := doStorageSplitPreflight(r.request, &rehearsal, &rehearsal)
	report, _, _ := strings.Cut(rehearsal.String(), "\nReady. ")
	if pcode != 0 || !summary.ready() {
		fmt.Fprint(r.stdout, rehearsal.String()) //nolint:errcheck // best-effort stdout
		why := summary.Blocks
		if summary.Unread != "" {
			why = append(why, summary.Unread)
		}
		return fmt.Errorf("the rehearsal refused the split (%s); nothing was written", strings.Join(why, "; "))
	}
	fmt.Fprint(r.stdout, report)                                                     //nolint:errcheck // best-effort stdout
	fmt.Fprintf(r.stdout, "\n%s: the rehearsal is ready; splitting.\n", r.logPrefix) //nolint:errcheck // best-effort stdout

	// The old journal goes aside, so the live one is always this split's. A
	// superseded split that wrote the floor leaves it behind, so the floor
	// from before the split is the one that split recorded, not the file now.
	floorBefore, floorFromOld := (*string)(nil), false
	if present {
		old := r.journal
		if old.open() && splitPhaseRank[old.Phase] >= splitPhaseRank[splitPhaseConfiguring] {
			floorBefore, floorFromOld = old.MinGCVersionBefore, true
		}
		if err := r.retireJournal(); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	r.journal = storageSplitJournal{
		Format: storageSplitJournalFormat, Run: reverseRunID(now), StartedAt: now,
		Binding: r.target.Binding, Root: r.target.Root, Dir: r.target.Dir, Database: r.target.Database,
		Forced: sortedUnique(r.request.Force.acceptedHazards(shared)),
	}

	// P3: the snapshot, before anything is written, pinned.
	opts := snapshotOptions{Work: true, Rigs: true, To: storageSnapshotDefaultDir(cityPath), Keep: storageSnapshotDefaultKeep, NoRetention: true}
	identity, err := currentWorkStoreIdentity(cityPath)
	if err != nil {
		return err
	}
	id, _, err := takeStorageSnapshot(ctx, r.request, opts, r.target, identity, r.stdout, r.stderr, r.logPrefix)
	if err != nil {
		return fmt.Errorf("the mandatory snapshot: %w", err)
	}
	// The journal names the snapshot before it is pinned, so no pinned
	// snapshot is ever left that no journal names (a kill between the two
	// leaves it unpinned, and retention prunes it).
	r.journal.SnapshotTo, r.journal.Snapshot = opts.To, id
	if err := r.advance(splitPhaseSnapshotted); err != nil {
		return err
	}
	if err := pinStorageSnapshot(opts.To, id, fmt.Sprintf("the stores before the guided split %s; `%s` unpins it", r.journal.Run, storageSplitAbandonInstruction())); err != nil {
		return fmt.Errorf("pinning the mandatory snapshot %s: %w", id, err)
	}

	// P2: the repairs legacy's demand pass applies every tick, once, before
	// the copy, for a split city that runs v2 (which does not apply them).
	if mode, _ := doctorReconcilerMode(splitHypotheticalConfig(r.request.Cfg), cityPath, nil); mode == reconcilerV2 {
		splitDemandRepairs(r.request.Cfg, cityPath, r.stderr)
	}
	if err := r.advance(splitPhaseReadied); err != nil {
		return err
	}

	// P4, its record: the bytes before, read here under the lock and the
	// guard, the sha256 of the bytes after, and the floor, proven to compose
	// to the split before anything is written. The bytes are the ones the
	// judge and the rehearsal read, or the run refuses.
	current, err := readSplitCityTOML(cityPath)
	if err != nil {
		return fmt.Errorf("reading city.toml: %w", err)
	}
	if sha256Hex(current.bytes) != sha256Hex(toml.bytes) {
		return fmt.Errorf("%s changed while this run rehearsed the split; nothing was written to it. Run this again", current.path)
	}
	after, err := splitCityTOMLAfter(cityPath, current.bytes, r.request.Cfg)
	if err != nil {
		return err
	}
	before := string(current.bytes)
	r.journal.CityTOMLPath = current.path
	r.journal.CityTOMLBefore = &before
	r.journal.CityTOMLBeforeSHA256 = sha256Hex(current.bytes)
	r.journal.CityTOMLAfterSHA256 = sha256Hex(after)
	r.journal.MinGCVersion = splitMinGCVersion
	switch data, err := os.ReadFile(minGCVersionPath(cityPath)); {
	case floorFromOld:
		r.journal.MinGCVersionBefore = floorBefore
	case err == nil:
		floor := string(data)
		r.journal.MinGCVersionBefore = &floor
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	return r.advance(splitPhaseConfiguring)
}

// retireJournal moves the journal of an earlier split aside, as
// .completed-<run> or .abandoned-<run>, or as .superseded-<run> for one
// that stopped and was undone, whose pinned snapshot it unpins: the stores
// have changed since.
func (r *splitRun) retireJournal() error {
	old := r.journal
	state := "superseded"
	switch old.Phase {
	case splitPhaseComplete:
		state = "completed"
	case splitPhaseAbandoned:
		state = "abandoned"
	default:
		unpinSplitSnapshot(old)
	}
	path := storageSplitJournalPath(r.request.CityPath)
	if err := os.Rename(path, path+"."+state+"-"+old.Run); err != nil {
		return fmt.Errorf("moving the earlier split's journal aside: %w", err)
	}
	syncInfraDir(filepath.Dir(path))
	return nil
}

// unpinSplitSnapshot lets retention prune a split's snapshot.
func unpinSplitSnapshot(j storageSplitJournal) {
	if j.SnapshotTo != "" && j.Snapshot != "" {
		_ = os.Remove(filepath.Join(j.SnapshotTo, j.Snapshot, storageSnapshotPinName))
	}
}

// splitCityTOMLAfter is city.toml with the split's [storage] appended, in
// place (config.AppendStorageToCityTOML), proven to compose, fragments and
// includes as they are on disk, to exactly the split.
func splitCityTOMLAfter(cityPath string, before []byte, cfg *config.City) ([]byte, error) {
	after, err := config.AppendStorageToCityTOML(before, []byte(splitStorageTOML(splitHypotheticalConfig(cfg))))
	if err != nil {
		return nil, fmt.Errorf("appending [storage] to city.toml: %w", err)
	}
	path := filepath.Join(cityPath, "city.toml")
	composed, err := loadCityConfigWithoutBuiltinPackRefreshFS(reverseOverlayFS{FS: fsys.OSFS{}, path: path, data: after}, path, io.Discard)
	if err != nil {
		return nil, fmt.Errorf("city.toml with the split's [storage] would not load: %w", err)
	}
	if !splitStorageMatches(composed) {
		return nil, errors.New("city.toml with the split's [storage] composes another [storage]: a fragment it includes declares one, and a fragment is the operator's to edit")
	}
	if shape, _ := storageSplitShapeOf(composed.EffectiveStorage()); shape != storageSplitWhole {
		return nil, errors.New("city.toml with the split's [storage] does not compose a whole split")
	}
	return after, nil
}

// finish runs a split from its journal's phase to the end: P4's writes, P6,
// P8 and P9. It returns the exit code, having said why on a non-zero one.
func (r *splitRun) finish(ctx context.Context, shared sharedStoreExclusion) int {
	cityPath := r.request.CityPath
	fail := func(err error) int {
		fmt.Fprintf(r.stderr, "%s: %v. Run `%s` to continue", r.logPrefix, err, storageSplitInstruction()) //nolint:errcheck // best-effort stderr
		if splitAbandonRefusal(r.journal) == nil {
			fmt.Fprintf(r.stderr, ", or `%s` to back it out", storageSplitAbandonInstruction()) //nolint:errcheck // best-effort stderr
		}
		fmt.Fprintln(r.stderr) //nolint:errcheck // best-effort stderr
		return 1
	}
	if r.journal.Phase == splitPhaseConfiguring {
		if err := r.configure(); err != nil {
			return fail(err)
		}
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		return fail(fmt.Errorf("reloading city.toml: %w", err))
	}
	if !splitStorageMatches(cfg) {
		return fail(errors.New("city.toml does not compose the split's [storage]"))
	}
	r.request.Cfg = cfg
	plan, err := resolveCityStoragePlan(cityPath, cfg)
	if err != nil {
		return fail(err)
	}

	if splitPhaseRank[r.journal.Phase] < splitPhaseRank[splitPhaseCityConverged] {
		_, code := runCityMigration(ctx, r.request, r.target, plan, shared, r.logPrefix, r.stderr)
		if code == storagePausedExitCode {
			return code
		}
		if code != 0 {
			return fail(errors.New("the city migration did not converge"))
		}
		if err := r.advance(splitPhaseCityConverged); err != nil {
			return fail(err)
		}
	}
	if splitPhaseRank[r.journal.Phase] < splitPhaseRank[splitPhaseRigsImported] {
		// The import needs the city's binding converged (S4b): the journal
		// says the city's copy was done, and the binding must still show it.
		state, err := readInfraConvergenceState(r.target)
		if err != nil {
			return fail(err)
		}
		if state != infraConvergenceMarked {
			return fail(fmt.Errorf("the split recorded the city's copy done, but the binding %s does not hold it now (no convergence marker, or its database is gone), and a rig is never imported onto a binding that does not hold the city's state; nothing was imported. `%s` names the repair", r.target.Binding, storageStatusInstruction()))
		}
		rigs, err := selectRigsForImport(cfg, nil)
		if err != nil {
			return fail(err)
		}
		if importRigs(r.request, r.target, rigs, r.stdout, r.stderr) != 0 {
			return fail(errors.New("a rig's import did not finish; the city is split and serves, and the rigs reported above stay readable until their import"))
		}
		if err := r.advance(splitPhaseRigsImported); err != nil {
			return fail(err)
		}
	}

	// P9: the split as `gc storage status` and `gc doctor` judge it.
	var status bytes.Buffer
	if doStorageStatus(r.request, &status, &status) != 0 {
		fmt.Fprint(r.stdout, status.String()) //nolint:errcheck // best-effort stdout
		return fail(fmt.Errorf("`%s` does not pass", storageStatusInstruction()))
	}
	check := newStorageSplitCheck(cfg, cityPath).Run(nil)
	switch check.Status {
	case doctor.StatusError:
		return fail(fmt.Errorf("the storage-split doctor check: %s", check.Message))
	case doctor.StatusWarning:
		fmt.Fprintf(r.stderr, "%s: the storage-split doctor check warns: %s\n", r.logPrefix, check.Message) //nolint:errcheck // best-effort stderr
	}
	if err := r.advance(splitPhaseComplete); err != nil {
		return fail(err)
	}
	return 0
}

// configure is P4's writes: the floor, then city.toml, each only if it is not
// already written. The journal holds the bytes before and the sha256 after.
func (r *splitRun) configure() error {
	cityPath := r.request.CityPath
	if err := raiseMinGCVersion(cityPath, r.journal.MinGCVersion); err != nil {
		return fmt.Errorf("writing %s: %w", minGCVersionPath(cityPath), err)
	}
	if err := infraEngineCrashPoint("split floor"); err != nil {
		return err
	}
	current, err := os.ReadFile(r.journal.CityTOMLPath)
	if err != nil {
		return err
	}
	switch sha256Hex(current) {
	case r.journal.CityTOMLBeforeSHA256, r.journal.CityTOMLAfterSHA256:
	default:
		return fmt.Errorf("%s is neither the file the split recorded before its edit nor the one it writes, so this run cannot tell its edit from yours; put it back as the journal %s records it (city_toml_before), or back the split out with `%s`", r.journal.CityTOMLPath, storageSplitJournalPath(cityPath), storageSplitAbandonInstruction())
	}
	if sha256Hex(current) == r.journal.CityTOMLBeforeSHA256 {
		after, err := splitCityTOMLAfter(cityPath, current, r.request.Cfg)
		if err != nil {
			return err
		}
		if sum := sha256Hex(after); sum != r.journal.CityTOMLAfterSHA256 {
			// Nothing is written yet, so the record follows the edit.
			r.journal.CityTOMLAfterSHA256 = sum
			if err := writeStorageSplitJournal(cityPath, r.journal); err != nil {
				return err
			}
		}
		if err := writeCityTOMLAtomic(r.journal.CityTOMLPath, after); err != nil {
			return fmt.Errorf("writing city.toml: %w", err)
		}
		fmt.Fprintf(r.stderr, "%s: city.toml: [storage] appended; its bytes from before the split are in %s\n", r.logPrefix, storageSplitJournalPath(cityPath)) //nolint:errcheck // best-effort stderr
		if err := infraEngineCrashPoint("split city.toml"); err != nil {
			return err
		}
	}
	return r.advance(splitPhaseConfigured)
}

// splitVersionFloorRefusal refuses a split by a gc below the floor it would
// leave in .gc/min-gc-version: floor, or the file's own when that is higher.
// Every gc that reads the file refuses the city below it, so such a gc would
// split the city and then refuse to run it. A gc whose version does not
// compare (dev, a Go pseudo-version) is not refused, as at boot.
func splitVersionFloorRefusal(cityPath, floor string) error {
	current, present, err := readMinGCVersion(cityPath)
	if err != nil {
		return err
	}
	if present && deps.CompareVersions(current, floor) > 0 {
		floor = current
	}
	if running := gcVersionForFloor(); gcVersionBelowFloor(running, floor) {
		return fmt.Errorf("this gc is %s, below the version floor %s the split leaves in %s, which every gc that reads it obeys: it would split the city and then refuse to run it. Split with gc %s or later; nothing was written", running, floor, minGCVersionPath(cityPath), floor)
	}
	return nil
}

// raiseMinGCVersion writes floor to the city's .gc/min-gc-version unless it
// already holds one at least as high: the split never lowers a floor. An
// unreadable floor refuses.
func raiseMinGCVersion(cityPath, floor string) error {
	current, present, err := readMinGCVersion(cityPath)
	if err != nil {
		return err
	}
	if present && deps.CompareVersions(current, floor) >= 0 {
		return nil
	}
	return writeMinGCVersion(cityPath, floor)
}

// splitDemandRepairs runs legacy's demand-migration repairs over the city's
// work legs, on the stores the window opens, through doctor's per-run store
// factory, says what the sweep could not read, and closes the stores it
// opened; a test seam.
//
// The sweep reads the sessions through cliSessionStore, whose route cache
// (cliStorageRoutes) remembers this city's routes for the process as they
// are now, before P4 writes [storage]. Nothing later in the split reads
// sessions through it; a step added after P4 that does must not trust it.
var splitDemandRepairs = func(cfg *config.City, cityPath string, stderr io.Writer) {
	// Every store the sweep opens is recorded as the factory opens it, and
	// exactly those are closed after: the sweep's own scope rules decide
	// what it opens, and nothing here repeats them.
	var opened []string
	open := perRunStoreFactory(openStoreForCity(cityPath), func(dir string) { opened = append(opened, dir) })
	swept := newV2DemandMigrationsCheck(cfg, cityPath, open).sweep(true, stderr)
	for _, problem := range swept.problems {
		fmt.Fprintf(stderr, "%s: demand repairs: %s\n", storageSplitLogName, problem) //nolint:errcheck // best-effort stderr
	}
	// The factory memoizes each scope and reports it once, so asking it
	// again for one the sweep opened hands back that handle, which is
	// closed here.
	for _, scope := range opened {
		if store, err := open(scope); err == nil {
			_ = closeBeadStoreHandle(store)
		}
	}
}

// doStorageSplitAbandon is `gc storage split --abandon`: it backs out a split
// that stopped before the city copy's convergence marker, recording each
// step in the journal so a killed abandon is finished by the next.
func doStorageSplitAbandon(ctx context.Context, request storageOperatorRequest, stdout, stderr io.Writer) int {
	logPrefix := storageSplitLogName + " --" + storageAbandonFlag
	fail := func(err error) int {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cityPath := request.CityPath
	judge := func() (storageSplitJournal, error) {
		j, present, err := readStorageSplitJournal(cityPath)
		switch {
		case err != nil:
			return j, err
		case !present || !j.open():
			return j, errors.New("no split is under way, so there is nothing to abandon")
		}
		return j, splitAbandonRefusal(j)
	}
	journal, err := judge()
	if err != nil {
		return fail(err)
	}
	controllerLock, err := acquireStorageVerbExclusion(request, storageVerbScope{})
	if err != nil {
		return fail(err)
	}
	defer controllerLock.Close() //nolint:errcheck // released last
	if pid := infraMigrationForeignControllerPID(cityPath); pid != 0 {
		return fail(fmt.Errorf("controller PID %d is live on this city. Stop it (%s) and run this again", pid, storageStopCommand))
	}
	if !request.FleetStopped {
		return fail(fmt.Errorf("pass --%s to attest that %s", storageFleetStoppedFlag, storageFleetStoppedAttestation))
	}
	guard, err := acquireStorageMigrationGuard(ctx, cityPath)
	if err != nil {
		return fail(err)
	}
	defer guard.Release() //nolint:errcheck // released before the lock
	if err := infraEngineCrashPoint("split abandon guarded"); err != nil {
		return fail(err)
	}
	again, err := judge()
	if err != nil {
		return fail(err)
	}
	if !reflect.DeepEqual(again, journal) {
		return fail(fmt.Errorf("the split journal %s changed while this run waited for the controller lock and the migration guard; nothing was written. Run this again", storageSplitJournalPath(cityPath)))
	}

	r := &splitRun{request: request, journal: journal, target: journal.target(), stdout: stdout, stderr: stderr, logPrefix: logPrefix}
	if journal.Phase != splitPhaseAbandoning {
		r.journal.AbandonedFrom = journal.Phase
		if err := r.advance(splitPhaseAbandoning); err != nil {
			return fail(err)
		}
	}
	// A split that stopped before its city.toml record (P4a) wrote no
	// [storage], no floor and no binding row: whatever the city holds of
	// those is someone else's, so the abandon closes the journal and unpins
	// the snapshot, and touches nothing else.
	if splitPhaseRank[r.journal.AbandonedFrom] < splitPhaseRank[splitPhaseConfiguring] {
		unpinSplitSnapshot(r.journal)
		if err := r.advance(splitPhaseAbandoned); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "Abandoned the split %s (stopped at %s, before it wrote city.toml): its journal is closed and snapshot %s in %s is unpinned; nothing else was changed.\n", //nolint:errcheck // best-effort stdout
			r.journal.Run, r.journal.AbandonedFrom, r.journal.Snapshot, r.journal.SnapshotTo)
		return 0
	}
	how, err := r.abandonCityTOML()
	if err != nil {
		return fail(err)
	}
	if err := infraEngineCrashPoint("split abandon city.toml"); err != nil {
		return fail(err)
	}
	removed, err := r.abandonBindingRows()
	if err != nil {
		return fail(err)
	}
	if err := infraEngineCrashPoint("split abandon rows"); err != nil {
		return fail(err)
	}
	if err := r.abandonFloor(); err != nil {
		return fail(err)
	}
	if err := infraEngineCrashPoint("split abandon floor"); err != nil {
		return fail(err)
	}
	unpinSplitSnapshot(r.journal)
	if err := r.advance(splitPhaseAbandoned); err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "Abandoned the split %s (stopped at %s): city.toml %s; %d half-copied binding row(s) deleted; the version floor %s. Every class is on the work store; start the city with `gc start`. Snapshot %s in %s is unpinned.\n", //nolint:errcheck // best-effort stdout
		r.journal.Run, r.journal.AbandonedFrom, how, removed, r.floorHow(), r.journal.Snapshot, r.journal.SnapshotTo)
	return 0
}

// splitAbandonRefusal says why the open split j cannot be abandoned, or nil:
// a split that stopped before P4a can always be (it wrote nothing to back
// out); past that, once the city's copy has converged (the journal past it, or a marker,
// even a stale one, beside the binding), the binding is the city's
// infrastructure state, and backing the split out by deleting it would lose
// it. The split's failure hint and --abandon both ask it.
func splitAbandonRefusal(j storageSplitJournal) error {
	// A split that stopped before its city.toml record wrote no binding row:
	// a marker there now is the operator's own migration, not this split's
	// copy, and the abandon touches none of it (it closes the journal).
	from := j.Phase
	if from == splitPhaseAbandoning {
		from = j.AbandonedFrom
	}
	if splitPhaseRank[from] < splitPhaseRank[splitPhaseConfiguring] {
		return nil
	}
	state, err := readInfraConvergenceState(j.target())
	if err != nil {
		return err
	}
	if splitPhaseRank[j.Phase] < splitPhaseRank[splitPhaseCityConverged] && state == infraConvergenceAbsent {
		return nil
	}
	return fmt.Errorf("the split %s converged the city's copy into binding %s (%s): the binding is this city's infrastructure state now, so it is not backed out by deleting it. Finish the split with `%s`, then reverse it with `%s`, which keeps every bead written since", j.Run, j.Binding, j.target().MarkerPath(), storageSplitInstruction(), storageReverseInstruction(reverseToWork, nil))
}

// abandonCityTOML takes [storage] back out of city.toml: the bytes from
// before the split when the file is still the one the split wrote and the
// recorded bytes prove against their sha256, otherwise the section removed in
// place, and nothing when it carries none.
func (r *splitRun) abandonCityTOML() (string, error) {
	cityPath := r.request.CityPath
	toml, err := readSplitCityTOML(cityPath)
	if err != nil {
		return "", err
	}
	j := r.journal
	// The bytes are restored only when they prove: a file JSON could not
	// carry byte for byte (invalid UTF-8) loses [storage] in place instead.
	if j.CityTOMLBefore != nil && sha256Hex([]byte(*j.CityTOMLBefore)) == j.CityTOMLBeforeSHA256 && sha256Hex(toml.bytes) == j.CityTOMLAfterSHA256 {
		if err := writeCityTOMLAtomic(toml.path, []byte(*j.CityTOMLBefore)); err != nil {
			return "", err
		}
		return "has its bytes from before the split back", nil
	}
	declared, err := config.CityTOMLDeclaresStorage(toml.bytes)
	if err != nil {
		return "", err
	}
	if !declared {
		return "carries no [storage] (unchanged)", nil
	}
	removed, err := config.RemoveStorageFromCityTOML(toml.bytes)
	if err != nil {
		return "", err
	}
	if err := reverseCheckCityTOML(cityPath, removed, ""); err != nil {
		return "", err
	}
	if err := writeCityTOMLAtomic(toml.path, removed); err != nil {
		return "", err
	}
	return "has [storage] removed in place (it changed since the split wrote it)", nil
}

// abandonBindingRows deletes the rows the city copy wrote into the binding
// before its marker, and the copy manifest. A row the copy did not write is
// refused, and nothing is deleted (prepareInfraDestination's rule).
func (r *splitRun) abandonBindingRows() (int, error) {
	present, err := infraPathExists(r.target.Database)
	if err != nil || !present {
		return 0, err
	}
	binding, err := openInfraDestination(r.target)
	if err != nil {
		return 0, fmt.Errorf("opening the binding to delete the half-copied rows: %w", err)
	}
	defer closeBeadStoreHandle(binding) //nolint:errcheck // best-effort close
	prepared, err := prepareInfraDestination(binding, nil)
	if err != nil {
		return 0, fmt.Errorf("deleting the half-copied binding rows: %w", err)
	}
	if err := os.Remove(r.target.ManifestPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	return prepared.stamped, nil
}

// abandonFloor puts .gc/min-gc-version back as it was before the split.
func (r *splitRun) abandonFloor() error {
	path := minGCVersionPath(r.request.CityPath)
	if r.journal.MinGCVersionBefore != nil {
		return fsys.WriteFileAtomic(fsys.OSFS{}, path, []byte(*r.journal.MinGCVersionBefore), 0o644)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (r *splitRun) floorHow() string {
	if r.journal.MinGCVersionBefore != nil {
		return "is back as it was"
	}
	return "is removed"
}
