package main

// The stranded-write repair: how the infrastructure beads a converged city's
// proven copy never carried get into the binding that is already serving.
//
// This is the recovery half of the hazard infra_class_migrate.go documents
// under "Writers, and the stranded-write window". A write that lands in the
// retained work store after the equality stage is absent from the binding, the
// per-boot containment re-check names it, and the boot refuses. The refusal
// used to tell the operator to recover the named beads into the binding's
// database and then stop — there was no command that did it, and the alarm
// named no verb. This is that command.
//
// # Why this is not `gc storage migrate` run twice
//
// The migration is one-shot BY DESIGN, and the gate is a safety property rather
// than an accident of sequencing. Two independent holds make re-running it
// wrong on a converged city:
//
//   - runInfraClassMigration reads the convergence state first and hands a
//     marked city straight to confirmInfraConvergence. It never re-copies.
//   - If the marker were removed to force the copy, prepareInfraDestination
//     would either refuse (any destination row without the migration's own
//     provenance stamp is content the migration did not write) or, on a
//     destination it did write, DELETE every stamped row and re-import from a
//     source that no longer holds them. The second arm is the loss, and it has
//     been measured rather than imagined: on the city this command was built
//     for, the binding held 51,127 rows and the retained source held 1,452, so
//     forcing a re-copy would have replaced the former with the latter. The
//     retained source's infrastructure slice shrinks over a city's life —
//     the whole point of the retained-source design is that the binding, not
//     the source, is authoritative after cutover.
//
// And the equality stage cannot pass on a live city at all: verifyInfraCopy
// demands the destination hold EXACTLY the source's infrastructure slice, while
// a serving binding legitimately grows beads the source never had.
//
// So this does not defeat that gate, and nothing here should be refactored into
// it. It is a scoped repair built from the same primitives, and everything it
// moves is additive: it copies only ids the binding does not hold and the
// manifest does not record, it never deletes a destination row, and it never
// writes to the source.
//
// # What it will not guess at
//
// The gap it repairs is classified by exactly the rule confirmInfraConvergence
// uses, against exactly the manifest that check reads, using coordclass as the
// classification authority — because a repair that disagreed with the guard
// would move beads and leave the boot refusing anyway.
//
// A bead whose class this cannot state is reported rather than moved: an
// infrastructure classification outside the classes the split relocates, a row
// whose class would change in the crossing, or a row whose dependency topology
// the source cannot state, is a bead nobody can say belongs in this binding in
// the shape it would arrive in. Those are named and the command exits non-zero.
//
// # Copy before delete, verify before record
//
// Nothing is deleted, from either side. The source keeps its rows verbatim, as
// it does after the migration, so a bad outcome here is a duplicate rather than
// a loss. The manifest — the record every later boot classifies absence
// against — is extended only after every moved bead has been re-read from a
// CLOSED AND REOPENED destination and proven field-, class- and dep-equal to
// the source, by the migration's own comparators.
//
// # Idempotence
//
// Re-running is a no-op: the gap is recomputed from live state, a bead already
// in the binding is skipped, DepAdd replaces an edge in place rather than
// duplicating it, and the manifest is rewritten as a superset. A partial
// failure leaves the binding holding what it managed to copy and the manifest
// unextended, which is the safe residue — those ids are still stranded, still
// named by the next check, and the next run copies the rest.
//
// # The rig census is deliberately not repeated here
//
// doStorageMigrate refuses while any rig scope holds an infrastructure bead,
// because the copy's source is the city work store and a rig-scoped row would
// be silently omitted from a cutover that then declares itself proven. This
// repair makes no such declaration: it extends a manifest with ids it copied
// out of the city work store and asserts nothing about any other scope, and the
// containment re-check it feeds reads the same one store. Refusing on a rig
// stray would block the repair of a real strand over a bead that is outside
// both the repair's reach and the check's.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// infraRecoverableClasses is the closed set of classes this repair will move,
// derived from the migration's own class list so the two cannot drift.
//
// It is keyed by coordclass.Class rather than by config.StorageClass because
// coordclass is what decides which store a bead belongs in — it is the
// authority readInfraSnapshot selects the gap with and the boot guard refuses
// on. A repair that classified by any other rule could move a bead the guard
// still counts as stranded, and the city would keep refusing to boot.
func infraRecoverableClasses() map[coordclass.Class]bool {
	byName := make(map[string]bool, len(infraMigrationClasses))
	for _, class := range infraMigrationClasses {
		byName[string(class)] = true
	}
	allowed := make(map[coordclass.Class]bool, len(infraMigrationClasses))
	for _, class := range coordclass.Classes() {
		if byName[class.String()] {
			allowed[class] = true
		}
	}
	return allowed
}

// newStorageRecoverCmd is the leaf the stranded refusal names.
func newStorageRecoverCmd(surface storageCommandSurface, stdout, stderr io.Writer) *cobra.Command {
	var (
		fromWork     bool
		fleetStopped bool
		dryRun       bool
		dumpPath     string
	)
	cmd := &cobra.Command{
		Use:          surface.Verb,
		Short:        "Copy stranded infrastructure beads from the retained work store into the converged binding",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		Long: `Copy the infrastructure beads a converged city's proven copy never carried
out of the retained work store and into the binding that is already serving.

This is the recovery the stranded-write refusal names. It is additive: it moves
only ids the binding does not hold and the proven-copy manifest does not record,
it deletes nothing from either store, and it extends the manifest only after
every moved bead has been proven equal against a closed and reopened
destination. A bead whose class it cannot state is named and left where it is.

It refuses on a city that has NOT converged — the whole copy is still owed
there, and ` + "`" + storageMigrationCommand + "`" + ` is what owes it. It is not that
command run twice: the migration is one-shot on purpose, and forcing it to
re-copy would re-import a serving binding from a source that no longer holds
what the binding does.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !fromWork {
				fmt.Fprintf(stderr, "gc %s %s: pass --%s. The source is stated explicitly rather than detected, exactly as the migration states it\n", //nolint:errcheck // best-effort stderr
					surface.Namespace, surface.Verb, surface.Flag)
				return errExit
			}
			request, err := resolveStorageOperatorRequest()
			if err != nil {
				fmt.Fprintf(stderr, "gc %s %s: %v\n", surface.Namespace, surface.Verb, err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			request.FleetStopped = fleetStopped
			return exitForCode(doStorageRecoverStranded(cmd.Context(), request, strandedRecoveryOptions{DryRun: dryRun, DumpPath: dumpPath}, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&fromWork, surface.Flag, false,
		"recover the stranded infrastructure beads out of this city's work store")
	cmd.Flags().BoolVar(&fleetStopped, storageFleetStoppedFlag, false,
		"attest that "+storageFleetStoppedAttestation)
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"report the gap and write the dump without touching the binding or the manifest")
	cmd.Flags().StringVar(&dumpPath, "dump", "",
		"write every stranded bead and its source dep edges to this JSON file before any write")
	return cmd
}

// strandedRecoveryOptions are the two knobs the repair carries.
type strandedRecoveryOptions struct {
	// DryRun reports and dumps without writing to the binding or the manifest.
	DryRun bool
	// DumpPath, when set, receives every stranded bead and its source dep edges
	// as JSON before any write happens.
	DumpPath string
}

// strandedBeadDump is one stranded bead and the source dep edges it carries, as
// written to the pre-write dump.
//
// It carries beads.Bead itself rather than a projection of it. The dump is a
// local operator artifact — the input to a repair, captured before the repair
// changes anything — not a wire type, and a projection would be a second
// definition of "what a bead is" that could silently omit the field an
// investigation needed.
type strandedBeadDump struct {
	Bead      beads.Bead  `json:"bead"`
	Class     string      `json:"class"`
	Deps      []beads.Dep `json:"deps"`
	DepSource string      `json:"dep_source"`
	DepError  string      `json:"dep_error,omitempty"`
}

// sourceDepReader answers "what does this bead depend on" against a work store
// that may not implement the relation read at all.
//
// The bd/Postgres backend answers DepList with `operation "IssueRelations" not
// supported by the postgres backend`, and that is a fact about the adapter
// rather than about the city: the same rows are still carried INLINE on every
// bead the store lists, in beads.Bead.Dependencies, because bd's own list JSON
// emits a `dependencies` array beside `dependency_count`. So the reader prefers
// the explicit relation read and falls back to the inline projection.
//
// (importInfraSnapshot and verifyInfraCopy both call DepList unconditionally,
// so the MIGRATION cannot run against such a source at all. That is a real gap
// on the migration path and it is tracked as its own bead rather than fixed
// here — this file is the repair, and widening the migration's source contract
// is a change to the one-shot cutover's proof.)
//
// The fallback is only trustworthy if the inline projection is actually LIVE on
// this adapter — an adapter that simply never populated the field would answer
// "no edges" for every bead and the fallback would silently drop the whole
// topology. So it is not assumed: newSourceDepReader witnesses the projection
// against the store's full contents and refuses to fall back at all unless it
// found at least one bead carrying an inline edge. An unwitnessed fallback
// makes every bead ambiguous rather than making every bead look edge-free.
type sourceDepReader struct {
	source beads.Store
	// relationsOK is false once DepList has refused; the reader stops asking.
	relationsOK bool
	// relationsErr is what DepList refused with, kept for the report.
	relationsErr error
	// inlineWitnessed is true when some bead in the source carries an inline
	// dep edge, which is what licenses reading an empty inline slice as "this
	// bead has no edges" rather than as "this adapter reports none".
	inlineWitnessed bool
	// witnessID names the bead that proved the projection, so the claim is
	// falsifiable from the report alone.
	witnessID string
}

// newSourceDepReader probes the relation read once and, if it is unsupported,
// witnesses the inline projection over the store's full contents.
func newSourceDepReader(source beads.Store, probeID string) (*sourceDepReader, error) {
	reader := &sourceDepReader{source: source, relationsOK: true}
	if probeID == "" {
		return reader, nil
	}
	if _, err := source.DepList(probeID, "down"); err != nil {
		reader.relationsOK = false
		reader.relationsErr = err
		rows, listErr := source.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
		if listErr != nil {
			return nil, fmt.Errorf("witnessing the source's inline dependency projection: %w", listErr)
		}
		for _, b := range rows {
			if len(b.Dependencies) > 0 {
				reader.inlineWitnessed = true
				reader.witnessID = b.ID
				break
			}
		}
	}
	return reader, nil
}

// deps returns one bead's outbound edges and whether the answer is trustworthy.
// ok=false means nothing here can state this bead's topology, and the caller
// must refuse to move it rather than move it edge-free.
func (r *sourceDepReader) deps(b beads.Bead) (edges []beads.Dep, ok bool, err error) {
	if r.relationsOK {
		listed, listErr := r.source.DepList(b.ID, "down")
		if listErr != nil {
			return nil, false, listErr
		}
		return listed, true, nil
	}
	if len(b.Dependencies) > 0 {
		// The inline projection carried edges, so it is live for this bead
		// whatever the corpus witness said.
		return b.Dependencies, true, nil
	}
	if len(b.Needs) > 0 {
		// Create-time shorthand with no materialized rows behind it that this
		// reader can resolve. Refuse rather than drop it.
		return nil, false, fmt.Errorf("carries %d unresolved needs shorthand and this source cannot list relations: %w", len(b.Needs), r.relationsErr)
	}
	if !r.inlineWitnessed {
		return nil, false, fmt.Errorf("nothing in this source carries an inline dependency projection, so an empty answer is not evidence of an empty topology, and it cannot list relations: %w", r.relationsErr)
	}
	return nil, true, nil
}

// describe states how this reader answered, for the operator report.
func (r *sourceDepReader) describe() string {
	if r.relationsOK {
		return "source dep edges: read through the store's relation listing"
	}
	if r.inlineWitnessed {
		return fmt.Sprintf("source dep edges: the store refuses relation listing (%v), so edges are read from the inline per-bead projection, witnessed live on %s", r.relationsErr, r.witnessID)
	}
	return fmt.Sprintf("source dep edges: UNREADABLE — the store refuses relation listing (%v) and no inline projection was witnessed", r.relationsErr)
}

// doStorageRecoverStranded performs the repair, in the order the hazards demand.
//
// Structural validation first, because a plan boot would refuse must not be
// repaired toward; then the served-binding note, because a repair into a
// binding that is not the one serving is a copy into an orphan; then
// convergence, because this is a repair of a copy that happened rather than a
// substitute for one that did not; then the writers; then the guard.
func doStorageRecoverStranded(ctx context.Context, request storageOperatorRequest, opts strandedRecoveryOptions, stdout, stderr io.Writer) int {
	// The spelling without its source flag, exactly as doStorageMigrate does it.
	// TestStorageRecoveryCommandNamesTheVerbTheTreeCarries pins it against
	// storageRecoveryCommand so a rename of the verb cannot leave diagnostics
	// naming a command the binary no longer has.
	const logPrefix = "gc storage recover-stranded"

	target, ok, err := resolveInfraBindingTarget(request.CityPath, request.Cfg)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !ok {
		fmt.Fprintf(stderr, "%s: this city's [storage.classes] do not assign %s to one shared non-work binding this build can serve, so there is no binding to recover into and nothing here is stranded. %s\n", //nolint:errcheck // best-effort stderr
			logPrefix, infraMigrationClassList(), storageSupportedTopologyStatement)
		return 1
	}
	if _, err := resolveCityStoragePlan(request.CityPath, request.Cfg); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if blocked, held := servedBindingNoteHold(request.CityPath, target.Binding, config.StorageProviderSQLiteBeads, target.Database); held {
		blocked.Target = target
		fmt.Fprintln(stderr, infraMigrationOperatorAdvice(blocked, logPrefix)) //nolint:errcheck // best-effort stderr
		return 1
	}

	state, err := readInfraConvergenceState(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if state != infraConvergenceMarked {
		fmt.Fprintf(stderr, "%s: this city has not converged onto binding %q (%s is absent or its database is gone), so nothing here is a stranded write — the whole copy is still owed. Run:  %s\n", //nolint:errcheck // best-effort stderr
			logPrefix, target.Binding, target.MarkerPath(), storageMigrationCommand)
		return 1
	}
	proven, recorded, err := readInfraCopyManifest(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !recorded {
		fmt.Fprintf(stderr, "%s: %s converged before %s was recorded, so nothing distinguishes a bead the copy never carried from one the binding's own GC has since collected. Recovering on that basis would import rows the binding deliberately deleted. Re-converge the binding to restore the check\n", //nolint:errcheck // best-effort stderr
			logPrefix, target.Database, target.ManifestPath())
		return 1
	}

	if pid := infraMigrationForeignControllerPID(request.CityPath); pid != 0 {
		fmt.Fprintf(stderr, "%s: controller PID %d is live on this city and is still writing infrastructure beads to the work store; a repair proven against a moving source proves nothing. Stop it (gc stop) and run this again\n", logPrefix, pid) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !request.FleetStopped && !opts.DryRun {
		fmt.Fprintf(stderr, "%s: pass --%s to attest that %s. This command proves the controller is stopped and cannot prove anything about the rest; a write that lands after this run stays stranded and the next check names it\n", //nolint:errcheck // best-effort stderr
			logPrefix, storageFleetStoppedFlag, storageFleetStoppedAttestation)
		return 1
	}

	guard, err := storebinding.AcquireMigrationGuard(ctx, cityMigrationGuardDirectory(request.CityPath), storageMigrationGeneration)
	if err != nil {
		if errors.Is(err, storebinding.ErrMigrationGuardBusy) {
			fmt.Fprintf(stderr, "%s: another storage migration or repair holds this city. Wait for it to finish, or resolve it, and run this again\n", logPrefix) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintf(stderr, "%s: taking the migration guard: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer func() {
		if releaseErr := guard.Release(); releaseErr != nil {
			fmt.Fprintf(stderr, "%s: releasing the migration guard: %v\n", logPrefix, releaseErr) //nolint:errcheck // best-effort stderr
		}
	}()

	source, err := openInfraMigrationSource(request.CityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: opening the work store: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer closeBeadStoreHandle(source) //nolint:errcheck // best-effort close

	rows, err := readInfraSnapshot(source)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	destination, err := openInfraDestination(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: opening binding %q at %s: %v\n", logPrefix, target.Binding, target.Database, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	closed := false
	defer func() {
		if !closed {
			_ = closeBeadStoreHandle(destination)
		}
	}()

	held, err := destination.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		fmt.Fprintf(stderr, "%s: listing binding %s: %v\n", logPrefix, target.Database, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	resolvable := make(map[string]bool, len(held)+len(rows))
	for _, b := range held {
		resolvable[b.ID] = true
	}

	// The gap, by exactly confirmInfraConvergence's rule: in the source, not in
	// the binding, and not in the manifest of what the copy was proven to
	// deliver. A bead the manifest records is one the binding's own lifecycle
	// removed, and importing it back would resurrect state the binding deleted.
	var stranded []beads.Bead
	removedSinceCutover := 0
	for _, b := range rows {
		if resolvable[b.ID] {
			continue
		}
		if proven[b.ID] {
			removedSinceCutover++
			continue
		}
		stranded = append(stranded, b)
	}
	sort.Slice(stranded, func(i, j int) bool { return stranded[i].ID < stranded[j].ID })

	fmt.Fprintf(stdout, "city:     %s\nbinding:  %s\ndatabase: %s\nmanifest: %s\n", request.CityPath, target.Binding, target.Database, target.ManifestPath()) //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "source infrastructure beads: %d\nbinding beads: %d\nproven copy: %d\nremoved since cutover: %d\nstranded: %d\n",                     //nolint:errcheck // best-effort stdout
		len(rows), len(held), len(proven), removedSinceCutover, len(stranded))

	probeID := ""
	if len(stranded) > 0 {
		probeID = stranded[0].ID
	}
	depReader, err := newSourceDepReader(source, probeID)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "%s\n", depReader.describe()) //nolint:errcheck // best-effort stdout

	// Refuse rather than guess. A bead whose owning class is outside the ones
	// this split relocates, whose class would change in the crossing, or whose
	// dependency topology this source cannot state, is one nobody can say
	// belongs in this binding in the shape it would arrive in.
	allowed := infraRecoverableClasses()
	byClass := map[string]int{}
	var movable []beads.Bead
	var ambiguous []string
	wantEdges := make(map[string][]beads.Dep, len(stranded))
	for _, b := range stranded {
		class := coordclass.Classify(b)
		if !allowed[class] {
			ambiguous = append(ambiguous, fmt.Sprintf("%s (classifies as %s, which binding %q does not serve)", b.ID, class, target.Binding))
			continue
		}
		if diff := infraCopyClassDifference(b, infraMigrationRow(b)); diff != "" {
			ambiguous = append(ambiguous, fmt.Sprintf("%s (%s)", b.ID, diff))
			continue
		}
		edges, depsOK, depErr := depReader.deps(b)
		if !depsOK {
			ambiguous = append(ambiguous, fmt.Sprintf("%s (dependency topology unreadable: %v)", b.ID, depErr))
			continue
		}
		wantEdges[b.ID] = edges
		byClass[class.String()]++
		movable = append(movable, b)
	}
	for _, name := range sortedMapKeys(byClass) {
		fmt.Fprintf(stdout, "  class %-9s %d\n", name, byClass[name]) //nolint:errcheck // best-effort stdout
	}
	if len(ambiguous) > 0 {
		fmt.Fprintf(stdout, "  ambiguous (NOT moved): %d\n", len(ambiguous)) //nolint:errcheck // best-effort stdout
	}

	if opts.DumpPath != "" {
		if err := writeStrandedDump(opts.DumpPath, depReader, stranded); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintf(stdout, "dump: %s (%d bead(s) with their source dep edges)\n", opts.DumpPath, len(stranded)) //nolint:errcheck // best-effort stdout
	}

	if opts.DryRun {
		fmt.Fprintf(stdout, "dry-run: nothing was written. %d bead(s) would be copied into %s\n", len(movable), target.Database) //nolint:errcheck // best-effort stdout
		reportAmbiguous(stdout, ambiguous)
		if len(ambiguous) > 0 {
			return 1
		}
		return 0
	}

	if len(movable) == 0 {
		fmt.Fprintln(stdout, "nothing to recover: the binding already holds every infrastructure bead the retained work store has that the manifest does not record.") //nolint:errcheck // best-effort stdout
		reportAmbiguous(stdout, ambiguous)
		if len(ambiguous) > 0 {
			return 1
		}
		return 0
	}

	creator, isCreator := destination.(beads.ForeignIDCreator)
	if !isCreator {
		fmt.Fprintf(stderr, "%s: binding store cannot preserve bead ids: %T does not implement ForeignIDCreator\n", logPrefix, destination) //nolint:errcheck // best-effort stderr
		return 1
	}
	copied := 0
	for _, b := range movable {
		if _, err := destination.Get(b.ID); err == nil {
			// Already there — an earlier run of this command, or a writer that
			// beat us to it. Idempotence, not a conflict.
			resolvable[b.ID] = true
			continue
		}
		if _, err := creator.CreateWithForeignID(infraMigrationRow(b)); err != nil {
			fmt.Fprintf(stderr, "%s: copying bead %s into %s: %v\n", logPrefix, b.ID, target.Database, err)                                                                                                                                           //nolint:errcheck // best-effort stderr
			fmt.Fprintf(stderr, "%s: %d bead(s) were copied before this failed. Nothing was deleted and the manifest was not extended, so the remainder is still stranded, still named by the next check, and a re-run resumes\n", logPrefix, copied) //nolint:errcheck // best-effort stderr
			return 1
		}
		resolvable[b.ID] = true
		copied++
	}

	// Dep edges, after every row exists, so an edge's far endpoint can be
	// resolved. Only edges whose far endpoint the destination actually owns are
	// written: a dep row referencing an id this store cannot resolve is a
	// dangling edge, which is the one thing the migration's own import refuses
	// to create.
	edges := 0
	skippedEdges := 0
	for _, b := range movable {
		for _, d := range wantEdges[b.ID] {
			if !resolvable[d.DependsOnID] {
				skippedEdges++
				continue
			}
			if err := destination.DepAdd(b.ID, d.DependsOnID, d.Type); err != nil {
				fmt.Fprintf(stderr, "%s: copying dep %s -> %s: %v\n", logPrefix, b.ID, d.DependsOnID, err) //nolint:errcheck // best-effort stderr
				return 1
			}
			edges++
		}
	}
	fmt.Fprintf(stdout, "copied: %d bead(s), %d dep edge(s) (%d cross-boundary edge(s) left as metadata linkage)\n", copied, edges, skippedEdges) //nolint:errcheck // best-effort stdout

	// Proven against durable bytes, not against the handle that wrote them:
	// closed and reopened, exactly as verifyInfraCopy insists.
	if err := closeBeadStoreHandle(destination); err != nil {
		fmt.Fprintf(stderr, "%s: closing binding %q at %s after the copy: %v\n", logPrefix, target.Binding, target.Database, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	closed = true

	verifier, err := openInfraDestination(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: reopening the binding for the equality stage: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer closeBeadStoreHandle(verifier) //nolint:errcheck // best-effort close
	after, err := verifier.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		fmt.Fprintf(stderr, "%s: listing the reopened binding: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	witnessed := make(map[string]bool, len(after))
	for _, b := range after {
		witnessed[b.ID] = true
	}
	for _, want := range movable {
		got, err := verifier.Get(want.ID)
		if err != nil {
			fmt.Fprintf(stderr, "%s: bead %s missing from the reopened binding: %v. The manifest was NOT extended, so this bead is still named as stranded\n", logPrefix, want.ID, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if diff := beadCopyDifference(want, got); diff != "" {
			fmt.Fprintf(stderr, "%s: bead %s differs after the copy: %s. The manifest was NOT extended\n", logPrefix, want.ID, diff) //nolint:errcheck // best-effort stderr
			return 1
		}
		if diff := infraCopyClassDifference(want, got); diff != "" {
			fmt.Fprintf(stderr, "%s: bead %s %s. The manifest was NOT extended\n", logPrefix, want.ID, diff) //nolint:errcheck // best-effort stderr
			return 1
		}
		gotDeps, err := verifier.DepList(want.ID, "down")
		if err != nil {
			fmt.Fprintf(stderr, "%s: listing copied deps of %s: %v\n", logPrefix, want.ID, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if diff := infraDepDifference(want.ID, wantEdges[want.ID], gotDeps, witnessed); diff != "" {
			fmt.Fprintf(stderr, "%s: %s. The manifest was NOT extended\n", logPrefix, diff) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	fmt.Fprintf(stdout, "verified: %d bead(s) re-read field-, class- and dep-equal from the closed and reopened %s\n", len(movable), target.Database) //nolint:errcheck // best-effort stdout

	// Only now the manifest, and only as a SUPERSET: the previous proven set is
	// history the next boot still needs to classify absence against. An id
	// dropped from it turns a bead the binding's own GC legitimately collected
	// back into a strand, and the city refuses to boot over a row nothing did
	// wrong to.
	backup, err := backupInfraCopyManifest(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	extended := make([]string, 0, len(proven)+len(movable))
	for id := range proven {
		extended = append(extended, id)
	}
	for _, b := range movable {
		if !proven[b.ID] {
			extended = append(extended, b.ID)
		}
	}
	sort.Strings(extended)
	if dropped := manifestIDsDropped(proven, extended); len(dropped) > 0 {
		fmt.Fprintf(stderr, "%s: the extended manifest would drop %d id(s) the previous one recorded (%s). It is written as a superset or not at all; nothing was written and the previous manifest at %s still stands\n", //nolint:errcheck // best-effort stderr
			logPrefix, len(dropped), strings.Join(dropped, ", "), target.ManifestPath())
		return 1
	}
	if err := writeInfraCopyManifest(target, extended); err != nil {
		fmt.Fprintf(stderr, "%s: %v. The previous manifest is at %s\n", logPrefix, err, backup) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "manifest: %d -> %d bead(s) (previous manifest retained at %s)\n", len(proven), len(extended), backup) //nolint:errcheck // best-effort stdout

	// The residual, read back through the same classifier the boot check uses.
	residual, err := classifyInfraContainmentGap(request.CityPath, target, setOf(extended))
	if err != nil {
		fmt.Fprintf(stderr, "%s: re-reading the containment gap: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "residual stranded: %d\nresidual removed-since-cutover: %d\n", len(residual.Stranded), residual.RemovedSinceCutover) //nolint:errcheck // best-effort stdout
	if len(residual.Stranded) > 0 {
		fmt.Fprintf(stdout, "residual ids: %s\n", strings.Join(residual.Stranded, ", ")) //nolint:errcheck // best-effort stdout
	}
	reportAmbiguous(stdout, ambiguous)
	if len(residual.Stranded) > 0 || len(ambiguous) > 0 {
		return 1
	}
	return 0
}

// manifestIDsDropped returns the sorted ids the previous proven set recorded
// that the replacement does not.
//
// The manifest is the only record that tells a bead the copy never carried
// apart from one the binding's own GC collected, so it is append-only in
// practice: every id it has ever recorded stays recorded. Checking rather than
// asserting is the point — the extension is built by hand a few lines above,
// and a superset is exactly the kind of property that stays true until someone
// makes it a set intersection by accident.
func manifestIDsDropped(previous map[string]bool, replacement []string) []string {
	keep := setOf(replacement)
	var dropped []string
	for id := range previous {
		if !keep[id] {
			dropped = append(dropped, id)
		}
	}
	sort.Strings(dropped)
	return dropped
}

// reportAmbiguous names the beads this repair declined to move, one per line,
// because a count alone is not a starting point for resolving them.
func reportAmbiguous(stdout io.Writer, ambiguous []string) {
	if len(ambiguous) == 0 {
		return
	}
	fmt.Fprintf(stdout, "REFUSED to move %d bead(s) whose class this repair cannot state; they are intact in the work store:\n", len(ambiguous)) //nolint:errcheck // best-effort stdout
	for _, entry := range ambiguous {
		fmt.Fprintf(stdout, "  %s\n", entry) //nolint:errcheck // best-effort stdout
	}
}

// setOf renders an id slice as the membership map the containment classifier
// takes.
func setOf(ids []string) map[string]bool {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

// writeStrandedDump records every stranded bead and its source dep edges before
// anything is written, so the input to the repair is reproducible from a file
// rather than from a store that has since changed.
func writeStrandedDump(path string, depReader *sourceDepReader, stranded []beads.Bead) error {
	entries := make([]strandedBeadDump, 0, len(stranded))
	for _, b := range stranded {
		entry := strandedBeadDump{Bead: b, Class: coordclass.Classify(b).String(), DepSource: "relations"}
		if !depReader.relationsOK {
			entry.DepSource = "inline"
		}
		deps, ok, err := depReader.deps(b)
		if !ok {
			entry.DepSource = "unreadable"
			entry.DepError = fmt.Sprint(err)
		}
		entry.Deps = deps
		entries = append(entries, entry)
	}
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("writing the stranded-bead dump: %w", err)
	}
	writer := bufio.NewWriter(file)
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(entries); err != nil {
		_ = file.Close()
		return fmt.Errorf("encoding the stranded-bead dump: %w", err)
	}
	if err := writer.Flush(); err != nil {
		_ = file.Close()
		return fmt.Errorf("flushing the stranded-bead dump: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing the stranded-bead dump: %w", err)
	}
	return nil
}

// backupInfraCopyManifest copies the current manifest beside itself before the
// repair rewrites it. The rewrite is atomic and a proven superset, so this is
// belt and braces — but the manifest is the only record that tells a strand
// apart from a GC delete, and a file that cannot be reconstructed is one worth
// keeping.
func backupInfraCopyManifest(target infraBindingTarget) (string, error) {
	contents, err := os.ReadFile(target.ManifestPath())
	if err != nil {
		return "", fmt.Errorf("reading the manifest to back it up: %w", err)
	}
	path := fmt.Sprintf("%s.bak-%s", target.ManifestPath(), time.Now().UTC().Format("20060102T150405Z"))
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		return "", fmt.Errorf("backing the manifest up to %s: %w", path, err)
	}
	return path, nil
}
